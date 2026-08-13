package application

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestAuthorizationServiceUsesExactConfiguredOperatorAndClosedScopes(t *testing.T) {
	operator := domain.GitHubUserIdentity{Login: "ifan", DatabaseID: 7, NodeID: "U_7", ActorType: "User"}
	service, err := NewAuthorizationService(ConfiguredOperatorIdentity{User: operator})
	if err != nil {
		t.Fatal(err)
	}
	requester, err := service.ResolveConfiguredRequester(requesterForUser(operator))
	if err != nil {
		t.Fatal(err)
	}
	controller, err := service.ControllerScopes(requester)
	if err != nil || !controller.HasController() || controller.Empty() {
		t.Fatalf("controller scopes=%+v err=%v", controller, err)
	}
	repository := repositoryAuthority(operator, false)
	repositoryScopes, err := service.RepositoryScopes(requester, repository)
	if err != nil || !repositoryScopes.AllowsRepositoryBinding(repository.BindingDigest) || repositoryScopes.HasController() {
		t.Fatalf("repository scopes=%+v err=%v", repositoryScopes, err)
	}
	lookalike := operator
	lookalike.DatabaseID++
	if _, err := service.ResolveConfiguredRequester(requesterForUser(lookalike)); err == nil {
		t.Fatal("configured operator lookalike was accepted")
	}
	if err := (AuthorityScope{Kind: "role", ID: "admin", AuthorityDigest: strings.Repeat("a", 64)}).Validate(); err == nil {
		t.Fatal("generic role scope was accepted")
	}
	runScopes, err := service.ControllerRunScopes(requester, []RunScopeAuthority{
		{RunID: "authorized-run", Repository: "owner/repo", BindingDigest: strings.Repeat("b", 64), AllowedLogins: []string{operator.Login}, TrustedOperators: []domain.GitHubUserIdentity{operator}},
		{RunID: "hidden-run", Repository: "other/repo", BindingDigest: strings.Repeat("c", 64), AllowedLogins: []string{"other"}, TrustedOperators: []domain.GitHubUserIdentity{{Login: "other", DatabaseID: 8, NodeID: "U_8", ActorType: "User"}}},
		{RunID: "corrupt-hidden-run", Repository: "corrupt/repo"},
	})
	if err != nil || !runScopes.HasController() || !runScopes.AllowsRun("authorized-run", strings.Repeat("b", 64)) || runScopes.AllowsRun("hidden-run", strings.Repeat("c", 64)) {
		t.Fatalf("controller run scopes=%+v err=%v", runScopes, err)
	}
	if runScopes.AllowsRun("authorized-run", strings.Repeat("d", 64)) {
		t.Fatal("run scope accepted a mismatched frozen binding digest")
	}
}

func TestOnboardingAuthorityBindsConfiguredOperatorThenRepository(t *testing.T) {
	operator := domain.GitHubUserIdentity{Login: "ifan", DatabaseID: 7, NodeID: "U_7", ActorType: "User"}
	service, _ := NewAuthorizationService(ConfiguredOperatorIdentity{User: operator})
	requester, _ := service.ResolveConfiguredRequester(requesterForUser(operator))
	prebound, err := service.OnboardingScopes(requester, OnboardingAuthority{OnboardingID: "onboarding-1"})
	if err != nil || prebound.Empty() {
		t.Fatalf("prebound=%+v err=%v", prebound, err)
	}
	repository := repositoryAuthority(operator, true)
	bound, err := service.OnboardingScopes(requester, OnboardingAuthority{OnboardingID: "onboarding-1", BoundRepository: &repository})
	if err != nil || bound.Empty() || bound.Digest() == prebound.Digest() {
		t.Fatalf("bound=%+v err=%v", bound, err)
	}
	drifted := repository
	drifted.TrustedOperators = []domain.GitHubUserIdentity{{Login: "other", DatabaseID: 8, NodeID: "U_8", ActorType: "User"}}
	drifted.AllowedLogins = []string{"other"}
	if _, err := service.OnboardingScopes(requester, OnboardingAuthority{OnboardingID: "onboarding-1", BoundRepository: &drifted}); err == nil {
		t.Fatal("bound onboarding authority drift was accepted")
	}
}

func TestFrozenRunScopeIgnoresMutableEnablementAndRejectsIdentityDrift(t *testing.T) {
	operator := domain.GitHubUserIdentity{Login: "ifan", DatabaseID: 7, NodeID: "U_7", ActorType: "User"}
	repository := LocalRepository{CanonicalRepository: "owner/repo", AllowedOperatorLogins: []string{"ifan"}, TrustedOperatorActors: []TrustedActorIdentity{{Login: operator.Login, DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, Type: operator.ActorType}}}
	raw, _ := json.Marshal(repository)
	run := Run{ID: "run-1", Repository: repository.CanonicalRepository, RepositoryConfigJSON: string(raw), RepositoryBindingDigest: strings.Repeat("b", 64)}
	service, _ := NewAuthorizationService(ConfiguredOperatorIdentity{User: operator})
	scopes, err := service.FrozenRunScopes(requesterForUser(operator), run)
	if err != nil || !scopes.AllowsRun(run.ID, run.RepositoryBindingDigest) {
		t.Fatalf("scopes=%+v err=%v", scopes, err)
	}
	// Repository lifecycle enablement is deliberately absent from frozen run
	// authority. A disable or removal cannot rewrite this result.
	drifted := operator
	drifted.NodeID = "U_CHANGED"
	if _, err := service.FrozenRunScopes(requesterForUser(drifted), run); err == nil {
		t.Fatal("frozen run accepted material requester identity drift")
	}
}

func requesterForUser(user domain.GitHubUserIdentity) Requester {
	return Requester{ID: user.Login, Kind: "github_login", DatabaseID: user.DatabaseID, NodeID: user.NodeID, ActorType: user.ActorType}
}

func repositoryAuthority(operator domain.GitHubUserIdentity, enabled bool) RepositoryAuthority {
	return RepositoryAuthority{Repository: "owner/repo", ProfileID: "repository-profile:owner/repo", BindingDigest: strings.Repeat("a", 64), AllowedLogins: []string{operator.Login}, TrustedOperators: []domain.GitHubUserIdentity{operator}, Enabled: enabled}
}
