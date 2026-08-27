package application

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
)

const (
	IntegritySchemaVersion   = "v1"
	IntegrityRegistryVersion = "v1"
	IntegrityDefaultLimit    = 50
	IntegrityMaximumLimit    = 100
)

type IntegrityFamily string

const (
	IntegrityStorageSchema        IntegrityFamily = "storage_schema"
	IntegrityRunDelivery          IntegrityFamily = "run_delivery"
	IntegrityOperationActivity    IntegrityFamily = "operation_activity"
	IntegrityConfiguration        IntegrityFamily = "configuration"
	IntegrityRepositoryOnboarding IntegrityFamily = "repository_onboarding"
	IntegritySchedulingAdmission  IntegrityFamily = "scheduling_admission"
	IntegrityOwnedResourceCleanup IntegrityFamily = "owned_resource_cleanup"
)

var integrityFamilies = []IntegrityFamily{
	IntegrityStorageSchema,
	IntegrityRunDelivery,
	IntegrityOperationActivity,
	IntegrityConfiguration,
	IntegrityRepositoryOnboarding,
	IntegritySchedulingAdmission,
	IntegrityOwnedResourceCleanup,
}

func IntegrityFamilies() []IntegrityFamily {
	return append([]IntegrityFamily(nil), integrityFamilies...)
}

func (f IntegrityFamily) Valid() bool { return slices.Contains(integrityFamilies, f) }

type IntegrityState string

const (
	IntegrityReady    IntegrityState = "ready"
	IntegrityNotReady IntegrityState = "not_ready"
	IntegrityUnknown  IntegrityState = "unknown"
	IntegrityConflict IntegrityState = "conflict"
)

func (s IntegrityState) Valid() bool {
	return slices.Contains([]IntegrityState{IntegrityReady, IntegrityNotReady, IntegrityUnknown, IntegrityConflict}, s)
}

func AggregateIntegrity(states ...IntegrityState) IntegrityState {
	for _, candidate := range []IntegrityState{IntegrityConflict, IntegrityUnknown, IntegrityNotReady, IntegrityReady} {
		if slices.Contains(states, candidate) {
			return candidate
		}
	}
	return IntegrityUnknown
}

type IntegrityFamilyResult struct {
	Family             IntegrityFamily `json:"family"`
	State              IntegrityState  `json:"state"`
	ReasonCode         string          `json:"reason_code"`
	CheckedRevision    int64           `json:"checked_revision"`
	AffectedScopeCount int             `json:"affected_scope_count"`
	CountComplete      bool            `json:"count_complete"`
	CoverageComplete   bool            `json:"coverage_complete"`
}

func (r IntegrityFamilyResult) Validate() error {
	if !r.Family.Valid() || !r.State.Valid() || strings.TrimSpace(r.ReasonCode) == "" || r.CheckedRevision < 0 || r.AffectedScopeCount < 0 {
		return errors.New("integrity family result is invalid")
	}
	if r.State == IntegrityReady && (!r.CountComplete || !r.CoverageComplete || r.AffectedScopeCount != 0) {
		return errors.New("ready integrity family is incomplete")
	}
	return nil
}

type IntegrityObservation struct {
	SchemaVersion       string                  `json:"schema_version"`
	RegistryVersion     string                  `json:"registry_version"`
	ObservationID       string                  `json:"observation_id"`
	Digest              string                  `json:"digest"`
	TargetGeneration    int64                   `json:"target_generation"`
	PublishedGeneration int64                   `json:"published_generation"`
	ObservedAt          time.Time               `json:"observed_at"`
	EffectiveReadiness  IntegrityState          `json:"effective_readiness"`
	ReasonCode          string                  `json:"reason_code"`
	Results             []IntegrityFamilyResult `json:"family_results"`
	AffectedScopeCount  int                     `json:"affected_scope_count"`
	CountComplete       bool                    `json:"count_complete"`
	CoverageComplete    bool                    `json:"coverage_complete"`
}

func (o IntegrityObservation) Validate() error {
	if o.SchemaVersion != IntegritySchemaVersion || o.RegistryVersion != IntegrityRegistryVersion || strings.TrimSpace(o.ObservationID) == "" || !validAuthorityDigest(o.Digest) || o.TargetGeneration < 0 || o.PublishedGeneration != o.TargetGeneration || o.ObservedAt.IsZero() || !o.EffectiveReadiness.Valid() || strings.TrimSpace(o.ReasonCode) == "" || len(o.Results) != len(integrityFamilies) || o.AffectedScopeCount < 0 {
		return errors.New("integrity observation is invalid")
	}
	seen := map[IntegrityFamily]bool{}
	states := make([]IntegrityState, 0, len(o.Results))
	for _, result := range o.Results {
		if result.Validate() != nil || seen[result.Family] {
			return errors.New("integrity observation family results are invalid")
		}
		seen[result.Family] = true
		states = append(states, result.State)
	}
	for _, family := range integrityFamilies {
		if !seen[family] {
			return errors.New("integrity observation family result is missing")
		}
	}
	if AggregateIntegrity(states...) != o.EffectiveReadiness {
		return errors.New("integrity observation readiness conflicts")
	}
	if o.EffectiveReadiness == IntegrityReady && (!o.CountComplete || !o.CoverageComplete || o.AffectedScopeCount != 0) {
		return errors.New("ready integrity observation is incomplete")
	}
	return nil
}

type IntegritySummary struct {
	CurrentGeneration int64                `json:"current_generation"`
	Current           bool                 `json:"current"`
	Readiness         IntegrityState       `json:"readiness"`
	ReasonCode        string               `json:"reason_code"`
	Observation       IntegrityObservation `json:"observation"`
}

type IntegrityFinding struct {
	FindingID      string             `json:"finding_id"`
	Family         IntegrityFamily    `json:"family"`
	ReasonCode     string             `json:"reason_code"`
	Scope          AuthorityScopeKind `json:"scope"`
	TargetID       string             `json:"target_id"`
	ObservationAt  time.Time          `json:"observation_at"`
	Classification map[string]string  `json:"classification,omitempty"`
}

type IntegrityFindingQuery struct {
	Requester Requester
	Family    IntegrityFamily
	Scope     AuthorityScopeKind
	TargetID  string
	Limit     int
	Cursor    string
}

type IntegrityFindingPage struct {
	ObservationID     string             `json:"observation_id"`
	ObservationDigest string             `json:"observation_digest"`
	Findings          []IntegrityFinding `json:"findings"`
	Count             int                `json:"count"`
	CountComplete     bool               `json:"count_complete"`
	NextCursor        string             `json:"next_cursor,omitempty"`
}

type IntegrityQueryStore interface {
	IntegritySummary(context.Context, AuthorizedScopeSet) (IntegritySummary, error)
	ListIntegrityFindings(context.Context, AuthorizedScopeSet, IntegrityFindingQuery) (IntegrityFindingPage, error)
}

type IntegrityQueryService struct {
	store      IntegrityQueryStore
	authorizer *AuthorizationService
}

func NewIntegrityQueryService(store IntegrityQueryStore, authorizer *AuthorizationService) (*IntegrityQueryService, error) {
	if store == nil || authorizer == nil {
		return nil, errors.New("integrity query service is unavailable")
	}
	return &IntegrityQueryService{store: store, authorizer: authorizer}, nil
}

func (s *IntegrityQueryService) Summary(ctx context.Context, requester Requester) (IntegritySummary, error) {
	configured, err := s.authorizer.ResolveConfiguredRequester(requester)
	if err != nil {
		return IntegritySummary{}, err
	}
	scopes, err := s.authorizer.ControllerScopes(configured)
	if err != nil {
		return IntegritySummary{}, err
	}
	return s.store.IntegritySummary(ctx, scopes)
}

func (s *IntegrityQueryService) Findings(ctx context.Context, query IntegrityFindingQuery) (IntegrityFindingPage, error) {
	if query.Family != "" && !query.Family.Valid() || query.Scope != "" && !slices.Contains([]AuthorityScopeKind{ScopeController, ScopeRepository, ScopeRun, ScopeOnboarding}, query.Scope) || query.Limit < 0 || query.Limit > IntegrityMaximumLimit || strings.ContainsRune(query.TargetID, '\x00') {
		return IntegrityFindingPage{}, serviceError(ErrorInvalidInput, "integrity finding query is invalid", nil)
	}
	configured, err := s.authorizer.ResolveConfiguredRequester(query.Requester)
	if err != nil {
		return IntegrityFindingPage{}, err
	}
	scopes, err := s.authorizer.ControllerScopes(configured)
	if err != nil {
		return IntegrityFindingPage{}, err
	}
	if query.Limit == 0 {
		query.Limit = IntegrityDefaultLimit
	}
	return s.store.ListIntegrityFindings(ctx, scopes, query)
}

type IntegrityMaintenanceResult struct {
	ScanID           string          `json:"scan_id,omitempty"`
	Family           IntegrityFamily `json:"family,omitempty"`
	TargetGeneration int64           `json:"target_generation"`
	Published        bool            `json:"published"`
	Superseded       bool            `json:"superseded"`
}
