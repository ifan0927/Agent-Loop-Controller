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

func TestControllerOnboardingQueryValidatesPrebindingRequesterBeforeCountAndOrder(t *testing.T) {
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
	reader, err := authorizer.ControllerReadCollectionAuthority(configured)
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.ListControllerOnboardings(ctx, application.ControllerOnboardingQuery{Authority: reader, ConfiguredRequester: operator, Limit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || len(page.Onboardings) != 2 || page.Onboardings[0].OnboardingID != "onboarding-owned-new" || page.Onboardings[1].OnboardingID != "onboarding-owned-old" {
		t.Fatalf("page=%+v", page)
	}
	if _, err := store.ListControllerOnboardings(ctx, application.ControllerOnboardingQuery{Authority: reader, ConfiguredRequester: other, Limit: 1}); err == nil {
		t.Fatal("stable reader accepted a different prebinding requester")
	}
}

func TestControllerOnboardingQueryFailsClosedOnContradictoryBindingEvidence(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	digest := func(value string) string { return strings.Repeat(value, 64) }
	operator := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	other := domain.GitHubUserIdentity{Login: "other", DatabaseID: 8, NodeID: "USER_8", ActorType: "User"}
	now := time.Date(2026, 8, 25, 8, 30, 0, 0, time.UTC)
	_, created, err := store.OpenOnboarding(ctx, application.OnboardingOpenInput{OnboardingID: "onboarding-corrupt", Kind: domain.OnboardingExistingCheckout, CanonicalRepository: "owner/corrupt", Requester: other, PrivateInputDigest: digest("a"), SourcePathDigest: digest("a"), SourceAncestorDigests: []string{digest("a")}, RequestDigest: digest("a"), ConfigurationBaseGenerationID: 1, ConfigurationBaseDigest: digest("b"), ConfigurationAuthorityVersion: 1, OpenedAt: now})
	if err != nil || !created {
		t.Fatalf("created=%t err=%v", created, err)
	}
	if _, err := store.db.Exec(`UPDATE repository_onboardings SET profile_id='profile-corrupt',profile_digest=?,repository_binding_digest=? WHERE onboarding_id='onboarding-corrupt'`, digest("c"), digest("d")); err != nil {
		t.Fatal(err)
	}
	var beforeStatus, beforeProfileID, beforeProfileDigest, beforeBinding, beforeUpdated string
	if err := store.db.QueryRow(`SELECT status,profile_id,profile_digest,repository_binding_digest,updated_at FROM repository_onboardings WHERE onboarding_id='onboarding-corrupt'`).Scan(&beforeStatus, &beforeProfileID, &beforeProfileDigest, &beforeBinding, &beforeUpdated); err != nil {
		t.Fatal(err)
	}
	authorizer, _ := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: operator})
	configured, _ := authorizer.ResolveConfiguredRequester(application.Requester{ID: operator.Login, Kind: "github_login", DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, ActorType: operator.ActorType})
	reader, _ := authorizer.ControllerReadCollectionAuthority(configured)
	if _, err := store.ListControllerOnboardings(ctx, application.ControllerOnboardingQuery{Authority: reader, ConfiguredRequester: operator, Limit: 1}); err == nil || strings.Contains(err.Error(), digest("c")) || strings.Contains(err.Error(), digest("d")) {
		t.Fatalf("unsafe contradiction result: %v", err)
	}
	var afterStatus, afterProfileID, afterProfileDigest, afterBinding, afterUpdated string
	if err := store.db.QueryRow(`SELECT status,profile_id,profile_digest,repository_binding_digest,updated_at FROM repository_onboardings WHERE onboarding_id='onboarding-corrupt'`).Scan(&afterStatus, &afterProfileID, &afterProfileDigest, &afterBinding, &afterUpdated); err != nil {
		t.Fatal(err)
	}
	if beforeStatus != afterStatus || beforeProfileID != afterProfileID || beforeProfileDigest != afterProfileDigest || beforeBinding != afterBinding || beforeUpdated != afterUpdated {
		t.Fatal("read-only contradiction validation mutated onboarding authority")
	}
}

func TestControllerOnboardingQueryIncludesReceiptBoundLifecycle(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	digest := func(value string) string { return strings.Repeat(value, 64) }
	operator := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	other := domain.GitHubUserIdentity{Login: "other", DatabaseID: 8, NodeID: "USER_8", ActorType: "User"}
	now := time.Date(2026, 8, 25, 8, 45, 0, 0, time.UTC)
	opened, created, err := store.OpenOnboarding(ctx, application.OnboardingOpenInput{OnboardingID: "onboarding-bound", Kind: domain.OnboardingExistingCheckout, CanonicalRepository: "owner/bound", Requester: other, PrivateInputDigest: digest("a"), SourcePathDigest: digest("b"), SourceAncestorDigests: []string{digest("b")}, RequestDigest: digest("c"), ConfigurationBaseGenerationID: 1, ConfigurationBaseDigest: digest("d"), ConfigurationAuthorityVersion: 1, OpenedAt: now})
	if err != nil || !created {
		t.Fatalf("opened=%+v created=%t err=%v", opened, created, err)
	}
	ready, err := store.SaveOnboardingPreflight(ctx, application.OnboardingPreflightInput{OnboardingID: opened.OnboardingID, ExpectedStatus: opened.Status, PreflightDigest: digest("e"), EvidenceDigest: digest("f"), ObservedAt: now.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	preview := digest("1")
	anchor := digestBytes([]byte("onboarding-start-v1\x00" + opened.OnboardingID + "\x00" + ready.PreflightDigest + "\x00" + preview))
	receipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationOnboardRepository, Scope: application.ScopeOnboarding, TargetID: opened.OnboardingID, Requester: other, RequestDigest: opened.RequestDigest, ExpectedAuthorityDigest: opened.ConfigurationBaseDigest, OperationAnchorDigest: anchor, TargetBindingDigest: onboardingV46IdentityDigest(other), AcceptedAt: now.Add(2*time.Second + 123*time.Millisecond)})
	profile := application.LocalRepository{ProfileID: "repository-profile:owner/bound", ProfileDigest: digest("2"), RepositoryBindingDigest: digest("3"), CanonicalRepository: opened.CanonicalRepository}
	started, _, changed, err := store.StartOnboarding(ctx, application.OnboardingStartAcceptance{OnboardingID: opened.OnboardingID, Expected: ready, PreflightDigest: ready.PreflightDigest, PreviewDigest: preview, Profile: profile, Receipt: receipt, AcceptedAt: receipt.AcceptedAt})
	if err != nil || !changed || started.Status != domain.OnboardingAccepted {
		t.Fatalf("started=%+v changed=%t err=%v", started, changed, err)
	}
	authorizer, _ := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: operator})
	configured, _ := authorizer.ResolveConfiguredRequester(application.Requester{ID: operator.Login, Kind: "github_login", DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, ActorType: operator.ActorType})
	reader, _ := authorizer.ControllerReadCollectionAuthority(configured)
	page, err := store.ListControllerOnboardings(ctx, application.ControllerOnboardingQuery{Authority: reader, ConfiguredRequester: operator, Limit: 2})
	if err != nil || page.Total != 1 || len(page.Onboardings) != 1 || page.Onboardings[0].OnboardingID != opened.OnboardingID || page.Onboardings[0].Status != domain.OnboardingAccepted {
		t.Fatalf("page=%+v err=%v", page, err)
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
