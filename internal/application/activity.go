package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	ActivitySchemaVersion        = "v1"
	ActivityDefaultLimit         = 50
	ActivityMaximumLimit         = 100
	OperationHistoryDefaultLimit = 25
	OperationHistoryMaximumLimit = 100
)

var (
	ErrActivityNotFound = errors.New("activity event not found")
	ErrActivityConflict = errors.New("activity event conflicts")
)

type ActivityCategory string

const (
	ActivityRun               ActivityCategory = "run"
	ActivityAttention         ActivityCategory = "attention"
	ActivityOperation         ActivityCategory = "operation"
	ActivityRepository        ActivityCategory = "repository"
	ActivityOnboarding        ActivityCategory = "onboarding"
	ActivityConfiguration     ActivityCategory = "configuration"
	ActivityWorker            ActivityCategory = "worker"
	ActivityAdmissionCapacity ActivityCategory = "admission_capacity"
)

type ActivityActor string

const (
	ActivityActorController         ActivityActor = "controller"
	ActivityActorConfiguredOperator ActivityActor = "configured_operator"
	ActivityActorExternalAuthority  ActivityActor = "external_authority"
	ActivityActorUnknown            ActivityActor = "unknown"
)

type ActivityEventKind string

const (
	ActivityRunTransition             ActivityEventKind = "run_transition"
	ActivityAttentionOpened           ActivityEventKind = "attention_opened"
	ActivityAttentionSuperseded       ActivityEventKind = "attention_superseded"
	ActivityAttentionResolved         ActivityEventKind = "attention_resolved"
	ActivityOperationSettled          ActivityEventKind = "operation_settled"
	ActivityRepositoryLifecycleChange ActivityEventKind = "repository_lifecycle_changed"
	ActivityRepositoryGateChange      ActivityEventKind = "repository_gate_changed"
	ActivityOnboardingMilestone       ActivityEventKind = "onboarding_milestone"
	ActivityOnboardingCompleted       ActivityEventKind = "onboarding_completed"
	ActivityOnboardingCancelled       ActivityEventKind = "onboarding_cancelled"
	ActivityOnboardingConflict        ActivityEventKind = "onboarding_conflicted"
	ActivityConfigurationApplied      ActivityEventKind = "configuration_applied"
	ActivityConfigurationRolledBack   ActivityEventKind = "configuration_rolled_back"
	ActivityConfigurationDrifted      ActivityEventKind = "configuration_drifted"
	ActivityConfigurationConverged    ActivityEventKind = "configuration_converged"
	ActivityWorkerReadinessChange     ActivityEventKind = "worker_readiness_changed"
	ActivityAdmissionConflict         ActivityEventKind = "admission_capacity_conflict"
)

type ActivityReasonCode string

const (
	ActivityReasonStateChanged     ActivityReasonCode = "state_changed"
	ActivityReasonOpened           ActivityReasonCode = "opened"
	ActivityReasonSuperseded       ActivityReasonCode = "superseded"
	ActivityReasonResolved         ActivityReasonCode = "resolved"
	ActivityReasonSucceeded        ActivityReasonCode = "succeeded"
	ActivityReasonFailed           ActivityReasonCode = "failed"
	ActivityReasonConflict         ActivityReasonCode = "conflict"
	ActivityReasonAmbiguous        ActivityReasonCode = "ambiguous"
	ActivityReasonMilestone        ActivityReasonCode = "milestone"
	ActivityReasonCompleted        ActivityReasonCode = "completed"
	ActivityReasonCancelled        ActivityReasonCode = "cancelled"
	ActivityReasonDriftDetected    ActivityReasonCode = "drift_detected"
	ActivityReasonConverged        ActivityReasonCode = "converged"
	ActivityReasonReadinessChanged ActivityReasonCode = "readiness_changed"
	ActivityReasonCapacityConflict ActivityReasonCode = "capacity_conflict"
)

type ActivityCoverageState string

const (
	ActivityCoverageComplete    ActivityCoverageState = "complete"
	ActivityCoverageBackfilling ActivityCoverageState = "backfilling"
	ActivityCoverageDegraded    ActivityCoverageState = "degraded"
	ActivityCoverageUnknown     ActivityCoverageState = "unknown"
	ActivityCoverageConflict    ActivityCoverageState = "conflict"
)

type ActivityIngestionClass string

const (
	ActivityIngestionCurrent  ActivityIngestionClass = "current"
	ActivityIngestionBackfill ActivityIngestionClass = "backfill"
	ActivityIngestionRuntime  ActivityIngestionClass = "runtime"
)

type ActivityEventCoverage struct {
	IngestionClass        ActivityIngestionClass `json:"ingestion_class"`
	LegacyReconstructable bool                   `json:"legacy_reconstructable"`
}

type ActivityCoverage struct {
	State                   ActivityCoverageState `json:"state"`
	ReasonCode              string                `json:"reason_code"`
	ProvenFrom              *time.Time            `json:"proven_from,omitempty"`
	IndexedThrough          *time.Time            `json:"indexed_through,omitempty"`
	FreshnessObservedAt     *time.Time            `json:"freshness_observed_at,omitempty"`
	LegacyLimitations       []string              `json:"legacy_limitations"`
	BackfillSourcesComplete int                   `json:"backfill_sources_complete"`
	BackfillSourcesTotal    int                   `json:"backfill_sources_total"`
}

type ActivityRelatedResource struct {
	Kind AuthorityScopeKind `json:"kind"`
	ID   string             `json:"id"`
}

type ActivityEvent struct {
	SchemaVersion    string                    `json:"schema_version"`
	EventID          string                    `json:"event_id"`
	Category         ActivityCategory          `json:"category"`
	EventKind        ActivityEventKind         `json:"event_kind"`
	Actor            ActivityActor             `json:"actor"`
	Scope            AuthorityScopeKind        `json:"scope"`
	TargetID         string                    `json:"target_id"`
	ReasonCode       ActivityReasonCode        `json:"reason_code"`
	PriorState       string                    `json:"prior_state,omitempty"`
	ResultingState   string                    `json:"resulting_state,omitempty"`
	PriorVersion     int64                     `json:"prior_version,omitempty"`
	ResultingVersion int64                     `json:"resulting_version,omitempty"`
	OccurredAt       time.Time                 `json:"occurred_at"`
	ObservedAt       *time.Time                `json:"observed_at,omitempty"`
	SettledAt        *time.Time                `json:"settled_at,omitempty"`
	RelatedResources []ActivityRelatedResource `json:"related_resources"`
	OperationIDs     []string                  `json:"operation_ids"`
	EvidenceDigests  []string                  `json:"evidence_digests"`
	Coverage         ActivityEventCoverage     `json:"coverage"`

	SourceKind           string `json:"-"`
	SourceIdentity       string `json:"-"`
	SourceEvidenceDigest string `json:"-"`
	TargetBindingDigest  string `json:"-"`
	SnapshotDigest       string `json:"-"`
	IngestionSequence    int64  `json:"-"`
}

type ActivityEventInput struct {
	SourceKind           string
	SourceIdentity       string
	SourceEvidenceDigest string
	Category             ActivityCategory
	EventKind            ActivityEventKind
	Actor                ActivityActor
	Scope                AuthorityScopeKind
	TargetID             string
	TargetBindingDigest  string
	ReasonCode           ActivityReasonCode
	PriorState           string
	ResultingState       string
	PriorVersion         int64
	ResultingVersion     int64
	OccurredAt           time.Time
	ObservedAt           *time.Time
	SettledAt            *time.Time
	RelatedResources     []ActivityRelatedResource
	OperationIDs         []string
	EvidenceDigests      []string
	Coverage             ActivityEventCoverage
}

func NewActivityEvent(input ActivityEventInput) ActivityEvent {
	event := ActivityEvent{
		SchemaVersion: ActivitySchemaVersion, Category: input.Category, EventKind: input.EventKind,
		Actor: input.Actor, Scope: input.Scope, TargetID: strings.TrimSpace(input.TargetID),
		ReasonCode: input.ReasonCode, PriorState: input.PriorState, ResultingState: input.ResultingState,
		PriorVersion: input.PriorVersion, ResultingVersion: input.ResultingVersion,
		OccurredAt: input.OccurredAt.UTC(), RelatedResources: append([]ActivityRelatedResource(nil), input.RelatedResources...),
		OperationIDs: append([]string(nil), input.OperationIDs...), EvidenceDigests: append([]string(nil), input.EvidenceDigests...),
		Coverage: input.Coverage, SourceKind: strings.TrimSpace(input.SourceKind), SourceIdentity: strings.TrimSpace(input.SourceIdentity),
		SourceEvidenceDigest: input.SourceEvidenceDigest, TargetBindingDigest: input.TargetBindingDigest,
	}
	if input.ObservedAt != nil {
		value := input.ObservedAt.UTC()
		event.ObservedAt = &value
	}
	if input.SettledAt != nil {
		value := input.SettledAt.UTC()
		event.SettledAt = &value
	}
	event.EventID = "activity-" + digestText(strings.Join([]string{ActivitySchemaVersion, event.SourceKind, event.SourceIdentity, string(event.EventKind)}, "\x00"))[:32]
	event.SnapshotDigest = activitySnapshotDigest(event)
	return event
}

func ValidateActivityEvent(event ActivityEvent) error {
	if event.SchemaVersion != ActivitySchemaVersion || !validActivityCategory(event.Category) || !validActivityEventKind(event.EventKind) || !validActivityActor(event.Actor) || !validOperationScope(event.Scope) || strings.TrimSpace(event.TargetID) == "" || strings.ContainsRune(event.TargetID, '\x00') || !validAuthorityDigest(event.TargetBindingDigest) || !validAuthorityDigest(event.SourceEvidenceDigest) || event.OccurredAt.IsZero() || event.PriorVersion < 0 || event.ResultingVersion < 0 {
		return errors.New("activity event authority is invalid")
	}
	if strings.TrimSpace(event.SourceKind) == "" || strings.TrimSpace(event.SourceIdentity) == "" || strings.ContainsRune(event.SourceKind, '\x00') || strings.ContainsRune(event.SourceIdentity, '\x00') || !validActivityReason(event.ReasonCode) || !validActivityCoverage(event.Coverage) {
		return errors.New("activity event classification is invalid")
	}
	expected := NewActivityEvent(ActivityEventInput{SourceKind: event.SourceKind, SourceIdentity: event.SourceIdentity, SourceEvidenceDigest: event.SourceEvidenceDigest, Category: event.Category, EventKind: event.EventKind, Actor: event.Actor, Scope: event.Scope, TargetID: event.TargetID, TargetBindingDigest: event.TargetBindingDigest, ReasonCode: event.ReasonCode, PriorState: event.PriorState, ResultingState: event.ResultingState, PriorVersion: event.PriorVersion, ResultingVersion: event.ResultingVersion, OccurredAt: event.OccurredAt, ObservedAt: event.ObservedAt, SettledAt: event.SettledAt, RelatedResources: event.RelatedResources, OperationIDs: event.OperationIDs, EvidenceDigests: event.EvidenceDigests, Coverage: event.Coverage})
	if event.EventID != expected.EventID || event.SnapshotDigest != expected.SnapshotDigest {
		return errors.New("activity event identity conflicts")
	}
	if len(event.RelatedResources) > 8 || len(event.OperationIDs) > 4 || len(event.EvidenceDigests) > 8 {
		return errors.New("activity event links exceed bounds")
	}
	seen := map[string]struct{}{}
	for _, link := range event.RelatedResources {
		if !validOperationScope(link.Kind) || strings.TrimSpace(link.ID) == "" || strings.ContainsRune(link.ID, '\x00') {
			return errors.New("activity related resource is invalid")
		}
		key := string(link.Kind) + "\x00" + link.ID
		if _, ok := seen[key]; ok {
			return errors.New("activity related resource is duplicated")
		}
		seen[key] = struct{}{}
	}
	for _, id := range event.OperationIDs {
		if strings.TrimSpace(id) == "" || strings.ContainsRune(id, '\x00') {
			return errors.New("activity operation link is invalid")
		}
	}
	for _, value := range event.EvidenceDigests {
		if !validAuthorityDigest(value) {
			return errors.New("activity evidence digest is invalid")
		}
	}
	if strings.ContainsRune(event.PriorState, '\x00') || strings.ContainsRune(event.ResultingState, '\x00') {
		return errors.New("activity state is invalid")
	}
	return nil
}

func activitySnapshotDigest(event ActivityEvent) string {
	copy := event
	copy.SnapshotDigest = ""
	copy.IngestionSequence = 0
	raw, _ := json.Marshal(copy)
	return digestText("activity-snapshot-v1\x00" + string(raw))
}

func validActivityCategory(value ActivityCategory) bool {
	return slices.Contains([]ActivityCategory{ActivityRun, ActivityAttention, ActivityOperation, ActivityRepository, ActivityOnboarding, ActivityConfiguration, ActivityWorker, ActivityAdmissionCapacity}, value)
}
func validActivityActor(value ActivityActor) bool {
	return slices.Contains([]ActivityActor{ActivityActorController, ActivityActorConfiguredOperator, ActivityActorExternalAuthority, ActivityActorUnknown}, value)
}
func validActivityEventKind(value ActivityEventKind) bool {
	return slices.Contains([]ActivityEventKind{ActivityRunTransition, ActivityAttentionOpened, ActivityAttentionSuperseded, ActivityAttentionResolved, ActivityOperationSettled, ActivityRepositoryLifecycleChange, ActivityRepositoryGateChange, ActivityOnboardingMilestone, ActivityOnboardingCompleted, ActivityOnboardingCancelled, ActivityOnboardingConflict, ActivityConfigurationApplied, ActivityConfigurationRolledBack, ActivityConfigurationDrifted, ActivityConfigurationConverged, ActivityWorkerReadinessChange, ActivityAdmissionConflict}, value)
}
func validActivityReason(value ActivityReasonCode) bool {
	return slices.Contains([]ActivityReasonCode{ActivityReasonStateChanged, ActivityReasonOpened, ActivityReasonSuperseded, ActivityReasonResolved, ActivityReasonSucceeded, ActivityReasonFailed, ActivityReasonConflict, ActivityReasonAmbiguous, ActivityReasonMilestone, ActivityReasonCompleted, ActivityReasonCancelled, ActivityReasonDriftDetected, ActivityReasonConverged, ActivityReasonReadinessChanged, ActivityReasonCapacityConflict}, value)
}
func validActivityCoverage(value ActivityEventCoverage) bool {
	return value.IngestionClass == ActivityIngestionCurrent || value.IngestionClass == ActivityIngestionBackfill || value.IngestionClass == ActivityIngestionRuntime
}

type ActivityFilter struct {
	Category        ActivityCategory
	Scope           AuthorityScopeKind
	TargetID        string
	OccurredFrom    *time.Time
	OccurredThrough *time.Time
}

type ActivityCursor struct {
	Version            string    `json:"v"`
	ScopeDigest        string    `json:"s"`
	FilterDigest       string    `json:"f"`
	OccurredAt         time.Time `json:"o"`
	EventID            string    `json:"e"`
	IngestionWatermark int64     `json:"w"`
}

type ActivityStoreQuery struct {
	Scopes AuthorizedScopeSet
	Filter ActivityFilter
	Limit  int
	Cursor *ActivityCursor
}
type ActivityStorePage struct {
	Events             []ActivityEvent
	Total              int
	HasMore            bool
	IngestionWatermark int64
	Coverage           ActivityCoverage
}

type ActivityQueryStore interface {
	ListActivity(context.Context, ActivityStoreQuery) (ActivityStorePage, error)
	GetActivity(context.Context, string, AuthorizedScopeSet) (ActivityEvent, ActivityCoverage, error)
}

type ActivityBackfillResult struct {
	SourceKind string `json:"source_kind,omitempty"`
	Indexed    int    `json:"indexed"`
	Complete   bool   `json:"complete"`
	Conflict   bool   `json:"conflict"`
}

type RuntimeActivityObservation struct {
	SourceKind           string
	SourceIdentity       string
	Classification       string
	SourceEvidenceDigest string
	TargetBindingDigest  string
	OccurredAt           time.Time
	ObservedAt           time.Time
}

type ActivityListQuery struct {
	Requester Requester
	Filter    ActivityFilter
	Limit     int
	Cursor    string
}
type ActivityDetailQuery struct {
	Requester Requester
	EventID   string
}
type ActivityCollectionMetadata struct {
	Total      int    `json:"total"`
	Truncated  bool   `json:"truncated"`
	NextCursor string `json:"next_cursor,omitempty"`
}
type ActivityPage struct {
	Metadata   RoutineProjectionMetadata  `json:"metadata"`
	Collection ActivityCollectionMetadata `json:"collection"`
	Coverage   ActivityCoverage           `json:"coverage"`
	Events     []ActivityEvent            `json:"events"`
}
type ActivityDetail struct {
	Metadata RoutineProjectionMetadata `json:"metadata"`
	Coverage ActivityCoverage          `json:"coverage"`
	Event    ActivityEvent             `json:"event"`
	Links    []ActivityRelatedResource `json:"links"`
}

type ActivityQueryService struct {
	store      ActivityQueryStore
	authorizer *AuthorizationService
}

func NewActivityQueryService(store ActivityQueryStore, authorizer *AuthorizationService) (*ActivityQueryService, error) {
	if store == nil || authorizer == nil {
		return nil, errors.New("activity query dependencies are required")
	}
	return &ActivityQueryService{store: store, authorizer: authorizer}, nil
}

func (s *ActivityQueryService) List(ctx context.Context, query ActivityListQuery, observedAt time.Time) (ActivityPage, error) {
	configured, err := s.authorizer.ResolveConfiguredRequester(query.Requester)
	if err != nil {
		return ActivityPage{}, hiddenTargetError()
	}
	scopes, err := s.authorizer.ControllerScopes(configured)
	if err != nil {
		return ActivityPage{}, hiddenTargetError()
	}
	limit := query.Limit
	if limit == 0 {
		limit = ActivityDefaultLimit
	}
	if limit < 1 || limit > ActivityMaximumLimit {
		return ActivityPage{}, serviceError(ErrorInvalidInput, "activity limit is invalid", nil)
	}
	if err := validateActivityFilter(query.Filter); err != nil {
		return ActivityPage{}, serviceError(ErrorInvalidInput, "activity filter is invalid", err)
	}
	filterDigest := activityFilterDigest(query.Filter)
	var cursor *ActivityCursor
	if query.Cursor != "" {
		decoded, decodeErr := decodeActivityCursor(query.Cursor)
		if decodeErr != nil || decoded.Version != ActivitySchemaVersion || decoded.ScopeDigest != scopes.Digest() || decoded.FilterDigest != filterDigest || decoded.IngestionWatermark < 1 || decoded.EventID == "" || decoded.OccurredAt.IsZero() {
			return ActivityPage{}, serviceError(ErrorInvalidInput, "activity cursor is invalid", nil)
		}
		cursor = &decoded
	}
	page, err := s.store.ListActivity(ctx, ActivityStoreQuery{Scopes: scopes, Filter: query.Filter, Limit: limit, Cursor: cursor})
	if err != nil {
		return ActivityPage{}, classifyServiceError(err)
	}
	result := ActivityPage{Metadata: RoutineProjectionMetadata{SchemaVersion: ActivitySchemaVersion, ObservedAt: observedAt.UTC()}, Collection: ActivityCollectionMetadata{Total: page.Total, Truncated: page.HasMore}, Coverage: page.Coverage, Events: page.Events}
	if page.HasMore && len(page.Events) != 0 {
		last := page.Events[len(page.Events)-1]
		result.Collection.NextCursor = encodeActivityCursor(ActivityCursor{Version: ActivitySchemaVersion, ScopeDigest: scopes.Digest(), FilterDigest: filterDigest, OccurredAt: last.OccurredAt, EventID: last.EventID, IngestionWatermark: page.IngestionWatermark})
	}
	result.Metadata.Digest = activityProjectionDigest(result)
	return result, nil
}

func (s *ActivityQueryService) Detail(ctx context.Context, query ActivityDetailQuery, observedAt time.Time) (ActivityDetail, error) {
	if strings.TrimSpace(query.EventID) == "" {
		return ActivityDetail{}, serviceError(ErrorInvalidInput, "activity event is required", nil)
	}
	configured, err := s.authorizer.ResolveConfiguredRequester(query.Requester)
	if err != nil {
		return ActivityDetail{}, hiddenTargetError()
	}
	scopes, err := s.authorizer.ControllerScopes(configured)
	if err != nil {
		return ActivityDetail{}, hiddenTargetError()
	}
	event, coverage, err := s.store.GetActivity(ctx, query.EventID, scopes)
	if errors.Is(err, ErrActivityNotFound) {
		return ActivityDetail{}, hiddenTargetError()
	}
	if err != nil {
		return ActivityDetail{}, classifyServiceError(err)
	}
	result := ActivityDetail{Metadata: RoutineProjectionMetadata{SchemaVersion: ActivitySchemaVersion, ObservedAt: observedAt.UTC()}, Coverage: coverage, Event: event, Links: append([]ActivityRelatedResource(nil), event.RelatedResources...)}
	result.Metadata.Digest = activityProjectionDigest(result)
	return result, nil
}

func validateActivityFilter(filter ActivityFilter) error {
	if filter.Category != "" && !validActivityCategory(filter.Category) || filter.Scope != "" && !validOperationScope(filter.Scope) || strings.ContainsRune(filter.TargetID, '\x00') {
		return errors.New("activity classification filter is invalid")
	}
	if filter.TargetID != "" && filter.Scope == "" {
		return errors.New("activity target requires scope")
	}
	if filter.OccurredFrom != nil && filter.OccurredThrough != nil && filter.OccurredFrom.After(*filter.OccurredThrough) {
		return errors.New("activity time window is invalid")
	}
	return nil
}

func activityFilterDigest(filter ActivityFilter) string {
	raw, _ := json.Marshal(filter)
	return digestText("activity-filter-v1\x00" + string(raw))
}
func encodeActivityCursor(cursor ActivityCursor) string {
	raw, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(raw)
}
func decodeActivityCursor(value string) (ActivityCursor, error) {
	var cursor ActivityCursor
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || json.Unmarshal(raw, &cursor) != nil {
		return ActivityCursor{}, errors.New("activity cursor is invalid")
	}
	return cursor, nil
}
func activityProjectionDigest(value any) string {
	raw, _ := json.Marshal(value)
	return digestText("activity-projection-v1\x00" + string(raw))
}

func ActivityReasonForOperation(outcome OperationOutcome) ActivityReasonCode {
	switch outcome {
	case OperationOutcomeSucceeded:
		return ActivityReasonSucceeded
	case OperationOutcomeFailed:
		return ActivityReasonFailed
	case OperationOutcomeConflict:
		return ActivityReasonConflict
	case OperationOutcomeAmbiguous:
		return ActivityReasonAmbiguous
	default:
		return ""
	}
}

func ActivityActorForOperation(_ OperationReceipt) ActivityActor {
	return ActivityActorConfiguredOperator
}

func ValidateActivityCoverage(value ActivityCoverage) error {
	if !slices.Contains([]ActivityCoverageState{ActivityCoverageComplete, ActivityCoverageBackfilling, ActivityCoverageDegraded, ActivityCoverageUnknown, ActivityCoverageConflict}, value.State) || strings.TrimSpace(value.ReasonCode) == "" || value.BackfillSourcesComplete < 0 || value.BackfillSourcesTotal < value.BackfillSourcesComplete || len(value.LegacyLimitations) > 16 {
		return errors.New("activity coverage is invalid")
	}
	for _, limitation := range value.LegacyLimitations {
		if strings.TrimSpace(limitation) == "" || strings.ContainsRune(limitation, '\x00') {
			return errors.New("activity legacy limitation is invalid")
		}
	}
	return nil
}

func ActivityCoveragePrecedence(states ...ActivityCoverageState) ActivityCoverageState {
	for _, candidate := range []ActivityCoverageState{ActivityCoverageConflict, ActivityCoverageUnknown, ActivityCoverageDegraded, ActivityCoverageBackfilling, ActivityCoverageComplete} {
		if slices.Contains(states, candidate) {
			return candidate
		}
	}
	return ActivityCoverageUnknown
}

func activityDebugIdentity(event ActivityEvent) string {
	return fmt.Sprintf("%s/%s", event.Category, event.EventKind)
}
