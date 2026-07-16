package application

import (
	"context"
	"errors"
	"testing"
	"time"

	processadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/process"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestAutomaticRetryClassificationFailsClosedExceptTypedStartAndUnavailable(t *testing.T) {
	if class, reason := ClassifyAutomaticRetryFailure(&ServiceError{Category: ErrorUnavailable, Message: "temporary"}); class != RetryFailureUnavailable || reason != RetryReasonUnavailable {
		t.Fatalf("unavailable class=%s reason=%s", class, reason)
	}
	if class, reason := ClassifyAutomaticRetryFailure(&ServiceError{Category: ErrorConflict, Message: "authority"}); class != RetryFailureAuthority || reason != RetryReasonAuthority {
		t.Fatalf("conflict class=%s reason=%s", class, reason)
	}
	if class, reason := ClassifyAutomaticRetryFailure(processadapter.FailureError{Category: processadapter.FailureStart}); class != RetryFailureTerminal || reason != RetryReasonTerminal {
		t.Fatalf("untyped process class=%s reason=%s", class, reason)
	}
	typed := typedRetryEvidence{}
	if class, reason := ClassifyAutomaticRetryFailure(typed); class != RetryFailureProcessStart || reason != RetryReasonProcessStart {
		t.Fatalf("typed process class=%s reason=%s", class, reason)
	}
	if class, reason := ClassifyAutomaticRetryFailure(typedRetryEvidence{class: RetryFailureIntegrity}); class != RetryFailureIntegrity || reason != RetryReasonIntegrity || RetryFailureIsRetryable(class) {
		t.Fatalf("typed integrity class=%s reason=%s", class, reason)
	}
	if class, reason := ClassifyAutomaticRetryFailure(errors.New("verification integrity failure")); class != RetryFailureTerminal || reason != RetryReasonTerminal {
		t.Fatalf("integrity class=%s reason=%s", class, reason)
	}
}

type typedRetryEvidence struct{ class RetryFailureClass }

func (typedRetryEvidence) Error() string { return "typed process failure" }
func (e typedRetryEvidence) AutomaticRetryFailureClass() string {
	if e.class == "" {
		return string(RetryFailureProcessStart)
	}
	return string(e.class)
}

func TestLinearTodoDispatcherPersistsRetryWaitAndClearsOnlySameRunPhase(t *testing.T) {
	dispatcher, store, scanner, _, _, driver := newDispatchLab(t)
	run := authorizeDispatchRun(Run{ID: "run-retry", IssueID: "IFAN-48", IdempotencyKey: "retry-key", Repository: "owner/repo", State: domain.StateExecuting})
	store.run = run
	now := time.Date(2026, 7, 15, 6, 0, 0, 0, time.UTC)
	dispatcher.now = func() time.Time { return now }
	dispatcher.policy.Retry = AutomaticRetryPolicy{MaxAttempts: 2, InitialDelay: time.Second, MaximumDelay: 2 * time.Second}
	driver.err = &ServiceError{Category: ErrorUnavailable, Message: "temporary"}
	first, err := dispatcher.Dispatch(context.Background())
	if err != nil || first.Outcome != LinearTodoDispatchRetryScheduled || first.Retry == nil || first.Retry.AttemptCount != 1 || len(driver.calls) != 1 || scanner.calls != 0 {
		t.Fatalf("first=%+v driver=%d scanner=%d err=%v", first, len(driver.calls), scanner.calls, err)
	}
	second, err := dispatcher.Dispatch(context.Background())
	if err != nil || second.Outcome != LinearTodoDispatchRetryWait || second.Retry == nil || second.Retry.NextEligibleAt != first.Retry.NextEligibleAt || len(driver.calls) != 1 || scanner.calls != 0 {
		t.Fatalf("wait=%+v driver=%d scanner=%d err=%v", second, len(driver.calls), scanner.calls, err)
	}
	now = first.Retry.NextEligibleAt
	driver.err = nil
	third, err := dispatcher.Dispatch(context.Background())
	if err != nil || third.Outcome != LinearTodoDispatchDriven || len(driver.calls) != 2 || scanner.calls != 0 {
		t.Fatalf("third=%+v driver=%d scanner=%d err=%v", third, len(driver.calls), scanner.calls, err)
	}
	if schedules, listErr := store.ListRetrySchedules(context.Background()); listErr != nil || len(schedules) != 0 {
		t.Fatalf("schedules=%+v err=%v", schedules, listErr)
	}
}

func TestLinearTodoDispatcherUsesPersistedStateAfterInitialStartFailure(t *testing.T) {
	candidate := LinearTodoCandidate{}
	candidate = dispatchCandidate("initial-retry", "IFAN-49", 1)
	dispatcher, store, scanner, _, _, driver := newDispatchLab(t, candidate)
	dispatcher.policy.Retry = AutomaticRetryPolicy{MaxAttempts: 2, InitialDelay: time.Second, MaximumDelay: 2 * time.Second}
	driver.err = &ServiceError{Category: ErrorUnavailable, Message: "temporary"}

	result, err := dispatcher.Dispatch(context.Background())
	if err != nil || result.Outcome != LinearTodoDispatchRetryScheduled || result.Retry == nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if result.Retry.Phase != AutomaticRetryPhaseForRun(store.run) || result.Retry.ControllerState != string(domain.StateExecuting) {
		t.Fatalf("retry=%+v persisted run=%+v", result.Retry, store.run)
	}
	if scanner.calls != 1 || len(driver.calls) != 1 {
		t.Fatalf("scanner=%d driver=%d", scanner.calls, len(driver.calls))
	}
}

func TestLinearTodoDispatcherStopsRetryForTerminalRun(t *testing.T) {
	dispatcher, store, _, _, _, _ := newDispatchLab(t)
	run := authorizeDispatchRun(Run{ID: "run-terminal-retry", IssueID: "IFAN-50", IdempotencyKey: "terminal-key", Repository: "owner/repo", State: domain.StateCompleted})
	store.run = run
	now := time.Date(2026, 7, 15, 7, 0, 0, 0, time.UTC)
	dispatcher.now = func() time.Time { return now }
	store.retrySchedules = []RetrySchedule{{RunID: run.ID, Phase: AutomaticRetryPhaseForRun(run), ControllerState: string(run.State), AttemptCount: 1, MaxAttempts: 2, InitialDelay: time.Second, MaximumDelay: 30 * time.Second, FailureClass: RetryFailureUnavailable, ReasonCode: RetryReasonUnavailable, Status: RetryScheduleScheduled, NextEligibleAt: now.Add(time.Minute), CreatedAt: now, UpdatedAt: now}}

	result, err := dispatcher.Dispatch(context.Background())
	if err != nil || result.Outcome != LinearTodoDispatchAttention || result.Retry == nil || result.Retry.Status != RetryScheduleAttention || result.Retry.ReasonCode != RetryReasonTerminal {
		t.Fatalf("result=%+v schedule=%+v err=%v", result, store.retrySchedules, err)
	}
}

func TestLinearTodoDispatcherIgnoresRetainedTerminalRetryAttention(t *testing.T) {
	candidate := dispatchCandidate("fresh-after-terminal", "IFAN-54", 1)
	dispatcher, store, scanner, _, _, driver := newDispatchLab(t, candidate)
	terminal := authorizeDispatchRun(Run{ID: "run-abandoned", IssueID: "IFAN-53", IdempotencyKey: "abandoned-key", Repository: "owner/repo", State: domain.StateFailed})
	store.run = terminal
	now := time.Date(2026, 7, 15, 7, 30, 0, 0, time.UTC)
	dispatcher.now = func() time.Time { return now }
	store.retrySchedules = []RetrySchedule{{RunID: terminal.ID, Phase: AutomaticRetryPhaseForRun(terminal), ControllerState: string(domain.StateManualIntervention), AttemptCount: 1, MaxAttempts: 2, InitialDelay: time.Second, MaximumDelay: 30 * time.Second, FailureClass: RetryFailureManual, ReasonCode: RetryReasonManual, Status: RetryScheduleAttention, AttentionAt: now.Add(-time.Minute), CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute)}}

	result, err := dispatcher.Dispatch(context.Background())
	if err != nil || result.Outcome != LinearTodoDispatchDriven || scanner.calls != 1 || len(driver.calls) != 1 {
		t.Fatalf("result=%+v scanner=%d driver=%d err=%v", result, scanner.calls, len(driver.calls), err)
	}
	if len(store.retrySchedules) != 1 || store.retrySchedules[0].Status != RetryScheduleAttention {
		t.Fatalf("terminal retry audit evidence changed: %+v", store.retrySchedules)
	}
}

func TestLinearTodoDispatcherBoundsTypedProcessStartRetry(t *testing.T) {
	dispatcher, store, scanner, _, _, driver := newDispatchLab(t)
	run := authorizeDispatchRun(Run{ID: "run-process-start", IssueID: "IFAN-51", IdempotencyKey: "process-start-key", Repository: "owner/repo", State: domain.StateExecuting})
	store.run = run
	now := time.Date(2026, 7, 15, 8, 0, 0, 0, time.UTC)
	dispatcher.now = func() time.Time { return now }
	dispatcher.policy.Retry = AutomaticRetryPolicy{MaxAttempts: 1, InitialDelay: time.Second, MaximumDelay: time.Second}
	driver.err = typedRetryEvidence{}

	first, err := dispatcher.Dispatch(context.Background())
	if err != nil || first.Outcome != LinearTodoDispatchRetryScheduled || first.Retry == nil {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	now = first.Retry.NextEligibleAt
	second, err := dispatcher.Dispatch(context.Background())
	if err != nil || second.Outcome != LinearTodoDispatchAttention || second.Retry == nil || second.Retry.Status != RetryScheduleAttention || second.Retry.ReasonCode != RetryReasonBudgetExhausted {
		t.Fatalf("second=%+v attention=%+v err=%v", second, store.attention, err)
	}
	if scanner.calls != 0 || len(driver.calls) != 2 || len(store.attention) != 1 {
		t.Fatalf("scanner=%d driver=%d attention=%d", scanner.calls, len(driver.calls), len(store.attention))
	}
}
