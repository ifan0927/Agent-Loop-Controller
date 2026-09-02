package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type routineAttentionStoreFixture struct {
	*legalActionStoreFixture
	candidates      []OperatorAttentionEvent
	query           RoutineAttentionCandidateQuery
	controllerQuery ControllerAttentionCandidateQuery
}

var (
	_ RoutineAttentionTargetCandidateStore = (*routineAttentionStoreFixture)(nil)
	_ ControllerAttentionCandidateStore    = (*routineAttentionStoreFixture)(nil)
	_ RoutineAttentionCandidateStore       = (*routineAttentionStoreFixture)(nil)
)

func (s *routineAttentionStoreFixture) ListControllerAttentionCandidates(_ context.Context, query ControllerAttentionCandidateQuery) ([]OperatorAttentionEvent, error) {
	s.controllerQuery = query
	return append([]OperatorAttentionEvent(nil), s.candidates...), nil
}

func (s *routineAttentionStoreFixture) ListRoutineAttentionCandidates(_ context.Context, query RoutineAttentionCandidateQuery) ([]OperatorAttentionEvent, error) {
	s.query = query
	return append([]OperatorAttentionEvent(nil), s.candidates...), nil
}

func (s *routineAttentionStoreFixture) ListAuthorizedRuns(context.Context, AuthorizedRunQuery) (AuthorizedRunPage, error) {
	return AuthorizedRunPage{}, nil
}

func (s *routineAttentionStoreFixture) ListControllerRuns(context.Context, ControllerRunQuery) (AuthorizedRunPage, error) {
	return AuthorizedRunPage{}, nil
}

func (s *routineAttentionStoreFixture) ListOperatorAttention(context.Context, OperatorAttentionQueryInput) ([]OperatorAttentionEvent, error) {
	return append([]OperatorAttentionEvent(nil), s.candidates...), nil
}

type routineAttentionProfiles struct{ profile RepositoryProfileAuthority }

func controllerReadAuthority(t *testing.T, authorizer *AuthorizationService, requester Requester) ControllerReadAuthority {
	t.Helper()
	configured, err := authorizer.ResolveConfiguredRequester(requester)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := authorizer.ControllerReadCollectionAuthority(configured)
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func (s routineAttentionProfiles) RepositoryProfile(_ context.Context, repository string) (RepositoryProfileAuthority, bool, error) {
	return s.profile, repository == s.profile.Authority.Repository, nil
}

func (s routineAttentionProfiles) ListRepositoryProfiles(context.Context) ([]RepositoryProfileAuthority, error) {
	return []RepositoryProfileAuthority{s.profile}, nil
}

func TestRoutineAttentionBindsSanitizedOffersToExactTypedItem(t *testing.T) {
	legal, authorizer, requester := legalActionDecisionFixture(t)
	legal.run.IssueID = "IFAN-177"
	legal.inspection.Run = legal.run
	current := legal.event
	sibling, err := newOperatorAttentionEvent(operatorAttentionEventInput{
		ScopeID: current.RunID, RunID: current.RunID, EventType: OperatorAttentionRetry,
		Profile: OperatorAttentionProfile{ID: current.RepositoryProfileID, Name: current.RepositoryProfileName},
		State:   legal.run.State, Severity: "error", ReasonCode: RetryReasonBudgetExhausted,
		RetryFailureClass: RetryFailureProcessStart, EvidenceDigest: strings.Repeat("c", 64),
		OccurredAt: current.OccurredAt.Add(-time.Minute), ObservedAt: current.ObservedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	store := &routineAttentionStoreFixture{legalActionStoreFixture: legal, candidates: []OperatorAttentionEvent{current, sibling}}
	profile := RepositoryProfileAuthority{Authority: RepositoryAuthority{
		Repository: legal.run.Repository, ProfileID: legal.run.ProfileID, BindingDigest: legal.run.RepositoryBindingDigest,
		AllowedLogins: append([]string(nil), legal.authority.AllowedLogins...), TrustedOperators: append([]domain.GitHubUserIdentity(nil), legal.authority.TrustedOperators...),
	}}
	service, err := NewRoutineAttentionQueryService(store, store, authorizer, routineAttentionProfiles{profile: profile})
	if err != nil {
		t.Fatal(err)
	}
	observedAt := current.ObservedAt.Add(time.Minute)
	page, err := service.ListController(context.Background(), controllerReadAuthority(t, authorizer, requester), RoutineAttentionQuery{Requester: requester, Scope: ScopeController, Limit: 10}, observedAt)
	if err != nil {
		t.Fatal(err)
	}
	if page.Metadata.SchemaVersion != RoutineAttentionSchemaVersion || page.Collection.Total != 2 || len(page.Items) != 2 || page.Metadata.Digest == "" {
		t.Fatalf("page=%+v", page)
	}
	if !store.controllerQuery.Authority.Valid() || store.controllerQuery.Limit != maximumRoutineAttentionCandidates+1 || !store.query.Scopes.Empty() {
		t.Fatalf("candidate query did not use stable collection authority: controller=%+v scoped=%+v", store.controllerQuery, store.query)
	}
	byID := map[string]RoutineAttentionItem{}
	for _, item := range page.Items {
		byID[item.EventID] = item
	}
	decision := byID[routineAttentionEventID(current.EventKey)]
	if decision.EventType != OperatorAttentionHumanDecision || decision.RunID != legal.run.ID || decision.LinearIdentifier != legal.run.IssueID || decision.Repository != legal.run.Repository || decision.ControllerState != string(legal.run.State) || decision.AttentionState != RoutineAttentionActive || decision.Navigation != RoutineAttentionNavigationRunDetail || len(decision.Offers) != 1 || decision.Offers[0].Action != OperationDecide {
		t.Fatalf("decision item=%+v", decision)
	}
	if siblingItem := byID[routineAttentionEventID(sibling.EventKey)]; siblingItem.AttentionState != RoutineAttentionUnknown || siblingItem.Navigation != RoutineAttentionNavigationRunDetail || len(siblingItem.Offers) != 0 {
		t.Fatalf("sibling offer leaked: %+v", siblingItem)
	}
	raw, _ := json.Marshal(page)
	for _, forbidden := range []string{"authority_digest", "evidence_digest", current.EvidenceDigest, sibling.EvidenceDigest, current.EventKey, sibling.EventKey} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("private offer authority leaked in %s", raw)
		}
	}
}

func TestRoutineAttentionPaginationAndCursorVersionAreStable(t *testing.T) {
	legal, authorizer, requester := legalActionDecisionFixture(t)
	current := legal.event
	older := current
	older.EventKey = "automation:" + current.RunID + ":human_decision_attention:" + strings.Repeat("d", 64)
	older.EvidenceDigest = strings.Repeat("d", 64)
	older.OccurredAt = current.OccurredAt.Add(-time.Hour)
	older.ObservedAt = older.OccurredAt
	older.PayloadDigest = OperatorAttentionPayloadDigest(older)
	store := &routineAttentionStoreFixture{legalActionStoreFixture: legal, candidates: []OperatorAttentionEvent{current, older}}
	profile := RepositoryProfileAuthority{Authority: RepositoryAuthority{Repository: legal.run.Repository, ProfileID: legal.run.ProfileID, BindingDigest: legal.run.RepositoryBindingDigest, AllowedLogins: legal.authority.AllowedLogins, TrustedOperators: legal.authority.TrustedOperators}}
	service, _ := NewRoutineAttentionQueryService(store, store, authorizer, routineAttentionProfiles{profile: profile})
	reader := controllerReadAuthority(t, authorizer, requester)
	first, err := service.ListController(context.Background(), reader, RoutineAttentionQuery{Requester: requester, Scope: ScopeController, Limit: 1}, time.Now().UTC())
	if err != nil || len(first.Items) != 1 || first.Collection.NextCursor == "" || first.Collection.Total != 2 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	added, err := CandidateScanIncompleteAttentionEvent("new-unrelated-repository", OperatorAttentionProfile{ID: "repository-profile:owner/other", Name: "owner/other"}, "truncated", strings.Repeat("a", 64), current.OccurredAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	store.candidates = append(store.candidates, added)
	second, err := service.ListController(context.Background(), reader, RoutineAttentionQuery{Requester: requester, Scope: ScopeController, Limit: 1, Cursor: first.Collection.NextCursor}, time.Now().UTC())
	if err != nil || len(second.Items) != 1 || second.Items[0].EventID == first.Items[0].EventID || second.Collection.Total != 3 {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	legacyRaw, _ := json.Marshal(routineAttentionCursor{Version: RoutineAttentionSchemaVersion, AuthorityDigest: reader.Digest(), Scope: ScopeController, Severity: 2, OccurredAt: current.OccurredAt, EventID: current.EventKey})
	legacy := base64.RawURLEncoding.EncodeToString(legacyRaw)
	if _, err := service.ListController(context.Background(), reader, RoutineAttentionQuery{Requester: requester, Scope: ScopeController, Cursor: legacy}, time.Now().UTC()); err == nil {
		t.Fatal("pre-v3 Attention cursor was accepted")
	}
	if _, err := service.ListController(context.Background(), reader, RoutineAttentionQuery{Requester: requester, Scope: ScopeController, TargetID: controllerScopeID, Cursor: first.Collection.NextCursor}, time.Now().UTC()); err == nil {
		t.Fatal("target-drifted Attention cursor was accepted")
	}
	other := domain.GitHubUserIdentity{Login: "other", DatabaseID: 8, NodeID: "U_8", ActorType: "User"}
	otherAuthorizer, _ := NewAuthorizationService(ConfiguredOperatorIdentity{User: other})
	otherReader := controllerReadAuthority(t, otherAuthorizer, requesterForUser(other))
	if _, err := service.ListController(context.Background(), otherReader, RoutineAttentionQuery{Requester: requester, Scope: ScopeController, Cursor: first.Collection.NextCursor}, time.Now().UTC()); err == nil {
		t.Fatal("reader-drifted Attention cursor was accepted")
	}
}

func TestRoutineAttentionCandidateBoundFailsSafely(t *testing.T) {
	legal, authorizer, requester := legalActionDecisionFixture(t)
	candidates := make([]OperatorAttentionEvent, maximumRoutineAttentionCandidates+1)
	for index := range candidates {
		candidates[index] = legal.event
	}
	store := &routineAttentionStoreFixture{legalActionStoreFixture: legal, candidates: candidates}
	profile := RepositoryProfileAuthority{Authority: RepositoryAuthority{Repository: legal.run.Repository, ProfileID: legal.run.ProfileID, BindingDigest: legal.run.RepositoryBindingDigest, AllowedLogins: legal.authority.AllowedLogins, TrustedOperators: legal.authority.TrustedOperators}}
	service, _ := NewRoutineAttentionQueryService(store, store, authorizer, routineAttentionProfiles{profile: profile})
	if _, err := service.ListController(context.Background(), controllerReadAuthority(t, authorizer, requester), RoutineAttentionQuery{Requester: requester, Scope: ScopeController}, time.Now().UTC()); err == nil {
		t.Fatal("candidate safety bound silently truncated the inbox")
	}
}

func TestRoutineAttentionPreservesConflictUnknownUnfamiliarAndResolvedConclusions(t *testing.T) {
	legal, authorizer, requester := legalActionDecisionFixture(t)
	current := legal.event
	invalid := current
	invalid.EventKey = "automation:" + current.RunID + ":human_decision_attention:" + strings.Repeat("e", 64)
	invalid.EvidenceDigest = strings.Repeat("e", 64)
	invalid.PayloadDigest = strings.Repeat("f", 64)
	resolvedRun := legal.run
	resolvedRun.State = domain.StateManualIntervention
	resolved, err := ManualInterventionAttentionEvent(resolvedRun, Transition{Sequence: 4, From: domain.StateExecuting, To: domain.StateManualIntervention, Reason: "manual intervention", CreatedAt: current.OccurredAt.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	unfamiliar, err := newOperatorAttentionEvent(operatorAttentionEventInput{ScopeID: "future-family", EventType: "future_attention_family", Profile: OperatorAttentionProfile{ID: "automation", Name: "linear-todo-admission"}, State: "future_state", Severity: "future_severity", ReasonCode: "future_reason", EvidenceDigest: strings.Repeat("a", 64), OccurredAt: current.OccurredAt.Add(2 * time.Minute), ObservedAt: current.OccurredAt.Add(2 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	store := &routineAttentionStoreFixture{legalActionStoreFixture: legal, candidates: []OperatorAttentionEvent{current, invalid, resolved, unfamiliar}}
	profile := RepositoryProfileAuthority{Authority: RepositoryAuthority{Repository: legal.run.Repository, ProfileID: legal.run.ProfileID, BindingDigest: legal.run.RepositoryBindingDigest, AllowedLogins: legal.authority.AllowedLogins, TrustedOperators: legal.authority.TrustedOperators}}
	service, _ := NewRoutineAttentionQueryService(store, store, authorizer, routineAttentionProfiles{profile: profile})
	page, err := service.ListController(context.Background(), controllerReadAuthority(t, authorizer, requester), RoutineAttentionQuery{Requester: requester, Scope: ScopeController, Limit: 10}, unfamiliar.ObservedAt.Add(time.Minute))
	if err != nil || page.Collection.Total != 3 || len(page.Items) != 3 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	states := map[string]RoutineAttentionState{}
	for _, item := range page.Items {
		states[item.EventType+":"+item.EventID] = item.AttentionState
		if item.EventID == routineAttentionEventID(resolved.EventKey) {
			t.Fatal("authoritatively resolved candidate remained visible")
		}
	}
	if states[current.EventType+":"+routineAttentionEventID(current.EventKey)] != RoutineAttentionActive || states[invalid.EventType+":"+routineAttentionEventID(invalid.EventKey)] != RoutineAttentionConflict || states[unfamiliar.EventType+":"+routineAttentionEventID(unfamiliar.EventKey)] != RoutineAttentionActive || unfamiliar.EventType != "unknown" {
		t.Fatalf("states=%+v unfamiliar=%+v", states, unfamiliar)
	}
}

func TestRoutineAttentionSanitizesAllFiveLegalActionTypes(t *testing.T) {
	cases := []struct {
		action      OperationType
		consequence LegalActionConsequence
	}{
		{OperationDecide, LegalActionConsequenceResumeExecution},
		{OperationRetry, LegalActionConsequenceScheduleRetry},
		{OperationAbandon, LegalActionConsequenceTerminateRun},
		{OperationRecoverOwnedPush, LegalActionConsequenceReturnToPushGate},
		{OperationAcceptExternalMerge, LegalActionConsequenceAcceptExistingMerge},
	}
	for _, test := range cases {
		t.Run(string(test.action), func(t *testing.T) {
			offer := LegalActionOffer{OfferID: "opaque-offer", Action: test.action, Scope: ScopeRun, TargetID: "private-target", RepositoryProfileID: "private-profile", Reason: "safe_reason", Confirmation: LegalActionConfirmationDanger, InputKind: LegalActionInputNone, Consequence: test.consequence, AuthorityDigest: strings.Repeat("b", 64), EvidenceDigest: strings.Repeat("c", 64)}
			summary := routineAttentionOfferSummary(offer)
			if summary.OfferID != offer.OfferID || summary.Action != test.action || summary.Reason != offer.Reason || summary.Confirmation != offer.Confirmation || summary.InputKind != offer.InputKind || summary.Consequence != test.consequence {
				t.Fatalf("summary=%+v", summary)
			}
			raw, _ := json.Marshal(summary)
			for _, forbidden := range []string{offer.TargetID, offer.RepositoryProfileID, offer.AuthorityDigest, offer.EvidenceDigest, "authority_digest", "evidence_digest"} {
				if strings.Contains(string(raw), forbidden) {
					t.Fatalf("summary leaked %q: %s", forbidden, raw)
				}
			}
		})
	}
}

func TestRoutineAttentionProjectsCompleteElevenFamilyInventory(t *testing.T) {
	legal, authorizer, requester := legalActionDecisionFixture(t)
	now := time.Date(2026, 9, 2, 6, 0, 0, 0, time.UTC)
	profile := OperatorAttentionProfile{ID: "automation", Name: "linear-todo-admission"}
	cases := []struct {
		eventType string
		state     any
		reason    string
		failure   RetryFailureClass
	}{
		{OperatorAttentionSourceCheckoutSkipped, domain.StateFailed, string(SourceSyncReasonDirtySource), ""},
		{OperatorAttentionCandidatePriorityTie, "scan", "top_priority_tie", ""},
		{OperatorAttentionCandidateScan, "scan", "truncated", ""},
		{OperatorAttentionSchedulerLease, "scheduler", "lease_conflict", ""},
		{OperatorAttentionAdmissionAuthority, domain.StateManualIntervention, "admission_authority_conflict", ""},
		{OperatorAttentionRetry, domain.StateReceived, RetryReasonBudgetExhausted, RetryFailureProcessStart},
		{OperatorAttentionCleanupResidue, domain.StateFailed, "cleanup_residue", ""},
		{OperatorAttentionManualIntervention, domain.StateManualIntervention, "manual_intervention", ""},
		{OperatorAttentionHumanDecision, domain.StateAwaitingHumanDecision, "human_decision_required", ""},
		{OperatorAttentionCISlow, domain.StateReconcilingReviews, "ci_wait_slow", ""},
		{OperatorAttentionCIWaitRecovery, domain.StatePROpen, "legacy_ci_topology_drift", ""},
	}
	events := make([]OperatorAttentionEvent, 0, len(cases))
	for index, test := range cases {
		digestMarker := []string{"a", "b", "c", "d", "e", "f"}[index%6]
		event, err := newOperatorAttentionEvent(operatorAttentionEventInput{ScopeID: fmt.Sprintf("inventory-%d", index), EventType: test.eventType, Profile: profile, State: test.state, Severity: "warning", ReasonCode: test.reason, RetryFailureClass: test.failure, EvidenceDigest: strings.Repeat(digestMarker, 64), OccurredAt: now.Add(time.Duration(index) * time.Second), ObservedAt: now.Add(time.Duration(index) * time.Second)})
		if err != nil {
			t.Fatalf("%s: %v", test.eventType, err)
		}
		if test.eventType == OperatorAttentionCandidatePriorityTie {
			event.SchemaVersion = OperatorAttentionLegacySchemaVersion
			event.AllowedActions = []OperatorAttentionActionID{}
			event.PayloadDigest = strings.Repeat("d", 64)
			if err := ValidateLegacyOperatorAttentionEvent(event); err != nil {
				t.Fatalf("legacy %s: %v", test.eventType, err)
			}
		}
		events = append(events, event)
	}
	store := &routineAttentionStoreFixture{legalActionStoreFixture: legal, candidates: events}
	profileAuthority := RepositoryProfileAuthority{Authority: RepositoryAuthority{Repository: legal.run.Repository, ProfileID: legal.run.ProfileID, BindingDigest: legal.run.RepositoryBindingDigest, AllowedLogins: legal.authority.AllowedLogins, TrustedOperators: legal.authority.TrustedOperators}}
	service, _ := NewRoutineAttentionQueryService(store, store, authorizer, routineAttentionProfiles{profile: profileAuthority})
	page, err := service.ListController(context.Background(), controllerReadAuthority(t, authorizer, requester), RoutineAttentionQuery{Requester: requester, Scope: ScopeController, Limit: 25}, now.Add(time.Minute))
	if err != nil || len(page.Items) != len(cases) || page.Collection.Total != len(cases) {
		t.Fatalf("items=%d total=%d err=%v", len(page.Items), page.Collection.Total, err)
	}
	families := map[string]bool{}
	for _, item := range page.Items {
		families[item.EventType] = true
		if item.AttentionState != RoutineAttentionActive || item.Navigation != RoutineAttentionNavigationNone || len(item.Offers) != 0 {
			t.Fatalf("non-run inventory item=%+v", item)
		}
	}
	for _, test := range cases {
		if !families[test.eventType] {
			t.Fatalf("missing family %s", test.eventType)
		}
	}
}
