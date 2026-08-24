package application

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	RepositoryRemovalDraftOpen      = "open"
	RepositoryRemovalDraftApplying  = "applying"
	RepositoryRemovalDraftApplied   = "applied"
	RepositoryRemovalDraftDiscarded = "discarded"
	RepositoryRemovalDraftAmbiguous = "ambiguous"
	RepositoryRemovalPending        = "removal_pending_convergence"
	RepositoryRemovalRetired        = "retired"
)

type RepositoryRemovalGuardResult struct {
	Guard      string `json:"guard"`
	Allowed    bool   `json:"allowed"`
	ReasonCode string `json:"reason_code"`
	NextAction string `json:"next_action"`
}

type RepositoryRemovalValidation struct {
	DraftID          string                         `json:"draft_id"`
	Revision         int64                          `json:"revision"`
	CandidateDigest  string                         `json:"candidate_digest"`
	ValidationDigest string                         `json:"validation_digest"`
	Valid            bool                           `json:"valid"`
	Guards           []RepositoryRemovalGuardResult `json:"guards"`
	ValidatedAt      time.Time                      `json:"validated_at"`
}

type RepositoryRemovalPreview struct {
	DraftID                       string    `json:"draft_id"`
	Revision                      int64     `json:"revision"`
	Repository                    string    `json:"repository"`
	IncarnationID                 string    `json:"incarnation_id"`
	ProfileID                     string    `json:"profile_id"`
	RepositoryCountBefore         int       `json:"repository_count_before"`
	RepositoryCountAfter          int       `json:"repository_count_after"`
	BaseGenerationID              int64     `json:"base_generation_id"`
	BaseDigest                    string    `json:"base_digest"`
	ProposedConfigurationDigest   string    `json:"proposed_configuration_digest"`
	LifecycleVersion              int64     `json:"lifecycle_version"`
	ConfigurationAuthorityVersion int64     `json:"configuration_authority_version"`
	WorkerRestartRequired         bool      `json:"worker_restart_required"`
	ExpectedState                 string    `json:"expected_state"`
	PreservedResources            []string  `json:"preserved_resources"`
	PreviewDigest                 string    `json:"preview_digest"`
	PreviewedAt                   time.Time `json:"previewed_at"`
}

type RepositoryRemovalDraft struct {
	DraftID                       string                       `json:"draft_id"`
	Revision                      int64                        `json:"revision"`
	State                         string                       `json:"state"`
	Repository                    string                       `json:"repository"`
	IncarnationID                 string                       `json:"incarnation_id"`
	ProfileID                     string                       `json:"profile_id"`
	ProfileDigest                 string                       `json:"profile_digest"`
	RepositoryBindingDigest       string                       `json:"repository_binding_digest"`
	LifecycleVersion              int64                        `json:"lifecycle_version"`
	BaseGenerationID              int64                        `json:"base_generation_id"`
	BaseDigest                    string                       `json:"base_digest"`
	ConfigurationAuthorityVersion int64                        `json:"configuration_authority_version"`
	RepositoryCountBefore         int                          `json:"repository_count_before"`
	Validation                    *RepositoryRemovalValidation `json:"validation,omitempty"`
	Preview                       *RepositoryRemovalPreview    `json:"preview,omitempty"`
	RemovalOperationID            string                       `json:"removal_operation_id,omitempty"`
	ConfigurationOperationID      string                       `json:"configuration_operation_id,omitempty"`
	ResultGenerationID            int64                        `json:"result_generation_id,omitempty"`
	ResultDigest                  string                       `json:"result_digest,omitempty"`
	Receipt                       *OperationReceipt            `json:"receipt,omitempty"`
	CreatedAt                     time.Time                    `json:"created_at"`
	UpdatedAt                     time.Time                    `json:"updated_at"`
	SettledAt                     time.Time                    `json:"settled_at,omitempty"`
	ReasonCode                    string                       `json:"reason_code,omitempty"`
}

type RepositoryRemovalOpenInput struct {
	DraftID         string
	Authority       RepositoryOperationAuthority
	Profile         LocalRepository
	RepositoryCount int
	Requester       ConfiguredRequester
	OpenedAt        time.Time
}

type RepositoryRemovalMetadataInput struct {
	DraftID          string
	ExpectedRevision int64
	Validation       RepositoryRemovalValidation
	Preview          *RepositoryRemovalPreview
	UpdatedAt        time.Time
}

type RepositoryRemovalAcceptance struct {
	DraftID          string
	ExpectedRevision int64
	Expected         RepositoryOperationAuthority
	CandidateDigest  string
	PreviewDigest    string
	Receipt          OperationReceipt
	AcceptedAt       time.Time
}

type RepositoryRemovalApplied struct {
	DraftID                  string
	RemovalOperationID       string
	ConfigurationOperationID string
	GenerationID             int64
	Digest                   string
	AppliedAt                time.Time
}

type RepositoryRemovalStore interface {
	OpenRepositoryRemovalDraft(context.Context, RepositoryRemovalOpenInput) (RepositoryRemovalDraft, bool, error)
	RepositoryRemovalDraft(context.Context, string) (RepositoryRemovalDraft, bool, error)
	RecordRepositoryRemovalMetadata(context.Context, RepositoryRemovalMetadataInput) (RepositoryRemovalDraft, error)
	DiscardRepositoryRemovalDraft(context.Context, string, int64, time.Time) (RepositoryRemovalDraft, bool, error)
	EvaluateRepositoryRemovalGuards(context.Context, RepositoryOperationAuthority, int, time.Time) ([]RepositoryRemovalGuardResult, error)
	AcceptRepositoryRemoval(context.Context, RepositoryRemovalAcceptance) (RepositoryRemovalDraft, OperationReceipt, bool, error)
	RecordRepositoryRemovalApplied(context.Context, RepositoryRemovalApplied) (RepositoryRemovalDraft, OperationReceipt, bool, error)
}

type RepositoryRemovalDocument interface {
	ConfigurationDraftDocument
	MaterializeRepositoryRemoval([]byte, LocalRepository) ([]byte, int, int, error)
	ValidateRepositoryRemovalCandidate([]byte, []byte, LocalRepository) (ValidatedConfigurationCandidate, error)
}

type RepositoryRemovalService struct {
	configuration *ConfigurationService
	repositories  *RepositoryService
	store         RepositoryRemovalStore
	document      RepositoryRemovalDocument
	now           func() time.Time
}

func NewRepositoryRemovalService(configuration *ConfigurationService, repositories *RepositoryService, store RepositoryRemovalStore, document RepositoryRemovalDocument) (*RepositoryRemovalService, error) {
	if configuration == nil || repositories == nil || store == nil || document == nil {
		return nil, errors.New("repository removal dependencies are required")
	}
	return &RepositoryRemovalService{configuration: configuration, repositories: repositories, store: store, document: document, now: func() time.Time { return time.Now().UTC() }}, nil
}

type RepositoryRemovalOpenCommand struct {
	Requester  Requester
	Repository string
}

type RepositoryRemovalCommand struct {
	Requester Requester
	DraftID   string
	Revision  int64
}

type RepositoryRemovalApplyCommand struct {
	Requester               Requester
	DraftID                 string
	Revision                int64
	PreviewDigest           string
	IncarnationID           string
	LifecycleVersion        int64
	ProfileID               string
	RepositoryBindingDigest string
	ExpectedGenerationID    int64
	ExpectedDigest          string
}

type RepositoryRemovalApplyResult struct {
	Draft       RepositoryRemovalDraft             `json:"draft"`
	Convergence ConfigurationConvergenceProjection `json:"convergence"`
}

func (s *RepositoryRemovalService) Open(ctx context.Context, command RepositoryRemovalOpenCommand) (RepositoryRemovalDraft, error) {
	profile, _, err := s.repositories.authorizedRepository(ctx, command.Requester, command.Repository)
	if err != nil {
		return RepositoryRemovalDraft{}, err
	}
	authority, configured, _, err := s.configuration.authorize(ctx, command.Requester)
	if err != nil {
		return RepositoryRemovalDraft{}, err
	}
	if authority.Incomplete != nil || authority.IncompleteRecovery != nil {
		return RepositoryRemovalDraft{}, serviceError(ErrorConflict, "configuration mutation authority is not idle", nil)
	}
	projection, err := s.configuration.Projection(ctx, command.Requester, s.now().UTC())
	if err != nil || projection.State != ConfigurationReady {
		return RepositoryRemovalDraft{}, serviceError(ErrorConflict, "configuration is not exactly converged", nil)
	}
	repositoryAuthority, err := s.storeAuthority(ctx, profile.Authority.Repository)
	if err != nil || repositoryAuthority.Lifecycle.Intent != RepositoryDisabled || repositoryAuthority.Removal != nil || repositoryAuthority.Recheck != nil {
		return RepositoryRemovalDraft{}, serviceError(ErrorConflict, "repository is not eligible for a removal draft", nil)
	}
	if !profileMatchesRemovalAuthority(profile.Profile, repositoryAuthority.Lifecycle) {
		return RepositoryRemovalDraft{}, serviceError(ErrorConflict, "repository profile authority changed", nil)
	}
	profiles, err := s.repositories.profiles.ListRepositoryProfiles(ctx)
	if err != nil {
		return RepositoryRemovalDraft{}, classifyServiceError(err)
	}
	draftID, err := newRepositoryRemovalDraftID()
	if err != nil {
		return RepositoryRemovalDraft{}, serviceError(ErrorInternal, "repository removal draft identity is unavailable", nil)
	}
	draft, _, err := s.store.OpenRepositoryRemovalDraft(ctx, RepositoryRemovalOpenInput{DraftID: draftID, Authority: repositoryAuthority, Profile: profile.Profile, RepositoryCount: len(profiles), Requester: configured, OpenedAt: s.now().UTC()})
	if err != nil {
		return RepositoryRemovalDraft{}, serviceError(ErrorConflict, "repository removal draft conflicts", nil)
	}
	return draft, nil
}

func (s *RepositoryRemovalService) storeAuthority(ctx context.Context, repository string) (RepositoryOperationAuthority, error) {
	store, ok := s.repositories.store.(interface {
		RepositoryOperationAuthority(context.Context, string) (RepositoryOperationAuthority, error)
	})
	if !ok {
		return RepositoryOperationAuthority{}, errors.New("repository removal authority is unavailable")
	}
	return store.RepositoryOperationAuthority(ctx, repository)
}

func (s *RepositoryRemovalService) Show(ctx context.Context, command RepositoryRemovalCommand) (RepositoryRemovalDraft, error) {
	if _, _, _, err := s.configuration.authorize(ctx, command.Requester); err != nil {
		return RepositoryRemovalDraft{}, err
	}
	// A read is also the safe restart reconciliation point: the configuration
	// service can publish a fresh worker observation, and the SQLite adapter
	// atomically retires any exact matching pending incarnation.
	_, _ = s.configuration.Projection(ctx, command.Requester, s.now().UTC())
	draft, found, err := s.store.RepositoryRemovalDraft(ctx, command.DraftID)
	if err != nil || !found {
		return RepositoryRemovalDraft{}, hiddenTargetError()
	}
	if command.Revision > 0 && draft.Revision != command.Revision {
		return RepositoryRemovalDraft{}, serviceError(ErrorConflict, "repository removal draft revision changed", nil)
	}
	return draft, nil
}

func (s *RepositoryRemovalService) Validate(ctx context.Context, command RepositoryRemovalCommand) (RepositoryRemovalValidation, error) {
	draft, base, candidate, _, _, authority, err := s.materialize(ctx, command)
	if err != nil {
		return RepositoryRemovalValidation{}, err
	}
	guards, err := s.removalGuards(ctx, command.Requester, draft, base, authority)
	if err != nil {
		return RepositoryRemovalValidation{}, classifyRepositoryError(err)
	}
	valid := true
	for _, guard := range guards {
		valid = valid && guard.Allowed
	}
	result := RepositoryRemovalValidation{DraftID: draft.DraftID, Revision: draft.Revision, CandidateDigest: digestBytes(candidate), Valid: valid, Guards: guards, ValidatedAt: s.now().UTC()}
	encoded, _ := json.Marshal(struct {
		DraftID         string
		Revision        int64
		CandidateDigest string
		Guards          []RepositoryRemovalGuardResult
	}{result.DraftID, result.Revision, result.CandidateDigest, result.Guards})
	result.ValidationDigest = digestText("repository-removal-validation-v1\x00" + string(encoded))
	stored, err := s.store.RecordRepositoryRemovalMetadata(ctx, RepositoryRemovalMetadataInput{DraftID: draft.DraftID, ExpectedRevision: draft.Revision, Validation: result, UpdatedAt: result.ValidatedAt})
	if err != nil || stored.Validation == nil {
		return RepositoryRemovalValidation{}, serviceError(ErrorConflict, "repository removal draft revision changed", nil)
	}
	return *stored.Validation, nil
}

func (s *RepositoryRemovalService) Preview(ctx context.Context, command RepositoryRemovalCommand) (RepositoryRemovalPreview, error) {
	draft, _, candidate, before, after, authority, err := s.materialize(ctx, command)
	if err != nil {
		return RepositoryRemovalPreview{}, err
	}
	validation, err := s.Validate(ctx, command)
	if err != nil {
		return RepositoryRemovalPreview{}, err
	}
	if !validation.Valid {
		return RepositoryRemovalPreview{}, serviceError(ErrorInvalidInput, "repository removal guards are not satisfied", nil)
	}
	if draft.Preview != nil && draft.Preview.ProposedConfigurationDigest == validation.CandidateDigest {
		return *draft.Preview, nil
	}
	preview := RepositoryRemovalPreview{DraftID: draft.DraftID, Revision: draft.Revision, Repository: draft.Repository, IncarnationID: draft.IncarnationID, ProfileID: draft.ProfileID, RepositoryCountBefore: before, RepositoryCountAfter: after, BaseGenerationID: draft.BaseGenerationID, BaseDigest: draft.BaseDigest, ProposedConfigurationDigest: digestBytes(candidate), LifecycleVersion: draft.LifecycleVersion, ConfigurationAuthorityVersion: authority.Version, WorkerRestartRequired: true, ExpectedState: RepositoryRemovalPending, PreservedResources: []string{"local_checkouts_and_managed_directories", "github_repositories_branches_pull_requests_and_app_profiles", "linear_labels_and_issues", "credentials_and_credential_references", "artifacts_historical_runs_receipts_and_audit_evidence"}, PreviewedAt: s.now().UTC()}
	encoded, _ := json.Marshal(preview)
	preview.PreviewDigest = digestText("repository-removal-preview-v1\x00" + string(encoded))
	stored, err := s.store.RecordRepositoryRemovalMetadata(ctx, RepositoryRemovalMetadataInput{DraftID: draft.DraftID, ExpectedRevision: draft.Revision, Validation: validation, Preview: &preview, UpdatedAt: preview.PreviewedAt})
	if err != nil || stored.Preview == nil {
		return RepositoryRemovalPreview{}, serviceError(ErrorConflict, "repository removal draft revision changed", nil)
	}
	return *stored.Preview, nil
}

func (s *RepositoryRemovalService) Apply(ctx context.Context, command RepositoryRemovalApplyCommand) (RepositoryRemovalApplyResult, error) {
	draft, base, candidate, _, _, authority, err := s.materialize(ctx, RepositoryRemovalCommand{Requester: command.Requester, DraftID: command.DraftID, Revision: command.Revision})
	if err != nil {
		return RepositoryRemovalApplyResult{}, err
	}
	if draft.Preview == nil || draft.Preview.PreviewDigest != command.PreviewDigest || draft.IncarnationID != command.IncarnationID || draft.LifecycleVersion != command.LifecycleVersion || draft.ProfileID != command.ProfileID || draft.RepositoryBindingDigest != command.RepositoryBindingDigest || draft.BaseGenerationID != command.ExpectedGenerationID || draft.BaseDigest != command.ExpectedDigest || !strings.EqualFold(digestBytes(base), draft.BaseDigest) {
		return RepositoryRemovalApplyResult{}, serviceError(ErrorConflict, "repository removal apply authority changed", nil)
	}
	if draft.State == RepositoryRemovalDraftApplied {
		projection, projectionErr := s.configuration.Projection(ctx, command.Requester, s.now().UTC())
		return RepositoryRemovalApplyResult{Draft: draft, Convergence: projection}, projectionErr
	}
	expected := removalExpectedAuthority(draft)
	replayingAppliedConfiguration := draft.State == RepositoryRemovalDraftApplying && authority.Desired.Digest == digestBytes(candidate)
	if !replayingAppliedConfiguration {
		guards, guardErr := s.removalGuards(ctx, command.Requester, draft, base, authority)
		if guardErr != nil || !allRemovalGuardsAllowed(guards) {
			return RepositoryRemovalApplyResult{}, serviceError(ErrorConflict, "repository removal guards changed", nil)
		}
	}
	configured, scopes, err := s.removalRequester(ctx, command.Requester)
	if err != nil {
		return RepositoryRemovalApplyResult{}, err
	}
	target, _ := scopes.ControllerOperationTarget()
	requestDigest := digestText("repository-removal-request-v1\x00" + draft.DraftID + "\x00" + command.PreviewDigest)
	receipt := NewOperationReceipt(OperationReceiptInput{OperationType: OperationRemoveRepository, Scope: ScopeController, TargetID: target.TargetID, Requester: configured.Identity(), RequestDigest: requestDigest, ExpectedAuthorityDigest: repositoryRemovalAuthorityDigest(draft), OperationAnchorDigest: digestText("repository-removal-v1\x00" + draft.IncarnationID + "\x00" + command.PreviewDigest), TargetBindingDigest: target.TargetBindingDigest, AcceptedAt: s.now().UTC()})
	draft, receipt, _, err = s.store.AcceptRepositoryRemoval(ctx, RepositoryRemovalAcceptance{DraftID: draft.DraftID, ExpectedRevision: draft.Revision, Expected: expected, CandidateDigest: digestBytes(candidate), PreviewDigest: command.PreviewDigest, Receipt: receipt, AcceptedAt: receipt.AcceptedAt})
	if err != nil {
		return RepositoryRemovalApplyResult{}, serviceError(ErrorConflict, "repository removal acceptance conflicted", nil)
	}
	result, err := s.configuration.Apply(ctx, ConfigurationApplyCommand{Requester: command.Requester, ExpectedGenerationID: command.ExpectedGenerationID, ExpectedDigest: command.ExpectedDigest, Payload: candidate, Provenance: ConfigurationApplyProvenance{Kind: ConfigurationApplyNormal}})
	if err != nil {
		return RepositoryRemovalApplyResult{}, err
	}
	draft, _, _, err = s.store.RecordRepositoryRemovalApplied(ctx, RepositoryRemovalApplied{DraftID: draft.DraftID, RemovalOperationID: receipt.OperationID, ConfigurationOperationID: result.Receipt.OperationID, GenerationID: result.Generation.GenerationID, Digest: result.Generation.Digest, AppliedAt: s.now().UTC()})
	if err != nil {
		return RepositoryRemovalApplyResult{}, serviceError(ErrorConflict, "repository removal settlement requires replay", nil)
	}
	projection, err := s.configuration.Projection(ctx, command.Requester, s.now().UTC())
	return RepositoryRemovalApplyResult{Draft: draft, Convergence: projection}, err
}

func (s *RepositoryRemovalService) Discard(ctx context.Context, command RepositoryRemovalCommand) (RepositoryRemovalDraft, error) {
	if _, _, _, err := s.configuration.authorize(ctx, command.Requester); err != nil {
		return RepositoryRemovalDraft{}, err
	}
	draft, _, err := s.store.DiscardRepositoryRemovalDraft(ctx, command.DraftID, command.Revision, s.now().UTC())
	if err != nil {
		return RepositoryRemovalDraft{}, serviceError(ErrorConflict, "repository removal draft cannot be discarded", nil)
	}
	return draft, nil
}

func (s *RepositoryRemovalService) materialize(ctx context.Context, command RepositoryRemovalCommand) (RepositoryRemovalDraft, []byte, []byte, int, int, ConfigurationAuthority, error) {
	authority, _, _, err := s.configuration.authorize(ctx, command.Requester)
	if err != nil {
		return RepositoryRemovalDraft{}, nil, nil, 0, 0, ConfigurationAuthority{}, err
	}
	draft, found, err := s.store.RepositoryRemovalDraft(ctx, command.DraftID)
	if err != nil || !found {
		return RepositoryRemovalDraft{}, nil, nil, 0, 0, ConfigurationAuthority{}, hiddenTargetError()
	}
	if draft.Revision != command.Revision || draft.State != RepositoryRemovalDraftOpen && draft.State != RepositoryRemovalDraftApplying && draft.State != RepositoryRemovalDraftApplied || authority.Desired.GenerationID != draft.BaseGenerationID && draft.State == RepositoryRemovalDraftOpen || authority.Desired.Digest != draft.BaseDigest && draft.State == RepositoryRemovalDraftOpen {
		return RepositoryRemovalDraft{}, nil, nil, 0, 0, ConfigurationAuthority{}, serviceError(ErrorConflict, "repository removal draft authority changed", nil)
	}
	generations, err := s.configuration.store.ListConfigurationGenerations(ctx)
	if err != nil {
		return RepositoryRemovalDraft{}, nil, nil, 0, 0, ConfigurationAuthority{}, serviceError(ErrorInternal, "configuration generation evidence is unavailable", nil)
	}
	baseGeneration, found := generationByID(generations, draft.BaseGenerationID)
	if !found || baseGeneration.Digest != draft.BaseDigest {
		return RepositoryRemovalDraft{}, nil, nil, 0, 0, ConfigurationAuthority{}, serviceError(ErrorConflict, "repository removal base evidence conflicts", nil)
	}
	base, err := s.configuration.files.ReadRaw(draft.BaseDigest, baseGeneration.Size)
	if err != nil {
		return RepositoryRemovalDraft{}, nil, nil, 0, 0, ConfigurationAuthority{}, serviceError(ErrorConflict, "repository removal base evidence is unavailable", nil)
	}
	profile := LocalRepository{ProfileID: draft.ProfileID, ProfileDigest: draft.ProfileDigest, RepositoryBindingDigest: draft.RepositoryBindingDigest, CanonicalRepository: draft.Repository}
	candidate, before, after, err := s.document.MaterializeRepositoryRemoval(base, profile)
	if err != nil {
		return RepositoryRemovalDraft{}, nil, nil, 0, 0, ConfigurationAuthority{}, serviceError(ErrorConflict, "repository removal candidate cannot be materialized", nil)
	}
	if _, err := s.document.ValidateRepositoryRemovalCandidate(base, candidate, profile); err != nil {
		return RepositoryRemovalDraft{}, nil, nil, 0, 0, ConfigurationAuthority{}, serviceError(ErrorInvalidInput, "repository removal candidate is invalid", nil)
	}
	return draft, base, candidate, before, after, authority, nil
}

func (s *RepositoryRemovalService) removalRequester(ctx context.Context, requester Requester) (ConfiguredRequester, AuthorizedScopeSet, error) {
	_, configured, scopes, err := s.configuration.authorize(ctx, requester)
	return configured, scopes, err
}

func (s *RepositoryRemovalService) removalGuards(ctx context.Context, requester Requester, draft RepositoryRemovalDraft, base []byte, authority ConfigurationAuthority) ([]RepositoryRemovalGuardResult, error) {
	guards, err := s.store.EvaluateRepositoryRemovalGuards(ctx, removalExpectedAuthority(draft), draft.RepositoryCountBefore, s.now().UTC())
	if err != nil {
		return nil, err
	}
	projection, projectionErr := s.configuration.Projection(ctx, requester, s.now().UTC())
	ready := projectionErr == nil && projection.State == ConfigurationReady && projection.DesiredGenerationID == draft.BaseGenerationID && projection.DesiredDigest == draft.BaseDigest && projection.EffectiveGenerationID == draft.BaseGenerationID && projection.LoadedConfigurationDigest == draft.BaseDigest
	guards = append(guards, removalGuard("live_configuration_converged", ready, "live_configuration_not_converged", "wait_for_exact_worker_convergence"))
	settings, settingsErr := s.document.ProjectEditable(base)
	admissionAllowed := settingsErr == nil && (draft.RepositoryCountBefore != 1 || !settings.Admission.Enabled)
	guards = append(guards, removalGuard("final_repository_admission_disabled", admissionAllowed, "automatic_admission_must_be_disabled", "disable_automatic_admission_through_config_draft"))
	return guards, nil
}

func removalGuard(name string, allowed bool, reason, next string) RepositoryRemovalGuardResult {
	if allowed {
		return RepositoryRemovalGuardResult{Guard: name, Allowed: true, ReasonCode: "clear", NextAction: "none"}
	}
	return RepositoryRemovalGuardResult{Guard: name, Allowed: false, ReasonCode: reason, NextAction: next}
}

func removalExpectedAuthority(draft RepositoryRemovalDraft) RepositoryOperationAuthority {
	result := RepositoryOperationAuthority{Lifecycle: RepositoryLifecycle{IncarnationID: draft.IncarnationID, Repository: draft.Repository, ProfileID: draft.ProfileID, ProfileDigest: draft.ProfileDigest, RepositoryBindingDigest: draft.RepositoryBindingDigest, Intent: RepositoryDisabled, Version: draft.LifecycleVersion, UpdatedAt: draft.CreatedAt}, ConfigurationAuthority: ConfigurationAdmissionAuthority{GenerationID: draft.BaseGenerationID, Digest: draft.BaseDigest, AuthorityVersion: draft.ConfigurationAuthorityVersion, ValidThrough: time.Now().UTC().Add(time.Minute)}}
	if draft.RemovalOperationID != "" {
		result.Removal = &RepositoryRemovalProjection{OperationID: draft.RemovalOperationID, State: RepositoryRemovalPending}
	}
	return result
}

func profileMatchesRemovalAuthority(profile LocalRepository, lifecycle RepositoryLifecycle) bool {
	return profile.CanonicalRepository == lifecycle.Repository && profile.ProfileID == lifecycle.ProfileID && profile.ProfileDigest == lifecycle.ProfileDigest && profile.RepositoryBindingDigest == lifecycle.RepositoryBindingDigest
}

func allRemovalGuardsAllowed(guards []RepositoryRemovalGuardResult) bool {
	if len(guards) == 0 {
		return false
	}
	for _, guard := range guards {
		if !guard.Allowed {
			return false
		}
	}
	return true
}

func repositoryRemovalAuthorityDigest(draft RepositoryRemovalDraft) string {
	payload, _ := json.Marshal(struct {
		IncarnationID, ProfileID, ProfileDigest, BindingDigest, BaseDigest string
		LifecycleVersion, BaseGenerationID, ConfigurationAuthorityVersion  int64
	}{draft.IncarnationID, draft.ProfileID, draft.ProfileDigest, draft.RepositoryBindingDigest, draft.BaseDigest, draft.LifecycleVersion, draft.BaseGenerationID, draft.ConfigurationAuthorityVersion})
	return digestText("repository-removal-authority-v1\x00" + string(payload))
}

func newRepositoryRemovalDraftID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "repository-removal-draft-" + hex.EncodeToString(value[:]), nil
}
