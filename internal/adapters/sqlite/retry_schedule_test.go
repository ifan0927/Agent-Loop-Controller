package sqlite

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestRetryScheduleSurvivesRestartAndBoundsAttention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	run := outboxRun(t, "run-retry-restart")
	if _, _, err := store.CreateRun(context.Background(), application.CreateRunInput{Run: run}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	policy := application.AutomaticRetryPolicy{MaxAttempts: 2, InitialDelay: 10 * time.Second, MaximumDelay: 20 * time.Second}
	now := time.Date(2026, 7, 15, 4, 0, 0, 0, time.UTC)
	request := application.RetryFailureRequest{RunID: run.ID, Phase: "state_received", ControllerState: run.State, ExpectedAttempt: 0, FailureClass: application.RetryFailureProcessStart, ReasonCode: application.RetryReasonProcessStart, Now: now, Policy: policy}
	first, applied, err := store.ApplyRetryFailure(context.Background(), request)
	if err != nil || !applied || first.AttemptCount != 1 || first.NextEligibleAt != now.Add(10*time.Second) || first.Status != application.RetryScheduleScheduled {
		t.Fatalf("first=%+v applied=%v err=%v", first, applied, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	resumed, found, err := store.GetRetrySchedule(context.Background(), run.ID, "state_received")
	if err != nil || !found || !resumed.NextEligibleAt.Equal(first.NextEligibleAt) || resumed.AttemptCount != 1 {
		t.Fatalf("resumed=%+v found=%v err=%v", resumed, found, err)
	}
	stale, applied, err := store.ApplyRetryFailure(context.Background(), request)
	if err != nil || applied || stale.AttemptCount != 1 || !stale.NextEligibleAt.Equal(first.NextEligibleAt) {
		t.Fatalf("stale=%+v applied=%v err=%v", stale, applied, err)
	}

	secondRequest := request
	secondRequest.ExpectedAttempt, secondRequest.Now = 1, first.NextEligibleAt
	secondRequest.Policy = application.AutomaticRetryPolicy{MaxAttempts: 10, InitialDelay: time.Minute, MaximumDelay: 2 * time.Minute}
	second, applied, err := store.ApplyRetryFailure(context.Background(), secondRequest)
	if err != nil || !applied || second.AttemptCount != 2 || second.NextEligibleAt != first.NextEligibleAt.Add(20*time.Second) {
		t.Fatalf("second=%+v applied=%v err=%v", second, applied, err)
	}
	thirdRequest := request
	thirdRequest.ExpectedAttempt, thirdRequest.Now = 2, second.NextEligibleAt
	attention, applied, err := store.ApplyRetryFailure(context.Background(), thirdRequest)
	if err != nil || !applied || attention.Status != application.RetryScheduleAttention || attention.AttemptCount != 3 || attention.ReasonCode != application.RetryReasonBudgetExhausted {
		t.Fatalf("attention=%+v applied=%v err=%v", attention, applied, err)
	}
	if _, found, err := store.GetRetrySchedule(context.Background(), run.ID, "state_received"); err != nil || !found {
		t.Fatalf("attention schedule was lost found=%v err=%v", found, err)
	}
	event, err := application.AutomaticRetryAttentionEvent(run, attention)
	if err != nil {
		t.Fatal(err)
	}
	if created, err := store.AppendOperatorAttention(context.Background(), event); err != nil || !created {
		t.Fatalf("attention append created=%v err=%v", created, err)
	}
	if created, err := store.AppendOperatorAttention(context.Background(), event); err != nil || created {
		t.Fatalf("attention replay created=%v err=%v", created, err)
	}
}

func TestRetryScheduleCASAllowsOneConcurrentFailureAndNoSecondRunAuthority(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := outboxRun(t, "run-retry-race")
	if _, _, err := store.CreateRun(context.Background(), application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	request := application.RetryFailureRequest{RunID: run.ID, Phase: "state_received", ControllerState: run.State, FailureClass: application.RetryFailureUnavailable, ReasonCode: application.RetryReasonUnavailable, Now: time.Date(2026, 7, 15, 5, 0, 0, 0, time.UTC), Policy: application.DefaultAutomaticRetryPolicy()}
	var wg sync.WaitGroup
	results := make(chan bool, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, applied, applyErr := store.ApplyRetryFailure(context.Background(), request)
			results <- applied
			errs <- applyErr
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	appliedCount := 0
	for applied := range results {
		if applied {
			appliedCount++
		}
	}
	for applyErr := range errs {
		if applyErr != nil {
			t.Fatal(applyErr)
		}
	}
	if appliedCount != 1 {
		t.Fatalf("applied count=%d", appliedCount)
	}
	schedules, err := store.ListRetrySchedules(context.Background())
	if err != nil || len(schedules) != 1 || schedules[0].RunID != run.ID || schedules[0].Phase != "state_received" {
		t.Fatalf("schedules=%+v err=%v", schedules, err)
	}
	if schedules[0].AttemptCount != 1 {
		t.Fatalf("attempt count=%d", schedules[0].AttemptCount)
	}
}

func TestRetryScheduleStateDriftBecomesAuthorityAttention(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := outboxRun(t, "run-retry-state-drift")
	if _, _, err := store.CreateRun(context.Background(), application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(context.Background(), run.ID, run.State, domain.StateAdmitting, "fixture state drift", "fixture", ""); err != nil {
		t.Fatal(err)
	}
	schedule, applied, err := store.ApplyRetryFailure(context.Background(), application.RetryFailureRequest{
		RunID: run.ID, Phase: "state_received", ControllerState: run.State, ExpectedAttempt: 0,
		FailureClass: application.RetryFailureUnavailable, ReasonCode: application.RetryReasonUnavailable,
		Now: time.Date(2026, 7, 15, 6, 0, 0, 0, time.UTC), Policy: application.DefaultAutomaticRetryPolicy(),
	})
	if err != nil || !applied || schedule.Status != application.RetryScheduleAttention || schedule.ReasonCode != application.RetryReasonAuthority || schedule.ControllerState != "admitting" {
		t.Fatalf("schedule=%+v applied=%v err=%v", schedule, applied, err)
	}
	if application.RetryFailureIsRetryable(schedule.FailureClass) {
		t.Fatalf("state drift remained retryable: %+v", schedule)
	}
}
