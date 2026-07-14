package main

import (
	"context"
	"errors"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

const (
	workerInitialBackoff = time.Second
	workerMaximumBackoff = 30 * time.Second
)

type admissionWorkerResult struct {
	Cycles      int
	LastOutcome string
	Stopped     string
}

type admissionWorkerDispatch func(context.Context) (application.LinearTodoDispatchResult, error)
type admissionWorkerWait func(context.Context, time.Duration) error

// runAdmissionWorker owns only cadence and retry policy. One dispatch retains
// sole authority for lease acquisition, recovery, candidate selection, and
// delivery; an attention outcome deliberately stops this worker before it can
// consider another issue.
func runAdmissionWorker(ctx context.Context, once bool, poll time.Duration, dispatch admissionWorkerDispatch, wait admissionWorkerWait) (admissionWorkerResult, error) {
	if poll <= 0 || dispatch == nil || wait == nil {
		return admissionWorkerResult{}, errors.New("automatic admission worker configuration is invalid")
	}
	backoff := workerInitialBackoff
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
			if once || !retryableWorkerError(err) {
				return result, err
			}
			result.Stopped = "retry_backoff"
			if err := wait(ctx, backoff); err != nil {
				result.Stopped = "canceled"
				return result, nil
			}
			backoff *= 2
			if backoff > workerMaximumBackoff {
				backoff = workerMaximumBackoff
			}
			continue
		}
		backoff = workerInitialBackoff
		result.LastOutcome = cycle.Outcome
		if once {
			result.Stopped = "once"
			return result, nil
		}
		if cycle.Outcome == application.LinearTodoDispatchAttention {
			result.Stopped = "attention_required"
			return result, nil
		}
		if err := wait(ctx, poll); err != nil {
			result.Stopped = "canceled"
			return result, nil
		}
	}
}

func retryableWorkerError(err error) bool {
	var service *application.ServiceError
	return errors.As(err, &service) && service.Category == application.ErrorUnavailable
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
