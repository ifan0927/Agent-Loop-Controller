package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

var (
	ErrOperationReceiptNotFound = errors.New("operation receipt not found")
	ErrOperationReceiptConflict = errors.New("operation receipt authority conflicts")
)

type OperationType string

const (
	OperationDecide               OperationType = "decide"
	OperationRetry                OperationType = "retry"
	OperationAbandon              OperationType = "abandon"
	OperationRecoverCIWait        OperationType = "recover_ci_wait"
	OperationRecoverOwnedPush     OperationType = "recover_owned_push"
	OperationAcceptExternalMerge  OperationType = "accept_external_merge"
	OperationApplyConfiguration   OperationType = "apply_configuration"
	OperationRestoreConfiguration OperationType = "restore_configuration"
)

type OperationPhase string

const (
	OperationPhaseAccepted OperationPhase = "accepted"
	OperationPhaseApplied  OperationPhase = "applied"
	OperationPhaseObserved OperationPhase = "observed"
)

type OperationOutcome string

const (
	OperationOutcomePending   OperationOutcome = "pending"
	OperationOutcomeSucceeded OperationOutcome = "succeeded"
	OperationOutcomeFailed    OperationOutcome = "failed"
	OperationOutcomeConflict  OperationOutcome = "conflict"
	OperationOutcomeAmbiguous OperationOutcome = "ambiguous"
)

// OperationReceipt is the sanitized, presentation-independent record for one
// Controller-authorized operator intent. Scope-neutral fields are required;
// run transition and attention details remain private action evidence.
type OperationReceipt struct {
	OperationID              string                    `json:"operation_id"`
	OperationType            OperationType             `json:"operation_type"`
	Scope                    AuthorityScopeKind        `json:"scope"`
	TargetID                 string                    `json:"target_id"`
	Requester                domain.GitHubUserIdentity `json:"requester"`
	RequestDigest            string                    `json:"request_digest"`
	ExpectedAuthorityDigest  string                    `json:"expected_authority_digest"`
	TargetBindingDigest      string                    `json:"target_binding_digest,omitempty"`
	Phase                    OperationPhase            `json:"phase"`
	Outcome                  OperationOutcome          `json:"outcome"`
	ResultingAuthorityDigest string                    `json:"resulting_authority_digest,omitempty"`
	ResultingState           string                    `json:"resulting_state,omitempty"`
	ResultingVersion         int64                     `json:"resulting_version,omitempty"`
	EvidenceDigest           string                    `json:"evidence_digest,omitempty"`
	ResultDigest             string                    `json:"result_digest,omitempty"`
	AcceptedAt               time.Time                 `json:"accepted_at"`
	AppliedAt                time.Time                 `json:"applied_at,omitempty"`
	SettledAt                time.Time                 `json:"settled_at,omitempty"`

	// AuthorityKey is a private uniqueness boundary shared by mutually
	// exclusive operations against one exact authority envelope.
	AuthorityKey          string `json:"-"`
	OperationAnchorDigest string `json:"-"`
}

type OperationReceiptInput struct {
	OperationType           OperationType
	Scope                   AuthorityScopeKind
	TargetID                string
	Requester               domain.GitHubUserIdentity
	RequestDigest           string
	ExpectedAuthorityDigest string
	OperationAnchorDigest   string
	TargetBindingDigest     string
	AcceptedAt              time.Time
}

type OperationReceiptMutation struct {
	OperationID              string
	ExpectedPhase            OperationPhase
	Phase                    OperationPhase
	Outcome                  OperationOutcome
	ResultingAuthorityDigest string
	ResultingState           string
	ResultingVersion         int64
	EvidenceDigest           string
	ResultDigest             string
	At                       time.Time
}

type OperationReceiptTarget struct {
	Scope               AuthorityScopeKind
	TargetID            string
	TargetBindingDigest string
}

type OperationReceiptStore interface {
	BeginOperationReceipt(context.Context, OperationReceipt) (OperationReceipt, bool, error)
	AdvanceOperationReceipt(context.Context, OperationReceiptMutation) (OperationReceipt, bool, error)
	GetOperationReceiptTarget(context.Context, string) (OperationReceiptTarget, error)
	GetAuthorizedOperationReceipt(context.Context, string, AuthorizedScopeSet) (OperationReceipt, error)
}

type OperationReceiptQueryStore interface {
	GetOperationReceiptTarget(context.Context, string) (OperationReceiptTarget, error)
	GetAuthorizedOperationReceipt(context.Context, string, AuthorizedScopeSet) (OperationReceipt, error)
	GetRunScopeAuthority(context.Context, string) (RunScopeAuthority, error)
}

type OnboardingAuthoritySource interface {
	OnboardingAuthority(context.Context, string) (OnboardingAuthority, bool, error)
}

type OperationReceiptQueryService struct {
	store        OperationReceiptQueryStore
	authorizer   *AuthorizationService
	repositories RepositoryAuthoritySource
	onboarding   OnboardingAuthoritySource
}

func NewOperationReceiptQueryService(store OperationReceiptQueryStore, authorizer *AuthorizationService, repositories RepositoryAuthoritySource, onboarding OnboardingAuthoritySource) (*OperationReceiptQueryService, error) {
	if store == nil || authorizer == nil {
		return nil, errors.New("operation receipt query dependencies are required")
	}
	return &OperationReceiptQueryService{store: store, authorizer: authorizer, repositories: repositories, onboarding: onboarding}, nil
}

func (s *OperationReceiptQueryService) Get(ctx context.Context, requester Requester, operationID string) (OperationReceipt, error) {
	if strings.TrimSpace(operationID) == "" {
		return OperationReceipt{}, serviceError(ErrorInvalidInput, "operation is required", nil)
	}
	configured, err := s.authorizer.ResolveConfiguredRequester(requester)
	if err != nil {
		return OperationReceipt{}, hiddenTargetError()
	}
	target, err := s.store.GetOperationReceiptTarget(ctx, operationID)
	if err != nil {
		if errors.Is(err, ErrOperationReceiptNotFound) {
			return OperationReceipt{}, hiddenTargetError()
		}
		return OperationReceipt{}, classifyServiceError(err)
	}
	var scopes AuthorizedScopeSet
	switch target.Scope {
	case ScopeController:
		scopes, err = s.authorizer.ControllerScopes(configured)
	case ScopeRun:
		var authority RunScopeAuthority
		authority, err = s.store.GetRunScopeAuthority(ctx, target.TargetID)
		if err == nil {
			scopes, err = s.authorizer.RunScopes(configured, authority)
		}
	case ScopeRepository:
		if s.repositories == nil {
			err = errors.New("repository operation authority is unavailable")
			break
		}
		var authority RepositoryAuthority
		var found bool
		authority, found, err = s.repositories.RepositoryAuthority(ctx, target.TargetID)
		if err == nil && !found {
			err = ErrOperationReceiptNotFound
		}
		if err == nil {
			scopes, err = s.authorizer.RepositoryScopes(configured, authority)
		}
	case ScopeOnboarding:
		if s.onboarding == nil {
			err = errors.New("onboarding operation authority is unavailable")
			break
		}
		var authority OnboardingAuthority
		var found bool
		authority, found, err = s.onboarding.OnboardingAuthority(ctx, target.TargetID)
		if err == nil && !found {
			err = ErrOperationReceiptNotFound
		}
		if err == nil {
			scopes, err = s.authorizer.OnboardingScopes(configured, authority)
		}
	default:
		err = errors.New("operation target scope is invalid")
	}
	if err != nil || !scopes.AllowsOperationTarget(target) {
		return OperationReceipt{}, hiddenTargetError()
	}
	receipt, err := s.store.GetAuthorizedOperationReceipt(ctx, operationID, scopes)
	if err != nil {
		if errors.Is(err, ErrOperationReceiptNotFound) {
			return OperationReceipt{}, hiddenTargetError()
		}
		return OperationReceipt{}, classifyServiceError(err)
	}
	return receipt, nil
}

type OperationReceiptService struct {
	store OperationReceiptStore
	now   func() time.Time
}

func NewOperationReceiptService(store OperationReceiptStore) (*OperationReceiptService, error) {
	if store == nil {
		return nil, errors.New("operation receipt store is required")
	}
	return &OperationReceiptService{store: store, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (s *OperationReceiptService) Accept(ctx context.Context, input OperationReceiptInput) (OperationReceipt, bool, error) {
	if input.AcceptedAt.IsZero() {
		input.AcceptedAt = s.now().UTC()
	}
	receipt := NewOperationReceipt(input)
	if err := ValidateOperationReceipt(receipt); err != nil {
		return OperationReceipt{}, false, serviceError(ErrorInvalidInput, "operation receipt input is invalid", err)
	}
	persisted, created, err := s.store.BeginOperationReceipt(ctx, receipt)
	if err != nil {
		if errors.Is(err, ErrOperationReceiptConflict) {
			return OperationReceipt{}, false, serviceError(ErrorConflict, "operation receipt authority changed", err)
		}
		return OperationReceipt{}, false, classifyServiceError(err)
	}
	return persisted, created, nil
}

func (s *OperationReceiptService) RecordApplied(ctx context.Context, mutation OperationReceiptMutation) (OperationReceipt, bool, error) {
	mutation.ExpectedPhase = OperationPhaseAccepted
	mutation.Phase = OperationPhaseApplied
	if mutation.Outcome == "" {
		mutation.Outcome = OperationOutcomePending
	}
	return s.advance(ctx, mutation)
}

func (s *OperationReceiptService) RecordSettled(ctx context.Context, mutation OperationReceiptMutation) (OperationReceipt, bool, error) {
	if mutation.Phase == "" {
		mutation.Phase = OperationPhaseObserved
	}
	return s.advance(ctx, mutation)
}

func (s *OperationReceiptService) advance(ctx context.Context, mutation OperationReceiptMutation) (OperationReceipt, bool, error) {
	if mutation.At.IsZero() {
		mutation.At = s.now().UTC()
	}
	if err := ValidateOperationReceiptMutation(mutation); err != nil {
		return OperationReceipt{}, false, serviceError(ErrorInvalidInput, "operation receipt result is invalid", err)
	}
	receipt, changed, err := s.store.AdvanceOperationReceipt(ctx, mutation)
	if err != nil {
		if errors.Is(err, ErrOperationReceiptConflict) {
			return OperationReceipt{}, false, serviceError(ErrorConflict, "operation receipt lifecycle changed", err)
		}
		return OperationReceipt{}, false, classifyServiceError(err)
	}
	return receipt, changed, nil
}

func NewOperationReceipt(input OperationReceiptInput) OperationReceipt {
	identity := normalizedOperationRequester(input.Requester)
	authorityPayload, _ := json.Marshal(struct {
		Scope, TargetID, Requester, OperationAnchorDigest string
	}{string(input.Scope), input.TargetID, identityDigest(identity), input.OperationAnchorDigest})
	authorityKey := digestText("operation-authority-v1\x00" + string(authorityPayload))
	identityPayload, _ := json.Marshal(struct {
		AuthorityKey, OperationType, RequestDigest, ExpectedAuthorityDigest string
	}{authorityKey, string(input.OperationType), input.RequestDigest, input.ExpectedAuthorityDigest})
	operationDigest := digestText("operation-identity-v1\x00" + string(identityPayload))
	return OperationReceipt{
		OperationID:             "operation-" + operationDigest[:32],
		OperationType:           input.OperationType,
		Scope:                   input.Scope,
		TargetID:                input.TargetID,
		Requester:               identity,
		RequestDigest:           input.RequestDigest,
		ExpectedAuthorityDigest: input.ExpectedAuthorityDigest,
		OperationAnchorDigest:   input.OperationAnchorDigest,
		TargetBindingDigest:     input.TargetBindingDigest,
		Phase:                   OperationPhaseAccepted,
		Outcome:                 OperationOutcomePending,
		AcceptedAt:              input.AcceptedAt.UTC(),
		AuthorityKey:            authorityKey,
	}
}

func ValidateOperationReceipt(receipt OperationReceipt) error {
	if !validOperationType(receipt.OperationType) || !validOperationScope(receipt.Scope) || strings.TrimSpace(receipt.TargetID) == "" || strings.ContainsRune(receipt.TargetID, '\x00') || receipt.Requester.Validate() != nil || !validAuthorityDigest(receipt.RequestDigest) || !validAuthorityDigest(receipt.ExpectedAuthorityDigest) || !validAuthorityDigest(receipt.OperationAnchorDigest) || !validAuthorityDigest(receipt.AuthorityKey) || receipt.AcceptedAt.IsZero() {
		return errors.New("operation receipt authority is invalid")
	}
	if !validAuthorityDigest(receipt.TargetBindingDigest) {
		return errors.New("operation target binding is invalid")
	}
	expected := NewOperationReceipt(OperationReceiptInput{OperationType: receipt.OperationType, Scope: receipt.Scope, TargetID: receipt.TargetID, Requester: receipt.Requester, RequestDigest: receipt.RequestDigest, ExpectedAuthorityDigest: receipt.ExpectedAuthorityDigest, OperationAnchorDigest: receipt.OperationAnchorDigest, TargetBindingDigest: receipt.TargetBindingDigest, AcceptedAt: receipt.AcceptedAt})
	if receipt.OperationID != expected.OperationID || receipt.AuthorityKey != expected.AuthorityKey {
		return errors.New("operation receipt identity is invalid")
	}
	if !validOperationPhase(receipt.Phase) || !validOperationOutcome(receipt.Outcome) || receipt.ResultingVersion < 0 || strings.ContainsRune(receipt.ResultingState, '\x00') {
		return errors.New("operation receipt lifecycle is invalid")
	}
	for _, digest := range []string{receipt.ResultingAuthorityDigest, receipt.EvidenceDigest, receipt.ResultDigest} {
		if digest != "" && !validAuthorityDigest(digest) {
			return errors.New("operation receipt result digest is invalid")
		}
	}
	if receipt.Phase == OperationPhaseAccepted && !receipt.AppliedAt.IsZero() || !receipt.AppliedAt.IsZero() && receipt.AppliedAt.Before(receipt.AcceptedAt) || !receipt.SettledAt.IsZero() && receipt.SettledAt.Before(receipt.AcceptedAt) {
		return errors.New("operation receipt timestamps are invalid")
	}
	if receipt.Phase == OperationPhaseApplied && receipt.AppliedAt.IsZero() || receipt.Phase == OperationPhaseObserved && (receipt.AppliedAt.IsZero() || receipt.SettledAt.IsZero()) {
		return errors.New("operation receipt phase timestamps are incomplete")
	}
	if receipt.Outcome == OperationOutcomePending && !receipt.SettledAt.IsZero() || receipt.Outcome != OperationOutcomePending && receipt.SettledAt.IsZero() {
		return errors.New("operation receipt settlement is invalid")
	}
	if receipt.Phase == OperationPhaseObserved && receipt.Outcome == OperationOutcomePending {
		return errors.New("observed operation cannot be pending")
	}
	return nil
}

func ValidateOperationReceiptMutation(mutation OperationReceiptMutation) error {
	if strings.TrimSpace(mutation.OperationID) == "" || !validOperationPhase(mutation.ExpectedPhase) || !validOperationPhase(mutation.Phase) || !validOperationOutcome(mutation.Outcome) || mutation.At.IsZero() || mutation.ResultingVersion < 0 || strings.ContainsRune(mutation.ResultingState, '\x00') {
		return errors.New("operation receipt mutation is invalid")
	}
	if mutation.ExpectedPhase == OperationPhaseAccepted && mutation.Phase != OperationPhaseApplied && mutation.Phase != OperationPhaseAccepted || mutation.ExpectedPhase == OperationPhaseApplied && mutation.Phase != OperationPhaseObserved && mutation.Phase != OperationPhaseApplied || mutation.ExpectedPhase == OperationPhaseObserved {
		return errors.New("operation receipt phase is not monotonic")
	}
	if mutation.Phase == OperationPhaseObserved && mutation.Outcome == OperationOutcomePending || mutation.Phase != OperationPhaseObserved && mutation.Outcome == OperationOutcomeSucceeded {
		return errors.New("operation receipt outcome does not match phase")
	}
	for _, digest := range []string{mutation.ResultingAuthorityDigest, mutation.EvidenceDigest, mutation.ResultDigest} {
		if digest != "" && !validAuthorityDigest(digest) {
			return errors.New("operation receipt mutation digest is invalid")
		}
	}
	return nil
}

func validOperationType(value OperationType) bool {
	switch value {
	case OperationDecide, OperationRetry, OperationAbandon, OperationRecoverCIWait, OperationRecoverOwnedPush, OperationAcceptExternalMerge, OperationApplyConfiguration, OperationRestoreConfiguration:
		return true
	default:
		return false
	}
}

func validOperationScope(value AuthorityScopeKind) bool {
	switch value {
	case ScopeController, ScopeRepository, ScopeRun, ScopeOnboarding:
		return true
	default:
		return false
	}
}

func validOperationPhase(value OperationPhase) bool {
	return value == OperationPhaseAccepted || value == OperationPhaseApplied || value == OperationPhaseObserved
}

func validOperationOutcome(value OperationOutcome) bool {
	switch value {
	case OperationOutcomePending, OperationOutcomeSucceeded, OperationOutcomeFailed, OperationOutcomeConflict, OperationOutcomeAmbiguous:
		return true
	default:
		return false
	}
}

func normalizedOperationRequester(value domain.GitHubUserIdentity) domain.GitHubUserIdentity {
	value.Login = strings.ToLower(strings.TrimSpace(value.Login))
	return value
}

func NoOperationInputDigest() string { return digestText("operation-input-v1:none") }
