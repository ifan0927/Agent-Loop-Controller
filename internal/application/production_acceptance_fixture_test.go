package application_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/codex"
	gitadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/git"
	processadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/process"
	storeadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/verifier"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestOfflineAcceptanceMissingVerifierCannotAuthorizeCandidateWithDurableEvidence(t *testing.T) {
	lab := newLocalLab(t)
	store, err := storeadapter.Open(lab.db)
	if err != nil {
		t.Fatal(err)
	}

	input := startInput(lab)
	input.Task.VerifierIDs = []string{"missing-verifier"}
	input.Repository.VerifierIDs = []string{"missing-verifier"}
	input.NormalizedJSON, err = json.Marshal(input.Task)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	input.TaskHash = acceptanceDigest(input.NormalizedJSON)

	workspace := gitadapter.Workspace{}
	missingProgram := filepath.Join(t.TempDir(), "missing-verifier")
	registry := verifier.NewRegistry(map[string]verifier.Command{
		"missing-verifier": {Program: missingProgram, Args: []string{"--fixture"}},
	}, processadapter.OSRunner{}, workspace)
	controller := application.NewLocalController(store, testWorktrees{}, codex.NewExecutor(&durableFakeProcess{}, "codex"), registry, workspace, "codex", lab.worktrees)
	run, startErr := controller.Start(context.Background(), input)
	if startErr == nil || run.State != domain.StateVerifying {
		store.Close()
		t.Fatalf("run=%+v err=%v", run, startErr)
	}

	inspection, err := store.Inspect(context.Background(), run.ID)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if len(inspection.Verifications) != 1 || len(inspection.Reviews) != 0 || run.State == domain.StateFreshReview || run.State == domain.StateApprovalReady {
		store.Close()
		t.Fatalf("run=%+v inspection=%+v", run, inspection)
	}
	record := inspection.Verifications[0]
	if record.ProcessOutcome != application.VerificationOutcomeNotStarted || record.FailureCategory != "process_start" || record.ExitCode == 0 {
		store.Close()
		t.Fatalf("verification=%+v", record)
	}
	for _, path := range []string{record.EvidencePath, record.StdoutPath, record.StderrPath} {
		if info, statErr := os.Stat(path); statErr != nil || !info.Mode().IsRegular() {
			store.Close()
			t.Fatalf("evidence path=%q info=%v err=%v", path, info, statErr)
		}
	}
	if strings.Contains(run.LastError, missingProgram) || strings.Contains(run.LastError, "--fixture") {
		store.Close()
		t.Fatalf("raw verifier command leaked into durable error: %q", run.LastError)
	}
	if !hasAcceptanceTransition(inspection.Timeline, domain.StateExecuting, domain.StateVerifying, "") {
		store.Close()
		t.Fatalf("missing verification timeline: %+v", inspection.Timeline)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storeadapter.Open(lab.db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	restarted, err := store.Inspect(context.Background(), run.ID)
	if err != nil || len(restarted.Verifications) != 1 || restarted.Verifications[0].FailureCategory != "process_start" || len(restarted.Reviews) != 0 {
		t.Fatalf("restarted inspection=%+v err=%v", restarted, err)
	}
}

func TestOfflineAcceptanceSparseEnvironmentUsesManagedVerifierAndGitPaths(t *testing.T) {
	lab := newLocalLab(t)
	goPath, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("managed Go verifier is unavailable: %v", err)
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("managed Git executable is unavailable: %v", err)
	}
	pathParts := []string{filepath.Dir(goPath), filepath.Dir(gitPath), "/bin", "/usr/bin"}
	seen := make(map[string]struct{}, len(pathParts))
	paths := make([]string, 0, len(pathParts))
	for _, part := range pathParts {
		if _, found := seen[part]; found {
			continue
		}
		seen[part] = struct{}{}
		paths = append(paths, part)
	}
	t.Setenv("PATH", strings.Join(paths, string(os.PathListSeparator)))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GOTOOLCHAIN", "local")
	t.Setenv("GOFLAGS", "-modcacherw")

	store, err := storeadapter.Open(lab.db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	process := &durableFakeProcess{}
	workspace := gitadapter.Workspace{Binary: gitPath}
	registry := verifier.NewRegistry(map[string]verifier.Command{
		"fixture-go-test": {Program: goPath, Args: []string{"test", "./..."}},
	}, processadapter.OSRunner{}, workspace)
	controller := application.NewLocalController(store, testWorktrees{}, codex.NewExecutor(process, "codex"), registry, workspace, "codex", lab.worktrees)
	run, err := controller.Start(context.Background(), startInput(lab))
	if err != nil || run.State != domain.StateApprovalReady {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	head, err := workspace.Head(context.Background(), run.WorktreePath)
	if err != nil {
		t.Fatal(err)
	}
	if head != run.CandidateHead || strings.TrimSpace(run.CandidateHead) == "" {
		t.Fatalf("candidate head=%q current head=%q", run.CandidateHead, head)
	}
	if count := strings.TrimSpace(runGitOutput(t, run.WorktreePath, "rev-list", "--count", run.BaseSHA+"..HEAD")); count != "1" {
		t.Fatalf("candidate commit count=%s", count)
	}

	inspection, err := store.Inspect(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	passedCandidate := 0
	for _, record := range inspection.Verifications {
		if record.Phase == "candidate" && record.VerifiedHead == run.CandidateHead && record.ProcessOutcome == application.VerificationOutcomeExited && record.ExitCode == 0 {
			passedCandidate++
		}
	}
	if passedCandidate != 1 || len(inspection.Reviews) != 1 || inspection.Reviews[0].ReviewedHead != run.CandidateHead {
		t.Fatalf("candidate evidence=%+v reviews=%+v", inspection.Verifications, inspection.Reviews)
	}
	if !hasAcceptanceTransition(inspection.Timeline, domain.StateVerifying, domain.StateFreshReview, run.CandidateHead) || !hasAcceptanceTransition(inspection.Timeline, domain.StateFreshReview, domain.StateApprovalReady, run.CandidateHead) {
		t.Fatalf("exact-head timeline=%+v", inspection.Timeline)
	}
	if len(inspection.SideEffects) != 0 {
		t.Fatalf("sparse local fixture unexpectedly produced external side effects: %+v", inspection.SideEffects)
	}
	if runs, err := store.ListNonterminalRuns(context.Background()); err != nil || len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatalf("nonterminal runs=%+v err=%v", runs, err)
	}
}

func TestOfflineAcceptanceProductionAbandonReleasesReceivedAdmissionWithoutExternalMutation(t *testing.T) {
	lab := newLocalLab(t)
	store, err := storeadapter.Open(lab.db)
	if err != nil {
		t.Fatal(err)
	}

	repository := lab.repository
	repository.ProfileID = "profile-test"
	repository.ProfileSnapshotVersion = 1
	repository.ProfileDigest = acceptanceDigest([]byte("profile"))
	repository.ProfileSnapshotJSON = `{}`
	repository.RegistryVersion = 1
	repository.RegistryDigest = acceptanceDigest([]byte("registry"))
	repository.RepositoryBindingDigest = acceptanceDigest([]byte("binding"))
	repository.AllowedOperatorLogins = []string{"operator"}
	source := productionLinearSource()
	source.IssueID = "123e4567-e89b-42d3-a456-426614174150"
	source.Identifier = "IFAN-ABANDON"
	reader := &productionLinearReader{source: source}
	controller := &acceptancePersistingController{store: store, persist: false}
	admission, err := application.NewLinearAdmissionService(reader, productionLinearResolver{repository: repository}, store, controller)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	coordinator, err := application.NewProductionCoordinator(admission, controller, store)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	requester := application.Requester{ID: "operator", Kind: "github_login"}
	_, _, err = admission.Start(context.Background(), application.LinearStartCommand{Requester: requester, Identifier: source.Identifier})
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	lease, acquired, err := store.AcquireLinearTodoAdmissionLease(context.Background(), "abandon-fixture", time.Minute, time.Now().UTC())
	if err != nil || !acquired {
		store.Close()
		t.Fatalf("admission lease=%+v acquired=%t err=%v", lease, acquired, err)
	}
	run, _, reserved, err := store.ReserveLinearTodoAdmission(context.Background(), application.LinearTodoAdmissionReservation{Lease: lease, ScanDigest: acceptanceDigest([]byte("abandon-scan")), IssueUUID: source.IssueID, Input: controller.input})
	if err != nil || !reserved || run.State != domain.StateReceived {
		store.Close()
		t.Fatalf("received reservation run=%+v reserved=%t err=%v", run, reserved, err)
	}
	if _, err := store.ReleaseLinearTodoAdmissionLease(context.Background(), lease); err != nil {
		store.Close()
		t.Fatal(err)
	}
	reader.source.State = application.LinearState{ID: "canceled", Name: "Canceled", Type: "canceled"}
	reader.source.SourceRevision = source.SourceRevision + "-canceled"

	cleanup := &acceptanceCleanupPort{}
	result, err := coordinator.Abandon(context.Background(), application.ProductionAbandonCommand{Requester: requester, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, cleanup)
	if err != nil || result.Action != application.ProductionAbandon || result.Run.State != domain.StateFailed || result.Idempotent || len(cleanup.calls) != 0 {
		store.Close()
		t.Fatalf("result=%+v cleanup=%v err=%v", result, cleanup.calls, err)
	}
	if reader.reads != 2 {
		store.Close()
		t.Fatalf("Linear reads=%d want initial admission plus one abandon revalidation", reader.reads)
	}
	inspection, err := store.Inspect(context.Background(), run.ID)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if len(inspection.SideEffects) != 0 || len(inspection.Resources) != 0 || len(inspection.OperatorAttention) != 0 {
		store.Close()
		t.Fatalf("abandon retained unexpected external evidence: %+v", inspection)
	}
	if !hasAcceptanceTransition(inspection.Timeline, domain.StateReceived, domain.StateFailed, "") {
		store.Close()
		t.Fatalf("abandon timeline=%+v", inspection.Timeline)
	}
	journal, found, err := store.GetLinearTodoAdmissionJournal(context.Background(), run.ID)
	if err != nil || !found || journal.Status != application.LinearTodoAdmissionJournalManualIntervention || journal.ReasonCode != application.AutomaticAdmissionAbandonReason {
		store.Close()
		t.Fatalf("abandon journal=%+v found=%t err=%v", journal, found, err)
	}
	if runs, err := store.ListNonterminalRuns(context.Background()); err != nil || len(runs) != 0 {
		store.Close()
		t.Fatalf("abandon left active runs=%+v err=%v", runs, err)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storeadapter.Open(lab.db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	restarted, err := store.Inspect(context.Background(), run.ID)
	if err != nil || restarted.Run.State != domain.StateFailed || len(restarted.Timeline) == 0 {
		t.Fatalf("restarted abandoned run=%+v err=%v", restarted, err)
	}
}

func TestOfflineAcceptanceProductionRepairRebindsFindingsVerificationAndReviewToNewHead(t *testing.T) {
	stack := newAcceptanceProductionStack(t, true)
	requester := stack.requester
	started, _, err := stack.admission.Start(context.Background(), application.LinearStartCommand{Requester: requester, Identifier: stack.source.Identifier})
	if err != nil || started.Run.State != domain.StateFreshReview {
		t.Fatalf("started=%+v err=%v", started, err)
	}
	run, err := stack.store.GetRun(context.Background(), started.Run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	oldHead := run.CandidateHead
	first, err := stack.store.Inspect(context.Background(), run.ID)
	if err != nil || len(first.Reviews) != 1 || first.Reviews[0].ReviewedHead != oldHead {
		t.Fatalf("initial review=%+v err=%v", first.Reviews, err)
	}

	handoff, err := stack.coordinator.Continue(context.Background(), application.ProductionContinueCommand{Requester: requester, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey})
	if err != nil || handoff.Run.State != domain.StateRepairing {
		t.Fatalf("handoff=%+v err=%v", handoff, err)
	}
	beforeRepair, err := stack.store.Inspect(context.Background(), run.ID)
	if err != nil || len(beforeRepair.Findings) != 1 {
		t.Fatalf("persisted findings=%+v err=%v", beforeRepair.Findings, err)
	}
	review := beforeRepair.Reviews[0]
	replay := application.FreshReviewRepairEvidence{RunID: run.ID, AttemptID: review.AttemptID, ReviewedHead: review.ReviewedHead, OutcomePath: review.OutcomePath, OutcomeHash: review.OutcomeHash, Findings: append([]application.FindingRecord(nil), beforeRepair.Findings...)}
	if changed, err := stack.store.PersistFreshReviewFindings(context.Background(), replay); err != nil || changed {
		t.Fatalf("fresh-review replay changed=%t err=%v", changed, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	driver, err := application.NewProductionDriver(stack.coordinator, stack.store, stack.store, stack.store, application.ProductionDriverPorts{}, application.ProductionDriverPolicy{PollInterval: time.Second, MaxImmediateAction: 1}, func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	})
	if err != nil {
		t.Fatal(err)
	}
	_, driveErr := driver.Drive(ctx, application.ProductionDriveCommand{Requester: requester, RunID: run.ID, Repository: run.Repository, IdempotencyKey: run.IdempotencyKey})
	if !errors.Is(driveErr, context.Canceled) {
		t.Fatalf("driver error=%v", driveErr)
	}

	finalRun, err := stack.store.GetRun(context.Background(), run.ID)
	if err != nil || finalRun.State != domain.StateApprovalReady || finalRun.CandidateHead == "" || finalRun.CandidateHead == oldHead {
		t.Fatalf("final run=%+v err=%v", finalRun, err)
	}
	inspection, err := stack.store.Inspect(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(inspection.Findings) != 1 || inspection.Findings[0].HeadSHA != oldHead {
		t.Fatalf("finding authority=%+v", inspection.Findings)
	}
	if len(inspection.Reviews) != 2 || inspection.Reviews[0].ReviewedHead != oldHead || inspection.Reviews[1].ReviewedHead != finalRun.CandidateHead {
		t.Fatalf("review heads=%+v", inspection.Reviews)
	}
	candidateHeads := map[string]int{}
	for _, verification := range inspection.Verifications {
		if verification.Phase != "candidate" {
			continue
		}
		if verification.ProcessOutcome != application.VerificationOutcomeExited || verification.ExitCode != 0 {
			t.Fatalf("candidate verification failed=%+v", verification)
		}
		candidateHeads[verification.VerifiedHead]++
	}
	if len(candidateHeads) != 2 || candidateHeads[oldHead] != 1 || candidateHeads[finalRun.CandidateHead] != 1 {
		t.Fatalf("candidate verification heads=%v records=%+v", candidateHeads, inspection.Verifications)
	}
	if !hasAcceptanceTransition(inspection.Timeline, domain.StateFreshReview, domain.StateRepairing, oldHead) || !hasAcceptanceTransition(inspection.Timeline, domain.StateRepairing, domain.StateExecuting, oldHead) || !hasAcceptanceTransition(inspection.Timeline, domain.StateVerifying, domain.StateFreshReview, finalRun.CandidateHead) || !hasAcceptanceTransition(inspection.Timeline, domain.StateFreshReview, domain.StateApprovalReady, finalRun.CandidateHead) {
		t.Fatalf("repair timeline=%+v", inspection.Timeline)
	}
	if stack.process.resumeCalls != 1 || stack.process.reviewCalls != 2 || stack.reader.reads != 3 {
		t.Fatalf("resume=%d reviews=%d Linear reads=%d", stack.process.resumeCalls, stack.process.reviewCalls, stack.reader.reads)
	}
}

func TestOfflineAcceptanceRepairDeadlineAnchorsAndCancellationDoesNotExpirePolicy(t *testing.T) {
	t.Run("persisted deadline", func(t *testing.T) {
		stack := newAcceptanceProductionStack(t, true)
		run := acceptanceReachRepairing(t, stack)
		run = acceptanceBeginInterruptedRepair(t, stack, run)
		anchor := acceptanceRepairAnchor(t, stack.store, run.ID)
		reads := stack.reader.reads
		if err := stack.store.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := storeadapter.Open(stack.lab.db)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		persisted, err := reopened.Inspect(context.Background(), run.ID)
		if err != nil || acceptanceRepairAnchorFromTimeline(persisted.Timeline).IsZero() || !acceptanceRepairAnchorFromTimeline(persisted.Timeline).Equal(anchor) {
			t.Fatalf("reopened repair anchor=%v want=%v err=%v", acceptanceRepairAnchorFromTimeline(persisted.Timeline), anchor, err)
		}
		controller := newControllerWithRepairClock(t, reopened, stack.lab, stack.process, gitadapter.Workspace{}, func() time.Time { return anchor.Add(31 * time.Minute) })
		admission, err := application.NewLinearAdmissionService(stack.reader, productionLinearResolver{repository: stack.repository}, reopened, controller)
		if err != nil {
			t.Fatal(err)
		}
		coordinator, err := application.NewProductionCoordinator(admission, controller, reopened)
		if err != nil {
			t.Fatal(err)
		}
		result, err := coordinator.Continue(context.Background(), application.ProductionContinueCommand{Requester: stack.requester, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey})
		if err == nil {
			t.Fatalf("deadline unexpectedly succeeded: result=%+v", result)
		}
		persistedRun, getErr := reopened.GetRun(context.Background(), run.ID)
		if getErr != nil || persistedRun.State != domain.StateManualIntervention || stack.reader.reads != reads || stack.process.resumeCalls != 1 {
			t.Fatalf("persisted=%+v reads=%d resumes=%d err=%v", persistedRun, stack.reader.reads, stack.process.resumeCalls, getErr)
		}
		inspection, inspectErr := reopened.Inspect(context.Background(), run.ID)
		if inspectErr != nil || !hasAcceptanceTransition(inspection.Timeline, domain.StateExecuting, domain.StateManualIntervention, "") {
			t.Fatalf("deadline timeline=%+v err=%v", inspection.Timeline, inspectErr)
		}
	})

	t.Run("caller cancellation", func(t *testing.T) {
		stack := newAcceptanceProductionStack(t, true)
		run := acceptanceReachRepairing(t, stack)
		run = acceptanceBeginInterruptedRepair(t, stack, run)
		anchor := acceptanceRepairAnchor(t, stack.store, run.ID)
		reads := stack.reader.reads
		if err := stack.store.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := storeadapter.Open(stack.lab.db)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		persisted, err := reopened.Inspect(context.Background(), run.ID)
		if err != nil || acceptanceRepairAnchorFromTimeline(persisted.Timeline).IsZero() || !acceptanceRepairAnchorFromTimeline(persisted.Timeline).Equal(anchor) {
			t.Fatalf("reopened repair anchor=%v want=%v err=%v", acceptanceRepairAnchorFromTimeline(persisted.Timeline), anchor, err)
		}
		controller := newControllerWithRepairClock(t, reopened, stack.lab, stack.process, gitadapter.Workspace{}, func() time.Time { return anchor.Add(time.Minute) })
		admission, err := application.NewLinearAdmissionService(stack.reader, productionLinearResolver{repository: stack.repository}, reopened, controller)
		if err != nil {
			t.Fatal(err)
		}
		coordinator, err := application.NewProductionCoordinator(admission, controller, reopened)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result, err := coordinator.Continue(ctx, application.ProductionContinueCommand{Requester: stack.requester, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled repair returned err=%v result=%+v", err, result)
		}
		persistedRun, getErr := reopened.GetRun(context.Background(), run.ID)
		if getErr != nil || persistedRun.State != domain.StateExecuting || stack.reader.reads != reads || stack.process.resumeCalls != 1 {
			t.Fatalf("canceled policy changed persisted=%+v reads=%d resumes=%d err=%v", persistedRun, stack.reader.reads, stack.process.resumeCalls, getErr)
		}
	})
}

type acceptanceProductionStack struct {
	lab         localLab
	store       *storeadapter.Store
	process     *durableFakeProcess
	local       *application.LocalController
	repository  application.LocalRepository
	reader      *productionLinearReader
	source      application.LinearTaskSource
	admission   *application.LinearAdmissionService
	coordinator *application.ProductionCoordinator
	requester   application.Requester
}

func newAcceptanceProductionStack(t *testing.T, reviewFindings bool) acceptanceProductionStack {
	t.Helper()
	lab := newLocalLab(t)
	store, err := storeadapter.Open(lab.db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	repository := lab.repository
	repository.AllowedOperatorLogins = []string{"operator"}
	process := &durableFakeProcess{reviewFindings: reviewFindings}
	local := newController(t, store, lab, process, gitadapter.Workspace{})
	source := productionLinearSource()
	reader := &productionLinearReader{source: source}
	admission, err := application.NewLinearAdmissionService(reader, productionLinearResolver{repository: repository}, store, local)
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := application.NewProductionCoordinator(admission, local, store)
	if err != nil {
		t.Fatal(err)
	}
	return acceptanceProductionStack{lab: lab, store: store, process: process, local: local, repository: repository, reader: reader, source: source, admission: admission, coordinator: coordinator, requester: application.Requester{ID: "operator", Kind: "github_login"}}
}

func newControllerWithRepairClock(t *testing.T, store application.RunStore, lab localLab, process *durableFakeProcess, git application.DurableGit, clock func() time.Time) *application.LocalController {
	t.Helper()
	workspace := gitadapter.Workspace{}
	registry := verifier.NewRegistry(map[string]verifier.Command{"fixture-go-test": {Program: "go", Args: []string{"test", "./..."}}}, processadapter.OSRunner{}, workspace)
	return application.NewLocalControllerWithClock(store, testWorktrees{}, codex.NewExecutor(process, "codex"), registry, git, "codex", lab.worktrees, clock)
}

func acceptanceReachRepairing(t *testing.T, stack acceptanceProductionStack) application.Run {
	t.Helper()
	started, _, err := stack.admission.Start(context.Background(), application.LinearStartCommand{Requester: stack.requester, Identifier: stack.source.Identifier})
	if err != nil || started.Run.State != domain.StateFreshReview {
		t.Fatalf("started=%+v err=%v", started, err)
	}
	run, err := stack.store.GetRun(context.Background(), started.Run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := stack.coordinator.Continue(context.Background(), application.ProductionContinueCommand{Requester: stack.requester, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey})
	if err != nil || result.Run.State != domain.StateRepairing {
		t.Fatalf("repair handoff=%+v err=%v", result, err)
	}
	repaired, err := stack.store.GetRun(context.Background(), run.ID)
	if err != nil || repaired.State != domain.StateRepairing {
		t.Fatalf("repairing=%+v err=%v", repaired, err)
	}
	return repaired
}

func acceptanceBeginInterruptedRepair(t *testing.T, stack acceptanceProductionStack, run application.Run) application.Run {
	t.Helper()
	inspection, err := stack.store.Inspect(context.Background(), run.ID)
	if err != nil || len(inspection.Findings) != 1 {
		t.Fatalf("repair findings=%+v err=%v", inspection.Findings, err)
	}
	stack.process.resumeStarted = make(chan struct{}, 1)
	stack.process.blockResumeUntilContextDone = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type repairResult struct {
		run application.Run
		err error
	}
	result := make(chan repairResult, 1)
	go func() {
		repaired, repairErr := stack.local.RepairFindings(ctx, run.ID, inspection.Findings)
		result <- repairResult{run: repaired, err: repairErr}
	}()
	select {
	case <-stack.process.resumeStarted:
		cancel()
	case <-time.After(5 * time.Second):
		t.Fatal("repair did not reach the persisted implementation session")
	}
	select {
	case outcome := <-result:
		if outcome.err == nil || outcome.run.State != domain.StateExecuting {
			t.Fatalf("interrupted repair=%+v err=%v", outcome.run, outcome.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interrupted repair did not stop after caller cancellation")
	}
	updated, err := stack.store.GetRun(context.Background(), run.ID)
	if err != nil || updated.State != domain.StateExecuting || updated.CandidateHead != "" {
		t.Fatalf("persisted interrupted repair=%+v err=%v", updated, err)
	}
	return updated
}

func acceptanceRepairAnchor(t *testing.T, store *storeadapter.Store, runID string) time.Time {
	t.Helper()
	inspection, err := store.Inspect(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	anchor := acceptanceRepairAnchorFromTimeline(inspection.Timeline)
	if anchor.IsZero() {
		t.Fatalf("repair timeline has no persisted deadline anchor: %+v", inspection.Timeline)
	}
	return anchor
}

func acceptanceRepairAnchorFromTimeline(timeline []application.Transition) time.Time {
	for _, transition := range timeline {
		if transition.From == domain.StateRepairing && transition.To == domain.StateExecuting {
			return transition.CreatedAt
		}
	}
	return time.Time{}
}

func hasAcceptanceTransition(timeline []application.Transition, from, to domain.State, head string) bool {
	for _, transition := range timeline {
		if transition.From == from && transition.To == to && (head == "" || transition.BoundHead == head) {
			return true
		}
	}
	return false
}

func acceptanceDigest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

type acceptancePersistingController struct {
	store   application.RunStore
	persist bool
	input   application.LocalStartInput
}

func (c *acceptancePersistingController) StartAuthorized(ctx context.Context, input application.LocalStartInput, _ func(application.Run) error) (application.Run, error) {
	c.input = input
	run, err := application.ReservedRunFromAdmissionSnapshot(input)
	if err != nil {
		return application.Run{}, err
	}
	run.State = domain.StateReceived
	if !c.persist {
		return run, nil
	}
	created, _, err := c.store.CreateRun(ctx, application.CreateRunInput{Run: run})
	return created, err
}

func (c *acceptancePersistingController) ContinueExpected(ctx context.Context, runID string, _ domain.State, _ string, _ *application.Decision) (application.Run, error) {
	return c.store.GetRun(ctx, runID)
}

func (c *acceptancePersistingController) EnforceRepairDeadline(ctx context.Context, runID string) (application.Run, error) {
	return c.store.GetRun(ctx, runID)
}

func (c *acceptancePersistingController) BoundRepairActionContext(ctx context.Context, _ string) (context.Context, context.CancelFunc, error) {
	return ctx, func() {}, nil
}

type acceptanceCleanupPort struct {
	calls []string
}

func (c *acceptanceCleanupPort) RemoveWorktree(context.Context, string, string, string, string) error {
	c.calls = append(c.calls, "worktree")
	return nil
}

func (c *acceptanceCleanupPort) DeleteLocalBranch(context.Context, string, string, string) error {
	c.calls = append(c.calls, "branch")
	return nil
}

func (c *acceptanceCleanupPort) DeleteRemoteBranch(context.Context, string, string, string) error {
	c.calls = append(c.calls, "remote")
	return nil
}
