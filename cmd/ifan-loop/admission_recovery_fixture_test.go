package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/bootstrap"
	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/codex"
	storeadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
	"github.com/ifan0927/Agent-Loop-Controller/internal/fixtureevidence"
)

// This fixture ends one worker invocation after scheduling a retry, closes and
// reopens the real SQLite store, then uses a fresh worker and adapter
// composition to recover. The Linear ports and driver are controlled offline
// adapters; no external service is contacted.
func TestOfflineAcceptanceWorkerRestartPreservesRetryAndParksAtDurableAttention(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	root := resolvedTempDir(t)
	configPath, dbPath := writeControllerStatusConfig(t, root)
	candidate := offlineAdmissionCandidate()
	candidate.Labels[1].Name = "repo:owner/repo"
	candidate.RepositoryLabels[0].Name = "repo:owner/repo"
	candidate.SourceDigest = offlineAdmissionDigest("config-backed-source")
	linearServer := retryFixtureLinearServer(t, candidate)
	defer linearServer.Close()
	rewriteRetryFixtureLinearURL(t, configPath, linearServer.URL+"/graphql")
	t.Setenv("IFAN_LOOP_LINEAR_TOKEN", "fixture-credential")
	loaded, err := bootstrap.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := loaded.Registry.Resolve("owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	repository := localRepository(binding)
	store, err := storeadapter.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}

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
		attention, _ := store.ListOperatorAttention(ctx, application.OperatorAttentionQueryInput{Limit: 10})
		store.Close()
		t.Fatalf("first worker=%+v attention=%+v err=%v", firstResult, attention, err)
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
	inspection, err := application.NewQueryService(store).Inspect(ctx, application.QueryInput{Requester: application.Requester{ID: "ifan0927", Kind: "github_login", DatabaseID: 33, NodeID: "MDQ6VXNlcjMz", ActorType: "User"}, RunID: runs[0].ID, Repository: runs[0].Repository})
	if err != nil || len(inspection.OperatorAttentionEvents) != 1 || len(inspection.RetrySchedules) != 1 || inspection.RetrySchedules[0].AttemptCount != 2 || inspection.Run.State != domain.StateExecuting {
		t.Fatalf("restart evidence=%+v err=%v", inspection, err)
	}
	journal, found, err := store.GetLinearTodoAdmissionJournal(ctx, runs[0].ID)
	if err != nil || !found || journal.Status != "started" {
		t.Fatalf("journal=%+v found=%t err=%v", journal, found, err)
	}
	driverCallsBeforeCLI := second.driver.calls()
	retryArgs := []string{runs[0].ID, "--config", configPath, "--requester", "ifan0927", "--requester-database-id", "33", "--requester-node-id", "MDQ6VXNlcjMz", "--requester-type", "User"}
	retryOutput, err := captureConfigOutput(func() error { return controllerRetry(retryArgs) })
	if err != nil || !strings.Contains(retryOutput, `"status": "observed"`) || strings.Contains(retryOutput, "fixture-credential") || second.driver.calls() != driverCallsBeforeCLI {
		t.Fatalf("controller retry output=%s driver=%d err=%v", retryOutput, second.driver.calls(), err)
	}
	replayOutput, err := captureConfigOutput(func() error { return controllerRetry(retryArgs) })
	if err != nil || replayOutput != retryOutput || second.driver.calls() != driverCallsBeforeCLI {
		t.Fatalf("controller retry replay output=%s first=%s driver=%d err=%v", replayOutput, retryOutput, second.driver.calls(), err)
	}
	var retried application.OperatorRetryResult
	if err := json.Unmarshal([]byte(retryOutput), &retried); err != nil || retried.Action.Status != application.OperatorActionStatusObserved || retried.Retry == nil || retried.Retry.Status != application.RetryScheduleScheduled {
		t.Fatalf("decoded controller retry=%+v err=%v", retried, err)
	}
	second.driver.succeed = true
	resumed, err := runAdmissionWorker(ctx, true, time.Minute, second.dispatcher.Dispatch, wait)
	if err != nil || resumed.LastOutcome != application.LinearTodoDispatchDriven || resumed.Cycles != 1 || second.driver.calls() != 2 {
		t.Fatalf("automatic resume=%+v driver=%d err=%v", resumed, second.driver.calls(), err)
	}
	if schedules, err := store.ListRetrySchedules(ctx); err != nil || len(schedules) != 0 {
		t.Fatalf("consumed retry schedules=%+v err=%v", schedules, err)
	}
	fixtureevidence.Emit(t, fixtureevidence.Evidence{Scenario: "park_notify_retry_resume", RunIDs: []string{runs[0].ID}, IssueIdentifiers: []string{runs[0].IssueID}, EventActionKeys: []string{inspection.OperatorAttentionEvents[0].EventKey, retried.Action.ActionID}, StateSequence: []string{"executing", "retry_attention", "operator_retry", "automatic_resume"}, RetryAbandonOutcomes: []string{"retry_observed", "retry_replay_idempotent", "worker_resumed"}, LeaseEvidence: []string{"released_on_restart", "reacquired_after_restart"}, ExactCandidateBindings: []string{"same_persisted_run"}, FinalWorkerState: "stopped"})
}

func retryFixtureLinearServer(t *testing.T, candidate application.LinearTodoCandidate) *httptest.Server {
	t.Helper()
	source := offlineAdmissionSource(candidate)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-credential" {
			t.Errorf("unexpected Linear authorization")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		labels := make([]map[string]string, 0, len(source.Labels))
		for _, label := range source.Labels {
			labels = append(labels, map[string]string{"id": label.ID, "name": label.Name})
		}
		response := map[string]any{"data": map[string]any{"issue": map[string]any{
			"id": source.IssueID, "identifier": source.Identifier, "url": source.URL, "title": source.Title,
			"description": source.Description, "createdAt": source.CreatedAt.Format(time.RFC3339Nano), "updatedAt": source.UpdatedAt.Format(time.RFC3339Nano), "branchName": source.BranchName,
			"team":   map[string]any{"id": source.Team.ID, "key": source.Team.Key, "name": source.Team.Name},
			"state":  map[string]any{"id": source.State.ID, "name": source.State.Name, "type": source.State.Type},
			"cycle":  map[string]any{"id": source.Cycle.ID, "number": source.Cycle.Number, "startsAt": source.Cycle.StartsAt.Format(time.RFC3339Nano), "endsAt": source.Cycle.EndsAt.Format(time.RFC3339Nano), "isActive": source.Cycle.IsActive},
			"labels": map[string]any{"nodes": labels, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""}},
		}}}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Error(err)
		}
	}))
}

func rewriteRetryFixtureLinearURL(t *testing.T, configPath, apiURL string) {
	t.Helper()
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	config["linear"].(map[string]any)["api_url"] = apiURL
	rewritten, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, rewritten, 0o600); err != nil {
		t.Fatal(err)
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
	driver := &acceptanceRetryDriver{store: store}
	controller := application.NewLocalController(store, worktrees, &acceptanceRetryCodex{}, offlineAdmissionVerifier{}, offlineAdmissionGit{}, "fixture-codex", repository.WorktreeRoot)
	dispatcher, err := newAcceptanceRetryDispatcher(scanner, reader, starter, store, controller, driver, repository, owner)
	if err != nil {
		return acceptanceRetryWorkerFixture{}, err
	}
	return acceptanceRetryWorkerFixture{scanner: scanner, starter: starter, worktrees: worktrees, driver: driver, dispatcher: dispatcher}, nil
}

type acceptanceRetryDriver struct {
	mu      sync.Mutex
	count   int
	store   *storeadapter.Store
	succeed bool
}

func (d *acceptanceRetryDriver) Drive(ctx context.Context, command application.ProductionDriveCommand) (application.ProductionDriveResult, error) {
	d.mu.Lock()
	d.count++
	succeed := d.succeed
	d.mu.Unlock()
	if succeed {
		return application.ProductionDriveResult{}, nil
	}
	if d.store != nil {
		attempt, err := d.store.BeginAttempt(ctx, command.RunID, "implementation", "fixture", "fixture")
		if err == nil {
			attempt.Status, attempt.ErrorCategory, attempt.ExitCode, attempt.FinishedAt = "failed", application.RetryReasonProcessStart, -1, time.Now().UTC()
			_ = d.store.FinishAttempt(ctx, attempt)
		}
	}
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
	requester := application.Requester{ID: "operator", Kind: "github_login"}
	if len(repository.AllowedOperatorLogins) > 0 {
		requester.ID = repository.AllowedOperatorLogins[0]
	}
	if len(repository.TrustedOperatorActors) > 0 {
		actor := repository.TrustedOperatorActors[0]
		requester.DatabaseID, requester.NodeID, requester.ActorType = actor.DatabaseID, actor.NodeID, actor.Type
	}
	return application.NewLinearTodoDispatcher(scanner, reader, offlineAdmissionResolver{repository: repository}, starter, store, controller, driver, application.LinearTodoDispatchPolicy{
		CandidateAuthority: application.LinearTodoCandidateAuthority{TeamID: "123e4567-e89b-42d3-a456-426614174100", TeamKey: "IFAN", TodoState: offlineAdmissionTodoState, InProgressState: offlineAdmissionInProgressState, MaxCandidates: 10, MaxPages: 1},
		StartAuthority:     application.LinearIssueStartAuthority{TeamID: "123e4567-e89b-42d3-a456-426614174100", TeamKey: "IFAN", TodoState: offlineAdmissionTodoState, InProgressState: offlineAdmissionInProgressState},
		LeaseTTL:           time.Minute,
		OwnerNonce:         owner,
		Requester:          requester,
		AttentionProfile:   application.OperatorAttentionProfile{ID: "offline", Name: "offline-retry-fixture"},
		Retry:              application.AutomaticRetryPolicy{MaxAttempts: 1, InitialDelay: time.Second, MaximumDelay: time.Second},
	})
}
