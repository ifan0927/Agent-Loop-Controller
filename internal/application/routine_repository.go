package application

import (
	"context"
	"errors"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type RoutineRepositorySummary struct {
	Repository               string                           `json:"repository"`
	LifecycleIntent          RepositoryLifecycleIntent        `json:"lifecycle_intent"`
	Readiness                domain.RepositoryReadinessStatus `json:"readiness"`
	ReadinessReasonCode      string                           `json:"readiness_reason_code"`
	Available                bool                             `json:"available"`
	AvailabilityReasonCode   string                           `json:"availability_reason_code"`
	ActiveRunID              string                           `json:"active_run_id,omitempty"`
	ConfigurationConvergence domain.RepositoryReadinessStatus `json:"configuration_convergence"`
	ConfigurationReasonCode  string                           `json:"configuration_reason_code"`
	Onboarding               *RoutineOnboardingSummary        `json:"onboarding,omitempty"`
	LastObservedAt           time.Time                        `json:"last_observed_at"`
	Acceptance               RoutineRepositoryAcceptance      `json:"acceptance"`
}

type RoutineRepositoryAcceptanceConclusion string

const (
	RoutineRepositoryAcceptingNewWork RoutineRepositoryAcceptanceConclusion = "accepting_new_work"
	RoutineRepositoryReadyDisabled    RoutineRepositoryAcceptanceConclusion = "ready_disabled"
	RoutineRepositoryNotReady         RoutineRepositoryAcceptanceConclusion = "not_ready"
	RoutineRepositoryConflict         RoutineRepositoryAcceptanceConclusion = "conflict"
	RoutineRepositoryUnknown          RoutineRepositoryAcceptanceConclusion = "unknown"
	RoutineRepositoryUnavailable      RoutineRepositoryAcceptanceConclusion = "unavailable"
)

type RoutineRepositoryOperatorDirection string

const (
	RoutineRepositoryDirectionNone                  RoutineRepositoryOperatorDirection = "none"
	RoutineRepositoryDirectionEnable                RoutineRepositoryOperatorDirection = "enable_repository"
	RoutineRepositoryDirectionResolveReadiness      RoutineRepositoryOperatorDirection = "resolve_readiness"
	RoutineRepositoryDirectionResolveConflict       RoutineRepositoryOperatorDirection = "resolve_conflict"
	RoutineRepositoryDirectionRefreshAuthority      RoutineRepositoryOperatorDirection = "refresh_authority"
	RoutineRepositoryDirectionInspectUnavailability RoutineRepositoryOperatorDirection = "inspect_unavailability"
)

type RoutineRepositoryAction string

const RoutineRepositoryActionEnable RoutineRepositoryAction = "enable_repository"

type RoutineRepositoryAcceptance struct {
	Conclusion    RoutineRepositoryAcceptanceConclusion `json:"conclusion"`
	ReasonCode    string                                `json:"reason_code"`
	NextDirection RoutineRepositoryOperatorDirection    `json:"next_direction"`
}

type RoutineRepositoryDimension struct {
	Dimension  domain.RepositoryReadinessDimension `json:"dimension"`
	Status     domain.RepositoryReadinessStatus    `json:"status"`
	ReasonCode string                              `json:"reason_code"`
	ObservedAt time.Time                           `json:"observed_at"`
}

type RoutineRepositoryDetail struct {
	Metadata         RoutineProjectionMetadata    `json:"metadata"`
	Repository       RoutineRepositorySummary     `json:"repository"`
	Dimensions       []RoutineRepositoryDimension `json:"dimensions"`
	LegalNextActions []RoutineRepositoryAction    `json:"legal_next_actions"`
}

type RoutineRepositoryPage struct {
	Metadata     RoutineProjectionMetadata  `json:"metadata"`
	Collection   RoutineCollectionMetadata  `json:"collection"`
	Repositories []RoutineRepositorySummary `json:"repositories"`
}

type RoutineRepositoryOnboardingSource interface {
	CurrentRepositoryOnboarding(context.Context, string) (Onboarding, bool, error)
}

type RoutineRepositoryQueryService struct {
	repositories *RepositoryService
	onboarding   RoutineRepositoryOnboardingSource
}

func NewRoutineRepositoryQueryService(repositories *RepositoryService, onboarding RoutineRepositoryOnboardingSource) (*RoutineRepositoryQueryService, error) {
	if repositories == nil {
		return nil, errors.New("routine repository query dependencies are required")
	}
	return &RoutineRepositoryQueryService{repositories: repositories, onboarding: onboarding}, nil
}

func (s *RoutineRepositoryQueryService) List(ctx context.Context, requester Requester, limit int, cursor string, observedAt time.Time) (RoutineRepositoryPage, error) {
	configured, err := s.repositories.authorizer.ResolveConfiguredRequester(requester)
	if err != nil {
		return RoutineRepositoryPage{}, hiddenTargetError()
	}
	reader, err := s.repositories.authorizer.ControllerReadCollectionAuthority(configured)
	if err != nil {
		return RoutineRepositoryPage{}, hiddenTargetError()
	}
	return s.ListController(ctx, reader, limit, cursor, observedAt)
}

func (s *RoutineRepositoryQueryService) ListController(ctx context.Context, authority ControllerReadAuthority, limit int, cursor string, observedAt time.Time) (RoutineRepositoryPage, error) {
	if limit == 0 {
		limit = RoutineQueryDefaultLimit
	}
	page, err := s.repositories.ListController(ctx, authority, limit, cursor)
	if err != nil {
		return RoutineRepositoryPage{}, err
	}
	result := RoutineRepositoryPage{Metadata: RoutineProjectionMetadata{SchemaVersion: RoutineQuerySchemaVersion, ObservedAt: observedAt.UTC()}, Collection: RoutineCollectionMetadata{Total: page.Total, Truncated: page.HasMore, NextCursor: page.NextCursor}}
	for _, repository := range page.Repositories {
		summary, projectErr := s.summary(ctx, repository)
		if projectErr != nil {
			return RoutineRepositoryPage{}, projectErr
		}
		result.Repositories = append(result.Repositories, summary)
	}
	result.Metadata.Digest = routineProjectionDigest(result)
	return result, nil
}

func (s *RoutineRepositoryQueryService) Detail(ctx context.Context, requester Requester, repository string, observedAt time.Time) (RoutineRepositoryDetail, error) {
	projection, err := s.repositories.Inspect(ctx, requester, repository)
	if err != nil {
		return RoutineRepositoryDetail{}, err
	}
	summary, err := s.summary(ctx, projection)
	if err != nil {
		return RoutineRepositoryDetail{}, err
	}
	result := RoutineRepositoryDetail{Metadata: RoutineProjectionMetadata{SchemaVersion: RoutineQuerySchemaVersion, ObservedAt: observedAt.UTC()}, Repository: summary}
	result.LegalNextActions = routineRepositoryLegalNextActions(projection)
	for _, dimension := range projection.Readiness.Dimensions {
		result.Dimensions = append(result.Dimensions, RoutineRepositoryDimension{Dimension: dimension.Dimension, Status: dimension.Status, ReasonCode: dimension.ReasonCode, ObservedAt: dimension.ObservedAt.UTC()})
	}
	result.Metadata.Digest = routineProjectionDigest(result)
	return result, nil
}

func (s *RoutineRepositoryQueryService) summary(ctx context.Context, projection RepositoryProjection) (RoutineRepositorySummary, error) {
	result := RoutineRepositorySummary{Repository: projection.Lifecycle.Repository, LifecycleIntent: projection.Lifecycle.Intent, Readiness: projection.Readiness.Status, ReadinessReasonCode: projection.Readiness.ReasonCode, Available: projection.Availability.Available, AvailabilityReasonCode: projection.Availability.ReasonCode, ActiveRunID: projection.Availability.ActiveRun, ConfigurationConvergence: domain.RepositoryUnknown, ConfigurationReasonCode: "observation_missing", LastObservedAt: projection.Readiness.ObservedAt.UTC()}
	for _, dimension := range projection.Readiness.Dimensions {
		if dimension.Dimension == domain.ReadinessConfigurationConvergence {
			result.ConfigurationConvergence, result.ConfigurationReasonCode = dimension.Status, dimension.ReasonCode
		}
	}
	if s.onboarding != nil {
		value, found, err := s.onboarding.CurrentRepositoryOnboarding(ctx, projection.Lifecycle.Repository)
		if err != nil {
			return RoutineRepositorySummary{}, classifyServiceError(err)
		}
		if found {
			onboarding := projectRoutineOnboarding(value)
			result.Onboarding = &onboarding
		}
	}
	result.Acceptance = routineRepositoryAcceptance(projection)
	if result.Acceptance.Conclusion == RoutineRepositoryConflict || result.Acceptance.Conclusion == RoutineRepositoryUnknown {
		result.Available = false
	}
	return result, nil
}

func routineRepositoryAcceptance(projection RepositoryProjection) RoutineRepositoryAcceptance {
	readiness := projection.Readiness
	switch readiness.Status {
	case domain.RepositoryConflict:
		return RoutineRepositoryAcceptance{Conclusion: RoutineRepositoryConflict, ReasonCode: readiness.ReasonCode, NextDirection: RoutineRepositoryDirectionResolveConflict}
	case domain.RepositoryUnknown:
		return RoutineRepositoryAcceptance{Conclusion: RoutineRepositoryUnknown, ReasonCode: readiness.ReasonCode, NextDirection: RoutineRepositoryDirectionRefreshAuthority}
	case domain.RepositoryNotReady:
		return RoutineRepositoryAcceptance{Conclusion: RoutineRepositoryNotReady, ReasonCode: readiness.ReasonCode, NextDirection: RoutineRepositoryDirectionResolveReadiness}
	}
	if projection.Lifecycle.Intent == RepositoryDisabled && projection.Recheck == nil && projection.Removal == nil && readiness.Status == domain.RepositoryReady {
		return RoutineRepositoryAcceptance{Conclusion: RoutineRepositoryReadyDisabled, ReasonCode: "repository_disabled", NextDirection: RoutineRepositoryDirectionEnable}
	}
	if projection.Availability.Available {
		return RoutineRepositoryAcceptance{Conclusion: RoutineRepositoryAcceptingNewWork, ReasonCode: projection.Availability.ReasonCode, NextDirection: RoutineRepositoryDirectionNone}
	}
	return RoutineRepositoryAcceptance{Conclusion: RoutineRepositoryUnavailable, ReasonCode: projection.Availability.ReasonCode, NextDirection: RoutineRepositoryDirectionInspectUnavailability}
}

func routineRepositoryLegalNextActions(projection RepositoryProjection) []RoutineRepositoryAction {
	if projection.Lifecycle.Intent == RepositoryDisabled && projection.Readiness.Status == domain.RepositoryReady && projection.Recheck == nil && projection.Removal == nil {
		return []RoutineRepositoryAction{RoutineRepositoryActionEnable}
	}
	return []RoutineRepositoryAction{}
}
