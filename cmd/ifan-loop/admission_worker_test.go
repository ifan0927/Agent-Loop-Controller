package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

func TestAdmissionWorkerOnceDispatchesExactlyOneCycle(t *testing.T) {
	calls, waits := 0, 0
	result, err := runAdmissionWorker(context.Background(), true, time.Minute, func(context.Context) (application.LinearTodoDispatchResult, error) {
		calls++
		return application.LinearTodoDispatchResult{Outcome: application.LinearTodoDispatchNoCandidate}, nil
	}, func(context.Context, time.Duration) error {
		waits++
		return nil
	})
	if err != nil || calls != 1 || waits != 0 || result.Cycles != 1 || result.LastOutcome != application.LinearTodoDispatchNoCandidate || result.Stopped != "once" || result.Status != workerStatusRunning {
		t.Fatalf("result=%+v calls=%d waits=%d err=%v", result, calls, waits, err)
	}
}

func TestAdmissionWorkerProjectsSanitizedQueueDecision(t *testing.T) {
	priority := 0
	decision := &application.LinearTodoQueueDecision{Reason: application.LinearTodoQueueDecisionSelectedPriority, CandidateCount: 3, SelectedPriority: &priority}
	result, err := runAdmissionWorker(context.Background(), true, time.Minute, func(context.Context) (application.LinearTodoDispatchResult, error) {
		return application.LinearTodoDispatchResult{Outcome: application.LinearTodoDispatchDriven, QueueDecision: decision}, nil
	}, func(context.Context, time.Duration) error { t.Fatal("once worker must not wait"); return nil })
	if err != nil || result.QueueDecision == nil || result.QueueDecision.Reason != application.LinearTodoQueueDecisionSelectedPriority || result.QueueDecision.CandidateCount != 3 || result.QueueDecision.SelectedPriority == nil || *result.QueueDecision.SelectedPriority != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	raw, err := json.Marshal(workerOutput{QueueDecision: result.QueueDecision, Stopped: result.Stopped})
	if err != nil {
		t.Fatal(err)
	}
	var projected workerOutput
	if err := json.Unmarshal(raw, &projected); err != nil || projected.QueueDecision == nil || projected.QueueDecision.SelectedPriority == nil || *projected.QueueDecision.SelectedPriority != 0 {
		t.Fatalf("projected=%+v raw=%s err=%v", projected, raw, err)
	}
}

func TestAdmissionWorkerKeepsPollingWhileAttentionParksAdmission(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	result, err := runAdmissionWorker(ctx, false, time.Minute, func(context.Context) (application.LinearTodoDispatchResult, error) {
		calls++
		return application.LinearTodoDispatchResult{Outcome: application.LinearTodoDispatchAttention}, nil
	}, func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	})
	if err != nil || calls != 1 || result.Stopped != "canceled" || result.LastOutcome != application.LinearTodoDispatchAttention || result.Status != workerStatusStopping || result.PreviousStatus != workerStatusParked {
		t.Fatalf("result=%+v calls=%d err=%v", result, calls, err)
	}
}

func TestAdmissionWorkerAutomaticallyResumesAfterOperatorRetryBecomesEligible(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	retryApplied := false
	result, err := runAdmissionWorker(ctx, false, time.Minute, func(context.Context) (application.LinearTodoDispatchResult, error) {
		calls++
		if !retryApplied {
			return application.LinearTodoDispatchResult{Outcome: application.LinearTodoDispatchAttention}, nil
		}
		return application.LinearTodoDispatchResult{Outcome: application.LinearTodoDispatchDriven}, nil
	}, func(context.Context, time.Duration) error {
		if !retryApplied {
			retryApplied = true
			return nil
		}
		cancel()
		return context.Canceled
	})
	if err != nil || calls != 2 || result.Stopped != "canceled" || result.Cycles != 2 || result.LastOutcome != application.LinearTodoDispatchDriven || result.PreviousStatus != workerStatusRunning {
		t.Fatalf("result=%+v calls=%d err=%v", result, calls, err)
	}
}

func TestAdmissionWorkerObservesLiveStatusTransitions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var statuses []string
	result, err := runAdmissionWorkerObserved(ctx, false, time.Minute, func(context.Context) (application.LinearTodoDispatchResult, error) {
		return application.LinearTodoDispatchResult{Outcome: application.LinearTodoDispatchAttention}, nil
	}, func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}, func(result admissionWorkerResult) error {
		statuses = append(statuses, result.Status)
		return nil
	})
	want := []string{workerStatusRunning, workerStatusDriving, workerStatusParked, workerStatusStopping}
	if err != nil || result.Status != workerStatusStopping || len(statuses) != len(want) {
		t.Fatalf("result=%+v statuses=%v err=%v", result, statuses, err)
	}
	for index := range want {
		if statuses[index] != want[index] {
			t.Fatalf("statuses=%v want=%v", statuses, want)
		}
	}
}

func TestAdmissionWorkerProjectsDrivingAndParkedStatuses(t *testing.T) {
	decision := application.LinearTodoDispatchResult{
		Outcome: application.LinearTodoDispatchDriven,
		Run:     application.RunResult{State: "awaiting_human_decision"},
		Drive:   &application.ProductionDriveResult{},
	}
	if status := admissionWorkerStatus(decision); status != workerStatusParked {
		t.Fatalf("decision status=%q", status)
	}

	ctx, cancel := context.WithCancel(context.Background())
	result, err := runAdmissionWorker(ctx, false, time.Minute, func(context.Context) (application.LinearTodoDispatchResult, error) {
		cancel()
		return application.LinearTodoDispatchResult{}, context.Canceled
	}, func(context.Context, time.Duration) error { t.Fatal("canceled dispatch must not wait"); return nil })
	if err != nil || result.Status != workerStatusStopping || result.PreviousStatus != workerStatusDriving {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestAdmissionWorkerDoesNotRecreateInMemoryRetryPolicy(t *testing.T) {
	result, err := runAdmissionWorker(context.Background(), false, time.Minute, func(context.Context) (application.LinearTodoDispatchResult, error) {
		return application.LinearTodoDispatchResult{}, &application.ServiceError{Category: application.ErrorUnavailable, Message: "unavailable"}
	}, func(context.Context, time.Duration) error { t.Fatal("worker must not own retry backoff"); return nil })
	if err == nil || result.Cycles != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}

	_, err = runAdmissionWorker(context.Background(), false, time.Minute, func(context.Context) (application.LinearTodoDispatchResult, error) {
		return application.LinearTodoDispatchResult{}, errors.New("not typed retryable")
	}, func(context.Context, time.Duration) error { t.Fatal("non-retryable error waited"); return nil })
	if err == nil {
		t.Fatal("non-retryable failure was accepted")
	}
}

func TestAdmissionWorkerWaitsForDurableRetryEligibility(t *testing.T) {
	calls := 0
	waits := []time.Duration{}
	now := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	result, err := runAdmissionWorkerAt(context.Background(), false, time.Minute, func(context.Context) (application.LinearTodoDispatchResult, error) {
		calls++
		if calls == 1 {
			schedule := application.RetrySchedule{RunID: "run", Phase: "state_executing", ControllerState: "executing", AttemptCount: 1, MaxAttempts: 3, InitialDelay: time.Second, MaximumDelay: 30 * time.Second, FailureClass: application.RetryFailureProcessStart, ReasonCode: application.RetryReasonProcessStart, Status: application.RetryScheduleScheduled, NextEligibleAt: now.Add(4 * time.Second), CreatedAt: now, UpdatedAt: now}
			return application.LinearTodoDispatchResult{Outcome: application.LinearTodoDispatchRetryScheduled, Retry: &schedule}, nil
		}
		return application.LinearTodoDispatchResult{Outcome: application.LinearTodoDispatchNoCandidate}, nil
	}, func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		if len(waits) == 2 {
			return context.Canceled
		}
		return nil
	}, func() time.Time { return now })
	if err != nil || calls != 2 || len(waits) != 2 || waits[0] != 4*time.Second || waits[1] != time.Minute || result.Stopped != "canceled" {
		t.Fatalf("result=%+v calls=%d waits=%v err=%v", result, calls, waits, err)
	}
}

func TestAdmissionWorkerHasNoSevenDayProcessExpiry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	started := now
	result, err := runAdmissionWorkerAt(ctx, false, 24*time.Hour, func(context.Context) (application.LinearTodoDispatchResult, error) {
		return application.LinearTodoDispatchResult{Outcome: application.LinearTodoDispatchNoCandidate}, nil
	}, func(context.Context, time.Duration) error {
		now = now.Add(24 * time.Hour)
		if now.Sub(started) > 7*24*time.Hour {
			cancel()
			return context.Canceled
		}
		return nil
	}, func() time.Time { return now })
	if err != nil || result.Cycles != 8 || result.Stopped != "canceled" || now.Sub(started) != 8*24*time.Hour {
		t.Fatalf("result=%+v elapsed=%s err=%v", result, now.Sub(started), err)
	}
}

func TestAdmissionWorkerCancellationInterruptsPollWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, err := runAdmissionWorker(ctx, false, time.Minute, func(context.Context) (application.LinearTodoDispatchResult, error) {
		return application.LinearTodoDispatchResult{Outcome: application.LinearTodoDispatchNoCandidate}, nil
	}, func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	})
	if err != nil || result.Cycles != 1 || result.Stopped != "canceled" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestAdmissionWorkerCancellationReturnedByOnceDispatchIsAStatusNotAnError(t *testing.T) {
	for _, test := range []struct{ name string }{{name: "Linear read"}, {name: "production driver"}} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			result, err := runAdmissionWorker(ctx, true, time.Minute, func(context.Context) (application.LinearTodoDispatchResult, error) {
				cancel()
				return application.LinearTodoDispatchResult{}, context.Canceled
			}, func(context.Context, time.Duration) error { t.Fatal("once dispatch must not wait"); return nil })
			if err != nil || result.Cycles != 1 || result.Stopped != "canceled" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestAdmissionWorkerCancellationDuringOnceDispatchIsAStatus(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result, err := runAdmissionWorker(ctx, true, time.Minute, func(ctx context.Context) (application.LinearTodoDispatchResult, error) {
		<-ctx.Done()
		return application.LinearTodoDispatchResult{}, ctx.Err()
	}, func(context.Context, time.Duration) error { t.Fatal("once dispatch must not wait"); return nil })
	if err != nil || result.Cycles != 1 || result.Stopped != "canceled" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
