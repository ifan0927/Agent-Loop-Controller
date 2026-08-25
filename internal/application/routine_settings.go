package application

import (
	"context"
	"errors"
	"sort"
	"time"
)

type RoutineConfigurationLifecycleSummary struct {
	GenerationID int64               `json:"generation_id"`
	OperationID  string              `json:"operation_id,omitempty"`
	State        string              `json:"state"`
	ReasonCode   ConfigurationReason `json:"reason_code,omitempty"`
	ObservedAt   time.Time           `json:"observed_at"`
}

type RoutineConfigurationDraftSummary struct {
	DraftID          string                  `json:"draft_id"`
	Revision         int64                   `json:"revision"`
	State            ConfigurationDraftState `json:"state"`
	BaseGenerationID int64                   `json:"base_generation_id"`
	UpdatedAt        time.Time               `json:"updated_at"`
}

type RoutineCredentialReadiness struct {
	Kind       string     `json:"kind"`
	State      string     `json:"state"`
	ReasonCode string     `json:"reason_code"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
}

type RoutineCredentialReadinessStore interface {
	PersistedCredentialReadiness(context.Context, AuthorizedScopeSet) ([]RoutineCredentialReadiness, error)
}

type RoutineSettingsProjection struct {
	Metadata              RoutineProjectionMetadata             `json:"metadata"`
	DesiredGenerationID   int64                                 `json:"desired_generation_id"`
	DesiredDigest         string                                `json:"desired_digest"`
	EffectiveGenerationID int64                                 `json:"effective_generation_id,omitempty"`
	LoadedDigest          string                                `json:"loaded_configuration_digest,omitempty"`
	Convergence           ConfigurationConvergenceProjection    `json:"convergence"`
	Settings              ConfigurationEditableSettings         `json:"settings"`
	ActiveDraft           *RoutineConfigurationDraftSummary     `json:"active_draft,omitempty"`
	IncompleteApply       *RoutineConfigurationLifecycleSummary `json:"incomplete_apply,omitempty"`
	IncompleteRecovery    *RoutineConfigurationLifecycleSummary `json:"incomplete_recovery,omitempty"`
	RepositoryCount       int                                   `json:"repository_count"`
	ProfileCount          int                                   `json:"profile_count"`
	Credentials           []RoutineCredentialReadiness          `json:"credential_readiness"`
}

type RoutineSettingsService struct {
	configuration *ConfigurationService
	drafts        ConfigurationDraftStore
	document      ConfigurationDraftDocument
	credentials   RoutineCredentialReadinessStore
}

func NewRoutineSettingsService(configuration *ConfigurationService, drafts ConfigurationDraftStore, document ConfigurationDraftDocument, credentials RoutineCredentialReadinessStore) (*RoutineSettingsService, error) {
	if configuration == nil || drafts == nil || document == nil {
		return nil, errors.New("routine settings dependencies are required")
	}
	return &RoutineSettingsService{configuration: configuration, drafts: drafts, document: document, credentials: credentials}, nil
}

func (s *RoutineSettingsService) Get(ctx context.Context, requester Requester, observedAt time.Time) (RoutineSettingsProjection, error) {
	authority, _, scopes, err := s.configuration.authorize(ctx, requester)
	if err != nil {
		return RoutineSettingsProjection{}, err
	}
	raw, err := s.configuration.files.ReadRaw(authority.Desired.Digest, authority.Desired.Size)
	if err != nil {
		return RoutineSettingsProjection{}, serviceError(ErrorConflict, "desired configuration evidence is unavailable", nil)
	}
	candidate, err := s.configuration.files.ValidateCurrent(raw)
	if err != nil || candidate.Digest != authority.Desired.Digest || candidate.Size != authority.Desired.Size || candidate.SchemaVersion != authority.Desired.SchemaVersion {
		return RoutineSettingsProjection{}, serviceError(ErrorConflict, "desired configuration evidence conflicts", nil)
	}
	settings, err := s.document.ProjectEditable(raw)
	if err != nil {
		return RoutineSettingsProjection{}, serviceError(ErrorConflict, "desired settings evidence is unavailable", nil)
	}
	convergence, err := s.configuration.ProjectionReadOnly(ctx, requester, observedAt)
	if err != nil {
		return RoutineSettingsProjection{}, err
	}
	if convergence.DesiredGenerationID != authority.Desired.GenerationID || convergence.DesiredDigest != authority.Desired.Digest || convergence.EffectiveGenerationID != authority.EffectiveID {
		return RoutineSettingsProjection{}, serviceError(ErrorConflict, "configuration authority changed during the query", nil)
	}
	result := RoutineSettingsProjection{Metadata: RoutineProjectionMetadata{SchemaVersion: RoutineQuerySchemaVersion, ObservedAt: observedAt.UTC()}, DesiredGenerationID: authority.Desired.GenerationID, DesiredDigest: authority.Desired.Digest, EffectiveGenerationID: authority.EffectiveID, LoadedDigest: convergence.LoadedConfigurationDigest, Convergence: convergence, Settings: settings, RepositoryCount: len(candidate.Repositories), Credentials: []RoutineCredentialReadiness{}}
	profiles := map[string]struct{}{}
	for _, repository := range candidate.Repositories {
		profiles[repository.GitHubAppProfileRef] = struct{}{}
	}
	result.ProfileCount = len(profiles)
	if draft, found, draftErr := s.drafts.ActiveConfigurationDraft(ctx); draftErr != nil {
		return RoutineSettingsProjection{}, classifyServiceError(draftErr)
	} else if found {
		result.ActiveDraft = &RoutineConfigurationDraftSummary{DraftID: draft.DraftID, Revision: draft.Revision, State: draft.State, BaseGenerationID: draft.BaseGenerationID, UpdatedAt: draft.UpdatedAt.UTC()}
	}
	if authority.Incomplete != nil {
		result.IncompleteApply = &RoutineConfigurationLifecycleSummary{GenerationID: authority.Incomplete.GenerationID, OperationID: authority.Incomplete.OperationID, State: string(authority.Incomplete.State), ReasonCode: authority.Incomplete.Reason, ObservedAt: authority.Incomplete.AcceptedAt.UTC()}
	}
	if authority.IncompleteRecovery != nil {
		result.IncompleteRecovery = &RoutineConfigurationLifecycleSummary{GenerationID: authority.IncompleteRecovery.DesiredGenerationID, OperationID: authority.IncompleteRecovery.OperationID, State: string(authority.IncompleteRecovery.State), ReasonCode: authority.IncompleteRecovery.Reason, ObservedAt: authority.IncompleteRecovery.AcceptedAt.UTC()}
	}
	if s.credentials != nil {
		result.Credentials, err = s.credentials.PersistedCredentialReadiness(ctx, scopes)
		if err != nil {
			return RoutineSettingsProjection{}, classifyServiceError(err)
		}
		if len(result.Credentials) > RoutineQueryMaximumLimit {
			return RoutineSettingsProjection{}, serviceError(ErrorInternal, "credential readiness exceeds its bound", nil)
		}
		for _, readiness := range result.Credentials {
			if !validRoutineCredentialReadiness(readiness) {
				return RoutineSettingsProjection{}, serviceError(ErrorConflict, "credential readiness evidence is invalid", nil)
			}
		}
	}
	if len(result.Credentials) == 0 {
		result.Credentials = []RoutineCredentialReadiness{{Kind: "github", State: "unknown", ReasonCode: "observation_missing"}, {Kind: "linear", State: "unknown", ReasonCode: "observation_missing"}}
	}
	sort.Slice(result.Credentials, func(i, j int) bool { return result.Credentials[i].Kind < result.Credentials[j].Kind })
	result.Metadata.Digest = routineProjectionDigest(result)
	return result, nil
}

func validRoutineCredentialReadiness(readiness RoutineCredentialReadiness) bool {
	if readiness.Kind != "github" && readiness.Kind != "linear" {
		return false
	}
	switch readiness.State {
	case "ready", "unknown", "unavailable", "conflict":
	default:
		return false
	}
	return operatorAttentionScope.MatchString(readiness.ReasonCode)
}

// projectPersisted composes Settings from authority already captured by an
// enclosing SQLite read snapshot. It performs only private, hash-bound file
// reads and pure classification; it never re-reads or mutates SQLite.
func (s *RoutineSettingsService) projectPersisted(authority ConfigurationAuthority, draft *ConfigurationDraft, runtime RuntimeObservation, observedAt time.Time) (RoutineSettingsProjection, error) {
	raw, err := s.configuration.files.ReadRaw(authority.Desired.Digest, authority.Desired.Size)
	if err != nil {
		return RoutineSettingsProjection{}, serviceError(ErrorConflict, "desired configuration evidence is unavailable", nil)
	}
	candidate, err := s.configuration.files.ValidateCurrent(raw)
	if err != nil || candidate.Digest != authority.Desired.Digest || candidate.Size != authority.Desired.Size || candidate.SchemaVersion != authority.Desired.SchemaVersion {
		return RoutineSettingsProjection{}, serviceError(ErrorConflict, "desired configuration evidence conflicts", nil)
	}
	settings, err := s.document.ProjectEditable(raw)
	if err != nil {
		return RoutineSettingsProjection{}, serviceError(ErrorConflict, "desired settings evidence is unavailable", nil)
	}
	result := RoutineSettingsProjection{Metadata: RoutineProjectionMetadata{SchemaVersion: RoutineQuerySchemaVersion, ObservedAt: observedAt.UTC()}, DesiredGenerationID: authority.Desired.GenerationID, DesiredDigest: authority.Desired.Digest, EffectiveGenerationID: authority.EffectiveID, Convergence: s.configuration.project(authority, runtime), Settings: settings, RepositoryCount: len(candidate.Repositories), Credentials: []RoutineCredentialReadiness{{Kind: "github", State: "unknown", ReasonCode: "observation_missing"}, {Kind: "linear", State: "unknown", ReasonCode: "observation_missing"}}}
	result.LoadedDigest = result.Convergence.LoadedConfigurationDigest
	profiles := map[string]struct{}{}
	for _, repository := range candidate.Repositories {
		profiles[repository.GitHubAppProfileRef] = struct{}{}
	}
	result.ProfileCount = len(profiles)
	if draft != nil {
		result.ActiveDraft = &RoutineConfigurationDraftSummary{DraftID: draft.DraftID, Revision: draft.Revision, State: draft.State, BaseGenerationID: draft.BaseGenerationID, UpdatedAt: draft.UpdatedAt.UTC()}
	}
	if authority.Incomplete != nil {
		result.IncompleteApply = &RoutineConfigurationLifecycleSummary{GenerationID: authority.Incomplete.GenerationID, OperationID: authority.Incomplete.OperationID, State: string(authority.Incomplete.State), ReasonCode: authority.Incomplete.Reason, ObservedAt: authority.Incomplete.AcceptedAt.UTC()}
	}
	if authority.IncompleteRecovery != nil {
		result.IncompleteRecovery = &RoutineConfigurationLifecycleSummary{GenerationID: authority.IncompleteRecovery.DesiredGenerationID, OperationID: authority.IncompleteRecovery.OperationID, State: string(authority.IncompleteRecovery.State), ReasonCode: authority.IncompleteRecovery.Reason, ObservedAt: authority.IncompleteRecovery.AcceptedAt.UTC()}
	}
	result.Metadata.Digest = routineProjectionDigest(result)
	return result, nil
}
