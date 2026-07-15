package application_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/codex"
	gitadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/git"
	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/localissue"
	processadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/process"
	storeadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/verifier"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type localLab struct {
	root, origin, source, worktrees, runs, db string
	snapshot                                  localissue.Snapshot
	repository                                application.LocalRepository
}

func newLocalLab(t *testing.T) localLab {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	source := filepath.Join(root, "source")
	runGit(t, root, "init", "--bare", origin)
	runGit(t, root, "init", "-b", "main", source)
	runGit(t, source, "config", "user.name", "Fixture")
	runGit(t, source, "config", "user.email", "fixture@example.invalid")
	mustMkdir(t, filepath.Join(source, "mathutil"))
	mustWrite(t, filepath.Join(source, "go.mod"), "module example.invalid/local-lab\n\ngo 1.26\n")
	mustWrite(t, filepath.Join(source, "mathutil", "doc.go"), "package mathutil\n")
	mustWrite(t, filepath.Join(source, ".gitignore"), "ignored.tmp\n")
	runGit(t, source, "add", "--all")
	runGit(t, source, "commit", "-m", "Fixture base")
	runGit(t, source, "remote", "add", "origin", origin)
	runGit(t, source, "push", "origin", "main")
	issue := localissue.Issue{IssueID: "LAB-1", Title: "Add Clamp", Description: "Add a pure integer Clamp function and table-driven tests.", Team: "IFAN", Labels: []string{"agent:codex", "repo:test-project"}, Status: "Todo", CurrentCycle: true, CycleID: "lab", RepositoryLabel: "repo:test-project", BaseBranch: "main", BranchName: "ifan/lab-1-clamp", Goal: "Implement mathutil.Clamp", AcceptanceCriteria: []string{"Clamp returns min below range, max above range, and value inside range.", "go test ./... passes."}, OutOfScope: []string{"Network", "External services"}, VerifierIDs: []string{"fixture-go-test"}, SourceRevision: "lab-v1", CreatedAt: time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)}
	raw, _ := json.Marshal(issue)
	snapshot, err := localissue.Admit(issue, raw, labAdmissionRegistry{})
	if err != nil {
		t.Fatal(err)
	}
	worktrees := filepath.Join(root, "worktrees")
	runs := filepath.Join(root, "runs")
	mustMkdir(t, worktrees)
	mustMkdir(t, runs)
	return localLab{root: root, origin: origin, source: source, worktrees: worktrees, runs: runs, db: filepath.Join(root, "controller.db"), snapshot: snapshot, repository: application.LocalRepository{CanonicalRepository: "repo:test-project", OriginPath: origin, SourcePath: source, RunRoot: runs, WorktreeRoot: worktrees, BaseBranch: "main", VerifierIDs: []string{"fixture-go-test"}}}
}

type labAdmissionRegistry struct{}

func (labAdmissionRegistry) HasRepository(label string) bool { return label == "repo:test-project" }
func (labAdmissionRegistry) HasVerifier(label, id string) bool {
	return label == "repo:test-project" && id == "fixture-go-test"
}

type productionLinearReader struct {
	source application.LinearTaskSource
	reads  int
}

func (r *productionLinearReader) ReadIssue(_ context.Context, _ string) (application.LinearTaskSource, []application.LinearRequestObservation, error) {
	r.reads++
	source := r.source
	if r.reads > 1 {
		source.State = application.LinearState{ID: "started", Name: "In Progress", Type: "started"}
	}
	return source, []application.LinearRequestObservation{{Operation: "read_issue", HTTPStatus: 200, ResponseDigest: "fixture", ObservedAt: time.Now().UTC()}}, nil
}

type productionLinearResolver struct{ repository application.LocalRepository }

func (r productionLinearResolver) ResolveLinearAdmissionRepository(label string) (application.LocalRepository, bool) {
	return r.repository, label == "repo:test-project"
}

func productionLinearSource() application.LinearTaskSource {
	created := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	updated := created.Add(time.Hour)
	return application.LinearTaskSource{
		Provider:       "linear",
		IssueID:        "linear-fixture-id",
		Identifier:     "IFAN-FIXTURE",
		URL:            "https://linear.example.invalid/IFAN-FIXTURE",
		Title:          "Add Clamp",
		Description:    "## Goal\n\nImplement mathutil.Clamp\n\n## Acceptance Criteria\n\n- Clamp returns min below range, max above range, and value inside range.\n- go test ./... passes.\n\n## Out of Scope\n\n- Network\n- External services",
		Team:           application.LinearTeam{ID: "team", Key: "IFAN", Name: "I-Fan"},
		State:          application.LinearState{ID: "todo", Name: "Todo", Type: "backlog"},
		Labels:         []application.LinearLabel{{ID: "agent", Name: "agent:codex"}, {ID: "repository", Name: "repo:test-project"}},
		Cycle:          application.LinearCycle{ID: "cycle", Number: 1, StartsAt: created, EndsAt: updated.Add(24 * time.Hour), IsActive: true},
		BranchName:     "ifan/ifan-fixture-clamp",
		SourceRevision: "fixture-v1",
		CreatedAt:      created,
		UpdatedAt:      updated,
		ObservedAt:     updated,
	}
}

type testWorktrees struct{ manager gitadapter.WorktreeManager }

func (w testWorktrees) Provision(ctx context.Context, s application.WorktreeSpec) (application.WorktreeRecord, error) {
	e, err := w.manager.Provision(ctx, gitadapter.WorktreeRequest{SourcePath: s.SourcePath, OriginPath: s.OriginPath, BaseBranch: s.BaseBranch, Branch: s.Branch, Path: s.Path, Nonce: s.Nonce})
	if err != nil {
		return application.WorktreeRecord{}, err
	}
	return application.WorktreeRecord{SourcePath: e.SourcePath, OriginPath: e.OriginPath, Path: e.Path, Branch: e.Branch, BaseBranch: e.BaseBranch, BaseSHA: e.BaseSHA, Nonce: e.Nonce}, nil
}

type crashAfterWorktree struct {
	testWorktrees
	once bool
}

func (w *crashAfterWorktree) Provision(ctx context.Context, s application.WorktreeSpec) (application.WorktreeRecord, error) {
	record, err := w.testWorktrees.Provision(ctx, s)
	if err == nil && w.once {
		w.once = false
		return application.WorktreeRecord{}, errors.New("simulated crash after worktree creation")
	}
	return record, err
}
func (w testWorktrees) ValidateOwned(ctx context.Context, r application.WorktreeRecord) error {
	return w.manager.ValidateOwned(ctx, gitadapter.WorktreeEvidence{SourcePath: r.SourcePath, OriginPath: r.OriginPath, Path: r.Path, Branch: r.Branch, BaseBranch: r.BaseBranch, BaseSHA: r.BaseSHA, Nonce: r.Nonce})
}

type durableFakeProcess struct {
	mu                                            sync.Mutex
	needsDecision                                 bool
	failFirstReview                               bool
	reviewFindings                                bool
	noChangeOnResume                              bool
	blockResumeUntilContextDone                   bool
	implementationCalls, resumeCalls, reviewCalls int
	resumeArgs                                    []string
}

func (p *durableFakeProcess) Run(ctx context.Context, s processadapter.Spec) (processadapter.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	stdout := ""
	stderr := ""
	if slices.Equal(s.Args, []string{"--version"}) {
		stdout = "codex-cli fake\n"
	} else if slices.Equal(s.Args, []string{"exec", "--help"}) {
		stdout = "--model\n--ignore-user-config\n--sandbox\n--cd\n--json\n--output-schema\n--output-last-message\n--ephemeral\n"
	} else if slices.Equal(s.Args, []string{"exec", "resume", "--help"}) {
		stdout = "Usage: codex exec resume [OPTIONS] [SESSION_ID]\n--model\n--ignore-user-config\n--config\n--json\n--output-schema\n--output-last-message\n"
	} else if len(s.Args) > 1 && s.Args[0] == "exec" {
		if s.Args[1] == "resume" {
			p.resumeCalls++
			p.resumeArgs = append([]string(nil), s.Args...)
			if p.blockResumeUntilContextDone {
				<-ctx.Done()
				return processadapter.Result{}, ctx.Err()
			}
			if !p.noChangeOnResume {
				mustWriteFile(filepath.Join(s.WorkingDir, "mathutil", "clamp.go"), "package mathutil\n\nfunc Clamp(value, min, max int) int { if value < min { return min }; if value > max { return max }; return value }\n")
				mustWriteFile(filepath.Join(s.WorkingDir, "mathutil", "clamp_test.go"), "package mathutil\n\nimport \"testing\"\n\nfunc TestClamp(t *testing.T) { tests := []struct{ v, min, max, want int }{{-1,0,5,0},{3,0,5,3},{9,0,5,5}}; for _, tt := range tests { if got := Clamp(tt.v,tt.min,tt.max); got != tt.want { t.Fatalf(\"got %d want %d\",got,tt.want) } } }\n")
			}
			writeLastMessage(s.Args, completedOutcome)
			sessionID := "implementation-session"
			if slices.Contains(s.Args, "recovered-session") {
				sessionID = "recovered-session"
			}
			stdout = fmt.Sprintf("{\"type\":\"thread.started\",\"thread_id\":%q}\n", sessionID)
		} else if argument(s.Args, "--sandbox") == "read-only" {
			p.reviewCalls++
			head := gitHead(s.WorkingDir)
			verdict, summary, findings := "pass", "ready", "[]"
			if p.failFirstReview && p.reviewCalls == 1 {
				verdict, summary = "failed", "transient reviewer failure"
			}
			if p.reviewFindings && p.reviewCalls == 1 {
				verdict, summary = "findings", "actionable finding"
				findings = `[{"id":"f1","severity":"medium","title":"Finding","body":"Fix it","file":null,"line":null}]`
			}
			writeLastMessage(s.Args, fmt.Sprintf(`{"verdict":%q,"summary":%q,"reviewed_head_sha":%q,"findings":%s}`, verdict, summary, head, findings))
			stdout = fmt.Sprintf("{\"type\":\"thread.started\",\"thread_id\":\"review-session-%d\"}\n", p.reviewCalls)
		} else {
			p.implementationCalls++
			if p.needsDecision {
				writeLastMessage(s.Args, decisionOutcome)
				stdout = "{\"type\":\"thread.started\",\"thread_id\":\"implementation-session\"}\n"
			} else {
				mustWriteFile(filepath.Join(s.WorkingDir, "mathutil", "clamp.go"), "package mathutil\n\nfunc Clamp(value, min, max int) int { if value < min { return min }; if value > max { return max }; return value }\n")
				mustWriteFile(filepath.Join(s.WorkingDir, "mathutil", "clamp_test.go"), "package mathutil\n\nimport \"testing\"\n\nfunc TestClamp(t *testing.T) { if Clamp(9,0,5) != 5 { t.Fatal(\"bad clamp\") } }\n")
				writeLastMessage(s.Args, completedOutcome)
				stdout = "{\"type\":\"thread.started\",\"thread_id\":\"implementation-session\"}\n"
			}
		}
	}
	if s.StdoutPath != "" {
		if err := exclusiveWrite(s.StdoutPath, stdout); err != nil {
			return processadapter.Result{}, err
		}
	}
	if s.StderrPath != "" {
		if err := exclusiveWrite(s.StderrPath, stderr); err != nil {
			return processadapter.Result{}, err
		}
	}
	return processadapter.Result{ExitCode: 0, StdoutPath: s.StdoutPath, StderrPath: s.StderrPath}, nil
}

const completedOutcome = `{"status":"completed","summary":"implemented","decision_request":null,"discovered_issues":[],"suggested_checks":[],"implementation_sha":null}`
const decisionOutcome = `{"status":"needs_human_decision","summary":"choose boundary behavior","decision_request":{"question":"Which boundary policy?","context":"The fixture requests a choice.","options":[{"id":"inclusive","description":"Use inclusive bounds"},{"id":"exclusive","description":"Use exclusive bounds"}],"recommendation":"inclusive","blocking_reason":"Behavior must be chosen"},"discovered_issues":[],"suggested_checks":[],"implementation_sha":null}`

func newController(t *testing.T, store application.RunStore, lab localLab, process *durableFakeProcess, git application.DurableGit) *application.LocalController {
	t.Helper()
	workspace := gitadapter.Workspace{}
	registry := verifier.NewRegistry(map[string]verifier.Command{"fixture-go-test": {Program: "go", Args: []string{"test", "./..."}}}, processadapter.OSRunner{}, workspace)
	return application.NewLocalController(store, testWorktrees{}, codex.NewExecutor(process, "codex"), registry, git, "codex", lab.worktrees)
}
func startInput(lab localLab) application.LocalStartInput {
	s := lab.snapshot
	return application.LocalStartInput{Task: s.Task, RawIssueJSON: s.RawJSON, RawIssueHash: s.RawHash, NormalizedJSON: s.NormalizedJSON, TaskHash: s.TaskHash, IdempotencyKey: s.IdempotencyKey, Repository: lab.repository, RunRoot: lab.runs, WorktreeRoot: lab.worktrees}
}

func TestLocalDurableHappyPathAndDuplicateStart(t *testing.T) {
	lab := newLocalLab(t)
	store, err := storeadapter.Open(lab.db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	process := &durableFakeProcess{}
	controller := newController(t, store, lab, process, gitadapter.Workspace{})
	run, err := controller.Start(context.Background(), startInput(lab))
	if err != nil {
		t.Fatal(err)
	}
	if run.State != domain.StateApprovalReady {
		t.Fatalf("state=%s", run.State)
	}
	inspection, err := store.Inspect(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempts := len(inspection.Attempts)
	if attempts != 2 {
		t.Fatalf("attempts=%d", attempts)
	}
	if _, err := controller.Start(context.Background(), startInput(lab)); err != nil {
		t.Fatal(err)
	}
	after, _ := store.Inspect(context.Background(), run.ID)
	if len(after.Attempts) != attempts || process.implementationCalls != 1 || process.reviewCalls != 1 {
		t.Fatal("duplicate start repeated a completed step")
	}
}

func TestArtifactRootRejectsPrecreatedDirectoryAndSymlink(t *testing.T) {
	for _, kind := range []string{"directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			lab := newLocalLab(t)
			target := filepath.Join(lab.runs, lab.snapshot.Task.RunID)
			if kind == "directory" {
				if err := os.Mkdir(target, 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				outside := t.TempDir()
				if err := os.Symlink(outside, target); err != nil {
					t.Fatal(err)
				}
			}
			store, err := storeadapter.Open(lab.db)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			process := &durableFakeProcess{}
			_, err = newController(t, store, lab, process, gitadapter.Workspace{}).Start(context.Background(), startInput(lab))
			if err == nil || !strings.Contains(err.Error(), "existed before") {
				t.Fatalf("error=%v", err)
			}
			if process.implementationCalls != 0 {
				t.Fatal("Codex ran with unowned artifact root")
			}
		})
	}
}

func TestRestartRejectsAttemptsSymlinkEscape(t *testing.T) {
	lab := newLocalLab(t)
	store, err := storeadapter.Open(lab.db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	process := &durableFakeProcess{needsDecision: true}
	controller := newController(t, store, lab, process, gitadapter.Workspace{})
	run, err := controller.Start(context.Background(), startInput(lab))
	if err != nil {
		t.Fatal(err)
	}
	attempts := filepath.Join(run.ArtifactRoot, "attempts")
	backup := attempts + "-backup"
	if err := os.Rename(attempts, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), attempts); err != nil {
		t.Fatal(err)
	}
	_, err = controller.Continue(context.Background(), run.ID, &application.Decision{ChoiceID: "inclusive", Instructions: "Use inclusive bounds."})
	if err == nil || !strings.Contains(err.Error(), "attempts path must be a real directory") {
		t.Fatalf("error=%v", err)
	}
	if process.resumeCalls != 0 {
		t.Fatal("resume wrote through attempts symlink")
	}
}

type exitVerifierProcess struct{}

func (exitVerifierProcess) Run(_ context.Context, s processadapter.Spec) (processadapter.Result, error) {
	if err := exclusiveWrite(s.StdoutPath, "failed verifier\n"); err != nil {
		return processadapter.Result{}, err
	}
	if err := exclusiveWrite(s.StderrPath, "failure detail\n"); err != nil {
		return processadapter.Result{}, err
	}
	return processadapter.Result{ExitCode: 7, StdoutPath: s.StdoutPath, StderrPath: s.StderrPath}, nil
}

func TestFailedVerifierEvidenceIsDurableAndInspectable(t *testing.T) {
	lab := newLocalLab(t)
	store, err := storeadapter.Open(lab.db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	process := &durableFakeProcess{}
	workspace := gitadapter.Workspace{}
	registry := verifier.NewRegistry(map[string]verifier.Command{"fixture-go-test": {Program: "go", Args: []string{"test", "./..."}}}, exitVerifierProcess{}, workspace)
	controller := application.NewLocalController(store, testWorktrees{}, codex.NewExecutor(process, "codex"), registry, workspace, "codex", lab.worktrees)
	run, err := controller.Start(context.Background(), startInput(lab))
	if err == nil {
		t.Fatal("expected verifier failure")
	}
	if run.State != domain.StateVerifying {
		t.Fatalf("state=%s", run.State)
	}
	inspection, inspectErr := store.Inspect(context.Background(), run.ID)
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if len(inspection.Verifications) != 1 {
		t.Fatalf("verifications=%+v", inspection.Verifications)
	}
	record := inspection.Verifications[0]
	if record.ExitCode != 7 || record.StdoutHash == "" || record.StderrHash == "" || record.EvidencePath == "" {
		t.Fatalf("record=%+v", record)
	}
	if _, statErr := os.Stat(record.EvidencePath); statErr != nil {
		t.Fatal(statErr)
	}
}

type failSecondVerifierProcess struct {
	mu    sync.Mutex
	calls int
}

func (p *failSecondVerifierProcess) Run(ctx context.Context, s processadapter.Spec) (processadapter.Result, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	if call == 2 {
		if err := exclusiveWrite(s.StdoutPath, "transient failure\n"); err != nil {
			return processadapter.Result{}, err
		}
		if err := exclusiveWrite(s.StderrPath, "retryable\n"); err != nil {
			return processadapter.Result{}, err
		}
		return processadapter.Result{ExitCode: 7, StdoutPath: s.StdoutPath, StderrPath: s.StderrPath}, nil
	}
	return (processadapter.OSRunner{}).Run(ctx, s)
}

func TestCandidateVerificationFailureThenSuccessfulRestartAdvances(t *testing.T) {
	lab := newLocalLab(t)
	store, err := storeadapter.Open(lab.db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	codexProcess := &durableFakeProcess{}
	workspace := gitadapter.Workspace{}
	verificationProcess := &failSecondVerifierProcess{}
	registry := verifier.NewRegistry(map[string]verifier.Command{"fixture-go-test": {Program: "go", Args: []string{"test", "./..."}}}, verificationProcess, workspace)
	controller := application.NewLocalController(store, testWorktrees{}, codex.NewExecutor(codexProcess, "codex"), registry, workspace, "codex", lab.worktrees)
	run, err := controller.Start(context.Background(), startInput(lab))
	if err == nil {
		t.Fatal("expected transient candidate verification failure")
	}
	if run.State != domain.StateVerifying || run.CandidateHead == "" {
		t.Fatalf("run=%+v", run)
	}
	run, err = application.NewLocalController(store, testWorktrees{}, codex.NewExecutor(codexProcess, "codex"), registry, workspace, "codex", lab.worktrees).Continue(context.Background(), run.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != domain.StateApprovalReady {
		t.Fatalf("state=%s error=%s", run.State, run.LastError)
	}
	inspection, _ := store.Inspect(context.Background(), run.ID)
	failed, passed := false, false
	for _, record := range inspection.Verifications {
		if record.Phase == "candidate" && record.VerifiedHead == run.CandidateHead {
			if record.ExitCode == 0 {
				passed = true
			} else {
				failed = true
			}
		}
	}
	if !failed || !passed {
		t.Fatalf("verification history=%+v", inspection.Verifications)
	}
}

func TestRestartRecoversProvisionedOwnedWorktree(t *testing.T) {
	lab := newLocalLab(t)
	store, err := storeadapter.Open(lab.db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	process := &durableFakeProcess{}
	workspace := gitadapter.Workspace{}
	registry := verifier.NewRegistry(map[string]verifier.Command{"fixture-go-test": {Program: "go", Args: []string{"test", "./..."}}}, processadapter.OSRunner{}, workspace)
	crashing := &crashAfterWorktree{once: true}
	controller := application.NewLocalController(store, crashing, codex.NewExecutor(process, "codex"), registry, workspace, "codex", lab.worktrees)
	run, err := controller.Start(context.Background(), startInput(lab))
	if err == nil {
		t.Fatal("expected worktree boundary crash")
	}
	if run.State != domain.StateProvisioning {
		t.Fatalf("state=%s", run.State)
	}
	run, err = newController(t, store, lab, process, workspace).Continue(context.Background(), run.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != domain.StateApprovalReady {
		t.Fatalf("state=%s error=%s", run.State, run.LastError)
	}
	inspection, err := store.Inspect(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range inspection.Resources {
		if resource.Kind != "worktree" && resource.Kind != "branch" {
			continue
		}
		var evidence struct {
			Nonce string `json:"nonce"`
		}
		if err := json.Unmarshal([]byte(resource.CreationEvidence), &evidence); err != nil || evidence.Nonce == "" {
			t.Fatalf("recovered resource=%+v nonce=%q err=%v", resource, evidence.Nonce, err)
		}
	}
}

func TestExplicitSessionResumeSurvivesControllerRestart(t *testing.T) {
	lab := newLocalLab(t)
	store, err := storeadapter.Open(lab.db)
	if err != nil {
		t.Fatal(err)
	}
	process := &durableFakeProcess{needsDecision: true}
	run, err := newController(t, store, lab, process, gitadapter.Workspace{}).Start(context.Background(), startInput(lab))
	if err != nil {
		t.Fatal(err)
	}
	if run.State != domain.StateAwaitingHumanDecision || run.ImplementationSession != "implementation-session" {
		t.Fatalf("run=%+v", run)
	}
	store.Close()
	store, err = storeadapter.Open(lab.db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run, err = newController(t, store, lab, process, gitadapter.Workspace{}).Continue(context.Background(), run.ID, &application.Decision{ChoiceID: "inclusive", Instructions: "Use inclusive min and max bounds."})
	if err != nil {
		t.Fatal(err)
	}
	if run.State != domain.StateApprovalReady {
		t.Fatalf("state=%s error=%s", run.State, run.LastError)
	}
	if process.resumeCalls != 1 || slices.Contains(process.resumeArgs, "--last") || !slices.Contains(process.resumeArgs, "implementation-session") {
		t.Fatalf("resume args=%v", process.resumeArgs)
	}
	inspection, _ := store.Inspect(context.Background(), run.ID)
	seen := map[string]bool{}
	for _, attempt := range inspection.Attempts {
		if seen[attempt.ArtifactDir] {
			t.Fatal("attempt artifact directory was reused")
		}
		seen[attempt.ArtifactDir] = true
	}
}

func TestPersistedDecisionTamperFailsBeforeResume(t *testing.T) {
	lab := newLocalLab(t)
	store, err := storeadapter.Open(lab.db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	process := &durableFakeProcess{needsDecision: true}
	wrapper := &failAfterTransitionStore{RunStore: store, from: domain.StateAwaitingHumanDecision, to: domain.StateExecuting, remaining: 1}
	controller := newController(t, wrapper, lab, process, gitadapter.Workspace{})
	run, err := controller.Start(context.Background(), startInput(lab))
	if err != nil {
		t.Fatal(err)
	}
	run, err = controller.Continue(context.Background(), run.ID, &application.Decision{ChoiceID: "inclusive", Instructions: "Use inclusive bounds."})
	if err == nil || run.State != domain.StateExecuting {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	inspection, _ := store.Inspect(context.Background(), run.ID)
	var evidence struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(inspection.Timeline[len(inspection.Timeline)-1].EvidenceReference), &evidence); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, evidence.Path, `{"choice_id":"exclusive","instructions":"tampered"}`)
	_, err = newController(t, store, lab, process, gitadapter.Workspace{}).Continue(context.Background(), run.ID, nil)
	if err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("error=%v", err)
	}
	if process.resumeCalls != 0 {
		t.Fatal("resume used tampered decision")
	}
}

func TestTransientFailedReviewCanRetrySameHead(t *testing.T) {
	lab := newLocalLab(t)
	store, err := storeadapter.Open(lab.db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	process := &durableFakeProcess{failFirstReview: true}
	controller := newController(t, store, lab, process, gitadapter.Workspace{})
	run, err := controller.Start(context.Background(), startInput(lab))
	if err == nil {
		t.Fatal("expected transient failed review")
	}
	if run.State != domain.StateFreshReview {
		t.Fatalf("state=%s", run.State)
	}
	run, err = newController(t, store, lab, process, gitadapter.Workspace{}).Continue(context.Background(), run.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != domain.StateApprovalReady || process.reviewCalls != 2 {
		t.Fatalf("run=%+v reviews=%d", run, process.reviewCalls)
	}
	inspection, _ := store.Inspect(context.Background(), run.ID)
	if len(inspection.Reviews) != 2 || inspection.Reviews[0].Verdict != "failed" || inspection.Reviews[1].Verdict != "pass" {
		t.Fatalf("reviews=%+v", inspection.Reviews)
	}
}

func TestNoChangeRepairReusesExactRepairBaseAndExactHeadEvidence(t *testing.T) {
	lab := newLocalLab(t)
	store, err := storeadapter.Open(lab.db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	process := &durableFakeProcess{noChangeOnResume: true}
	controller := newController(t, store, lab, process, gitadapter.Workspace{})
	input := startInput(lab)
	input.Task.Policy.MaxRepairAttempts = domain.DefaultMaxRepairAttempts
	input.NormalizedJSON, err = json.Marshal(input.Task)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(input.NormalizedJSON)
	input.TaskHash = hex.EncodeToString(digest[:])
	run, err := controller.Start(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	base := run.CandidateHead
	for _, transition := range []struct{ from, to domain.State }{
		{domain.StateApprovalReady, domain.StatePushingBranch},
		{domain.StatePushingBranch, domain.StateBranchPushed},
		{domain.StateBranchPushed, domain.StateOpeningPR},
		{domain.StateOpeningPR, domain.StatePROpen},
		{domain.StatePROpen, domain.StateReconcilingReviews},
		{domain.StateReconcilingReviews, domain.StateRepairing},
	} {
		if err := store.Transition(context.Background(), run.ID, transition.from, transition.to, "fixture transition", "fixture", base); err != nil {
			t.Fatal(err)
		}
	}
	run, err = controller.Repair(context.Background(), run.ID, "the normalized finding no longer requires a source change")
	if err != nil || run.State != domain.StateApprovalReady || run.CandidateHead != base {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	if process.resumeCalls != 1 || process.reviewCalls != 2 {
		t.Fatalf("resumes=%d reviews=%d", process.resumeCalls, process.reviewCalls)
	}
}

func TestLaterRepairUsesFirstRepairDeadlineAndStopsAtManualIntervention(t *testing.T) {
	lab := newLocalLab(t)
	store, err := storeadapter.Open(lab.db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	process := &durableFakeProcess{}
	controller := newController(t, store, lab, process, gitadapter.Workspace{})
	input := startInput(lab)
	input.Task.Policy.MaxRepairAttempts = domain.DefaultMaxRepairAttempts
	input.NormalizedJSON, err = json.Marshal(input.Task)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(input.NormalizedJSON)
	input.TaskHash = hex.EncodeToString(digest[:])
	run, err := controller.Start(context.Background(), input)
	if err != nil || run.State != domain.StateApprovalReady {
		t.Fatalf("initial run=%+v err=%v", run, err)
	}
	moveToRepairing := func(run application.Run) {
		for _, transition := range []struct{ from, to domain.State }{
			{domain.StateApprovalReady, domain.StatePushingBranch},
			{domain.StatePushingBranch, domain.StateBranchPushed},
			{domain.StateBranchPushed, domain.StateOpeningPR},
			{domain.StateOpeningPR, domain.StatePROpen},
			{domain.StatePROpen, domain.StateReconcilingReviews},
			{domain.StateReconcilingReviews, domain.StateRepairing},
		} {
			if err := store.Transition(context.Background(), run.ID, transition.from, transition.to, "fixture transition", "fixture", run.CandidateHead); err != nil {
				t.Fatal(err)
			}
		}
	}
	moveToRepairing(run)
	run, err = controller.Repair(context.Background(), run.ID, "first bounded repair")
	if err != nil || run.State != domain.StateApprovalReady || process.resumeCalls != 1 {
		t.Fatalf("first repair run=%+v resumes=%d err=%v", run, process.resumeCalls, err)
	}
	moveToRepairing(run)
	process.blockResumeUntilContextDone = true
	anchored := firstRepairDeadlineStore{RunStore: store, firstRepairAt: time.Now().UTC().Add(-30*time.Minute + 750*time.Millisecond)}
	deadlineController := newController(t, anchored, lab, process, gitadapter.Workspace{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	run, err = deadlineController.Repair(ctx, run.ID, "second bounded repair")
	if err == nil || run.State != domain.StateManualIntervention {
		t.Fatalf("second repair run=%+v err=%v", run, err)
	}
	if process.resumeCalls != 2 {
		t.Fatalf("second repair did not start the existing implementation session: resumes=%d", process.resumeCalls)
	}
	if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
		t.Fatalf("later repair exceeded the first repair deadline: elapsed=%s", elapsed)
	}
}

func TestCallerDeadlineDoesNotBecomeRepairPolicyManualIntervention(t *testing.T) {
	_, store, process, run := beginInterruptedRepair(t)
	defer store.Close()
	if run.State != domain.StateExecuting || process.resumeCalls != 1 {
		t.Fatalf("run=%+v resumes=%d", run, process.resumeCalls)
	}
}

func TestExpiredRepairDeadlineStopsVerificationAndFreshReview(t *testing.T) {
	for _, state := range []domain.State{domain.StateVerifying, domain.StateFreshReview} {
		t.Run(string(state), func(t *testing.T) {
			lab, store, process, run := beginInterruptedRepair(t)
			defer store.Close()
			if err := store.Transition(context.Background(), run.ID, domain.StateExecuting, domain.StateVerifying, "fixture repair progression", "fixture", run.CandidateHead); err != nil {
				t.Fatal(err)
			}
			if state == domain.StateFreshReview {
				if err := store.Transition(context.Background(), run.ID, domain.StateVerifying, domain.StateFreshReview, "fixture repair progression", "fixture", run.CandidateHead); err != nil {
					t.Fatal(err)
				}
			}
			expired := firstRepairDeadlineStore{RunStore: store, firstRepairAt: time.Now().UTC().Add(-31 * time.Minute)}
			updated, err := newController(t, expired, lab, process, gitadapter.Workspace{}).Continue(context.Background(), run.ID, nil)
			if err == nil || updated.State != domain.StateManualIntervention {
				t.Fatalf("state=%s updated=%+v err=%v", state, updated, err)
			}
		})
	}
}

func beginInterruptedRepair(t *testing.T) (localLab, *storeadapter.Store, *durableFakeProcess, application.Run) {
	t.Helper()
	lab := newLocalLab(t)
	store, err := storeadapter.Open(lab.db)
	if err != nil {
		t.Fatal(err)
	}
	process := &durableFakeProcess{}
	controller := newController(t, store, lab, process, gitadapter.Workspace{})
	input := startInput(lab)
	input.Task.Policy.MaxRepairAttempts = domain.DefaultMaxRepairAttempts
	input.NormalizedJSON, err = json.Marshal(input.Task)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	digest := sha256.Sum256(input.NormalizedJSON)
	input.TaskHash = hex.EncodeToString(digest[:])
	run, err := controller.Start(context.Background(), input)
	if err != nil || run.State != domain.StateApprovalReady {
		store.Close()
		t.Fatalf("initial run=%+v err=%v", run, err)
	}
	for _, transition := range []struct{ from, to domain.State }{
		{domain.StateApprovalReady, domain.StatePushingBranch},
		{domain.StatePushingBranch, domain.StateBranchPushed},
		{domain.StateBranchPushed, domain.StateOpeningPR},
		{domain.StateOpeningPR, domain.StatePROpen},
		{domain.StatePROpen, domain.StateReconcilingReviews},
		{domain.StateReconcilingReviews, domain.StateRepairing},
	} {
		if err := store.Transition(context.Background(), run.ID, transition.from, transition.to, "fixture transition", "fixture", run.CandidateHead); err != nil {
			store.Close()
			t.Fatal(err)
		}
	}
	process.blockResumeUntilContextDone = true
	callerCtx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	run, err = controller.Repair(callerCtx, run.ID, "caller deadline fixture")
	if err == nil || run.State != domain.StateExecuting {
		store.Close()
		t.Fatalf("interrupted repair run=%+v err=%v", run, err)
	}
	return lab, store, process, run
}

func TestFreshReviewFindingsPersistAndResumeBoundedRepairWithSameSession(t *testing.T) {
	lab := newLocalLab(t)
	store, err := storeadapter.Open(lab.db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	process := &durableFakeProcess{reviewFindings: true}
	controller := newController(t, store, lab, process, gitadapter.Workspace{})
	input := startInput(lab)
	input.Task.Policy.MaxRepairAttempts = domain.DefaultMaxRepairAttempts
	input.NormalizedJSON, err = json.Marshal(input.Task)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(input.NormalizedJSON)
	input.TaskHash = hex.EncodeToString(digest[:])
	run, err := controller.Start(context.Background(), input)
	if err != nil || run.State != domain.StateRepairing {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	inspection, err := store.Inspect(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(inspection.Findings) != 1 {
		t.Fatalf("findings=%+v", inspection.Findings)
	}
	finding := inspection.Findings[0]
	findingDigest := sha256.Sum256([]byte(finding.Body))
	if finding.Source != "controller_fresh_review" || finding.SourceID != "fresh-review:f1" || finding.HeadSHA != run.CandidateHead || finding.BodyDigest != hex.EncodeToString(findingDigest[:]) {
		t.Fatalf("finding=%+v candidate=%s", finding, run.CandidateHead)
	}
	if len(inspection.Timeline) == 0 || inspection.Timeline[len(inspection.Timeline)-1].From != domain.StateFreshReview || inspection.Timeline[len(inspection.Timeline)-1].To != domain.StateRepairing || inspection.Timeline[len(inspection.Timeline)-1].BoundHead != run.CandidateHead {
		t.Fatalf("timeline=%+v", inspection.Timeline)
	}
	run, err = newController(t, store, lab, process, gitadapter.Workspace{}).RepairFindings(context.Background(), run.ID, inspection.Findings)
	if err != nil || run.State != domain.StateApprovalReady {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	if process.resumeCalls != 1 || process.reviewCalls != 2 {
		t.Fatalf("resumes=%d reviews=%d", process.resumeCalls, process.reviewCalls)
	}
}

func TestProductionDriverRoutesFreshReviewFindingsIntoBoundedSameSessionRepair(t *testing.T) {
	lab := newLocalLab(t)
	store, err := storeadapter.Open(lab.db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	repository := lab.repository
	repository.AllowedOperatorLogins = []string{"operator"}
	process := &durableFakeProcess{reviewFindings: true}
	local := newController(t, store, lab, process, gitadapter.Workspace{})
	reader := &productionLinearReader{source: productionLinearSource()}
	admission, err := application.NewLinearAdmissionService(reader, productionLinearResolver{repository: repository}, store, local)
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := application.NewProductionCoordinator(admission, local, store)
	if err != nil {
		t.Fatal(err)
	}
	requester := application.Requester{ID: "operator", Kind: "github_login"}
	started, _, err := admission.Start(context.Background(), application.LinearStartCommand{Requester: requester, Identifier: "IFAN-FIXTURE"})
	if err != nil || started.Run.State != domain.StateRepairing {
		t.Fatalf("started=%+v err=%v", started, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	driver, err := application.NewProductionDriver(coordinator, store, application.ProductionDriverPorts{}, application.ProductionDriverPolicy{PollInterval: time.Second, MaxImmediateAction: 1}, func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = driver.Drive(ctx, application.ProductionDriveCommand{Requester: requester, RunID: started.Run.RunID, Repository: repository.CanonicalRepository, IdempotencyKey: started.Run.IdempotencyKey})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("driver error=%v", err)
	}
	run, err := store.GetRun(context.Background(), started.Run.RunID)
	if err != nil || run.State != domain.StateApprovalReady {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	if process.resumeCalls != 1 || process.reviewCalls != 2 || reader.reads != 2 {
		t.Fatalf("resumes=%d reviews=%d linear_reads=%d", process.resumeCalls, process.reviewCalls, reader.reads)
	}
}

type failingTransitionStore struct {
	application.RunStore
	from, to  domain.State
	remaining int
}

type firstRepairDeadlineStore struct {
	application.RunStore
	firstRepairAt time.Time
}

func (s firstRepairDeadlineStore) Inspect(ctx context.Context, runID string) (application.RunInspection, error) {
	inspection, err := s.RunStore.Inspect(ctx, runID)
	if err != nil {
		return inspection, err
	}
	for index := range inspection.Timeline {
		if inspection.Timeline[index].From == domain.StateRepairing && inspection.Timeline[index].To == domain.StateExecuting {
			inspection.Timeline[index].CreatedAt = s.firstRepairAt
			break
		}
	}
	return inspection, nil
}

func (s firstRepairDeadlineStore) SaveFinding(ctx context.Context, finding application.FindingRecord) error {
	persister, ok := s.RunStore.(interface {
		SaveFinding(context.Context, application.FindingRecord) error
	})
	if !ok {
		return errors.New("finding persistence is unavailable")
	}
	return persister.SaveFinding(ctx, finding)
}

type failAfterTransitionStore struct {
	application.RunStore
	from, to  domain.State
	remaining int
}

func (s *failAfterTransitionStore) Transition(ctx context.Context, id string, from, to domain.State, reason, evidence, head string) error {
	err := s.RunStore.Transition(ctx, id, from, to, reason, evidence, head)
	if err == nil && from == s.from && to == s.to && s.remaining > 0 {
		s.remaining--
		return errors.New("simulated crash after durable transition")
	}
	return err
}

func TestRestartRecoversStartedAttemptSessionAndResumesExplicitly(t *testing.T) {
	lab := newLocalLab(t)
	store, err := storeadapter.Open(lab.db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	process := &durableFakeProcess{}
	wrapper := &failAfterTransitionStore{RunStore: store, from: domain.StateProvisioning, to: domain.StateExecuting, remaining: 1}
	run, err := newController(t, wrapper, lab, process, gitadapter.Workspace{}).Start(context.Background(), startInput(lab))
	if err == nil || run.State != domain.StateExecuting {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	directory := filepath.Join(run.ArtifactRoot, "attempts", "interrupted")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginAttempt(context.Background(), run.ID, "implementation", codex.ImplementationModel, directory); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(directory, "implementation.stdout.jsonl"), "{\"type\":\"thread.started\",\"thread_id\":\"recovered-session\"}\n")
	mustWrite(t, filepath.Join(directory, "implementation.stderr.txt"), "")
	run, err = newController(t, store, lab, process, gitadapter.Workspace{}).Continue(context.Background(), run.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != domain.StateApprovalReady || run.ImplementationSession != "recovered-session" {
		t.Fatalf("run=%+v", run)
	}
	if process.resumeCalls != 1 || !slices.Contains(process.resumeArgs, "recovered-session") || slices.Contains(process.resumeArgs, "--last") {
		t.Fatalf("resume args=%v", process.resumeArgs)
	}
	if argument(process.resumeArgs, "--model") != codex.ImplementationModel {
		t.Fatalf("resume model=%q args=%v", argument(process.resumeArgs, "--model"), process.resumeArgs)
	}
	inspection, _ := store.Inspect(context.Background(), run.ID)
	if inspection.Run.ImplementationModel != codex.ImplementationModel || inspection.Run.ReviewModel != codex.ReviewModel {
		t.Fatalf("persisted model policy=%+v", inspection.Run)
	}
	found := false
	for _, attempt := range inspection.Attempts {
		if attempt.ErrorCategory == "controller_restart_session_recovered" && attempt.SessionID == "recovered-session" {
			found = true
		}
	}
	if !found {
		t.Fatal("recovered interrupted attempt evidence missing")
	}
}

func TestControllerRefusesCompetingActiveLease(t *testing.T) {
	lab := newLocalLab(t)
	store, err := storeadapter.Open(lab.db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	input := startInput(lab)
	repositoryJSON, _ := json.Marshal(input.Repository)
	_, _, err = store.CreateRun(context.Background(), application.CreateRunInput{Run: application.Run{ID: input.Task.RunID, IssueID: input.Task.IssueID, IdempotencyKey: input.IdempotencyKey, SourceRevision: input.Task.SourceRevision, RawIssueJSON: string(input.RawIssueJSON), RawIssueHash: input.RawIssueHash, NormalizedTaskJSON: string(input.NormalizedJSON), TaskHash: input.TaskHash, Repository: input.Task.Repository, RepositoryConfigJSON: string(repositoryJSON), BaseBranch: input.Task.BaseBranch, WorkingBranch: input.Task.WorkingBranch, WorktreePath: filepath.Join(lab.worktrees, input.Task.RunID), ArtifactRoot: filepath.Join(lab.runs, input.Task.RunID)}})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := store.AcquireLease(context.Background(), input.Task.RunID, "other-controller", time.Now().Add(time.Minute)); err != nil || !ok {
		t.Fatalf("lease=%v err=%v", ok, err)
	}
	_, err = newController(t, store, lab, &durableFakeProcess{}, gitadapter.Workspace{}).Continue(context.Background(), input.Task.RunID, nil)
	if err == nil || !strings.Contains(err.Error(), "actively leased") {
		t.Fatalf("error=%v", err)
	}
}

func (s *failingTransitionStore) Transition(ctx context.Context, id string, from, to domain.State, reason, evidence, head string) error {
	if from == s.from && to == s.to && s.remaining > 0 {
		s.remaining--
		return errors.New("simulated durable boundary crash")
	}
	return s.RunStore.Transition(ctx, id, from, to, reason, evidence, head)
}

func TestRestartReusesVerificationAndReviewEvidence(t *testing.T) {
	for _, boundary := range []struct {
		name     string
		from, to domain.State
	}{{"after implementation", domain.StateExecuting, domain.StateVerifying}, {"after verification", domain.StateVerifying, domain.StateFreshReview}, {"after review", domain.StateFreshReview, domain.StateApprovalReady}} {
		t.Run(boundary.name, func(t *testing.T) {
			lab := newLocalLab(t)
			store, err := storeadapter.Open(lab.db)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			process := &durableFakeProcess{}
			wrapper := &failingTransitionStore{RunStore: store, from: boundary.from, to: boundary.to, remaining: 1}
			run, err := newController(t, wrapper, lab, process, gitadapter.Workspace{}).Start(context.Background(), startInput(lab))
			if err == nil {
				t.Fatal("expected boundary crash")
			}
			before, _ := store.Inspect(context.Background(), run.ID)
			reviewCalls := process.reviewCalls
			run, err = newController(t, store, lab, process, gitadapter.Workspace{}).Continue(context.Background(), run.ID, nil)
			if err != nil {
				t.Fatal(err)
			}
			if run.State != domain.StateApprovalReady {
				t.Fatalf("state=%s", run.State)
			}
			after, _ := store.Inspect(context.Background(), run.ID)
			if boundary.to == domain.StateVerifying && process.implementationCalls != 1 {
				t.Fatal("implementation was rerun")
			}
			if boundary.to == domain.StateFreshReview && len(after.Verifications) != len(before.Verifications) {
				t.Fatal("verification was rerun")
			}
			if boundary.to == domain.StateApprovalReady && process.reviewCalls != reviewCalls {
				t.Fatal("review was rerun")
			}
		})
	}
}

type crashAfterCommitGit struct {
	gitadapter.Workspace
	once bool
}

func (g *crashAfterCommitGit) CommitCandidate(ctx context.Context, dir, msg string) (string, error) {
	head, err := g.Workspace.CommitCandidate(ctx, dir, msg)
	if err == nil && g.once {
		g.once = false
		return "", errors.New("simulated crash after git commit")
	}
	return head, err
}
func TestRestartRecoversControllerCandidateWithoutDuplicateCommit(t *testing.T) {
	lab := newLocalLab(t)
	store, err := storeadapter.Open(lab.db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	process := &durableFakeProcess{}
	crashGit := &crashAfterCommitGit{once: true}
	run, err := newController(t, store, lab, process, crashGit).Start(context.Background(), startInput(lab))
	if err == nil {
		t.Fatal("expected simulated crash")
	}
	headBefore := gitHead(run.WorktreePath)
	run, err = newController(t, store, lab, process, gitadapter.Workspace{}).Continue(context.Background(), run.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != domain.StateApprovalReady || run.CandidateHead != headBefore {
		t.Fatalf("run=%+v", run)
	}
	if count := strings.TrimSpace(runGitOutput(t, run.WorktreePath, "rev-list", "--count", run.BaseSHA+"..HEAD")); count != "1" {
		t.Fatalf("candidate commit count=%s", count)
	}
}

func TestApprovalInvalidatesOnWorkspaceAndEvidenceMutation(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, application.Run)
	}{
		{"untracked", func(t *testing.T, r application.Run) {
			mustWrite(t, filepath.Join(r.WorktreePath, "extra.txt"), "mutation")
		}},
		{"ignored", func(t *testing.T, r application.Run) {
			mustWrite(t, filepath.Join(r.WorktreePath, "ignored.tmp"), "mutation")
		}},
		{"branch", func(t *testing.T, r application.Run) { runGit(t, r.WorktreePath, "switch", "-c", "other/branch") }},
		{"head", func(t *testing.T, r application.Run) {
			mustWrite(t, filepath.Join(r.WorktreePath, "extra.txt"), "mutation")
			runGit(t, r.WorktreePath, "add", "--all")
			runGit(t, r.WorktreePath, "commit", "-m", "unauthorized")
		}},
		{"verification evidence", func(t *testing.T, r application.Run) {
			store, err := storeadapter.Open(filepath.Join(filepath.Dir(filepath.Dir(r.ArtifactRoot)), "controller.db"))
			if err == nil {
				inspection, _ := store.Inspect(context.Background(), r.ID)
				store.Close()
				for _, v := range inspection.Verifications {
					if v.Phase == "candidate" {
						mustWrite(t, v.EvidencePath, "tampered")
						return
					}
				}
			}
			t.Fatal("missing verification")
		}},
		{"verification stdout", func(t *testing.T, r application.Run) {
			store, err := storeadapter.Open(filepath.Join(filepath.Dir(filepath.Dir(r.ArtifactRoot)), "controller.db"))
			if err != nil {
				t.Fatal(err)
			}
			inspection, err := store.Inspect(context.Background(), r.ID)
			store.Close()
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range inspection.Verifications {
				if v.Phase == "candidate" {
					mustWrite(t, v.StdoutPath, "tampered")
					return
				}
			}
			t.Fatal("missing verification stdout")
		}},
		{"review evidence", func(t *testing.T, r application.Run) {
			paths, err := filepath.Glob(filepath.Join(r.ArtifactRoot, "attempts", "review-*", "review-outcome.json"))
			if err != nil || len(paths) != 1 {
				t.Fatalf("review paths=%v err=%v", paths, err)
			}
			mustWrite(t, paths[0], "tampered")
		}},
		{"review stdout", func(t *testing.T, r application.Run) {
			paths, err := filepath.Glob(filepath.Join(r.ArtifactRoot, "attempts", "review-*", "review.stdout.jsonl"))
			if err != nil || len(paths) != 1 {
				t.Fatalf("review paths=%v err=%v", paths, err)
			}
			mustWrite(t, paths[0], "tampered")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			lab := newLocalLab(t)
			store, err := storeadapter.Open(lab.db)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			process := &durableFakeProcess{}
			controller := newController(t, store, lab, process, gitadapter.Workspace{})
			run, err := controller.Start(context.Background(), startInput(lab))
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(t, run)
			run, err = controller.Continue(context.Background(), run.ID, nil)
			if err != nil {
				t.Fatal(err)
			}
			if run.State != domain.StateFailed {
				t.Fatalf("state=%s", run.State)
			}
		})
	}
}

func argument(args []string, name string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}
func writeLastMessage(args []string, value string) {
	path := argument(args, "--output-last-message")
	mustWriteFile(path, value)
}
func exclusiveWrite(path, value string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = file.WriteString(value)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func mustWriteFile(path, value string) {
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		panic(err)
	}
}
func mustWrite(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}
func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}
func gitHead(path string) string {
	command := exec.Command("git", "rev-parse", "HEAD")
	command.Dir = path
	output, err := command.Output()
	if err != nil {
		panic(err)
	}
	return strings.TrimSpace(string(output))
}
func runGit(t *testing.T, dir string, args ...string) { t.Helper(); _ = runGitOutput(t, dir, args...) }
func runGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return string(output)
}
