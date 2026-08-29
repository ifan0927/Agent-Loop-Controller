package main

import (
	"context"
	"errors"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type admissionWorkerResult struct {
	Cycles                    int
	LastOutcome               string
	QueueDecision             *application.LinearTodoQueueDecision
	Stopped                   string
	Status                    string
	PreviousStatus            string
	LastCycleCompletedAt      time.Time
	NextAdmissionEvaluationAt time.Time
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
type admissionWorkerMaintenance func(context.Context)

type boundedAdmissionWorker struct {
	capacity int
	dispatch admissionWorkerDispatch
	results  chan application.LinearTodoDispatchResult
	errors   chan error
	active   int
}

func (w *boundedAdmissionWorker) drain() {
	_ = w.drainWithError()
}

func (w *boundedAdmissionWorker) drainWithError() error {
	var firstErr error
	for w.active > 0 {
		select {
		case err := <-w.errors:
			if firstErr == nil {
				firstErr = err
			}
			w.active--
		case <-w.results:
			w.active--
		}
	}
	return firstErr
}

func newBoundedAdmissionWorker(capacity int, dispatch admissionWorkerDispatch) (*boundedAdmissionWorker, error) {
	if capacity < 1 || capacity > application.MaxHeavyCapacity+1 || dispatch == nil {
		return nil, errors.New("bounded admission worker configuration is invalid")
	}
	return &boundedAdmissionWorker{capacity: capacity, dispatch: dispatch, results: make(chan application.LinearTodoDispatchResult, capacity), errors: make(chan error, capacity)}, nil
}

func (w *boundedAdmissionWorker) fill(ctx context.Context) {
	// Consume completed dispatches before refilling. Otherwise an idle batch can
	// continuously replace one buffered no-candidate result at a time and turn
	// the configured admission poll interval into a busy loop.
	if len(w.results)+len(w.errors) > 0 {
		return
	}
	for w.active < w.capacity {
		w.active++
		go func() {
			result, err := w.dispatch(ctx)
			if err != nil {
				w.errors <- err
				return
			}
			w.results <- result
		}()
	}
}

func (w *boundedAdmissionWorker) fillReady(ctx context.Context, now, notBefore time.Time) {
	if !notBefore.IsZero() && now.Before(notBefore) {
		return
	}
	w.fill(ctx)
}

func (w *boundedAdmissionWorker) next(ctx context.Context) (application.LinearTodoDispatchResult, error) {
	select {
	case <-ctx.Done():
		return application.LinearTodoDispatchResult{}, ctx.Err()
	case err := <-w.errors:
		w.active--
		return application.LinearTodoDispatchResult{}, err
	case result := <-w.results:
		w.active--
		return result, nil
	}
}

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
	return runBoundedAdmissionWorkerAtObserved(ctx, once, poll, 1, dispatch, wait, now, observe)
}

func runBoundedAdmissionWorkerAtObserved(ctx context.Context, once bool, poll time.Duration, capacity int, dispatch admissionWorkerDispatch, wait admissionWorkerWait, now func() time.Time, observe admissionWorkerObserve) (admissionWorkerResult, error) {
	return runBoundedAdmissionWorkerAtObservedWithMaintenance(ctx, once, poll, capacity, dispatch, wait, now, observe, nil)
}

func runBoundedAdmissionWorkerAtObservedWithMaintenance(ctx context.Context, once bool, poll time.Duration, capacity int, dispatch admissionWorkerDispatch, wait admissionWorkerWait, now func() time.Time, observe admissionWorkerObserve, maintenance admissionWorkerMaintenance) (admissionWorkerResult, error) {
	if poll <= 0 || dispatch == nil || wait == nil || now == nil {
		return admissionWorkerResult{}, errors.New("automatic admission worker configuration is invalid")
	}
	bounded, err := newBoundedAdmissionWorker(capacity, dispatch)
	if err != nil {
		return admissionWorkerResult{}, err
	}
	dispatchCtx, cancelDispatches := context.WithCancel(ctx)
	defer cancelDispatches()
	joinDispatches := func() {
		cancelDispatches()
		bounded.drain()
	}
	result := admissionWorkerResult{Status: workerStatusRunning}
	if err := observeAdmissionWorker(observe, result); err != nil {
		return result, err
	}
	var nextWake time.Time
	var nextAdmissionScan time.Time
	maintenancePending := false
	for {
		if err := ctx.Err(); err != nil {
			joinDispatches()
			stopAdmissionWorker(&result)
			if observeErr := observeAdmissionWorker(observe, result); observeErr != nil {
				return result, observeErr
			}
			return result, nil
		}
		cycleNow := now().UTC()
		runWakeDue := !nextWake.IsZero() && !cycleNow.Before(nextWake)
		if runWakeDue {
			nextWake = time.Time{}
		}
		if len(bounded.results)+len(bounded.errors) == 0 && !runWakeDue && !nextAdmissionScan.IsZero() && cycleNow.Before(nextAdmissionScan) {
			delay := nextAdmissionScan.Sub(cycleNow)
			if bounded.active > 0 && delay > 250*time.Millisecond {
				delay = 250 * time.Millisecond
			}
			if err := wait(ctx, delay); err != nil {
				joinDispatches()
				stopAdmissionWorker(&result)
				if observeErr := observeAdmissionWorker(observe, result); observeErr != nil {
					return result, observeErr
				}
				return result, nil
			}
			continue
		}
		result.Cycles++
		result.PreviousStatus, result.Status = result.Status, workerStatusDriving
		if err := observeAdmissionWorker(observe, result); err != nil {
			joinDispatches()
			return result, err
		}
		dispatchNotBefore := nextAdmissionScan
		if runWakeDue {
			dispatchNotBefore = time.Time{}
		}
		if !maintenancePending {
			bounded.fillReady(dispatchCtx, cycleNow, dispatchNotBefore)
		}
		cycle, err := bounded.next(ctx)
		// A cycle can surface context cancellation from a Linear read or a
		// long-running driver. Treat the worker context as authoritative before
		// --once or retry policy so the CLI can emit its sanitized final status.
		if ctx.Err() != nil {
			joinDispatches()
			stopAdmissionWorker(&result)
			if observeErr := observeAdmissionWorker(observe, result); observeErr != nil {
				return result, observeErr
			}
			return result, nil
		}
		if err != nil {
			// One dispatch error must not cancel healthy repository siblings. Let
			// the bounded batch settle before the supervisor reports the error.
			bounded.drain()
			return result, err
		}
		maintenancePending = true
		result.LastOutcome = cycle.Outcome
		result.QueueDecision = cycle.QueueDecision
		result.LastCycleCompletedAt = now().UTC()
		if !once && (cycle.Outcome == application.LinearTodoDispatchNoCandidate || cycle.QueueDecision != nil && cycle.QueueDecision.Reason == application.LinearTodoQueueDecisionNoEligibleCandidate) {
			nextAdmissionScan = result.LastCycleCompletedAt.Add(poll)
		} else {
			nextAdmissionScan = time.Time{}
		}
		if !cycle.NextRunnableAt.IsZero() && (nextWake.IsZero() || cycle.NextRunnableAt.Before(nextWake)) {
			nextWake = cycle.NextRunnableAt
		}
		if once {
			result.NextAdmissionEvaluationAt = time.Time{}
		} else {
			result.NextAdmissionEvaluationAt = earliestWorkerEvaluation(nextAdmissionScan, nextWake)
		}
		nextStatus := admissionWorkerStatus(cycle)
		if bounded.active > 0 {
			nextStatus = workerStatusDriving
		}
		result.PreviousStatus, result.Status = result.Status, nextStatus
		if err := observeAdmissionWorker(observe, result); err != nil {
			joinDispatches()
			return result, err
		}
		if once {
			// Production --once uses capacity one. Draining rather than canceling
			// also preserves the quiescence contract for direct bounded fixtures.
			if err := bounded.drainWithError(); err != nil {
				return result, err
			}
			if maintenancePending && maintenance != nil {
				maintenance(ctx)
			}
			result.Stopped = "once"
			return result, nil
		}
		if maintenancePending && bounded.active == 0 && len(bounded.results)+len(bounded.errors) == 0 {
			if maintenance != nil {
				maintenance(ctx)
			}
			maintenancePending = false
		}
		delay := poll
		if cycle.Outcome == application.LinearTodoDispatchRetryWait || cycle.Outcome == application.LinearTodoDispatchRetryScheduled {
			if cycle.Retry == nil || cycle.Retry.NextEligibleAt.IsZero() {
				joinDispatches()
				return result, errors.New("durable retry outcome is missing eligibility evidence")
			}
			// Eligibility is already persisted on the individual run. Immediately
			// give another repository a scheduling opportunity instead of turning
			// one run's backoff into a global worker sleep.
			delay = 0
		} else if len(bounded.results)+len(bounded.errors) > 0 {
			// Drain the rest of a completed batch before starting replacements.
			delay = 0
		} else if bounded.active > 0 && delay > 250*time.Millisecond {
			// Sibling dispatches may still be driving runs or handing off the
			// short admission lease. Keep filling available supervisor slots
			// without turning an idle worker into a busy loop.
			delay = 250 * time.Millisecond
		}
		if !nextWake.IsZero() {
			remaining := nextWake.Sub(now().UTC())
			if remaining <= 0 {
				nextWake = time.Time{}
				delay = 0
			} else if remaining < delay {
				delay = remaining
			}
		}
		if err := wait(ctx, delay); err != nil {
			joinDispatches()
			stopAdmissionWorker(&result)
			if observeErr := observeAdmissionWorker(observe, result); observeErr != nil {
				return result, observeErr
			}
			return result, nil
		}
	}
}

func earliestWorkerEvaluation(values ...time.Time) time.Time {
	var earliest time.Time
	for _, value := range values {
		if value.IsZero() {
			continue
		}
		value = value.UTC()
		if earliest.IsZero() || value.Before(earliest) {
			earliest = value
		}
	}
	return earliest
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
