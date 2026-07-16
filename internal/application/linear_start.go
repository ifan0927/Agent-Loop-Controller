package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

const linearIssueStartEffectKind = "linear_move_to_started"
const LinearIssueStartTodoRetryResult = `{"category":"todo_after_mutation"}`

// LinearIssueStartAuthority is the configured immutable workflow authority
// needed for the one pre-delivery Linear transition.
type LinearIssueStartAuthority struct {
	TeamID          string
	TeamKey         string
	TodoState       LinearState
	InProgressState LinearState
}

type MoveReservedIssueToStartedCommand struct {
	RunID string
}

type MoveReservedIssueToStartedResult struct {
	Run          RunResult                  `json:"run"`
	Status       string                     `json:"status"`
	Observations []LinearRequestObservation `json:"observations"`
}

type linearIssueStartStore interface {
	RunStore
	BeginSideEffect(context.Context, SideEffectRecord) (SideEffectRecord, bool, error)
	FinishLinearIssueStartSideEffect(context.Context, SideEffectRecord, string, int) error
	RetryLinearIssueStartSideEffect(context.Context, SideEffectRecord) (SideEffectRecord, bool, error)
	ClaimLinearIssueStartSideEffect(context.Context, SideEffectRecord, time.Time) (SideEffectRecord, bool, error)
	SaveLinearRequestObservation(context.Context, string, LinearRequestObservation) error
}

// LinearReservedIssueStartService deliberately stops at the durable Linear
// state mutation. It never selects candidates, creates a scheduler lease, or
// invokes the local controller.
type LinearReservedIssueStartService struct {
	reader    LinearIssueReader
	starter   LinearReservedIssueStarter
	resolver  LinearAdmissionRepositoryResolver
	store     linearIssueStartStore
	authority LinearIssueStartAuthority
}

func NewLinearReservedIssueStartService(reader LinearIssueReader, starter LinearReservedIssueStarter, resolver LinearAdmissionRepositoryResolver, store linearIssueStartStore, authority LinearIssueStartAuthority) (*LinearReservedIssueStartService, error) {
	if reader == nil || starter == nil || resolver == nil || store == nil {
		return nil, errors.New("Linear issue start dependencies are required")
	}
	if err := authority.validate(); err != nil {
		return nil, err
	}
	return &LinearReservedIssueStartService{reader: reader, starter: starter, resolver: resolver, store: store, authority: authority}, nil
}

func (a LinearIssueStartAuthority) validate() error {
	if !validLinearUUID(a.TeamID) || a.TeamKey != "IFAN" ||
		!sameLinearWorkflowState(a.TodoState, "Todo", "unstarted") ||
		!sameLinearWorkflowState(a.InProgressState, "In Progress", "started") ||
		a.TodoState.ID == a.InProgressState.ID {
		return errors.New("Linear issue start authority is invalid")
	}
	return nil
}

func validLinearUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == value && parsed.Variant() == uuid.RFC4122
}

func sameLinearWorkflowState(state LinearState, name, stateType string) bool {
	return validLinearUUID(state.ID) && state.Name == name && state.Type == stateType
}

// MoveReservedIssueToStarted persists its immutable intent before the only
// mutation, then reconciles every outcome through an authoritative re-read.
func (s *LinearReservedIssueStartService) MoveReservedIssueToStarted(ctx context.Context, command MoveReservedIssueToStartedCommand) (MoveReservedIssueToStartedResult, error) {
	if strings.TrimSpace(command.RunID) == "" {
		return MoveReservedIssueToStartedResult{}, serviceError(ErrorInvalidInput, "run ID is required", nil)
	}
	run, err := s.store.GetRun(ctx, command.RunID)
	if err != nil {
		return MoveReservedIssueToStartedResult{}, classifyServiceError(err)
	}
	if run.State != domain.StateReceived {
		return MoveReservedIssueToStartedResult{Run: projectRunResult(run)}, serviceError(ErrorConflict, "reserved run is no longer eligible to start", nil)
	}
	reservation, err := reservedLinearIssue(run)
	if err != nil {
		return MoveReservedIssueToStartedResult{}, s.manual(ctx, run, SideEffectRecord{}, "persisted_reservation")
	}
	intent, err := linearIssueStartIntent(run, reservation, s.authority)
	if err != nil {
		return MoveReservedIssueToStartedResult{}, s.manual(ctx, run, SideEffectRecord{}, "persisted_reservation")
	}

	source, observations, readErr := s.read(ctx, run)
	if readErr != nil {
		return MoveReservedIssueToStartedResult{}, s.manual(ctx, run, SideEffectRecord{}, "prewrite_read")
	}
	if stateMatches(source.State, s.authority.InProgressState) {
		if err := s.proveStartedReservation(run, reservation, source); err != nil {
			return MoveReservedIssueToStartedResult{}, s.manual(ctx, run, SideEffectRecord{}, "started_reconciliation_drift")
		}
		side, _, beginErr := s.store.BeginSideEffect(ctx, sideEffectFromIntent(run.ID, intent, 1))
		if beginErr != nil {
			return MoveReservedIssueToStartedResult{}, classifyServiceError(beginErr)
		}
		return s.observeStarted(ctx, run, side, observations)
	}
	if err := s.proveTodoReservation(run, reservation, source); err != nil {
		return MoveReservedIssueToStartedResult{}, s.manual(ctx, run, SideEffectRecord{}, "prewrite_drift")
	}
	side, _, err := s.store.BeginSideEffect(ctx, sideEffectFromIntent(run.ID, intent, 1))
	if err != nil {
		return MoveReservedIssueToStartedResult{}, classifyServiceError(err)
	}
	if side.Status == "observed" {
		return MoveReservedIssueToStartedResult{}, s.manual(ctx, run, side, "contradictory_observed_intent")
	}
	if side.Status == "failed" && side.Attempt == 1 {
		if side.ResultJSON != LinearIssueStartTodoRetryResult {
			return MoveReservedIssueToStartedResult{}, s.manual(ctx, run, side, "non_retryable_failed_mutation")
		}
		var retried bool
		side, retried, err = s.store.RetryLinearIssueStartSideEffect(ctx, side)
		if err != nil {
			return MoveReservedIssueToStartedResult{}, classifyServiceError(err)
		}
		if !retried {
			return MoveReservedIssueToStartedResult{Run: projectRunResult(run), Observations: observations}, serviceError(ErrorUnavailable, "Linear issue start mutation is already in progress", nil)
		}
	}
	if side.Status == "in_flight" {
		return MoveReservedIssueToStartedResult{}, s.manualWithoutEffect(ctx, run, "unreconciled_in_flight_mutation")
	}
	if side.Status != "intent" || side.Attempt < 1 || side.Attempt > 2 {
		return MoveReservedIssueToStartedResult{}, s.manual(ctx, run, side, "exhausted_intent")
	}

	for {
		var claimed bool
		side, claimed, err = s.store.ClaimLinearIssueStartSideEffect(ctx, side, time.Now().UTC())
		if err != nil {
			return MoveReservedIssueToStartedResult{}, classifyServiceError(err)
		}
		if !claimed {
			return MoveReservedIssueToStartedResult{Run: projectRunResult(run), Observations: observations}, serviceError(ErrorUnavailable, "Linear issue start mutation is already in progress", nil)
		}
		mutation, mutationObservations, mutationErr := s.starter.MoveReservedIssueToStarted(ctx, LinearIssueStartMutation{IssueID: reservation.IssueID, TargetStateID: s.authority.InProgressState.ID})
		observations = append(observations, mutationObservations...)
		if err := s.saveObservations(ctx, run.ID, mutationObservations); err != nil {
			return MoveReservedIssueToStartedResult{}, classifyServiceError(err)
		}
		if mutationErr == nil && (mutation.IssueID != reservation.IssueID || !stateMatches(mutation.State, s.authority.InProgressState)) {
			return MoveReservedIssueToStartedResult{}, s.manual(ctx, run, side, "partial_or_contradictory_mutation")
		}
		if mutationErr != nil && !ambiguousLinearStartError(mutationErr) {
			return MoveReservedIssueToStartedResult{}, s.manual(ctx, run, side, linearStartErrorClass(mutationErr))
		}

		reconciled, rereadObservations, rereadErr := s.read(ctx, run)
		observations = append(observations, rereadObservations...)
		if rereadErr != nil {
			return MoveReservedIssueToStartedResult{}, s.manual(ctx, run, side, "postwrite_read")
		}
		if stateMatches(reconciled.State, s.authority.InProgressState) {
			if err := s.proveStartedReservation(run, reservation, reconciled); err != nil {
				return MoveReservedIssueToStartedResult{}, s.manual(ctx, run, side, "started_reconciliation_drift")
			}
			return s.observeStarted(ctx, run, side, observations)
		}
		if err := s.proveTodoReservation(run, reservation, reconciled); err != nil {
			return MoveReservedIssueToStartedResult{}, s.manual(ctx, run, side, "postwrite_drift")
		}
		if side.Attempt != 1 {
			return MoveReservedIssueToStartedResult{}, s.manual(ctx, run, side, "todo_after_bounded_retry")
		}
		if err := s.finish(ctx, &side, "failed", LinearIssueStartTodoRetryResult); err != nil {
			return MoveReservedIssueToStartedResult{}, classifyServiceError(err)
		}
		var retried bool
		side, retried, err = s.store.RetryLinearIssueStartSideEffect(ctx, side)
		if err != nil {
			return MoveReservedIssueToStartedResult{}, classifyServiceError(err)
		}
		if !retried {
			return MoveReservedIssueToStartedResult{Run: projectRunResult(run), Observations: observations}, serviceError(ErrorUnavailable, "Linear issue start mutation is already in progress", nil)
		}
		if side.Status != "intent" || side.Attempt != 2 {
			return MoveReservedIssueToStartedResult{}, s.manual(ctx, run, side, "retry_claim_conflict")
		}
	}
}

type linearIssueStartIntentRecord struct {
	IssueID           string `json:"issue_id"`
	Identifier        string `json:"identifier"`
	SourceStateID     string `json:"source_state_id"`
	TargetStateID     string `json:"target_state_id"`
	SourceRevision    string `json:"source_revision"`
	TaskHash          string `json:"task_hash"`
	RunID             string `json:"run_id"`
	IdempotencyDigest string `json:"idempotency_digest"`
}

func linearIssueStartIntent(run Run, source LinearTaskSource, authority LinearIssueStartAuthority) (linearIssueStartIntentRecord, error) {
	if source.IssueID == "" || source.Identifier != run.IssueID || source.SourceRevision != run.SourceRevision || run.TaskHash == "" {
		return linearIssueStartIntentRecord{}, errors.New("persisted reservation is incomplete")
	}
	seed := strings.Join([]string{run.ID, source.IssueID, authority.TodoState.ID, authority.InProgressState.ID, source.SourceRevision, run.TaskHash}, "\x00")
	digest := sha256.Sum256([]byte(seed))
	return linearIssueStartIntentRecord{IssueID: source.IssueID, Identifier: source.Identifier, SourceStateID: authority.TodoState.ID, TargetStateID: authority.InProgressState.ID, SourceRevision: source.SourceRevision, TaskHash: run.TaskHash, RunID: run.ID, IdempotencyDigest: hex.EncodeToString(digest[:])}, nil
}

func sideEffectFromIntent(runID string, intent linearIssueStartIntentRecord, attempt int) SideEffectRecord {
	raw, _ := json.Marshal(intent)
	return SideEffectRecord{RunID: runID, Kind: linearIssueStartEffectKind, IdempotencyKey: intent.IdempotencyDigest, IntentJSON: string(raw), Attempt: attempt}
}

func reservedLinearIssue(run Run) (LinearTaskSource, error) {
	var source LinearTaskSource
	if json.Unmarshal([]byte(run.RawIssueJSON), &source) != nil || source.Provider != "linear" || !validLinearUUID(source.IssueID) || source.Identifier != run.IssueID || source.SourceRevision != run.SourceRevision {
		return LinearTaskSource{}, errors.New("persisted Linear reservation is invalid")
	}
	var task domain.CodingTask
	if json.Unmarshal([]byte(run.NormalizedTaskJSON), &task) != nil || task.IssueID != run.IssueID || stableTaskDigest(run.NormalizedTaskJSON) == "" {
		return LinearTaskSource{}, errors.New("persisted normalized task is invalid")
	}
	return source, nil
}

func (s *LinearReservedIssueStartService) proveTodoReservation(run Run, reservation, source LinearTaskSource) error {
	if source.Provider != "linear" || source.IssueID != reservation.IssueID || source.Identifier != run.IssueID || source.Team.ID != s.authority.TeamID || source.Team.Key != s.authority.TeamKey || !stateMatches(source.State, s.authority.TodoState) || source.SourceRevision != reservation.SourceRevision || source.SourceRevision != run.SourceRevision || source.SourceRevision != source.UpdatedAt.UTC().Format(time.RFC3339Nano) {
		return errors.New("Linear Todo reservation drifted")
	}
	if !sameLinearReservationMetadata(reservation, source) {
		return errors.New("Linear Todo reservation metadata drifted")
	}
	snapshot, repository, err := normalizeLinearTask(source, s.resolver, false, false, false)
	if err != nil || repository.CanonicalRepository != run.Repository || snapshot.TaskHash != run.TaskHash || snapshot.Task.WorkingBranch != run.WorkingBranch {
		return errors.New("Linear Todo task no longer matches the reservation")
	}
	return nil
}

func (s *LinearReservedIssueStartService) proveStartedReservation(run Run, reservation, source LinearTaskSource) error {
	if source.Provider != "linear" || source.IssueID != reservation.IssueID || source.Identifier != run.IssueID || source.Team.ID != s.authority.TeamID || source.Team.Key != s.authority.TeamKey || !stateMatches(source.State, s.authority.InProgressState) || strings.TrimSpace(source.SourceRevision) == "" || source.SourceRevision != source.UpdatedAt.UTC().Format(time.RFC3339Nano) || !source.UpdatedAt.After(reservation.UpdatedAt) {
		return errors.New("Linear started reservation drifted")
	}
	if !sameLinearReservationMetadata(reservation, source) {
		return errors.New("Linear started reservation metadata drifted")
	}
	snapshot, repository, err := normalizeLinearTask(source, s.resolver, true, false, false)
	if err != nil || repository.CanonicalRepository != run.Repository || snapshot.Task.WorkingBranch != run.WorkingBranch || stableTaskDigest(run.NormalizedTaskJSON) != stableTaskDigestFromTask(snapshot.Task) {
		return errors.New("Linear started task no longer matches the reservation")
	}
	return nil
}

func sameLinearReservationMetadata(expected, actual LinearTaskSource) bool {
	if expected.Team != actual.Team || expected.Cycle.ID != actual.Cycle.ID || expected.Cycle.Number != actual.Cycle.Number || !expected.Cycle.StartsAt.Equal(actual.Cycle.StartsAt) || !expected.Cycle.EndsAt.Equal(actual.Cycle.EndsAt) || expected.Cycle.IsActive != actual.Cycle.IsActive || expected.BranchName != actual.BranchName || len(expected.Labels) != len(actual.Labels) {
		return false
	}
	expectedLabels := make(map[string]string, len(expected.Labels))
	for _, label := range expected.Labels {
		if label.ID == "" || label.Name == "" {
			return false
		}
		expectedLabels[label.ID] = label.Name
	}
	for _, label := range actual.Labels {
		if expectedLabels[label.ID] != label.Name {
			return false
		}
	}
	return true
}

func stateMatches(actual, expected LinearState) bool {
	return actual.ID == expected.ID && actual.Name == expected.Name && actual.Type == expected.Type
}

func (s *LinearReservedIssueStartService) read(ctx context.Context, run Run) (LinearTaskSource, []LinearRequestObservation, error) {
	source, observations, err := s.reader.ReadIssue(ctx, run.IssueID)
	if saveErr := s.saveObservations(ctx, run.ID, observations); saveErr != nil {
		return LinearTaskSource{}, observations, saveErr
	}
	return source, observations, err
}

func (s *LinearReservedIssueStartService) saveObservations(ctx context.Context, runID string, observations []LinearRequestObservation) error {
	for _, observation := range observations {
		if err := s.store.SaveLinearRequestObservation(ctx, runID, observation); err != nil {
			return err
		}
	}
	return nil
}

func (s *LinearReservedIssueStartService) observeStarted(ctx context.Context, run Run, side SideEffectRecord, observations []LinearRequestObservation) (MoveReservedIssueToStartedResult, error) {
	result := `{"status":"started"}`
	if side.Status != "observed" {
		if err := s.finish(ctx, &side, "observed", result); err != nil {
			return MoveReservedIssueToStartedResult{}, classifyServiceError(err)
		}
	}
	return MoveReservedIssueToStartedResult{Run: projectRunResult(run), Status: "started", Observations: observations}, nil
}

func (s *LinearReservedIssueStartService) manual(ctx context.Context, run Run, side SideEffectRecord, category string) error {
	if side.ID > 0 && side.Status != "observed" {
		result, _ := json.Marshal(map[string]string{"category": category})
		if err := s.finish(ctx, &side, "failed", string(result)); err != nil {
			return classifyServiceError(err)
		}
	}
	_ = s.store.SetLastError(ctx, run.ID, "Linear issue start requires a human decision")
	if err := s.store.Transition(ctx, run.ID, run.State, domain.StateManualIntervention, "Linear issue start requires a human decision", "linear_issue_start:"+category, ""); err != nil {
		return classifyServiceError(err)
	}
	return serviceError(ErrorConflict, "Linear issue start requires a human decision", nil)
}

// manualWithoutEffect records the operator stop but intentionally leaves an
// unknown in-flight mutation untouched. A concurrent HTTP request may still
// complete; this caller must not overwrite its claim or infer process death.
func (s *LinearReservedIssueStartService) manualWithoutEffect(ctx context.Context, run Run, category string) error {
	_ = s.store.SetLastError(ctx, run.ID, "Linear issue start requires a human decision")
	if err := s.store.Transition(ctx, run.ID, run.State, domain.StateManualIntervention, "Linear issue start requires a human decision", "linear_issue_start:"+category, ""); err != nil {
		return classifyServiceError(err)
	}
	return serviceError(ErrorConflict, "Linear issue start requires a human decision", nil)
}

func (s *LinearReservedIssueStartService) finish(ctx context.Context, side *SideEffectRecord, status, result string) error {
	expectedStatus, expectedAttempt := side.Status, side.Attempt
	side.Status, side.ResultJSON, side.ObservedAt = status, result, time.Now().UTC()
	return s.store.FinishLinearIssueStartSideEffect(ctx, *side, expectedStatus, expectedAttempt)
}

func ambiguousLinearStartError(err error) bool {
	var mutationError *LinearIssueStartMutationError
	return errors.As(err, &mutationError) && mutationError.Ambiguous
}

func linearStartErrorClass(err error) string {
	var mutationError *LinearIssueStartMutationError
	if errors.As(err, &mutationError) && mutationError.Class != "" {
		return mutationError.Class
	}
	return "mutation_failed"
}
