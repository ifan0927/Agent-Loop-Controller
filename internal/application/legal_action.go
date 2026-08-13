package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type LegalActionConfirmation string

const (
	LegalActionConfirmationNone    LegalActionConfirmation = "none"
	LegalActionConfirmationInput   LegalActionConfirmation = "input"
	LegalActionConfirmationConfirm LegalActionConfirmation = "confirm"
	LegalActionConfirmationDanger  LegalActionConfirmation = "danger"
)

type LegalActionInputKind string

const (
	LegalActionInputNone     LegalActionInputKind = "none"
	LegalActionInputDecision LegalActionInputKind = "decision_v1"
)

type LegalActionConsequence string

const (
	LegalActionConsequenceResumeExecution     LegalActionConsequence = "resume_execution"
	LegalActionConsequenceScheduleRetry       LegalActionConsequence = "schedule_retry"
	LegalActionConsequenceTerminateRun        LegalActionConsequence = "terminate_run"
	LegalActionConsequenceRefreshCIEvidence   LegalActionConsequence = "refresh_ci_evidence"
	LegalActionConsequenceReturnToPushGate    LegalActionConsequence = "return_to_push_gate"
	LegalActionConsequenceAcceptExistingMerge LegalActionConsequence = "accept_existing_merge"
)

type LegalActionOffer struct {
	OfferID             string                  `json:"offer_id"`
	Action              OperationType           `json:"action"`
	Scope               AuthorityScopeKind      `json:"scope"`
	TargetID            string                  `json:"target_id"`
	RepositoryProfileID string                  `json:"repository_profile_id"`
	Reason              string                  `json:"reason"`
	Confirmation        LegalActionConfirmation `json:"confirmation"`
	InputKind           LegalActionInputKind    `json:"input_kind"`
	Consequence         LegalActionConsequence  `json:"consequence"`
	AuthorityDigest     string                  `json:"authority_digest"`
	EvidenceDigest      string                  `json:"evidence_digest"`
}

type LegalActionOfferQuery struct {
	Requester Requester `json:"requester"`
	RunID     string    `json:"run_id"`
}

type LegalActionExecutionCommand struct {
	Requester Requester `json:"requester"`
	OfferID   string    `json:"offer_id"`
}

type LegalDecisionInput struct {
	ChoiceID     string `json:"choice_id"`
	Instructions string `json:"instructions"`
}

type LegalActionStore interface {
	GetRunScopeAuthority(context.Context, string) (RunScopeAuthority, error)
	GetAuthorizedRun(context.Context, string, AuthorizedScopeSet) (Run, error)
	Inspect(context.Context, string) (RunInspection, error)
	CurrentOperatorAttention(context.Context, string) (OperatorAttentionEvent, bool, error)
}

type LegalActionService struct {
	store      LegalActionStore
	authorizer *AuthorizationService
}

func NewLegalActionService(store LegalActionStore, authorizer *AuthorizationService) (*LegalActionService, error) {
	if store == nil || authorizer == nil {
		return nil, errors.New("legal action dependencies are required")
	}
	return &LegalActionService{store: store, authorizer: authorizer}, nil
}

// ListLegalActionOffers is a persisted-state-only query. Its store boundary
// has no Git, GitHub, Linear, filesystem, process, or mutation capability.
func (s *LegalActionService) ListLegalActionOffers(ctx context.Context, query LegalActionOfferQuery) ([]LegalActionOffer, error) {
	if strings.TrimSpace(query.RunID) == "" {
		return nil, serviceError(ErrorInvalidInput, "run is required", nil)
	}
	configured, err := s.authorizer.ResolveConfiguredRequester(query.Requester)
	if err != nil {
		return nil, hiddenTargetError()
	}
	authority, err := s.store.GetRunScopeAuthority(ctx, query.RunID)
	if err != nil {
		if errors.Is(err, ErrRunNotFound) {
			return nil, hiddenTargetError()
		}
		return nil, classifyServiceError(err)
	}
	scopes, err := s.authorizer.RunScopes(configured, authority)
	if err != nil {
		return nil, hiddenTargetError()
	}
	run, err := s.store.GetAuthorizedRun(ctx, query.RunID, scopes)
	if err != nil {
		if errors.Is(err, ErrRunNotFound) {
			return nil, hiddenTargetError()
		}
		return nil, classifyServiceError(err)
	}
	return s.offersForAuthorizedRun(ctx, run, configured.identity)
}

// ResolveLegalActionOffer reauthorizes the configured operator, resolves an
// opaque offer without treating it as bearer authority, and recomputes every
// current offer before returning its run target to an action-specific service.
func (s *LegalActionService) ResolveLegalActionOffer(ctx context.Context, requester Requester, offerID string, expected OperationType) (LegalActionOffer, Run, error) {
	if strings.TrimSpace(offerID) == "" || !validOperationType(expected) {
		return LegalActionOffer{}, Run{}, serviceError(ErrorInvalidInput, "legal action offer is invalid", nil)
	}
	configured, err := s.authorizer.ResolveConfiguredRequester(requester)
	if err != nil {
		return LegalActionOffer{}, Run{}, hiddenTargetError()
	}
	targetID, decodedAction, bindingID, err := decodeLegalActionOfferID(offerID)
	if err != nil || decodedAction != expected {
		return LegalActionOffer{}, Run{}, serviceError(ErrorConflict, "legal action offer is stale or unavailable", nil)
	}
	authority, err := s.store.GetRunScopeAuthority(ctx, targetID)
	if err != nil {
		return LegalActionOffer{}, Run{}, serviceError(ErrorConflict, "legal action offer is stale or unavailable", nil)
	}
	scopes, err := s.authorizer.RunScopes(configured, authority)
	if err != nil {
		return LegalActionOffer{}, Run{}, hiddenTargetError()
	}
	run, err := s.store.GetAuthorizedRun(ctx, targetID, scopes)
	if err != nil {
		return LegalActionOffer{}, Run{}, serviceError(ErrorConflict, "legal action offer is stale or unavailable", nil)
	}
	offers, err := s.offersForAuthorizedRun(ctx, run, configured.identity)
	if err != nil {
		return LegalActionOffer{}, Run{}, err
	}
	for _, offer := range offers {
		if offer.OfferID == offerID && offer.Action == expected {
			return offer, run, nil
		}
	}
	inspection, inspectErr := s.store.Inspect(ctx, run.ID)
	if inspectErr != nil {
		return LegalActionOffer{}, Run{}, classifyServiceError(inspectErr)
	}
	for index := len(inspection.OperatorActions) - 1; index >= 0; index-- {
		action := inspection.OperatorActions[index]
		if action.ActionType != OperatorActionType(expected) || !sameOperationRequester(action.Requester, configured.identity) || !validAuthorityDigest(action.ExpectedAuthorityDigest) {
			continue
		}
		expectedBinding := legalActionOfferBindingID(identityDigest(configured.identity), action.ExpectedAuthorityDigest, expected)
		if expectedBinding == bindingID && encodeLegalActionOfferID(run.ID, expected, identityDigest(configured.identity), action.ExpectedAuthorityDigest) == offerID && historicalActionReplayable(action, run, inspection) {
			return LegalActionOffer{OfferID: offerID, Action: expected, Scope: ScopeRun, TargetID: run.ID, RepositoryProfileID: run.ProfileID, AuthorityDigest: action.ExpectedAuthorityDigest}, run, nil
		}
	}
	return LegalActionOffer{}, Run{}, serviceError(ErrorConflict, "legal action offer is stale or unavailable", nil)
}

func historicalActionReplayable(action OperatorActionRecord, run Run, inspection RunInspection) bool {
	if action.Status == OperatorActionStatusApplied || action.Status == OperatorActionStatusObserved {
		return true
	}
	sequence := latestTransitionSequence(inspection.Timeline)
	if run.State == action.ExpectedState && sequence == action.TransitionSequence {
		return true
	}
	if len(inspection.Timeline) == 0 {
		return false
	}
	last := inspection.Timeline[len(inspection.Timeline)-1]
	switch action.ActionType {
	case OperatorActionDecide:
		return last.From == domain.StateAwaitingHumanDecision && last.To == domain.StateExecuting
	case OperatorActionAbandon:
		return last.To == domain.StateFailed && last.Reason == AutomaticAdmissionAbandonTransition
	case OperatorActionRecoverOwnedPush:
		return last.From == domain.StateManualIntervention && last.To == domain.StateApprovalReady && strings.HasPrefix(last.EvidenceReference, "recover_owned_push:") && last.BoundHead == run.CandidateHead
	case OperatorActionAcceptExternalMerge:
		return last.From == domain.StateManualIntervention && last.To == domain.StateAwaitingLinearCompletion && strings.HasPrefix(last.EvidenceReference, "external_merge:") && inspection.Merge != nil && inspection.Merge.Method == "external"
	default:
		return false
	}
}

func (s *LegalActionService) ExecuteDecision(ctx context.Context, command LegalActionExecutionCommand, input LegalDecisionInput, controller LocalRunController) (OperationReceipt, error) {
	if controller == nil || strings.TrimSpace(input.ChoiceID) == "" || strings.TrimSpace(input.Instructions) == "" || int64(len([]byte(input.ChoiceID))+len([]byte(input.Instructions))) > maxStructuredOutcomeBytes {
		return OperationReceipt{}, serviceError(ErrorInvalidInput, "decision input is invalid", nil)
	}
	offer, run, err := s.ResolveLegalActionOffer(ctx, command.Requester, command.OfferID, OperationDecide)
	if err != nil {
		return OperationReceipt{}, err
	}
	ctx = withLegalActionExecutionAuthority(ctx, offer)
	if receipt, action, found, lookupErr := s.actionReceiptForOffer(ctx, run, offer); lookupErr != nil {
		return OperationReceipt{}, lookupErr
	} else if found && action.Status == OperatorActionStatusObserved {
		return receipt, nil
	} else if found && run.State != domain.StateAwaitingHumanDecision {
		inspection, inspectErr := s.store.Inspect(ctx, run.ID)
		if inspectErr != nil {
			return OperationReceipt{}, classifyServiceError(inspectErr)
		}
		persisted, persistedFound, persistedErr := findPersistedDecision(inspection)
		if persistedErr != nil || !persistedFound || DecisionOperationInputDigest(persisted) != action.RequestDigest {
			return OperationReceipt{}, serviceError(ErrorConflict, "accepted decision outcome cannot be reconciled", persistedErr)
		}
		actions, actionsErr := s.operatorActionService()
		if actionsErr != nil {
			return OperationReceipt{}, serviceError(ErrorInternal, "decision operation receipt is unavailable", actionsErr)
		}
		if settleErr := settleLegalRecoveryAction(ctx, actions, action, run, latestTransitionSequence(inspection.Timeline), "decision"); settleErr != nil {
			return OperationReceipt{}, settleErr
		}
		return s.receiptForRunAction(ctx, run.ID, OperatorActionDecide)
	}
	store, ok := s.store.(RunStore)
	if !ok {
		return OperationReceipt{}, serviceError(ErrorInternal, "decision execution store is unavailable", nil)
	}
	decision := &Decision{ChoiceID: input.ChoiceID, Instructions: input.Instructions}
	if _, err := NewCommandService(controller, store).Continue(ctx, ContinueCommand{Requester: command.Requester, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey, Decision: decision}); err != nil {
		return OperationReceipt{}, err
	}
	return s.receiptForRunAction(ctx, run.ID, OperatorActionDecide)
}

func (s *LegalActionService) ExecuteRetry(ctx context.Context, command LegalActionExecutionCommand, revalidator OperatorRetryRevalidator) (OperationReceipt, error) {
	offer, run, err := s.ResolveLegalActionOffer(ctx, command.Requester, command.OfferID, OperationRetry)
	if err != nil {
		return OperationReceipt{}, err
	}
	ctx = withLegalActionExecutionAuthority(ctx, offer)
	if receipt, action, found, lookupErr := s.actionReceiptForOffer(ctx, run, offer); lookupErr != nil {
		return OperationReceipt{}, lookupErr
	} else if found && action.Status == OperatorActionStatusObserved {
		return receipt, nil
	}
	store, ok := s.store.(OperatorRetryStore)
	if !ok {
		return OperationReceipt{}, serviceError(ErrorInternal, "retry execution store is unavailable", nil)
	}
	service, err := NewOperatorRetryService(store, revalidator)
	if err != nil {
		return OperationReceipt{}, serviceError(ErrorInternal, "retry execution is unavailable", err)
	}
	if _, err := service.Retry(ctx, OperatorRetryCommand{Requester: command.Requester, RunID: run.ID}); err != nil {
		return OperationReceipt{}, err
	}
	return s.receiptForRunAction(ctx, run.ID, OperatorActionRetry)
}

func (s *LegalActionService) ExecuteAbandon(ctx context.Context, command LegalActionExecutionCommand, coordinator *ProductionCoordinator, cleanup CleanupPort, childStopper AutomaticAdmissionChildStopper, readers ...GitHubReadPort) (OperationReceipt, error) {
	if coordinator == nil {
		return OperationReceipt{}, serviceError(ErrorInternal, "abandon execution is unavailable", nil)
	}
	offer, run, err := s.ResolveLegalActionOffer(ctx, command.Requester, command.OfferID, OperationAbandon)
	if err != nil {
		return OperationReceipt{}, err
	}
	ctx = withLegalActionExecutionAuthority(ctx, offer)
	if receipt, action, found, lookupErr := s.actionReceiptForOffer(ctx, run, offer); lookupErr != nil {
		return OperationReceipt{}, lookupErr
	} else if found && action.Status == OperatorActionStatusObserved {
		return receipt, nil
	}
	if _, err := coordinator.Abandon(ctx, ProductionAbandonCommand{Requester: command.Requester, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, cleanup, childStopper, readers...); err != nil {
		return OperationReceipt{}, err
	}
	return s.receiptForRunAction(ctx, run.ID, OperatorActionAbandon)
}

func (s *LegalActionService) ExecuteCIWaitRecovery(ctx context.Context, command LegalActionExecutionCommand, revalidator CIWaitRecoveryLinearRevalidator, reader GitHubReadPort, local CIWaitLocalAuthorityPort) (OperationReceipt, error) {
	offer, run, err := s.ResolveLegalActionOffer(ctx, command.Requester, command.OfferID, OperationRecoverCIWait)
	if err != nil {
		return OperationReceipt{}, err
	}
	ctx = withLegalActionExecutionAuthority(ctx, offer)
	if receipt, action, found, lookupErr := s.actionReceiptForOffer(ctx, run, offer); lookupErr != nil {
		return OperationReceipt{}, lookupErr
	} else if found && action.Status == OperatorActionStatusObserved {
		return receipt, nil
	}
	store, ok := s.store.(CIWaitRecoveryStore)
	if !ok {
		return OperationReceipt{}, serviceError(ErrorInternal, "CI wait recovery store is unavailable", nil)
	}
	service, err := NewCIWaitRecoveryService(store, revalidator)
	if err != nil {
		return OperationReceipt{}, serviceError(ErrorInternal, "CI wait recovery is unavailable", err)
	}
	if _, err := service.Recover(ctx, CIWaitRecoveryCommand{Requester: command.Requester, RunID: run.ID}, reader, local); err != nil {
		return OperationReceipt{}, err
	}
	return s.receiptForRunAction(ctx, run.ID, OperatorActionRecoverCIWait)
}

func (s *LegalActionService) ExecuteOwnedPushRecovery(ctx context.Context, command LegalActionExecutionCommand, coordinator *ProductionCoordinator) (OperationReceipt, error) {
	if coordinator == nil {
		return OperationReceipt{}, serviceError(ErrorInternal, "owned push recovery is unavailable", nil)
	}
	offer, run, err := s.ResolveLegalActionOffer(ctx, command.Requester, command.OfferID, OperationRecoverOwnedPush)
	if err != nil {
		return OperationReceipt{}, err
	}
	ctx = withLegalActionExecutionAuthority(ctx, offer)
	if receipt, action, found, lookupErr := s.actionReceiptForOffer(ctx, run, offer); lookupErr != nil {
		return OperationReceipt{}, lookupErr
	} else if found && action.Status == OperatorActionStatusObserved {
		return receipt, nil
	} else if found && run.State == domain.StateApprovalReady {
		inspection, inspectErr := s.store.Inspect(ctx, run.ID)
		if inspectErr != nil {
			return OperationReceipt{}, classifyServiceError(inspectErr)
		}
		if len(inspection.Timeline) == 0 || inspection.Timeline[len(inspection.Timeline)-1].From != domain.StateManualIntervention || inspection.Timeline[len(inspection.Timeline)-1].To != domain.StateApprovalReady || !strings.HasPrefix(inspection.Timeline[len(inspection.Timeline)-1].EvidenceReference, "recover_owned_push:") || inspection.Timeline[len(inspection.Timeline)-1].BoundHead != run.CandidateHead {
			return OperationReceipt{}, serviceError(ErrorConflict, "accepted owned push recovery outcome cannot be reconciled", nil)
		}
		actions, actionsErr := s.operatorActionService()
		if actionsErr != nil {
			return OperationReceipt{}, serviceError(ErrorInternal, "owned push recovery receipt is unavailable", actionsErr)
		}
		if settleErr := settleLegalRecoveryAction(ctx, actions, action, run, latestTransitionSequence(inspection.Timeline), "recover-owned-push"); settleErr != nil {
			return OperationReceipt{}, settleErr
		}
		return s.receiptForRunAction(ctx, run.ID, OperatorActionRecoverOwnedPush)
	}
	if _, err := coordinator.RecoverOwnedPush(ctx, ProductionRecoverOwnedPushCommand{Requester: command.Requester, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}); err != nil {
		return OperationReceipt{}, err
	}
	return s.receiptForRunAction(ctx, run.ID, OperatorActionRecoverOwnedPush)
}

func (s *LegalActionService) ExecuteExternalMergeAcceptance(ctx context.Context, command LegalActionExecutionCommand, coordinator *ProductionCoordinator, validator ExternalMergeCandidateValidator, verifier ExternalMergeVerifier) (OperationReceipt, error) {
	if coordinator == nil {
		return OperationReceipt{}, serviceError(ErrorInternal, "external merge acceptance is unavailable", nil)
	}
	offer, run, err := s.ResolveLegalActionOffer(ctx, command.Requester, command.OfferID, OperationAcceptExternalMerge)
	if err != nil {
		return OperationReceipt{}, err
	}
	ctx = withLegalActionExecutionAuthority(ctx, offer)
	if receipt, action, found, lookupErr := s.actionReceiptForOffer(ctx, run, offer); lookupErr != nil {
		return OperationReceipt{}, lookupErr
	} else if found && action.Status == OperatorActionStatusObserved {
		return receipt, nil
	} else if found && run.State == domain.StateAwaitingLinearCompletion {
		inspection, inspectErr := s.store.Inspect(ctx, run.ID)
		if inspectErr != nil {
			return OperationReceipt{}, classifyServiceError(inspectErr)
		}
		if inspection.Merge == nil || inspection.Merge.Method != "external" || inspection.Merge.RunID != run.ID || inspection.Merge.PreMergeSHA != run.CandidateHead {
			return OperationReceipt{}, serviceError(ErrorConflict, "accepted external merge outcome cannot be reconciled", nil)
		}
		actions, actionsErr := s.operatorActionService()
		if actionsErr != nil {
			return OperationReceipt{}, serviceError(ErrorInternal, "external merge receipt is unavailable", actionsErr)
		}
		if settleErr := settleLegalRecoveryAction(ctx, actions, action, run, latestTransitionSequence(inspection.Timeline), "accept-external-merge"); settleErr != nil {
			return OperationReceipt{}, settleErr
		}
		return s.receiptForRunAction(ctx, run.ID, OperatorActionAcceptExternalMerge)
	}
	if _, err := coordinator.AcceptExternalMerge(ctx, ProductionAcceptExternalMergeCommand{Requester: command.Requester, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, validator, verifier); err != nil {
		return OperationReceipt{}, err
	}
	return s.receiptForRunAction(ctx, run.ID, OperatorActionAcceptExternalMerge)
}

func (s *LegalActionService) receiptForRunAction(ctx context.Context, runID string, actionType OperatorActionType) (OperationReceipt, error) {
	inspection, err := s.store.Inspect(ctx, runID)
	if err != nil {
		return OperationReceipt{}, classifyServiceError(err)
	}
	for index := len(inspection.OperatorActions) - 1; index >= 0; index-- {
		action := inspection.OperatorActions[index]
		if action.ActionType != actionType {
			continue
		}
		binding := inspection.Run.RepositoryBindingDigest
		if !validAuthorityDigest(binding) {
			binding = LegacyRunAuthorityDigest(inspection.Run.Repository)
		}
		receipt, err := OperationReceiptForOperatorAction(action, binding)
		if err != nil {
			return OperationReceipt{}, serviceError(ErrorInternal, "operation receipt projection is invalid", err)
		}
		return receipt, nil
	}
	return OperationReceipt{}, serviceError(ErrorInternal, "operation receipt was not persisted", nil)
}

func (s *LegalActionService) actionReceiptForOffer(ctx context.Context, run Run, offer LegalActionOffer) (OperationReceipt, OperatorActionRecord, bool, error) {
	inspection, err := s.store.Inspect(ctx, run.ID)
	if err != nil {
		return OperationReceipt{}, OperatorActionRecord{}, false, classifyServiceError(err)
	}
	for index := len(inspection.OperatorActions) - 1; index >= 0; index-- {
		action := inspection.OperatorActions[index]
		if action.ActionType != OperatorActionType(offer.Action) || action.ExpectedAuthorityDigest != offer.AuthorityDigest {
			continue
		}
		binding := run.RepositoryBindingDigest
		if !validAuthorityDigest(binding) {
			binding = LegacyRunAuthorityDigest(run.Repository)
		}
		receipt, projectionErr := OperationReceiptForOperatorAction(action, binding)
		if projectionErr != nil {
			return OperationReceipt{}, OperatorActionRecord{}, false, serviceError(ErrorInternal, "operation receipt projection is invalid", projectionErr)
		}
		return receipt, action, true, nil
	}
	return OperationReceipt{}, OperatorActionRecord{}, false, nil
}

func (s *LegalActionService) operatorActionService() (*OperatorActionService, error) {
	store, ok := s.store.(OperatorActionStore)
	if !ok {
		return nil, errors.New("operator action store is unavailable")
	}
	return NewOperatorActionService(store)
}

func (s *LegalActionService) offersForAuthorizedRun(ctx context.Context, run Run, requester domain.GitHubUserIdentity) ([]LegalActionOffer, error) {
	inspection, err := s.store.Inspect(ctx, run.ID)
	if err != nil {
		return nil, classifyServiceError(err)
	}
	if inspection.Run.ID != run.ID || inspection.Run.RepositoryBindingDigest != run.RepositoryBindingDigest || inspection.Run.State != run.State || inspection.Run.IdempotencyKey != run.IdempotencyKey {
		return nil, serviceError(ErrorConflict, "run authority changed while listing legal actions", nil)
	}
	event, found, err := s.store.CurrentOperatorAttention(ctx, run.ID)
	if err != nil {
		return nil, classifyServiceError(err)
	}
	if !found || event.RunID != run.ID || event.ControllerState != string(run.State) {
		return []LegalActionOffer{}, nil
	}
	actions := legalActionIDsForInspection(run, inspection, event)
	if len(actions) == 0 {
		return []LegalActionOffer{}, nil
	}
	authorityDigest := legalActionAuthorityDigest(run, inspection, event, requester)
	offers := make([]LegalActionOffer, 0, len(actions))
	for _, action := range actions {
		offer := legalActionOfferFor(action, run, event, authorityDigest, requester)
		if err := validateLegalActionOffer(offer); err != nil {
			return nil, serviceError(ErrorInternal, "legal action projection is invalid", err)
		}
		offers = append(offers, offer)
	}
	return offers, nil
}

func legalActionIDsForInspection(run Run, inspection RunInspection, event OperatorAttentionEvent) []OperatorAttentionActionID {
	if inspection.Run.ID != "" && inspection.Run.ID != run.ID || event.RunID != run.ID || event.ControllerState != string(run.State) {
		return []OperatorAttentionActionID{}
	}
	candidates := allowedOperatorAttentionActionsFor(event.EventType, event.ControllerState, event.ReasonCode, event.RetryFailureClass)
	if schedule, found := retryScheduleForPhase(inspection.RetrySchedules, AutomaticRetryPhaseForRun(run)); found && validateLegacyCIWaitRecovery(inspection, schedule) == nil {
		candidates = append(candidates, OperatorAttentionActionRecoverCIWait)
	}
	if event.EventType == OperatorAttentionManualIntervention && run.State == domain.StateManualIntervention {
		if validOwnedPushRecoveryOffer(run, inspection) {
			candidates = append(candidates, OperatorAttentionActionRecoverOwnedPush)
		}
		if _, err := validateExternalMergeEvidence(run, inspection); err == nil {
			candidates = append(candidates, OperatorAttentionActionAcceptExternalMerge)
		}
	}
	candidates = slices.Compact(candidates)
	result := make([]OperatorAttentionActionID, 0, len(candidates))
	for _, action := range candidates {
		switch action {
		case OperatorAttentionActionRetry:
			phase := AutomaticRetryPhaseForRun(run)
			schedule, found := retryScheduleForPhase(inspection.RetrySchedules, phase)
			if !found || validateOperatorRetryPlan(run, inspection, schedule) != nil {
				continue
			}
		case OperatorAttentionActionAbandon:
			if !GracefulAbandonState(run.State) {
				continue
			}
		case OperatorAttentionActionDecide:
			transition, err := latestHumanDecisionTransition(inspection)
			if err != nil || transition.Sequence != latestTransitionSequence(inspection.Timeline) || transition.To != run.State {
				continue
			}
		case OperatorAttentionActionRecoverCIWait:
			if event.EventType != OperatorAttentionCIWaitRecovery || !slices.Contains(event.AllowedActions, OperatorAttentionActionRecoverCIWait) {
				schedule, found := retryScheduleForPhase(inspection.RetrySchedules, AutomaticRetryPhaseForRun(run))
				if !found || validateLegacyCIWaitRecovery(inspection, schedule) != nil {
					continue
				}
			}
		case OperatorAttentionActionRecoverOwnedPush:
			if !validOwnedPushRecoveryOffer(run, inspection) {
				continue
			}
		case OperatorAttentionActionAcceptExternalMerge:
			if _, err := validateExternalMergeEvidence(run, inspection); err != nil {
				continue
			}
		default:
			continue
		}
		result = append(result, action)
	}
	sequence := latestTransitionSequence(inspection.Timeline)
	for index := len(inspection.OperatorActions) - 1; index >= 0; index-- {
		action := inspection.OperatorActions[index]
		if action.ExpectedState != run.State || action.TransitionSequence != sequence || action.AttentionEventKey != event.EventKey {
			continue
		}
		selected := OperatorAttentionActionID(action.ActionType)
		if slices.Contains(result, selected) {
			return []OperatorAttentionActionID{selected}
		}
		return []OperatorAttentionActionID{}
	}
	return result
}

func validOwnedPushRecoveryOffer(run Run, inspection RunInspection) bool {
	if run.State != domain.StateManualIntervention || len(inspection.Timeline) == 0 {
		return false
	}
	last := inspection.Timeline[len(inspection.Timeline)-1]
	if last.To != domain.StateManualIntervention || last.BoundHead != run.CandidateHead || strings.TrimSpace(run.CandidateHead) == "" {
		return false
	}
	pr, err := retainedOwnedPullRequest(inspection, run)
	return err == nil && pr != nil && pr.HeadSHA != run.CandidateHead
}

func legalActionAuthorityDigest(run Run, inspection RunInspection, event OperatorAttentionEvent, requester domain.GitHubUserIdentity) string {
	payload, _ := json.Marshal(struct {
		RunID, RepositoryBinding, State, RunAuthority, EventKey, EventPayload, EventEvidence, Requester string
		TransitionSequence                                                                              int64
	}{run.ID, run.RepositoryBindingDigest, string(run.State), digestText(run.IdempotencyKey), event.EventKey, event.PayloadDigest, event.EvidenceDigest, identityDigest(requester), latestTransitionSequence(inspection.Timeline)})
	return digestText("legal-action-authority-v1\x00" + string(payload))
}

func legalActionOfferFor(action OperatorAttentionActionID, run Run, event OperatorAttentionEvent, authorityDigest string, requester domain.GitHubUserIdentity) LegalActionOffer {
	offer := LegalActionOffer{Action: OperationType(action), Scope: ScopeRun, TargetID: run.ID, RepositoryProfileID: run.ProfileID, Reason: event.ReasonCode, AuthorityDigest: authorityDigest, EvidenceDigest: event.EvidenceDigest, InputKind: LegalActionInputNone}
	switch action {
	case OperatorAttentionActionDecide:
		offer.Reason, offer.Confirmation, offer.InputKind, offer.Consequence = "human_decision_required", LegalActionConfirmationInput, LegalActionInputDecision, LegalActionConsequenceResumeExecution
	case OperatorAttentionActionRetry:
		offer.Reason, offer.Confirmation, offer.Consequence = "retry_budget_exhausted", LegalActionConfirmationNone, LegalActionConsequenceScheduleRetry
	case OperatorAttentionActionAbandon:
		offer.Reason, offer.Confirmation, offer.Consequence = "graceful_abandon", LegalActionConfirmationDanger, LegalActionConsequenceTerminateRun
	case OperatorAttentionActionRecoverCIWait:
		offer.Reason, offer.Confirmation, offer.Consequence = "legacy_ci_topology_drift", LegalActionConfirmationConfirm, LegalActionConsequenceRefreshCIEvidence
	case OperatorAttentionActionRecoverOwnedPush:
		offer.Reason, offer.Confirmation, offer.Consequence = "owned_push_recovery", LegalActionConfirmationConfirm, LegalActionConsequenceReturnToPushGate
	case OperatorAttentionActionAcceptExternalMerge:
		offer.Reason, offer.Confirmation, offer.Consequence = "external_merge_recovery", LegalActionConfirmationConfirm, LegalActionConsequenceAcceptExistingMerge
	}
	offer.OfferID = encodeLegalActionOfferID(run.ID, OperationType(action), identityDigest(requester), authorityDigest)
	return offer
}

type legalActionOfferIdentity struct {
	Version   string        `json:"v"`
	TargetID  string        `json:"target"`
	Action    OperationType `json:"action"`
	BindingID string        `json:"binding"`
}

func encodeLegalActionOfferID(targetID string, action OperationType, requesterDigest, authorityDigest string) string {
	payload := legalActionOfferIdentity{Version: "v1", TargetID: targetID, Action: action, BindingID: legalActionOfferBindingID(requesterDigest, authorityDigest, action)}
	raw, _ := json.Marshal(payload)
	return "legal-offer-" + base64.RawURLEncoding.EncodeToString(raw)
}

func legalActionOfferBindingID(requesterDigest, authorityDigest string, action OperationType) string {
	return digestText("legal-action-offer-binding-v1\x00" + requesterDigest + "\x00" + authorityDigest + "\x00" + string(action))
}

func decodeLegalActionOfferID(value string) (string, OperationType, string, error) {
	if !strings.HasPrefix(value, "legal-offer-") {
		return "", "", "", errors.New("legal action offer identity is invalid")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, "legal-offer-"))
	if err != nil || len(raw) > 1024 {
		return "", "", "", errors.New("legal action offer identity is invalid")
	}
	var payload legalActionOfferIdentity
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil || payload.Version != "v1" || strings.TrimSpace(payload.TargetID) == "" || strings.ContainsRune(payload.TargetID, '\x00') || !validOperationType(payload.Action) || !validAuthorityDigest(payload.BindingID) {
		return "", "", "", errors.New("legal action offer identity is invalid")
	}
	return payload.TargetID, payload.Action, payload.BindingID, nil
}

func sameOperationRequester(requester Requester, identity domain.GitHubUserIdentity) bool {
	return strings.EqualFold(requester.ID, identity.Login) && requester.DatabaseID == identity.DatabaseID && requester.NodeID == identity.NodeID && requester.ActorType == identity.ActorType
}

func validateLegalActionOffer(offer LegalActionOffer) error {
	if !strings.HasPrefix(offer.OfferID, "legal-offer-") || !validOperationType(offer.Action) || offer.Scope != ScopeRun || strings.TrimSpace(offer.TargetID) == "" || strings.TrimSpace(offer.RepositoryProfileID) == "" || !validAuthorityDigest(offer.AuthorityDigest) || !validAuthorityDigest(offer.EvidenceDigest) {
		return errors.New("legal action offer authority is invalid")
	}
	if !slices.Contains([]LegalActionConfirmation{LegalActionConfirmationNone, LegalActionConfirmationInput, LegalActionConfirmationConfirm, LegalActionConfirmationDanger}, offer.Confirmation) || !slices.Contains([]LegalActionInputKind{LegalActionInputNone, LegalActionInputDecision}, offer.InputKind) || !slices.Contains([]LegalActionConsequence{LegalActionConsequenceResumeExecution, LegalActionConsequenceScheduleRetry, LegalActionConsequenceTerminateRun, LegalActionConsequenceRefreshCIEvidence, LegalActionConsequenceReturnToPushGate, LegalActionConsequenceAcceptExistingMerge}, offer.Consequence) {
		return errors.New("legal action offer presentation semantics are invalid")
	}
	return nil
}
