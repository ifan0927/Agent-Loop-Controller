package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

func TestBoundedWorkerRunsUpToCapacityAndIsolatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan int, 3)
	release := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	var mu sync.Mutex
	next, current, maximum := 0, 0, 0
	dispatch := func(ctx context.Context) (application.LinearTodoDispatchResult, error) {
		mu.Lock()
		index := next
		next++
		current++
		if current > maximum {
			maximum = current
		}
		mu.Unlock()
		entered <- index
		select {
		case <-ctx.Done():
			return application.LinearTodoDispatchResult{}, ctx.Err()
		case <-release[index]:
		}
		mu.Lock()
		current--
		mu.Unlock()
		return application.LinearTodoDispatchResult{Outcome: application.LinearTodoDispatchDriven}, nil
	}
	worker, err := newBoundedAdmissionWorker(2, dispatch)
	if err != nil {
		t.Fatal(err)
	}
	worker.fill(ctx)
	first, second := <-entered, <-entered
	if first == second || maximum != 2 || worker.active != 2 {
		t.Fatalf("first=%d second=%d maximum=%d active=%d", first, second, maximum, worker.active)
	}
	close(release[first])
	if _, err := worker.next(ctx); err != nil {
		t.Fatal(err)
	}
	worker.fill(ctx)
	third := <-entered
	if third == second || worker.active != 2 {
		t.Fatalf("third=%d second=%d active=%d", third, second, worker.active)
	}
	close(release[second])
	if _, err := worker.next(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-release[third]:
		t.Fatal("sibling cancellation channel was affected")
	default:
	}
	close(release[third])
	if _, err := worker.next(ctx); err != nil {
		t.Fatal(err)
	}
	if worker.active != 0 {
		t.Fatalf("active=%d", worker.active)
	}
}

func TestBoundedWorkerJoinsEveryDispatchBeforeCancellationReturns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{}, 2)
	var exited atomic.Int32
	done := make(chan error, 1)
	go func() {
		_, err := runBoundedAdmissionWorkerAtObserved(ctx, false, time.Minute, 2, func(dispatchCtx context.Context) (application.LinearTodoDispatchResult, error) {
			entered <- struct{}{}
			<-dispatchCtx.Done()
			exited.Add(1)
			return application.LinearTodoDispatchResult{}, dispatchCtx.Err()
		}, waitAdmissionWorker, time.Now, nil)
		done <- err
	}()
	<-entered
	<-entered
	cancel()
	select {
	case err := <-done:
		if err != nil || exited.Load() != 2 {
			t.Fatalf("err=%v exited=%d", err, exited.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("worker returned before all dispatches joined")
	}
}

func TestBoundedWorkerGenericCapacityAboveTwo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	worker, err := newBoundedAdmissionWorker(4, func(context.Context) (application.LinearTodoDispatchResult, error) {
		entered <- struct{}{}
		<-release
		return application.LinearTodoDispatchResult{Outcome: application.LinearTodoDispatchDriven}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	worker.fill(ctx)
	for index := 0; index < 4; index++ {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("generic capacity was not filled")
		}
	}
	if worker.active != 4 {
		t.Fatalf("active=%d", worker.active)
	}
	close(release)
	for index := 0; index < 4; index++ {
		if _, err := worker.next(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBoundedWorkerDrainsCompletedIdleBatchBeforeRefill(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	worker, err := newBoundedAdmissionWorker(2, func(context.Context) (application.LinearTodoDispatchResult, error) {
		calls.Add(1)
		<-release
		return application.LinearTodoDispatchResult{Outcome: application.LinearTodoDispatchNoCandidate}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	worker.fill(context.Background())
	close(release)
	deadline := time.Now().Add(time.Second)
	for len(worker.results) != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(worker.results) != 2 {
		t.Fatalf("buffered results=%d", len(worker.results))
	}
	if _, err := worker.next(context.Background()); err != nil {
		t.Fatal(err)
	}
	worker.fill(context.Background())
	if calls.Load() != 2 || worker.active != 1 {
		t.Fatalf("calls=%d active=%d", calls.Load(), worker.active)
	}
	if _, err := worker.next(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBoundedWorkerJoinsSiblingWhenStatusObservationFails(t *testing.T) {
	secondStarted := make(chan struct{})
	var next atomic.Int32
	var siblingExited atomic.Bool
	expected := errors.New("status write failed")
	_, err := runBoundedAdmissionWorkerAtObserved(context.Background(), false, time.Minute, 2, func(ctx context.Context) (application.LinearTodoDispatchResult, error) {
		index := next.Add(1)
		if index == 1 {
			<-secondStarted
			return application.LinearTodoDispatchResult{Outcome: application.LinearTodoDispatchDriven}, nil
		}
		close(secondStarted)
		<-ctx.Done()
		siblingExited.Store(true)
		return application.LinearTodoDispatchResult{}, ctx.Err()
	}, waitAdmissionWorker, time.Now, func(result admissionWorkerResult) error {
		if result.LastOutcome == application.LinearTodoDispatchDriven {
			return expected
		}
		return nil
	})
	if !errors.Is(err, expected) || !siblingExited.Load() {
		t.Fatalf("err=%v sibling_exited=%t", err, siblingExited.Load())
	}
}

func TestBoundedWorkerUsesEarliestPersistedRunnableDeadline(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	deadline := now.Add(30 * time.Second)
	var observedDelay time.Duration
	_, err := runBoundedAdmissionWorkerAtObserved(context.Background(), false, 5*time.Minute, 1, func(context.Context) (application.LinearTodoDispatchResult, error) {
		return application.LinearTodoDispatchResult{Outcome: application.LinearTodoDispatchDriven, NextRunnableAt: deadline}, nil
	}, func(_ context.Context, delay time.Duration) error {
		observedDelay = delay
		return context.Canceled
	}, func() time.Time { return now }, nil)
	if err != nil || observedDelay != 30*time.Second {
		t.Fatalf("delay=%s err=%v", observedDelay, err)
	}
}

func TestBoundedWorkerDispatchesDueRunBeforeNextAdmissionScan(t *testing.T) {
	current := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	deadline := current.Add(30 * time.Second)
	var calls atomic.Int32
	var waits []time.Duration
	_, err := runBoundedAdmissionWorkerAtObserved(context.Background(), false, 5*time.Minute, 1, func(context.Context) (application.LinearTodoDispatchResult, error) {
		if calls.Add(1) == 1 {
			return application.LinearTodoDispatchResult{Outcome: application.LinearTodoDispatchNoCandidate, NextRunnableAt: deadline}, nil
		}
		return application.LinearTodoDispatchResult{Outcome: application.LinearTodoDispatchDriven}, nil
	}, func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		current = current.Add(delay)
		if len(waits) == 2 {
			return context.Canceled
		}
		return nil
	}, func() time.Time { return current }, nil)
	if err != nil || calls.Load() != 2 || len(waits) != 2 || waits[0] != 30*time.Second {
		t.Fatalf("calls=%d waits=%v err=%v", calls.Load(), waits, err)
	}
}

func TestBoundedWorkerJoinsSiblingWhenNextCycleStatusWriteFails(t *testing.T) {
	secondStarted := make(chan struct{})
	var calls atomic.Int32
	var drivingObservations atomic.Int32
	var siblingExited atomic.Bool
	var secondStartOnce sync.Once
	expected := errors.New("next-cycle status write failed")
	_, err := runBoundedAdmissionWorkerAtObserved(context.Background(), false, time.Minute, 2, func(ctx context.Context) (application.LinearTodoDispatchResult, error) {
		if calls.Add(1) == 1 {
			<-secondStarted
			return application.LinearTodoDispatchResult{Outcome: application.LinearTodoDispatchDriven}, nil
		}
		secondStartOnce.Do(func() { close(secondStarted) })
		<-ctx.Done()
		siblingExited.Store(true)
		return application.LinearTodoDispatchResult{}, ctx.Err()
	}, func(context.Context, time.Duration) error { return nil }, time.Now, func(result admissionWorkerResult) error {
		if result.Status == workerStatusDriving && drivingObservations.Add(1) == 3 {
			return expected
		}
		return nil
	})
	if !errors.Is(err, expected) || !siblingExited.Load() {
		t.Fatalf("err=%v sibling_exited=%t", err, siblingExited.Load())
	}
}

func TestBoundedWorkerDispatchErrorDoesNotCancelHealthySibling(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	var siblingCanceled atomic.Bool
	expected := errors.New("run-scoped failure")
	done := make(chan error, 1)
	go func() {
		_, err := runBoundedAdmissionWorkerAtObserved(context.Background(), false, time.Minute, 2, func(ctx context.Context) (application.LinearTodoDispatchResult, error) {
			if calls.Add(1) == 1 {
				<-started
				return application.LinearTodoDispatchResult{}, expected
			}
			close(started)
			select {
			case <-ctx.Done():
				siblingCanceled.Store(true)
			case <-release:
			}
			return application.LinearTodoDispatchResult{Outcome: application.LinearTodoDispatchDriven}, nil
		}, waitAdmissionWorker, time.Now, nil)
		done <- err
	}()
	<-started
	select {
	case err := <-done:
		t.Fatalf("worker returned before sibling settled: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-done; !errors.Is(err, expected) || siblingCanceled.Load() {
		t.Fatalf("err=%v sibling_canceled=%t", err, siblingCanceled.Load())
	}
}

func TestBoundedWorkerDoesNotRescanIdleQueueWhileSiblingIsDriving(t *testing.T) {
	var calls atomic.Int32
	worker, err := newBoundedAdmissionWorker(2, func(context.Context) (application.LinearTodoDispatchResult, error) {
		calls.Add(1)
		return application.LinearTodoDispatchResult{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	worker.active = 1 // One long-running heavy sibling already occupies a slot.
	now := time.Now().UTC()
	worker.fillReady(context.Background(), now, now.Add(time.Minute))
	if calls.Load() != 0 || worker.active != 1 {
		t.Fatalf("calls=%d active=%d", calls.Load(), worker.active)
	}
}
