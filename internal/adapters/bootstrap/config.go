// Package bootstrap owns the controller's composition-root configuration.
package bootstrap

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/githubapp"
	linearadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/linear"
	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/localregistry"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

const (
	LegacyVersion  = 1
	VersionTwo     = 2
	VersionThree   = 3
	VersionFour    = 4
	CurrentVersion = 5

	minimumDeliveryPollInterval = 30 * time.Second
	maximumDeliveryPollInterval = 5 * time.Minute
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Error is safe to display to an operator. It deliberately excludes file
// paths, credential references, and underlying parser details.
type Error struct {
	Category string
	Message  string
}

func (e *Error) Error() string { return e.Category + ": " + e.Message }

func invalid(message string) error  { return &Error{Category: "invalid_config", Message: message} }
func missing(message string) error  { return &Error{Category: "missing_reference", Message: message} }
func conflict(message string) error { return &Error{Category: "identity_conflict", Message: message} }
func unsafe(message string) error   { return &Error{Category: "unsafe_path", Message: message} }

type Controller struct {
	DatabasePath string
	CodexBinary  string
	RunTimeout   time.Duration
	Operator     domain.GitHubUserIdentity
}

type GitHubProfile struct {
	ID     string
	Config githubapp.Config
	Digest string
}

type Bootstrap struct {
	Path                string
	Version             int
	Digest              string
	Controller          Controller
	Linear              linearadapter.Config
	Registry            localregistry.Registry
	GitHubProfiles      map[string]GitHubProfile
	RegistryPath        string
	CredentialDirectory string
	Automation          Automation
}

// Automation contains validated local worker authority and cadence. It does
// not create an admission source, delivery driver, or external operation.
type Automation struct {
	LinearTodoAdmission LinearTodoAdmission
}

type LinearTodoAdmission struct {
	Enabled               bool
	TeamID                string
	TeamKey               string
	TodoState             WorkflowState
	InProgressState       WorkflowState
	PollInterval          time.Duration
	DeliveryPollInterval  time.Duration
	SchedulerLeaseTTL     time.Duration
	SchedulerLeaseRenewal time.Duration
	MaxCandidates         int
	MaxPages              int
	// MaxActiveRuns is populated only when reading legacy version 3 input.
	MaxActiveRuns       int
	HeavyCapacity       int
	Requester           localregistry.TrustedActorIdentity
	NotificationMode    string
	CredentialSourceRef string
}

type WorkflowState struct {
	ID   string
	Name string
	Type string
}

// EditableSettings is the closed, secret-free projection used by the managed
// configuration draft adapter. It deliberately excludes every authority,
// identity, credential reference, executable, and path.
type EditableSettings struct {
	RunTimeout                    time.Duration
	AdmissionEnabled              bool
	AdmissionPollInterval         time.Duration
	DeliveryPollInterval          time.Duration
	SchedulerLeaseTTL             time.Duration
	SchedulerLeaseRenewalInterval time.Duration
	AdmissionMaxCandidates        int
	AdmissionMaxPages             int
	AdmissionHeavyCapacity        int
}

type readinessFile struct {
	Version             int                   `json:"version"`
	ConfigurationDigest string                `json:"configuration_digest"`
	Offline             bool                  `json:"offline"`
	Controller          readinessController   `json:"controller"`
	Linear              readinessLinear       `json:"linear"`
	Repositories        []readinessRepository `json:"repository_profiles"`
	GitHubProfiles      []readinessGitHub     `json:"github_app_profiles"`
	Automation          readinessAutomation   `json:"automation"`
}

type readinessController struct {
	DatabaseConfigured bool                `json:"database_configured"`
	CodexConfigured    bool                `json:"codex_configured"`
	Operator           *readinessRequester `json:"operator,omitempty"`
}

type readinessLinear struct {
	TeamKey              string `json:"team_key"`
	CredentialSourceType string `json:"credential_source_type"`
}

type readinessRepository struct {
	ProfileID     string `json:"profile_id"`
	ProfileDigest string `json:"profile_digest"`
	Repository    string `json:"repository"`
	GitHubProfile string `json:"github_app_profile"`
}

type readinessGitHub struct {
	ProfileID string `json:"profile_id"`
	Digest    string `json:"profile_digest"`
	AppID     int64  `json:"app_id"`
}

type readinessAutomation struct {
	LinearTodoAdmission readinessLinearTodoAdmission `json:"linear_todo_admission"`
}

type readinessLinearTodoAdmission struct {
	Enabled               bool                `json:"enabled"`
	PollInterval          string              `json:"poll_interval,omitempty"`
	DeliveryPollInterval  string              `json:"delivery_poll_interval,omitempty"`
	SchedulerLeaseTTL     string              `json:"scheduler_lease_ttl,omitempty"`
	SchedulerLeaseRenewal string              `json:"scheduler_lease_renewal_interval,omitempty"`
	MaxCandidates         int                 `json:"max_candidates,omitempty"`
	MaxPages              int                 `json:"max_pages,omitempty"`
	HeavyCapacity         int                 `json:"heavy_capacity,omitempty"`
	Requester             *readinessRequester `json:"requester,omitempty"`
}

type readinessRequester struct {
	DatabaseID int64  `json:"database_id"`
	NodeID     string `json:"node_id"`
	Login      string `json:"login"`
	Type       string `json:"type"`
}

// Readiness is an offline, credential-safe report. It never performs network
// I/O, reads environment variables, opens a database, or reads key contents.
func (b Bootstrap) Readiness() any {
	bindings := b.Registry.Bindings()
	repositories := make([]readinessRepository, 0, len(bindings))
	for _, binding := range bindings {
		repositories = append(repositories, readinessRepository{ProfileID: binding.ProfileID, ProfileDigest: binding.ProfileDigest, Repository: binding.CanonicalRepository, GitHubProfile: binding.GitHubAppProfileRef})
	}
	ids := make([]string, 0, len(b.GitHubProfiles))
	for id := range b.GitHubProfiles {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	profiles := make([]readinessGitHub, 0, len(ids))
	for _, id := range ids {
		profile := b.GitHubProfiles[id]
		profiles = append(profiles, readinessGitHub{ProfileID: profile.ID, Digest: profile.Digest, AppID: profile.Config.AppID})
	}
	admission := readinessLinearTodoAdmission{Enabled: b.Automation.LinearTodoAdmission.Enabled}
	if configured := b.Automation.LinearTodoAdmission; configured.Enabled {
		admission.PollInterval = configured.PollInterval.String()
		admission.DeliveryPollInterval = configured.DeliveryPollInterval.String()
		admission.SchedulerLeaseTTL = configured.SchedulerLeaseTTL.String()
		admission.SchedulerLeaseRenewal = configured.SchedulerLeaseRenewal.String()
		admission.MaxCandidates = configured.MaxCandidates
		admission.MaxPages = configured.MaxPages
		admission.HeavyCapacity = configured.HeavyCapacity
		admission.Requester = &readinessRequester{DatabaseID: configured.Requester.DatabaseID, NodeID: configured.Requester.NodeID, Login: configured.Requester.Login, Type: configured.Requester.Type}
	}
	controller := readinessController{DatabaseConfigured: b.Controller.DatabasePath != "", CodexConfigured: b.Controller.CodexBinary != ""}
	if b.Controller.Operator.Validate() == nil {
		controller.Operator = &readinessRequester{DatabaseID: b.Controller.Operator.DatabaseID, NodeID: b.Controller.Operator.NodeID, Login: b.Controller.Operator.Login, Type: b.Controller.Operator.ActorType}
	}
	return readinessFile{Version: b.Version, ConfigurationDigest: b.Digest, Offline: true,
		Controller: controller,
		Linear:     readinessLinear{TeamKey: b.Linear.TeamKey, CredentialSourceType: linearadapter.CredentialSourceType(b.Linear.CredentialSourceRef)}, Repositories: repositories, GitHubProfiles: profiles,
		Automation: readinessAutomation{LinearTodoAdmission: admission}}
}

// GitHubProfileForRepository returns the already cross-checked configuration.
func (b Bootstrap) GitHubProfileForRepository(repository string) (GitHubProfile, error) {
	binding, err := b.Registry.Resolve(repository)
	if err != nil {
		return GitHubProfile{}, missing("repository profile is not configured")
	}
	profile, ok := b.GitHubProfiles[binding.GitHubAppProfileRef]
	if !ok {
		return GitHubProfile{}, missing("GitHub App profile is not configured")
	}
	return profile, nil
}

type configFile struct {
	Version                int             `json:"version"`
	Controller             controllerFile  `json:"controller"`
	Linear                 json.RawMessage `json:"linear"`
	RepositoryRegistryFile json.RawMessage `json:"repository_registry_file"`
	Repositories           json.RawMessage `json:"repositories"`
	GitHubAppProfiles      []profileFile   `json:"github_app_profiles"`
	Automation             json.RawMessage `json:"automation"`
}

type automationFile struct {
	LinearTodoAdmission json.RawMessage `json:"linear_todo_admission"`
}

type linearTodoAdmissionFile struct {
	Enabled                       *bool             `json:"enabled"`
	TeamID                        string            `json:"team_id"`
	TeamKey                       string            `json:"team_key"`
	TodoState                     workflowStateFile `json:"todo_state"`
	InProgressState               workflowStateFile `json:"in_progress_state"`
	PollInterval                  string            `json:"poll_interval"`
	DeliveryPollInterval          json.RawMessage   `json:"delivery_poll_interval"`
	SchedulerLeaseTTL             string            `json:"scheduler_lease_ttl"`
	SchedulerLeaseRenewalInterval string            `json:"scheduler_lease_renewal_interval"`
	MaxCandidates                 int               `json:"max_candidates"`
	MaxPages                      int               `json:"max_pages"`
	MaxActiveRuns                 json.RawMessage   `json:"max_active_runs"`
	HeavyCapacity                 json.RawMessage   `json:"heavy_capacity"`
	Requester                     requesterFile     `json:"requester"`
	NotificationMode              string            `json:"notification_mode"`
	CredentialSourceRef           string            `json:"credential_source_ref"`
}

type workflowStateFile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type requesterFile struct {
	DatabaseID int64  `json:"database_id"`
	NodeID     string `json:"node_id"`
	Login      string `json:"login"`
	Type       string `json:"type"`
}

type controllerFile struct {
	DatabasePath string        `json:"database_path"`
	CodexBinary  string        `json:"codex_binary"`
	RunTimeout   string        `json:"run_timeout"`
	Operator     requesterFile `json:"operator"`
}

type profileFile struct {
	ID     string          `json:"id"`
	Config json.RawMessage `json:"config"`
}

// Load performs strict, offline composition validation. Credential files are
// inspected only as filesystem objects; their contents are never read.
func Load(path string) (Bootstrap, error) {
	data, canonicalPath, err := readRegularConfig(path)
	if err != nil {
		return Bootstrap{}, err
	}
	return ValidateBytes(canonicalPath, data)
}

// ValidateBytes applies the same strict, offline bootstrap contract to one
// already captured payload. The canonical path supplies only the local
// authority root used by path validation; the payload is never reread here.
// This seam lets configuration transactions prove that validation, digesting,
// and private retention all refer to the same exact bytes.
func ValidateBytes(canonicalPath string, data []byte) (Bootstrap, error) {
	if _, err := canonicalConfigLocation(canonicalPath); err != nil {
		return Bootstrap{}, err
	}
	if len(data) > 256<<10 {
		return Bootstrap{}, invalid("controller configuration is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var raw configFile
	if err := decoder.Decode(&raw); err != nil {
		return Bootstrap{}, invalid("controller configuration must contain one strict JSON value")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Bootstrap{}, invalid("controller configuration must contain one strict JSON value")
	}
	if raw.Version != LegacyVersion && raw.Version != VersionTwo && raw.Version != VersionThree && raw.Version != VersionFour && raw.Version != CurrentVersion {
		return Bootstrap{}, invalid("unsupported controller configuration version")
	}
	controller, err := decodeController(raw.Controller)
	if err != nil {
		return Bootstrap{}, err
	}
	linear, err := linearadapter.DecodeConfig(bytes.NewReader(raw.Linear))
	if err != nil {
		return Bootstrap{}, invalid("Linear profile is invalid")
	}
	registry, registryPath, err := decodeRegistry(raw)
	if err != nil {
		return Bootstrap{}, err
	}
	profiles, err := decodeProfiles(raw.GitHubAppProfiles)
	if err != nil {
		return Bootstrap{}, err
	}
	if err := crossCheck(registry, profiles, raw.Version); err != nil {
		return Bootstrap{}, err
	}
	operator, err := decodeConfiguredOperator(raw.Controller.Operator, raw.Version)
	if err != nil {
		return Bootstrap{}, err
	}
	if raw.Version != CurrentVersion {
		operator, err = deriveLegacyConfiguredOperator(registry)
		if err != nil {
			return Bootstrap{}, err
		}
	}
	controller.Operator = operator
	if raw.Version == CurrentVersion && operator.Validate() == nil && !operatorTrustedByEveryProfile(operator, registry) {
		return Bootstrap{}, conflict("configured operator is not trusted by every repository profile")
	}
	automation, err := decodeAutomation(raw, registry)
	if err != nil {
		return Bootstrap{}, err
	}
	if automation.LinearTodoAdmission.Enabled && linear.TeamKey != automation.LinearTodoAdmission.TeamKey {
		return Bootstrap{}, conflict("automatic admission team does not match the Linear profile")
	}
	digest := sha256.Sum256(data)
	return Bootstrap{Path: canonicalPath, Version: raw.Version, Digest: hex.EncodeToString(digest[:]), Controller: controller, Linear: linear, Registry: registry, GitHubProfiles: profiles, RegistryPath: registryPath, CredentialDirectory: filepath.Join(filepath.Dir(canonicalPath), "secrets"), Automation: automation}, nil
}

// ValidateCurrentBytes is the mutation boundary for a new configuration
// candidate. Historical supported schemas remain valid for one-time baseline
// adoption, but every later apply must use the current schema.
func ValidateCurrentBytes(canonicalPath string, data []byte) (Bootstrap, error) {
	loaded, err := ValidateBytes(canonicalPath, data)
	if err != nil {
		return Bootstrap{}, err
	}
	if loaded.Version != CurrentVersion {
		return Bootstrap{}, invalid("configuration apply requires the current schema version")
	}
	return loaded, nil
}

// ProjectEditableSettings validates the exact current-schema payload before
// projecting only the closed routine settings surface.
func ProjectEditableSettings(canonicalPath string, data []byte) (EditableSettings, error) {
	if _, err := ValidateCurrentBytes(canonicalPath, data); err != nil {
		return EditableSettings{}, err
	}
	raw, admission, err := decodeEditableDocument(data)
	if err != nil || raw.Version != CurrentVersion {
		return EditableSettings{}, invalid("configuration editable settings are invalid")
	}
	settings := EditableSettings{
		AdmissionEnabled: false, AdmissionPollInterval: 5 * time.Minute,
		DeliveryPollInterval: 30 * time.Second, SchedulerLeaseTTL: time.Minute,
		SchedulerLeaseRenewalInterval: 20 * time.Second, AdmissionMaxCandidates: 20,
		AdmissionMaxPages: 5, AdmissionHeavyCapacity: 2,
	}
	if admission.Enabled != nil {
		settings.AdmissionEnabled = *admission.Enabled
	}
	settings.RunTimeout, _ = time.ParseDuration(raw.Controller.RunTimeout)
	if admission.PollInterval != "" {
		settings.AdmissionPollInterval, _ = time.ParseDuration(admission.PollInterval)
	}
	if len(admission.DeliveryPollInterval) != 0 {
		var value string
		if json.Unmarshal(admission.DeliveryPollInterval, &value) == nil {
			settings.DeliveryPollInterval, _ = time.ParseDuration(value)
		}
	}
	if admission.SchedulerLeaseTTL != "" {
		settings.SchedulerLeaseTTL, _ = time.ParseDuration(admission.SchedulerLeaseTTL)
	}
	if admission.SchedulerLeaseRenewalInterval != "" {
		settings.SchedulerLeaseRenewalInterval, _ = time.ParseDuration(admission.SchedulerLeaseRenewalInterval)
	}
	if admission.MaxCandidates != 0 {
		settings.AdmissionMaxCandidates = admission.MaxCandidates
	}
	if admission.MaxPages != 0 {
		settings.AdmissionMaxPages = admission.MaxPages
	}
	if len(admission.HeavyCapacity) != 0 {
		_ = json.Unmarshal(admission.HeavyCapacity, &settings.AdmissionHeavyCapacity)
	}
	return settings, nil
}

// ProjectHistoricalEditableSettings strictly extracts only the nine managed
// scalar settings from a retained schema-version 1 through 5 document. It does
// not resolve legacy registries, inspect credential paths, or validate any
// external authority.
func ProjectHistoricalEditableSettings(data []byte, expectedVersion int) (EditableSettings, error) {
	if len(data) == 0 || len(data) > 256<<10 || expectedVersion < LegacyVersion || expectedVersion > CurrentVersion {
		return EditableSettings{}, invalid("historical configuration settings are invalid")
	}
	if rejectDuplicateJSONKeys(data) != nil {
		return EditableSettings{}, invalid("historical configuration settings are invalid")
	}
	raw, admission, err := decodeEditableDocument(data)
	if err != nil || raw.Version != expectedVersion {
		return EditableSettings{}, invalid("historical configuration settings are invalid")
	}
	settings := EditableSettings{
		AdmissionEnabled: false, AdmissionPollInterval: 5 * time.Minute,
		DeliveryPollInterval: minimumDeliveryPollInterval, SchedulerLeaseTTL: time.Minute,
		SchedulerLeaseRenewalInterval: 20 * time.Second, AdmissionMaxCandidates: 20,
		AdmissionMaxPages: 5, AdmissionHeavyCapacity: 2,
	}
	settings.RunTimeout, err = time.ParseDuration(raw.Controller.RunTimeout)
	if err != nil || settings.RunTimeout <= 0 || settings.RunTimeout > 2*time.Hour {
		return EditableSettings{}, invalid("historical configuration settings are invalid")
	}
	if expectedVersion == LegacyVersion || expectedVersion == VersionTwo {
		if len(raw.Automation) != 0 {
			return EditableSettings{}, invalid("historical configuration settings are invalid")
		}
		return settings, nil
	}
	if len(raw.Automation) == 0 {
		return settings, nil
	}
	var automation automationFile
	if decodeStrictObject(raw.Automation, &automation) != nil || len(automation.LinearTodoAdmission) == 0 || admission.Enabled == nil {
		return EditableSettings{}, invalid("historical configuration settings are invalid")
	}
	settings.AdmissionEnabled = *admission.Enabled
	if !settings.AdmissionEnabled {
		return settings, nil
	}
	if admission.PollInterval == "" || admission.SchedulerLeaseTTL == "" || admission.SchedulerLeaseRenewalInterval == "" || admission.MaxCandidates == 0 || admission.MaxPages == 0 {
		return EditableSettings{}, invalid("historical configuration settings are invalid")
	}
	if expectedVersion == VersionThree {
		settings.AdmissionHeavyCapacity = 1
	}
	if admission.PollInterval != "" {
		settings.AdmissionPollInterval, err = time.ParseDuration(admission.PollInterval)
		if err != nil || settings.AdmissionPollInterval < time.Minute || settings.AdmissionPollInterval > time.Hour {
			return EditableSettings{}, invalid("historical configuration settings are invalid")
		}
	}
	if len(admission.DeliveryPollInterval) != 0 {
		var value string
		if json.Unmarshal(admission.DeliveryPollInterval, &value) != nil {
			return EditableSettings{}, invalid("historical configuration settings are invalid")
		}
		settings.DeliveryPollInterval, err = time.ParseDuration(value)
		if err != nil || settings.DeliveryPollInterval < minimumDeliveryPollInterval || settings.DeliveryPollInterval > maximumDeliveryPollInterval {
			return EditableSettings{}, invalid("historical configuration settings are invalid")
		}
	}
	if admission.SchedulerLeaseTTL != "" {
		settings.SchedulerLeaseTTL, err = time.ParseDuration(admission.SchedulerLeaseTTL)
		if err != nil || settings.SchedulerLeaseTTL < 30*time.Second || settings.SchedulerLeaseTTL > 10*time.Minute {
			return EditableSettings{}, invalid("historical configuration settings are invalid")
		}
	}
	if admission.SchedulerLeaseRenewalInterval != "" {
		settings.SchedulerLeaseRenewalInterval, err = time.ParseDuration(admission.SchedulerLeaseRenewalInterval)
		if err != nil || settings.SchedulerLeaseRenewalInterval < 5*time.Second || settings.SchedulerLeaseRenewalInterval > settings.SchedulerLeaseTTL/2 {
			return EditableSettings{}, invalid("historical configuration settings are invalid")
		}
	}
	if admission.MaxCandidates != 0 {
		settings.AdmissionMaxCandidates = admission.MaxCandidates
	}
	if admission.MaxPages != 0 {
		settings.AdmissionMaxPages = admission.MaxPages
	}
	if settings.AdmissionMaxCandidates < 1 || settings.AdmissionMaxCandidates > 100 || settings.AdmissionMaxPages < 1 || settings.AdmissionMaxPages > 20 {
		return EditableSettings{}, invalid("historical configuration settings are invalid")
	}
	if expectedVersion == VersionThree {
		var maxActiveRuns int
		if len(admission.HeavyCapacity) != 0 || len(admission.MaxActiveRuns) != 0 && (string(admission.MaxActiveRuns) == "null" || json.Unmarshal(admission.MaxActiveRuns, &maxActiveRuns) != nil || maxActiveRuns != 1) {
			return EditableSettings{}, invalid("historical configuration settings are invalid")
		}
		if settings.AdmissionEnabled && len(admission.MaxActiveRuns) == 0 {
			return EditableSettings{}, invalid("historical configuration settings are invalid")
		}
	} else {
		if len(admission.MaxActiveRuns) != 0 {
			return EditableSettings{}, invalid("historical configuration settings are invalid")
		}
		if len(admission.HeavyCapacity) != 0 {
			if string(admission.HeavyCapacity) == "null" || json.Unmarshal(admission.HeavyCapacity, &settings.AdmissionHeavyCapacity) != nil || settings.AdmissionHeavyCapacity < 1 || settings.AdmissionHeavyCapacity > 32 {
				return EditableSettings{}, invalid("historical configuration settings are invalid")
			}
		}
	}
	return settings, nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var readValue func() error
	readValue = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]struct{}{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("JSON object key is invalid")
				}
				if _, exists := seen[key]; exists {
					return errors.New("JSON object key is duplicated")
				}
				seen[key] = struct{}{}
				if err := readValue(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := readValue(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return errors.New("JSON delimiter is invalid")
		}
	}
	if err := readValue(); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("JSON contains trailing values")
	}
	return nil
}

// MaterializeEditableSettings rewrites only the allowlisted leaves in memory.
// If the projection is unchanged it returns the exact retained bytes so the
// generation service preserves its same-digest no-op contract.
func MaterializeEditableSettings(canonicalPath string, base []byte, settings EditableSettings) ([]byte, error) {
	current, err := ProjectEditableSettings(canonicalPath, base)
	if err != nil {
		return nil, err
	}
	if current == settings {
		return append([]byte(nil), base...), nil
	}
	var document map[string]json.RawMessage
	if err := decodeStrictObject(base, &document); err != nil {
		return nil, invalid("configuration editable settings are invalid")
	}
	var controller map[string]json.RawMessage
	var automation map[string]json.RawMessage
	var admission map[string]json.RawMessage
	if decodeStrictObject(document["controller"], &controller) != nil {
		return nil, invalid("configuration editable settings are invalid")
	}
	hadAutomation := len(document["automation"]) != 0
	if hadAutomation && decodeStrictObject(document["automation"], &automation) != nil {
		return nil, invalid("configuration editable settings are invalid")
	}
	if automation == nil {
		automation = make(map[string]json.RawMessage)
	}
	hadAdmission := len(automation["linear_todo_admission"]) != 0
	if hadAdmission && decodeStrictObject(automation["linear_todo_admission"], &admission) != nil {
		return nil, invalid("configuration editable settings are invalid")
	}
	if admission == nil {
		admission = make(map[string]json.RawMessage)
	}
	set := func(target map[string]json.RawMessage, key string, value any) error {
		encoded, marshalErr := json.Marshal(value)
		if marshalErr == nil {
			target[key] = encoded
		}
		return marshalErr
	}
	if current.RunTimeout != settings.RunTimeout {
		if err := set(controller, "run_timeout", settings.RunTimeout.String()); err != nil {
			return nil, err
		}
	}
	admissionChanged := current.AdmissionEnabled != settings.AdmissionEnabled ||
		current.AdmissionPollInterval != settings.AdmissionPollInterval ||
		current.DeliveryPollInterval != settings.DeliveryPollInterval ||
		current.SchedulerLeaseTTL != settings.SchedulerLeaseTTL ||
		current.SchedulerLeaseRenewalInterval != settings.SchedulerLeaseRenewalInterval ||
		current.AdmissionMaxCandidates != settings.AdmissionMaxCandidates ||
		current.AdmissionMaxPages != settings.AdmissionMaxPages ||
		current.AdmissionHeavyCapacity != settings.AdmissionHeavyCapacity
	if !hadAdmission && admissionChanged {
		for key, value := range map[string]any{
			"enabled": settings.AdmissionEnabled, "poll_interval": settings.AdmissionPollInterval.String(),
			"delivery_poll_interval": settings.DeliveryPollInterval.String(), "scheduler_lease_ttl": settings.SchedulerLeaseTTL.String(),
			"scheduler_lease_renewal_interval": settings.SchedulerLeaseRenewalInterval.String(), "max_candidates": settings.AdmissionMaxCandidates,
			"max_pages": settings.AdmissionMaxPages, "heavy_capacity": settings.AdmissionHeavyCapacity,
		} {
			if err := set(admission, key, value); err != nil {
				return nil, err
			}
		}
	} else if current.AdmissionEnabled != settings.AdmissionEnabled {
		if err := set(admission, "enabled", settings.AdmissionEnabled); err != nil {
			return nil, err
		}
	}
	if current.AdmissionPollInterval != settings.AdmissionPollInterval {
		if err := set(admission, "poll_interval", settings.AdmissionPollInterval.String()); err != nil {
			return nil, err
		}
	}
	if current.DeliveryPollInterval != settings.DeliveryPollInterval {
		if err := set(admission, "delivery_poll_interval", settings.DeliveryPollInterval.String()); err != nil {
			return nil, err
		}
	}
	if current.SchedulerLeaseTTL != settings.SchedulerLeaseTTL {
		if err := set(admission, "scheduler_lease_ttl", settings.SchedulerLeaseTTL.String()); err != nil {
			return nil, err
		}
	}
	if current.SchedulerLeaseRenewalInterval != settings.SchedulerLeaseRenewalInterval {
		if err := set(admission, "scheduler_lease_renewal_interval", settings.SchedulerLeaseRenewalInterval.String()); err != nil {
			return nil, err
		}
	}
	if current.AdmissionMaxCandidates != settings.AdmissionMaxCandidates {
		if err := set(admission, "max_candidates", settings.AdmissionMaxCandidates); err != nil {
			return nil, err
		}
	}
	if current.AdmissionMaxPages != settings.AdmissionMaxPages {
		if err := set(admission, "max_pages", settings.AdmissionMaxPages); err != nil {
			return nil, err
		}
	}
	if current.AdmissionHeavyCapacity != settings.AdmissionHeavyCapacity {
		if err := set(admission, "heavy_capacity", settings.AdmissionHeavyCapacity); err != nil {
			return nil, err
		}
	}
	controllerJSON, err := json.Marshal(controller)
	if err != nil {
		return nil, err
	}
	document["controller"] = controllerJSON
	if hadAutomation || admissionChanged {
		admissionJSON, err := json.Marshal(admission)
		if err != nil {
			return nil, err
		}
		automation["linear_todo_admission"] = admissionJSON
		automationJSON, err := json.Marshal(automation)
		if err != nil {
			return nil, err
		}
		document["automation"] = automationJSON
	}
	candidate, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	candidate = append(candidate, '\n')
	if len(candidate) > 256<<10 {
		return nil, invalid("controller configuration is too large")
	}
	return candidate, nil
}

func decodeEditableDocument(data []byte) (configFile, linearTodoAdmissionFile, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var raw configFile
	if err := decoder.Decode(&raw); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return configFile{}, linearTodoAdmissionFile{}, invalid("configuration editable settings are invalid")
	}
	if len(raw.Automation) == 0 {
		return raw, linearTodoAdmissionFile{}, nil
	}
	var automation automationFile
	if decodeStrictObject(raw.Automation, &automation) != nil {
		return configFile{}, linearTodoAdmissionFile{}, invalid("configuration editable settings are invalid")
	}
	var admission linearTodoAdmissionFile
	if len(automation.LinearTodoAdmission) == 0 {
		return raw, admission, nil
	}
	if decodeStrictObject(automation.LinearTodoAdmission, &admission) != nil {
		return configFile{}, linearTodoAdmissionFile{}, invalid("configuration editable settings are invalid")
	}
	return raw, admission, nil
}

func decodeStrictObject(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("JSON contains trailing values")
	}
	return nil
}

func decodeRegistry(raw configFile) (localregistry.Registry, string, error) {
	switch raw.Version {
	case LegacyVersion:
		if len(raw.Repositories) != 0 {
			return localregistry.Registry{}, "", invalid("controller configuration version 1 must use repository_registry_file")
		}
		if len(raw.RepositoryRegistryFile) == 0 {
			return localregistry.Registry{}, "", missing("repository registry file is required")
		}
		var registryFile string
		if err := json.Unmarshal(raw.RepositoryRegistryFile, &registryFile); err != nil || strings.TrimSpace(registryFile) == "" {
			return localregistry.Registry{}, "", invalid("repository registry file is invalid")
		}
		registryPath, err := canonicalRegularPath(registryFile)
		if err != nil {
			return localregistry.Registry{}, "", err
		}
		registry, err := localregistry.Load(registryPath)
		if err != nil {
			return localregistry.Registry{}, "", invalid("repository registry is invalid")
		}
		return registry, registryPath, nil
	case VersionTwo, VersionThree, VersionFour, CurrentVersion:
		if len(raw.RepositoryRegistryFile) != 0 {
			return localregistry.Registry{}, "", invalid("controller configuration must use inline repositories")
		}
		if len(raw.Repositories) == 0 {
			return localregistry.Registry{}, "", missing("inline repositories are required")
		}
		decoder := json.NewDecoder(bytes.NewReader(raw.Repositories))
		decoder.DisallowUnknownFields()
		var repositories []localregistry.Repository
		if err := decoder.Decode(&repositories); err != nil {
			return localregistry.Registry{}, "", invalid("inline repositories are invalid")
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF || len(repositories) == 0 && raw.Version != CurrentVersion {
			return localregistry.Registry{}, "", invalid("inline repositories are invalid")
		}
		registry, err := localregistry.New(repositories)
		if err != nil {
			return localregistry.Registry{}, "", invalid("inline repositories are invalid")
		}
		return registry, "", nil
	default:
		return localregistry.Registry{}, "", invalid("unsupported controller configuration version")
	}
}

// decodeAutomation intentionally has no dependency on a client, database, or
// credential source. The configuration remains inert until the worker composes
// its admission and delivery mechanisms.
func decodeAutomation(raw configFile, registry localregistry.Registry) (Automation, error) {
	if raw.Version != VersionThree && raw.Version != VersionFour && raw.Version != CurrentVersion {
		if len(raw.Automation) != 0 {
			return Automation{}, invalid("automatic admission requires controller configuration version 3, 4, or 5")
		}
		return Automation{}, nil
	}
	if len(raw.Automation) == 0 {
		return Automation{}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw.Automation))
	decoder.DisallowUnknownFields()
	var file automationFile
	if err := decoder.Decode(&file); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return Automation{}, invalid("automation configuration is invalid")
	}
	if len(file.LinearTodoAdmission) == 0 {
		return Automation{}, invalid("automatic admission configuration is required")
	}
	decoder = json.NewDecoder(bytes.NewReader(file.LinearTodoAdmission))
	decoder.DisallowUnknownFields()
	var admission linearTodoAdmissionFile
	if err := decoder.Decode(&admission); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return Automation{}, invalid("automatic admission configuration is invalid")
	}
	if admission.Enabled == nil {
		return Automation{}, invalid("automatic admission enabled flag is required")
	}
	if !*admission.Enabled {
		return Automation{}, nil
	}
	return validateLinearTodoAdmission(admission, registry, raw.Version)
}

func validateLinearTodoAdmission(raw linearTodoAdmissionFile, registry localregistry.Registry, version int) (Automation, error) {
	if len(registry.Bindings()) == 0 {
		return Automation{}, conflict("automatic admission requires at least one repository profile")
	}
	if !validUUID(raw.TeamID) || raw.TeamKey != "IFAN" ||
		!validWorkflowState(raw.TodoState, "Todo", "unstarted") ||
		!validWorkflowState(raw.InProgressState, "In Progress", "started") ||
		raw.TodoState.ID == raw.InProgressState.ID {
		return Automation{}, invalid("automatic admission workflow authority is invalid")
	}
	poll, err := time.ParseDuration(raw.PollInterval)
	if err != nil || poll < time.Minute || poll > time.Hour {
		return Automation{}, invalid("automatic admission poll interval is invalid")
	}
	deliveryPoll := minimumDeliveryPollInterval
	if len(raw.DeliveryPollInterval) != 0 {
		var value string
		if decodeErr := json.Unmarshal(raw.DeliveryPollInterval, &value); decodeErr != nil {
			return Automation{}, invalid("automatic delivery poll interval is invalid")
		}
		configured, parseErr := time.ParseDuration(value)
		if parseErr != nil || configured < minimumDeliveryPollInterval || configured > maximumDeliveryPollInterval {
			return Automation{}, invalid("automatic delivery poll interval is invalid")
		}
		deliveryPoll = configured
	}
	leaseTTL, err := time.ParseDuration(raw.SchedulerLeaseTTL)
	if err != nil || leaseTTL < 30*time.Second || leaseTTL > 10*time.Minute {
		return Automation{}, invalid("automatic admission scheduler lease is invalid")
	}
	leaseRenewal, err := time.ParseDuration(raw.SchedulerLeaseRenewalInterval)
	if err != nil || leaseRenewal < 5*time.Second || leaseRenewal > leaseTTL/2 {
		return Automation{}, invalid("automatic admission scheduler lease renewal is invalid")
	}
	heavyCapacity := 0
	if version == VersionThree {
		var maxActiveRuns int
		if len(raw.MaxActiveRuns) == 0 || string(raw.MaxActiveRuns) == "null" || json.Unmarshal(raw.MaxActiveRuns, &maxActiveRuns) != nil || maxActiveRuns != 1 || len(raw.HeavyCapacity) != 0 {
			return Automation{}, invalid("automatic admission singleton migration authority is invalid")
		}
		heavyCapacity = 1
	} else {
		if len(raw.MaxActiveRuns) != 0 {
			return Automation{}, invalid("max_active_runs is retired; use heavy_capacity")
		}
		if len(raw.HeavyCapacity) == 0 {
			heavyCapacity = 2
		} else if string(raw.HeavyCapacity) == "null" || json.Unmarshal(raw.HeavyCapacity, &heavyCapacity) != nil {
			return Automation{}, invalid("automatic admission limits are invalid")
		}
	}
	if raw.MaxCandidates < 1 || raw.MaxCandidates > 100 || raw.MaxPages < 1 || raw.MaxPages > 20 || heavyCapacity < 1 || heavyCapacity > 32 {
		return Automation{}, invalid("automatic admission limits are invalid")
	}
	if raw.NotificationMode != "local_outbox" || !linearadapter.ValidCredentialSourceRef(raw.CredentialSourceRef) {
		return Automation{}, invalid("automatic admission notification or credential reference is invalid")
	}
	requester := localregistry.TrustedActorIdentity{DatabaseID: raw.Requester.DatabaseID, NodeID: raw.Requester.NodeID, Login: raw.Requester.Login, Type: raw.Requester.Type}
	if !validRequester(requester) {
		return Automation{}, invalid("automatic admission requester is invalid")
	}
	if !requesterTrustedByEveryProfile(requester, registry) {
		return Automation{}, conflict("automatic admission requester is not trusted by every repository profile")
	}
	return Automation{LinearTodoAdmission: LinearTodoAdmission{Enabled: true, TeamID: raw.TeamID, TeamKey: raw.TeamKey,
		TodoState:       WorkflowState{ID: raw.TodoState.ID, Name: raw.TodoState.Name, Type: raw.TodoState.Type},
		InProgressState: WorkflowState{ID: raw.InProgressState.ID, Name: raw.InProgressState.Name, Type: raw.InProgressState.Type},
		PollInterval:    poll, DeliveryPollInterval: deliveryPoll, SchedulerLeaseTTL: leaseTTL, SchedulerLeaseRenewal: leaseRenewal, MaxCandidates: raw.MaxCandidates,
		MaxPages: raw.MaxPages, MaxActiveRuns: func() int {
			if version == VersionThree {
				return 1
			}
			return 0
		}(), HeavyCapacity: heavyCapacity, Requester: requester, NotificationMode: raw.NotificationMode,
		CredentialSourceRef: raw.CredentialSourceRef}}, nil
}

func validUUID(value string) bool {
	if !uuidPattern.MatchString(value) {
		return false
	}
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.Variant() == uuid.RFC4122
}

func validWorkflowState(state workflowStateFile, name, stateType string) bool {
	return validUUID(state.ID) && state.Name == name && state.Type == stateType
}

func validRequester(requester localregistry.TrustedActorIdentity) bool {
	return requester.DatabaseID > 0 && requester.NodeID != "" && requester.Login != "" && requester.Type == "User"
}

func decodeConfiguredOperator(raw requesterFile, version int) (domain.GitHubUserIdentity, error) {
	operator := domain.GitHubUserIdentity{Login: raw.Login, DatabaseID: raw.DatabaseID, NodeID: raw.NodeID, ActorType: raw.Type}
	if version != CurrentVersion {
		if raw != (requesterFile{}) {
			return domain.GitHubUserIdentity{}, invalid("configured operator requires controller configuration version 5")
		}
		return domain.GitHubUserIdentity{}, nil
	}
	if operator.Validate() != nil {
		return domain.GitHubUserIdentity{}, invalid("configured operator identity is invalid")
	}
	return operator, nil
}

func operatorTrustedByEveryProfile(operator domain.GitHubUserIdentity, registry localregistry.Registry) bool {
	for _, binding := range registry.Bindings() {
		if !slices.ContainsFunc(binding.OperatorIdentityPolicy.AllowedLogins, func(login string) bool { return strings.EqualFold(login, operator.Login) }) ||
			!slices.ContainsFunc(binding.OperatorIdentityPolicy.TrustedActors, func(actor localregistry.TrustedActorIdentity) bool {
				return actor.DatabaseID == operator.DatabaseID && actor.NodeID == operator.NodeID && strings.EqualFold(actor.Login, operator.Login) && actor.Type == operator.ActorType
			}) {
			return false
		}
	}
	return true
}

func deriveLegacyConfiguredOperator(registry localregistry.Registry) (domain.GitHubUserIdentity, error) {
	bindings := registry.Bindings()
	if len(bindings) == 0 {
		return domain.GitHubUserIdentity{}, conflict("legacy configuration has no controller migration authority")
	}
	var candidates []domain.GitHubUserIdentity
	for _, actor := range bindings[0].OperatorIdentityPolicy.TrustedActors {
		candidate := domain.GitHubUserIdentity{Login: actor.Login, DatabaseID: actor.DatabaseID, NodeID: actor.NodeID, ActorType: actor.Type}
		if candidate.Validate() == nil && operatorTrustedByEveryProfile(candidate, registry) {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) == 0 {
		// A legacy file never declared Controller-wide authority. Preserve its
		// repository-scoped admission semantics without elevating one repository's
		// actor; configuration apply remains unavailable until a safe shared actor
		// exists through an explicit future migration/recovery contract.
		return domain.GitHubUserIdentity{}, nil
	}
	slices.SortFunc(candidates, func(left, right domain.GitHubUserIdentity) int {
		if order := strings.Compare(strings.ToLower(left.Login), strings.ToLower(right.Login)); order != 0 {
			return order
		}
		if left.DatabaseID < right.DatabaseID {
			return -1
		}
		if left.DatabaseID > right.DatabaseID {
			return 1
		}
		return strings.Compare(left.NodeID, right.NodeID)
	})
	return candidates[0], nil
}

func requesterTrustedByEveryProfile(requester localregistry.TrustedActorIdentity, registry localregistry.Registry) bool {
	for _, binding := range registry.Bindings() {
		matched := false
		for _, actor := range binding.OperatorIdentityPolicy.TrustedActors {
			if actor == requester {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func decodeController(raw controllerFile) (Controller, error) {
	databasePath, err := canonicalDatabasePath(raw.DatabasePath)
	if err != nil {
		return Controller{}, err
	}
	if !validCodexBinary(raw.CodexBinary) {
		return Controller{}, invalid("Codex binary must be a simple executable name or canonical absolute path")
	}
	timeout, err := time.ParseDuration(raw.RunTimeout)
	if err != nil || timeout <= 0 || timeout > 2*time.Hour {
		return Controller{}, invalid("controller run timeout is invalid")
	}
	return Controller{DatabasePath: databasePath, CodexBinary: raw.CodexBinary, RunTimeout: timeout}, nil
}

func validCodexBinary(value string) bool {
	if strings.TrimSpace(value) == "" || strings.Contains(value, "\\") {
		return false
	}
	if !strings.Contains(value, "/") {
		return true
	}
	return filepath.IsAbs(value) && filepath.Clean(value) == value
}

func decodeProfiles(raw []profileFile) (map[string]GitHubProfile, error) {
	if len(raw) == 0 {
		return nil, missing("at least one GitHub App profile is required")
	}
	profiles := make(map[string]GitHubProfile, len(raw))
	for _, item := range raw {
		if !validProfileID(item.ID) {
			return nil, invalid("GitHub App profile ID is invalid")
		}
		if _, exists := profiles[item.ID]; exists {
			return nil, conflict("duplicate GitHub App profile ID")
		}
		config, err := githubapp.DecodeConfigWithoutPrivateKey(bytes.NewReader(item.Config))
		if err != nil {
			return nil, invalid("GitHub App profile is invalid")
		}
		if err := inspectPrivateKeyPath(config.PrivateKeyFile); err != nil {
			return nil, err
		}
		digest := sha256.Sum256(item.Config)
		profiles[item.ID] = GitHubProfile{ID: item.ID, Config: config, Digest: hex.EncodeToString(digest[:])}
	}
	return profiles, nil
}

func crossCheck(registry localregistry.Registry, profiles map[string]GitHubProfile, version int) error {
	used := make(map[string]struct{})
	for _, binding := range registry.Bindings() {
		profile, ok := profiles[binding.GitHubAppProfileRef]
		if !ok {
			return missing("repository references a missing GitHub App profile")
		}
		used[profile.ID] = struct{}{}
		parts := strings.Split(binding.CanonicalRepository, "/")
		if profile.Config.AppID != binding.GitHubAppID || profile.Config.InstallationID != binding.GitHubInstallationID || profile.Config.RepositoryID != binding.ExpectedRepositoryID || !strings.EqualFold(profile.Config.RepositoryOwner, parts[0]) || !strings.EqualFold(profile.Config.RepositoryName, parts[1]) {
			return conflict("GitHub App profile does not match repository authority")
		}
	}
	// Current-schema repository removal deliberately preserves GitHub App
	// profiles. They can therefore be temporarily unreferenced, including when
	// the last managed repository is retired.
	if version != CurrentVersion && len(used) != len(profiles) {
		return missing("GitHub App profile is not referenced by a repository")
	}
	return nil
}

func validProfileID(value string) bool {
	if !strings.HasPrefix(value, "github-app-profile:") || len(value) > 128 {
		return false
	}
	name := strings.TrimPrefix(value, "github-app-profile:")
	if name == "" {
		return false
	}
	for _, char := range name {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '.' || char == '_' || char == '-' {
			continue
		}
		return false
	}
	return true
}

func readRegularConfig(path string) ([]byte, string, error) {
	canonical, err := canonicalRegularPath(path)
	if err != nil {
		return nil, "", err
	}
	before, err := os.Lstat(canonical)
	if err != nil {
		return nil, "", unsafe("controller configuration is unreadable")
	}
	file, err := os.Open(canonical)
	if err != nil {
		return nil, "", unsafe("controller configuration is unreadable")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) || opened.Mode()&os.ModeSymlink != 0 || !opened.Mode().IsRegular() {
		return nil, "", unsafe("configuration changed while it was being read")
	}
	data, err := io.ReadAll(io.LimitReader(file, (256<<10)+1))
	if err != nil {
		return nil, "", unsafe("controller configuration is unreadable")
	}
	if len(data) > 256<<10 {
		return nil, "", invalid("controller configuration is too large")
	}
	after, err := file.Stat()
	current, currentErr := os.Lstat(canonical)
	if err != nil || currentErr != nil || !os.SameFile(opened, after) || !os.SameFile(opened, current) ||
		after.Size() != int64(len(data)) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
		return nil, "", unsafe("configuration changed while it was being read")
	}
	return data, canonical, nil
}

func canonicalConfigLocation(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", unsafe("configuration path must be absolute and canonical")
	}
	parent := filepath.Dir(path)
	info, err := os.Lstat(parent)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", unsafe("configuration parent must be a non-symlink directory")
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil || resolved != parent {
		return "", unsafe("configuration parent path is ambiguous")
	}
	return path, nil
}

func canonicalRegularPath(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", unsafe("configuration path must be absolute and canonical")
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", unsafe("configuration file must be a non-symlink regular file")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return "", unsafe("configuration path is ambiguous")
	}
	return path, nil
}

func canonicalDatabasePath(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", unsafe("database path must be absolute and canonical")
	}
	parent := filepath.Dir(path)
	info, err := os.Lstat(parent)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", unsafe("database parent must be a non-symlink directory")
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil || resolved != parent {
		return "", unsafe("database parent path is ambiguous")
	}
	if info, err := os.Lstat(path); err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return "", unsafe("database path must be absent or a regular file")
	}
	return path, nil
}

func inspectPrivateKeyPath(path string) error {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return unsafe("GitHub App credential source is not a private regular file")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path || info.Size() > 64<<10 {
		return unsafe("GitHub App credential source path is ambiguous or invalid")
	}
	return nil
}
