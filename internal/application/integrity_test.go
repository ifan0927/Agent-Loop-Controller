package application

import (
	"context"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type integrityQueryStoreStub struct{ calls int }

func (s *integrityQueryStoreStub) IntegritySummary(context.Context, AuthorizedScopeSet) (IntegritySummary, error) {
	s.calls++
	return IntegritySummary{Readiness: IntegrityUnknown, ReasonCode: "initial_scan_required"}, nil
}

func (s *integrityQueryStoreStub) ListIntegrityFindings(context.Context, AuthorizedScopeSet, IntegrityFindingQuery) (IntegrityFindingPage, error) {
	s.calls++
	return IntegrityFindingPage{}, nil
}

func TestIntegrityRegistryAndAggregateAreClosed(t *testing.T) {
	want := []IntegrityFamily{IntegrityStorageSchema, IntegrityRunDelivery, IntegrityOperationActivity, IntegrityConfiguration, IntegrityRepositoryOnboarding, IntegritySchedulingAdmission, IntegrityOwnedResourceCleanup}
	got := IntegrityFamilies()
	if len(got) != len(want) {
		t.Fatalf("families=%v", got)
	}
	for index := range want {
		if got[index] != want[index] || !got[index].Valid() {
			t.Fatalf("families=%v", got)
		}
	}
	got[0] = "mutated"
	if IntegrityFamilies()[0] != IntegrityStorageSchema {
		t.Fatal("registry escaped through mutable slice")
	}
	for _, test := range []struct {
		states []IntegrityState
		want   IntegrityState
	}{
		{[]IntegrityState{IntegrityReady, IntegrityNotReady}, IntegrityNotReady},
		{[]IntegrityState{IntegrityNotReady, IntegrityUnknown}, IntegrityUnknown},
		{[]IntegrityState{IntegrityUnknown, IntegrityConflict}, IntegrityConflict},
		{[]IntegrityState{IntegrityReady, IntegrityReady}, IntegrityReady},
	} {
		if got := AggregateIntegrity(test.states...); got != test.want {
			t.Fatalf("aggregate %v=%s want=%s", test.states, got, test.want)
		}
	}
}

func TestIntegrityObservationRequiresEveryFamilyExactlyOnce(t *testing.T) {
	results := make([]IntegrityFamilyResult, 0, len(IntegrityFamilies()))
	for _, family := range IntegrityFamilies() {
		results = append(results, IntegrityFamilyResult{Family: family, State: IntegrityReady, ReasonCode: "complete", CountComplete: true, CoverageComplete: true})
	}
	observation := IntegrityObservation{SchemaVersion: IntegritySchemaVersion, RegistryVersion: IntegrityRegistryVersion, ObservationID: "observation", Digest: string(make([]byte, 64)), ObservedAt: time.Now().UTC(), EffectiveReadiness: IntegrityReady, ReasonCode: "complete", CountComplete: true, CoverageComplete: true, Results: results}
	for index := range observation.Digest {
		observation.Digest = observation.Digest[:index] + "a" + observation.Digest[index+1:]
	}
	if err := observation.Validate(); err != nil {
		t.Fatal(err)
	}
	observation.Results[6] = observation.Results[0]
	if err := observation.Validate(); err == nil {
		t.Fatal("duplicate family was accepted")
	}
}

func TestIntegrityQueriesAuthorizeBeforePersistence(t *testing.T) {
	operator := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	authorizer, err := NewAuthorizationService(ConfiguredOperatorIdentity{User: operator})
	if err != nil {
		t.Fatal(err)
	}
	store := &integrityQueryStoreStub{}
	service, err := NewIntegrityQueryService(store, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	unauthorized := Requester{ID: operator.Login, Kind: "github_login", DatabaseID: 8, NodeID: operator.NodeID, ActorType: operator.ActorType}
	if _, err := service.Summary(context.Background(), unauthorized); err == nil || store.calls != 0 {
		t.Fatalf("summary err=%v calls=%d", err, store.calls)
	}
	if _, err := service.Findings(context.Background(), IntegrityFindingQuery{Requester: unauthorized, TargetID: "hidden"}); err == nil || store.calls != 0 {
		t.Fatalf("findings err=%v calls=%d", err, store.calls)
	}
	authorized := Requester{ID: operator.Login, Kind: "github_login", DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, ActorType: operator.ActorType}
	if _, err := service.Summary(context.Background(), authorized); err != nil || store.calls != 1 {
		t.Fatalf("authorized err=%v calls=%d", err, store.calls)
	}
}
