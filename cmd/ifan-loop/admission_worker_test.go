package main

import (
	"context"
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
	if err != nil || calls != 1 || waits != 0 || result.Cycles != 1 || result.LastOutcome != application.LinearTodoDispatchNoCandidate || result.Stopped != "once" {
		t.Fatalf("result=%+v calls=%d waits=%d err=%v", result, calls, waits, err)
	}
}

func TestAdmissionWorkerStopsOnAttentionBeforeAnotherAdmission(t *testing.T) {
	calls := 0
	result, err := runAdmissionWorker(context.Background(), false, time.Minute, func(context.Context) (application.LinearTodoDispatchResult, error) {
		calls++
		return application.LinearTodoDispatchResult{Outcome: application.LinearTodoDispatchAttention}, nil
	}, func(context.Context, time.Duration) error {
		t.Fatal("attention must not enter another poll")
		return nil
	})
	if err != nil || calls != 1 || result.Stopped != "attention_required" {
		t.Fatalf("result=%+v calls=%d err=%v", result, calls, err)
	}
}

func TestAdmissionWorkerRetriesOnlyUnavailableFailuresWithBoundedBackoff(t *testing.T) {
	calls := 0
	var waits []time.Duration
	result, err := runAdmissionWorker(context.Background(), false, time.Minute, func(context.Context) (application.LinearTodoDispatchResult, error) {
		calls++
		if calls == 1 {
			return application.LinearTodoDispatchResult{}, &application.ServiceError{Category: application.ErrorUnavailable, Message: "unavailable"}
		}
		return application.LinearTodoDispatchResult{Outcome: application.LinearTodoDispatchAttention}, nil
	}, func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	})
	if err != nil || calls != 2 || len(waits) != 1 || waits[0] != workerInitialBackoff || result.Cycles != 2 || result.Stopped != "attention_required" {
		t.Fatalf("result=%+v calls=%d waits=%v err=%v", result, calls, waits, err)
	}

	_, err = runAdmissionWorker(context.Background(), false, time.Minute, func(context.Context) (application.LinearTodoDispatchResult, error) {
		return application.LinearTodoDispatchResult{}, errors.New("not typed retryable")
	}, func(context.Context, time.Duration) error { t.Fatal("non-retryable error waited"); return nil })
	if err == nil {
		t.Fatal("non-retryable failure was accepted")
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

func TestAdmissionWorkerMaxRuntimeCancellationDuringOnceDispatchIsAStatus(t *testing.T) {
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
