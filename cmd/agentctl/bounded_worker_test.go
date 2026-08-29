package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sqlitestore "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
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

func TestBoundedWorkerPublishesIntegrityOnlyAfterConcurrentBatchIsQuiescent(t *testing.T) {
	store, err := sqlitestore.Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	for index := 0; index < 32; index++ {
		if _, err := store.BackfillActivityBatch(ctx, 25, now.Add(time.Duration(index)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.RunIntegrityMaintenance(ctx, "fixture-preparation", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= 7; index++ {
		identity := application.ConfigurationEvidenceDigest("quiescence-preparation", string(rune('a'+index)))
		if _, err := store.ConfigureHeavyCapacity(ctx, 1+index%2, identity, now.Add(time.Duration(index)*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.RunIntegrityMaintenance(ctx, "fixture-preparation", now.Add(time.Duration(index+1)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	var dispatches atomic.Int32
	var cleanups atomic.Int32
	maintenanceEntered := make(chan struct{})
	releaseMaintenance := make(chan struct{})
	maintenanceFinished := make(chan struct{})
	maintenanceErrors := make(chan error, 1)
	done := make(chan error, 1)
	go func() {
		_, runErr := runBoundedAdmissionWorkerAtObservedWithMaintenance(ctx, false, time.Minute, 2, func(dispatchCtx context.Context) (application.LinearTodoDispatchResult, error) {
			index := dispatches.Add(1)
			defer cleanups.Add(1)
			identity := application.ConfigurationEvidenceDigest("quiescent-dispatch", string(rune('a'+index)))
			if _, err := store.ConfigureHeavyCapacity(dispatchCtx, 1+int(index)%2, identity, now.Add(time.Duration(index+10)*time.Minute)); err != nil {
				return application.LinearTodoDispatchResult{}, err
			}
			return application.LinearTodoDispatchResult{Outcome: application.LinearTodoDispatchDriven}, nil
		}, waitAdmissionWorker, time.Now, nil, func(maintenanceCtx context.Context) {
			close(maintenanceEntered)
			defer close(maintenanceFinished)
			<-releaseMaintenance
			if cleanups.Load() != 2 {
				maintenanceErrors <- errors.New("maintenance ran before deferred dispatch cleanup")
				cancel()
				return
			}
			published, err := store.RunIntegrityMaintenance(maintenanceCtx, "automatic-worker", now.Add(20*time.Minute))
			if err != nil || !published.Published || !published.Superseded {
				maintenanceErrors <- errors.New("quiescent full-family publication failed")
				cancel()
				return
			}
			current, err := store.RunIntegrityMaintenance(maintenanceCtx, "fixture-probe", now.Add(21*time.Minute))
			if err != nil || current.Published || current.Superseded || current.TargetGeneration != published.TargetGeneration {
				maintenanceErrors <- errors.New("publication was not current at the quiescent boundary")
				cancel()
				return
			}
			maintenanceErrors <- nil
			cancel()
		})
		done <- runErr
	}()
	select {
	case <-maintenanceEntered:
	case <-time.After(time.Second):
		t.Fatal("maintenance did not reach the quiescent boundary")
	}
	if dispatches.Load() != 2 || cleanups.Load() != 2 {
		t.Fatalf("dispatches=%d cleanups=%d", dispatches.Load(), cleanups.Load())
	}
	time.Sleep(20 * time.Millisecond)
	if dispatches.Load() != 2 {
		t.Fatalf("new dispatch admitted during maintenance: %d", dispatches.Load())
	}
	close(releaseMaintenance)
	select {
	case <-maintenanceFinished:
	case <-time.After(time.Second):
		t.Fatal("maintenance did not finish")
	}
	if err := <-maintenanceErrors; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestBoundedWorkerDoesNotMaintainAfterDispatchError(t *testing.T) {
	expected := errors.New("dispatch failed")
	started := make(chan struct{})
	release := make(chan struct{})
	var maintained atomic.Bool
	var calls atomic.Int32
	done := make(chan error, 1)
	go func() {
		_, err := runBoundedAdmissionWorkerAtObservedWithMaintenance(context.Background(), false, time.Minute, 2, func(context.Context) (application.LinearTodoDispatchResult, error) {
			if calls.Add(1) == 1 {
				close(started)
				return application.LinearTodoDispatchResult{}, expected
			} else {
				<-release
				return application.LinearTodoDispatchResult{Outcome: application.LinearTodoDispatchDriven}, nil
			}
		}, waitAdmissionWorker, time.Now, nil, func(context.Context) { maintained.Store(true) })
		done <- err
	}()
	<-started
	close(release)
	if err := <-done; !errors.Is(err, expected) || maintained.Load() {
		t.Fatalf("err=%v maintained=%t", err, maintained.Load())
	}
}

func TestBoundedWorkerOnceMaintainsAfterDeferredCleanup(t *testing.T) {
	var cleaned atomic.Bool
	var maintained atomic.Bool
	result, err := runBoundedAdmissionWorkerAtObservedWithMaintenance(context.Background(), true, time.Minute, 1, func(context.Context) (application.LinearTodoDispatchResult, error) {
		defer cleaned.Store(true)
		return application.LinearTodoDispatchResult{Outcome: application.LinearTodoDispatchNoCandidate}, nil
	}, waitAdmissionWorker, time.Now, nil, func(context.Context) {
		if !cleaned.Load() {
			t.Error("maintenance ran before dispatch cleanup")
		}
		maintained.Store(true)
	})
	if err != nil || result.Stopped != "once" || !maintained.Load() {
		t.Fatalf("result=%+v maintained=%t err=%v", result, maintained.Load(), err)
	}
}

func TestBoundedWorkerOnceRejectsSiblingErrorBeforeMaintenance(t *testing.T) {
	expected := errors.New("sibling failed")
	secondStarted := make(chan struct{})
	releaseError := make(chan struct{})
	var calls atomic.Int32
	var maintained atomic.Bool
	done := make(chan error, 1)
	go func() {
		_, err := runBoundedAdmissionWorkerAtObservedWithMaintenance(context.Background(), true, time.Minute, 2, func(context.Context) (application.LinearTodoDispatchResult, error) {
			if calls.Add(1) == 1 {
				<-secondStarted
				return application.LinearTodoDispatchResult{Outcome: application.LinearTodoDispatchDriven}, nil
			}
			close(secondStarted)
			<-releaseError
			return application.LinearTodoDispatchResult{}, expected
		}, waitAdmissionWorker, time.Now, nil, func(context.Context) { maintained.Store(true) })
		done <- err
	}()
	<-secondStarted
	close(releaseError)
	if err := <-done; !errors.Is(err, expected) || maintained.Load() {
		t.Fatalf("err=%v maintained=%t", err, maintained.Load())
	}
}

func TestBoundedWorkerCancellationJoinsSiblingsWithoutMaintenance(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{}, 2)
	var exited atomic.Int32
	var maintained atomic.Bool
	done := make(chan error, 1)
	go func() {
		_, err := runBoundedAdmissionWorkerAtObservedWithMaintenance(ctx, false, time.Minute, 2, func(dispatchCtx context.Context) (application.LinearTodoDispatchResult, error) {
			entered <- struct{}{}
			<-dispatchCtx.Done()
			exited.Add(1)
			return application.LinearTodoDispatchResult{}, dispatchCtx.Err()
		}, waitAdmissionWorker, time.Now, nil, func(context.Context) { maintained.Store(true) })
		done <- err
	}()
	<-entered
	<-entered
	cancel()
	if err := <-done; err != nil || exited.Load() != 2 || maintained.Load() {
		t.Fatalf("err=%v exited=%d maintained=%t", err, exited.Load(), maintained.Load())
	}
}
