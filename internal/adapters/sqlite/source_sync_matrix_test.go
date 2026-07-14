package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	gitadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/git"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

// These fixtures intentionally exercise the application boundary with a real
// local Git repository and SQLite store. They never use a network remote.
func TestDisposableSourceSyncFixtureMatrix(t *testing.T) {
	t.Run("clean exact merge survives restart and completes owned cleanup", func(t *testing.T) {
		fixture := newSourceSyncMatrixFixture(t)
		run, merge, resources := fixture.persistRun(t)
		adapter := &matrixSourceSync{sync: gitadapter.SourceSynchronizer{}}

		// This models a process loss after an already-persisted intent and Git's
		// exact target write, but before the result could be persisted. Restart
		// adopts the local result without another fast-forward.
		if err := fixture.store.UpsertCleanup(context.Background(), application.CleanupRecord{RunID: run.ID, Kind: "source_checkout", Name: "configured_source_checkout", Status: "intent"}); err != nil {
			t.Fatal(err)
		}
		first, err := adapter.sync.Sync(context.Background(), fixture.request(merge.MergeSHA))
		if err != nil || first.Status != gitadapter.SourceSyncSynced || first.Outcome != gitadapter.SourceSyncFastForwarded {
			t.Fatalf("initial Git result=%+v err=%v", first, err)
		}
		if err := application.SyncSourceCheckout(context.Background(), fixture.store, adapter, run, merge); err != nil {
			t.Fatal(err)
		}
		if adapter.calls != 1 { // the persisted intent caused one restart adoption
			t.Fatalf("source sync calls=%d, want 1", adapter.calls)
		}
		if got := matrixGitOutput(t, fixture.source, "rev-parse", "HEAD"); got != merge.MergeSHA || got == run.CandidateHead || got == fixture.later {
			t.Fatalf("source head=%s merge=%s candidate=%s later=%s", got, merge.MergeSHA, run.CandidateHead, fixture.later)
		}

		cleanup := &matrixCleanup{}
		if err := application.SyncSourceCheckout(context.Background(), fixture.store, adapter, run, merge); err != nil {
			t.Fatal(err)
		}
		if adapter.calls != 1 {
			t.Fatalf("terminal source result was invoked again: calls=%d", adapter.calls)
		}
		if err := application.CleanupOwned(context.Background(), fixture.store, cleanup, run, merge, resources); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(cleanup.calls, []string{"worktree", "remote_branch", "local_branch"}) {
			t.Fatalf("owned cleanup ordering=%v", cleanup.calls)
		}
		if err := application.CleanupOwned(context.Background(), fixture.store, cleanup, run, merge, resources); err != nil {
			t.Fatal(err)
		}
		if len(cleanup.calls) != 3 {
			t.Fatalf("terminal owned cleanup repeated: %v", cleanup.calls)
		}
		fixture.complete(t, run, merge)

		inspection, err := fixture.store.Inspect(context.Background(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if inspection.Run.State != domain.StateCompleted || inspection.Merge == nil || inspection.Merge.MergeSHA != merge.MergeSHA {
			t.Fatalf("terminal evidence=%+v", inspection)
		}
		if len(inspection.Cleanup) != 5 || inspection.Cleanup[0].Kind != "source_checkout" || inspection.Cleanup[0].Status != "synced" {
			t.Fatalf("cleanup evidence=%+v", inspection.Cleanup)
		}
		for _, resource := range inspection.Resources {
			if resource.Kind == "source_checkout" {
				t.Fatalf("operator source was recorded as owned: %+v", inspection.Resources)
			}
		}
		fixture.assertProjection(t, run, nil)
	})

	t.Run("already at or ahead never rewinds", func(t *testing.T) {
		for _, test := range []struct {
			name    string
			prepare func(*sourceSyncMatrixFixture)
		}{
			{"already_at_target", func(f *sourceSyncMatrixFixture) {
				matrixGit(t, f.source, "fetch", "origin", "main")
				matrixGit(t, f.source, "merge", "--ff-only", f.merge)
			}},
			{"already_contains_target", func(f *sourceSyncMatrixFixture) {
				matrixGit(t, f.source, "fetch", "origin", "main")
				matrixGit(t, f.source, "merge", "--ff-only", f.later)
			}},
		} {
			t.Run(test.name, func(t *testing.T) {
				fixture := newSourceSyncMatrixFixture(t)
				test.prepare(fixture)
				run, merge, resources := fixture.persistRun(t)
				before := matrixGitOutput(t, fixture.source, "rev-parse", "HEAD")
				if err := application.SyncSourceCheckout(context.Background(), fixture.store, &matrixSourceSync{sync: gitadapter.SourceSynchronizer{}}, run, merge); err != nil {
					t.Fatal(err)
				}
				if got := matrixGitOutput(t, fixture.source, "rev-parse", "HEAD"); got != before {
					t.Fatalf("source was rewound: got=%s want=%s", got, before)
				}
				if err := application.CleanupOwned(context.Background(), fixture.store, &matrixCleanup{}, run, merge, resources); err != nil {
					t.Fatal(err)
				}
				fixture.complete(t, run, merge)
				fixture.assertProjection(t, run, nil)
			})
		}
	})

	t.Run("unsafe source variants preserve checkout and expose one sanitized attention", func(t *testing.T) {
		cases := []struct {
			name    string
			prepare func(*sourceSyncMatrixFixture)
			reason  string
		}{
			{"staged", func(f *sourceSyncMatrixFixture) {
				f.write(t, "staged.txt", "dirty-sentinel")
				matrixGit(t, f.source, "add", "staged.txt")
			}, "dirty_source"},
			{"unstaged", func(f *sourceSyncMatrixFixture) { f.write(t, "base.txt", "dirty-sentinel") }, "dirty_source"},
			{"untracked", func(f *sourceSyncMatrixFixture) { f.write(t, "untracked.txt", "dirty-sentinel") }, "dirty_source"},
			{"ignored", func(f *sourceSyncMatrixFixture) { f.write(t, "ignored/sentinel.txt", "dirty-sentinel") }, "dirty_source"},
			{"wrong_branch", func(f *sourceSyncMatrixFixture) { matrixGit(t, f.source, "switch", "-c", "operator-branch") }, "wrong_branch"},
			{"detached", func(f *sourceSyncMatrixFixture) { matrixGit(t, f.source, "checkout", "--detach") }, "detached_head"},
			{"diverged", func(f *sourceSyncMatrixFixture) {
				f.write(t, "local.txt", "local divergence")
				matrixGit(t, f.source, "add", "local.txt")
				matrixGit(t, f.source, "commit", "-m", "local divergence")
			}, "source_diverged"},
		}
		for _, test := range cases {
			t.Run(test.name, func(t *testing.T) {
				fixture := newSourceSyncMatrixFixture(t)
				test.prepare(fixture)
				run, merge, resources := fixture.persistRun(t)
				before := matrixGitOutput(t, fixture.source, "rev-parse", "HEAD")
				beforeBranch := matrixGitOutput(t, fixture.source, "branch", "--show-current")
				beforeStatus := matrixGitOutput(t, fixture.source, "status", "--porcelain=v1", "--untracked-files=all", "--ignored=matching")
				cleanup := &matrixCleanup{}
				if err := application.SyncSourceCheckout(context.Background(), fixture.store, &matrixSourceSync{sync: gitadapter.SourceSynchronizer{}}, run, merge); err != nil {
					t.Fatal(err)
				}
				if got := matrixGitOutput(t, fixture.source, "rev-parse", "HEAD"); got != before {
					t.Fatalf("unsafe source changed: got=%s want=%s", got, before)
				}
				if got := matrixGitOutput(t, fixture.source, "branch", "--show-current"); got != beforeBranch {
					t.Fatalf("unsafe source branch changed: got=%q want=%q", got, beforeBranch)
				}
				if got := matrixGitOutput(t, fixture.source, "status", "--porcelain=v1", "--untracked-files=all", "--ignored=matching"); got != beforeStatus {
					t.Fatalf("unsafe source status changed: got=%q want=%q", got, beforeStatus)
				}
				if err := application.CleanupOwned(context.Background(), fixture.store, cleanup, run, merge, resources); err != nil {
					t.Fatal(err)
				}
				fixture.complete(t, run, merge)
				fixture.assertProjection(t, run, &test.reason)
			})
		}
	})

	t.Run("retryable source and owned cleanup failures preserve durable boundaries", func(t *testing.T) {
		fixture := newSourceSyncMatrixFixture(t)
		run, merge, resources := fixture.persistRun(t)
		retry := &matrixSourceSync{result: application.SourceSyncResult{Status: application.SourceSyncRetryableFailure, Outcome: application.SourceSyncNotApplied, Reason: application.SourceSyncReasonFetchFailed, MergeSHA: merge.MergeSHA}}
		if err := application.SyncSourceCheckout(context.Background(), fixture.store, retry, run, merge); err == nil {
			t.Fatalf("retryable source sync error=%v", err)
		}
		if progress, err := fixture.store.CleanupProgress(context.Background(), run.ID); err != nil || len(progress) != 1 || progress[0].Status != "failed" {
			t.Fatalf("retryable source persistence=%+v err=%v", progress, err)
		}
		// Production does not cross the source-result persistence boundary. Restore
		// a terminal result before exercising the separate owned-resource retry.
		retry.result = application.SourceSyncResult{Status: application.SourceSyncSynced, Outcome: application.SourceSyncFastForwarded, MergeSHA: merge.MergeSHA}
		if err := application.SyncSourceCheckout(context.Background(), fixture.store, retry, run, merge); err != nil {
			t.Fatal(err)
		}
		failed := &matrixCleanup{fail: map[string]bool{"remote_branch": true}}
		if err := application.CleanupOwned(context.Background(), fixture.store, failed, run, merge, resources); err == nil {
			t.Fatal("expected owned cleanup failure")
		}
		resume := &matrixCleanup{}
		if err := application.CleanupOwned(context.Background(), fixture.store, resume, run, merge, resources); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(resume.calls, []string{"remote_branch"}) {
			t.Fatalf("owned restart retried completed resources: %v", resume.calls)
		}
	})

	t.Run("adapter errors retain intent and retry without crossing cleanup", func(t *testing.T) {
		fixture := newSourceSyncMatrixFixture(t)
		run, merge, _ := fixture.persistRun(t)
		adapterFailure := errors.New("simulated source adapter failure")
		adapter := &matrixSourceSync{err: adapterFailure}
		for attempt := 1; attempt <= 2; attempt++ {
			err := application.SyncSourceCheckout(context.Background(), fixture.store, adapter, run, merge)
			if !errors.Is(err, adapterFailure) {
				t.Fatalf("attempt %d error=%v", attempt, err)
			}
			progress, progressErr := fixture.store.CleanupProgress(context.Background(), run.ID)
			if progressErr != nil || len(progress) != 1 || progress[0].Kind != "source_checkout" || progress[0].Status != "intent" {
				t.Fatalf("attempt %d crossed source intent boundary: progress=%+v err=%v", attempt, progress, progressErr)
			}
			durable, getErr := fixture.store.GetRun(context.Background(), run.ID)
			if getErr != nil || durable.State != domain.StateCleaning {
				t.Fatalf("attempt %d changed run state: run=%+v err=%v", attempt, durable, getErr)
			}
		}
		if adapter.calls != 2 {
			t.Fatalf("adapter calls=%d, want one retry per invocation", adapter.calls)
		}
	})

	t.Run("invalid merge origin and path authority fail closed before owned cleanup", func(t *testing.T) {
		for _, test := range []struct {
			name   string
			mutate func(*sourceSyncMatrixFixture, *application.Run, *application.MergeRecord)
		}{
			{"unreachable_merge", func(_ *sourceSyncMatrixFixture, _ *application.Run, merge *application.MergeRecord) {
				merge.MergeSHA = strings.Repeat("0", 40)
			}},
			{"origin_mismatch", func(f *sourceSyncMatrixFixture, _ *application.Run, _ *application.MergeRecord) {
				matrixGit(t, f.source, "remote", "set-url", "origin", filepath.Join(f.root, "other.git"))
			}},
			{"noncanonical_source_path", func(_ *sourceSyncMatrixFixture, run *application.Run, _ *application.MergeRecord) {
				var repository application.LocalRepository
				if err := json.Unmarshal([]byte(run.RepositoryConfigJSON), &repository); err != nil {
					t.Fatal(err)
				}
				repository.SourcePath += "/."
				data, err := json.Marshal(repository)
				if err != nil {
					t.Fatal(err)
				}
				run.RepositoryConfigJSON = string(data)
			}},
		} {
			t.Run(test.name, func(t *testing.T) {
				fixture := newSourceSyncMatrixFixture(t)
				run, merge, _ := fixture.persistRun(t)
				test.mutate(fixture, &run, &merge)
				before := matrixGitOutput(t, fixture.source, "rev-parse", "HEAD")
				if err := application.SyncSourceCheckout(context.Background(), fixture.store, &matrixSourceSync{sync: gitadapter.SourceSynchronizer{}}, run, merge); err == nil {
					t.Fatal("expected fail-closed source authority error")
				}
				if got := matrixGitOutput(t, fixture.source, "rev-parse", "HEAD"); got != before {
					t.Fatalf("authority failure changed source: got=%s want=%s", got, before)
				}
				progress, err := fixture.store.CleanupProgress(context.Background(), run.ID)
				if err != nil || len(progress) != 1 || progress[0].Kind != "source_checkout" || progress[0].Status != "intent" {
					t.Fatalf("authority failure crossed cleanup boundary: progress=%+v err=%v", progress, err)
				}
			})
		}
	})
}

type sourceSyncMatrixFixture struct {
	t         *testing.T
	root      string
	origin    string
	source    string
	store     *Store
	base      string
	candidate string
	merge     string
	later     string
}

func newSourceSyncMatrixFixture(t *testing.T) *sourceSyncMatrixFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	origin := filepath.Join(root, "origin.git")
	matrixGit(t, root, "init", "--bare", origin)
	seed := filepath.Join(root, "seed")
	matrixGit(t, root, "clone", origin, seed)
	matrixConfigure(t, seed)
	matrixGit(t, seed, "switch", "-c", "main")
	matrixWrite(t, seed, "base.txt", "base\n")
	matrixWrite(t, seed, ".gitignore", "ignored/\n")
	matrixGit(t, seed, "add", ".")
	matrixGit(t, seed, "commit", "-m", "base")
	base := matrixGitOutput(t, seed, "rev-parse", "HEAD")
	matrixGit(t, seed, "push", "-u", "origin", "main")
	matrixGit(t, root, "--git-dir="+origin, "symbolic-ref", "HEAD", "refs/heads/main")

	source := filepath.Join(root, "source")
	matrixGit(t, root, "clone", origin, source)
	matrixConfigure(t, source)
	candidateWorktree := filepath.Join(root, "candidate")
	matrixGit(t, root, "clone", origin, candidateWorktree)
	matrixConfigure(t, candidateWorktree)
	matrixGit(t, candidateWorktree, "switch", "-c", "candidate", "origin/main")
	matrixWrite(t, candidateWorktree, "candidate.txt", "candidate\n")
	matrixGit(t, candidateWorktree, "add", "candidate.txt")
	matrixGit(t, candidateWorktree, "commit", "-m", "candidate")
	candidate := matrixGitOutput(t, candidateWorktree, "rev-parse", "HEAD")
	matrixGit(t, candidateWorktree, "push", "origin", "candidate")

	merger := filepath.Join(root, "merger")
	matrixGit(t, root, "clone", origin, merger)
	matrixConfigure(t, merger)
	matrixGit(t, merger, "merge", "--squash", "origin/candidate")
	matrixGit(t, merger, "commit", "-m", "squash merge")
	merge := matrixGitOutput(t, merger, "rev-parse", "HEAD")
	matrixGit(t, merger, "push", "origin", "main")
	matrixWrite(t, merger, "later.txt", "later\n")
	matrixGit(t, merger, "add", "later.txt")
	matrixGit(t, merger, "commit", "-m", "later remote commit")
	later := matrixGitOutput(t, merger, "rev-parse", "HEAD")
	matrixGit(t, merger, "push", "origin", "main")

	store, err := Open(filepath.Join(root, "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &sourceSyncMatrixFixture{t: t, root: root, origin: origin, source: source, store: store, base: base, candidate: candidate, merge: merge, later: later}
}

func (f *sourceSyncMatrixFixture) write(t *testing.T, name, content string) {
	t.Helper()
	matrixWrite(t, f.source, name, content)
}

func (f *sourceSyncMatrixFixture) request(mergeSHA string) gitadapter.SourceSyncRequest {
	return gitadapter.SourceSyncRequest{Repository: "fixture/repository", SourcePath: f.source, OriginPath: f.origin, BaseBranch: "main", MergeSHA: mergeSHA}
}

func (f *sourceSyncMatrixFixture) persistRun(t *testing.T) (application.Run, application.MergeRecord, []application.OwnedResource) {
	t.Helper()
	ctx := context.Background()
	repository, err := json.Marshal(application.LocalRepository{CanonicalRepository: "fixture/repository", SourcePath: f.source, OriginPath: f.origin, RunRoot: filepath.Join(f.root, "runs"), WorktreeRoot: filepath.Join(f.root, "worktrees"), BaseBranch: "main", AllowedOperatorLogins: []string{"operator"}})
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(f.root, "worktrees", "fixture-run")
	artifacts := filepath.Join(f.root, "runs", "fixture-run")
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(artifacts, "attempts"), 0o700); err != nil {
		t.Fatal(err)
	}
	run := application.Run{ID: "fixture-run", IssueID: "IFAN-FIXTURE", IdempotencyKey: "fixture-key", SourceRevision: "fixture-v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "fixture/repository", RepositoryConfigJSON: string(repository), BaseBranch: "main", WorkingBranch: "ifan/fixture", BaseSHA: f.base, WorktreePath: worktree, ArtifactRoot: artifacts, ImplementationModel: "fixture", ReviewModel: "fixture"}
	if _, _, err := f.store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetWorkspace(ctx, run.ID, run.BaseSHA, run.WorktreePath); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetCandidateHead(ctx, run.ID, f.candidate); err != nil {
		t.Fatal(err)
	}
	// This is a persisted post-merge fixture baseline. Production reaches this
	// state through the earlier merge and Linear-completion gates.
	if _, err := f.store.db.ExecContext(ctx, `UPDATE runs SET current_state=? WHERE run_id=?`, domain.StateCleaning, run.ID); err != nil {
		t.Fatal(err)
	}
	run.State = domain.StateCleaning
	run.CandidateHead = f.candidate
	merge := application.MergeRecord{RunID: run.ID, PRNumber: 1, PreMergeSHA: run.CandidateHead, BaseSHA: run.BaseSHA, Method: "squash", MergeSHA: f.merge, MergedAt: time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)}
	if err := f.store.SaveMerge(ctx, merge); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SaveLinearCompletionObservation(ctx, application.LinearCompletionObservation{RunID: run.ID, MergeSHA: merge.MergeSHA, Identifier: "IFAN-FIXTURE", Status: application.LinearCompletionCompleted, ObservedAt: merge.MergedAt.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	evidence := `{"source_path":"` + f.source + `","origin_path":"` + f.origin + `","path":"` + worktree + `","branch":"` + run.WorkingBranch + `","base_branch":"main","base_sha":"` + f.base + `","nonce":"fixture-nonce"}`
	artifact := `{"path":"` + artifacts + `","attempts_path":"` + filepath.Join(artifacts, "attempts") + `","run_root":"` + filepath.Join(f.root, "runs") + `","nonce":"fixture-artifact-nonce","task_hash":"task"}`
	resources := []application.OwnedResource{{RunID: run.ID, Kind: "artifact_root", Name: artifacts, Status: "owned", CreationEvidence: artifact}, {RunID: run.ID, Kind: "worktree", Name: worktree, Status: "owned", CreationEvidence: evidence}, {RunID: run.ID, Kind: "branch", Name: run.WorkingBranch, Status: "owned", CreationEvidence: evidence}, {RunID: run.ID, Kind: "remote_branch", Name: run.WorkingBranch, Status: "owned", CreationEvidence: evidence}}
	for _, resource := range resources {
		if err := f.store.AddOwnedResource(ctx, resource); err != nil {
			t.Fatal(err)
		}
	}
	return run, merge, resources
}

func (f *sourceSyncMatrixFixture) complete(t *testing.T, run application.Run, merge application.MergeRecord) {
	t.Helper()
	if err := f.store.Transition(context.Background(), run.ID, domain.StateCleaning, domain.StateCompleted, "owned cleanup completed", merge.MergeSHA, run.CandidateHead); err != nil {
		t.Fatal(err)
	}
}

func (f *sourceSyncMatrixFixture) assertProjection(t *testing.T, run application.Run, reason *string) {
	t.Helper()
	service := application.NewQueryService(f.store)
	input := application.QueryInput{Requester: application.Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository}
	status, err := service.Status(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	inspect, err := service.Inspect(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(status.OperatorAttention, inspect.OperatorAttention) {
		t.Fatalf("status/inspect attention mismatch: status=%+v inspect=%+v", status.OperatorAttention, inspect.OperatorAttention)
	}
	if reason == nil {
		if status.OperatorAttention == nil || len(status.OperatorAttention) != 0 {
			t.Fatalf("unexpected attention=%+v", status.OperatorAttention)
		}
	} else if len(status.OperatorAttention) != 1 || status.OperatorAttention[0].ReasonCode != *reason || status.OperatorAttention[0].Code != "source_checkout_sync_required" {
		t.Fatalf("attention=%+v want=%s", status.OperatorAttention, *reason)
	}
	raw, err := json.Marshal(inspect)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{f.root, f.source, f.origin, "dirty-sentinel", "Authorization:", "Bearer "} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("public projection leaked %q: %s", forbidden, raw)
		}
	}
}

type matrixSourceSync struct {
	sync   gitadapter.SourceSynchronizer
	result application.SourceSyncResult
	err    error
	calls  int
}

func (p *matrixSourceSync) Sync(ctx context.Context, request application.SourceSyncRequest) (application.SourceSyncResult, error) {
	p.calls++
	if p.err != nil {
		return application.SourceSyncResult{}, p.err
	}
	if p.result.Status != "" {
		return p.result, nil
	}
	result, err := p.sync.Sync(ctx, gitadapter.SourceSyncRequest{Repository: request.Repository, SourcePath: request.SourcePath, OriginPath: request.OriginPath, BaseBranch: request.BaseBranch, MergeSHA: request.MergeSHA})
	return application.SourceSyncResult{Status: application.SourceSyncStatus(result.Status), Outcome: application.SourceSyncOutcome(result.Outcome), Reason: application.SourceSyncReason(result.Reason), BeforeSHA: result.BeforeSHA, AfterSHA: result.AfterSHA, MergeSHA: result.MergeSHA}, err
}

type matrixCleanup struct {
	fail  map[string]bool
	calls []string
}

func (c *matrixCleanup) RemoveWorktree(context.Context, string, string, string, string) error {
	c.calls = append(c.calls, "worktree")
	if c.fail["worktree"] {
		return errors.New("temporary worktree failure")
	}
	return nil
}
func (c *matrixCleanup) DeleteLocalBranch(context.Context, string, string, string) error {
	c.calls = append(c.calls, "local_branch")
	if c.fail["local_branch"] {
		return errors.New("temporary local branch failure")
	}
	return nil
}
func (c *matrixCleanup) DeleteRemoteBranch(context.Context, string, string, string) error {
	c.calls = append(c.calls, "remote_branch")
	if c.fail["remote_branch"] {
		return errors.New("temporary remote branch failure")
	}
	return nil
}

func matrixConfigure(t *testing.T, directory string) {
	t.Helper()
	matrixGit(t, directory, "config", "user.email", "fixture@example.invalid")
	matrixGit(t, directory, "config", "user.name", "Fixture")
}

func matrixWrite(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func matrixGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	_ = matrixGitOutput(t, directory, args...)
}

func matrixGitOutput(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}
