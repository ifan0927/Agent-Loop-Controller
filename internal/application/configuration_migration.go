package application

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
)

const ConfigurationMigrationTargetSchemaVersion = 5

type ConfigurationMigrationPreservation struct {
	ControllerAuthorityPreserved bool `json:"controller_authority_preserved"`
	LinearAuthorityPreserved     bool `json:"linear_authority_preserved"`
	GitHubProfilesPreserved      bool `json:"github_profiles_preserved"`
	RepositoryProfilesPreserved  bool `json:"repository_profiles_preserved"`
	RepositoryBindingsPreserved  bool `json:"repository_bindings_preserved"`
	AutomationPolicyPreserved    bool `json:"automation_policy_preserved"`
}

func (p ConfigurationMigrationPreservation) valid() bool {
	return p.ControllerAuthorityPreserved && p.LinearAuthorityPreserved && p.GitHubProfilesPreserved && p.RepositoryProfilesPreserved && p.RepositoryBindingsPreserved && p.AutomationPolicyPreserved
}

type ConfigurationMigrationMaterialization struct {
	Payload      []byte
	Candidate    ValidatedConfigurationCandidate
	SourceSchema int
	Preservation ConfigurationMigrationPreservation
}

type ConfigurationMigrationDocument interface {
	MaterializeLegacyMigration([]byte) (ConfigurationMigrationMaterialization, error)
}

type ConfigurationMigrationGuard interface {
	ConfigurationMigrationBlocked(context.Context) (bool, error)
}

type ConfigurationMigrationPreview struct {
	SourceSchemaVersion      int                                `json:"source_schema_version"`
	TargetSchemaVersion      int                                `json:"target_schema_version"`
	ExpectedGenerationID     int64                              `json:"expected_generation_id"`
	ExpectedDigest           string                             `json:"expected_digest"`
	ExpectedAuthorityVersion int64                              `json:"expected_authority_version"`
	CandidateDigest          string                             `json:"candidate_digest"`
	MigrationDigest          string                             `json:"migration_digest"`
	PreviewDigest            string                             `json:"preview_digest"`
	RestartRequired          bool                               `json:"restart_required"`
	Preservation             ConfigurationMigrationPreservation `json:"semantic_preservation"`
	PreviewedAt              time.Time                          `json:"previewed_at"`
}

type ConfigurationMigrationApplyCommand struct {
	Requester                Requester
	RequestID                string
	ExpectedGenerationID     int64
	ExpectedDigest           string
	ExpectedAuthorityVersion int64
	SourceSchemaVersion      int
	TargetSchemaVersion      int
	CandidateDigest          string
	MigrationDigest          string
	PreviewDigest            string
}

type ConfigurationMigrationApplyResult struct {
	Migration   ConfigurationMigrationPreview      `json:"migration"`
	Apply       ConfigurationApplyResult           `json:"apply"`
	Convergence ConfigurationConvergenceProjection `json:"convergence"`
}

type ConfigurationMigrationService struct {
	configuration *ConfigurationService
	document      ConfigurationMigrationDocument
	now           func() time.Time
}

func NewConfigurationMigrationService(configuration *ConfigurationService, document ConfigurationMigrationDocument) (*ConfigurationMigrationService, error) {
	if configuration == nil || document == nil {
		return nil, errors.New("configuration migration dependencies are required")
	}
	return &ConfigurationMigrationService{configuration: configuration, document: document, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (s *ConfigurationMigrationService) Preview(ctx context.Context, requester Requester) (ConfigurationMigrationPreview, error) {
	authority, _, _, err := s.configuration.authorize(ctx, requester)
	if err != nil {
		return ConfigurationMigrationPreview{}, err
	}
	if authority.Desired.SchemaVersion == ConfigurationMigrationTargetSchemaVersion {
		return ConfigurationMigrationPreview{}, serviceError(ErrorConflict, "configuration already uses the current schema", nil)
	}
	if authority.Desired.SchemaVersion < 2 || authority.Desired.SchemaVersion > 4 {
		return ConfigurationMigrationPreview{}, serviceError(ErrorConflict, "configuration schema is not eligible for inline migration", nil)
	}
	if err := s.requireIdleMutationLane(ctx); err != nil {
		return ConfigurationMigrationPreview{}, err
	}
	decision, err := s.configuration.CheckNewAdmissionReadOnly(ctx)
	if err != nil || !decision.Allowed || decision.Authority.GenerationID != authority.Desired.GenerationID || decision.Authority.Digest != authority.Desired.Digest || decision.Authority.AuthorityVersion != authority.Version {
		return ConfigurationMigrationPreview{}, serviceError(ErrorConflict, "configuration migration requires exact desired, effective, and live convergence", nil)
	}
	return s.materializePreview(authority, s.now().UTC())
}

func (s *ConfigurationMigrationService) Apply(ctx context.Context, command ConfigurationMigrationApplyCommand) (ConfigurationMigrationApplyResult, error) {
	if !validConfigurationMigrationRequestID(command.RequestID) || command.ExpectedGenerationID < 1 || command.ExpectedAuthorityVersion < 1 || !validAuthorityDigest(command.ExpectedDigest) || command.SourceSchemaVersion < 2 || command.SourceSchemaVersion > 4 || command.TargetSchemaVersion != ConfigurationMigrationTargetSchemaVersion || !validAuthorityDigest(command.CandidateDigest) || !validAuthorityDigest(command.MigrationDigest) || !validAuthorityDigest(command.PreviewDigest) {
		return ConfigurationMigrationApplyResult{}, serviceError(ErrorInvalidInput, "complete configuration migration authority is required", nil)
	}
	if replay, found, err := s.exactReplay(ctx, command); found || err != nil {
		return replay, err
	}
	if _, _, _, err := s.configuration.authorize(ctx, command.Requester); err != nil {
		return ConfigurationMigrationApplyResult{}, err
	}
	generations, err := s.configuration.store.ListConfigurationGenerations(ctx)
	if err != nil {
		return ConfigurationMigrationApplyResult{}, serviceError(ErrorInternal, "configuration migration source is unavailable", nil)
	}
	source, found := generationByID(generations, command.ExpectedGenerationID)
	if !found || source.Digest != command.ExpectedDigest || source.SchemaVersion != command.SourceSchemaVersion || !source.RawRetained {
		return ConfigurationMigrationApplyResult{}, serviceError(ErrorConflict, "configuration migration source authority changed", nil)
	}
	payload, err := s.configuration.files.ReadRaw(source.Digest, source.Size)
	if err != nil {
		return ConfigurationMigrationApplyResult{}, serviceError(ErrorConflict, "configuration migration source evidence is unavailable", nil)
	}
	materialized, err := s.document.MaterializeLegacyMigration(payload)
	if err != nil || materialized.SourceSchema != command.SourceSchemaVersion || materialized.Candidate.Digest != command.CandidateDigest || !materialized.Preservation.valid() {
		return ConfigurationMigrationApplyResult{}, serviceError(ErrorConflict, "configuration migration candidate conflicts", nil)
	}
	preview := configurationMigrationPreview(command.ExpectedGenerationID, command.ExpectedDigest, command.ExpectedAuthorityVersion, materialized, time.Time{})
	if preview.MigrationDigest != command.MigrationDigest || preview.PreviewDigest != command.PreviewDigest {
		return ConfigurationMigrationApplyResult{}, serviceError(ErrorConflict, "configuration migration preview changed", nil)
	}
	authority, present, err := s.configuration.store.ConfigurationAuthority(ctx)
	if err != nil || !present {
		return ConfigurationMigrationApplyResult{}, serviceError(ErrorConflict, "configuration migration authority is unavailable", nil)
	}
	if authority.Desired.GenerationID == command.ExpectedGenerationID {
		if authority.Desired.Digest != command.ExpectedDigest || authority.Desired.SchemaVersion != command.SourceSchemaVersion || authority.Version != command.ExpectedAuthorityVersion {
			return ConfigurationMigrationApplyResult{}, serviceError(ErrorConflict, "configuration migration authority changed", nil)
		}
		if err := s.requireIdleMutationLane(ctx); err != nil {
			return ConfigurationMigrationApplyResult{}, err
		}
		decision, decisionErr := s.configuration.CheckNewAdmissionReadOnly(ctx)
		if decisionErr != nil || !decision.Allowed || decision.Authority.GenerationID != command.ExpectedGenerationID || decision.Authority.Digest != command.ExpectedDigest || decision.Authority.AuthorityVersion != command.ExpectedAuthorityVersion {
			return ConfigurationMigrationApplyResult{}, serviceError(ErrorConflict, "configuration migration requires exact desired, effective, and live convergence", nil)
		}
	}
	apply, err := s.configuration.Apply(ctx, configurationMigrationApplyCommand(command, materialized.Payload))
	if err != nil {
		return ConfigurationMigrationApplyResult{}, err
	}
	return s.result(ctx, command.Requester, preview, apply, true)
}

func (s *ConfigurationMigrationService) exactReplay(ctx context.Context, command ConfigurationMigrationApplyCommand) (ConfigurationMigrationApplyResult, bool, error) {
	preview := configurationMigrationReplayPreview(command, s.now().UTC())
	if preview.MigrationDigest != command.MigrationDigest || preview.PreviewDigest != command.PreviewDigest {
		return ConfigurationMigrationApplyResult{}, true, serviceError(ErrorConflict, "configuration migration preview changed", nil)
	}
	applyCommand := configurationMigrationApplyCommand(command, nil)
	replay, found := s.configuration.configurationReplay(ctx, applyCommand, ValidatedConfigurationCandidate{Digest: command.CandidateDigest})
	if !found {
		return ConfigurationMigrationApplyResult{}, false, nil
	}
	generations, err := s.configuration.store.ListConfigurationGenerations(ctx)
	if err != nil {
		return ConfigurationMigrationApplyResult{}, true, serviceError(ErrorInternal, "configuration migration replay evidence is unavailable", nil)
	}
	source, sourceFound := generationByID(generations, command.ExpectedGenerationID)
	if !sourceFound || source.Digest != command.ExpectedDigest || source.SchemaVersion != command.SourceSchemaVersion || replay.Generation.ParentID != source.GenerationID || replay.Generation.Digest != command.CandidateDigest || replay.Generation.SchemaVersion != ConfigurationMigrationTargetSchemaVersion || replay.Generation.Origin != ConfigurationOriginApply || replay.Generation.ApplyKind != ConfigurationApplySchemaMigration {
		return ConfigurationMigrationApplyResult{}, true, serviceError(ErrorConflict, "configuration migration replay evidence conflicts", nil)
	}
	if replay.Receipt.Phase == OperationPhaseAccepted {
		if !replay.Generation.RawRetained {
			return ConfigurationMigrationApplyResult{}, true, serviceError(ErrorConflict, "configuration migration target evidence is unavailable", nil)
		}
		payload, readErr := s.configuration.files.ReadRaw(replay.Generation.Digest, replay.Generation.Size)
		if readErr != nil {
			return ConfigurationMigrationApplyResult{}, true, serviceError(ErrorConflict, "configuration migration target evidence is unavailable", nil)
		}
		applyCommand.Payload = payload
		replay, err = s.configuration.Apply(ctx, applyCommand)
		if err != nil {
			return ConfigurationMigrationApplyResult{}, true, err
		}
	}
	result, resultErr := s.result(ctx, command.Requester, preview, replay, false)
	return result, true, resultErr
}

func (s *ConfigurationMigrationService) result(ctx context.Context, requester Requester, preview ConfigurationMigrationPreview, apply ConfigurationApplyResult, requireProjection bool) (ConfigurationMigrationApplyResult, error) {
	convergence, err := s.configuration.ProjectionReadOnly(ctx, requester, s.now().UTC())
	if err != nil && requireProjection {
		return ConfigurationMigrationApplyResult{}, err
	}
	if err != nil {
		convergence = ConfigurationConvergenceProjection{State: ConfigurationUnknown, Reason: ConfigurationReasonRuntimeUnknown, NextAction: ConfigurationActionInspectRuntime}
	}
	preview.PreviewedAt = s.now().UTC()
	return ConfigurationMigrationApplyResult{Migration: preview, Apply: apply, Convergence: convergence}, nil
}

func configurationMigrationApplyCommand(command ConfigurationMigrationApplyCommand, payload []byte) ConfigurationApplyCommand {
	return ConfigurationApplyCommand{
		Requester: command.Requester, ExpectedGenerationID: command.ExpectedGenerationID, ExpectedDigest: command.ExpectedDigest,
		ExpectedAuthorityVersion: command.ExpectedAuthorityVersion, Payload: payload,
		Provenance: ConfigurationApplyProvenance{Kind: ConfigurationApplySchemaMigration, MigrationRequestID: command.RequestID, MigrationPreviewDigest: command.PreviewDigest},
	}
}

func configurationMigrationReplayPreview(command ConfigurationMigrationApplyCommand, at time.Time) ConfigurationMigrationPreview {
	materialized := ConfigurationMigrationMaterialization{SourceSchema: command.SourceSchemaVersion, Candidate: ValidatedConfigurationCandidate{Digest: command.CandidateDigest}, Preservation: completeConfigurationMigrationPreservation()}
	return configurationMigrationPreview(command.ExpectedGenerationID, command.ExpectedDigest, command.ExpectedAuthorityVersion, materialized, at)
}

func completeConfigurationMigrationPreservation() ConfigurationMigrationPreservation {
	return ConfigurationMigrationPreservation{ControllerAuthorityPreserved: true, LinearAuthorityPreserved: true, GitHubProfilesPreserved: true, RepositoryProfilesPreserved: true, RepositoryBindingsPreserved: true, AutomationPolicyPreserved: true}
}

func (s *ConfigurationMigrationService) requireIdleMutationLane(ctx context.Context) error {
	guard, ok := s.configuration.store.(ConfigurationMigrationGuard)
	if !ok {
		return serviceError(ErrorInternal, "configuration migration mutation authority is unavailable", nil)
	}
	blocked, err := guard.ConfigurationMigrationBlocked(ctx)
	if err != nil {
		return serviceError(ErrorInternal, "configuration migration mutation authority is unavailable", nil)
	}
	if blocked {
		return serviceError(ErrorConflict, "configuration migration requires an idle mutation lane", nil)
	}
	return nil
}

func (s *ConfigurationMigrationService) materializePreview(authority ConfigurationAuthority, at time.Time) (ConfigurationMigrationPreview, error) {
	payload, err := s.configuration.files.ReadRaw(authority.Desired.Digest, authority.Desired.Size)
	if err != nil {
		return ConfigurationMigrationPreview{}, serviceError(ErrorConflict, "configuration migration source evidence is unavailable", nil)
	}
	materialized, err := s.document.MaterializeLegacyMigration(payload)
	if err != nil || materialized.SourceSchema != authority.Desired.SchemaVersion || !materialized.Preservation.valid() {
		return ConfigurationMigrationPreview{}, serviceError(ErrorConflict, "configuration migration candidate cannot be materialized", nil)
	}
	return configurationMigrationPreview(authority.Desired.GenerationID, authority.Desired.Digest, authority.Version, materialized, at), nil
}

func configurationMigrationPreview(generationID int64, digest string, authorityVersion int64, materialized ConfigurationMigrationMaterialization, at time.Time) ConfigurationMigrationPreview {
	boolText := func(value bool) string { return strconv.FormatBool(value) }
	p := materialized.Preservation
	migrationDigest := configurationDigest("configuration-schema-migration-v1", strconv.Itoa(materialized.SourceSchema), strconv.Itoa(ConfigurationMigrationTargetSchemaVersion), digest, materialized.Candidate.Digest,
		boolText(p.ControllerAuthorityPreserved), boolText(p.LinearAuthorityPreserved), boolText(p.GitHubProfilesPreserved), boolText(p.RepositoryProfilesPreserved), boolText(p.RepositoryBindingsPreserved), boolText(p.AutomationPolicyPreserved))
	previewDigest := configurationDigest("configuration-schema-migration-preview-v1", strconv.FormatInt(generationID, 10), digest, strconv.FormatInt(authorityVersion, 10), migrationDigest)
	return ConfigurationMigrationPreview{SourceSchemaVersion: materialized.SourceSchema, TargetSchemaVersion: ConfigurationMigrationTargetSchemaVersion, ExpectedGenerationID: generationID, ExpectedDigest: digest, ExpectedAuthorityVersion: authorityVersion, CandidateDigest: materialized.Candidate.Digest, MigrationDigest: migrationDigest, PreviewDigest: previewDigest, RestartRequired: true, Preservation: p, PreviewedAt: at}
}

func validConfigurationMigrationRequestID(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) < 1 || len(value) > 128 || strings.ContainsRune(value, '\x00') {
		return false
	}
	for _, char := range value {
		if char < 0x21 || char > 0x7e {
			return false
		}
	}
	return true
}
