package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSourceSynchronizerFastForwardsOnlyPersistedMerge(t *testing.T) {
	fixture := sourceSyncFixture(t)
	result, err := (SourceSynchronizer{}).Sync(context.Background(), fixture.request())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != SourceSyncSynced || result.Outcome != SourceSyncFastForwarded || result.BeforeSHA != fixture.base || result.AfterSHA != fixture.target || result.MergeSHA != fixture.target {
		t.Fatalf("result=%+v", result)
	}
	if head := stringOutput(t, fixture.source, "rev-parse", "HEAD"); head != fixture.target {
		t.Fatalf("source HEAD=%s, want exact target %s", head, fixture.target)
	}
	if fixture.later == fixture.target {
		t.Fatal("fixture did not create a later remote commit")
	}
}

func TestSourceSynchronizerNeverRewindsTargetContainingSource(t *testing.T) {
	fixture := sourceSyncFixture(t)
	runGit(t, fixture.source, "fetch", "origin", "main")
	runGit(t, fixture.source, "merge", "--ff-only", fixture.later)
	before := stringOutput(t, fixture.source, "rev-parse", "HEAD")
	result, err := (SourceSynchronizer{}).Sync(context.Background(), fixture.request())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != SourceSyncSynced || result.Outcome != SourceSyncAlreadyContainsTarget || result.AfterSHA != before {
		t.Fatalf("result=%+v before=%s", result, before)
	}
	if head := stringOutput(t, fixture.source, "rev-parse", "HEAD"); head != before {
		t.Fatalf("source was rewound: got %s want %s", head, before)
	}
}

func TestSourceSynchronizerRecognizesExactTarget(t *testing.T) {
	fixture := sourceSyncFixture(t)
	runGit(t, fixture.source, "fetch", "origin", "main")
	runGit(t, fixture.source, "merge", "--ff-only", fixture.target)
	result, err := (SourceSynchronizer{}).Sync(context.Background(), fixture.request())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != SourceSyncSynced || result.Outcome != SourceSyncAlreadyAtTarget || result.AfterSHA != fixture.target {
		t.Fatalf("result=%+v", result)
	}
}

func TestSourceSynchronizerSkipsUnsafeCheckoutsWithoutWriting(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, f syncFixture)
		reason  SourceSyncReason
	}{
		{"staged", func(t *testing.T, f syncFixture) {
			writeSyncFile(t, f.source, "staged.txt")
			runGit(t, f.source, "add", "staged.txt")
		}, SourceSyncReasonDirtySource},
		{"unstaged", func(t *testing.T, f syncFixture) {
			if err := os.WriteFile(filepath.Join(f.source, "base.txt"), []byte("changed\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, SourceSyncReasonDirtySource},
		{"untracked", func(t *testing.T, f syncFixture) { writeSyncFile(t, f.source, "untracked.txt") }, SourceSyncReasonDirtySource},
		{"ignored", func(t *testing.T, f syncFixture) { writeSyncFile(t, f.source, "ignored.txt") }, SourceSyncReasonDirtySource},
		{"wrong branch", func(t *testing.T, f syncFixture) { runGit(t, f.source, "switch", "-c", "other") }, SourceSyncReasonWrongBranch},
		{"detached", func(t *testing.T, f syncFixture) { runGit(t, f.source, "checkout", "--detach") }, SourceSyncReasonDetachedHead},
		{"diverged", func(t *testing.T, f syncFixture) {
			writeSyncFile(t, f.source, "local.txt")
			runGit(t, f.source, "add", "local.txt")
			runGit(t, f.source, "commit", "-m", "local divergence")
		}, SourceSyncReasonSourceDiverged},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := sourceSyncFixture(t)
			test.prepare(t, fixture)
			before := stringOutput(t, fixture.source, "rev-parse", "HEAD")
			result, err := (SourceSynchronizer{}).Sync(context.Background(), fixture.request())
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != SourceSyncSkippedAttention || result.Outcome != SourceSyncNotApplied || result.Reason != test.reason || result.AfterSHA != before {
				t.Fatalf("result=%+v", result)
			}
			if head := stringOutput(t, fixture.source, "rev-parse", "HEAD"); head != before {
				t.Fatalf("unsafe checkout changed from %s to %s", before, head)
			}
		})
	}
}

func TestSourceSynchronizerFailsClosedForInvalidAuthority(t *testing.T) {
	fixture := sourceSyncFixture(t)
	tests := []struct {
		name   string
		mutate func(*SourceSyncRequest)
	}{
		{"unreachable merge", func(r *SourceSyncRequest) { r.MergeSHA = fixture.unreachable }},
		{"short SHA", func(r *SourceSyncRequest) { r.MergeSHA = "deadbeef" }},
		{"noncanonical SHA", func(r *SourceSyncRequest) { r.MergeSHA = strings.ToUpper(fixture.target) }},
		{"revision injection", func(r *SourceSyncRequest) { r.MergeSHA = fixture.target + "^{commit}" }},
		{"origin mismatch", func(r *SourceSyncRequest) { r.OriginPath = filepath.Join(fixture.root, "other.git") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := fixture.request()
			test.mutate(&request)
			before := stringOutput(t, fixture.source, "rev-parse", "HEAD")
			result, err := (SourceSynchronizer{}).Sync(context.Background(), request)
			if err == nil || result != (SourceSyncResult{}) {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if strings.Contains(err.Error(), fixture.source) || strings.Contains(err.Error(), fixture.origin) || strings.Contains(err.Error(), request.MergeSHA) {
				t.Fatalf("authority error leaked sensitive input: %v", err)
			}
			if head := stringOutput(t, fixture.source, "rev-parse", "HEAD"); head != before {
				t.Fatalf("failed authority changed checkout: %s -> %s", before, head)
			}
		})
	}

	t.Run("symlink source", func(t *testing.T) {
		link := filepath.Join(fixture.root, "source-link")
		if err := os.Symlink(fixture.source, link); err != nil {
			t.Fatal(err)
		}
		request := fixture.request()
		request.SourcePath = link
		if _, err := (SourceSynchronizer{}).Sync(context.Background(), request); err == nil {
			t.Fatal("symlink source was accepted")
		}
	})
}

func TestSourceSynchronizerRejectsAnnotatedTagObject(t *testing.T) {
	fixture := sourceSyncFixture(t)
	runGit(t, fixture.source, "fetch", "origin", "refs/tags/merge-tag")
	request := fixture.request()
	request.MergeSHA = fixture.annotatedTag
	before := stringOutput(t, fixture.source, "rev-parse", "HEAD")
	result, err := (SourceSynchronizer{}).Sync(context.Background(), request)
	if err == nil || result != (SourceSyncResult{}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if head := stringOutput(t, fixture.source, "rev-parse", "HEAD"); head != before {
		t.Fatalf("annotated tag changed source: %s -> %s", before, head)
	}
}

func TestSourceSynchronizerDetectsDriftBeforeWrite(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(t *testing.T, f syncFixture)
		wantError bool
	}{
		{"branch", func(t *testing.T, f syncFixture) { runGit(t, f.source, "switch", "-c", "drift") }, false},
		{"status", func(t *testing.T, f syncFixture) { writeSyncFile(t, f.source, "drift.txt") }, false},
		{"origin", func(t *testing.T, f syncFixture) {
			runGit(t, f.source, "remote", "set-url", "origin", filepath.Join(f.root, "other.git"))
		}, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := sourceSyncFixture(t)
			adapter := SourceSynchronizer{afterFetch: func() { test.mutate(t, fixture) }}
			result, err := adapter.Sync(context.Background(), fixture.request())
			if test.wantError {
				if err == nil {
					t.Fatal("origin drift was accepted")
				}
				return
			}
			if err != nil || result.Status != SourceSyncSkippedAttention || result.Reason != SourceSyncReasonStateDrift {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if head := stringOutput(t, fixture.source, "rev-parse", "HEAD"); head != fixture.base {
				t.Fatalf("drift case wrote target: %s", head)
			}
		})
	}
}

func TestSourceSynchronizerReconcilesAmbiguousFastForward(t *testing.T) {
	fixture := sourceSyncFixture(t)
	normal := SourceSynchronizer{}
	adapter := SourceSynchronizer{run: func(ctx context.Context, directory string, args ...string) (string, error) {
		output, err := normal.command(ctx, directory, args...)
		if len(args) > 0 && args[0] == "merge" && err == nil {
			return "", errors.New("simulated lost response")
		}
		return output, err
	}}
	result, err := adapter.Sync(context.Background(), fixture.request())
	if err != nil || result.Status != SourceSyncSynced || result.Outcome != SourceSyncFastForwarded || result.AfterSHA != fixture.target {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestSourceSynchronizerReportsRetryableFailedFastForwardAfterReconciliation(t *testing.T) {
	fixture := sourceSyncFixture(t)
	normal := SourceSynchronizer{}
	adapter := SourceSynchronizer{run: func(ctx context.Context, directory string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "merge" {
			return "", errors.New("simulated failed fast-forward")
		}
		return normal.command(ctx, directory, args...)
	}}
	result, err := adapter.Sync(context.Background(), fixture.request())
	if err != nil || result.Status != SourceSyncRetryableFailure || result.Outcome != SourceSyncNotApplied || result.Reason != SourceSyncReasonGitUncertain || result.BeforeSHA != fixture.base || result.AfterSHA != fixture.base {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if head := stringOutput(t, fixture.source, "rev-parse", "HEAD"); head != fixture.base {
		t.Fatalf("failed fast-forward changed source to %s", head)
	}
}

func TestSourceSynchronizerReconcilesFetchFailuresWithoutClaimingSync(t *testing.T) {
	tests := []struct {
		name    string
		adapter func(t *testing.T, fixture syncFixture) SourceSynchronizer
	}{
		{
			name: "fetch completed but response was lost",
			adapter: func(t *testing.T, fixture syncFixture) SourceSynchronizer {
				normal := SourceSynchronizer{}
				return SourceSynchronizer{run: func(ctx context.Context, directory string, args ...string) (string, error) {
					output, err := normal.command(ctx, directory, args...)
					if len(args) > 0 && args[0] == "fetch" && err == nil {
						return "", errors.New("simulated lost fetch response")
					}
					return output, err
				}}
			},
		},
		{
			name: "fetch actually failed",
			adapter: func(t *testing.T, fixture syncFixture) SourceSynchronizer {
				normal := SourceSynchronizer{}
				changed := false
				return SourceSynchronizer{run: func(ctx context.Context, directory string, args ...string) (string, error) {
					output, err := normal.command(ctx, directory, args...)
					if !changed && len(args) == 3 && args[0] == "remote" && args[1] == "get-url" && args[2] == "origin" && err == nil {
						changed = true
						runGit(t, fixture.source, "remote", "set-url", "origin", filepath.Join(fixture.root, "missing-origin.git"))
					}
					return output, err
				}}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := sourceSyncFixture(t)
			result, err := test.adapter(t, fixture).Sync(context.Background(), fixture.request())
			if err != nil || result.Status != SourceSyncRetryableFailure || result.Outcome != SourceSyncNotApplied || result.Reason != SourceSyncReasonFetchFailed || result.BeforeSHA != fixture.base || result.AfterSHA != fixture.base {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if head := stringOutput(t, fixture.source, "rev-parse", "HEAD"); head != fixture.base {
				t.Fatalf("fetch failure changed source to %s", head)
			}
		})
	}
}

func TestSourceSynchronizerDisablesHostileMergeAutostash(t *testing.T) {
	fixture := sourceSyncFixture(t)
	runGit(t, fixture.source, "config", "merge.autoStash", "true")
	gitBinary, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(fixture.root, "git-argv.log")
	wrapper := filepath.Join(fixture.root, "git-wrapper")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> '" + log + "'\nexec '" + gitBinary + "' \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := (SourceSynchronizer{Workspace: Workspace{Binary: wrapper}}).Sync(context.Background(), fixture.request())
	if err != nil || result.Status != SourceSyncSynced || result.Outcome != SourceSyncFastForwarded {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	argv, err := os.ReadFile(log)
	if err != nil || !strings.Contains(string(argv), "merge.autoStash=false\n") {
		t.Fatalf("merge autostash override missing: %q err=%v", argv, err)
	}
	stashCommand := exec.Command("git", "stash", "list")
	stashCommand.Dir = fixture.source
	stash, err := stashCommand.Output()
	if err != nil || strings.TrimSpace(string(stash)) != "" {
		t.Fatalf("hostile merge config created a stash: %q err=%v", stash, err)
	}
}

func TestSourceSynchronizerUsesOnlyAllowedArgvOperations(t *testing.T) {
	fixture := sourceSyncFixture(t)
	normal := SourceSynchronizer{}
	var calls [][]string
	adapter := SourceSynchronizer{run: func(ctx context.Context, directory string, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		return normal.command(ctx, directory, args...)
	}}
	if _, err := adapter.Sync(context.Background(), fixture.request()); err != nil {
		t.Fatal(err)
	}
	forbidden := []string{"pull", "reset", "checkout", "switch", "clean", "stash", "rebase", "update-ref", "worktree", "push"}
	for _, call := range calls {
		if len(call) == 0 {
			t.Fatal("empty Git argv")
		}
		if slices.Contains(forbidden, call[0]) {
			t.Fatalf("forbidden Git operation %q in %v", call[0], call)
		}
	}
	if !slices.ContainsFunc(calls, func(call []string) bool {
		return slices.Equal(call, []string{"fetch", "--no-tags", "origin", "refs/heads/main"})
	}) {
		t.Fatalf("missing exact branch-only fetch: %v", calls)
	}
	if !slices.ContainsFunc(calls, func(call []string) bool {
		return slices.Equal(call, []string{"merge", "--ff-only", "--no-edit", fixture.target})
	}) {
		t.Fatalf("missing exact fast-forward merge: %v", calls)
	}
}

type syncFixture struct {
	root, origin, source, base, target, later, unreachable, annotatedTag string
}

func (f syncFixture) request() SourceSyncRequest {
	return SourceSyncRequest{Repository: "owner/repo", SourcePath: f.source, OriginPath: f.origin, BaseBranch: "main", MergeSHA: f.target}
}

func sourceSyncFixture(t *testing.T) syncFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	origin, source, writer := filepath.Join(root, "origin.git"), filepath.Join(root, "source"), filepath.Join(root, "writer")
	runGit(t, root, "init", "--bare", origin)
	runGit(t, root, "init", "-b", "main", source)
	runGit(t, source, "config", "user.name", "Fixture")
	runGit(t, source, "config", "user.email", "fixture@example.invalid")
	if err := os.WriteFile(filepath.Join(source, ".gitignore"), []byte("ignored.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeSyncFile(t, source, "base.txt")
	runGit(t, source, "add", "--all")
	runGit(t, source, "commit", "-m", "base")
	base := stringOutput(t, source, "rev-parse", "HEAD")
	runGit(t, source, "remote", "add", "origin", origin)
	runGit(t, source, "push", "origin", "main")
	runGit(t, root, "clone", "-b", "main", origin, writer)
	runGit(t, writer, "config", "user.name", "Fixture")
	runGit(t, writer, "config", "user.email", "fixture@example.invalid")
	writeSyncFile(t, writer, "target.txt")
	runGit(t, writer, "add", "target.txt")
	runGit(t, writer, "commit", "-m", "merge target")
	target := stringOutput(t, writer, "rev-parse", "HEAD")
	runGit(t, writer, "push", "origin", "main")
	runGit(t, writer, "tag", "-a", "merge-tag", "-m", "annotated target", target)
	annotatedTag := stringOutput(t, writer, "rev-parse", "refs/tags/merge-tag")
	runGit(t, writer, "push", "origin", "refs/tags/merge-tag")
	writeSyncFile(t, writer, "later.txt")
	runGit(t, writer, "add", "later.txt")
	runGit(t, writer, "commit", "-m", "later remote")
	later := stringOutput(t, writer, "rev-parse", "HEAD")
	runGit(t, writer, "push", "origin", "main")
	runGit(t, writer, "switch", "-c", "unreachable", base)
	writeSyncFile(t, writer, "unreachable.txt")
	runGit(t, writer, "add", "unreachable.txt")
	runGit(t, writer, "commit", "-m", "unreachable")
	unreachable := stringOutput(t, writer, "rev-parse", "HEAD")
	runGit(t, writer, "push", "origin", "unreachable")
	return syncFixture{root: root, origin: origin, source: source, base: base, target: target, later: later, unreachable: unreachable, annotatedTag: annotatedTag}
}

func writeSyncFile(t *testing.T, directory, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, name), []byte(name+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
