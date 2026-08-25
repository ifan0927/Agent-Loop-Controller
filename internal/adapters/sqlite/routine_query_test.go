package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestRoutineOnboardingQueryFiltersAuthorityBeforeCountAndOrder(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 8, 25, 8, 0, 0, 0, time.UTC)
	digest := func(value string) string { return strings.Repeat(value, 64) }
	operator := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	other := domain.GitHubUserIdentity{Login: "other", DatabaseID: 8, NodeID: "USER_8", ActorType: "User"}
	open := func(id, repository, marker string, requester domain.GitHubUserIdentity, at time.Time) {
		_, created, openErr := store.OpenOnboarding(ctx, application.OnboardingOpenInput{OnboardingID: id, Kind: domain.OnboardingExistingCheckout, CanonicalRepository: repository, Requester: requester, PrivateInputDigest: digest(marker), SourcePathDigest: digest(marker), SourceAncestorDigests: []string{digest(marker)}, RequestDigest: digest(marker), ConfigurationBaseGenerationID: 1, ConfigurationBaseDigest: digest("f"), ConfigurationAuthorityVersion: 1, OpenedAt: at})
		if openErr != nil || !created {
			t.Fatalf("open %s created=%t err=%v", id, created, openErr)
		}
	}
	open("onboarding-owned-new", "owner/new", "a", operator, now.Add(2*time.Second))
	open("onboarding-hidden", "owner/hidden", "b", other, now.Add(3*time.Second))
	open("onboarding-owned-old", "owner/old", "c", operator, now.Add(time.Second))
	authorizer, err := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: operator})
	if err != nil {
		t.Fatal(err)
	}
	configured, err := authorizer.ResolveConfiguredRequester(application.Requester{ID: operator.Login, Kind: "github_login", DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, ActorType: operator.ActorType})
	if err != nil {
		t.Fatal(err)
	}
	scopes, err := authorizer.ControllerScopes(configured)
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.ListAuthorizedOnboardings(ctx, application.AuthorizedOnboardingQuery{Requester: operator, Scopes: scopes, Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || len(page.Onboardings) != 2 || page.Onboardings[0].OnboardingID != "onboarding-owned-new" || page.Onboardings[1].OnboardingID != "onboarding-owned-old" {
		t.Fatalf("page=%+v", page)
	}
}

func TestRoutineOverviewReadsOneBoundedReadOnlySnapshot(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	operator := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	baseline := application.ConfigurationBaselineInput{Candidate: application.ValidatedConfigurationCandidate{Digest: strings.Repeat("d", 64), Size: 1, SchemaVersion: 5, DatabasePath: path, Operator: operator}, CanonicalConfigPath: path + ".json", ObservedAt: now}
	if err := store.PrepareConfigurationBaseline(ctx, baseline); err != nil {
		t.Fatal(err)
	}
	authority, _, err := store.AdoptConfigurationBaseline(ctx, baseline)
	if err != nil {
		t.Fatal(err)
	}
	authorizer, err := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: operator})
	if err != nil {
		t.Fatal(err)
	}
	configured, err := authorizer.ResolveConfiguredRequester(application.Requester{ID: operator.Login, Kind: "github_login", DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, ActorType: operator.ActorType})
	if err != nil {
		t.Fatal(err)
	}
	scopes, err := authorizer.ControllerScopes(configured)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.ReadRoutineOverviewSnapshot(ctx, scopes, operator, application.RoutineOverviewItemLimit)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ObservedAt.IsZero() || snapshot.Configuration.Desired.GenerationID != authority.Desired.GenerationID || snapshot.Runs.Active != 0 || snapshot.Repositories.Total != 0 || snapshot.Onboarding.Total != 0 || snapshot.QueueSnapshot != nil {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	after, found, err := store.ConfigurationAuthority(ctx)
	if err != nil || !found || after.Version != authority.Version || after.Desired.GenerationID != authority.Desired.GenerationID {
		t.Fatalf("authority changed after read: %+v found=%t err=%v", after, found, err)
	}
}
