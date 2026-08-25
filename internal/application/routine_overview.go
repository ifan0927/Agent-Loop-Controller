package application

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type RoutineActionableItem struct {
	ItemID        string             `json:"item_id"`
	Scope         AuthorityScopeKind `json:"scope"`
	TargetID      string             `json:"target_id"`
	Severity      string             `json:"severity"`
	ReasonCode    string             `json:"reason_code"`
	Navigation    string             `json:"navigation"`
	ObservedAt    time.Time          `json:"observed_at"`
	OfferID       string             `json:"offer_id,omitempty"`
	ProcedureCode string             `json:"procedure_code,omitempty"`
}

type RoutineRepositoryCounts struct {
	Total       int `json:"total"`
	Enabled     int `json:"enabled"`
	Disabled    int `json:"disabled"`
	Ready       int `json:"ready"`
	NotReady    int `json:"not_ready"`
	Unavailable int `json:"unavailable"`
}

type RoutineRunCounts struct {
	Active          int                 `json:"active_total"`
	Recent          int                 `json:"recent_total"`
	ActiveRuns      []RoutineRunSummary `json:"active"`
	RecentRuns      []RoutineRunSummary `json:"recent"`
	ActiveTruncated bool                `json:"active_truncated"`
	RecentTruncated bool                `json:"recent_truncated"`
}

type RoutineOnboardingOverview struct {
	Active    []RoutineOnboardingSummary `json:"active"`
	Total     int                        `json:"total"`
	Truncated bool                       `json:"truncated"`
}

// RoutinePersistedOverviewSnapshot is returned by exactly one store call. A
// SQLite adapter must populate it from one read transaction; runtime remains a
// separately timestamped observation.
type RoutinePersistedOverviewSnapshot struct {
	ObservedAt          time.Time
	Capacity            CapacityProjection
	AdmissionEnabled    bool
	QueueSnapshot       *QueueSnapshot
	QueueRepositories   []RoutineQueueRepositoryAuthority
	QueueAttention      *RoutineQueueAttention
	Runs                RoutineRunCounts
	Attention           []RoutineAttentionSummary
	AttentionTotal      int
	AttentionTruncated  bool
	Repositories        RoutineRepositoryCounts
	Onboarding          RoutineOnboardingOverview
	Settings            RoutineSettingsProjection
	Configuration       ConfigurationAuthority
	ActiveDraft         *ConfigurationDraft
	Actionable          []RoutineActionableItem
	ActionableTotal     int
	ActionableTruncated bool
}

// RoutineQueueRepositoryAuthority is the minimum persisted authority needed
// to remove private queue binding identifiers from an Overview response.
type RoutineQueueRepositoryAuthority struct {
	ProfileID     string
	BindingDigest string
	Repository    string
}

type RoutineOverviewStore interface {
	ReadRoutineOverviewSnapshot(context.Context, AuthorizedScopeSet, domain.GitHubUserIdentity, int) (RoutinePersistedOverviewSnapshot, error)
}

type RoutineOverviewProjection struct {
	Metadata            RoutineProjectionMetadata `json:"metadata"`
	PersistedAt         time.Time                 `json:"persisted_observed_at"`
	Readiness           AggregateReadiness        `json:"readiness"`
	Worker              RuntimeObservation        `json:"worker"`
	Capacity            CapacityProjection        `json:"capacity"`
	AdmissionEnabled    bool                      `json:"admission_enabled"`
	Queue               RoutineQueueProjection    `json:"queue"`
	Runs                RoutineRunCounts          `json:"runs"`
	Attention           []RoutineAttentionSummary `json:"active_attention"`
	AttentionTotal      int                       `json:"active_attention_total"`
	AttentionTruncated  bool                      `json:"active_attention_truncated"`
	Repositories        RoutineRepositoryCounts   `json:"repositories"`
	Onboarding          RoutineOnboardingOverview `json:"onboarding"`
	Settings            RoutineSettingsProjection `json:"settings"`
	Actionable          []RoutineActionableItem   `json:"actionable_items"`
	ActionableTotal     int                       `json:"actionable_total"`
	ActionableTruncated bool                      `json:"actionable_truncated"`
}

type RoutineOverviewService struct {
	store      RoutineOverviewStore
	authorizer *AuthorizationService
	runtime    *RuntimeObservationService
	settings   *RoutineSettingsService
}

func NewRoutineOverviewService(store RoutineOverviewStore, authorizer *AuthorizationService, runtime *RuntimeObservationService, settings ...*RoutineSettingsService) (*RoutineOverviewService, error) {
	if store == nil || authorizer == nil || runtime == nil {
		return nil, errors.New("routine overview dependencies are required")
	}
	service := &RoutineOverviewService{store: store, authorizer: authorizer, runtime: runtime}
	if len(settings) > 1 {
		return nil, errors.New("routine overview settings dependency is invalid")
	}
	if len(settings) == 1 {
		service.settings = settings[0]
	}
	return service, nil
}

func (s *RoutineOverviewService) Get(ctx context.Context, requester Requester, observedAt time.Time) (RoutineOverviewProjection, error) {
	configured, err := s.authorizer.ResolveConfiguredRequester(requester)
	if err != nil {
		return RoutineOverviewProjection{}, hiddenTargetError()
	}
	scopes, err := s.authorizer.ControllerScopes(configured)
	if err != nil || !scopes.HasController() {
		return RoutineOverviewProjection{}, hiddenTargetError()
	}
	persisted, err := s.store.ReadRoutineOverviewSnapshot(ctx, scopes, configured.Identity(), RoutineOverviewItemLimit)
	if err != nil || persisted.ObservedAt.IsZero() {
		return RoutineOverviewProjection{}, serviceError(ErrorInternal, "routine overview snapshot is unavailable", err)
	}
	runtime, err := s.runtime.Observe(ctx, requester, observedAt.UTC())
	if err != nil {
		return RoutineOverviewProjection{}, err
	}
	if persisted.Configuration.Desired.GenerationID != 0 {
		if s.settings == nil {
			return RoutineOverviewProjection{}, serviceError(ErrorInternal, "routine overview settings projection is unavailable", nil)
		}
		persisted.Settings, err = s.settings.projectPersisted(persisted.Configuration, persisted.ActiveDraft, runtime, observedAt.UTC())
		if err != nil {
			return RoutineOverviewProjection{}, err
		}
		persisted.AdmissionEnabled = persisted.Settings.Settings.Admission.Enabled
	}
	readinessInputs := []AggregateReadiness{aggregateRuntimeReadiness(runtime), aggregateConfigurationReadiness(persisted.Settings.Convergence)}
	if len(persisted.Attention) != 0 {
		readinessInputs = append(readinessInputs, AggregateAttentionRequired)
	}
	queue, err := projectRoutineOverviewQueue(persisted, observedAt.UTC())
	if err != nil {
		return RoutineOverviewProjection{}, err
	}
	result := RoutineOverviewProjection{Metadata: RoutineProjectionMetadata{SchemaVersion: RoutineQuerySchemaVersion, ObservedAt: observedAt.UTC()}, PersistedAt: persisted.ObservedAt.UTC(), Readiness: ClassifyAggregateReadiness(readinessInputs...), Worker: runtime, Capacity: persisted.Capacity, AdmissionEnabled: persisted.AdmissionEnabled, Queue: queue, Runs: persisted.Runs, Attention: persisted.Attention, AttentionTotal: persisted.AttentionTotal, AttentionTruncated: persisted.AttentionTruncated, Repositories: persisted.Repositories, Onboarding: persisted.Onboarding, Settings: persisted.Settings, ActionableTotal: persisted.ActionableTotal, ActionableTruncated: persisted.ActionableTruncated}
	if result.ActionableTotal == 0 {
		result.ActionableTotal = len(persisted.Actionable)
	}
	actionable := append([]RoutineActionableItem(nil), persisted.Actionable...)
	sort.Slice(actionable, func(i, j int) bool {
		left, right := routineSeverityRank(actionable[i].Severity), routineSeverityRank(actionable[j].Severity)
		if left != right {
			return left < right
		}
		if !actionable[i].ObservedAt.Equal(actionable[j].ObservedAt) {
			return actionable[i].ObservedAt.Before(actionable[j].ObservedAt)
		}
		if actionable[i].Scope != actionable[j].Scope {
			return actionable[i].Scope < actionable[j].Scope
		}
		if actionable[i].TargetID != actionable[j].TargetID {
			return actionable[i].TargetID < actionable[j].TargetID
		}
		return actionable[i].ItemID < actionable[j].ItemID
	})
	if len(actionable) > RoutineOverviewItemLimit {
		result.ActionableTruncated = true
		actionable = actionable[:RoutineOverviewItemLimit]
	}
	result.Actionable = actionable
	result.Metadata.Digest = routineProjectionDigest(result)
	return result, nil
}

func projectRoutineOverviewQueue(snapshot RoutinePersistedOverviewSnapshot, observedAt time.Time) (RoutineQueueProjection, error) {
	result := RoutineQueueProjection{Metadata: RoutineProjectionMetadata{SchemaVersion: RoutineQuerySchemaVersion, ObservedAt: observedAt}, State: RoutineQueueAbsent, ReasonCode: "snapshot_absent", Candidates: []RoutineQueueCandidate{}}
	if snapshot.QueueSnapshot == nil {
		result.Metadata.Digest = routineProjectionDigest(result)
		return result, nil
	}
	queue := *snapshot.QueueSnapshot
	if queue.Validate() != nil {
		return RoutineQueueProjection{}, serviceError(ErrorInternal, "queue snapshot is corrupt", nil)
	}
	repositories := map[string]RoutineQueueRepositoryAuthority{}
	for _, authority := range snapshot.QueueRepositories {
		repositories[authority.ProfileID] = authority
	}
	result.State, result.ReasonCode, result.SnapshotDigest = RoutineQueueCurrent, "latest_complete_snapshot", queue.Digest
	queueAt := queue.ObservedAt.UTC()
	result.SnapshotObservedAt = &queueAt
	for index, candidate := range queue.Candidates {
		projected := RoutineQueueCandidate{Rank: index + 1, LinearIdentifier: candidate.TeamKey + "-" + integerString(candidate.IssueSequence), Priority: candidate.Priority, Classification: candidate.Classification, ReasonCode: candidate.ReasonCode}
		if authority, ok := repositories[candidate.RepositoryProfileID]; ok && authority.BindingDigest == candidate.RepositoryBindingDigest {
			projected.Repository = authority.Repository
		}
		result.Candidates = append(result.Candidates, projected)
	}
	result.Collection.Total = len(result.Candidates)
	if snapshot.QueueAttention != nil && snapshot.QueueAttention.OccurredAt.After(queue.ObservedAt) {
		result.State, result.ReasonCode = RoutineQueueStale, "newer_scheduler_attention"
		if snapshot.QueueAttention.Degraded {
			result.State = RoutineQueueDegraded
		}
		if strings.TrimSpace(snapshot.QueueAttention.ReasonCode) != "" {
			result.ReasonCode = snapshot.QueueAttention.ReasonCode
		}
	}
	result.Metadata.Digest = routineProjectionDigest(result)
	return result, nil
}

func aggregateRuntimeReadiness(runtime RuntimeObservation) AggregateReadiness {
	switch runtime.Liveness {
	case RuntimeLivenessFresh:
		return AggregateReady
	case RuntimeLivenessStale:
		return AggregateStale
	case RuntimeLivenessOffline:
		return AggregateOffline
	case RuntimeLivenessConflict:
		return AggregateConflict
	default:
		return AggregateUnknown
	}
}

func aggregateConfigurationReadiness(configuration ConfigurationConvergenceProjection) AggregateReadiness {
	switch configuration.State {
	case ConfigurationReady:
		return AggregateReady
	case ConfigurationRestartRequired:
		return AggregateRestartRequired
	case ConfigurationStarting:
		return AggregateDegraded
	case ConfigurationStale:
		return AggregateStale
	case ConfigurationOffline:
		return AggregateOffline
	case ConfigurationConflict:
		return AggregateConflict
	default:
		return AggregateUnknown
	}
}

func routineSeverityRank(value string) int {
	switch value {
	case "critical":
		return 0
	case "error":
		return 1
	case "warning":
		return 2
	case "info":
		return 3
	default:
		return 4
	}
}
