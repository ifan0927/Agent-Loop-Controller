package application

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type AuthorityScopeKind string

const (
	ScopeController AuthorityScopeKind = "controller"
	ScopeRepository AuthorityScopeKind = "repository"
	ScopeRun        AuthorityScopeKind = "run"
	ScopeOnboarding AuthorityScopeKind = "onboarding"

	controllerScopeID = "local-controller"
)

type AuthorityScope struct {
	Kind                AuthorityScopeKind `json:"kind"`
	ID                  string             `json:"id"`
	AuthorityDigest     string             `json:"authority_digest"`
	TargetBindingDigest string             `json:"target_binding_digest,omitempty"`
}

func (s AuthorityScope) Validate() error {
	switch s.Kind {
	case ScopeController, ScopeRepository, ScopeRun, ScopeOnboarding:
	default:
		return errors.New("authority scope kind is invalid")
	}
	if strings.TrimSpace(s.ID) == "" || strings.ContainsRune(s.ID, '\x00') || !validAuthorityDigest(s.AuthorityDigest) {
		return errors.New("authority scope is incomplete")
	}
	if s.Kind == ScopeRun && (strings.TrimSpace(s.TargetBindingDigest) == "" || strings.ContainsRune(s.TargetBindingDigest, '\x00')) {
		return errors.New("run authority scope target is incomplete")
	}
	return nil
}

type ConfiguredOperatorIdentity struct {
	User domain.GitHubUserIdentity `json:"user"`
}

func (i ConfiguredOperatorIdentity) Validate() error { return i.User.Validate() }

type ConfiguredRequester struct {
	identity domain.GitHubUserIdentity
}

func (r ConfiguredRequester) Identity() domain.GitHubUserIdentity { return r.identity }

type RepositoryAuthority struct {
	Repository       string
	ProfileID        string
	BindingDigest    string
	AllowedLogins    []string
	TrustedOperators []domain.GitHubUserIdentity
	Enabled          bool
}

func (a RepositoryAuthority) Validate() error {
	if strings.TrimSpace(a.Repository) == "" || strings.TrimSpace(a.ProfileID) == "" || !validAuthorityDigest(a.BindingDigest) || len(a.AllowedLogins) == 0 || len(a.TrustedOperators) == 0 {
		return errors.New("repository authority is incomplete")
	}
	for _, operator := range a.TrustedOperators {
		if operator.Validate() != nil || !slices.ContainsFunc(a.AllowedLogins, func(login string) bool { return strings.EqualFold(login, operator.Login) }) {
			return errors.New("repository authority contains an invalid operator")
		}
	}
	return nil
}

type OnboardingAuthority struct {
	OnboardingID string
	// BoundRepository is nil before the onboarding flow establishes repository
	// authority. The configured operator owns that pre-binding scope.
	BoundRepository *RepositoryAuthority
}

type RunScopeAuthority struct {
	RunID                   string
	Repository              string
	BindingDigest           string
	PersistenceBindingValue string
	AllowedLogins           []string
	TrustedOperators        []domain.GitHubUserIdentity
}

func (a RunScopeAuthority) Validate() error {
	if strings.TrimSpace(a.RunID) == "" || strings.TrimSpace(a.Repository) == "" || !validAuthorityDigest(a.BindingDigest) || len(a.AllowedLogins) == 0 || len(a.TrustedOperators) == 0 {
		return errors.New("frozen run authority is incomplete")
	}
	for _, operator := range a.TrustedOperators {
		if operator.Validate() != nil {
			return errors.New("frozen run authority contains an invalid operator")
		}
	}
	return nil
}

func (a RunScopeAuthority) targetBindingValue() string {
	if a.PersistenceBindingValue != "" {
		return a.PersistenceBindingValue
	}
	return a.BindingDigest
}

func (a OnboardingAuthority) Validate() error {
	if strings.TrimSpace(a.OnboardingID) == "" || strings.ContainsRune(a.OnboardingID, '\x00') {
		return errors.New("onboarding authority is incomplete")
	}
	if a.BoundRepository != nil {
		return a.BoundRepository.Validate()
	}
	return nil
}

// AuthorizedScopeSet is produced only by AuthorizationService. Persistence
// adapters may inspect it but cannot add grants to it.
type AuthorizedScopeSet struct {
	requester domain.GitHubUserIdentity
	scopes    []AuthorityScope
	digest    string
}

type AuthorizedRunPredicate struct {
	RunID         string
	BindingDigest string
}

func (s AuthorizedScopeSet) Digest() string { return s.digest }

func (s AuthorizedScopeSet) Empty() bool {
	return len(s.scopes) == 0 || !validAuthorityDigest(s.digest)
}

func (s AuthorizedScopeSet) HasController() bool {
	return slices.ContainsFunc(s.scopes, func(scope AuthorityScope) bool { return scope.Kind == ScopeController })
}

func (s AuthorizedScopeSet) ControllerOperationTarget() (OperationReceiptTarget, bool) {
	for _, scope := range s.scopes {
		if scope.Kind == ScopeController {
			return OperationReceiptTarget{Scope: ScopeController, TargetID: ConfigurationTargetID, TargetBindingDigest: scope.AuthorityDigest}, true
		}
	}
	return OperationReceiptTarget{}, false
}

func (s AuthorizedScopeSet) RepositoryBindingDigests() []string {
	return scopeValues(s.scopes, ScopeRepository, func(scope AuthorityScope) string { return scope.AuthorityDigest })
}

func (s AuthorizedScopeSet) RunPredicates() []AuthorizedRunPredicate {
	result := make([]AuthorizedRunPredicate, 0, len(s.scopes))
	for _, scope := range s.scopes {
		if scope.Kind == ScopeRun {
			result = append(result, AuthorizedRunPredicate{RunID: scope.ID, BindingDigest: scope.TargetBindingDigest})
		}
	}
	return result
}

func (s AuthorizedScopeSet) AllowsRepositoryBinding(digest string) bool {
	return slices.Contains(s.RepositoryBindingDigests(), digest)
}

func (s AuthorizedScopeSet) AllowsRun(runID, repositoryBindingDigest string) bool {
	if s.AllowsRepositoryBinding(repositoryBindingDigest) {
		return true
	}
	return slices.ContainsFunc(s.RunPredicates(), func(predicate AuthorizedRunPredicate) bool {
		return predicate.RunID == runID && predicate.BindingDigest == repositoryBindingDigest
	})
}

func (s AuthorizedScopeSet) AllowsOperationTarget(target OperationReceiptTarget) bool {
	if s.Empty() || strings.TrimSpace(target.TargetID) == "" || !validAuthorityDigest(target.TargetBindingDigest) {
		return false
	}
	if target.Scope == ScopeRun {
		return s.AllowsRun(target.TargetID, target.TargetBindingDigest)
	}
	if target.Scope == ScopeController && target.TargetID == ConfigurationTargetID {
		return s.HasController()
	}
	if target.Scope == ScopeController && target.TargetID == IntegrityTargetID {
		return s.HasController()
	}
	return slices.ContainsFunc(s.scopes, func(scope AuthorityScope) bool {
		return scope.Kind == target.Scope && scope.ID == target.TargetID && scope.AuthorityDigest == target.TargetBindingDigest
	})
}

func scopeValues(scopes []AuthorityScope, kind AuthorityScopeKind, value func(AuthorityScope) string) []string {
	result := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		if scope.Kind == kind {
			result = append(result, value(scope))
		}
	}
	slices.Sort(result)
	return slices.Compact(result)
}

type AuthorizationService struct {
	operator ConfiguredOperatorIdentity
}

func NewAuthorizationService(operator ConfiguredOperatorIdentity) (*AuthorizationService, error) {
	if err := operator.Validate(); err != nil {
		return nil, err
	}
	return &AuthorizationService{operator: operator}, nil
}

func (s *AuthorizationService) ResolveConfiguredRequester(requester Requester) (ConfiguredRequester, error) {
	if s == nil {
		return ConfiguredRequester{}, serviceError(ErrorInternal, "configured operator authority is unavailable", nil)
	}
	identity, err := requester.githubUserIdentity()
	if err != nil {
		return ConfiguredRequester{}, err
	}
	if !s.operator.User.Equal(identity) {
		return ConfiguredRequester{}, serviceError(ErrorConflict, "requester is not authorized for the controller", nil)
	}
	return ConfiguredRequester{identity: identity}, nil
}

func (s *AuthorizationService) ControllerScopes(requester ConfiguredRequester) (AuthorizedScopeSet, error) {
	if s == nil || !s.operator.User.Equal(requester.identity) {
		return AuthorizedScopeSet{}, serviceError(ErrorConflict, "requester is not authorized for the controller", nil)
	}
	digest := identityDigest(s.operator.User)
	return newAuthorizedScopeSet(requester.identity, AuthorityScope{Kind: ScopeController, ID: controllerScopeID, AuthorityDigest: digest})
}

func (s *AuthorizationService) ControllerRunScopes(requester ConfiguredRequester, authorities []RunScopeAuthority) (AuthorizedScopeSet, error) {
	if s == nil || !s.operator.User.Equal(requester.identity) {
		return AuthorizedScopeSet{}, serviceError(ErrorConflict, "requester is not authorized for the controller", nil)
	}
	scopes := []AuthorityScope{{Kind: ScopeController, ID: controllerScopeID, AuthorityDigest: identityDigest(s.operator.User)}}
	for _, authority := range authorities {
		if err := authority.Validate(); err != nil {
			// Invalid frozen authority cannot grant visibility. Treat it as a
			// hidden row so corrupt or legacy sibling evidence cannot alter an
			// authorized collection's page shape.
			continue
		}
		if runAuthorityAllows(authority, requester.identity) {
			scopes = append(scopes, AuthorityScope{Kind: ScopeRun, ID: authority.RunID, AuthorityDigest: authority.BindingDigest, TargetBindingDigest: authority.targetBindingValue()})
		}
	}
	return newAuthorizedScopeSet(requester.identity, scopes...)
}

// RunScopes derives one frozen-run grant for the configured operator. The
// caller must obtain the authority through the narrow target-authority port;
// the complete run aggregate is not needed to make this decision.
func (s *AuthorizationService) RunScopes(requester ConfiguredRequester, authority RunScopeAuthority) (AuthorizedScopeSet, error) {
	if s == nil || !s.operator.User.Equal(requester.identity) {
		return AuthorizedScopeSet{}, serviceError(ErrorConflict, "requester is not authorized for the run", nil)
	}
	if err := authority.Validate(); err != nil {
		return AuthorizedScopeSet{}, serviceError(ErrorConflict, "frozen run authority is invalid", err)
	}
	if !runAuthorityAllows(authority, requester.identity) {
		return AuthorizedScopeSet{}, serviceError(ErrorConflict, "requester is not authorized for the run", nil)
	}
	return newAuthorizedScopeSet(requester.identity, AuthorityScope{Kind: ScopeRun, ID: authority.RunID, AuthorityDigest: authority.BindingDigest, TargetBindingDigest: authority.targetBindingValue()})
}

func (s *AuthorizationService) ConfiguredFrozenRunScopes(requester ConfiguredRequester, run Run) (AuthorizedScopeSet, error) {
	authority, err := frozenRunScopeAuthority(run)
	if err != nil {
		return AuthorizedScopeSet{}, err
	}
	return s.RunScopes(requester, authority)
}

// cliRequesterRunScopes preserves the existing requester-flag compatibility
// boundary while producing the same frozen-run predicate used by scoped
// persistence. Version-5 callers resolve the configured requester separately.
func cliRequesterRunScopes(requester Requester, authority RunScopeAuthority) (AuthorizedScopeSet, error) {
	if strings.TrimSpace(authority.RunID) == "" || strings.TrimSpace(authority.Repository) == "" || !validAuthorityDigest(authority.BindingDigest) || len(authority.AllowedLogins) == 0 {
		return AuthorizedScopeSet{}, serviceError(ErrorConflict, "frozen run authority is invalid", nil)
	}
	trusted := make([]TrustedActorIdentity, 0, len(authority.TrustedOperators))
	for _, operator := range authority.TrustedOperators {
		trusted = append(trusted, TrustedActorIdentity{Login: operator.Login, DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, Type: operator.ActorType})
	}
	if err := requester.authorize(authority.AllowedLogins, trusted); err != nil {
		return AuthorizedScopeSet{}, err
	}
	identity, err := requester.githubUserIdentity()
	if err != nil {
		if len(trusted) != 0 {
			return AuthorizedScopeSet{}, err
		}
		identity = domain.GitHubUserIdentity{Login: requester.ID, DatabaseID: 1, NodeID: "legacy:" + requester.ID, ActorType: "User"}
	}
	return newAuthorizedScopeSet(identity, AuthorityScope{Kind: ScopeRun, ID: authority.RunID, AuthorityDigest: authority.BindingDigest, TargetBindingDigest: authority.targetBindingValue()})
}

func (s *AuthorizationService) RepositoryScopes(requester ConfiguredRequester, authority RepositoryAuthority) (AuthorizedScopeSet, error) {
	if s == nil || !s.operator.User.Equal(requester.identity) {
		return AuthorizedScopeSet{}, serviceError(ErrorConflict, "requester is not authorized for the repository", nil)
	}
	if err := authority.Validate(); err != nil {
		return AuthorizedScopeSet{}, serviceError(ErrorConflict, "repository authority is invalid", err)
	}
	if !repositoryAllows(authority, requester.identity) {
		return AuthorizedScopeSet{}, serviceError(ErrorConflict, "requester is not authorized for the repository", nil)
	}
	return newAuthorizedScopeSet(requester.identity, AuthorityScope{Kind: ScopeRepository, ID: authority.Repository, AuthorityDigest: authority.BindingDigest})
}

func (s *AuthorizationService) OnboardingScopes(requester ConfiguredRequester, authority OnboardingAuthority) (AuthorizedScopeSet, error) {
	if s == nil || !s.operator.User.Equal(requester.identity) {
		return AuthorizedScopeSet{}, serviceError(ErrorConflict, "requester is not authorized for onboarding", nil)
	}
	if err := authority.Validate(); err != nil {
		return AuthorizedScopeSet{}, serviceError(ErrorConflict, "onboarding authority is invalid", err)
	}
	authorityDigest := identityDigest(requester.identity)
	if authority.BoundRepository != nil {
		if !repositoryAllows(*authority.BoundRepository, requester.identity) {
			return AuthorizedScopeSet{}, serviceError(ErrorConflict, "requester is not authorized for onboarding", nil)
		}
		authorityDigest = authority.BoundRepository.BindingDigest
	}
	return newAuthorizedScopeSet(requester.identity, AuthorityScope{Kind: ScopeOnboarding, ID: authority.OnboardingID, AuthorityDigest: authorityDigest})
}

// FrozenRunScopes deliberately ignores mutable repository enablement. It
// re-reads and validates the authority frozen into the target run.
func (s *AuthorizationService) FrozenRunScopes(requester Requester, run Run) (AuthorizedScopeSet, error) {
	var repository LocalRepository
	if err := json.Unmarshal([]byte(run.RepositoryConfigJSON), &repository); err != nil {
		return AuthorizedScopeSet{}, serviceError(ErrorConflict, "persisted repository authority is invalid", err)
	}
	if err := requester.authorize(repository.AllowedOperatorLogins, repository.TrustedOperatorActors); err != nil {
		return AuthorizedScopeSet{}, err
	}
	identity, err := requester.githubUserIdentity()
	if err != nil {
		// Legacy test/run records without immutable trusted actors retain their
		// existing login-only compatibility until migrated.
		if len(repository.TrustedOperatorActors) != 0 {
			return AuthorizedScopeSet{}, err
		}
		identity = domain.GitHubUserIdentity{Login: requester.ID, DatabaseID: 1, NodeID: "legacy:" + requester.ID, ActorType: "User"}
	}
	targetBinding := run.RepositoryBindingDigest
	digest := targetBinding
	if !validAuthorityDigest(digest) {
		digest = digestText("legacy-run-authority\x00" + strings.ToLower(run.Repository))
	}
	if targetBinding == "" {
		targetBinding = digest
	}
	return newAuthorizedScopeSet(identity, AuthorityScope{Kind: ScopeRun, ID: run.ID, AuthorityDigest: digest, TargetBindingDigest: targetBinding})
}

func frozenRunScopeAuthority(run Run) (RunScopeAuthority, error) {
	var repository LocalRepository
	if err := json.Unmarshal([]byte(run.RepositoryConfigJSON), &repository); err != nil {
		return RunScopeAuthority{}, serviceError(ErrorConflict, "persisted repository authority is invalid", err)
	}
	trusted := make([]domain.GitHubUserIdentity, 0, len(repository.TrustedOperatorActors))
	for _, actor := range repository.TrustedOperatorActors {
		trusted = append(trusted, domain.GitHubUserIdentity{Login: actor.Login, DatabaseID: actor.DatabaseID, NodeID: actor.NodeID, ActorType: actor.Type})
	}
	targetBinding := run.RepositoryBindingDigest
	digest := targetBinding
	if !validAuthorityDigest(digest) {
		digest = digestText("legacy-run-authority\x00" + strings.ToLower(run.Repository))
	}
	if targetBinding == "" {
		targetBinding = digest
	}
	return RunScopeAuthority{RunID: run.ID, Repository: run.Repository, BindingDigest: digest, PersistenceBindingValue: targetBinding, AllowedLogins: append([]string(nil), repository.AllowedOperatorLogins...), TrustedOperators: trusted}, nil
}

func repositoryAllows(authority RepositoryAuthority, requester domain.GitHubUserIdentity) bool {
	return slices.ContainsFunc(authority.AllowedLogins, func(login string) bool { return strings.EqualFold(login, requester.Login) }) &&
		slices.ContainsFunc(authority.TrustedOperators, func(operator domain.GitHubUserIdentity) bool { return operator.Equal(requester) })
}

func runAuthorityAllows(authority RunScopeAuthority, requester domain.GitHubUserIdentity) bool {
	return slices.ContainsFunc(authority.AllowedLogins, func(login string) bool { return strings.EqualFold(login, requester.Login) }) &&
		slices.ContainsFunc(authority.TrustedOperators, func(operator domain.GitHubUserIdentity) bool { return operator.Equal(requester) })
}

func newAuthorizedScopeSet(requester domain.GitHubUserIdentity, scopes ...AuthorityScope) (AuthorizedScopeSet, error) {
	if requester.Validate() != nil || len(scopes) == 0 {
		return AuthorizedScopeSet{}, errors.New("authorized scope requester is invalid")
	}
	for _, scope := range scopes {
		if err := scope.Validate(); err != nil {
			return AuthorizedScopeSet{}, err
		}
	}
	slices.SortFunc(scopes, func(a, b AuthorityScope) int {
		if comparison := strings.Compare(string(a.Kind), string(b.Kind)); comparison != 0 {
			return comparison
		}
		if comparison := strings.Compare(a.ID, b.ID); comparison != 0 {
			return comparison
		}
		return strings.Compare(a.AuthorityDigest, b.AuthorityDigest)
	})
	scopes = slices.Compact(scopes)
	encoded, _ := json.Marshal(struct {
		Requester domain.GitHubUserIdentity `json:"requester"`
		Scopes    []AuthorityScope          `json:"scopes"`
	}{requester, scopes})
	return AuthorizedScopeSet{requester: requester, scopes: scopes, digest: digestText(string(encoded))}, nil
}

func (r Requester) githubUserIdentity() (domain.GitHubUserIdentity, error) {
	identity := domain.GitHubUserIdentity{Login: r.ID, DatabaseID: r.DatabaseID, NodeID: r.NodeID, ActorType: r.ActorType}
	if r.Kind != "github_login" || identity.Validate() != nil {
		return domain.GitHubUserIdentity{}, serviceError(ErrorInvalidInput, "complete GitHub User requester identity is required", nil)
	}
	return identity, nil
}

func identityDigest(identity domain.GitHubUserIdentity) string {
	return digestText(strings.ToLower(identity.Login) + "\x00" + hex.EncodeToString([]byte(identity.NodeID)) + "\x00" + identity.ActorType + "\x00" + strconv.FormatInt(identity.DatabaseID, 10))
}

func digestText(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func LegacyRunAuthorityDigest(repository string) string {
	return digestText("legacy-run-authority\x00" + strings.ToLower(repository))
}

func validAuthorityDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
