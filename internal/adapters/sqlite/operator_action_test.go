package sqlite

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestOperatorActionJournalBindsAuthorityReplaysAndSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, run, event, sequence := operatorActionFixture(t, path)
	service, err := application.NewOperatorActionService(store)
	if err != nil {
		t.Fatal(err)
	}
	input := operatorActionInput(run, event, sequence, application.OperatorActionAbandon)
	first, created, err := service.Prepare(context.Background(), input)
	if err != nil || !created || first.Status != application.OperatorActionStatusValidated || first.ResultStatus != application.OperatorActionResultPending {
		t.Fatalf("first=%+v created=%t err=%v", first, created, err)
	}
	replay, created, err := service.Prepare(context.Background(), input)
	if err != nil || created || replay.ActionID != first.ActionID || replay.PayloadDigest != first.PayloadDigest {
		t.Fatalf("replay=%+v created=%t err=%v", replay, created, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, _ = application.NewOperatorActionService(store)
	restarted, created, err := service.Prepare(context.Background(), input)
	if err != nil || created || restarted.ActionID != first.ActionID || restarted.Status != application.OperatorActionStatusValidated {
		t.Fatalf("restarted=%+v created=%t err=%v", restarted, created, err)
	}

	evidence := strings.Repeat("a", 64)
	appliedResult := application.OperatorActionMutationResult{ActionID: first.ActionID, ExpectedStatus: application.OperatorActionStatusValidated, ResultStatus: application.OperatorActionResultApplied, ResultingState: run.State, ResultingTransitionSequence: sequence, EvidenceDigest: evidence, At: time.Now().UTC()}
	applied, changed, err := service.RecordApplied(context.Background(), appliedResult)
	if err != nil || !changed || applied.Status != application.OperatorActionStatusApplied {
		t.Fatalf("applied=%+v changed=%t err=%v", applied, changed, err)
	}
	if replay, changed, err := service.RecordApplied(context.Background(), appliedResult); err != nil || changed || replay.Status != application.OperatorActionStatusApplied {
		t.Fatalf("applied replay=%+v changed=%t err=%v", replay, changed, err)
	}
	if err := store.Transition(context.Background(), run.ID, run.State, domain.StateAdmitting, "fixture progressed before observation", "fixture", run.CandidateHead); err != nil {
		t.Fatal(err)
	}
	outcome := strings.Repeat("b", 64)
	observedResult := application.OperatorActionMutationResult{ActionID: first.ActionID, ExpectedStatus: application.OperatorActionStatusApplied, ResultStatus: application.OperatorActionResultSucceeded, ResultingState: run.State, ResultingTransitionSequence: sequence, EvidenceDigest: outcome, At: time.Now().UTC()}
	observed, changed, err := service.RecordObserved(context.Background(), observedResult)
	if err != nil || !changed || observed.Status != application.OperatorActionStatusObserved || observed.ResultStatus != application.OperatorActionResultSucceeded || observed.EvidenceDigest != evidence || observed.OutcomeDigest != outcome || observed.ResultingState != run.State || observed.ResultingTransitionSequence != sequence {
		t.Fatalf("observed=%+v changed=%t err=%v", observed, changed, err)
	}
	if replay, changed, err := service.RecordObserved(context.Background(), observedResult); err != nil || changed || replay.Status != application.OperatorActionStatusObserved {
		t.Fatalf("observed replay=%+v changed=%t err=%v", replay, changed, err)
	}
	inspection, err := application.NewQueryService(store).Inspect(context.Background(), application.QueryInput{Requester: input.Requester, RunID: run.ID, Repository: run.Repository})
	if err != nil || len(inspection.OperatorActions) != 1 || inspection.OperatorActions[0].ActionID != first.ActionID {
		t.Fatalf("inspection=%+v err=%v", inspection.OperatorActions, err)
	}
	raw, _ := json.Marshal(inspection.OperatorActions[0])
	if strings.Contains(string(raw), run.IdempotencyKey) || strings.Contains(string(raw), first.IdempotencyKey) || !strings.Contains(string(raw), `"action_type":"abandon"`) {
		t.Fatalf("unsafe projection=%s", raw)
	}
}

func TestOperatorActionJournalRejectsUnadvertisedAndDriftedAuthority(t *testing.T) {
	store, run, event, sequence := operatorActionFixture(t, filepath.Join(t.TempDir(), "controller.db"))
	defer store.Close()
	service, _ := application.NewOperatorActionService(store)
	input := operatorActionInput(run, event, sequence, application.OperatorActionRetry)
	for _, mutate := range []func(*application.OperatorActionInput){
		func(value *application.OperatorActionInput) { value.Repository = "other/repo" },
		func(value *application.OperatorActionInput) { value.TransitionSequence++ },
		func(value *application.OperatorActionInput) { value.ReasonCode = "different_reason" },
		func(value *application.OperatorActionInput) { value.AttentionEventKey = "different-event" },
		func(value *application.OperatorActionInput) {
			value.ActionType = application.OperatorActionType("decide")
		},
		func(value *application.OperatorActionInput) { value.Requester.DatabaseID = 0 },
	} {
		changed := input
		mutate(&changed)
		if _, _, err := service.Prepare(context.Background(), changed); err == nil {
			t.Fatalf("drifted input accepted: %+v", changed)
		}
	}
	inspection, err := store.Inspect(context.Background(), run.ID)
	if err != nil || len(inspection.OperatorActions) != 0 {
		t.Fatalf("invalid actions were persisted: %+v err=%v", inspection.OperatorActions, err)
	}
}

func TestOperatorActionJournalRejectsHistoricalAttentionAndAcceptsCurrentBeyondProjectionLimit(t *testing.T) {
	store, run, oldEvent, sequence := operatorActionFixture(t, filepath.Join(t.TempDir(), "controller.db"))
	defer store.Close()
	var current application.OperatorAttentionEvent
	for attempt := 5; attempt <= 105; attempt++ {
		at := time.Date(2026, 7, 16, 13, 0, attempt, 0, time.UTC)
		schedule := application.RetrySchedule{RunID: run.ID, Phase: application.AutomaticRetryPhaseForRun(run), ControllerState: string(run.State), AttemptCount: attempt, MaxAttempts: 3, InitialDelay: time.Second, MaximumDelay: 30 * time.Second, FailureClass: application.RetryFailureProcessStart, FailureEvidenceRef: "attempt:1", ReasonCode: application.RetryReasonBudgetExhausted, Status: application.RetryScheduleAttention, AttentionAt: at, CreatedAt: at.Add(-time.Minute), UpdatedAt: at}
		event, err := application.AutomaticRetryAttentionEvent(run, schedule)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.AppendOperatorAttention(context.Background(), event); err != nil {
			t.Fatal(err)
		}
		current = event
	}
	service, _ := application.NewOperatorActionService(store)
	if _, _, err := service.Prepare(context.Background(), operatorActionInput(run, oldEvent, sequence, application.OperatorActionRetry)); err == nil {
		t.Fatal("historical attention authorized an operator action")
	}
	if record, created, err := service.Prepare(context.Background(), operatorActionInput(run, current, sequence, application.OperatorActionRetry)); err != nil || !created || record.AttentionEventKey != current.EventKey {
		t.Fatalf("record=%+v created=%t err=%v", record, created, err)
	}
}

func TestOperatorActionConcurrentSamePayloadIsOneCreateAndOneReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	firstStore, run, event, sequence := operatorActionFixture(t, path)
	defer firstStore.Close()
	secondStore, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer secondStore.Close()
	input := operatorActionInput(run, event, sequence, application.OperatorActionRetry)
	type outcome struct {
		record  application.OperatorActionRecord
		created bool
		err     error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	for _, store := range []*Store{firstStore, secondStore} {
		go func(store *Store) {
			service, _ := application.NewOperatorActionService(store)
			<-start
			record, created, err := service.Prepare(context.Background(), input)
			results <- outcome{record: record, created: created, err: err}
		}(store)
	}
	close(start)
	one, two := <-results, <-results
	if one.err != nil || two.err != nil || one.record.ActionID == "" || one.record.ActionID != two.record.ActionID || one.created == two.created {
		t.Fatalf("one=%+v two=%+v", one, two)
	}
}

func TestOperatorActionConcurrentContradictoryAnswersFailClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	firstStore, run, event, sequence := operatorActionFixture(t, path)
	defer firstStore.Close()
	secondStore, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer secondStore.Close()
	type outcome struct {
		created bool
		err     error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	for index, store := range []*Store{firstStore, secondStore} {
		action := application.OperatorActionRetry
		if index == 1 {
			action = application.OperatorActionAbandon
		}
		go func(store *Store, action application.OperatorActionType) {
			service, _ := application.NewOperatorActionService(store)
			<-start
			_, created, err := service.Prepare(context.Background(), operatorActionInput(run, event, sequence, action))
			results <- outcome{created: created, err: err}
		}(store, action)
	}
	close(start)
	one, two := <-results, <-results
	if (one.err == nil) == (two.err == nil) || one.created == two.created {
		t.Fatalf("one=%+v two=%+v", one, two)
	}
	inspection, err := firstStore.Inspect(context.Background(), run.ID)
	if err != nil || len(inspection.OperatorActions) != 1 {
		t.Fatalf("actions=%+v err=%v", inspection.OperatorActions, err)
	}
}

func TestOperatorActionIntentReplayReturnsPersistedResultAfterRunAdvances(t *testing.T) {
	store, run, event, sequence := operatorActionFixture(t, filepath.Join(t.TempDir(), "controller.db"))
	defer store.Close()
	service, _ := application.NewOperatorActionService(store)
	input := operatorActionInput(run, event, sequence, application.OperatorActionRetry)
	first, created, err := service.Prepare(context.Background(), input)
	if err != nil || !created {
		t.Fatalf("first=%+v created=%t err=%v", first, created, err)
	}
	if err := store.Transition(context.Background(), run.ID, run.State, domain.StateAdmitting, "fixture advanced after action", "fixture", run.CandidateHead); err != nil {
		t.Fatal(err)
	}
	replay, created, err := service.Prepare(context.Background(), input)
	if err != nil || created || replay.ActionID != first.ActionID || replay.Status != application.OperatorActionStatusValidated {
		t.Fatalf("replay=%+v created=%t err=%v", replay, created, err)
	}
}

func TestOperatorActionMigrationFromV23CreatesEmptyJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE operator_actions`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`ALTER TABLE automatic_retry_schedules DROP COLUMN failure_evidence_ref`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`ALTER TABLE attempts DROP COLUMN process_control_key`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DELETE FROM schema_migrations WHERE version IN (24,25,26,27)`); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if version, err := store.SchemaVersion(context.Background()); err != nil || version != schemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM operator_actions`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func operatorActionFixture(t *testing.T, path string) (*Store, application.Run, application.OperatorAttentionEvent, int64) {
	t.Helper()
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	run := outboxRun(t, "run-operator-action")
	run.State = domain.StateExecuting
	if _, _, err := store.CreateRun(context.Background(), application.CreateRunInput{Run: run}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	inspection, err := store.Inspect(context.Background(), run.ID)
	if err != nil || len(inspection.Timeline) == 0 {
		store.Close()
		t.Fatalf("timeline=%+v err=%v", inspection.Timeline, err)
	}
	run = inspection.Run
	sequence := inspection.Timeline[len(inspection.Timeline)-1].Sequence
	now := time.Date(2026, 7, 16, 13, 0, 0, 0, time.UTC)
	var schedule application.RetrySchedule
	for attempt := 0; attempt < 4; attempt++ {
		schedule, _, err = store.ApplyRetryFailure(context.Background(), application.RetryFailureRequest{RunID: run.ID, Phase: application.AutomaticRetryPhaseForRun(run), ControllerState: run.State, ExpectedAttempt: attempt, FailureClass: application.RetryFailureUnavailable, ReasonCode: application.RetryReasonUnavailable, Now: now.Add(time.Duration(attempt) * time.Second), Policy: application.AutomaticRetryPolicy{MaxAttempts: 3, InitialDelay: time.Second, MaximumDelay: 30 * time.Second}})
		if err != nil {
			store.Close()
			t.Fatal(err)
		}
	}
	event, err := application.AutomaticRetryAttentionEvent(run, schedule)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if _, err := store.AppendOperatorAttention(context.Background(), event); err != nil {
		store.Close()
		t.Fatal(err)
	}
	return store, run, event, sequence
}

func operatorActionInput(run application.Run, event application.OperatorAttentionEvent, sequence int64, action application.OperatorActionType) application.OperatorActionInput {
	return application.OperatorActionInput{Requester: application.Requester{ID: "operator", Kind: "github_login", DatabaseID: 33, NodeID: "MDQ6VXNlcjMz", ActorType: "User"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, RunIdempotencyKey: run.IdempotencyKey, TransitionSequence: sequence, ActionType: action, ReasonCode: event.ReasonCode, AttentionEventKey: event.EventKey}
}
