package main

import (
	"context"
	"errors"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

type admissionWorkerResult struct {
	Cycles      int
	LastOutcome string
	Stopped     string
}

type admissionWorkerDispatch func(context.Context) (application.LinearTodoDispatchResult, error)
type admissionWorkerWait func(context.Context, time.Duration) error

// runAdmissionWorker owns only cadence. Retry policy is persisted by the
// dispatcher per run and phase; the worker only waits for the returned durable
// eligibility time. One dispatch retains sole authority for lease acquisition,
// recovery, candidate selection, and delivery.
func runAdmissionWorker(ctx context.Context, once bool, poll time.Duration, dispatch admissionWorkerDispatch, wait admissionWorkerWait) (admissionWorkerResult, error) {
	return runAdmissionWorkerAt(ctx, once, poll, dispatch, wait, func() time.Time { return time.Now().UTC() })
}

func runAdmissionWorkerAt(ctx context.Context, once bool, poll time.Duration, dispatch admissionWorkerDispatch, wait admissionWorkerWait, now func() time.Time) (admissionWorkerResult, error) {
	if poll <= 0 || dispatch == nil || wait == nil || now == nil {
		return admissionWorkerResult{}, errors.New("automatic admission worker configuration is invalid")
	}
	var result admissionWorkerResult
	for {
		if err := ctx.Err(); err != nil {
			result.Stopped = "canceled"
			return result, nil
		}
		result.Cycles++
		cycle, err := dispatch(ctx)
		// A cycle can surface context cancellation from a Linear read or a
		// long-running driver. Treat the worker context as authoritative before
		// --once or retry policy so the CLI can emit its sanitized final status.
		if ctx.Err() != nil {
			result.Stopped = "canceled"
			return result, nil
		}
		if err != nil {
			return result, err
		}
		result.LastOutcome = cycle.Outcome
		if once {
			result.Stopped = "once"
			return result, nil
		}
		if cycle.Outcome == application.LinearTodoDispatchAttention {
			result.Stopped = "attention_required"
			return result, nil
		}
		delay := poll
		if cycle.Outcome == application.LinearTodoDispatchRetryWait || cycle.Outcome == application.LinearTodoDispatchRetryScheduled {
			if cycle.Retry == nil || cycle.Retry.NextEligibleAt.IsZero() {
				return result, errors.New("durable retry outcome is missing eligibility evidence")
			}
			delay = cycle.Retry.NextEligibleAt.Sub(now().UTC())
			if delay < 0 {
				delay = 0
			}
		}
		if err := wait(ctx, delay); err != nil {
			result.Stopped = "canceled"
			return result, nil
		}
	}
}

func waitAdmissionWorker(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
