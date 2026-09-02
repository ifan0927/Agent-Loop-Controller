package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type repositoryServiceProfileSource struct {
	profile RepositoryProfileAuthority
	found   bool
	calls   int
}

func (s *repositoryServiceProfileSource) RepositoryProfile(context.Context, string) (RepositoryProfileAuthority, bool, error) {
	s.calls++
	return s.profile, s.found, nil
}

func (s *repositoryServiceProfileSource) ListRepositoryProfiles(context.Context) ([]RepositoryProfileAuthority, error) {
	s.calls++
	return []RepositoryProfileAuthority{s.profile}, nil
}

type repositoryServiceStore struct {
	inspectErr       error
	repositories     []RepositoryProjection
	collectionInputs []ControllerRepositoryQuery
}

func (*repositoryServiceStore) BeginOperationReceipt(context.Context, OperationReceipt) (OperationReceipt, bool, error) {
	panic("not used")
}
func (*repositoryServiceStore) AdvanceOperationReceipt(context.Context, OperationReceiptMutation) (OperationReceipt, bool, error) {
	panic("not used")
}
func (*repositoryServiceStore) GetOperationReceiptTarget(context.Context, string) (OperationReceiptTarget, error) {
	panic("not used")
}
func (*repositoryServiceStore) GetAuthorizedOperationReceipt(context.Context, string, AuthorizedScopeSet) (OperationReceipt, error) {
	panic("not used")
}
func (*repositoryServiceStore) AdoptRepositoryLifecycleBaseline(context.Context, RepositoryBaselineInput) error {
	panic("not used")
}
func (*repositoryServiceStore) RepositoryOperationAuthority(context.Context, string) (RepositoryOperationAuthority, error) {
	panic("not used")
}
func (s *repositoryServiceStore) ListControllerRepositories(_ context.Context, input ControllerRepositoryQuery) (RepositoryListPage, error) {
	s.collectionInputs = append(s.collectionInputs, input)
	page := RepositoryListPage{Total: len(s.repositories)}
	for _, repository := range s.repositories {
		if repository.Lifecycle.Repository > input.AfterRepository {
			page.Repositories = append(page.Repositories, repository)
		}
		if len(page.Repositories) == input.Limit {
			break
		}
	}
	return page, nil
}

func TestRepositoryCollectionUsesStableReaderWithoutProfileEnumeration(t *testing.T) {
	identity := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	authorizer, _ := NewAuthorizationService(ConfiguredOperatorIdentity{User: identity})
	profiles := &repositoryServiceProfileSource{}
	lifecycle := func(repository, marker string) RepositoryProjection {
		return RepositoryProjection{Lifecycle: RepositoryLifecycle{IncarnationID: "repository-incarnation-" + strings.Repeat(marker, 32), Repository: repository, ProfileID: "profile-" + marker, ProfileDigest: strings.Repeat(marker, 64), RepositoryBindingDigest: strings.Repeat(marker, 64), Intent: RepositoryEnabled, Version: 1, UpdatedAt: time.Date(2026, 9, 2, 1, 0, 0, 0, time.UTC)}}
	}
	store := &repositoryServiceStore{repositories: []RepositoryProjection{lifecycle("owner/b", "b"), lifecycle("owner/c", "c")}}
	service, err := NewRepositoryService(store, authorizer, profiles, RepositoryObservers{})
	if err != nil {
		t.Fatal(err)
	}
	requester := requesterForUser(identity)
	configured, _ := authorizer.ResolveConfiguredRequester(requester)
	reader, _ := authorizer.ControllerReadCollectionAuthority(configured)
	first, err := service.List(context.Background(), requester, 1, "")
	if err != nil || len(first.Repositories) != 1 || first.Repositories[0].Lifecycle.Repository != "owner/b" || first.Total != 2 || first.NextCursor == "" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	store.repositories = append([]RepositoryProjection{lifecycle("owner/a", "a")}, store.repositories...)
	second, err := service.ListController(context.Background(), reader, 1, first.NextCursor)
	if err != nil || len(second.Repositories) != 1 || second.Repositories[0].Lifecycle.Repository != "owner/c" || second.Total != 3 || second.HasMore {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	if profiles.calls != 0 || len(store.collectionInputs) != 2 || store.collectionInputs[1].Authority.Digest() != reader.Digest() || store.collectionInputs[1].AfterRepository != "owner/b" {
		t.Fatalf("profiles=%d inputs=%+v", profiles.calls, store.collectionInputs)
	}
	legacyRaw, _ := json.Marshal(struct{ Scope, Repository string }{reader.Digest(), "owner/b"})
	if _, err := service.ListController(context.Background(), reader, 1, base64.RawURLEncoding.EncodeToString(legacyRaw)); err == nil {
		t.Fatal("legacy dynamic-scope repository cursor was accepted")
	}
}
func (s *repositoryServiceStore) GetAuthorizedRepository(context.Context, string, AuthorizedScopeSet) (RepositoryProjection, error) {
	return RepositoryProjection{}, s.inspectErr
}
func (*repositoryServiceStore) BeginRepositoryRecheck(context.Context, RepositoryRecheckStart) (RepositoryRecheckState, bool, error) {
	panic("not used")
}
func (*repositoryServiceStore) SaveRepositoryRecheckObservation(context.Context, string, domain.RepositoryDimensionResult) error {
	panic("not used")
}
func (*repositoryServiceStore) PublishRepositoryRecheck(context.Context, RepositoryRecheckPublication) (RepositoryProjection, OperationReceipt, error) {
	panic("not used")
}
func (*repositoryServiceStore) SettleRepositoryRecheckFailure(context.Context, RepositoryRecheckFailure) error {
	panic("not used")
}
func (*repositoryServiceStore) ChangeRepositoryLifecycle(context.Context, RepositoryLifecycleChange) (RepositoryProjection, OperationReceipt, error) {
	panic("not used")
}
func (*repositoryServiceStore) SettleRepositoryLifecycleFailure(context.Context, RepositoryLifecycleFailure) error {
	panic("not used")
}
func (*repositoryServiceStore) CheckRepositoryAdmission(context.Context, LocalRepository) (RepositoryAdmissionDecision, error) {
	panic("not used")
}

func TestRepositoryInspectAuthorizesBeforeLookupAndHidesTargets(t *testing.T) {
	identity := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	authorizer, err := NewAuthorizationService(ConfiguredOperatorIdentity{User: identity})
	if err != nil {
		t.Fatal(err)
	}
	authority := RepositoryAuthority{Repository: "owner/repo", ProfileID: "repository-profile:owner/repo", BindingDigest: strings.Repeat("b", 64), AllowedLogins: []string{"operator"}, TrustedOperators: []domain.GitHubUserIdentity{identity}}
	profiles := &repositoryServiceProfileSource{profile: RepositoryProfileAuthority{Authority: authority, Profile: LocalRepository{CanonicalRepository: authority.Repository}}, found: true}
	store := &repositoryServiceStore{inspectErr: ErrRepositoryLifecycleMissing}
	service, err := NewRepositoryService(store, authorizer, profiles, RepositoryObservers{})
	if err != nil {
		t.Fatal(err)
	}
	unauthorized := Requester{ID: "other", Kind: "github_login", DatabaseID: 8, NodeID: "USER_8", ActorType: "User"}
	_, deniedErr := service.Inspect(context.Background(), unauthorized, authority.Repository)
	if profiles.calls != 0 {
		t.Fatalf("repository profile looked up before requester authorization: calls=%d", profiles.calls)
	}
	authorized := Requester{ID: identity.Login, Kind: "github_login", DatabaseID: identity.DatabaseID, NodeID: identity.NodeID, ActorType: identity.ActorType}
	_, missingErr := service.Inspect(context.Background(), authorized, authority.Repository)
	var deniedService, missingService *ServiceError
	if !errors.As(deniedErr, &deniedService) || !errors.As(missingErr, &missingService) || deniedService.Category != ErrorNotFound || missingService.Category != ErrorNotFound || deniedErr.Error() != missingErr.Error() {
		t.Fatalf("denied=%v missing=%v", deniedErr, missingErr)
	}
}

func TestRepositoryReadinessSnapshotRejectsPublicationBeforeObservation(t *testing.T) {
	results := make([]domain.RepositoryDimensionResult, 0, len(domain.RepositoryReadinessDimensions))
	observed := time.Date(2026, 8, 24, 2, 0, 0, 0, time.UTC)
	for _, dimension := range domain.RepositoryReadinessDimensions {
		results = append(results, domain.RepositoryDimensionResult{Dimension: dimension, Status: domain.RepositoryReady, ReasonCode: "ready", EvidenceDigest: strings.Repeat("a", 64), ObservedAt: observed})
	}
	snapshot := RepositoryReadinessSnapshot{SnapshotID: "snapshot", Repository: "owner/repo", ProfileID: "profile", ProfileDigest: strings.Repeat("b", 64), RepositoryBindingDigest: strings.Repeat("c", 64), LifecycleVersion: 1, ConfigurationGenerationID: 1, ConfigurationDigest: strings.Repeat("d", 64), ConfigurationAuthorityVersion: 1, Status: domain.RepositoryReady, ReasonCode: "ready", SnapshotDigest: strings.Repeat("e", 64), Dimensions: results, ObservedAt: observed, PublishedAt: observed.Add(-time.Second)}
	if err := snapshot.Validate(); err == nil {
		t.Fatal("snapshot published before observation was accepted")
	}
}
