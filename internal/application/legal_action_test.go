package application

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type legalActionStoreFixture struct {
	run        Run
	inspection RunInspection
	event      OperatorAttentionEvent
	authority  RunScopeAuthority
	inspect    int
	attention  int
	listed     int
}

func (s *legalActionStoreFixture) GetRunScopeAuthority(_ context.Context, id string) (RunScopeAuthority, error) {
	if id != s.run.ID {
		return RunScopeAuthority{}, ErrRunNotFound
	}
	return s.authority, nil
}
func (s *legalActionStoreFixture) ListRunScopeAuthorities(context.Context) ([]RunScopeAuthority, error) {
	s.listed++
	return []RunScopeAuthority{s.authority}, nil
}
func (s *legalActionStoreFixture) GetAuthorizedRun(_ context.Context, id string, scopes AuthorizedScopeSet) (Run, error) {
	if id != s.run.ID || !scopes.AllowsRun(s.run.ID, s.run.RepositoryBindingDigest) {
		return Run{}, ErrRunNotFound
	}
	return s.run, nil
}
func (s *legalActionStoreFixture) Inspect(_ context.Context, id string) (RunInspection, error) {
	s.inspect++
	if id != s.run.ID {
		return RunInspection{}, ErrRunNotFound
	}
	s.inspection.Run = s.run
	return s.inspection, nil
}
func (s *legalActionStoreFixture) CurrentOperatorAttention(_ context.Context, id string) (OperatorAttentionEvent, bool, error) {
	s.attention++
	return s.event, id == s.run.ID, nil
}

func legalActionDecisionFixture(t *testing.T) (*legalActionStoreFixture, *AuthorizationService, Requester) {
	t.Helper()
	identity := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	repository := LocalRepository{ProfileID: "repository-profile:owner/repo", CanonicalRepository: "owner/repo", AllowedOperatorLogins: []string{identity.Login}, TrustedOperatorActors: []TrustedActorIdentity{{Login: identity.Login, DatabaseID: identity.DatabaseID, NodeID: identity.NodeID, Type: identity.ActorType}}}
	raw, _ := json.Marshal(repository)
	now := time.Date(2026, 8, 13, 1, 0, 0, 0, time.UTC)
	run := Run{ID: "run-legal", Repository: repository.CanonicalRepository, RepositoryConfigJSON: string(raw), ProfileID: repository.ProfileID, RepositoryBindingDigest: strings.Repeat("a", 64), IdempotencyKey: "private-run-key", State: domain.StateAwaitingHumanDecision}
	transition := Transition{Sequence: 3, From: domain.StateExecuting, To: domain.StateAwaitingHumanDecision, Reason: "decision required", EvidenceReference: "decision_request", CreatedAt: now}
	event, err := HumanDecisionAttentionEvent(run, transition)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := frozenRunScopeAuthority(run)
	if err != nil {
		t.Fatal(err)
	}
	authorizer, err := NewAuthorizationService(ConfiguredOperatorIdentity{User: identity})
	if err != nil {
		t.Fatal(err)
	}
	requester := Requester{ID: identity.Login, Kind: "github_login", DatabaseID: identity.DatabaseID, NodeID: identity.NodeID, ActorType: identity.ActorType}
	return &legalActionStoreFixture{run: run, inspection: RunInspection{Run: run, Timeline: []Transition{transition}}, event: event, authority: authority}, authorizer, requester
}

func TestListLegalActionOffersIsAuthorizedStableSanitizedAndPersistedStateOnly(t *testing.T) {
	store, authorizer, requester := legalActionDecisionFixture(t)
	service, err := NewLegalActionService(store, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.ListLegalActionOffers(context.Background(), LegalActionOfferQuery{Requester: requester, RunID: store.run.ID})
	if err != nil || len(first) != 1 {
		t.Fatalf("offers=%+v err=%v", first, err)
	}
	second, err := service.ListLegalActionOffers(context.Background(), LegalActionOfferQuery{Requester: requester, RunID: store.run.ID})
	if err != nil || len(second) != 1 || first[0] != second[0] {
		t.Fatalf("first=%+v second=%+v err=%v", first, second, err)
	}
	offer := first[0]
	if offer.Action != OperationDecide || offer.Confirmation != LegalActionConfirmationInput || offer.InputKind != LegalActionInputDecision || offer.Consequence != LegalActionConsequenceResumeExecution || offer.Reason != "human_decision_required" {
		t.Fatalf("offer=%+v", offer)
	}
	raw, _ := json.Marshal(offer)
	for _, forbidden := range []string{"private-run-key", "idempotency_key", "expected_state", "target_state", "transition_sequence", "instructions", "filesystem", "session"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("offer leaked %q: %s", forbidden, raw)
		}
	}
	resolved, run, err := service.ResolveLegalActionOffer(context.Background(), requester, offer.OfferID, OperationDecide)
	if err != nil || resolved != offer || run.ID != store.run.ID {
		t.Fatalf("resolved=%+v run=%+v err=%v", resolved, run, err)
	}
	if store.inspect < 3 || store.attention < 3 {
		t.Fatalf("persisted state reads were skipped: inspect=%d attention=%d", store.inspect, store.attention)
	}
}

func TestLegalActionOfferPossessionDoesNotBypassAuthorizationOrStaleness(t *testing.T) {
	store, authorizer, requester := legalActionDecisionFixture(t)
	service, _ := NewLegalActionService(store, authorizer)
	offers, _ := service.ListLegalActionOffers(context.Background(), LegalActionOfferQuery{Requester: requester, RunID: store.run.ID})
	denied := requester
	denied.DatabaseID++
	if _, _, err := service.ResolveLegalActionOffer(context.Background(), denied, offers[0].OfferID, OperationDecide); err == nil {
		t.Fatal("offer possession bypassed configured operator identity")
	}
	store.run.State = domain.StateExecuting
	store.inspection.Run = store.run
	if _, _, err := service.ResolveLegalActionOffer(context.Background(), requester, offers[0].OfferID, OperationDecide); err == nil {
		t.Fatal("stale offer survived run transition")
	}
	store.run.State = domain.StateAwaitingHumanDecision
	store.inspection.Run = store.run
	store.event.EventKey += "-superseded"
	store.event.PayloadDigest = OperatorAttentionPayloadDigest(store.event)
	if _, _, err := service.ResolveLegalActionOffer(context.Background(), requester, offers[0].OfferID, OperationDecide); err == nil {
		t.Fatal("superseded attention survived offer recomputation")
	}
}

func TestExactOperationReplayResolvesWithoutReadingSiblingAuthorities(t *testing.T) {
	store, authorizer, requester := legalActionDecisionFixture(t)
	service, _ := NewLegalActionService(store, authorizer)
	offers, err := service.ListLegalActionOffers(context.Background(), LegalActionOfferQuery{Requester: requester, RunID: store.run.ID})
	if err != nil || len(offers) != 1 {
		t.Fatalf("offers=%+v err=%v", offers, err)
	}
	action := newOperatorActionRecord(OperatorActionInput{Requester: requester, RunID: store.run.ID, Repository: store.run.Repository, ExpectedState: store.run.State, RunIdempotencyKey: store.run.IdempotencyKey, TransitionSequence: 3, ActionType: OperatorActionDecide, ReasonCode: store.event.ReasonCode, AttentionEventKey: store.event.EventKey, RequestDigest: DecisionOperationInputDigest(Decision{ChoiceID: "choice", Instructions: "bounded"}), ExpectedAuthorityDigest: offers[0].AuthorityDigest}, store.inspection.Timeline[0].CreatedAt.Add(time.Second))
	action.Status, action.ResultStatus = OperatorActionStatusObserved, OperatorActionResultSucceeded
	action.ResultingState, action.ResultingTransitionSequence = domain.StateExecuting, 4
	action.EvidenceDigest, action.OutcomeDigest = strings.Repeat("b", 64), strings.Repeat("c", 64)
	action.AppliedAt, action.ObservedAt = action.ValidatedAt.Add(time.Second), action.ValidatedAt.Add(2*time.Second)
	store.inspection.OperatorActions = []OperatorActionRecord{action}
	store.run.State = domain.StateExecuting
	store.inspection.Run = store.run
	store.inspection.Timeline = append(store.inspection.Timeline, Transition{Sequence: 4, From: domain.StateAwaitingHumanDecision, To: domain.StateExecuting, Reason: "accepted decision", CreatedAt: action.AppliedAt})
	resolved, run, err := service.ResolveLegalActionOffer(context.Background(), requester, offers[0].OfferID, OperationDecide)
	if err != nil || resolved.OfferID != offers[0].OfferID || resolved.AuthorityDigest != action.ExpectedAuthorityDigest || run.State != domain.StateExecuting {
		t.Fatalf("resolved=%+v run=%+v err=%v", resolved, run, err)
	}
	if store.listed != 0 {
		t.Fatalf("offer replay read %d sibling authority collections", store.listed)
	}
}

func TestRecoveryOffersAndAttentionUseTheSameEligibilityPredicates(t *testing.T) {
	now := time.Date(2026, 8, 13, 1, 0, 0, 0, time.UTC)
	run := Run{ID: "run-owned-push", Repository: "owner/repo", ProfileID: "repository-profile:owner/repo", IdempotencyKey: "run-key", WorkingBranch: "ifan/108", BaseBranch: "main", BaseSHA: strings.Repeat("b", 40), CandidateHead: strings.Repeat("a", 40), State: domain.StateManualIntervention}
	transition := Transition{Sequence: 9, From: domain.StateRepairing, To: domain.StateManualIntervention, Reason: "repair push requires operator recovery", BoundHead: run.CandidateHead, CreatedAt: now}
	pr := domain.PullRequest{Number: 7, DatabaseID: 70, URL: "https://example.invalid/pull/7", NodeID: "PR_7", HeadBranch: run.WorkingBranch, BaseBranch: run.BaseBranch, HeadSHA: strings.Repeat("c", 40), BaseSHA: run.BaseSHA, BodyDigest: "body", OwnershipKey: run.IdempotencyKey, State: "open"}
	inspection := RunInspection{Run: run, Timeline: []Transition{transition}, PullRequest: &pr}
	event := OperatorAttentionEvent{RunID: run.ID, EventType: OperatorAttentionManualIntervention, ControllerState: string(run.State), ReasonCode: "owned_push_recovery"}
	actions := legalActionIDsForInspection(run, inspection, event)
	if !slices.Contains(actions, OperatorAttentionActionAbandon) || !slices.Contains(actions, OperatorAttentionActionRecoverOwnedPush) || slices.Contains(actions, OperatorAttentionActionAcceptExternalMerge) {
		t.Fatalf("actions=%v", actions)
	}
	event.EventKey = "automation:" + run.ID + ":manual_intervention:9"
	event.AllowedActions = []OperatorAttentionActionID{OperatorAttentionActionAbandon}
	inspection.OperatorAttention = []OperatorAttentionEvent{event}
	projected := projectInspection(inspection, event.EventKey)
	if len(projected.OperatorAttentionEvents) != 1 || !slices.Equal(projected.OperatorAttentionEvents[0].AllowedActions, actions) {
		t.Fatalf("projected=%+v legal=%v", projected.OperatorAttentionEvents, actions)
	}
	pr.HeadSHA = run.CandidateHead
	inspection.PullRequest = &pr
	actions = legalActionIDsForInspection(run, inspection, event)
	if slices.Contains(actions, OperatorAttentionActionRecoverOwnedPush) {
		t.Fatalf("already-pushed candidate was advertised: %v", actions)
	}
}

func TestUnauthorizedAndUnknownLegalActionTargetsAreNonDisclosing(t *testing.T) {
	store, authorizer, requester := legalActionDecisionFixture(t)
	service, _ := NewLegalActionService(store, authorizer)
	_, unknownErr := service.ListLegalActionOffers(context.Background(), LegalActionOfferQuery{Requester: requester, RunID: "unknown"})
	denied := requester
	denied.NodeID = "USER_other"
	_, deniedErr := service.ListLegalActionOffers(context.Background(), LegalActionOfferQuery{Requester: denied, RunID: store.run.ID})
	var missing, unauthorized *ServiceError
	if !errors.As(unknownErr, &missing) || !errors.As(deniedErr, &unauthorized) || missing.Category != ErrorNotFound || unauthorized.Category != ErrorNotFound || unknownErr.Error() != deniedErr.Error() {
		t.Fatalf("unknown=%v denied=%v", unknownErr, deniedErr)
	}
}

func TestAllLegalActionOffersHaveStablePresentationSemantics(t *testing.T) {
	identity := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	run := Run{ID: "run", ProfileID: "repository-profile:owner/repo"}
	event := OperatorAttentionEvent{ReasonCode: "attention", EvidenceDigest: strings.Repeat("e", 64)}
	authority := strings.Repeat("a", 64)
	tests := []struct {
		action       OperatorAttentionActionID
		confirmation LegalActionConfirmation
		input        LegalActionInputKind
		consequence  LegalActionConsequence
	}{
		{OperatorAttentionActionDecide, LegalActionConfirmationInput, LegalActionInputDecision, LegalActionConsequenceResumeExecution},
		{OperatorAttentionActionRetry, LegalActionConfirmationNone, LegalActionInputNone, LegalActionConsequenceScheduleRetry},
		{OperatorAttentionActionAbandon, LegalActionConfirmationDanger, LegalActionInputNone, LegalActionConsequenceTerminateRun},
		{OperatorAttentionActionRecoverOwnedPush, LegalActionConfirmationConfirm, LegalActionInputNone, LegalActionConsequenceReturnToPushGate},
		{OperatorAttentionActionAcceptExternalMerge, LegalActionConfirmationConfirm, LegalActionInputNone, LegalActionConsequenceAcceptExistingMerge},
	}
	for _, test := range tests {
		offer := legalActionOfferFor(test.action, run, event, authority, identity)
		if err := validateLegalActionOffer(offer); err != nil || offer.Action != OperationType(test.action) || offer.Confirmation != test.confirmation || offer.InputKind != test.input || offer.Consequence != test.consequence {
			t.Fatalf("action=%s offer=%+v err=%v", test.action, offer, err)
		}
	}
}

func TestAcceptedActionSuppressesMutuallyExclusiveOffersAndAttentionActions(t *testing.T) {
	store, authorizer, requester := legalActionDecisionFixture(t)
	store.run.State = domain.StateProvisioning
	store.inspection.Run = store.run
	store.inspection.Timeline = []Transition{{Sequence: 4, From: domain.StateAdmitting, To: domain.StateProvisioning, Reason: "provisioning", CreatedAt: time.Date(2026, 8, 13, 1, 0, 0, 0, time.UTC)}}
	store.inspection.Attempts = []Attempt{{ID: 1, RunID: store.run.ID, Status: "failed", ErrorCategory: RetryReasonProcessStart, FinishedAt: time.Date(2026, 8, 13, 0, 59, 0, 0, time.UTC)}}
	schedule := RetrySchedule{RunID: store.run.ID, Phase: AutomaticRetryPhaseForRun(store.run), ControllerState: string(store.run.State), AttemptCount: 4, MaxAttempts: 3, InitialDelay: time.Second, MaximumDelay: 30 * time.Second, FailureClass: RetryFailureProcessStart, FailureEvidenceRef: "attempt:1", ReasonCode: RetryReasonBudgetExhausted, Status: RetryScheduleAttention, AttentionAt: time.Date(2026, 8, 13, 1, 0, 0, 0, time.UTC), CreatedAt: time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 8, 13, 1, 0, 0, 0, time.UTC)}
	store.inspection.RetrySchedules = []RetrySchedule{schedule}
	event, err := AutomaticRetryAttentionEvent(store.run, schedule)
	if err != nil {
		t.Fatal(err)
	}
	store.event = event
	action := newOperatorActionRecord(OperatorActionInput{Requester: requester, RunID: store.run.ID, Repository: store.run.Repository, ExpectedState: store.run.State, RunIdempotencyKey: store.run.IdempotencyKey, TransitionSequence: 4, ActionType: OperatorActionRetry, ReasonCode: store.event.ReasonCode, AttentionEventKey: store.event.EventKey}, schedule.AttentionAt.Add(time.Second))
	store.inspection.OperatorActions = []OperatorActionRecord{action}
	service, err := NewLegalActionService(store, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	offers, err := service.ListLegalActionOffers(context.Background(), LegalActionOfferQuery{Requester: requester, RunID: store.run.ID})
	if err != nil || len(offers) != 1 || offers[0].Action != OperationRetry {
		t.Fatalf("offers=%+v err=%v", offers, err)
	}
	actions := legalActionIDsForInspection(store.run, store.inspection, store.event)
	if !slices.Equal(actions, []OperatorAttentionActionID{OperatorAttentionActionRetry}) {
		t.Fatalf("actions=%v", actions)
	}
}
