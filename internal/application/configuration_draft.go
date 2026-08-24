package application

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type ConfigurationFieldID string

const (
	ConfigurationFieldRunTimeout                    ConfigurationFieldID = "controller.run_timeout"
	ConfigurationFieldAdmissionEnabled              ConfigurationFieldID = "automation.linear_todo_admission.enabled"
	ConfigurationFieldAdmissionPollInterval         ConfigurationFieldID = "automation.linear_todo_admission.poll_interval"
	ConfigurationFieldDeliveryPollInterval          ConfigurationFieldID = "automation.linear_todo_admission.delivery_poll_interval"
	ConfigurationFieldSchedulerLeaseTTL             ConfigurationFieldID = "automation.linear_todo_admission.scheduler_lease_ttl"
	ConfigurationFieldSchedulerLeaseRenewalInterval ConfigurationFieldID = "automation.linear_todo_admission.scheduler_lease_renewal_interval"
	ConfigurationFieldAdmissionMaxCandidates        ConfigurationFieldID = "automation.linear_todo_admission.max_candidates"
	ConfigurationFieldAdmissionMaxPages             ConfigurationFieldID = "automation.linear_todo_admission.max_pages"
	ConfigurationFieldAdmissionHeavyCapacity        ConfigurationFieldID = "automation.linear_todo_admission.heavy_capacity"
)

var configurationFields = []ConfigurationFieldID{
	ConfigurationFieldRunTimeout,
	ConfigurationFieldAdmissionEnabled,
	ConfigurationFieldAdmissionPollInterval,
	ConfigurationFieldDeliveryPollInterval,
	ConfigurationFieldSchedulerLeaseTTL,
	ConfigurationFieldSchedulerLeaseRenewalInterval,
	ConfigurationFieldAdmissionMaxCandidates,
	ConfigurationFieldAdmissionMaxPages,
	ConfigurationFieldAdmissionHeavyCapacity,
}

type ConfigurationDuration time.Duration

func (d ConfigurationDuration) Duration() time.Duration { return time.Duration(d) }
func (d ConfigurationDuration) String() string          { return time.Duration(d).String() }

func (d ConfigurationDuration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

func (d *ConfigurationDuration) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return err
	}
	*d = ConfigurationDuration(parsed)
	return nil
}

type ConfigurationEditableSettings struct {
	RunTimeout ConfigurationDuration                  `json:"run_timeout"`
	Admission  ConfigurationEditableAdmissionSettings `json:"linear_todo_admission"`
}

type ConfigurationEditableAdmissionSettings struct {
	Enabled                       bool                  `json:"enabled"`
	PollInterval                  ConfigurationDuration `json:"poll_interval"`
	DeliveryPollInterval          ConfigurationDuration `json:"delivery_poll_interval"`
	SchedulerLeaseTTL             ConfigurationDuration `json:"scheduler_lease_ttl"`
	SchedulerLeaseRenewalInterval ConfigurationDuration `json:"scheduler_lease_renewal_interval"`
	MaxCandidates                 int                   `json:"max_candidates"`
	MaxPages                      int                   `json:"max_pages"`
	HeavyCapacity                 int                   `json:"heavy_capacity"`
}

type ConfigurationDraftState string

const (
	ConfigurationDraftOpen      ConfigurationDraftState = "open"
	ConfigurationDraftApplying  ConfigurationDraftState = "applying"
	ConfigurationDraftApplied   ConfigurationDraftState = "applied"
	ConfigurationDraftDiscarded ConfigurationDraftState = "discarded"
	ConfigurationDraftAmbiguous ConfigurationDraftState = "ambiguous"
)

type ConfigurationDraftOrigin string

const (
	ConfigurationDraftOriginNormal   ConfigurationDraftOrigin = "normal"
	ConfigurationDraftOriginRollback ConfigurationDraftOrigin = "rollback"
)

type ConfigurationValidationSeverity string

const ConfigurationValidationError ConfigurationValidationSeverity = "error"

type ConfigurationValidationReason string

const (
	ConfigurationValidationOutOfBounds      ConfigurationValidationReason = "out_of_bounds"
	ConfigurationValidationLeaseConflict    ConfigurationValidationReason = "lease_policy_conflict"
	ConfigurationValidationCandidateInvalid ConfigurationValidationReason = "candidate_invalid"
)

type ConfigurationValidationFinding struct {
	Field    ConfigurationFieldID            `json:"field"`
	Reason   ConfigurationValidationReason   `json:"reason_code"`
	Severity ConfigurationValidationSeverity `json:"severity"`
}

type ConfigurationValidationResult struct {
	DraftID          string                           `json:"draft_id"`
	Revision         int64                            `json:"revision"`
	CandidateDigest  string                           `json:"candidate_digest"`
	ValidationDigest string                           `json:"validation_digest"`
	Valid            bool                             `json:"valid"`
	Findings         []ConfigurationValidationFinding `json:"findings"`
	ValidatedAt      time.Time                        `json:"validated_at"`
}

type ConfigurationPreviewCategory string

const (
	ConfigurationPreviewRunTimeoutChanged            ConfigurationPreviewCategory = "run_timeout_changed"
	ConfigurationPreviewAutomaticAdmissionEnabled    ConfigurationPreviewCategory = "automatic_admission_enabled"
	ConfigurationPreviewAutomaticAdmissionDisabled   ConfigurationPreviewCategory = "automatic_admission_disabled"
	ConfigurationPreviewAdmissionPollIntervalChanged ConfigurationPreviewCategory = "admission_poll_interval_changed"
	ConfigurationPreviewDeliveryPollIntervalChanged  ConfigurationPreviewCategory = "delivery_poll_interval_changed"
	ConfigurationPreviewSchedulerLeasePolicyChanged  ConfigurationPreviewCategory = "scheduler_lease_policy_changed"
	ConfigurationPreviewCandidateScanBoundsChanged   ConfigurationPreviewCategory = "candidate_scan_bounds_changed"
	ConfigurationPreviewHeavyCapacityIncreased       ConfigurationPreviewCategory = "heavy_capacity_increased"
	ConfigurationPreviewHeavyCapacityDecreased       ConfigurationPreviewCategory = "heavy_capacity_decreased"
)

type ConfigurationPreviewImpact string

const (
	ConfigurationImpactWorkerReloadRequired                   ConfigurationPreviewImpact = "worker_reload_required"
	ConfigurationImpactNewAdmissionFencedUntilConverged       ConfigurationPreviewImpact = "new_admission_fenced_until_converged"
	ConfigurationImpactActiveRunsContinueUnderFrozenAuthority ConfigurationPreviewImpact = "active_runs_continue_under_frozen_authority"
	ConfigurationImpactCapacityReductionUsesDrainSemantics    ConfigurationPreviewImpact = "capacity_reduction_uses_drain_semantics"
)

type ConfigurationPreviewValue struct {
	Boolean  *bool                  `json:"boolean,omitempty"`
	Duration *ConfigurationDuration `json:"duration,omitempty"`
	Integer  *int                   `json:"integer,omitempty"`
}

type ConfigurationPreviewFieldChange struct {
	Field  ConfigurationFieldID      `json:"field"`
	Before ConfigurationPreviewValue `json:"before"`
	After  ConfigurationPreviewValue `json:"after"`
}

type ConfigurationPreviewChange struct {
	Category ConfigurationPreviewCategory      `json:"category"`
	Fields   []ConfigurationPreviewFieldChange `json:"fields"`
}

type ConfigurationPreview struct {
	DraftID                    string                       `json:"draft_id"`
	Revision                   int64                        `json:"revision"`
	BaseGenerationID           int64                        `json:"base_generation_id"`
	BaseDigest                 string                       `json:"base_digest"`
	CandidateDigest            string                       `json:"candidate_digest"`
	DraftOrigin                ConfigurationDraftOrigin     `json:"draft_origin"`
	RollbackSourceGenerationID int64                        `json:"rollback_source_generation_id,omitempty"`
	RollbackSourceDigest       string                       `json:"rollback_source_digest,omitempty"`
	PreviewDigest              string                       `json:"preview_digest"`
	Changes                    []ConfigurationPreviewChange `json:"changes"`
	Impacts                    []ConfigurationPreviewImpact `json:"impacts"`
	PreviewedAt                time.Time                    `json:"previewed_at"`
}

type ConfigurationDraft struct {
	DraftID                    string                         `json:"draft_id"`
	BaseGenerationID           int64                          `json:"base_generation_id"`
	BaseDigest                 string                         `json:"base_digest"`
	Revision                   int64                          `json:"revision"`
	State                      ConfigurationDraftState        `json:"state"`
	DraftOrigin                ConfigurationDraftOrigin       `json:"draft_origin"`
	RollbackSourceGenerationID int64                          `json:"rollback_source_generation_id,omitempty"`
	RollbackSourceDigest       string                         `json:"rollback_source_digest,omitempty"`
	Settings                   ConfigurationEditableSettings  `json:"settings"`
	SettingsDigest             string                         `json:"settings_digest"`
	LastEditField              ConfigurationFieldID           `json:"last_edit_field,omitempty"`
	LastEditBaseRevision       int64                          `json:"-"`
	LastEditDigest             string                         `json:"-"`
	Validation                 *ConfigurationValidationResult `json:"validation,omitempty"`
	Preview                    *ConfigurationPreview          `json:"preview,omitempty"`
	ResultOperationID          string                         `json:"result_operation_id,omitempty"`
	ResultGenerationID         int64                          `json:"result_generation_id,omitempty"`
	ResultNoOp                 bool                           `json:"result_no_op,omitempty"`
	CreatedAt                  time.Time                      `json:"created_at"`
	UpdatedAt                  time.Time                      `json:"updated_at"`
	SettledAt                  time.Time                      `json:"settled_at,omitempty"`
	Reason                     string                         `json:"reason_code,omitempty"`
}

type ConfigurationEdit struct {
	Field    ConfigurationFieldID
	Boolean  *bool
	Duration *ConfigurationDuration
	Integer  *int
}

type ConfigurationDraftOpenInput struct {
	DraftID                    string
	BaseGenerationID           int64
	BaseDigest                 string
	Settings                   ConfigurationEditableSettings
	SettingsDigest             string
	DraftOrigin                ConfigurationDraftOrigin
	RollbackSourceGenerationID int64
	RollbackSourceDigest       string
	OpenedAt                   time.Time
}

type ConfigurationDraftEditInput struct {
	DraftID          string
	ExpectedRevision int64
	Settings         ConfigurationEditableSettings
	SettingsDigest   string
	Field            ConfigurationFieldID
	EditDigest       string
	EditedAt         time.Time
}

type ConfigurationDraftMetadataInput struct {
	DraftID                    string
	ExpectedRevision           int64
	Validation                 *ConfigurationValidationResult
	Preview                    *ConfigurationPreview
	DraftOrigin                ConfigurationDraftOrigin
	RollbackSourceGenerationID int64
	RollbackSourceDigest       string
	UpdatedAt                  time.Time
}

type ConfigurationDraftApplyBinding struct {
	DraftID          string
	ExpectedRevision int64
	PreviewDigest    string
	State            ConfigurationDraftState
	OperationID      string
	GenerationID     int64
	NoOp             bool
	Reason           string
	UpdatedAt        time.Time
}

type ConfigurationDraftDiscardInput struct {
	DraftID          string
	ExpectedRevision int64
	DiscardedAt      time.Time
}

type ConfigurationDraftStore interface {
	OpenConfigurationDraft(context.Context, ConfigurationDraftOpenInput) (ConfigurationDraft, bool, error)
	ConfigurationDraft(context.Context, string) (ConfigurationDraft, bool, error)
	ActiveConfigurationDraft(context.Context) (ConfigurationDraft, bool, error)
	EditConfigurationDraft(context.Context, ConfigurationDraftEditInput) (ConfigurationDraft, bool, error)
	RecordConfigurationDraftMetadata(context.Context, ConfigurationDraftMetadataInput) (ConfigurationDraft, error)
	BindConfigurationDraftApply(context.Context, ConfigurationDraftApplyBinding) (ConfigurationDraft, bool, error)
	DiscardConfigurationDraft(context.Context, ConfigurationDraftDiscardInput) (ConfigurationDraft, bool, error)
}

type ConfigurationDraftDocument interface {
	ProjectEditable([]byte) (ConfigurationEditableSettings, error)
	ProjectHistoricalEditable([]byte, int) (ConfigurationEditableSettings, error)
	MaterializeEditable([]byte, ConfigurationEditableSettings) ([]byte, error)
	ValidateEditableCandidate([]byte, []byte) (ValidatedConfigurationCandidate, error)
}

type ConfigurationRollbackSource struct {
	GenerationID  int64                         `json:"generation_id"`
	Digest        string                        `json:"digest"`
	SchemaVersion int                           `json:"schema_version"`
	Origin        ConfigurationGenerationOrigin `json:"origin"`
	CommittedAt   time.Time                     `json:"committed_at"`
	EffectiveAt   *time.Time                    `json:"effective_at,omitempty"`
	SupersededAt  time.Time                     `json:"superseded_at"`
}

type ConfigurationRollbackSources struct {
	DesiredGenerationID int64                         `json:"desired_generation_id"`
	DesiredDigest       string                        `json:"desired_digest"`
	Sources             []ConfigurationRollbackSource `json:"sources"`
}

type ConfigurationRollbackOpenCommand struct {
	Requester            Requester
	SourceGenerationID   int64
	SourceDigest         string
	ExpectedGenerationID int64
	ExpectedDigest       string
}

type ConfigurationDraftService struct {
	configuration *ConfigurationService
	store         ConfigurationDraftStore
	document      ConfigurationDraftDocument
	now           func() time.Time
}

func NewConfigurationDraftService(configuration *ConfigurationService, store ConfigurationDraftStore, document ConfigurationDraftDocument) (*ConfigurationDraftService, error) {
	if configuration == nil || store == nil || document == nil {
		return nil, errors.New("configuration draft dependencies are required")
	}
	return &ConfigurationDraftService{configuration: configuration, store: store, document: document, now: func() time.Time { return time.Now().UTC() }}, nil
}

type ConfigurationDraftCommand struct {
	Requester Requester
	DraftID   string
	Revision  int64
}

type ConfigurationDraftEditCommand struct {
	Requester Requester
	DraftID   string
	Revision  int64
	Edit      ConfigurationEdit
}

type ConfigurationDraftApplyCommand struct {
	Requester            Requester
	DraftID              string
	Revision             int64
	PreviewDigest        string
	ExpectedGenerationID int64
	ExpectedDigest       string
}

type ConfigurationDraftApplyResult struct {
	Apply       ConfigurationApplyResult           `json:"apply"`
	Convergence ConfigurationConvergenceProjection `json:"convergence"`
}

type ManagedConfigurationStatus struct {
	Convergence ConfigurationConvergenceProjection `json:"convergence"`
	ActiveDraft *ConfigurationDraft                `json:"active_draft,omitempty"`
}

func (s *ConfigurationDraftService) Open(ctx context.Context, requester Requester) (ConfigurationDraft, error) {
	authority, _, _, err := s.configuration.authorize(ctx, requester)
	if err != nil {
		return ConfigurationDraft{}, err
	}
	if authority.Desired.SchemaVersion != 5 {
		return ConfigurationDraft{}, serviceError(ErrorConflict, "configuration schema upgrade is required", nil)
	}
	base, err := s.configuration.files.ReadRaw(authority.Desired.Digest, authority.Desired.Size)
	if err != nil {
		return ConfigurationDraft{}, serviceError(ErrorConflict, "configuration base evidence is unavailable", nil)
	}
	settings, err := s.document.ProjectEditable(base)
	if err != nil {
		return ConfigurationDraft{}, serviceError(ErrorConflict, "configuration base cannot be projected", nil)
	}
	draftID, err := newConfigurationDraftID()
	if err != nil {
		return ConfigurationDraft{}, serviceError(ErrorInternal, "configuration draft identity is unavailable", nil)
	}
	draft, _, err := s.store.OpenConfigurationDraft(ctx, ConfigurationDraftOpenInput{DraftID: draftID, BaseGenerationID: authority.Desired.GenerationID, BaseDigest: authority.Desired.Digest, Settings: settings, SettingsDigest: ConfigurationSettingsDigest(settings), DraftOrigin: ConfigurationDraftOriginNormal, OpenedAt: s.now().UTC()})
	if err != nil {
		return ConfigurationDraft{}, serviceError(ErrorInternal, "configuration draft could not be opened", nil)
	}
	return draft, nil
}

func (s *ConfigurationDraftService) RollbackSources(ctx context.Context, requester Requester) (ConfigurationRollbackSources, error) {
	authority, _, _, err := s.configuration.authorize(ctx, requester)
	if err != nil {
		return ConfigurationRollbackSources{}, err
	}
	generations, err := s.configuration.store.ListConfigurationGenerations(ctx)
	if err != nil {
		return ConfigurationRollbackSources{}, serviceError(ErrorInternal, "configuration rollback sources are unavailable", nil)
	}
	result := ConfigurationRollbackSources{DesiredGenerationID: authority.Desired.GenerationID, DesiredDigest: authority.Desired.Digest, Sources: []ConfigurationRollbackSource{}}
	for _, generation := range generations {
		if len(result.Sources) >= ConfigurationRawRetainCount {
			break
		}
		if !eligibleRollbackGeneration(generation, authority.Desired.GenerationID) {
			continue
		}
		payload, readErr := s.configuration.files.ReadRaw(generation.Digest, generation.Size)
		if readErr != nil {
			continue
		}
		settings, projectionErr := s.document.ProjectHistoricalEditable(payload, generation.SchemaVersion)
		if projectionErr != nil || len(configurationSettingsFindings(settings)) != 0 {
			continue
		}
		var effectiveAt *time.Time
		if !generation.EffectiveAt.IsZero() {
			value := generation.EffectiveAt
			effectiveAt = &value
		}
		result.Sources = append(result.Sources, ConfigurationRollbackSource{GenerationID: generation.GenerationID, Digest: generation.Digest, SchemaVersion: generation.SchemaVersion, Origin: generation.Origin, CommittedAt: generation.CommittedAt, EffectiveAt: effectiveAt, SupersededAt: generation.SupersededAt})
	}
	return result, nil
}

func (s *ConfigurationDraftService) OpenRollback(ctx context.Context, command ConfigurationRollbackOpenCommand) (ConfigurationDraft, error) {
	if command.SourceGenerationID <= 0 || command.ExpectedGenerationID <= 0 || !validAuthorityDigest(command.SourceDigest) || !validAuthorityDigest(command.ExpectedDigest) {
		return ConfigurationDraft{}, serviceError(ErrorInvalidInput, "configuration rollback authority is invalid", nil)
	}
	if _, _, _, err := s.configuration.authorize(ctx, command.Requester); err != nil {
		return ConfigurationDraft{}, err
	}
	authority, err := s.configuration.Reconcile(ctx)
	if err != nil {
		return ConfigurationDraft{}, err
	}
	// Reconciliation may settle an apply that changes configured authority.
	authority, _, _, err = s.configuration.authorize(ctx, command.Requester)
	if err != nil {
		return ConfigurationDraft{}, err
	}
	if authority.Incomplete != nil || authority.Desired.GenerationID != command.ExpectedGenerationID || authority.Desired.Digest != command.ExpectedDigest || authority.Desired.SchemaVersion != 5 {
		return ConfigurationDraft{}, serviceError(ErrorConflict, "configuration rollback authority changed", nil)
	}
	livePayload, live, liveErr := s.configuration.files.ReadLive()
	desiredPayload, desiredErr := s.configuration.files.ReadRaw(authority.Desired.Digest, authority.Desired.Size)
	if liveErr != nil || desiredErr != nil || live.Digest != authority.Desired.Digest || !bytes.Equal(livePayload, desiredPayload) {
		return ConfigurationDraft{}, serviceError(ErrorConflict, "live configuration conflicts with desired authority", nil)
	}
	if active, found, activeErr := s.store.ActiveConfigurationDraft(ctx); activeErr != nil {
		return ConfigurationDraft{}, serviceError(ErrorInternal, "configuration draft authority is unavailable", nil)
	} else if found {
		if active.DraftOrigin == ConfigurationDraftOriginRollback && active.BaseGenerationID == command.ExpectedGenerationID && active.BaseDigest == command.ExpectedDigest && active.RollbackSourceGenerationID == command.SourceGenerationID && active.RollbackSourceDigest == command.SourceDigest {
			return active, nil
		}
		return ConfigurationDraft{}, serviceError(ErrorConflict, "configuration rollback draft conflicts", nil)
	}
	generations, err := s.configuration.store.ListConfigurationGenerations(ctx)
	if err != nil {
		return ConfigurationDraft{}, serviceError(ErrorInternal, "configuration rollback source is unavailable", nil)
	}
	source, found := generationByID(generations, command.SourceGenerationID)
	if !found || source.Digest != command.SourceDigest || !eligibleRollbackGeneration(source, authority.Desired.GenerationID) {
		return ConfigurationDraft{}, serviceError(ErrorConflict, "configuration rollback source is ineligible", nil)
	}
	payload, err := s.configuration.files.ReadRaw(source.Digest, source.Size)
	if err != nil {
		return ConfigurationDraft{}, serviceError(ErrorConflict, "configuration rollback source is unavailable", nil)
	}
	settings, err := s.document.ProjectHistoricalEditable(payload, source.SchemaVersion)
	if err != nil || len(configurationSettingsFindings(settings)) != 0 {
		return ConfigurationDraft{}, serviceError(ErrorConflict, "configuration rollback source is incompatible", nil)
	}
	draftID, err := newConfigurationDraftID()
	if err != nil {
		return ConfigurationDraft{}, serviceError(ErrorInternal, "configuration draft identity is unavailable", nil)
	}
	input := ConfigurationDraftOpenInput{DraftID: draftID, BaseGenerationID: authority.Desired.GenerationID, BaseDigest: authority.Desired.Digest, Settings: settings, SettingsDigest: ConfigurationSettingsDigest(settings), DraftOrigin: ConfigurationDraftOriginRollback, RollbackSourceGenerationID: source.GenerationID, RollbackSourceDigest: source.Digest, OpenedAt: s.now().UTC()}
	draft, _, err := s.store.OpenConfigurationDraft(ctx, input)
	if err != nil {
		return ConfigurationDraft{}, serviceError(ErrorConflict, "configuration rollback draft conflicts", nil)
	}
	if draft.DraftOrigin != ConfigurationDraftOriginRollback || draft.BaseGenerationID != input.BaseGenerationID || draft.BaseDigest != input.BaseDigest || draft.RollbackSourceGenerationID != input.RollbackSourceGenerationID || draft.RollbackSourceDigest != input.RollbackSourceDigest {
		return ConfigurationDraft{}, serviceError(ErrorConflict, "configuration rollback draft conflicts", nil)
	}
	return draft, nil
}

func eligibleRollbackGeneration(generation ConfigurationGeneration, desiredID int64) bool {
	if generation.GenerationID <= 0 || generation.GenerationID == desiredID || generation.State != ConfigurationGenerationSuperseded || !generation.RawRetained || !generation.SettlementEvidenceValid || generation.SchemaVersion < 1 || generation.SchemaVersion > 5 || !validAuthorityDigest(generation.Digest) {
		return false
	}
	if generation.Origin != ConfigurationOriginBaseline && generation.Origin != ConfigurationOriginApply {
		return false
	}
	if generation.CommittedAt.IsZero() || generation.SupersededAt.IsZero() || generation.SettledAt.IsZero() || generation.CommittedAt.Before(generation.CreatedAt) || generation.SupersededAt.Before(generation.CommittedAt) || generation.SettledAt != generation.SupersededAt || !generation.EffectiveAt.IsZero() && (generation.EffectiveAt.Before(generation.CommittedAt) || generation.SupersededAt.Before(generation.EffectiveAt)) {
		return false
	}
	if generation.Origin == ConfigurationOriginBaseline {
		return generation.ParentID == 0 && generation.OperationID == "" && generation.Requester == (domain.GitHubUserIdentity{})
	}
	return generation.ParentID > 0 && generation.OperationID != "" && generation.Requester.Validate() == nil
}

func (s *ConfigurationDraftService) Show(ctx context.Context, command ConfigurationDraftCommand) (ConfigurationDraft, error) {
	if _, _, _, err := s.configuration.authorize(ctx, command.Requester); err != nil {
		return ConfigurationDraft{}, err
	}
	draft, found, err := s.store.ConfigurationDraft(ctx, command.DraftID)
	if err != nil || !found {
		return ConfigurationDraft{}, hiddenTargetError()
	}
	if command.Revision > 0 && draft.Revision != command.Revision {
		return ConfigurationDraft{}, serviceError(ErrorConflict, "configuration draft revision changed", nil)
	}
	return draft, nil
}

func (s *ConfigurationDraftService) Edit(ctx context.Context, command ConfigurationDraftEditCommand) (ConfigurationDraft, error) {
	authority, _, _, err := s.configuration.authorize(ctx, command.Requester)
	if err != nil {
		return ConfigurationDraft{}, err
	}
	draft, found, err := s.store.ConfigurationDraft(ctx, command.DraftID)
	if err != nil || !found {
		return ConfigurationDraft{}, hiddenTargetError()
	}
	if draft.State != ConfigurationDraftOpen || draft.BaseGenerationID != authority.Desired.GenerationID || draft.BaseDigest != authority.Desired.Digest {
		return ConfigurationDraft{}, serviceError(ErrorConflict, "configuration draft authority changed", nil)
	}
	settings, err := applyConfigurationEdit(draft.Settings, command.Edit)
	if err != nil {
		return ConfigurationDraft{}, serviceError(ErrorInvalidInput, "configuration edit is invalid", nil)
	}
	editDigest := configurationEditDigest(command.Edit)
	updated, _, err := s.store.EditConfigurationDraft(ctx, ConfigurationDraftEditInput{DraftID: command.DraftID, ExpectedRevision: command.Revision, Settings: settings, SettingsDigest: ConfigurationSettingsDigest(settings), Field: command.Edit.Field, EditDigest: editDigest, EditedAt: s.now().UTC()})
	if err != nil {
		return ConfigurationDraft{}, serviceError(ErrorConflict, "configuration draft revision changed", nil)
	}
	return updated, nil
}

func (s *ConfigurationDraftService) Validate(ctx context.Context, command ConfigurationDraftCommand) (ConfigurationValidationResult, error) {
	authority, _, _, err := s.configuration.authorize(ctx, command.Requester)
	if err != nil {
		return ConfigurationValidationResult{}, err
	}
	draft, base, err := s.mutableDraft(ctx, command, authority)
	if err != nil {
		return ConfigurationValidationResult{}, err
	}
	result, _ := s.computeValidation(draft, base)
	result.ValidatedAt = s.now().UTC()
	stored, err := s.store.RecordConfigurationDraftMetadata(ctx, ConfigurationDraftMetadataInput{DraftID: draft.DraftID, ExpectedRevision: draft.Revision, Validation: &result, DraftOrigin: draft.DraftOrigin, RollbackSourceGenerationID: draft.RollbackSourceGenerationID, RollbackSourceDigest: draft.RollbackSourceDigest, UpdatedAt: result.ValidatedAt})
	if err != nil || stored.Validation == nil {
		return ConfigurationValidationResult{}, serviceError(ErrorConflict, "configuration draft revision changed", nil)
	}
	return *stored.Validation, nil
}

func (s *ConfigurationDraftService) Preview(ctx context.Context, command ConfigurationDraftCommand) (ConfigurationPreview, error) {
	authority, _, _, err := s.configuration.authorize(ctx, command.Requester)
	if err != nil {
		return ConfigurationPreview{}, err
	}
	draft, base, err := s.mutableDraft(ctx, command, authority)
	if err != nil {
		return ConfigurationPreview{}, err
	}
	validation, candidate := s.computeValidation(draft, base)
	if !validation.Valid {
		return ConfigurationPreview{}, serviceError(ErrorInvalidInput, "configuration draft is not valid", nil)
	}
	preview, err := s.computePreview(draft, base, candidate, authority)
	if err != nil {
		return ConfigurationPreview{}, serviceError(ErrorInternal, "configuration preview is unavailable", nil)
	}
	preview.PreviewedAt = s.now().UTC()
	validation.ValidatedAt = preview.PreviewedAt
	stored, err := s.store.RecordConfigurationDraftMetadata(ctx, ConfigurationDraftMetadataInput{DraftID: draft.DraftID, ExpectedRevision: draft.Revision, Validation: &validation, Preview: &preview, DraftOrigin: draft.DraftOrigin, RollbackSourceGenerationID: draft.RollbackSourceGenerationID, RollbackSourceDigest: draft.RollbackSourceDigest, UpdatedAt: preview.PreviewedAt})
	if err != nil || stored.Preview == nil {
		return ConfigurationPreview{}, serviceError(ErrorConflict, "configuration draft revision changed", nil)
	}
	return *stored.Preview, nil
}

func (s *ConfigurationDraftService) Apply(ctx context.Context, command ConfigurationDraftApplyCommand) (ConfigurationDraftApplyResult, error) {
	authority, _, _, err := s.configuration.authorize(ctx, command.Requester)
	if err != nil {
		return ConfigurationDraftApplyResult{}, err
	}
	draft, found, err := s.store.ConfigurationDraft(ctx, command.DraftID)
	if err != nil || !found {
		return ConfigurationDraftApplyResult{}, hiddenTargetError()
	}
	if draft.State == ConfigurationDraftApplied {
		if draft.Revision != command.Revision || draft.Preview == nil || draft.Preview.PreviewDigest != command.PreviewDigest {
			return ConfigurationDraftApplyResult{}, serviceError(ErrorConflict, "configuration draft apply authority changed", nil)
		}
		return s.appliedReplay(ctx, command, draft)
	}
	if draft.State != ConfigurationDraftOpen && draft.State != ConfigurationDraftApplying {
		return ConfigurationDraftApplyResult{}, serviceError(ErrorConflict, "configuration draft is not applicable", nil)
	}
	if draft.Revision != command.Revision || draft.BaseGenerationID != command.ExpectedGenerationID || draft.BaseDigest != command.ExpectedDigest {
		return ConfigurationDraftApplyResult{}, serviceError(ErrorConflict, "configuration draft apply authority changed", nil)
	}
	if draft.State == ConfigurationDraftOpen && (authority.Desired.GenerationID != command.ExpectedGenerationID || authority.Desired.Digest != command.ExpectedDigest) {
		return ConfigurationDraftApplyResult{}, serviceError(ErrorConflict, "configuration draft apply authority changed", nil)
	}
	generations, err := s.configuration.store.ListConfigurationGenerations(ctx)
	if err != nil {
		return ConfigurationDraftApplyResult{}, serviceError(ErrorInternal, "configuration generation evidence is unavailable", nil)
	}
	baseGeneration, found := generationByID(generations, draft.BaseGenerationID)
	if !found || baseGeneration.Digest != draft.BaseDigest {
		return ConfigurationDraftApplyResult{}, serviceError(ErrorConflict, "configuration base evidence conflicts", nil)
	}
	base, err := s.configuration.files.ReadRaw(draft.BaseDigest, baseGeneration.Size)
	if err != nil {
		return ConfigurationDraftApplyResult{}, serviceError(ErrorConflict, "configuration base evidence is unavailable", nil)
	}
	validation, candidate := s.computeValidation(draft, base)
	if !validation.Valid {
		return ConfigurationDraftApplyResult{}, serviceError(ErrorInvalidInput, "configuration draft is not valid", nil)
	}
	candidateDigest := digestBytes(candidate)
	if draft.State == ConfigurationDraftApplying && authority.Desired.Digest != draft.BaseDigest && authority.Desired.Digest != candidateDigest {
		return ConfigurationDraftApplyResult{}, serviceError(ErrorConflict, "configuration draft apply authority changed", nil)
	}
	preview, err := s.computePreview(draft, base, candidate, authority)
	desiredReplay := authority.Desired.Digest == candidateDigest && (candidateDigest == draft.BaseDigest || generationProvenanceMatchesDraft(authority.Desired, draft))
	incompleteReplay := false
	if authority.Incomplete != nil && authority.Incomplete.ParentID == draft.BaseGenerationID && authority.Incomplete.ParentDigest == draft.BaseDigest && authority.Incomplete.TargetDigest == candidateDigest {
		if generation, exists := generationByID(generations, authority.Incomplete.GenerationID); exists {
			incompleteReplay = generationProvenanceMatchesDraft(generation, draft)
		}
	}
	acceptedReplay := draft.State == ConfigurationDraftApplying && draft.Preview != nil && draft.Preview.PreviewDigest == command.PreviewDigest && (desiredReplay || incompleteReplay)
	if err != nil || preview.PreviewDigest != command.PreviewDigest && !acceptedReplay {
		return ConfigurationDraftApplyResult{}, serviceError(ErrorConflict, "configuration preview authority changed", nil)
	}
	preview.PreviewDigest = command.PreviewDigest
	if draft.State == ConfigurationDraftOpen {
		bound, _, bindErr := s.store.BindConfigurationDraftApply(ctx, ConfigurationDraftApplyBinding{DraftID: draft.DraftID, ExpectedRevision: draft.Revision, PreviewDigest: preview.PreviewDigest, State: ConfigurationDraftApplying, UpdatedAt: s.now().UTC()})
		if bindErr != nil {
			return ConfigurationDraftApplyResult{}, serviceError(ErrorConflict, "configuration draft apply authority changed", nil)
		}
		draft = bound
	}
	result, applyErr := s.configuration.Apply(ctx, ConfigurationApplyCommand{Requester: command.Requester, ExpectedGenerationID: command.ExpectedGenerationID, ExpectedDigest: command.ExpectedDigest, Payload: candidate, Provenance: draftApplyProvenance(draft)})
	if applyErr != nil {
		if !s.bindIncompleteApply(ctx, draft, candidate, preview.PreviewDigest) {
			_, _, _ = s.store.BindConfigurationDraftApply(ctx, ConfigurationDraftApplyBinding{DraftID: draft.DraftID, ExpectedRevision: draft.Revision, PreviewDigest: preview.PreviewDigest, State: ConfigurationDraftOpen, Reason: "apply_rejected", UpdatedAt: s.now().UTC()})
		}
		return ConfigurationDraftApplyResult{}, applyErr
	}
	settled, _, err := s.store.BindConfigurationDraftApply(ctx, ConfigurationDraftApplyBinding{DraftID: draft.DraftID, ExpectedRevision: draft.Revision, PreviewDigest: preview.PreviewDigest, State: ConfigurationDraftApplied, OperationID: result.Receipt.OperationID, GenerationID: result.Generation.GenerationID, NoOp: result.NoOp, Reason: string(result.Generation.Reason), UpdatedAt: s.now().UTC()})
	if err != nil || settled.State != ConfigurationDraftApplied {
		return ConfigurationDraftApplyResult{}, serviceError(ErrorConflict, "configuration draft settlement requires replay", nil)
	}
	projection, err := s.configuration.Projection(ctx, command.Requester, s.now().UTC())
	if err != nil {
		return ConfigurationDraftApplyResult{}, err
	}
	return ConfigurationDraftApplyResult{Apply: result, Convergence: projection}, nil
}

func (s *ConfigurationDraftService) Discard(ctx context.Context, command ConfigurationDraftCommand) (ConfigurationDraft, error) {
	if _, _, _, err := s.configuration.authorize(ctx, command.Requester); err != nil {
		return ConfigurationDraft{}, err
	}
	draft, _, err := s.store.DiscardConfigurationDraft(ctx, ConfigurationDraftDiscardInput{DraftID: command.DraftID, ExpectedRevision: command.Revision, DiscardedAt: s.now().UTC()})
	if err != nil {
		return ConfigurationDraft{}, serviceError(ErrorConflict, "configuration draft discard authority changed", nil)
	}
	return draft, nil
}

func (s *ConfigurationDraftService) Status(ctx context.Context, requester Requester) (ManagedConfigurationStatus, error) {
	if _, _, _, err := s.configuration.authorize(ctx, requester); err != nil {
		return ManagedConfigurationStatus{}, err
	}
	projection, err := s.configuration.Projection(ctx, requester, s.now().UTC())
	if err != nil {
		return ManagedConfigurationStatus{}, err
	}
	draft, found, err := s.store.ActiveConfigurationDraft(ctx)
	if err != nil {
		return ManagedConfigurationStatus{}, serviceError(ErrorInternal, "configuration draft status is unavailable", nil)
	}
	status := ManagedConfigurationStatus{Convergence: projection}
	if found {
		status.ActiveDraft = &draft
	}
	return status, nil
}

func (s *ConfigurationDraftService) mutableDraft(ctx context.Context, command ConfigurationDraftCommand, authority ConfigurationAuthority) (ConfigurationDraft, []byte, error) {
	draft, found, err := s.store.ConfigurationDraft(ctx, command.DraftID)
	if err != nil || !found {
		return ConfigurationDraft{}, nil, hiddenTargetError()
	}
	if draft.State != ConfigurationDraftOpen || draft.Revision != command.Revision || draft.BaseGenerationID != authority.Desired.GenerationID || draft.BaseDigest != authority.Desired.Digest {
		return ConfigurationDraft{}, nil, serviceError(ErrorConflict, "configuration draft authority changed", nil)
	}
	base, err := s.configuration.files.ReadRaw(draft.BaseDigest, authority.Desired.Size)
	if err != nil {
		return ConfigurationDraft{}, nil, serviceError(ErrorConflict, "configuration base evidence is unavailable", nil)
	}
	return draft, base, nil
}

func (s *ConfigurationDraftService) computeValidation(draft ConfigurationDraft, base []byte) (ConfigurationValidationResult, []byte) {
	candidate, materializeErr := s.document.MaterializeEditable(base, draft.Settings)
	candidateDigest := digestBytes(candidate)
	if candidateDigest == "" {
		candidateDigest = configurationDigest("configuration-invalid-candidate-v1", draft.SettingsDigest)
	}
	findings := configurationSettingsFindings(draft.Settings)
	if findings == nil {
		findings = []ConfigurationValidationFinding{}
	}
	if materializeErr != nil || len(candidate) > 256<<10 {
		findings = appendFinding(findings, validationField(draft), ConfigurationValidationCandidateInvalid)
	} else if len(findings) == 0 {
		validated, err := s.document.ValidateEditableCandidate(base, candidate)
		if err != nil || validated.Digest != candidateDigest || validated.Size != int64(len(candidate)) || validated.SchemaVersion != 5 {
			findings = appendFinding(findings, validationField(draft), ConfigurationValidationCandidateInvalid)
		}
	}
	findingsJSON, _ := json.Marshal(findings)
	validationDigest := configurationDigest("configuration-validation-v1", draft.DraftID, strconv.FormatInt(draft.Revision, 10), draft.BaseDigest, candidateDigest, string(findingsJSON))
	return ConfigurationValidationResult{DraftID: draft.DraftID, Revision: draft.Revision, CandidateDigest: candidateDigest, ValidationDigest: validationDigest, Valid: len(findings) == 0, Findings: findings}, candidate
}

func (s *ConfigurationDraftService) computePreview(draft ConfigurationDraft, base, candidate []byte, authority ConfigurationAuthority) (ConfigurationPreview, error) {
	baseSettings, err := s.document.ProjectEditable(base)
	if err != nil {
		return ConfigurationPreview{}, err
	}
	changes := configurationSemanticChanges(baseSettings, draft.Settings)
	impacts := configurationPreviewImpacts(changes)
	changesJSON, _ := json.Marshal(changes)
	impactsJSON, _ := json.Marshal(impacts)
	candidateDigest := digestBytes(candidate)
	previewDigest := configurationDigest("configuration-preview-v1", draft.DraftID, strconv.FormatInt(draft.Revision, 10), strconv.FormatInt(draft.BaseGenerationID, 10), draft.BaseDigest, candidateDigest, string(changesJSON), string(impactsJSON), strconv.FormatInt(authority.Desired.GenerationID, 10), authority.Desired.Digest, strconv.FormatInt(authority.Version, 10))
	if draft.DraftOrigin == ConfigurationDraftOriginRollback {
		previewDigest = configurationDigest("configuration-preview-v2", draft.DraftID, strconv.FormatInt(draft.Revision, 10), strconv.FormatInt(draft.BaseGenerationID, 10), draft.BaseDigest, string(draft.DraftOrigin), strconv.FormatInt(draft.RollbackSourceGenerationID, 10), draft.RollbackSourceDigest, candidateDigest, string(changesJSON), string(impactsJSON), strconv.FormatInt(authority.Desired.GenerationID, 10), authority.Desired.Digest, strconv.FormatInt(authority.Version, 10))
	}
	return ConfigurationPreview{DraftID: draft.DraftID, Revision: draft.Revision, BaseGenerationID: draft.BaseGenerationID, BaseDigest: draft.BaseDigest, CandidateDigest: candidateDigest, DraftOrigin: draft.DraftOrigin, RollbackSourceGenerationID: draft.RollbackSourceGenerationID, RollbackSourceDigest: draft.RollbackSourceDigest, PreviewDigest: previewDigest, Changes: changes, Impacts: impacts}, nil
}

func (s *ConfigurationDraftService) bindIncompleteApply(ctx context.Context, draft ConfigurationDraft, candidate []byte, previewDigest string) bool {
	authority, found, err := s.configuration.store.ConfigurationAuthority(ctx)
	if err != nil || !found {
		return false
	}
	candidateDigest := digestBytes(candidate)
	generations, err := s.configuration.store.ListConfigurationGenerations(ctx)
	if err != nil {
		return false
	}
	if authority.Incomplete != nil && authority.Incomplete.TargetDigest == candidateDigest {
		generation, found := generationByID(generations, authority.Incomplete.GenerationID)
		if !found || !generationProvenanceMatchesDraft(generation, draft) {
			return false
		}
		state := ConfigurationDraftApplying
		if authority.Incomplete.State == ConfigurationApplyAmbiguous {
			state = ConfigurationDraftAmbiguous
		}
		_, _, _ = s.store.BindConfigurationDraftApply(ctx, ConfigurationDraftApplyBinding{DraftID: draft.DraftID, ExpectedRevision: draft.Revision, PreviewDigest: previewDigest, State: state, OperationID: authority.Incomplete.OperationID, GenerationID: authority.Incomplete.GenerationID, Reason: string(authority.Incomplete.Reason), UpdatedAt: s.now().UTC()})
		return true
	}
	if authority.Desired.ParentID == draft.BaseGenerationID && authority.Desired.Digest == candidateDigest && authority.Desired.OperationID != "" && generationProvenanceMatchesDraft(authority.Desired, draft) {
		_, _, _ = s.store.BindConfigurationDraftApply(ctx, ConfigurationDraftApplyBinding{DraftID: draft.DraftID, ExpectedRevision: draft.Revision, PreviewDigest: previewDigest, State: ConfigurationDraftApplying, OperationID: authority.Desired.OperationID, GenerationID: authority.Desired.GenerationID, Reason: string(authority.Desired.Reason), UpdatedAt: s.now().UTC()})
		return true
	}
	return false
}

func (s *ConfigurationDraftService) appliedReplay(ctx context.Context, command ConfigurationDraftApplyCommand, draft ConfigurationDraft) (ConfigurationDraftApplyResult, error) {
	_, _, scopes, err := s.configuration.authorize(ctx, command.Requester)
	if err != nil {
		return ConfigurationDraftApplyResult{}, err
	}
	receipt, err := s.configuration.store.GetAuthorizedOperationReceipt(ctx, draft.ResultOperationID, scopes)
	if err != nil {
		return ConfigurationDraftApplyResult{}, serviceError(ErrorConflict, "configuration draft replay evidence is unavailable", nil)
	}
	generations, err := s.configuration.store.ListConfigurationGenerations(ctx)
	if err != nil {
		return ConfigurationDraftApplyResult{}, serviceError(ErrorInternal, "configuration generation evidence is unavailable", nil)
	}
	generation, found := generationByID(generations, draft.ResultGenerationID)
	if !found || !draft.ResultNoOp && !generationProvenanceMatchesDraft(generation, draft) {
		return ConfigurationDraftApplyResult{}, serviceError(ErrorConflict, "configuration draft replay evidence conflicts", nil)
	}
	projection, err := s.configuration.Projection(ctx, command.Requester, s.now().UTC())
	if err != nil {
		return ConfigurationDraftApplyResult{}, err
	}
	return ConfigurationDraftApplyResult{Apply: ConfigurationApplyResult{Generation: generation, Receipt: receipt, NoOp: draft.ResultNoOp}, Convergence: projection}, nil
}

func draftApplyProvenance(draft ConfigurationDraft) ConfigurationApplyProvenance {
	if draft.DraftOrigin == ConfigurationDraftOriginRollback {
		return ConfigurationApplyProvenance{Kind: ConfigurationApplyRollback, RollbackSourceGenerationID: draft.RollbackSourceGenerationID, RollbackSourceDigest: draft.RollbackSourceDigest}
	}
	return ConfigurationApplyProvenance{Kind: ConfigurationApplyNormal}
}

func generationProvenanceMatchesDraft(generation ConfigurationGeneration, draft ConfigurationDraft) bool {
	if draft.DraftOrigin == ConfigurationDraftOriginRollback {
		return generation.RollbackSourceGenerationID == draft.RollbackSourceGenerationID && generation.RollbackSourceDigest == draft.RollbackSourceDigest
	}
	return generation.RollbackSourceGenerationID == 0 && generation.RollbackSourceDigest == ""
}

func ConfigurationSettingsDigest(settings ConfigurationEditableSettings) string {
	raw, _ := json.Marshal(settings)
	return configurationDigest("configuration-settings-v1", string(raw))
}

func applyConfigurationEdit(settings ConfigurationEditableSettings, edit ConfigurationEdit) (ConfigurationEditableSettings, error) {
	if !slices.Contains(configurationFields, edit.Field) {
		return settings, errors.New("unknown configuration field")
	}
	values := 0
	if edit.Boolean != nil {
		values++
	}
	if edit.Duration != nil {
		values++
	}
	if edit.Integer != nil {
		values++
	}
	if values != 1 {
		return settings, errors.New("exactly one typed value is required")
	}
	switch edit.Field {
	case ConfigurationFieldAdmissionEnabled:
		if edit.Boolean == nil {
			return settings, errors.New("boolean value is required")
		}
		settings.Admission.Enabled = *edit.Boolean
	case ConfigurationFieldRunTimeout:
		if edit.Duration == nil || edit.Duration.Duration() <= 0 || edit.Duration.Duration() > 2*time.Hour {
			return settings, errors.New("duration is out of bounds")
		}
		settings.RunTimeout = *edit.Duration
	case ConfigurationFieldAdmissionPollInterval:
		if edit.Duration == nil || edit.Duration.Duration() < time.Minute || edit.Duration.Duration() > time.Hour {
			return settings, errors.New("duration is out of bounds")
		}
		settings.Admission.PollInterval = *edit.Duration
	case ConfigurationFieldDeliveryPollInterval:
		if edit.Duration == nil || edit.Duration.Duration() < 30*time.Second || edit.Duration.Duration() > 5*time.Minute {
			return settings, errors.New("duration is out of bounds")
		}
		settings.Admission.DeliveryPollInterval = *edit.Duration
	case ConfigurationFieldSchedulerLeaseTTL:
		if edit.Duration == nil || edit.Duration.Duration() < 30*time.Second || edit.Duration.Duration() > 10*time.Minute {
			return settings, errors.New("duration is out of bounds")
		}
		settings.Admission.SchedulerLeaseTTL = *edit.Duration
	case ConfigurationFieldSchedulerLeaseRenewalInterval:
		if edit.Duration == nil || edit.Duration.Duration() < 5*time.Second || edit.Duration.Duration() > 5*time.Minute {
			return settings, errors.New("duration is out of bounds")
		}
		settings.Admission.SchedulerLeaseRenewalInterval = *edit.Duration
	case ConfigurationFieldAdmissionMaxCandidates:
		if edit.Integer == nil || *edit.Integer < 1 || *edit.Integer > 100 {
			return settings, errors.New("integer is out of bounds")
		}
		settings.Admission.MaxCandidates = *edit.Integer
	case ConfigurationFieldAdmissionMaxPages:
		if edit.Integer == nil || *edit.Integer < 1 || *edit.Integer > 20 {
			return settings, errors.New("integer is out of bounds")
		}
		settings.Admission.MaxPages = *edit.Integer
	case ConfigurationFieldAdmissionHeavyCapacity:
		if edit.Integer == nil || *edit.Integer < 1 || *edit.Integer > MaxHeavyCapacity {
			return settings, errors.New("integer is out of bounds")
		}
		settings.Admission.HeavyCapacity = *edit.Integer
	}
	return settings, nil
}

func configurationSettingsFindings(settings ConfigurationEditableSettings) []ConfigurationValidationFinding {
	var findings []ConfigurationValidationFinding
	if settings.RunTimeout.Duration() <= 0 || settings.RunTimeout.Duration() > 2*time.Hour {
		findings = appendFinding(findings, ConfigurationFieldRunTimeout, ConfigurationValidationOutOfBounds)
	}
	if !settings.Admission.Enabled {
		return findings
	}
	checks := []struct {
		field   ConfigurationFieldID
		invalid bool
	}{
		{ConfigurationFieldAdmissionPollInterval, settings.Admission.PollInterval.Duration() < time.Minute || settings.Admission.PollInterval.Duration() > time.Hour},
		{ConfigurationFieldDeliveryPollInterval, settings.Admission.DeliveryPollInterval.Duration() < 30*time.Second || settings.Admission.DeliveryPollInterval.Duration() > 5*time.Minute},
		{ConfigurationFieldSchedulerLeaseTTL, settings.Admission.SchedulerLeaseTTL.Duration() < 30*time.Second || settings.Admission.SchedulerLeaseTTL.Duration() > 10*time.Minute},
		{ConfigurationFieldSchedulerLeaseRenewalInterval, settings.Admission.SchedulerLeaseRenewalInterval.Duration() < 5*time.Second || settings.Admission.SchedulerLeaseRenewalInterval.Duration() > 5*time.Minute},
		{ConfigurationFieldAdmissionMaxCandidates, settings.Admission.MaxCandidates < 1 || settings.Admission.MaxCandidates > 100},
		{ConfigurationFieldAdmissionMaxPages, settings.Admission.MaxPages < 1 || settings.Admission.MaxPages > 20},
		{ConfigurationFieldAdmissionHeavyCapacity, settings.Admission.HeavyCapacity < 1 || settings.Admission.HeavyCapacity > MaxHeavyCapacity},
	}
	for _, check := range checks {
		if check.invalid {
			findings = appendFinding(findings, check.field, ConfigurationValidationOutOfBounds)
		}
	}
	if settings.Admission.SchedulerLeaseTTL.Duration() > 0 && settings.Admission.SchedulerLeaseRenewalInterval.Duration() > settings.Admission.SchedulerLeaseTTL.Duration()/2 {
		findings = appendFinding(findings, ConfigurationFieldSchedulerLeaseRenewalInterval, ConfigurationValidationLeaseConflict)
	}
	return findings
}

func appendFinding(findings []ConfigurationValidationFinding, field ConfigurationFieldID, reason ConfigurationValidationReason) []ConfigurationValidationFinding {
	for _, finding := range findings {
		if finding.Field == field && finding.Reason == reason {
			return findings
		}
	}
	return append(findings, ConfigurationValidationFinding{Field: field, Reason: reason, Severity: ConfigurationValidationError})
}

func validationField(draft ConfigurationDraft) ConfigurationFieldID {
	if slices.Contains(configurationFields, draft.LastEditField) {
		return draft.LastEditField
	}
	return ConfigurationFieldRunTimeout
}

func configurationEditDigest(edit ConfigurationEdit) string {
	value := ""
	if edit.Boolean != nil {
		value = strconv.FormatBool(*edit.Boolean)
	}
	if edit.Duration != nil {
		value = edit.Duration.String()
	}
	if edit.Integer != nil {
		value = strconv.Itoa(*edit.Integer)
	}
	return configurationDigest("configuration-edit-v1", string(edit.Field), value)
}

func configurationSemanticChanges(before, after ConfigurationEditableSettings) []ConfigurationPreviewChange {
	var changes []ConfigurationPreviewChange
	if before.RunTimeout != after.RunTimeout {
		changes = append(changes, durationChange(ConfigurationPreviewRunTimeoutChanged, ConfigurationFieldRunTimeout, before.RunTimeout, after.RunTimeout))
	}
	if before.Admission.Enabled != after.Admission.Enabled {
		category := ConfigurationPreviewAutomaticAdmissionEnabled
		if !after.Admission.Enabled {
			category = ConfigurationPreviewAutomaticAdmissionDisabled
		}
		changes = append(changes, boolChange(category, ConfigurationFieldAdmissionEnabled, before.Admission.Enabled, after.Admission.Enabled))
	}
	if before.Admission.PollInterval != after.Admission.PollInterval {
		changes = append(changes, durationChange(ConfigurationPreviewAdmissionPollIntervalChanged, ConfigurationFieldAdmissionPollInterval, before.Admission.PollInterval, after.Admission.PollInterval))
	}
	if before.Admission.DeliveryPollInterval != after.Admission.DeliveryPollInterval {
		changes = append(changes, durationChange(ConfigurationPreviewDeliveryPollIntervalChanged, ConfigurationFieldDeliveryPollInterval, before.Admission.DeliveryPollInterval, after.Admission.DeliveryPollInterval))
	}
	var lease []ConfigurationPreviewFieldChange
	if before.Admission.SchedulerLeaseTTL != after.Admission.SchedulerLeaseTTL {
		lease = append(lease, durationFieldChange(ConfigurationFieldSchedulerLeaseTTL, before.Admission.SchedulerLeaseTTL, after.Admission.SchedulerLeaseTTL))
	}
	if before.Admission.SchedulerLeaseRenewalInterval != after.Admission.SchedulerLeaseRenewalInterval {
		lease = append(lease, durationFieldChange(ConfigurationFieldSchedulerLeaseRenewalInterval, before.Admission.SchedulerLeaseRenewalInterval, after.Admission.SchedulerLeaseRenewalInterval))
	}
	if len(lease) > 0 {
		changes = append(changes, ConfigurationPreviewChange{Category: ConfigurationPreviewSchedulerLeasePolicyChanged, Fields: lease})
	}
	var bounds []ConfigurationPreviewFieldChange
	if before.Admission.MaxCandidates != after.Admission.MaxCandidates {
		bounds = append(bounds, intFieldChange(ConfigurationFieldAdmissionMaxCandidates, before.Admission.MaxCandidates, after.Admission.MaxCandidates))
	}
	if before.Admission.MaxPages != after.Admission.MaxPages {
		bounds = append(bounds, intFieldChange(ConfigurationFieldAdmissionMaxPages, before.Admission.MaxPages, after.Admission.MaxPages))
	}
	if len(bounds) > 0 {
		changes = append(changes, ConfigurationPreviewChange{Category: ConfigurationPreviewCandidateScanBoundsChanged, Fields: bounds})
	}
	if before.Admission.HeavyCapacity != after.Admission.HeavyCapacity {
		category := ConfigurationPreviewHeavyCapacityIncreased
		if after.Admission.HeavyCapacity < before.Admission.HeavyCapacity {
			category = ConfigurationPreviewHeavyCapacityDecreased
		}
		changes = append(changes, ConfigurationPreviewChange{Category: category, Fields: []ConfigurationPreviewFieldChange{intFieldChange(ConfigurationFieldAdmissionHeavyCapacity, before.Admission.HeavyCapacity, after.Admission.HeavyCapacity)}})
	}
	if changes == nil {
		return []ConfigurationPreviewChange{}
	}
	return changes
}

func configurationPreviewImpacts(changes []ConfigurationPreviewChange) []ConfigurationPreviewImpact {
	if len(changes) == 0 {
		return []ConfigurationPreviewImpact{}
	}
	impacts := []ConfigurationPreviewImpact{ConfigurationImpactWorkerReloadRequired, ConfigurationImpactNewAdmissionFencedUntilConverged, ConfigurationImpactActiveRunsContinueUnderFrozenAuthority}
	for _, change := range changes {
		if change.Category == ConfigurationPreviewHeavyCapacityDecreased {
			impacts = append(impacts, ConfigurationImpactCapacityReductionUsesDrainSemantics)
			break
		}
	}
	return impacts
}

func boolChange(category ConfigurationPreviewCategory, field ConfigurationFieldID, before, after bool) ConfigurationPreviewChange {
	b, a := before, after
	return ConfigurationPreviewChange{Category: category, Fields: []ConfigurationPreviewFieldChange{{Field: field, Before: ConfigurationPreviewValue{Boolean: &b}, After: ConfigurationPreviewValue{Boolean: &a}}}}
}

func durationChange(category ConfigurationPreviewCategory, field ConfigurationFieldID, before, after ConfigurationDuration) ConfigurationPreviewChange {
	return ConfigurationPreviewChange{Category: category, Fields: []ConfigurationPreviewFieldChange{durationFieldChange(field, before, after)}}
}

func durationFieldChange(field ConfigurationFieldID, before, after ConfigurationDuration) ConfigurationPreviewFieldChange {
	b, a := before, after
	return ConfigurationPreviewFieldChange{Field: field, Before: ConfigurationPreviewValue{Duration: &b}, After: ConfigurationPreviewValue{Duration: &a}}
}

func intFieldChange(field ConfigurationFieldID, before, after int) ConfigurationPreviewFieldChange {
	b, a := before, after
	return ConfigurationPreviewFieldChange{Field: field, Before: ConfigurationPreviewValue{Integer: &b}, After: ConfigurationPreviewValue{Integer: &a}}
}

func newConfigurationDraftID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return "configuration-draft-" + hex.EncodeToString(value), nil
}

func digestBytes(value []byte) string {
	if len(value) == 0 {
		return ""
	}
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
