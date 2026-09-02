package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type controllerOnboardingCollectionFixture struct {
	queries []ControllerOnboardingQuery
	pages   []ControllerOnboardingPage
}

func (s *controllerOnboardingCollectionFixture) ListControllerOnboardings(_ context.Context, query ControllerOnboardingQuery) (ControllerOnboardingPage, error) {
	s.queries = append(s.queries, query)
	page := s.pages[0]
	s.pages = s.pages[1:]
	return page, nil
}

func TestRoutineOnboardingCollectionUsesStableReaderAndCompleteKeyset(t *testing.T) {
	operator := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	requester := requesterForUser(operator)
	authorizer, _ := NewAuthorizationService(ConfiguredOperatorIdentity{User: operator})
	configured, _ := authorizer.ResolveConfiguredRequester(requester)
	reader, _ := authorizer.ControllerReadCollectionAuthority(configured)
	now := time.Date(2026, 9, 2, 3, 0, 0, 0, time.UTC)
	newer := Onboarding{OnboardingID: "onboarding-newer", Kind: domain.OnboardingExistingCheckout, CanonicalRepository: "owner/repo", Status: domain.OnboardingOpened, Requester: operator, CreatedAt: now, UpdatedAt: now}
	older := Onboarding{OnboardingID: "onboarding-older", Kind: domain.OnboardingExistingCheckout, CanonicalRepository: "owner/repo", Status: domain.OnboardingOpened, Requester: operator, CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute)}
	store := &controllerOnboardingCollectionFixture{pages: []ControllerOnboardingPage{
		{Onboardings: []Onboarding{newer, older}, Total: 2},
		{Onboardings: []Onboarding{older}, Total: 3},
	}}
	service, err := NewRoutineOnboardingQueryService(store, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.ListController(context.Background(), reader, RoutineOnboardingQuery{Requester: requester, Repository: "owner/repo", Limit: 1}, now)
	if err != nil || first.Collection.Total != 2 || !first.Collection.Truncated || first.Collection.NextCursor == "" || len(first.Onboardings) != 1 || first.Onboardings[0].OnboardingID != newer.OnboardingID {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := service.ListController(context.Background(), reader, RoutineOnboardingQuery{Requester: requester, Repository: "owner/repo", Limit: 1, Cursor: first.Collection.NextCursor}, now.Add(time.Second))
	if err != nil || second.Collection.Total != 3 || second.Collection.Truncated || len(second.Onboardings) != 1 || second.Onboardings[0].OnboardingID != older.OnboardingID {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	if len(store.queries) != 2 || store.queries[1].Authority.Digest() != reader.Digest() || !store.queries[1].ConfiguredRequester.Equal(operator) || store.queries[1].CanonicalRepository != "owner/repo" || store.queries[1].BeforeUpdatedAt != newer.UpdatedAt || store.queries[1].BeforeOnboardingID != newer.OnboardingID {
		t.Fatalf("queries=%+v", store.queries)
	}
	legacyRaw, _ := json.Marshal(struct {
		Version, ScopeDigest, Repository string
		UpdatedAt                        time.Time
		OnboardingID                     string
	}{RoutineQuerySchemaVersion, reader.Digest(), "owner/repo", now, newer.OnboardingID})
	if _, err := service.ListController(context.Background(), reader, RoutineOnboardingQuery{Requester: requester, Repository: "owner/repo", Cursor: base64.RawURLEncoding.EncodeToString(legacyRaw)}, now); err == nil {
		t.Fatal("legacy dynamic-scope onboarding cursor was accepted")
	}
	if _, err := service.ListController(context.Background(), reader, RoutineOnboardingQuery{Requester: requester, Repository: "owner/other", Cursor: first.Collection.NextCursor}, now); err == nil {
		t.Fatal("repository-filter drift was accepted")
	}
	lookalike := requester
	lookalike.NodeID = "USER_CHANGED"
	if _, err := service.ListController(context.Background(), reader, RoutineOnboardingQuery{Requester: lookalike}, now); err == nil || strings.Contains(err.Error(), "USER_CHANGED") {
		t.Fatalf("unsafe requester result: %v", err)
	}
}

func TestRoutineOnboardingCollectionClosesSelectedLifecycleRows(t *testing.T) {
	operator := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	other := domain.GitHubUserIdentity{Login: "other", DatabaseID: 8, NodeID: "USER_8", ActorType: "User"}
	requester := requesterForUser(operator)
	authorizer, _ := NewAuthorizationService(ConfiguredOperatorIdentity{User: operator})
	configured, _ := authorizer.ResolveConfiguredRequester(requester)
	reader, _ := authorizer.ControllerReadCollectionAuthority(configured)
	now := time.Date(2026, 9, 2, 4, 0, 0, 0, time.UTC)
	bound := Onboarding{OnboardingID: "onboarding-bound", Kind: domain.OnboardingExistingCheckout, CanonicalRepository: "owner/bound", Status: domain.OnboardingAccepted, Requester: other, OperationID: "operation-bound", PreflightDigest: strings.Repeat("a", 64), PreviewDigest: strings.Repeat("b", 64), ProfileID: "repository-profile:owner/bound", ProfileDigest: strings.Repeat("c", 64), RepositoryBindingDigest: strings.Repeat("d", 64), AcceptedAt: now.Add(time.Second), CreatedAt: now, UpdatedAt: now.Add(time.Second)}
	forgedPrebinding := bound
	forgedPrebinding.OnboardingID = "onboarding-forged"
	forgedPrebinding.Status = domain.OnboardingOpened
	forgedPrebinding.CreatedAt = now.Add(2 * time.Second)
	forgedPrebinding.UpdatedAt = now.Add(2 * time.Second)
	forgedPrebinding.AcceptedAt = time.Time{}
	store := &controllerOnboardingCollectionFixture{pages: []ControllerOnboardingPage{{Onboardings: []Onboarding{bound}, Total: 1}, {Onboardings: []Onboarding{forgedPrebinding}, Total: 1}}}
	service, err := NewRoutineOnboardingQueryService(store, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	page, err := service.ListController(context.Background(), reader, RoutineOnboardingQuery{Requester: requester}, now)
	if err != nil || len(page.Onboardings) != 1 || page.Onboardings[0].OnboardingID != bound.OnboardingID {
		t.Fatalf("valid bound page=%+v err=%v", page, err)
	}
	if _, err := service.ListController(context.Background(), reader, RoutineOnboardingQuery{Requester: requester}, now); err == nil {
		t.Fatal("forged pre-binding lifecycle was accepted")
	}
}
