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

const routineOnboardingCursorVersion = "v2"

type RoutineOnboardingQuery struct {
	Requester  Requester `json:"requester"`
	Repository string    `json:"repository,omitempty"`
	Limit      int       `json:"limit,omitempty"`
	Cursor     string    `json:"cursor,omitempty"`
}

type ControllerOnboardingQuery struct {
	Authority           ControllerReadAuthority
	ConfiguredRequester domain.GitHubUserIdentity
	CanonicalRepository string
	BeforeUpdatedAt     time.Time
	BeforeOnboardingID  string
	Limit               int
}

type ControllerOnboardingPage struct {
	Onboardings []Onboarding
	Total       int
}

type ControllerOnboardingCollectionStore interface {
	ListControllerOnboardings(context.Context, ControllerOnboardingQuery) (ControllerOnboardingPage, error)
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
	ReaderDigest string    `json:"reader_digest"`
	Repository   string    `json:"repository,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
	OnboardingID string    `json:"onboarding_id"`
}

type RoutineOnboardingQueryService struct {
	store      ControllerOnboardingCollectionStore
	authorizer *AuthorizationService
}

func NewRoutineOnboardingQueryService(store ControllerOnboardingCollectionStore, authorizer *AuthorizationService) (*RoutineOnboardingQueryService, error) {
	if store == nil || authorizer == nil {
		return nil, errors.New("routine onboarding query dependencies are required")
	}
	return &RoutineOnboardingQueryService{store: store, authorizer: authorizer}, nil
}

func (s *RoutineOnboardingQueryService) List(ctx context.Context, query RoutineOnboardingQuery, observedAt time.Time) (RoutineOnboardingPage, error) {
	configured, err := s.authorizer.ResolveConfiguredRequester(query.Requester)
	if err != nil {
		return RoutineOnboardingPage{}, hiddenTargetError()
	}
	reader, err := s.authorizer.ControllerReadCollectionAuthority(configured)
	if err != nil {
		return RoutineOnboardingPage{}, hiddenTargetError()
	}
	return s.listController(ctx, reader, configured, query, observedAt)
}

func (s *RoutineOnboardingQueryService) ListController(ctx context.Context, authority ControllerReadAuthority, query RoutineOnboardingQuery, observedAt time.Time) (RoutineOnboardingPage, error) {
	configured, err := s.authorizer.ResolveConfiguredRequester(query.Requester)
	if err != nil {
		return RoutineOnboardingPage{}, hiddenTargetError()
	}
	expected, err := s.authorizer.ControllerReadCollectionAuthority(configured)
	if err != nil || !authority.Valid() || expected.Digest() != authority.Digest() {
		return RoutineOnboardingPage{}, serviceError(ErrorInternal, "controller onboarding collection authority is unavailable", nil)
	}
	return s.listController(ctx, authority, configured, query, observedAt)
}

func (s *RoutineOnboardingQueryService) listController(ctx context.Context, authority ControllerReadAuthority, configured ConfiguredRequester, query RoutineOnboardingQuery, observedAt time.Time) (RoutineOnboardingPage, error) {
	limit := query.Limit
	if limit == 0 {
		limit = RoutineQueryDefaultLimit
	}
	if limit < 1 || limit > RoutineQueryMaximumLimit || len(query.Cursor) > 1024 {
		return RoutineOnboardingPage{}, serviceError(ErrorInvalidInput, "onboarding collection bounds are invalid", nil)
	}
	if query.Repository != "" && !validCanonicalRepositoryFilter(query.Repository) {
		return RoutineOnboardingPage{}, serviceError(ErrorInvalidInput, "onboarding repository filter is invalid", nil)
	}
	cursor, err := decodeRoutineOnboardingCursor(query.Cursor)
	if err != nil {
		return RoutineOnboardingPage{}, err
	}
	if query.Cursor != "" && (cursor.ReaderDigest != authority.Digest() || cursor.Repository != query.Repository) {
		return RoutineOnboardingPage{}, serviceError(ErrorInvalidInput, "cursor is invalid", nil)
	}
	stored, err := s.store.ListControllerOnboardings(ctx, ControllerOnboardingQuery{Authority: authority, ConfiguredRequester: configured.Identity(), CanonicalRepository: query.Repository, BeforeUpdatedAt: cursor.UpdatedAt, BeforeOnboardingID: cursor.OnboardingID, Limit: limit + 1})
	if err != nil {
		return RoutineOnboardingPage{}, classifyServiceError(err)
	}
	if stored.Total < len(stored.Onboardings) || len(stored.Onboardings) > limit+1 {
		return RoutineOnboardingPage{}, serviceError(ErrorInternal, "controller onboarding authority conflicts", nil)
	}
	previous := cursor
	for _, value := range stored.Onboardings {
		if !validControllerOnboardingCollectionRow(value, configured.Identity(), query.Repository, previous) {
			return RoutineOnboardingPage{}, serviceError(ErrorInternal, "controller onboarding authority conflicts", nil)
		}
		previous.UpdatedAt, previous.OnboardingID = value.UpdatedAt.UTC(), value.OnboardingID
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
		result.Collection.NextCursor = encodeRoutineOnboardingCursor(routineOnboardingCursor{Version: routineOnboardingCursorVersion, ReaderDigest: authority.Digest(), Repository: query.Repository, UpdatedAt: last.UpdatedAt.UTC(), OnboardingID: last.OnboardingID})
	}
	result.Metadata.Digest = routineProjectionDigest(result)
	return result, nil
}

func validControllerOnboardingCollectionRow(value Onboarding, configured domain.GitHubUserIdentity, repository string, before routineOnboardingCursor) bool {
	if strings.TrimSpace(value.OnboardingID) == "" || strings.ContainsRune(value.OnboardingID, '\x00') || !validCanonicalRepositoryFilter(value.CanonicalRepository) || repository != "" && value.CanonicalRepository != repository || value.UpdatedAt.IsZero() {
		return false
	}
	if !ControllerOnboardingCollectionLifecycleValid(value, configured) {
		return false
	}
	if before.OnboardingID == "" {
		return true
	}
	updated := value.UpdatedAt.UTC()
	return updated.Before(before.UpdatedAt) || updated.Equal(before.UpdatedAt) && value.OnboardingID < before.OnboardingID
}

// ControllerOnboardingCollectionLifecycleValid closes the collection contract
// over the persisted onboarding lifecycle. Pre-binding rows remain requester
// local; accepted and later rows require complete binding and operation
// acceptance evidence before they can enter the Controller-wide collection.
func ControllerOnboardingCollectionLifecycleValid(value Onboarding, configured domain.GitHubUserIdentity) bool {
	plan, ok := domain.OnboardingStepPlan(value.Kind)
	if !ok || configured.Validate() != nil || value.Requester.Validate() != nil || value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() || value.UpdatedAt.Before(value.CreatedAt) || len(value.CompletedSteps) > len(plan) {
		return false
	}
	for index, step := range value.CompletedSteps {
		if step != plan[index] {
			return false
		}
	}
	prebindingFieldsEmpty := value.ProfileID == "" && value.ProfileDigest == "" && value.RepositoryBindingDigest == ""
	operationFieldsEmpty := value.OperationID == "" && value.PreviewDigest == "" && value.AcceptedAt.IsZero()
	switch value.Status {
	case domain.OnboardingOpened:
		return value.Requester.Equal(configured) && prebindingFieldsEmpty && operationFieldsEmpty && value.PreflightDigest == "" && len(value.CompletedSteps) == 0 && value.SettledAt.IsZero()
	case domain.OnboardingPreflightReady:
		return value.Requester.Equal(configured) && prebindingFieldsEmpty && operationFieldsEmpty && validAuthorityDigest(value.PreflightDigest) && len(value.CompletedSteps) == 0 && value.SettledAt.IsZero()
	case domain.OnboardingCancelled:
		return value.Requester.Equal(configured) && prebindingFieldsEmpty && operationFieldsEmpty && (value.PreflightDigest == "" || validAuthorityDigest(value.PreflightDigest)) && len(value.CompletedSteps) == 0 && !value.SettledAt.IsZero() && value.SettledAt.Equal(value.UpdatedAt)
	case domain.OnboardingAccepted, domain.OnboardingRunning, domain.OnboardingWaitingForOperator, domain.OnboardingConflict, domain.OnboardingReadyDisabled:
		if strings.TrimSpace(value.OperationID) == "" || strings.ContainsRune(value.OperationID, '\x00') || strings.TrimSpace(value.ProfileID) == "" || strings.ContainsRune(value.ProfileID, '\x00') || !validAuthorityDigest(value.ProfileDigest) || !validAuthorityDigest(value.RepositoryBindingDigest) || !validAuthorityDigest(value.PreflightDigest) || !validAuthorityDigest(value.PreviewDigest) || value.AcceptedAt.IsZero() || value.AcceptedAt.Before(value.CreatedAt) || value.UpdatedAt.Before(value.AcceptedAt) {
			return false
		}
	default:
		return false
	}
	switch value.Status {
	case domain.OnboardingAccepted:
		return len(value.CompletedSteps) == 0 && value.SettledAt.IsZero()
	case domain.OnboardingRunning, domain.OnboardingWaitingForOperator:
		return len(value.CompletedSteps) < len(plan) && value.SettledAt.IsZero()
	case domain.OnboardingConflict:
		return len(value.CompletedSteps) < len(plan) && !value.SettledAt.IsZero() && value.SettledAt.Equal(value.UpdatedAt)
	case domain.OnboardingReadyDisabled:
		return len(value.CompletedSteps) == len(plan) && !value.SettledAt.IsZero() && value.SettledAt.Equal(value.UpdatedAt)
	default:
		return false
	}
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
	if json.Unmarshal(raw, &cursor) != nil || cursor.Version != routineOnboardingCursorVersion || !validAuthorityDigest(cursor.ReaderDigest) || cursor.Repository != "" && !validCanonicalRepositoryFilter(cursor.Repository) || cursor.UpdatedAt.IsZero() || strings.TrimSpace(cursor.OnboardingID) == "" || strings.ContainsRune(cursor.OnboardingID, '\x00') {
		return routineOnboardingCursor{}, serviceError(ErrorInvalidInput, "cursor is invalid", nil)
	}
	return cursor, nil
}

func encodeRoutineOnboardingCursor(cursor routineOnboardingCursor) string {
	raw, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(raw)
}
