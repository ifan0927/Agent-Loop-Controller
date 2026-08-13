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

var ErrOperatorActionConflict = errors.New("operator action authority conflicts")

type OperatorActionType string

const (
	OperatorActionDecide              OperatorActionType = "decide"
	OperatorActionRetry               OperatorActionType = "retry"
	OperatorActionAbandon             OperatorActionType = "abandon"
	OperatorActionRecoverCIWait       OperatorActionType = "recover_ci_wait"
	OperatorActionRecoverOwnedPush    OperatorActionType = "recover_owned_push"
	OperatorActionAcceptExternalMerge OperatorActionType = "accept_external_merge"
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
	RequestDigest               string
	RunID                       string
	Repository                  string
	ExpectedState               domain.State
	RunIdempotencyKey           string
	TransitionSequence          int64
	ActionType                  OperatorActionType
	Requester                   Requester
	ReasonCode                  string
	AttentionEventKey           string
	ExpectedAuthorityDigest     string
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
	Requester               Requester
	RunID                   string
	Repository              string
	ExpectedState           domain.State
	RunIdempotencyKey       string
	TransitionSequence      int64
	ActionType              OperatorActionType
	ReasonCode              string
	AttentionEventKey       string
	RequestDigest           string
	ExpectedAuthorityDigest string
}

type legalActionAuthorityContextKey struct{}

type legalActionExecutionAuthority struct {
	RunID           string
	Action          OperatorActionType
	AuthorityDigest string
}

func withLegalActionExecutionAuthority(ctx context.Context, offer LegalActionOffer) context.Context {
	return context.WithValue(ctx, legalActionAuthorityContextKey{}, legalActionExecutionAuthority{RunID: offer.TargetID, Action: OperatorActionType(offer.Action), AuthorityDigest: offer.AuthorityDigest})
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
	if authority, ok := ctx.Value(legalActionAuthorityContextKey{}).(legalActionExecutionAuthority); ok && authority.RunID == input.RunID && authority.Action == input.ActionType && validAuthorityDigest(authority.AuthorityDigest) {
		input.ExpectedAuthorityDigest = authority.AuthorityDigest
	}
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
	if !found || event.EventKey != input.AttentionEventKey || event.RunID != run.ID || event.ControllerState != string(run.State) || event.ReasonCode != input.ReasonCode || !slices.Contains(legalActionIDsForInspection(run, inspection, event), OperatorAttentionActionID(input.ActionType)) {
		return OperatorActionRecord{}, false, serviceError(ErrorConflict, "operator action is not advertised by current attention", nil)
	}
	persisted, created, err := s.store.BeginOperatorAction(ctx, record)
	if err != nil {
		if errors.Is(err, ErrOperatorActionConflict) {
			return OperatorActionRecord{}, false, serviceError(ErrorConflict, "operator action authority changed", err)
		}
		return OperatorActionRecord{}, false, classifyServiceError(err)
	}
	return persisted, created, nil
}

func replayOperatorAction(records []OperatorActionRecord, expected OperatorActionRecord) (OperatorActionRecord, bool, bool) {
	for _, record := range records {
		if legacyOperatorActionMatches(record, expected) {
			return record, true, false
		}
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

func legacyOperatorActionMatches(record, expected OperatorActionRecord) bool {
	if record.RequestDigest != record.PayloadDigest || !legacyOperatorActionIdentity(record) || expected.RequestDigest != NoOperationInputDigest() {
		return false
	}
	return record.RunID == expected.RunID && record.Repository == expected.Repository && record.ExpectedState == expected.ExpectedState && record.RunIdempotencyKey == expected.RunIdempotencyKey && record.TransitionSequence == expected.TransitionSequence && record.ActionType == expected.ActionType && strings.EqualFold(record.Requester.ID, expected.Requester.ID) && record.Requester.DatabaseID == expected.Requester.DatabaseID && record.Requester.NodeID == expected.Requester.NodeID && record.Requester.ActorType == expected.Requester.ActorType && record.ReasonCode == expected.ReasonCode && record.AttentionEventKey == expected.AttentionEventKey
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
	requestDigest := input.RequestDigest
	if requestDigest == "" {
		requestDigest = NoOperationInputDigest()
	}
	authority := operatorActionAuthorityDigest(input)
	payload := struct {
		AuthorityDigest, ActionType, RequestDigest string
	}{authority, string(input.ActionType), requestDigest}
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	idempotencySum := sha256.Sum256([]byte("operator-action-idempotency:" + digest))
	idempotency := hex.EncodeToString(idempotencySum[:])
	return OperatorActionRecord{ActionID: "operator-action-" + idempotency[:24], IdempotencyKey: idempotency, PayloadDigest: digest, RequestDigest: requestDigest, RunID: input.RunID, Repository: input.Repository, ExpectedState: input.ExpectedState, RunIdempotencyKey: input.RunIdempotencyKey, TransitionSequence: input.TransitionSequence, ActionType: input.ActionType, Requester: input.Requester, ReasonCode: input.ReasonCode, AttentionEventKey: input.AttentionEventKey, ExpectedAuthorityDigest: authority, Status: OperatorActionStatusValidated, ResultStatus: OperatorActionResultPending, ReceivedAt: received, ValidatedAt: received}
}

func operatorActionAuthorityDigest(input OperatorActionInput) string {
	if validAuthorityDigest(input.ExpectedAuthorityDigest) {
		return input.ExpectedAuthorityDigest
	}
	return operatorActionAnchorDigest(input)
}

func operatorActionAnchorDigest(input OperatorActionInput) string {
	payload := struct {
		RunID, Repository, ExpectedState, RunKey, RequesterLogin, RequesterNode, RequesterType, Reason, EventKey string
		TransitionSequence, RequesterDatabaseID                                                                  int64
	}{input.RunID, input.Repository, string(input.ExpectedState), input.RunIdempotencyKey, strings.ToLower(input.Requester.ID), input.Requester.NodeID, input.Requester.ActorType, input.ReasonCode, input.AttentionEventKey, input.TransitionSequence, input.Requester.DatabaseID}
	raw, _ := json.Marshal(payload)
	return digestText("operator-action-authority-v1\x00" + string(raw))
}

func validateOperatorActionInput(input OperatorActionInput) error {
	if input.RunID == "" || input.Repository == "" || input.ExpectedState == "" || input.RunIdempotencyKey == "" || input.TransitionSequence < 1 || input.ReasonCode == "" || input.AttentionEventKey == "" {
		return errors.New("operator action authority is incomplete")
	}
	if !validOperationType(OperationType(input.ActionType)) {
		return errors.New("operator action type is invalid")
	}
	if input.RequestDigest != "" && !validAuthorityDigest(input.RequestDigest) {
		return errors.New("operator action request digest is invalid")
	}
	if input.ExpectedAuthorityDigest != "" && !validAuthorityDigest(input.ExpectedAuthorityDigest) {
		return errors.New("operator action expected authority digest is invalid")
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
	input := OperatorActionInput{Requester: record.Requester, RunID: record.RunID, Repository: record.Repository, ExpectedState: record.ExpectedState, RunIdempotencyKey: record.RunIdempotencyKey, TransitionSequence: record.TransitionSequence, ActionType: record.ActionType, ReasonCode: record.ReasonCode, AttentionEventKey: record.AttentionEventKey, RequestDigest: record.RequestDigest, ExpectedAuthorityDigest: record.ExpectedAuthorityDigest}
	expected := newOperatorActionRecord(input, record.ReceivedAt)
	legacy := record.RequestDigest == record.PayloadDigest && legacyOperatorActionIdentity(record)
	if err := validateOperatorActionInput(input); err != nil || !legacy && (expected.PayloadDigest != record.PayloadDigest || expected.IdempotencyKey != record.IdempotencyKey || expected.ActionID != record.ActionID) {
		return errors.New("operator action record authority is invalid")
	}
	if legacy {
		record.ExpectedAuthorityDigest = operatorActionAuthorityDigest(input)
	} else if record.ExpectedAuthorityDigest != expected.ExpectedAuthorityDigest {
		return errors.New("operator action expected authority is invalid")
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

func legacyOperatorActionIdentity(record OperatorActionRecord) bool {
	payload := struct {
		RunID, Repository, ExpectedState, RunKey, ActionType, RequesterLogin, RequesterNode, RequesterType, Reason, EventKey string
		TransitionSequence, RequesterDatabaseID                                                                              int64
	}{record.RunID, record.Repository, string(record.ExpectedState), record.RunIdempotencyKey, string(record.ActionType), strings.ToLower(record.Requester.ID), record.Requester.NodeID, record.Requester.ActorType, record.ReasonCode, record.AttentionEventKey, record.TransitionSequence, record.Requester.DatabaseID}
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	idempotencySum := sha256.Sum256([]byte("operator-action-idempotency:" + digest))
	idempotency := hex.EncodeToString(idempotencySum[:])
	return record.PayloadDigest == digest && record.IdempotencyKey == idempotency && record.ActionID == "operator-action-"+idempotency[:24]
}

func OperationReceiptForOperatorAction(record OperatorActionRecord, targetBindingDigest string) (OperationReceipt, error) {
	identity, err := record.Requester.githubUserIdentity()
	if err != nil {
		return OperationReceipt{}, err
	}
	requestDigest := record.RequestDigest
	if requestDigest == "" {
		requestDigest = record.PayloadDigest
	}
	authorityDigest := record.ExpectedAuthorityDigest
	if authorityDigest == "" {
		authorityDigest = operatorActionAuthorityDigest(OperatorActionInput{Requester: record.Requester, RunID: record.RunID, Repository: record.Repository, ExpectedState: record.ExpectedState, RunIdempotencyKey: record.RunIdempotencyKey, TransitionSequence: record.TransitionSequence, ActionType: record.ActionType, ReasonCode: record.ReasonCode, AttentionEventKey: record.AttentionEventKey, RequestDigest: requestDigest})
	}
	anchorDigest := operatorActionAnchorDigest(OperatorActionInput{Requester: record.Requester, RunID: record.RunID, Repository: record.Repository, ExpectedState: record.ExpectedState, RunIdempotencyKey: record.RunIdempotencyKey, TransitionSequence: record.TransitionSequence, ActionType: record.ActionType, ReasonCode: record.ReasonCode, AttentionEventKey: record.AttentionEventKey, RequestDigest: requestDigest})
	receipt := NewOperationReceipt(OperationReceiptInput{OperationType: OperationType(record.ActionType), Scope: ScopeRun, TargetID: record.RunID, Requester: identity, RequestDigest: requestDigest, ExpectedAuthorityDigest: authorityDigest, OperationAnchorDigest: anchorDigest, TargetBindingDigest: targetBindingDigest, AcceptedAt: record.ValidatedAt})
	switch record.Status {
	case OperatorActionStatusApplied:
		receipt.Phase = OperationPhaseApplied
		receipt.AppliedAt = record.AppliedAt
	case OperatorActionStatusObserved:
		receipt.Phase = OperationPhaseObserved
		receipt.AppliedAt = record.AppliedAt
		receipt.SettledAt = record.ObservedAt
	}
	receipt.ResultingState = string(record.ResultingState)
	receipt.ResultingVersion = record.ResultingTransitionSequence
	receipt.EvidenceDigest = record.EvidenceDigest
	receipt.ResultDigest = record.OutcomeDigest
	switch record.ResultStatus {
	case OperatorActionResultSucceeded:
		receipt.Outcome = OperationOutcomeSucceeded
	case OperatorActionResultFailed:
		receipt.Outcome = OperationOutcomeFailed
	case OperatorActionResultAmbiguous:
		receipt.Outcome = OperationOutcomeAmbiguous
	default:
		receipt.Outcome = OperationOutcomePending
	}
	if err := ValidateOperationReceipt(receipt); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
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
