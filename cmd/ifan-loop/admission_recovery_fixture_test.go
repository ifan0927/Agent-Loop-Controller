package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/bootstrap"
	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/codex"
	processadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/process"
	storeadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/verifier"
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
	unauthenticatedArgs := []string{runs[0].ID, "--config", configPath, "--requester", "ifan0927", "--requester-database-id", "33", "--requester-type", "User"}
	if output, unauthenticatedErr := captureConfigOutput(func() error { return controllerRetry(unauthenticatedArgs) }); unauthenticatedErr == nil || output != "" {
		t.Fatalf("unauthenticated retry output=%q err=%v", output, unauthenticatedErr)
	}
	unchanged, err := store.Inspect(ctx, runs[0].ID)
	if err != nil || unchanged.Run.State != domain.StateExecuting || len(unchanged.OperatorActions) != 0 || len(unchanged.RetrySchedules) != 1 || unchanged.RetrySchedules[0].Status != application.RetryScheduleAttention || second.driver.calls() != driverCallsBeforeCLI {
		t.Fatalf("unauthenticated metadata mutated state=%+v driver=%d err=%v", unchanged, second.driver.calls(), err)
	}
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
		debug, debugErr := store.Inspect(ctx, runs[0].ID)
		t.Fatalf("automatic resume=%+v driver=%d driverErr=%v state=%s attempts=%+v reviews=%+v debugErr=%v err=%v", resumed, second.driver.calls(), second.driver.err(), debug.Run.State, debug.Attempts, debug.Reviews, debugErr, err)
	}
	if schedules, err := store.ListRetrySchedules(ctx); err != nil || len(schedules) != 0 {
		t.Fatalf("consumed retry schedules=%+v err=%v", schedules, err)
	}
	completed, err := store.Inspect(ctx, runs[0].ID)
	if err != nil || completed.Run.State != domain.StateApprovalReady || len(completed.Verifications) < 2 || len(completed.Reviews) != 1 || completed.Run.CandidateHead == "" || completed.Reviews[0].ReviewedHead != completed.Run.CandidateHead {
		t.Fatalf("exact-head resume evidence=%+v err=%v", completed, err)
	}
	candidateVerifications := 0
	for _, verification := range completed.Verifications {
		if verification.Phase != "candidate" {
			continue
		}
		candidateVerifications++
		if verification.VerifiedHead != completed.Run.CandidateHead || verification.ProcessOutcome != application.VerificationOutcomeExited || verification.ExitCode != 0 {
			t.Fatalf("verification is not bound to exact candidate: %+v candidate=%s", verification, completed.Run.CandidateHead)
		}
	}
	if candidateVerifications != 1 {
		t.Fatalf("candidate verification count=%d records=%+v", candidateVerifications, completed.Verifications)
	}
	projection, err := application.NewQueryService(store).Inspect(ctx, application.QueryInput{Requester: application.Requester{ID: "ifan0927", Kind: "github_login", DatabaseID: 33, NodeID: "MDQ6VXNlcjMz", ActorType: "User"}, RunID: runs[0].ID, Repository: runs[0].Repository})
	if err != nil {
		t.Fatal(err)
	}
	projectionJSON, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	assertAcceptanceRuntimeProjectionSafe(t, string(projectionJSON), []string{retryOutput, replayOutput}, []string{root, repository.OriginPath, repository.SourcePath, repository.RunRoot, repository.WorktreeRoot})
	t.Logf("IFAN_FIXTURE_RUNTIME_DB %s", projectionJSON)
	t.Logf("IFAN_FIXTURE_RUNTIME_CLI %s", strings.TrimSpace(retryOutput))
	fixtureevidence.Emit(t, fixtureevidence.Evidence{Scenario: "park_notify_retry_resume", RunIDs: []string{runs[0].ID}, IssueIdentifiers: []string{runs[0].IssueID}, EventActionKeys: []string{inspection.OperatorAttentionEvents[0].EventKey, retried.Action.ActionID}, StateSequence: []string{"executing", "retry_attention", "operator_retry", "automatic_resume", "verifying", "fresh_review", "approval_ready"}, RetryAbandonOutcomes: []string{"retry_observed", "replay_idempotent", "worker_resumed"}, LeaseEvidence: []string{"restart_released", "restart_reacquired"}, ExactCandidateBindings: []string{"same_run", "auth_required", "exact_head"}, FinalWorkerState: "stopped"})
}

func assertAcceptanceRuntimeProjectionSafe(t *testing.T, projection string, cliOutputs, localPaths []string) {
	t.Helper()
	for name, value := range map[string]string{"database projection": projection, "CLI output": strings.Join(cliOutputs, "\n")} {
		lower := strings.ToLower(value)
		for _, forbidden := range []string{"fixture-credential", `"authorization":`, "authorization:", "bearer ", "-----begin", "deliver one task", "acceptance criteria", "controller drive", "private key"} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("%s contains forbidden runtime material %q", name, forbidden)
			}
		}
		for _, path := range localPaths {
			if path != "" && strings.Contains(value, path) {
				t.Fatalf("%s contains a local path", name)
			}
		}
	}
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
	codexFixture := &acceptanceRetryCodex{failNext: true}
	gitFixture := &acceptanceRetryGit{branch: candidate.BranchName, head: "offline-base", dirty: true}
	verification := acceptanceRetryVerifier{git: gitFixture}
	controller := application.NewLocalController(store, worktrees, codexFixture, verification, gitFixture, "fixture-codex", repository.WorktreeRoot)
	driver := &acceptanceRetryDriver{store: store, controller: controller, codex: codexFixture}
	dispatcher, err := newAcceptanceRetryDispatcher(scanner, reader, starter, store, controller, driver, repository, owner)
	if err != nil {
		return acceptanceRetryWorkerFixture{}, err
	}
	return acceptanceRetryWorkerFixture{scanner: scanner, starter: starter, worktrees: worktrees, driver: driver, dispatcher: dispatcher}, nil
}

type acceptanceRetryDriver struct {
	mu         sync.Mutex
	count      int
	store      *storeadapter.Store
	controller application.LocalRunController
	codex      *acceptanceRetryCodex
	succeed    bool
	lastErr    error
}

func (d *acceptanceRetryDriver) Drive(ctx context.Context, command application.ProductionDriveCommand) (application.ProductionDriveResult, error) {
	d.mu.Lock()
	d.count++
	succeed := d.succeed
	d.mu.Unlock()
	if succeed {
		d.codex.allowSuccess()
		run, err := d.store.GetRun(ctx, command.RunID)
		if err != nil {
			return application.ProductionDriveResult{}, err
		}
		resumed, err := d.controller.ContinueExpected(ctx, run.ID, run.State, run.IdempotencyKey, nil)
		if err != nil {
			d.mu.Lock()
			d.lastErr = err
			d.mu.Unlock()
			return application.ProductionDriveResult{}, err
		}
		return application.ProductionDriveResult{Run: application.RunResult{RunID: resumed.ID, State: resumed.State}, Action: application.ProductionStop, Reason: "exact_head_review_complete"}, nil
	}
	if d.store != nil {
		run, runErr := d.store.GetRun(ctx, command.RunID)
		if runErr != nil {
			return application.ProductionDriveResult{}, runErr
		}
		attempt, err := d.store.BeginAttempt(ctx, command.RunID, "implementation", run.ImplementationModel, filepath.Join(run.ArtifactRoot, "fixture-driver-failure"))
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

func (d *acceptanceRetryDriver) err() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastErr
}

type acceptanceRetryFailure struct{}

func (acceptanceRetryFailure) Error() string { return "offline retry evidence" }

func (acceptanceRetryFailure) AutomaticRetryFailureClass() string {
	return string(application.RetryFailureProcessStart)
}

type acceptanceRetryCodex struct {
	mu       sync.Mutex
	failNext bool
}

func (*acceptanceRetryCodex) Preflight(context.Context, string, string) (codex.PreflightEvidence, error) {
	return codex.PreflightEvidence{Version: "fixture-codex"}, nil
}

func (c *acceptanceRetryCodex) Implementation(_ context.Context, _ codex.CommandSpec, artifacts string) (codex.StructuredResult[domain.AgentOutcome], error) {
	c.mu.Lock()
	fail := c.failNext
	c.failNext = false
	c.mu.Unlock()
	result, err := acceptanceRetryAgentResult(artifacts)
	if err != nil {
		return result, err
	}
	if fail {
		return result, acceptanceRetryFailure{}
	}
	return result, nil
}

func (c *acceptanceRetryCodex) allowSuccess() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failNext = false
}

func (c *acceptanceRetryCodex) Resume(ctx context.Context, spec codex.CommandSpec, artifacts string) (codex.StructuredResult[domain.AgentOutcome], error) {
	return c.Implementation(ctx, spec, artifacts)
}

func (*acceptanceRetryCodex) Review(_ context.Context, _ codex.CommandSpec, artifacts string) (codex.StructuredResult[domain.ReviewOutcome], error) {
	outcome := domain.ReviewOutcome{Verdict: domain.ReviewPass, Summary: "fixture review passed", ReviewedHeadSHA: "candidate-head"}
	if err := outcome.Validate(); err != nil {
		return codex.StructuredResult[domain.ReviewOutcome]{}, err
	}
	stdout, stderr := filepath.Join(artifacts, "review.stdout.jsonl"), filepath.Join(artifacts, "review.stderr.txt")
	if err := writeAcceptanceRetryCapture(filepath.Join(artifacts, "review-outcome.json"), outcome, stdout, stderr); err != nil {
		return codex.StructuredResult[domain.ReviewOutcome]{}, err
	}
	return codex.StructuredResult[domain.ReviewOutcome]{SessionID: "fixture-review-session", Outcome: outcome, Process: processadapter.Result{Outcome: processadapter.OutcomeExited, ExitCode: 0, StdoutPath: stdout, StderrPath: stderr}}, nil
}

func acceptanceRetryAgentResult(artifacts string) (codex.StructuredResult[domain.AgentOutcome], error) {
	outcome := domain.AgentOutcome{Status: domain.AgentCompleted, Summary: "fixture implementation completed"}
	if err := outcome.Validate(); err != nil {
		return codex.StructuredResult[domain.AgentOutcome]{}, err
	}
	stdout, stderr := filepath.Join(artifacts, "implementation.stdout.jsonl"), filepath.Join(artifacts, "implementation.stderr.txt")
	if err := writeAcceptanceRetryCapture(filepath.Join(artifacts, "implementation-outcome.json"), outcome, stdout, stderr); err != nil {
		return codex.StructuredResult[domain.AgentOutcome]{}, err
	}
	return codex.StructuredResult[domain.AgentOutcome]{SessionID: "fixture-implementation-session", Outcome: outcome, Process: processadapter.Result{Outcome: processadapter.OutcomeExited, ExitCode: 0, StdoutPath: stdout, StderrPath: stderr}}, nil
}

func writeAcceptanceRetryCapture(path string, outcome any, stdout, stderr string) error {
	raw, err := json.Marshal(outcome)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(stdout, []byte(`{"type":"thread.started","thread_id":"fixture-session"}`+"\n"), 0o600); err != nil {
		return err
	}
	return os.WriteFile(stderr, nil, 0o600)
}

type acceptanceRetryGit struct {
	mu     sync.Mutex
	branch string
	head   string
	dirty  bool
}

func (g *acceptanceRetryGit) Head(context.Context, string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.head, nil
}

func (g *acceptanceRetryGit) Branch(context.Context, string) (string, error) { return g.branch, nil }
func (g *acceptanceRetryGit) Status(context.Context, string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.dirty {
		return " M fixture.go", nil
	}
	return "", nil
}
func (*acceptanceRetryGit) ValidateRemoteBase(context.Context, string, string, string) error {
	return nil
}
func (g *acceptanceRetryGit) CommitCandidate(context.Context, string, string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.head, g.dirty = "candidate-head", false
	return g.head, nil
}
func (*acceptanceRetryGit) CommitMetadata(context.Context, string, string) (string, string, error) {
	return "offline-base", "Controller candidate", nil
}

type acceptanceRetryVerifier struct{ git *acceptanceRetryGit }

func (v acceptanceRetryVerifier) Run(_ context.Context, ids []string, _ string, artifacts, phase string) (verifier.Evidence, error) {
	head, _ := v.git.Head(context.Background(), "")
	checks := make([]verifier.CheckEvidence, 0, len(ids))
	for index, id := range ids {
		stdout := filepath.Join(artifacts, fmt.Sprintf("%s-verifier-%02d-%s.stdout.txt", phase, index+1, id))
		stderr := filepath.Join(artifacts, fmt.Sprintf("%s-verifier-%02d-%s.stderr.txt", phase, index+1, id))
		if err := os.WriteFile(stdout, []byte("fixture verifier passed\n"), 0o600); err != nil {
			return verifier.Evidence{}, err
		}
		if err := os.WriteFile(stderr, nil, 0o600); err != nil {
			return verifier.Evidence{}, err
		}
		checks = append(checks, verifier.CheckEvidence{VerifierID: id, Program: "fixture-verifier", ProcessOutcome: processadapter.OutcomeExited, ExitCode: 0, StdoutPath: stdout, StderrPath: stderr})
	}
	evidence := verifier.Evidence{VerifiedHeadSHA: head, Checks: checks}
	raw, err := json.Marshal(evidence)
	if err != nil {
		return verifier.Evidence{}, err
	}
	if err := os.WriteFile(filepath.Join(artifacts, phase+"-verification.json"), raw, 0o600); err != nil {
		return verifier.Evidence{}, err
	}
	return evidence, nil
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
