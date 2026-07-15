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

// This fixture restarts the dispatcher between every worker cycle while the
// real SQLite store remains the only retry authority. The Linear ports and
// driver are controlled offline adapters; no external service is contacted.
func TestOfflineAcceptanceWorkerRestartPreservesRetryAndStopsAtDurableAttention(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	store, err := openOfflineAdmissionStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	repository := offlineAdmissionRepository(t)
	candidate := offlineAdmissionCandidate()
	reader := newOfflineAdmissionReader(offlineAdmissionSource(candidate))
	scanner := &offlineAdmissionScanner{scan: application.LinearTodoCandidateScan{Candidates: []application.LinearTodoCandidate{candidate}, Digest: offlineAdmissionDigest("recovery-scan"), ObservedAt: candidate.UpdatedAt}}
	starter := &offlineAdmissionStarter{reader: reader}
	worktrees := &offlineAdmissionWorktrees{}
	driver := &acceptanceRetryDriver{}

	owners := []string{}
	waits := []time.Duration{}
	dispatches := 0
	dispatch := func(ctx context.Context) (application.LinearTodoDispatchResult, error) {
		dispatches++
		owner := "acceptance-restart-owner-" + string(rune('0'+dispatches))
		owners = append(owners, owner)
		controller := application.NewLocalController(store, worktrees, &acceptanceRetryCodex{}, offlineAdmissionVerifier{}, offlineAdmissionGit{}, "fixture-codex", repository.WorktreeRoot)
		dispatcher, err := newAcceptanceRetryDispatcher(scanner, reader, starter, store, controller, driver, repository, owner)
		if err != nil {
			return application.LinearTodoDispatchResult{}, err
		}
		return dispatcher.Dispatch(ctx)
	}
	wait := func(ctx context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return waitAdmissionWorker(ctx, delay)
	}
	worker, err := runAdmissionWorker(ctx, false, time.Minute, dispatch, wait)
	if err != nil {
		t.Fatal(err)
	}
	if worker.Cycles != 2 || worker.LastOutcome != application.LinearTodoDispatchAttention || worker.Stopped != "attention_required" || dispatches != 2 || len(owners) != 2 || owners[0] == owners[1] {
		t.Fatalf("worker=%+v dispatches=%d owners=%v", worker, dispatches, owners)
	}
	if len(waits) != 1 || waits[0] < 900*time.Millisecond || waits[0] > application.DefaultAutomaticRetryInitialDelay {
		t.Fatalf("durable retry waits=%v", waits)
	}
	if scanner.calls() != 1 || len(starter.mutations()) != 1 || driver.calls() != 1 || worktrees.calls() != 1 {
		t.Fatalf("second admission side effects scan=%d mutations=%d driver=%d worktrees=%d", scanner.calls(), len(starter.mutations()), driver.calls(), worktrees.calls())
	}

	runs, err := store.ListNonterminalRuns(ctx)
	if err != nil || len(runs) != 1 {
		t.Fatalf("nonterminal runs=%+v err=%v", runs, err)
	}
	schedules, err := store.ListRetrySchedules(ctx)
	if err != nil || len(schedules) != 1 {
		t.Fatalf("retry schedules=%+v err=%v", schedules, err)
	}
	schedule := schedules[0]
	if schedule.RunID != runs[0].ID || schedule.AttemptCount != 2 || schedule.Status != application.RetryScheduleAttention || schedule.ReasonCode != application.RetryReasonBudgetExhausted || schedule.FailureClass != application.RetryFailureProcessStart || !schedule.AttentionAt.After(schedule.CreatedAt) || !schedule.NextEligibleAt.IsZero() {
		t.Fatalf("durable attention=%+v", schedule)
	}
	inspection, err := store.Inspect(ctx, runs[0].ID)
	if err != nil || len(inspection.OperatorAttention) != 1 || len(inspection.RetrySchedules) != 1 || inspection.RetrySchedules[0].AttemptCount != 2 || inspection.Run.State != domain.StateExecuting {
		t.Fatalf("restart evidence=%+v err=%v", inspection, err)
	}
	journal, found, err := store.GetLinearTodoAdmissionJournal(ctx, runs[0].ID)
	if err != nil || !found || journal.Status != "started" {
		t.Fatalf("journal=%+v found=%t err=%v", journal, found, err)
	}
}

func openOfflineAdmissionStore(t *testing.T) (*storeadapter.Store, error) {
	t.Helper()
	return storeadapter.Open(t.TempDir() + "/controller.db")
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
