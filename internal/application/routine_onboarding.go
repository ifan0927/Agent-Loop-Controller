package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type RoutineOnboardingQuery struct {
	Requester  Requester `json:"requester"`
	Repository string    `json:"repository,omitempty"`
	Limit      int       `json:"limit,omitempty"`
	Cursor     string    `json:"cursor,omitempty"`
}

type AuthorizedOnboardingQuery struct {
	Requester          domain.GitHubUserIdentity
	Scopes             AuthorizedScopeSet
	Repository         string
	BeforeUpdatedAt    time.Time
	BeforeOnboardingID string
	Limit              int
}

type AuthorizedOnboardingPage struct {
	Onboardings []Onboarding
	Total       int
}

type RoutineOnboardingStore interface {
	ListAuthorizedOnboardings(context.Context, AuthorizedOnboardingQuery) (AuthorizedOnboardingPage, error)
}

type RoutineOnboardingSummary struct {
	OnboardingID        string                        `json:"onboarding_id"`
	Kind                domain.OnboardingKind         `json:"kind"`
	CanonicalRepository string                        `json:"canonical_repository"`
	Status              domain.OnboardingStatus       `json:"status"`
	CompletedStepCount  int                           `json:"completed_step_count"`
	ReasonCode          string                        `json:"reason_code,omitempty"`
	LegalNextActions    []domain.OnboardingNextAction `json:"legal_next_actions"`
	OperationID         string                        `json:"operation_id,omitempty"`
	CreatedAt           time.Time                     `json:"created_at"`
	UpdatedAt           time.Time                     `json:"updated_at"`
	SettledAt           *time.Time                    `json:"settled_at,omitempty"`
}

type RoutineOnboardingPage struct {
	Metadata    RoutineProjectionMetadata  `json:"metadata"`
	Collection  RoutineCollectionMetadata  `json:"collection"`
	Repository  string                     `json:"repository,omitempty"`
	Onboardings []RoutineOnboardingSummary `json:"onboardings"`
}

type routineOnboardingCursor struct {
	Version      string    `json:"version"`
	ScopeDigest  string    `json:"scope_digest"`
	Repository   string    `json:"repository,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
	OnboardingID string    `json:"onboarding_id"`
}

type RoutineOnboardingQueryService struct {
	store      RoutineOnboardingStore
	authorizer *AuthorizationService
	profiles   RepositoryProfileSource
}

func NewRoutineOnboardingQueryService(store RoutineOnboardingStore, authorizer *AuthorizationService, profiles RepositoryProfileSource) (*RoutineOnboardingQueryService, error) {
	if store == nil || authorizer == nil || profiles == nil {
		return nil, errors.New("routine onboarding query dependencies are required")
	}
	return &RoutineOnboardingQueryService{store: store, authorizer: authorizer, profiles: profiles}, nil
}

func (s *RoutineOnboardingQueryService) List(ctx context.Context, query RoutineOnboardingQuery, observedAt time.Time) (RoutineOnboardingPage, error) {
	limit := query.Limit
	if limit == 0 {
		limit = RoutineQueryDefaultLimit
	}
	if limit < 1 || limit > RoutineQueryMaximumLimit || len(query.Cursor) > 1024 {
		return RoutineOnboardingPage{}, serviceError(ErrorInvalidInput, "onboarding collection bounds are invalid", nil)
	}
	configured, err := s.authorizer.ResolveConfiguredRequester(query.Requester)
	if err != nil {
		return RoutineOnboardingPage{}, hiddenTargetError()
	}
	scopes, err := s.authorizedScopes(ctx, configured, query.Repository)
	if err != nil {
		return RoutineOnboardingPage{}, err
	}
	cursor, err := decodeRoutineOnboardingCursor(query.Cursor)
	if err != nil {
		return RoutineOnboardingPage{}, err
	}
	if query.Cursor != "" && (cursor.ScopeDigest != scopes.Digest() || cursor.Repository != query.Repository) {
		return RoutineOnboardingPage{}, serviceError(ErrorInvalidInput, "cursor is invalid", nil)
	}
	stored, err := s.store.ListAuthorizedOnboardings(ctx, AuthorizedOnboardingQuery{Requester: configured.Identity(), Scopes: scopes, Repository: query.Repository, BeforeUpdatedAt: cursor.UpdatedAt, BeforeOnboardingID: cursor.OnboardingID, Limit: limit + 1})
	if err != nil {
		return RoutineOnboardingPage{}, classifyServiceError(err)
	}
	result := RoutineOnboardingPage{Metadata: RoutineProjectionMetadata{SchemaVersion: RoutineQuerySchemaVersion, ObservedAt: observedAt.UTC()}, Collection: RoutineCollectionMetadata{Total: stored.Total}, Repository: query.Repository}
	values := stored.Onboardings
	if len(values) > limit {
		result.Collection.Truncated = true
		values = values[:limit]
	}
	for _, value := range values {
		result.Onboardings = append(result.Onboardings, projectRoutineOnboarding(value))
	}
	if result.Collection.Truncated && len(values) != 0 {
		last := values[len(values)-1]
		result.Collection.NextCursor = encodeRoutineOnboardingCursor(routineOnboardingCursor{Version: RoutineQuerySchemaVersion, ScopeDigest: scopes.Digest(), Repository: query.Repository, UpdatedAt: last.UpdatedAt.UTC(), OnboardingID: last.OnboardingID})
	}
	result.Metadata.Digest = routineProjectionDigest(result)
	return result, nil
}

func (s *RoutineOnboardingQueryService) authorizedScopes(ctx context.Context, configured ConfiguredRequester, repository string) (AuthorizedScopeSet, error) {
	if repository != "" {
		profile, found, err := s.profiles.RepositoryProfile(ctx, repository)
		if err != nil {
			return AuthorizedScopeSet{}, classifyServiceError(err)
		}
		if !found || profile.Authority.Repository != repository {
			return AuthorizedScopeSet{}, hiddenTargetError()
		}
		scopes, err := s.authorizer.RepositoryScopes(configured, profile.Authority)
		if err != nil {
			return AuthorizedScopeSet{}, hiddenTargetError()
		}
		return scopes, nil
	}
	profiles, err := s.profiles.ListRepositoryProfiles(ctx)
	if err != nil {
		return AuthorizedScopeSet{}, classifyServiceError(err)
	}
	controller, err := s.authorizer.ControllerScopes(configured)
	if err != nil {
		return AuthorizedScopeSet{}, hiddenTargetError()
	}
	all := append([]AuthorityScope(nil), controller.scopes...)
	for _, profile := range profiles {
		repositoryScopes, scopeErr := s.authorizer.RepositoryScopes(configured, profile.Authority)
		if scopeErr == nil {
			all = append(all, repositoryScopes.scopes...)
		}
	}
	return newAuthorizedScopeSet(configured.Identity(), all...)
}

func projectRoutineOnboarding(value Onboarding) RoutineOnboardingSummary {
	result := RoutineOnboardingSummary{OnboardingID: value.OnboardingID, Kind: value.Kind, CanonicalRepository: value.CanonicalRepository, Status: value.Status, CompletedStepCount: len(value.CompletedSteps), ReasonCode: value.ReasonCode, LegalNextActions: domain.OnboardingLegalActions(value.Status, validAuthorityDigest(value.PreflightDigest), value.ReasonCode), OperationID: value.OperationID, CreatedAt: value.CreatedAt.UTC(), UpdatedAt: value.UpdatedAt.UTC()}
	if !value.SettledAt.IsZero() {
		settled := value.SettledAt.UTC()
		result.SettledAt = &settled
	}
	return result
}

func decodeRoutineOnboardingCursor(value string) (routineOnboardingCursor, error) {
	if value == "" {
		return routineOnboardingCursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return routineOnboardingCursor{}, serviceError(ErrorInvalidInput, "cursor is invalid", nil)
	}
	var cursor routineOnboardingCursor
	if json.Unmarshal(raw, &cursor) != nil || cursor.Version != RoutineQuerySchemaVersion || !validAuthorityDigest(cursor.ScopeDigest) || cursor.UpdatedAt.IsZero() || strings.TrimSpace(cursor.OnboardingID) == "" {
		return routineOnboardingCursor{}, serviceError(ErrorInvalidInput, "cursor is invalid", nil)
	}
	return cursor, nil
}

func encodeRoutineOnboardingCursor(cursor routineOnboardingCursor) string {
	raw, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(raw)
}
