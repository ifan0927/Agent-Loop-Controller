package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/codex"
	storeadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

// This fixture ends one worker invocation after scheduling a retry, closes and
// reopens the real SQLite store, then uses a fresh worker and adapter
// composition to recover. The Linear ports and driver are controlled offline
// adapters; no external service is contacted.
func TestOfflineAcceptanceWorkerRestartPreservesRetryAndParksAtDurableAttention(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dbPath := t.TempDir() + "/controller.db"
	store, err := storeadapter.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	repository := offlineAdmissionRepository(t)
	candidate := offlineAdmissionCandidate()
	waits := []time.Duration{}
	wait := func(ctx context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return waitAdmissionWorker(ctx, delay)
	}

	first, err := newAcceptanceRetryWorkerFixture(t, store, repository, candidate, "acceptance-restart-owner-one")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	firstResult, err := runAdmissionWorker(ctx, true, time.Minute, first.dispatcher.Dispatch, wait)
	if err != nil || firstResult.Cycles != 1 || firstResult.LastOutcome != application.LinearTodoDispatchRetryScheduled || firstResult.Stopped != "once" {
		store.Close()
		t.Fatalf("first worker=%+v err=%v", firstResult, err)
	}
	schedules, err := store.ListRetrySchedules(ctx)
	if err != nil || len(schedules) != 1 || schedules[0].Status != application.RetryScheduleScheduled || schedules[0].AttemptCount != 1 || schedules[0].NextEligibleAt.Sub(schedules[0].UpdatedAt) != application.DefaultAutomaticRetryInitialDelay {
		store.Close()
		t.Fatalf("first durable retry schedules=%+v err=%v", schedules, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = storeadapter.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	second, err := newAcceptanceRetryWorkerFixture(t, store, repository, candidate, "acceptance-restart-owner-two")
	if err != nil {
		t.Fatal(err)
	}
	secondCtx, stopSecond := context.WithCancel(ctx)
	secondWait := func(waitCtx context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		if len(waits) == 2 {
			stopSecond()
			return context.Canceled
		}
		return waitAdmissionWorker(waitCtx, delay)
	}
	secondResult, err := runAdmissionWorker(secondCtx, false, time.Minute, second.dispatcher.Dispatch, secondWait)
	stopSecond()
	if err != nil {
		t.Fatal(err)
	}
	if secondResult.Cycles != 2 || secondResult.LastOutcome != application.LinearTodoDispatchAttention || secondResult.Stopped != "canceled" {
		t.Fatalf("second worker=%+v err=%v", secondResult, err)
	}
	if len(waits) != 2 || waits[0] <= 0 || waits[0] > application.DefaultAutomaticRetryInitialDelay || waits[1] != time.Minute {
		t.Fatalf("durable retry waits=%v", waits)
	}
	if first.scanner.calls()+second.scanner.calls() != 1 || len(first.starter.mutations())+len(second.starter.mutations()) != 1 || first.driver.calls()+second.driver.calls() != 1 || first.worktrees.calls()+second.worktrees.calls() != 1 {
		t.Fatalf("second admission side effects scan=%d mutations=%d driver=%d worktrees=%d", first.scanner.calls()+second.scanner.calls(), len(first.starter.mutations())+len(second.starter.mutations()), first.driver.calls()+second.driver.calls(), first.worktrees.calls()+second.worktrees.calls())
	}

	runs, err := store.ListNonterminalRuns(ctx)
	if err != nil || len(runs) != 1 {
		t.Fatalf("nonterminal runs=%+v err=%v", runs, err)
	}
	schedules, err = store.ListRetrySchedules(ctx)
	if err != nil || len(schedules) != 1 {
		t.Fatalf("retry schedules=%+v err=%v", schedules, err)
	}
	schedule := schedules[0]
	if schedule.RunID != runs[0].ID || schedule.AttemptCount != 2 || schedule.Status != application.RetryScheduleAttention || schedule.ReasonCode != application.RetryReasonBudgetExhausted || schedule.FailureClass != application.RetryFailureProcessStart || !schedule.AttentionAt.After(schedule.CreatedAt) || !schedule.NextEligibleAt.IsZero() {
		t.Fatalf("durable attention=%+v", schedule)
	}
	inspection, err := application.NewQueryService(store).Inspect(ctx, application.QueryInput{Requester: application.Requester{ID: "operator", Kind: "github_login"}, RunID: runs[0].ID, Repository: runs[0].Repository})
	if err != nil || len(inspection.OperatorAttentionEvents) != 1 || len(inspection.RetrySchedules) != 1 || inspection.RetrySchedules[0].AttemptCount != 2 || inspection.Run.State != domain.StateExecuting {
		t.Fatalf("restart evidence=%+v err=%v", inspection, err)
	}
	journal, found, err := store.GetLinearTodoAdmissionJournal(ctx, runs[0].ID)
	if err != nil || !found || journal.Status != "started" {
		t.Fatalf("journal=%+v found=%t err=%v", journal, found, err)
	}
}

type acceptanceRetryWorkerFixture struct {
	scanner    *offlineAdmissionScanner
	starter    *offlineAdmissionStarter
	worktrees  *offlineAdmissionWorktrees
	driver     *acceptanceRetryDriver
	dispatcher *application.LinearTodoDispatcher
}

func newAcceptanceRetryWorkerFixture(t *testing.T, store *storeadapter.Store, repository application.LocalRepository, candidate application.LinearTodoCandidate, owner string) (acceptanceRetryWorkerFixture, error) {
	t.Helper()
	reader := newOfflineAdmissionReader(offlineAdmissionSource(candidate))
	scanner := &offlineAdmissionScanner{scan: application.LinearTodoCandidateScan{Candidates: []application.LinearTodoCandidate{candidate}, Digest: offlineAdmissionDigest("recovery-scan"), ObservedAt: candidate.UpdatedAt}}
	starter := &offlineAdmissionStarter{reader: reader}
	worktrees := &offlineAdmissionWorktrees{}
	driver := &acceptanceRetryDriver{}
	controller := application.NewLocalController(store, worktrees, &acceptanceRetryCodex{}, offlineAdmissionVerifier{}, offlineAdmissionGit{}, "fixture-codex", repository.WorktreeRoot)
	dispatcher, err := newAcceptanceRetryDispatcher(scanner, reader, starter, store, controller, driver, repository, owner)
	if err != nil {
		return acceptanceRetryWorkerFixture{}, err
	}
	return acceptanceRetryWorkerFixture{scanner: scanner, starter: starter, worktrees: worktrees, driver: driver, dispatcher: dispatcher}, nil
}

type acceptanceRetryDriver struct {
	mu    sync.Mutex
	count int
}

func (d *acceptanceRetryDriver) Drive(context.Context, application.ProductionDriveCommand) (application.ProductionDriveResult, error) {
	d.mu.Lock()
	d.count++
	d.mu.Unlock()
	return application.ProductionDriveResult{}, acceptanceRetryFailure{}
}

func (d *acceptanceRetryDriver) calls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.count
}

type acceptanceRetryFailure struct{}

func (acceptanceRetryFailure) Error() string { return "offline retry evidence" }

func (acceptanceRetryFailure) AutomaticRetryFailureClass() string {
	return string(application.RetryFailureProcessStart)
}

type acceptanceRetryCodex struct {
	offlineAdmissionCodex
}

func (c *acceptanceRetryCodex) Implementation(ctx context.Context, spec codex.CommandSpec, artifacts string) (codex.StructuredResult[domain.AgentOutcome], error) {
	result, err := c.offlineAdmissionCodex.Implementation(ctx, spec, artifacts)
	if err != nil {
		return result, err
	}
	return result, acceptanceRetryFailure{}
}

func newAcceptanceRetryDispatcher(scanner application.LinearTodoCandidateScanner, reader application.LinearIssueReader, starter application.LinearReservedIssueStarter, store *storeadapter.Store, controller application.LocalRunController, driver application.LinearTodoDispatchDriver, repository application.LocalRepository, owner string) (*application.LinearTodoDispatcher, error) {
	return application.NewLinearTodoDispatcher(scanner, reader, offlineAdmissionResolver{repository: repository}, starter, store, controller, driver, application.LinearTodoDispatchPolicy{
		CandidateAuthority: application.LinearTodoCandidateAuthority{TeamID: "123e4567-e89b-42d3-a456-426614174100", TeamKey: "IFAN", TodoState: offlineAdmissionTodoState, InProgressState: offlineAdmissionInProgressState, MaxCandidates: 10, MaxPages: 1},
		StartAuthority:     application.LinearIssueStartAuthority{TeamID: "123e4567-e89b-42d3-a456-426614174100", TeamKey: "IFAN", TodoState: offlineAdmissionTodoState, InProgressState: offlineAdmissionInProgressState},
		LeaseTTL:           time.Minute,
		OwnerNonce:         owner,
		Requester:          application.Requester{ID: "operator", Kind: "github_login"},
		AttentionProfile:   application.OperatorAttentionProfile{ID: "offline", Name: "offline-retry-fixture"},
		Retry:              application.AutomaticRetryPolicy{MaxAttempts: 1, InitialDelay: time.Second, MaximumDelay: time.Second},
	})
}
