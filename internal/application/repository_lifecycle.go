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

const controllerRepositoryCursorVersion = "v2"

var (
	ErrRepositoryLifecycleConflict = errors.New("repository lifecycle authority conflicts")
	ErrRepositoryLifecycleMissing  = errors.New("repository lifecycle authority is missing")
	ErrRepositoryAdmissionConflict = errors.New("repository admission authority conflicts")
)

type RepositoryLifecycleIntent string

const (
	RepositoryEnabled  RepositoryLifecycleIntent = "enabled"
	RepositoryDisabled RepositoryLifecycleIntent = "disabled"
)

type RepositoryLifecycle struct {
	IncarnationID            string                    `json:"incarnation_id"`
	Repository               string                    `json:"repository"`
	ProfileID                string                    `json:"profile_id"`
	ProfileDigest            string                    `json:"profile_digest"`
	RepositoryBindingDigest  string                    `json:"repository_binding_digest"`
	Intent                   RepositoryLifecycleIntent `json:"intent"`
	Version                  int64                     `json:"version"`
	UpdatedAt                time.Time                 `json:"updated_at"`
	RetiredAt                time.Time                 `json:"retired_at,omitempty"`
	RetirementEvidenceDigest string                    `json:"retirement_evidence_digest,omitempty"`
}

func (l RepositoryLifecycle) Validate() error {
	if !validRepositoryIncarnationID(l.IncarnationID) || strings.TrimSpace(l.Repository) == "" || strings.TrimSpace(l.ProfileID) == "" || !validAuthorityDigest(l.ProfileDigest) || !validAuthorityDigest(l.RepositoryBindingDigest) || l.Intent != RepositoryEnabled && l.Intent != RepositoryDisabled || l.Version < 1 || l.UpdatedAt.IsZero() {
		return errors.New("repository lifecycle is invalid")
	}
	if l.RetiredAt.IsZero() != (l.RetirementEvidenceDigest == "") || !l.RetiredAt.IsZero() && (!validAuthorityDigest(l.RetirementEvidenceDigest) || l.RetiredAt.Before(l.UpdatedAt)) {
		return errors.New("repository retirement evidence is invalid")
	}
	return nil
}

type RepositoryReadinessSnapshot struct {
	SnapshotID                    string                             `json:"snapshot_id"`
	IncarnationID                 string                             `json:"incarnation_id"`
	Repository                    string                             `json:"repository"`
	ProfileID                     string                             `json:"profile_id"`
	ProfileDigest                 string                             `json:"profile_digest"`
	RepositoryBindingDigest       string                             `json:"repository_binding_digest"`
	LifecycleVersion              int64                              `json:"lifecycle_version"`
	ConfigurationGenerationID     int64                              `json:"configuration_generation_id"`
	ConfigurationDigest           string                             `json:"configuration_digest"`
	ConfigurationAuthorityVersion int64                              `json:"configuration_authority_version"`
	Status                        domain.RepositoryReadinessStatus   `json:"status"`
	ReasonCode                    string                             `json:"reason_code"`
	SnapshotDigest                string                             `json:"snapshot_digest"`
	Dimensions                    []domain.RepositoryDimensionResult `json:"dimensions"`
	ObservedAt                    time.Time                          `json:"observed_at"`
	PublishedAt                   time.Time                          `json:"published_at"`
}

func (s RepositoryReadinessSnapshot) Validate() error {
	status, err := domain.AggregateRepositoryReadiness(s.Dimensions)
	if err != nil || status != s.Status || strings.TrimSpace(s.SnapshotID) == "" || !validRepositoryIncarnationID(s.IncarnationID) || strings.TrimSpace(s.Repository) == "" || strings.TrimSpace(s.ProfileID) == "" || !validAuthorityDigest(s.ProfileDigest) || !validAuthorityDigest(s.RepositoryBindingDigest) || s.LifecycleVersion < 1 || s.ConfigurationGenerationID < 1 || s.ConfigurationAuthorityVersion < 1 || !validAuthorityDigest(s.ConfigurationDigest) || !validAuthorityDigest(s.SnapshotDigest) || strings.TrimSpace(s.ReasonCode) == "" || s.ObservedAt.IsZero() || s.PublishedAt.IsZero() || s.PublishedAt.Before(s.ObservedAt) {
		return errors.New("repository readiness snapshot is invalid")
	}
	return nil
}

type RepositoryRecheckState struct {
	AttemptID   string    `json:"attempt_id"`
	OperationID string    `json:"operation_id"`
	Refreshing  bool      `json:"refreshing"`
	StartedAt   time.Time `json:"started_at"`
}

type RepositoryRemovalProjection struct {
	OperationID        string    `json:"operation_id"`
	ResultGenerationID int64     `json:"result_generation_id,omitempty"`
	ResultDigest       string    `json:"result_digest,omitempty"`
	State              string    `json:"state"`
	NextAction         string    `json:"next_action"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type RepositoryAvailability struct {
	Available  bool   `json:"available"`
	ReasonCode string `json:"reason_code"`
	ActiveRun  string `json:"active_run,omitempty"`
}

type RepositoryProjection struct {
	Lifecycle    RepositoryLifecycle          `json:"lifecycle"`
	Readiness    RepositoryReadinessSnapshot  `json:"readiness"`
	Availability RepositoryAvailability       `json:"availability"`
	Recheck      *RepositoryRecheckState      `json:"recheck,omitempty"`
	Removal      *RepositoryRemovalProjection `json:"removal,omitempty"`
}

type RepositoryListPage struct {
	Repositories []RepositoryProjection `json:"repositories"`
	NextCursor   string                 `json:"next_cursor,omitempty"`
	HasMore      bool                   `json:"has_more"`
	Total        int                    `json:"total"`
}

type ControllerRepositoryQuery struct {
	Authority       ControllerReadAuthority
	AfterRepository string
	Limit           int
}

type ControllerRepositoryCollectionStore interface {
	ListControllerRepositories(context.Context, ControllerRepositoryQuery) (RepositoryListPage, error)
}

type controllerRepositoryCursor struct {
	Version      string `json:"version"`
	ReaderDigest string `json:"reader_digest"`
	Repository   string `json:"repository"`
}

type RepositoryProfileAuthority struct {
	Authority RepositoryAuthority
	Profile   LocalRepository
}

type RepositoryProfileSource interface {
	RepositoryProfile(context.Context, string) (RepositoryProfileAuthority, bool, error)
	ListRepositoryProfiles(context.Context) ([]RepositoryProfileAuthority, error)
}

type RepositoryBaselineInput struct {
	Profiles  []RepositoryProfileAuthority
	AdoptedAt time.Time
}

type RepositoryOperationAuthority struct {
	Lifecycle              RepositoryLifecycle
	Snapshot               RepositoryReadinessSnapshot
	Recheck                *RepositoryRecheckState
	ConfigurationAuthority ConfigurationAdmissionAuthority
	Removal                *RepositoryRemovalProjection
}

type RepositoryEligibilityToken struct {
	Repository                    string `json:"repository"`
	LifecycleVersion              int64  `json:"lifecycle_version"`
	SnapshotID                    string `json:"snapshot_id"`
	SnapshotDigest                string `json:"snapshot_digest"`
	ProfileDigest                 string `json:"profile_digest"`
	RepositoryBindingDigest       string `json:"repository_binding_digest"`
	ConfigurationGenerationID     int64  `json:"configuration_generation_id"`
	ConfigurationDigest           string `json:"configuration_digest"`
	ConfigurationAuthorityVersion int64  `json:"configuration_authority_version"`
}

func (t RepositoryEligibilityToken) Valid() bool {
	return strings.TrimSpace(t.Repository) != "" && t.LifecycleVersion > 0 && strings.TrimSpace(t.SnapshotID) != "" && validAuthorityDigest(t.SnapshotDigest) && validAuthorityDigest(t.ProfileDigest) && validAuthorityDigest(t.RepositoryBindingDigest) && t.ConfigurationGenerationID > 0 && validAuthorityDigest(t.ConfigurationDigest) && t.ConfigurationAuthorityVersion > 0
}

type RepositoryAdmissionDecision struct {
	Allowed bool                       `json:"allowed"`
	Reason  string                     `json:"reason"`
	Token   RepositoryEligibilityToken `json:"token,omitempty"`
}

type RepositoryAdmissionGate interface {
	CheckRepositoryAdmission(context.Context, LocalRepository) (RepositoryAdmissionDecision, error)
}

type RepositoryRecheckStart struct {
	AttemptID   string
	OperationID string
	Expected    RepositoryOperationAuthority
	Profile     LocalRepository
	StartedAt   time.Time
}

type RepositoryRecheckPublication struct {
	AttemptID   string
	OperationID string
	Expected    RepositoryOperationAuthority
	Profile     LocalRepository
	Results     []domain.RepositoryDimensionResult
	PublishedAt time.Time
}

type RepositoryRecheckFailure struct {
	AttemptID   string
	OperationID string
	Outcome     OperationOutcome
	ReasonCode  string
	SettledAt   time.Time
}

type RepositoryLifecycleChange struct {
	OperationID string
	Expected    RepositoryOperationAuthority
	Intent      RepositoryLifecycleIntent
	ChangedAt   time.Time
}

type RepositoryLifecycleFailure struct {
	OperationID string
	Outcome     OperationOutcome
	ReasonCode  string
	SettledAt   time.Time
}

type RepositoryLifecycleStore interface {
	OperationReceiptStore
	ControllerRepositoryCollectionStore
	AdoptRepositoryLifecycleBaseline(context.Context, RepositoryBaselineInput) error
	RepositoryOperationAuthority(context.Context, string) (RepositoryOperationAuthority, error)
	GetAuthorizedRepository(context.Context, string, AuthorizedScopeSet) (RepositoryProjection, error)
	BeginRepositoryRecheck(context.Context, RepositoryRecheckStart) (RepositoryRecheckState, bool, error)
	SaveRepositoryRecheckObservation(context.Context, string, domain.RepositoryDimensionResult) error
	PublishRepositoryRecheck(context.Context, RepositoryRecheckPublication) (RepositoryProjection, OperationReceipt, error)
	SettleRepositoryRecheckFailure(context.Context, RepositoryRecheckFailure) error
	ChangeRepositoryLifecycle(context.Context, RepositoryLifecycleChange) (RepositoryProjection, OperationReceipt, error)
	SettleRepositoryLifecycleFailure(context.Context, RepositoryLifecycleFailure) error
	CheckRepositoryAdmission(context.Context, LocalRepository) (RepositoryAdmissionDecision, error)
}

type RepositoryConfigurationObserver interface {
	ObserveRepositoryConfiguration(context.Context, LocalRepository) (domain.RepositoryDimensionResult, ConfigurationAdmissionAuthority, error)
}

type RepositoryGitObserver interface {
	ObserveRepositoryGit(context.Context, LocalRepository) ([2]domain.RepositoryDimensionResult, error)
}

type RepositoryGitHubObserver interface {
	ObserveRepositoryGitHub(context.Context, LocalRepository) ([2]domain.RepositoryDimensionResult, error)
}

type RepositoryLinearObserver interface {
	ObserveRepositoryLinear(context.Context, LocalRepository) (domain.RepositoryDimensionResult, error)
}

type RepositoryVerifierObserver interface {
	ObserveRepositoryVerifiers(context.Context, LocalRepository) (domain.RepositoryDimensionResult, error)
}

type RepositoryObservers struct {
	Configuration RepositoryConfigurationObserver
	Git           RepositoryGitObserver
	GitHub        RepositoryGitHubObserver
	Linear        RepositoryLinearObserver
	Verifier      RepositoryVerifierObserver
}

type RepositoryService struct {
	store      RepositoryLifecycleStore
	authorizer *AuthorizationService
	profiles   RepositoryProfileSource
	observers  RepositoryObservers
	now        func() time.Time
}

func NewRepositoryService(store RepositoryLifecycleStore, authorizer *AuthorizationService, profiles RepositoryProfileSource, observers RepositoryObservers) (*RepositoryService, error) {
	if store == nil || authorizer == nil || profiles == nil {
		return nil, errors.New("repository service dependencies are required")
	}
	return &RepositoryService{store: store, authorizer: authorizer, profiles: profiles, observers: observers, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (s *RepositoryService) List(ctx context.Context, requester Requester, limit int, cursor string) (RepositoryListPage, error) {
	configured, err := s.authorizer.ResolveConfiguredRequester(requester)
	if err != nil {
		return RepositoryListPage{}, hiddenTargetError()
	}
	reader, err := s.authorizer.ControllerReadCollectionAuthority(configured)
	if err != nil {
		return RepositoryListPage{}, hiddenTargetError()
	}
	return s.ListController(ctx, reader, limit, cursor)
}

// ListController reads the current local Repository collection through the
// collection-only Controller reader. Direct repository inspection and every
// mutation continue to resolve the current target authority separately.
func (s *RepositoryService) ListController(ctx context.Context, authority ControllerReadAuthority, limit int, cursor string) (RepositoryListPage, error) {
	if limit == 0 {
		limit = 50
	}
	if !authority.Valid() {
		return RepositoryListPage{}, serviceError(ErrorInternal, "controller repository collection authority is unavailable", nil)
	}
	if limit < 1 || limit > 100 || len(cursor) > 512 {
		return RepositoryListPage{}, serviceError(ErrorInvalidInput, "repository collection bounds are invalid", nil)
	}
	position, err := decodeControllerRepositoryCursor(cursor)
	if err != nil {
		return RepositoryListPage{}, err
	}
	if cursor != "" && position.ReaderDigest != authority.Digest() {
		return RepositoryListPage{}, serviceError(ErrorInvalidInput, "cursor is invalid", nil)
	}
	stored, err := s.store.ListControllerRepositories(ctx, ControllerRepositoryQuery{Authority: authority, AfterRepository: position.Repository, Limit: limit + 1})
	if err != nil {
		return RepositoryListPage{}, classifyServiceError(err)
	}
	if stored.Total < len(stored.Repositories) || len(stored.Repositories) > limit+1 {
		return RepositoryListPage{}, serviceError(ErrorInternal, "controller repository authority conflicts", nil)
	}
	values := stored.Repositories
	page := RepositoryListPage{Repositories: make([]RepositoryProjection, 0, min(len(values), limit)), Total: stored.Total}
	if len(values) > limit {
		page.HasMore = true
		values = values[:limit]
	}
	last := position.Repository
	for _, projection := range values {
		if projection.Lifecycle.Repository > last && validCanonicalRepositoryFilter(projection.Lifecycle.Repository) && projection.Lifecycle.RetiredAt.IsZero() && projection.Lifecycle.Validate() == nil {
			page.Repositories = append(page.Repositories, projection)
			last = projection.Lifecycle.Repository
			continue
		}
		return RepositoryListPage{}, serviceError(ErrorInternal, "controller repository authority conflicts", nil)
	}
	if page.HasMore && len(values) != 0 {
		page.NextCursor = encodeControllerRepositoryCursor(controllerRepositoryCursor{Version: controllerRepositoryCursorVersion, ReaderDigest: authority.Digest(), Repository: values[len(values)-1].Lifecycle.Repository})
	}
	return page, nil
}

func decodeControllerRepositoryCursor(value string) (controllerRepositoryCursor, error) {
	if value == "" {
		return controllerRepositoryCursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return controllerRepositoryCursor{}, serviceError(ErrorInvalidInput, "cursor is invalid", nil)
	}
	var cursor controllerRepositoryCursor
	if json.Unmarshal(raw, &cursor) != nil || cursor.Version != controllerRepositoryCursorVersion || !validAuthorityDigest(cursor.ReaderDigest) || !validCanonicalRepositoryFilter(cursor.Repository) {
		return controllerRepositoryCursor{}, serviceError(ErrorInvalidInput, "cursor is invalid", nil)
	}
	return cursor, nil
}

func encodeControllerRepositoryCursor(cursor controllerRepositoryCursor) string {
	raw, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func validCanonicalRepositoryFilter(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || value != strings.ToLower(value) || strings.ContainsRune(value, '\x00') {
		return false
	}
	parts := strings.Split(value, "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != ""
}

func (s *RepositoryService) Inspect(ctx context.Context, requester Requester, repository string) (RepositoryProjection, error) {
	profile, scopes, err := s.authorizedRepository(ctx, requester, repository)
	if err != nil {
		return RepositoryProjection{}, err
	}
	projection, err := s.store.GetAuthorizedRepository(ctx, profile.Authority.Repository, scopes)
	if err != nil {
		if errors.Is(err, ErrRepositoryLifecycleMissing) {
			return RepositoryProjection{}, hiddenTargetError()
		}
		return RepositoryProjection{}, classifyRepositoryError(err)
	}
	return projection, nil
}

type RepositoryMutationCommand struct {
	Requester  Requester `json:"requester"`
	Repository string    `json:"repository"`
	RequestID  string    `json:"request_id"`
}

type RepositoryMutationResult struct {
	Repository RepositoryProjection `json:"repository"`
	Receipt    OperationReceipt     `json:"receipt"`
}

func (s *RepositoryService) Enable(ctx context.Context, command RepositoryMutationCommand) (RepositoryMutationResult, error) {
	return s.changeLifecycle(ctx, command, RepositoryEnabled, OperationEnableRepository)
}

func (s *RepositoryService) Disable(ctx context.Context, command RepositoryMutationCommand) (RepositoryMutationResult, error) {
	return s.changeLifecycle(ctx, command, RepositoryDisabled, OperationDisableRepository)
}

func (s *RepositoryService) changeLifecycle(ctx context.Context, command RepositoryMutationCommand, intent RepositoryLifecycleIntent, operationType OperationType) (RepositoryMutationResult, error) {
	profile, _, err := s.authorizedRepository(ctx, command.Requester, command.Repository)
	if err != nil {
		return RepositoryMutationResult{}, err
	}
	if strings.TrimSpace(command.RequestID) == "" || len(command.RequestID) > 128 || strings.ContainsRune(command.RequestID, '\x00') {
		return RepositoryMutationResult{}, serviceError(ErrorInvalidInput, "repository request ID is invalid", nil)
	}
	authority, err := s.store.RepositoryOperationAuthority(ctx, profile.Authority.Repository)
	if err != nil {
		return RepositoryMutationResult{}, classifyRepositoryError(err)
	}
	requestDigest := digestText("repository-lifecycle-request-v1\x00" + string(operationType) + "\x00" + profile.Authority.Repository + "\x00" + command.RequestID)
	authorityDigest := repositoryOperationAuthorityDigest(authority)
	anchorPrefix := "repository-lifecycle-v1"
	if authority.Lifecycle.Intent == intent {
		anchorPrefix = "repository-lifecycle-noop-v1\x00" + string(operationType)
	}
	receipts, _ := NewOperationReceiptService(s.store)
	receipt, created, err := receipts.Accept(ctx, OperationReceiptInput{OperationType: operationType, Scope: ScopeRepository, TargetID: profile.Authority.Repository, Requester: command.RequesterIdentity(), RequestDigest: requestDigest, ExpectedAuthorityDigest: authorityDigest, OperationAnchorDigest: digestText(anchorPrefix + "\x00" + profile.Authority.Repository + "\x00" + authorityDigest), TargetBindingDigest: profile.Authority.BindingDigest})
	if err != nil {
		return RepositoryMutationResult{}, err
	}
	if !created && receipt.Outcome != OperationOutcomePending {
		projection, inspectErr := s.Inspect(ctx, command.Requester, command.Repository)
		return RepositoryMutationResult{Repository: projection, Receipt: receipt}, inspectErr
	}
	projection, receipt, err := s.store.ChangeRepositoryLifecycle(ctx, RepositoryLifecycleChange{OperationID: receipt.OperationID, Expected: authority, Intent: intent, ChangedAt: s.now().UTC()})
	if err != nil {
		_ = s.store.SettleRepositoryLifecycleFailure(ctx, RepositoryLifecycleFailure{OperationID: receipt.OperationID, Outcome: OperationOutcomeConflict, ReasonCode: "lifecycle_authority_conflict", SettledAt: s.now().UTC()})
		return RepositoryMutationResult{}, classifyRepositoryError(err)
	}
	return RepositoryMutationResult{Repository: projection, Receipt: receipt}, nil
}

func (s *RepositoryService) Recheck(ctx context.Context, command RepositoryMutationCommand) (RepositoryMutationResult, error) {
	profile, _, err := s.authorizedRepository(ctx, command.Requester, command.Repository)
	if err != nil {
		return RepositoryMutationResult{}, err
	}
	if strings.TrimSpace(command.RequestID) == "" || len(command.RequestID) > 128 || strings.ContainsRune(command.RequestID, '\x00') {
		return RepositoryMutationResult{}, serviceError(ErrorInvalidInput, "repository request ID is invalid", nil)
	}
	if s.observers.Configuration == nil || s.observers.Git == nil || s.observers.GitHub == nil || s.observers.Linear == nil || s.observers.Verifier == nil {
		return RepositoryMutationResult{}, serviceError(ErrorInternal, "repository readiness observers are unavailable", nil)
	}
	authority, err := s.store.RepositoryOperationAuthority(ctx, profile.Authority.Repository)
	if err != nil {
		return RepositoryMutationResult{}, classifyRepositoryError(err)
	}
	requestDigest := digestText("repository-recheck-request-v1\x00" + profile.Authority.Repository + "\x00" + command.RequestID)
	authorityDigest := repositoryOperationAuthorityDigest(authority)
	receipts, _ := NewOperationReceiptService(s.store)
	receipt, created, err := receipts.Accept(ctx, OperationReceiptInput{OperationType: OperationRecheckRepository, Scope: ScopeRepository, TargetID: profile.Authority.Repository, Requester: command.RequesterIdentity(), RequestDigest: requestDigest, ExpectedAuthorityDigest: authorityDigest, OperationAnchorDigest: digestText("repository-recheck-v1\x00" + profile.Authority.Repository + "\x00" + authorityDigest), TargetBindingDigest: profile.Authority.BindingDigest})
	if err != nil {
		return RepositoryMutationResult{}, err
	}
	if !created && receipt.Outcome != OperationOutcomePending {
		projection, inspectErr := s.Inspect(ctx, command.Requester, command.Repository)
		return RepositoryMutationResult{Repository: projection, Receipt: receipt}, inspectErr
	}
	attemptID := "repository-recheck-" + receipt.OperationID
	_, attemptCreated, err := s.store.BeginRepositoryRecheck(ctx, RepositoryRecheckStart{AttemptID: attemptID, OperationID: receipt.OperationID, Expected: authority, Profile: profile.Profile, StartedAt: s.now().UTC()})
	if err != nil {
		s.settleRecheckFailure(ctx, attemptID, receipt.OperationID, OperationOutcomeConflict, "recheck_start_conflict")
		return RepositoryMutationResult{}, classifyRepositoryError(err)
	}
	if !attemptCreated {
		projection, inspectErr := s.Inspect(ctx, command.Requester, command.Repository)
		return RepositoryMutationResult{Repository: projection, Receipt: receipt}, inspectErr
	}
	results, observedAuthority, err := s.observe(ctx, profile.Profile)
	if err != nil {
		s.settleRecheckFailure(ctx, attemptID, receipt.OperationID, OperationOutcomeFailed, "readiness_observation_failed")
		return RepositoryMutationResult{}, classifyServiceError(err)
	}
	if observedAuthority.GenerationID != authority.ConfigurationAuthority.GenerationID || observedAuthority.Digest != authority.ConfigurationAuthority.Digest || observedAuthority.AuthorityVersion != authority.ConfigurationAuthority.AuthorityVersion {
		s.settleRecheckFailure(ctx, attemptID, receipt.OperationID, OperationOutcomeConflict, "configuration_authority_changed")
		return RepositoryMutationResult{}, serviceError(ErrorConflict, "configuration authority changed during repository recheck", ErrRepositoryLifecycleConflict)
	}
	for _, result := range results {
		if err := s.store.SaveRepositoryRecheckObservation(ctx, attemptID, result); err != nil {
			s.settleRecheckFailure(ctx, attemptID, receipt.OperationID, OperationOutcomeConflict, "readiness_observation_conflict")
			return RepositoryMutationResult{}, classifyRepositoryError(err)
		}
	}
	projection, receipt, err := s.store.PublishRepositoryRecheck(ctx, RepositoryRecheckPublication{AttemptID: attemptID, OperationID: receipt.OperationID, Expected: authority, Profile: profile.Profile, Results: results, PublishedAt: s.now().UTC()})
	if err != nil {
		s.settleRecheckFailure(ctx, attemptID, receipt.OperationID, OperationOutcomeConflict, "readiness_publication_conflict")
		return RepositoryMutationResult{}, classifyRepositoryError(err)
	}
	return RepositoryMutationResult{Repository: projection, Receipt: receipt}, nil
}

func (s *RepositoryService) settleRecheckFailure(ctx context.Context, attemptID, operationID string, outcome OperationOutcome, reason string) {
	_ = s.store.SettleRepositoryRecheckFailure(ctx, RepositoryRecheckFailure{AttemptID: attemptID, OperationID: operationID, Outcome: outcome, ReasonCode: reason, SettledAt: s.now().UTC()})
}

func (s *RepositoryService) observe(ctx context.Context, profile LocalRepository) ([]domain.RepositoryDimensionResult, ConfigurationAdmissionAuthority, error) {
	configuration, authority, err := s.observers.Configuration.ObserveRepositoryConfiguration(ctx, profile)
	if err != nil {
		return nil, ConfigurationAdmissionAuthority{}, err
	}
	git, err := s.observers.Git.ObserveRepositoryGit(ctx, profile)
	if err != nil {
		return nil, ConfigurationAdmissionAuthority{}, err
	}
	github, err := s.observers.GitHub.ObserveRepositoryGitHub(ctx, profile)
	if err != nil {
		return nil, ConfigurationAdmissionAuthority{}, err
	}
	linear, err := s.observers.Linear.ObserveRepositoryLinear(ctx, profile)
	if err != nil {
		return nil, ConfigurationAdmissionAuthority{}, err
	}
	verifier, err := s.observers.Verifier.ObserveRepositoryVerifiers(ctx, profile)
	if err != nil {
		return nil, ConfigurationAdmissionAuthority{}, err
	}
	now := s.now().UTC()
	profileResult := domain.RepositoryDimensionResult{Dimension: domain.ReadinessProfileConfiguration, Status: domain.RepositoryReady, ReasonCode: "profile_configuration_valid", EvidenceDigest: digestText("repository-profile-observation-v1\x00" + profile.ProfileDigest + "\x00" + profile.RepositoryBindingDigest), ObservedAt: now}
	results := []domain.RepositoryDimensionResult{profileResult, configuration, git[0], git[1], github[0], github[1], linear, verifier}
	if err := domain.ValidateCompleteRepositoryReadiness(results); err != nil {
		return nil, ConfigurationAdmissionAuthority{}, errors.New("repository observer returned incomplete readiness")
	}
	return results, authority, nil
}

func (s *RepositoryService) authorizedRepository(ctx context.Context, requester Requester, repository string) (RepositoryProfileAuthority, AuthorizedScopeSet, error) {
	configured, err := s.authorizer.ResolveConfiguredRequester(requester)
	if err != nil {
		return RepositoryProfileAuthority{}, AuthorizedScopeSet{}, hiddenTargetError()
	}
	profile, found, err := s.profiles.RepositoryProfile(ctx, strings.ToLower(strings.TrimSpace(repository)))
	if err != nil || !found {
		return RepositoryProfileAuthority{}, AuthorizedScopeSet{}, hiddenTargetError()
	}
	scopes, err := s.authorizer.RepositoryScopes(configured, profile.Authority)
	if err != nil {
		return RepositoryProfileAuthority{}, AuthorizedScopeSet{}, hiddenTargetError()
	}
	return profile, scopes, nil
}

func (c RepositoryMutationCommand) RequesterIdentity() domain.GitHubUserIdentity {
	identity, _ := c.Requester.githubUserIdentity()
	return identity
}

func repositoryOperationAuthorityDigest(authority RepositoryOperationAuthority) string {
	payload, _ := json.Marshal(struct {
		IncarnationID, Repository, ProfileDigest, BindingDigest, Intent, SnapshotID, SnapshotDigest, ConfigurationDigest string
		LifecycleVersion, ConfigurationGenerationID, ConfigurationAuthorityVersion                                       int64
	}{IncarnationID: authority.Lifecycle.IncarnationID, Repository: authority.Lifecycle.Repository, ProfileDigest: authority.Lifecycle.ProfileDigest, BindingDigest: authority.Lifecycle.RepositoryBindingDigest, Intent: string(authority.Lifecycle.Intent), SnapshotID: authority.Snapshot.SnapshotID, SnapshotDigest: authority.Snapshot.SnapshotDigest, ConfigurationDigest: authority.ConfigurationAuthority.Digest, LifecycleVersion: authority.Lifecycle.Version, ConfigurationGenerationID: authority.ConfigurationAuthority.GenerationID, ConfigurationAuthorityVersion: authority.ConfigurationAuthority.AuthorityVersion})
	return digestText("repository-operation-authority-v1\x00" + string(payload))
}

func validRepositoryIncarnationID(value string) bool {
	if !strings.HasPrefix(value, "repository-incarnation-") || len(value) != len("repository-incarnation-")+32 {
		return false
	}
	for _, char := range value[len("repository-incarnation-"):] {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return false
		}
	}
	return true
}

func classifyRepositoryError(err error) error {
	switch {
	case errors.Is(err, ErrRepositoryLifecycleMissing):
		return serviceError(ErrorNotFound, "repository was not found", err)
	case errors.Is(err, ErrRepositoryLifecycleConflict), errors.Is(err, ErrRepositoryAdmissionConflict), errors.Is(err, ErrOperationReceiptConflict):
		return serviceError(ErrorConflict, "repository authority changed", err)
	default:
		return classifyServiceError(err)
	}
}
