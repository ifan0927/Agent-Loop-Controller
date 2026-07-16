package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type OperatorActionType string

const (
	OperatorActionRetry   OperatorActionType = "retry"
	OperatorActionAbandon OperatorActionType = "abandon"
)

const (
	OperatorActionStatusValidated = "validated"
	OperatorActionStatusApplied   = "applied"
	OperatorActionStatusObserved  = "observed"

	OperatorActionResultPending   = "pending"
	OperatorActionResultApplied   = "applied"
	OperatorActionResultSucceeded = "succeeded"
	OperatorActionResultFailed    = "failed"
	OperatorActionResultAmbiguous = "ambiguous"
)

// OperatorActionRecord is narrow proof that an authenticated operator answered
// one exact parked attention event. It never contains CLI prose or executable
// input. Lifecycle updates are monotonic CAS operations on this immutable
// authority envelope.
type OperatorActionRecord struct {
	ActionID                    string
	IdempotencyKey              string
	PayloadDigest               string
	RunID                       string
	Repository                  string
	ExpectedState               domain.State
	RunIdempotencyKey           string
	TransitionSequence          int64
	ActionType                  OperatorActionType
	Requester                   Requester
	ReasonCode                  string
	AttentionEventKey           string
	Status                      string
	ResultStatus                string
	ResultingState              domain.State
	ResultingTransitionSequence int64
	EvidenceDigest              string
	OutcomeDigest               string
	NextEligibleAt              time.Time
	ReceivedAt                  time.Time
	ValidatedAt                 time.Time
	AppliedAt                   time.Time
	ObservedAt                  time.Time
}

type OperatorActionInput struct {
	Requester          Requester
	RunID              string
	Repository         string
	ExpectedState      domain.State
	RunIdempotencyKey  string
	TransitionSequence int64
	ActionType         OperatorActionType
	ReasonCode         string
	AttentionEventKey  string
}

type OperatorActionMutationResult struct {
	ActionID                    string
	ExpectedStatus              string
	ResultStatus                string
	ResultingState              domain.State
	ResultingTransitionSequence int64
	EvidenceDigest              string
	At                          time.Time
}

type OperatorActionStore interface {
	RunStore
	CurrentOperatorAttentionQuery
	BeginOperatorAction(context.Context, OperatorActionRecord) (OperatorActionRecord, bool, error)
	ApplyOperatorActionResult(context.Context, OperatorActionMutationResult) (OperatorActionRecord, bool, error)
	ObserveOperatorActionResult(context.Context, OperatorActionMutationResult) (OperatorActionRecord, bool, error)
}

type OperatorActionService struct {
	store OperatorActionStore
	now   func() time.Time
}

func NewOperatorActionService(store OperatorActionStore) (*OperatorActionService, error) {
	if store == nil {
		return nil, errors.New("operator action store is required")
	}
	return &OperatorActionService{store: store, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (s *OperatorActionService) Prepare(ctx context.Context, input OperatorActionInput) (OperatorActionRecord, bool, error) {
	if err := validateOperatorActionInput(input); err != nil {
		return OperatorActionRecord{}, false, serviceError(ErrorInvalidInput, "operator action input is invalid", err)
	}
	inspection, err := s.store.Inspect(ctx, input.RunID)
	if err != nil {
		return OperatorActionRecord{}, false, classifyServiceError(err)
	}
	run := inspection.Run
	if err := authorizePersistedRequester(run, input.Requester); err != nil {
		return OperatorActionRecord{}, false, err
	}
	received := s.now().UTC()
	record := newOperatorActionRecord(input, received)
	if persisted, found, conflict := replayOperatorAction(inspection.OperatorActions, record); found || conflict {
		if conflict {
			return OperatorActionRecord{}, false, serviceError(ErrorConflict, "operator action idempotency authority changed", nil)
		}
		return persisted, false, nil
	}
	if run.Repository != input.Repository || run.State != input.ExpectedState || run.IdempotencyKey != input.RunIdempotencyKey {
		return OperatorActionRecord{}, false, serviceError(ErrorConflict, "operator action run authority changed", nil)
	}
	sequence := latestTransitionSequence(inspection.Timeline)
	if sequence != input.TransitionSequence {
		return OperatorActionRecord{}, false, serviceError(ErrorConflict, "operator action transition authority changed", nil)
	}
	event, found, err := s.store.CurrentOperatorAttention(ctx, input.RunID)
	if err != nil {
		return OperatorActionRecord{}, false, classifyServiceError(err)
	}
	if !found || event.EventKey != input.AttentionEventKey || event.RunID != run.ID || event.ControllerState != string(run.State) || event.ReasonCode != input.ReasonCode || !slices.Contains(event.AllowedActions, OperatorAttentionActionID(input.ActionType)) {
		return OperatorActionRecord{}, false, serviceError(ErrorConflict, "operator action is not advertised by current attention", nil)
	}
	persisted, created, err := s.store.BeginOperatorAction(ctx, record)
	if err != nil {
		return OperatorActionRecord{}, false, classifyServiceError(err)
	}
	return persisted, created, nil
}

func replayOperatorAction(records []OperatorActionRecord, expected OperatorActionRecord) (OperatorActionRecord, bool, bool) {
	for _, record := range records {
		if record.IdempotencyKey != expected.IdempotencyKey {
			continue
		}
		if record.ActionID != expected.ActionID || record.PayloadDigest != expected.PayloadDigest {
			return OperatorActionRecord{}, false, true
		}
		return record, true, false
	}
	return OperatorActionRecord{}, false, false
}

func (s *OperatorActionService) RecordApplied(ctx context.Context, result OperatorActionMutationResult) (OperatorActionRecord, bool, error) {
	if err := ValidateOperatorActionMutationResult(result, false); err != nil {
		return OperatorActionRecord{}, false, serviceError(ErrorInvalidInput, "operator action applied result is invalid", err)
	}
	record, changed, err := s.store.ApplyOperatorActionResult(ctx, result)
	if err != nil {
		return OperatorActionRecord{}, false, classifyServiceError(err)
	}
	return record, changed, nil
}

func (s *OperatorActionService) RecordObserved(ctx context.Context, result OperatorActionMutationResult) (OperatorActionRecord, bool, error) {
	if err := ValidateOperatorActionMutationResult(result, true); err != nil {
		return OperatorActionRecord{}, false, serviceError(ErrorInvalidInput, "operator action observed result is invalid", err)
	}
	record, changed, err := s.store.ObserveOperatorActionResult(ctx, result)
	if err != nil {
		return OperatorActionRecord{}, false, classifyServiceError(err)
	}
	return record, changed, nil
}

func newOperatorActionRecord(input OperatorActionInput, received time.Time) OperatorActionRecord {
	payload := struct {
		RunID, Repository, ExpectedState, RunKey, ActionType, RequesterLogin, RequesterNode, RequesterType, Reason, EventKey string
		TransitionSequence, RequesterDatabaseID                                                                              int64
	}{input.RunID, input.Repository, string(input.ExpectedState), input.RunIdempotencyKey, string(input.ActionType), strings.ToLower(input.Requester.ID), input.Requester.NodeID, input.Requester.ActorType, input.ReasonCode, input.AttentionEventKey, input.TransitionSequence, input.Requester.DatabaseID}
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	idempotencySum := sha256.Sum256([]byte("operator-action-idempotency:" + digest))
	idempotency := hex.EncodeToString(idempotencySum[:])
	return OperatorActionRecord{ActionID: "operator-action-" + idempotency[:24], IdempotencyKey: idempotency, PayloadDigest: digest, RunID: input.RunID, Repository: input.Repository, ExpectedState: input.ExpectedState, RunIdempotencyKey: input.RunIdempotencyKey, TransitionSequence: input.TransitionSequence, ActionType: input.ActionType, Requester: input.Requester, ReasonCode: input.ReasonCode, AttentionEventKey: input.AttentionEventKey, Status: OperatorActionStatusValidated, ResultStatus: OperatorActionResultPending, ReceivedAt: received, ValidatedAt: received}
}

func validateOperatorActionInput(input OperatorActionInput) error {
	if input.RunID == "" || input.Repository == "" || input.ExpectedState == "" || input.RunIdempotencyKey == "" || input.TransitionSequence < 1 || input.ReasonCode == "" || input.AttentionEventKey == "" {
		return errors.New("operator action authority is incomplete")
	}
	if input.ActionType != OperatorActionRetry && input.ActionType != OperatorActionAbandon {
		return errors.New("operator action type is invalid")
	}
	if input.Requester.ID == "" || input.Requester.Kind != "github_login" || input.Requester.DatabaseID < 1 || input.Requester.NodeID == "" || input.Requester.ActorType != "User" {
		return errors.New("operator action requester identity is incomplete")
	}
	return nil
}

func ValidateOperatorActionRecord(record OperatorActionRecord) error {
	if record.ActionID == "" || !validOperatorAttentionDigest(record.IdempotencyKey) || !validOperatorAttentionDigest(record.PayloadDigest) || record.Status != OperatorActionStatusValidated && record.Status != OperatorActionStatusApplied && record.Status != OperatorActionStatusObserved || record.ReceivedAt.IsZero() || record.ValidatedAt.IsZero() || record.ValidatedAt.Before(record.ReceivedAt) {
		return errors.New("operator action record is invalid")
	}
	input := OperatorActionInput{Requester: record.Requester, RunID: record.RunID, Repository: record.Repository, ExpectedState: record.ExpectedState, RunIdempotencyKey: record.RunIdempotencyKey, TransitionSequence: record.TransitionSequence, ActionType: record.ActionType, ReasonCode: record.ReasonCode, AttentionEventKey: record.AttentionEventKey}
	expected := newOperatorActionRecord(input, record.ReceivedAt)
	if err := validateOperatorActionInput(input); err != nil || expected.PayloadDigest != record.PayloadDigest || expected.IdempotencyKey != record.IdempotencyKey || expected.ActionID != record.ActionID {
		return errors.New("operator action record authority is invalid")
	}
	if record.Status == OperatorActionStatusValidated {
		if record.ResultStatus != OperatorActionResultPending || !record.AppliedAt.IsZero() || !record.ObservedAt.IsZero() || !record.NextEligibleAt.IsZero() || record.ResultingState != "" || record.ResultingTransitionSequence != 0 || record.EvidenceDigest != "" || record.OutcomeDigest != "" {
			return errors.New("validated operator action result is invalid")
		}
		return nil
	}
	if record.ResultingState == "" || record.ResultingTransitionSequence < record.TransitionSequence || !validOperatorAttentionDigest(record.EvidenceDigest) || record.AppliedAt.IsZero() || record.AppliedAt.Before(record.ValidatedAt) {
		return errors.New("operator action applied result is invalid")
	}
	if record.ActionType == OperatorActionRetry && !record.NextEligibleAt.After(record.AppliedAt) {
		return errors.New("operator retry eligibility evidence is invalid")
	}
	if record.Status == OperatorActionStatusApplied {
		if record.ResultStatus != OperatorActionResultApplied || !record.ObservedAt.IsZero() || record.OutcomeDigest != "" {
			return errors.New("applied operator action result is invalid")
		}
		return nil
	}
	if record.ResultStatus != OperatorActionResultSucceeded && record.ResultStatus != OperatorActionResultFailed && record.ResultStatus != OperatorActionResultAmbiguous || !validOperatorAttentionDigest(record.OutcomeDigest) || record.ObservedAt.IsZero() || record.ObservedAt.Before(record.AppliedAt) {
		return errors.New("observed operator action result is invalid")
	}
	return nil
}

func ValidateOperatorActionMutationResult(result OperatorActionMutationResult, observed bool) error {
	if result.ActionID == "" || result.ResultingState == "" || result.ResultingTransitionSequence < 1 || !validOperatorAttentionDigest(result.EvidenceDigest) || result.At.IsZero() {
		return errors.New("operator action mutation result is invalid")
	}
	if !observed {
		if result.ExpectedStatus != OperatorActionStatusValidated || result.ResultStatus != OperatorActionResultApplied {
			return errors.New("operator action applied result is invalid")
		}
		return nil
	}
	if result.ExpectedStatus != OperatorActionStatusApplied || result.ResultStatus != OperatorActionResultSucceeded && result.ResultStatus != OperatorActionResultFailed && result.ResultStatus != OperatorActionResultAmbiguous {
		return errors.New("operator action observed result is invalid")
	}
	return nil
}

func latestTransitionSequence(transitions []Transition) int64 {
	if len(transitions) == 0 {
		return 0
	}
	return transitions[len(transitions)-1].Sequence
}
