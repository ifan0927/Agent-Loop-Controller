package application

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

const (
	startTeamID  = "123e4567-e89b-42d3-a456-426614174100"
	startTodoID  = "123e4567-e89b-42d3-a456-426614174101"
	startStateID = "123e4567-e89b-42d3-a456-426614174102"
	startIssueID = "123e4567-e89b-42d3-a456-426614174103"
)

type linearStartReader struct {
	sources []LinearTaskSource
	calls   int
}

func (r *linearStartReader) ReadIssue(_ context.Context, _ string) (LinearTaskSource, []LinearRequestObservation, error) {
	if r.calls >= len(r.sources) {
		return LinearTaskSource{}, []LinearRequestObservation{{Operation: "read_issue", ObservedAt: time.Now().UTC()}}, errLinearStartFixture
	}
	source := r.sources[r.calls]
	r.calls++
	return source, []LinearRequestObservation{{Operation: "read_issue", HTTPStatus: 200, ResponseDigest: "digest", ObservedAt: time.Now().UTC()}}, nil
}

type linearStartMover struct {
	results []LinearIssueStartMutationResult
	errors  []error
	calls   []LinearIssueStartMutation
}

func (m *linearStartMover) MoveReservedIssueToStarted(_ context.Context, mutation LinearIssueStartMutation) (LinearIssueStartMutationResult, []LinearRequestObservation, error) {
	i := len(m.calls)
	m.calls = append(m.calls, mutation)
	return m.results[i], []LinearRequestObservation{{Operation: "move_reserved_issue_to_started", HTTPStatus: 200, ResponseDigest: "mutation", ObservedAt: time.Now().UTC()}}, m.errors[i]
}

type linearStartStore struct {
	RunStore
	run          Run
	effects      map[string]SideEffectRecord
	observations []LinearRequestObservation
	transitions  []Transition
	lastError    string
	retryDenied  bool
}

func (s *linearStartStore) GetRun(context.Context, string) (Run, error) { return s.run, nil }
func (s *linearStartStore) SaveLinearRequestObservation(_ context.Context, _ string, record LinearRequestObservation) error {
	s.observations = append(s.observations, record)
	return nil
}
func (s *linearStartStore) BeginSideEffect(_ context.Context, record SideEffectRecord) (SideEffectRecord, bool, error) {
	if existing, found := s.effects[record.IdempotencyKey]; found {
		if existing.IntentJSON != record.IntentJSON || existing.Kind != record.Kind {
			return existing, false, errLinearStartFixture
		}
		return existing, false, nil
	}
	record.ID, record.Status = int64(len(s.effects)+1), "intent"
	s.effects[record.IdempotencyKey] = record
	return record, true, nil
}
func (s *linearStartStore) FinishLinearIssueStartSideEffect(_ context.Context, record SideEffectRecord, expectedStatus string, expectedAttempt int) error {
	stored := s.effects[record.IdempotencyKey]
	if stored.ID != record.ID || stored.Status != expectedStatus || stored.Attempt != expectedAttempt {
		return errLinearStartFixture
	}
	record.ClaimedAt = time.Time{}
	s.effects[record.IdempotencyKey] = record
	return nil
}
func (s *linearStartStore) RetryLinearIssueStartSideEffect(_ context.Context, record SideEffectRecord) (SideEffectRecord, bool, error) {
	stored := s.effects[record.IdempotencyKey]
	if s.retryDenied {
		return stored, false, nil
	}
	if stored.ID != record.ID || stored.Status != "failed" || stored.Attempt != 1 {
		return stored, false, nil
	}
	stored.Status, stored.ResultJSON, stored.Attempt = "intent", "", 2
	s.effects[record.IdempotencyKey] = stored
	return stored, true, nil
}
func (s *linearStartStore) ClaimLinearIssueStartSideEffect(_ context.Context, record SideEffectRecord, claimedAt time.Time) (SideEffectRecord, bool, error) {
	stored := s.effects[record.IdempotencyKey]
	if stored.ID != record.ID || stored.Status != "intent" || stored.Attempt != record.Attempt {
		return stored, false, nil
	}
	stored.Status, stored.ClaimedAt = "in_flight", claimedAt
	s.effects[record.IdempotencyKey] = stored
	return stored, true, nil
}
func (s *linearStartStore) SetLastError(_ context.Context, _ string, message string) error {
	s.lastError = message
	return nil
}
func (s *linearStartStore) Transition(_ context.Context, _ string, from, to domain.State, reason, evidence, _ string) error {
	if s.run.State != from {
		return errLinearStartFixture
	}
	s.run.State = to
	s.transitions = append(s.transitions, Transition{From: from, To: to, Reason: reason, EvidenceReference: evidence})
	return nil
}

var errLinearStartFixture = &LinearIssueStartMutationError{Class: "fixture"}

func TestMoveReservedIssueToStartedPersistsIntentThenObservesExactStartedState(t *testing.T) {
	todo := validStartSource()
	started := todo
	started.State = startAuthority().InProgressState
	started.UpdatedAt = todo.UpdatedAt.Add(time.Second)
	started.SourceRevision = started.UpdatedAt.Format(time.RFC3339Nano)
	store, service := newLinearStartService(t, []LinearTaskSource{todo, started, started}, []LinearIssueStartMutationResult{{IssueID: startIssueID, State: startAuthority().InProgressState}}, []error{nil})
	result, err := service.MoveReservedIssueToStarted(context.Background(), MoveReservedIssueToStartedCommand{RunID: store.run.ID})
	if err != nil || result.Status != "started" || len(store.transitions) != 0 || len(store.effects) != 1 {
		t.Fatalf("result=%+v err=%v transitions=%+v effects=%+v", result, err, store.transitions, store.effects)
	}
	for _, effect := range store.effects {
		if effect.Kind != linearIssueStartEffectKind || effect.Status != "observed" || effect.Attempt != 1 || strings.Contains(effect.IntentJSON, "Freeze one trusted") || strings.Contains(effect.IntentJSON, "secret") {
			t.Fatalf("unsafe or incomplete persisted effect: %+v", effect)
		}
		var intent linearIssueStartIntentRecord
		if json.Unmarshal([]byte(effect.IntentJSON), &intent) != nil || intent.IssueID != startIssueID || intent.SourceStateID != startTodoID || intent.TargetStateID != startStateID || intent.TaskHash != store.run.TaskHash || intent.RunID != store.run.ID || intent.IdempotencyDigest != effect.IdempotencyKey {
			t.Fatalf("unexpected intent: %+v effect=%+v", intent, effect)
		}
	}
	if repeated, repeatErr := service.MoveReservedIssueToStarted(context.Background(), MoveReservedIssueToStartedCommand{RunID: store.run.ID}); repeatErr != nil || repeated.Status != "started" {
		t.Fatalf("restart after remote success result=%+v err=%v", repeated, repeatErr)
	}
}

func TestMoveReservedIssueToStartedReconcilesAmbiguousResponseWithOneDurableRetry(t *testing.T) {
	todo := validStartSource()
	started := todo
	started.State = startAuthority().InProgressState
	started.UpdatedAt = todo.UpdatedAt.Add(time.Second)
	started.SourceRevision = started.UpdatedAt.Format(time.RFC3339Nano)
	store, service := newLinearStartService(t, []LinearTaskSource{todo, todo, started}, []LinearIssueStartMutationResult{{}, {IssueID: startIssueID, State: startAuthority().InProgressState}}, []error{&LinearIssueStartMutationError{Class: "transport", Ambiguous: true}, nil})
	result, err := service.MoveReservedIssueToStarted(context.Background(), MoveReservedIssueToStartedCommand{RunID: store.run.ID})
	if err != nil || result.Status != "started" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, effect := range store.effects {
		if effect.Attempt != 2 || effect.Status != "observed" {
			t.Fatalf("retry was not durably bounded: %+v", effect)
		}
	}
}

func TestMoveReservedIssueToStartedRecoversPersistedIntentBeforeWriteOrResult(t *testing.T) {
	t.Run("before write", func(t *testing.T) {
		todo := validStartSource()
		started := todo
		started.State = startAuthority().InProgressState
		started.UpdatedAt = todo.UpdatedAt.Add(time.Second)
		started.SourceRevision = started.UpdatedAt.Format(time.RFC3339Nano)
		store, service := newLinearStartService(t, []LinearTaskSource{todo, started}, []LinearIssueStartMutationResult{{IssueID: startIssueID, State: startAuthority().InProgressState}}, []error{nil})
		seedLinearStartIntent(t, store)
		if result, err := service.MoveReservedIssueToStarted(context.Background(), MoveReservedIssueToStartedCommand{RunID: store.run.ID}); err != nil || result.Status != "started" || len(store.effects) != 1 {
			t.Fatalf("result=%+v err=%v effects=%+v", result, err, store.effects)
		}
	})
	t.Run("after remote success before result", func(t *testing.T) {
		started := validStartSource()
		started.State = startAuthority().InProgressState
		started.UpdatedAt = started.UpdatedAt.Add(time.Second)
		started.SourceRevision = started.UpdatedAt.Format(time.RFC3339Nano)
		store, service := newLinearStartService(t, []LinearTaskSource{started}, nil, nil)
		seedLinearStartIntent(t, store)
		if result, err := service.MoveReservedIssueToStarted(context.Background(), MoveReservedIssueToStartedCommand{RunID: store.run.ID}); err != nil || result.Status != "started" || len(store.effects) != 1 {
			t.Fatalf("result=%+v err=%v effects=%+v", result, err, store.effects)
		}
	})
	t.Run("in flight target adoption", func(t *testing.T) {
		started := validStartSource()
		started.State = startAuthority().InProgressState
		started.UpdatedAt = started.UpdatedAt.Add(time.Second)
		started.SourceRevision = started.UpdatedAt.Format(time.RFC3339Nano)
		store, service := newLinearStartService(t, []LinearTaskSource{started}, nil, nil)
		seedLinearStartIntent(t, store)
		for key, effect := range store.effects {
			effect.Status, effect.ClaimedAt = "in_flight", time.Now().UTC()
			store.effects[key] = effect
		}
		if result, err := service.MoveReservedIssueToStarted(context.Background(), MoveReservedIssueToStartedCommand{RunID: store.run.ID}); err != nil || result.Status != "started" {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		for _, effect := range store.effects {
			if effect.Status != "observed" {
				t.Fatalf("target was not adopted: %+v", effect)
			}
		}
	})
}

func TestMoveReservedIssueToStartedRestartsBetweenFailedTodoAndRetryClaim(t *testing.T) {
	todo := validStartSource()
	started := todo
	started.State = startAuthority().InProgressState
	started.UpdatedAt = todo.UpdatedAt.Add(time.Second)
	started.SourceRevision = started.UpdatedAt.Format(time.RFC3339Nano)
	store, service := newLinearStartService(t, []LinearTaskSource{todo, started}, []LinearIssueStartMutationResult{{IssueID: startIssueID, State: startAuthority().InProgressState}}, []error{nil})
	seedLinearStartIntent(t, store)
	for key, effect := range store.effects {
		effect.Status, effect.ResultJSON, effect.ObservedAt = "failed", `{"category":"todo_after_mutation"}`, time.Now().UTC()
		store.effects[key] = effect
	}
	result, err := service.MoveReservedIssueToStarted(context.Background(), MoveReservedIssueToStartedCommand{RunID: store.run.ID})
	if err != nil || result.Status != "started" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, effect := range store.effects {
		if effect.Attempt != 2 || effect.Status != "observed" {
			t.Fatalf("restart did not claim the one durable retry: %+v", effect)
		}
	}
}

func TestMoveReservedIssueToStartedRetryCASLoserStopsUnavailable(t *testing.T) {
	store, service := newLinearStartService(t, []LinearTaskSource{validStartSource()}, nil, nil)
	seedLinearStartIntent(t, store)
	store.retryDenied = true
	for key, effect := range store.effects {
		effect.Status, effect.ResultJSON = "failed", `{"category":"todo_after_mutation"}`
		store.effects[key] = effect
	}
	_, err := service.MoveReservedIssueToStarted(context.Background(), MoveReservedIssueToStartedCommand{RunID: store.run.ID})
	if err == nil || !strings.Contains(err.Error(), "already in progress") || store.run.State != domain.StateReceived || len(service.starter.(*linearStartMover).calls) != 0 {
		t.Fatalf("err=%v run=%+v calls=%d", err, store.run, len(service.starter.(*linearStartMover).calls))
	}
}

func TestMoveReservedIssueToStartedDoesNotRetryNonConclusiveFailedEffects(t *testing.T) {
	for _, category := range []string{"forbidden", "partial_mutation", "postwrite_read"} {
		t.Run(category, func(t *testing.T) {
			store, service := newLinearStartService(t, []LinearTaskSource{validStartSource()}, nil, nil)
			seedLinearStartIntent(t, store)
			for key, effect := range store.effects {
				effect.Status, effect.ResultJSON = "failed", `{"category":"`+category+`"}`
				store.effects[key] = effect
			}
			_, err := service.MoveReservedIssueToStarted(context.Background(), MoveReservedIssueToStartedCommand{RunID: store.run.ID})
			if err == nil || store.run.State != domain.StateManualIntervention || len(service.starter.(*linearStartMover).calls) != 0 {
				t.Fatalf("err=%v run=%+v calls=%d", err, store.run, len(service.starter.(*linearStartMover).calls))
			}
		})
	}
}

func TestMoveReservedIssueToStartedHaltsTodoInFlightClaimsWithoutRetryingThem(t *testing.T) {
	for _, test := range []struct {
		name      string
		attempt   int
		claimedAt time.Time
	}{
		{name: "active first claim", attempt: 1, claimedAt: time.Now().UTC()},
		// A legal Linear HTTP request may outlive the former 30-second timeout.
		// Its old claim is still not evidence that the request process died.
		{name: "two minute old first claim", attempt: 1, claimedAt: time.Now().UTC().Add(-2 * time.Minute)},
		{name: "second claim", attempt: 2, claimedAt: time.Now().UTC().Add(-2 * time.Minute)},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, service := newLinearStartService(t, []LinearTaskSource{validStartSource()}, nil, nil)
			seedLinearStartIntent(t, store)
			for key, effect := range store.effects {
				effect.Status, effect.Attempt, effect.ClaimedAt = "in_flight", test.attempt, test.claimedAt
				store.effects[key] = effect
			}
			_, err := service.MoveReservedIssueToStarted(context.Background(), MoveReservedIssueToStartedCommand{RunID: store.run.ID})
			if err == nil || store.run.State != domain.StateManualIntervention || len(service.starter.(*linearStartMover).calls) != 0 {
				t.Fatalf("err=%v run=%+v calls=%d", err, store.run, len(service.starter.(*linearStartMover).calls))
			}
			for _, effect := range store.effects {
				if effect.Status != "in_flight" || effect.Attempt != test.attempt {
					t.Fatalf("non-owner overwrote in-flight claim: %+v", effect)
				}
			}
		})
	}
}

func TestMoveReservedIssueToStartedHaltsBeforeMutationOnReservationDrift(t *testing.T) {
	drifted := validStartSource()
	drifted.Labels = append(drifted.Labels, LinearLabel{ID: "label-hermes", Name: "agent:hermes"})
	store, service := newLinearStartService(t, []LinearTaskSource{drifted}, nil, nil)
	_, err := service.MoveReservedIssueToStarted(context.Background(), MoveReservedIssueToStartedCommand{RunID: store.run.ID})
	if err == nil || store.run.State != domain.StateManualIntervention || len(store.effects) != 0 || store.lastError == "" || !strings.Contains(store.transitions[0].EvidenceReference, "prewrite_drift") {
		t.Fatalf("err=%v run=%+v effects=%+v transitions=%+v", err, store.run, store.effects, store.transitions)
	}
}

func TestMoveReservedIssueToStartedHaltsOnEveryReservationAuthorityDrift(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*LinearTaskSource)
	}{
		{name: "team", mutate: func(source *LinearTaskSource) { source.Team.ID = "123e4567-e89b-42d3-a456-426614174199" }},
		{name: "source revision", mutate: func(source *LinearTaskSource) {
			source.SourceRevision = source.UpdatedAt.Add(time.Second).Format(time.RFC3339Nano)
		}},
		{name: "cycle", mutate: func(source *LinearTaskSource) { source.Cycle.ID = "different-cycle" }},
		{name: "branch", mutate: func(source *LinearTaskSource) { source.BranchName = "ifan/other" }},
		{name: "task", mutate: func(source *LinearTaskSource) { source.Description += "\nChanged." }},
		{name: "external state", mutate: func(source *LinearTaskSource) {
			source.State = LinearState{ID: "123e4567-e89b-42d3-a456-426614174198", Name: "In Review", Type: "started"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			drifted := validStartSource()
			test.mutate(&drifted)
			store, service := newLinearStartService(t, []LinearTaskSource{drifted}, nil, nil)
			_, err := service.MoveReservedIssueToStarted(context.Background(), MoveReservedIssueToStartedCommand{RunID: store.run.ID})
			if err == nil || store.run.State != domain.StateManualIntervention || len(store.effects) != 0 {
				t.Fatalf("err=%v run=%+v effects=%+v", err, store.run, store.effects)
			}
		})
	}
}

func TestMoveReservedIssueToStartedRejectsSameNameWrongUUIDMutationResponse(t *testing.T) {
	todo := validStartSource()
	wrong := startAuthority().InProgressState
	wrong.ID = "123e4567-e89b-42d3-a456-426614174104"
	store, service := newLinearStartService(t, []LinearTaskSource{todo}, []LinearIssueStartMutationResult{{IssueID: startIssueID, State: wrong}}, []error{nil})
	_, err := service.MoveReservedIssueToStarted(context.Background(), MoveReservedIssueToStartedCommand{RunID: store.run.ID})
	if err == nil || store.run.State != domain.StateManualIntervention || len(store.effects) != 1 {
		t.Fatalf("err=%v run=%+v effects=%+v", err, store.run, store.effects)
	}
}

func newLinearStartService(t *testing.T, sources []LinearTaskSource, results []LinearIssueStartMutationResult, errors []error) (*linearStartStore, *LinearReservedIssueStartService) {
	t.Helper()
	source := validStartSource()
	repository := LocalRepository{CanonicalRepository: "owner/repo", BaseBranch: "main", VerifierIDs: []string{"fixture-go-test"}}
	snapshot, _, err := admitLinearTask(source, admissionResolver{repositories: map[string]LocalRepository{"owner/repo": repository}})
	if err != nil {
		t.Fatal(err)
	}
	run := Run{ID: "run-linear-start", IssueID: source.Identifier, IdempotencyKey: snapshot.IdempotencyKey, SourceRevision: source.SourceRevision, RawIssueJSON: string(snapshot.RawJSON), NormalizedTaskJSON: string(snapshot.NormalizedJSON), TaskHash: snapshot.TaskHash, Repository: repository.CanonicalRepository, WorkingBranch: source.BranchName, State: domain.StateReceived}
	store := &linearStartStore{run: run, effects: make(map[string]SideEffectRecord)}
	service, err := NewLinearReservedIssueStartService(&linearStartReader{sources: sources}, &linearStartMover{results: results, errors: errors}, admissionResolver{repositories: map[string]LocalRepository{"owner/repo": repository}}, store, startAuthority())
	if err != nil {
		t.Fatal(err)
	}
	return store, service
}

func seedLinearStartIntent(t *testing.T, store *linearStartStore) {
	t.Helper()
	reservation, err := reservedLinearIssue(store.run)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := linearIssueStartIntent(store.run, reservation, startAuthority())
	if err != nil {
		t.Fatal(err)
	}
	effect := sideEffectFromIntent(store.run.ID, intent, 1)
	effect.ID, effect.Status = 1, "intent"
	store.effects[effect.IdempotencyKey] = effect
}

func startAuthority() LinearIssueStartAuthority {
	return LinearIssueStartAuthority{TeamID: startTeamID, TeamKey: "IFAN", TodoState: LinearState{ID: startTodoID, Name: "Todo", Type: "unstarted"}, InProgressState: LinearState{ID: startStateID, Name: "In Progress", Type: "started"}}
}

func validStartSource() LinearTaskSource {
	created := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	updated := created.Add(time.Hour)
	return LinearTaskSource{Provider: "linear", IssueID: startIssueID, Identifier: "IFAN-32", URL: "https://linear.app/ifan/issue/IFAN-32/fixture", Title: "Freeze one trusted task",
		Description: "## Outcome\n\nFreeze one trusted task snapshot.\n\n## Acceptance Criteria\n\n- Preserve a narrow mutation.\n\n## Out of Scope\n\n- Driver invocation.",
		Team:        LinearTeam{ID: startTeamID, Key: "IFAN", Name: "I-Fan"}, State: startAuthority().TodoState,
		Labels: []LinearLabel{{ID: "label-codex", Name: "agent:codex"}, {ID: "label-repository", Name: "owner/repo"}}, Cycle: LinearCycle{ID: "cycle", Number: 1, StartsAt: created, EndsAt: created.Add(7 * 24 * time.Hour), IsActive: true},
		BranchName: "ifan/ifan-32-linear-start", SourceRevision: updated.Format(time.RFC3339Nano), CreatedAt: created, UpdatedAt: updated, ObservedAt: updated}
}
