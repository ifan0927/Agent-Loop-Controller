package main

import (
	"context"
	"errors"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type admissionWorkerResult struct {
	Cycles         int
	LastOutcome    string
	QueueDecision  *application.LinearTodoQueueDecision
	Stopped        string
	Status         string
	PreviousStatus string
}

const (
	workerStatusRunning  = "running"
	workerStatusParked   = "parked"
	workerStatusDriving  = "driving"
	workerStatusStopping = "stopping"
)

type admissionWorkerDispatch func(context.Context) (application.LinearTodoDispatchResult, error)
type admissionWorkerWait func(context.Context, time.Duration) error
type admissionWorkerObserve func(admissionWorkerResult) error

// runAdmissionWorker owns only cadence. Retry policy is persisted by the
// dispatcher per run and phase; the worker only waits for the returned durable
// eligibility time. One dispatch retains sole authority for lease acquisition,
// recovery, candidate selection, and delivery.
func runAdmissionWorker(ctx context.Context, once bool, poll time.Duration, dispatch admissionWorkerDispatch, wait admissionWorkerWait) (admissionWorkerResult, error) {
	return runAdmissionWorkerObserved(ctx, once, poll, dispatch, wait, nil)
}

func runAdmissionWorkerAt(ctx context.Context, once bool, poll time.Duration, dispatch admissionWorkerDispatch, wait admissionWorkerWait, now func() time.Time) (admissionWorkerResult, error) {
	return runAdmissionWorkerAtObserved(ctx, once, poll, dispatch, wait, now, nil)
}

func runAdmissionWorkerObserved(ctx context.Context, once bool, poll time.Duration, dispatch admissionWorkerDispatch, wait admissionWorkerWait, observe admissionWorkerObserve) (admissionWorkerResult, error) {
	return runAdmissionWorkerAtObserved(ctx, once, poll, dispatch, wait, func() time.Time { return time.Now().UTC() }, observe)
}

func runAdmissionWorkerAtObserved(ctx context.Context, once bool, poll time.Duration, dispatch admissionWorkerDispatch, wait admissionWorkerWait, now func() time.Time, observe admissionWorkerObserve) (admissionWorkerResult, error) {
	if poll <= 0 || dispatch == nil || wait == nil || now == nil {
		return admissionWorkerResult{}, errors.New("automatic admission worker configuration is invalid")
	}
	result := admissionWorkerResult{Status: workerStatusRunning}
	if err := observeAdmissionWorker(observe, result); err != nil {
		return result, err
	}
	for {
		if err := ctx.Err(); err != nil {
			stopAdmissionWorker(&result)
			if observeErr := observeAdmissionWorker(observe, result); observeErr != nil {
				return result, observeErr
			}
			return result, nil
		}
		result.Cycles++
		result.PreviousStatus, result.Status = result.Status, workerStatusDriving
		if err := observeAdmissionWorker(observe, result); err != nil {
			return result, err
		}
		cycle, err := dispatch(ctx)
		// A cycle can surface context cancellation from a Linear read or a
		// long-running driver. Treat the worker context as authoritative before
		// --once or retry policy so the CLI can emit its sanitized final status.
		if ctx.Err() != nil {
			stopAdmissionWorker(&result)
			if observeErr := observeAdmissionWorker(observe, result); observeErr != nil {
				return result, observeErr
			}
			return result, nil
		}
		if err != nil {
			return result, err
		}
		result.LastOutcome = cycle.Outcome
		result.QueueDecision = cycle.QueueDecision
		result.PreviousStatus, result.Status = result.Status, admissionWorkerStatus(cycle)
		if err := observeAdmissionWorker(observe, result); err != nil {
			return result, err
		}
		if once {
			result.Stopped = "once"
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
			stopAdmissionWorker(&result)
			if observeErr := observeAdmissionWorker(observe, result); observeErr != nil {
				return result, observeErr
			}
			return result, nil
		}
	}
}

func observeAdmissionWorker(observe admissionWorkerObserve, result admissionWorkerResult) error {
	if observe == nil {
		return nil
	}
	return observe(result)
}

func stopAdmissionWorker(result *admissionWorkerResult) {
	if result == nil {
		return
	}
	result.PreviousStatus, result.Status = result.Status, workerStatusStopping
	result.Stopped = "canceled"
}

func admissionWorkerStatus(cycle application.LinearTodoDispatchResult) string {
	if cycle.Outcome == application.LinearTodoDispatchAttention {
		return workerStatusParked
	}
	if cycle.Outcome == application.LinearTodoDispatchDriven {
		state := cycle.Run.State
		if cycle.Drive != nil && cycle.Drive.Run.State != "" {
			state = cycle.Drive.Run.State
		}
		switch state {
		case domain.StateAwaitingHumanDecision, domain.StateManualIntervention:
			return workerStatusParked
		}
	}
	return workerStatusRunning
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
