package sqlite

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

type operatorRetryRevalidator struct {
	run   application.Run
	err   error
	calls int
}

func (r *operatorRetryRevalidator) RevalidateForOperatorRetry(_ context.Context, _ application.LinearRevalidateCommand) (application.Run, error) {
	r.calls++
	return r.run, r.err
}

func TestOperatorRetryAtomicallyReschedulesObservesAndReplaysAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, run, _, _ := operatorActionFixture(t, path)
	requester := operatorActionInput(run, application.OperatorAttentionEvent{}, 0, application.OperatorActionRetry).Requester
	revalidator := &operatorRetryRevalidator{run: run}
	service, err := application.NewOperatorRetryService(store, revalidator)
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.Retry(context.Background(), application.OperatorRetryCommand{Requester: requester, RunID: run.ID})
	if err != nil || first.Action.Status != application.OperatorActionStatusObserved || first.Action.ResultStatus != application.OperatorActionResultSucceeded || first.Retry == nil || first.Retry.Status != application.RetryScheduleScheduled || first.Retry.AttemptCount != 4 || first.Retry.ReasonCode != application.RetryReasonOperatorRetry || !first.NextEligibleAt.After(first.Retry.UpdatedAt) {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if first.Action.ResultingState != run.State || first.Action.ResultingTransitionSequence < 1 || first.Action.EvidenceDigest == "" || first.Action.OutcomeDigest == "" {
		t.Fatalf("action evidence=%+v", first.Action)
	}
	replay, err := service.Retry(context.Background(), application.OperatorRetryCommand{Requester: requester, RunID: run.ID})
	if err != nil || replay.Action.ActionID != first.Action.ActionID || replay.Action.Status != application.OperatorActionStatusObserved || replay.Retry == nil || !replay.NextEligibleAt.Equal(first.NextEligibleAt) {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, _ = application.NewOperatorRetryService(store, revalidator)
	restarted, err := service.Retry(context.Background(), application.OperatorRetryCommand{Requester: requester, RunID: run.ID})
	if err != nil || restarted.Action.ActionID != first.Action.ActionID || restarted.Action.Status != application.OperatorActionStatusObserved {
		t.Fatalf("restarted=%+v err=%v", restarted, err)
	}
	if cleared, err := store.ClearRetrySchedule(context.Background(), run.ID, application.AutomaticRetryPhaseForRun(run), 4); err != nil || !cleared {
		t.Fatalf("cleared=%t err=%v", cleared, err)
	}
	consumed, err := service.Retry(context.Background(), application.OperatorRetryCommand{Requester: requester, RunID: run.ID})
	if err != nil || consumed.Action.ActionID != first.Action.ActionID || consumed.Retry != nil || !consumed.NextEligibleAt.Equal(first.NextEligibleAt) || !consumed.Action.NextEligibleAt.Equal(first.NextEligibleAt) {
		t.Fatalf("consumed replay=%+v err=%v", consumed, err)
	}
}

func TestOperatorRetryRecoversAppliedJournalAndNewFailureParksWithNewAttention(t *testing.T) {
	store, run, event, sequence := operatorActionFixture(t, filepath.Join(t.TempDir(), "controller.db"))
	defer store.Close()
	requester := operatorActionInput(run, event, sequence, application.OperatorActionRetry).Requester
	actions, _ := application.NewOperatorActionService(store)
	action, _, err := actions.Prepare(context.Background(), operatorActionInput(run, event, sequence, application.OperatorActionRetry))
	if err != nil {
		t.Fatal(err)
	}
	schedule, found, err := store.GetRetrySchedule(context.Background(), run.ID, application.AutomaticRetryPhaseForRun(run))
	if err != nil || !found {
		t.Fatalf("schedule=%+v found=%t err=%v", schedule, found, err)
	}
	applied, _, _, err := store.ApplyOperatorRetry(context.Background(), application.OperatorRetryApply{ActionID: action.ActionID, Phase: schedule.Phase, ExpectedAttempt: schedule.AttemptCount, NextEligibleAt: action.ValidatedAt.Add(time.Nanosecond), AppliedAt: action.ValidatedAt, EvidenceDigest: stringDigest('a')})
	if err != nil || applied.Status != application.OperatorActionStatusApplied {
		t.Fatalf("applied=%+v err=%v", applied, err)
	}
	service, _ := application.NewOperatorRetryService(store, &operatorRetryRevalidator{run: run})
	recovered, err := service.Retry(context.Background(), application.OperatorRetryCommand{Requester: requester, RunID: run.ID})
	if err != nil || recovered.Action.Status != application.OperatorActionStatusObserved || recovered.Action.ActionID != action.ActionID {
		t.Fatalf("recovered=%+v err=%v", recovered, err)
	}
	failedAt := recovered.NextEligibleAt.Add(time.Second)
	parked, changed, err := store.ApplyRetryFailure(context.Background(), application.RetryFailureRequest{RunID: run.ID, Phase: schedule.Phase, ControllerState: run.State, ExpectedAttempt: schedule.AttemptCount, FailureClass: application.RetryFailureUnavailable, ReasonCode: application.RetryReasonUnavailable, Now: failedAt})
	if err != nil || !changed || parked.Status != application.RetryScheduleAttention || parked.AttemptCount != 5 || parked.ReasonCode != application.RetryReasonBudgetExhausted {
		t.Fatalf("parked=%+v changed=%t err=%v", parked, changed, err)
	}
	newEvent, err := application.AutomaticRetryAttentionEvent(run, parked)
	if err != nil || newEvent.EventKey == event.EventKey {
		t.Fatalf("new event=%+v err=%v", newEvent, err)
	}
}

func TestOperatorRetryRejectsRevalidationDriftWithoutMutation(t *testing.T) {
	store, run, _, _ := operatorActionFixture(t, filepath.Join(t.TempDir(), "controller.db"))
	defer store.Close()
	drifted := run
	drifted.SourceRevision = "changed"
	revalidator := &operatorRetryRevalidator{run: drifted}
	service, _ := application.NewOperatorRetryService(store, revalidator)
	requester := operatorActionInput(run, application.OperatorAttentionEvent{}, 0, application.OperatorActionRetry).Requester
	if _, err := service.Retry(context.Background(), application.OperatorRetryCommand{Requester: requester, RunID: run.ID}); err == nil {
		t.Fatal("revalidation drift authorized retry")
	}
	inspection, err := store.Inspect(context.Background(), run.ID)
	if err != nil || len(inspection.OperatorActions) != 0 || len(inspection.RetrySchedules) != 1 || inspection.RetrySchedules[0].Status != application.RetryScheduleAttention {
		t.Fatalf("inspection=%+v err=%v", inspection, err)
	}
}

func TestOperatorRetryConcurrentIdenticalCommandsShareOneAction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	firstStore, run, _, _ := operatorActionFixture(t, path)
	defer firstStore.Close()
	secondStore, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer secondStore.Close()
	requester := operatorActionInput(run, application.OperatorAttentionEvent{}, 0, application.OperatorActionRetry).Requester
	type outcome struct {
		result application.OperatorRetryResult
		err    error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for _, store := range []*Store{firstStore, secondStore} {
		go func(store *Store) {
			service, _ := application.NewOperatorRetryService(store, &operatorRetryRevalidator{run: run})
			ready.Done()
			<-start
			result, retryErr := service.Retry(context.Background(), application.OperatorRetryCommand{Requester: requester, RunID: run.ID})
			results <- outcome{result: result, err: retryErr}
		}(store)
	}
	ready.Wait()
	close(start)
	one, two := <-results, <-results
	if one.err != nil || two.err != nil || one.result.Action.ActionID == "" || one.result.Action.ActionID != two.result.Action.ActionID || one.result.Action.Status != application.OperatorActionStatusObserved || two.result.Action.Status != application.OperatorActionStatusObserved {
		t.Fatalf("one=%+v two=%+v", one, two)
	}
	inspection, err := firstStore.Inspect(context.Background(), run.ID)
	if err != nil || len(inspection.OperatorActions) != 1 || len(inspection.RetrySchedules) != 1 || inspection.RetrySchedules[0].Status != application.RetryScheduleScheduled {
		t.Fatalf("inspection=%+v err=%v", inspection, err)
	}
}

func stringDigest(value byte) string {
	result := make([]byte, 64)
	for index := range result {
		result[index] = value
	}
	return string(result)
}
