package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

const (
	ConfigurationTargetID       = "controller-configuration"
	ConfigurationRawRetainCount = 10
)

var (
	ErrConfigurationAuthorityConflict = errors.New("configuration authority conflicts")
	ErrConfigurationApplyInProgress   = errors.New("configuration apply is unresolved")
)

type ConfigurationGenerationState string

const (
	ConfigurationGenerationAccepted       ConfigurationGenerationState = "accepted"
	ConfigurationGenerationPendingRestart ConfigurationGenerationState = "pending_restart"
	ConfigurationGenerationEffective      ConfigurationGenerationState = "effective"
	ConfigurationGenerationSuperseded     ConfigurationGenerationState = "superseded"
	ConfigurationGenerationFailed         ConfigurationGenerationState = "failed"
	ConfigurationGenerationAmbiguous      ConfigurationGenerationState = "ambiguous"
)

type ConfigurationGenerationOrigin string

const (
	ConfigurationOriginBaseline ConfigurationGenerationOrigin = "baseline"
	ConfigurationOriginApply    ConfigurationGenerationOrigin = "apply"
)

type ConfigurationApplyState string

const (
	ConfigurationApplyAccepted  ConfigurationApplyState = "accepted"
	ConfigurationApplyCommitted ConfigurationApplyState = "committed"
	ConfigurationApplyFailed    ConfigurationApplyState = "failed"
	ConfigurationApplyAmbiguous ConfigurationApplyState = "ambiguous"
)

type ConfigurationReason string

const (
	ConfigurationReasonReady              ConfigurationReason = "configuration_ready"
	ConfigurationReasonBaselinePending    ConfigurationReason = "baseline_pending_runtime"
	ConfigurationReasonRestartRequired    ConfigurationReason = "worker_restart_required"
	ConfigurationReasonRuntimeStarting    ConfigurationReason = "runtime_starting"
	ConfigurationReasonRuntimeStale       ConfigurationReason = "runtime_stale"
	ConfigurationReasonRuntimeOffline     ConfigurationReason = "runtime_offline"
	ConfigurationReasonRuntimeUnknown     ConfigurationReason = "runtime_unknown"
	ConfigurationReasonRuntimeConflict    ConfigurationReason = "runtime_identity_conflict"
	ConfigurationReasonExternalDrift      ConfigurationReason = "external_config_drift"
	ConfigurationReasonUnsafeLiveFile     ConfigurationReason = "unsafe_live_configuration"
	ConfigurationReasonAuthorityMissing   ConfigurationReason = "configuration_authority_missing"
	ConfigurationReasonApplyIncomplete    ConfigurationReason = "configuration_apply_incomplete"
	ConfigurationReasonApplyAmbiguous     ConfigurationReason = "configuration_apply_ambiguous"
	ConfigurationReasonAuthorityConflict  ConfigurationReason = "configuration_authority_conflict"
	ConfigurationReasonEffectiveUnsettled ConfigurationReason = "effective_observation_pending"
)

type ConfigurationNextAction string

const (
	ConfigurationActionNone             ConfigurationNextAction = "none"
	ConfigurationActionRestartWorker    ConfigurationNextAction = "restart_worker"
	ConfigurationActionWaitForWorker    ConfigurationNextAction = "wait_for_worker"
	ConfigurationActionInspectRuntime   ConfigurationNextAction = "inspect_runtime"
	ConfigurationActionRecoverAuthority ConfigurationNextAction = "recover_configuration_authority"
)

type ConfigurationReadiness string

const (
	ConfigurationReady           ConfigurationReadiness = "ready"
	ConfigurationRestartRequired ConfigurationReadiness = "restart_required"
	ConfigurationStarting        ConfigurationReadiness = "starting"
	ConfigurationStale           ConfigurationReadiness = "stale"
	ConfigurationOffline         ConfigurationReadiness = "offline"
	ConfigurationUnknown         ConfigurationReadiness = "unknown"
	ConfigurationConflict        ConfigurationReadiness = "conflict"
)

type ConfigurationGeneration struct {
	GenerationID       int64                         `json:"generation_id"`
	ParentID           int64                         `json:"parent_generation_id,omitempty"`
	Digest             string                        `json:"digest"`
	Size               int64                         `json:"size"`
	SchemaVersion      int                           `json:"schema_version"`
	Origin             ConfigurationGenerationOrigin `json:"origin"`
	Requester          domain.GitHubUserIdentity     `json:"requester,omitempty"`
	ConfiguredOperator domain.GitHubUserIdentity     `json:"configured_operator,omitempty"`
	OperationID        string                        `json:"operation_id,omitempty"`
	State              ConfigurationGenerationState  `json:"state"`
	RawRetained        bool                          `json:"raw_retained"`
	CreatedAt          time.Time                     `json:"created_at"`
	CommittedAt        time.Time                     `json:"committed_at,omitempty"`
	EffectiveAt        time.Time                     `json:"effective_at,omitempty"`
	SupersededAt       time.Time                     `json:"superseded_at,omitempty"`
	SettledAt          time.Time                     `json:"settled_at,omitempty"`
	Reason             ConfigurationReason           `json:"reason,omitempty"`
}

type ConfigurationApplyIntent struct {
	GenerationID int64                   `json:"generation_id"`
	ParentID     int64                   `json:"parent_generation_id"`
	ParentDigest string                  `json:"parent_digest"`
	TargetDigest string                  `json:"target_digest"`
	OperationID  string                  `json:"operation_id"`
	State        ConfigurationApplyState `json:"state"`
	AcceptedAt   time.Time               `json:"accepted_at"`
	SettledAt    time.Time               `json:"settled_at,omitempty"`
	Reason       ConfigurationReason     `json:"reason,omitempty"`
}

// ConfigurationAuthority contains public generation identity plus the private
// binding needed only by Controller composition. Private paths are never
// serialized or copied into audit/projection records.
type ConfigurationAuthority struct {
	Desired             ConfigurationGeneration   `json:"desired"`
	EffectiveID         int64                     `json:"effective_generation_id,omitempty"`
	Incomplete          *ConfigurationApplyIntent `json:"incomplete_apply,omitempty"`
	Version             int64                     `json:"authority_version"`
	UpdatedAt           time.Time                 `json:"updated_at"`
	CanonicalConfigPath string                    `json:"-"`
	DatabasePath        string                    `json:"-"`
}

type ConfigurationRepositoryAuthority struct {
	CanonicalRepository     string
	ProfileID               string
	ProfileDigest           string
	RepositoryBindingDigest string
	BaseBranch              string
	OriginPath              string
	SourcePath              string
	RunRoot                 string
	WorktreeRoot            string
	VerifierRegistryRef     string
	VerifierIDs             []string
	GitHubAppProfileRef     string
	GitHubAppID             int64
	GitHubInstallationID    int64
	ExpectedRepositoryID    int64
	AllowedOperatorLogins   []string
	TrustedOperatorActors   []TrustedActorIdentity
}

// ValidatedConfigurationCandidate is Controller-derived private evidence. Raw
// bytes remain with the caller/file adapter and are never part of a projection.
type ValidatedConfigurationCandidate struct {
	Digest        string
	Size          int64
	SchemaVersion int
	DatabasePath  string
	Operator      domain.GitHubUserIdentity
	Repositories  map[string]ConfigurationRepositoryAuthority
}

// DatabaseFileIdentity is private filesystem authority for the exact SQLite
// inode opened by production composition. It is never caller or presentation
// input.
type DatabaseFileIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

func (i DatabaseFileIdentity) Valid() bool { return i.Device != 0 && i.Inode != 0 }

type ConfigurationBaselineInput struct {
	Candidate           ValidatedConfigurationCandidate
	CanonicalConfigPath string
	ObservedAt          time.Time
}

type ConfigurationApplyAcceptance struct {
	ExpectedGenerationID int64
	ExpectedDigest       string
	Candidate            ValidatedConfigurationCandidate
	Requester            domain.GitHubUserIdentity
	Receipt              OperationReceipt
	AcceptedAt           time.Time
}

type ConfigurationApplySettlement struct {
	GenerationID   int64
	ParentID       int64
	OperationID    string
	Outcome        ConfigurationApplyState
	Reason         ConfigurationReason
	EvidenceDigest string
	SettledAt      time.Time
}

type ConfigurationEffectiveObservation struct {
	ExpectedGenerationID int64
	ExpectedDigest       string
	WorkerInstanceID     string
	BuildIdentity        string
	ObservedAt           time.Time
	EvidenceDigest       string
}

type ConfigurationDriftObservation struct {
	ExpectedGenerationID int64
	ExpectedDigest       string
	ObservedDigest       string
	Drifted              bool
	Reason               ConfigurationReason
	ObservedAt           time.Time
}

type ConfigurationNoOpSettlement struct {
	ExpectedGenerationID int64
	ExpectedDigest       string
	Receipt              OperationReceipt
	EvidenceDigest       string
	ResultDigest         string
	SettledAt            time.Time
}

type ConfigurationGenerationStore interface {
	ConfigurationAuthority(context.Context) (ConfigurationAuthority, bool, error)
	PrepareConfigurationBaseline(context.Context, ConfigurationBaselineInput) error
	AdoptConfigurationBaseline(context.Context, ConfigurationBaselineInput) (ConfigurationAuthority, bool, error)
	RecordConfigurationNoOp(context.Context, ConfigurationNoOpSettlement) (ConfigurationAuthority, OperationReceipt, bool, error)
	BeginConfigurationApply(context.Context, ConfigurationApplyAcceptance) (ConfigurationGeneration, OperationReceipt, bool, error)
	SettleConfigurationApply(context.Context, ConfigurationApplySettlement) (ConfigurationAuthority, OperationReceipt, bool, error)
	ObserveConfigurationEffective(context.Context, ConfigurationEffectiveObservation) (ConfigurationAuthority, bool, error)
	ObserveConfigurationDrift(context.Context, ConfigurationDriftObservation) (bool, error)
	ListConfigurationGenerations(context.Context) ([]ConfigurationGeneration, error)
	ConfigurationRawPruneCandidates(context.Context, int) ([]string, error)
	ConfigurationRawPruneClaims(context.Context) ([]string, error)
	ClaimConfigurationRawPrune(context.Context, string) (bool, error)
	CompleteConfigurationRawPrune(context.Context, string, bool) error
	ListNonterminalRuns(context.Context) ([]Run, error)
}

type ConfigurationFileAuthority interface {
	CanonicalConfigPath() string
	ValidateBaseline([]byte) (ValidatedConfigurationCandidate, error)
	ValidateCurrent([]byte) (ValidatedConfigurationCandidate, error)
	ReadLive() ([]byte, ValidatedConfigurationCandidate, error)
	RetainRaw(string, []byte) error
	ReadRaw(string, int64) ([]byte, error)
	HasRaw(string, int64) bool
	PublishBaselineBinding(ValidatedConfigurationCandidate) error
	AcquireMutation() (ConfigurationReplacementLock, bool, error)
	AcquireReplacement(string) (ConfigurationReplacementLock, bool, error)
	ReplaceLive(string, []byte, []byte) error
	ReconcileReplacement(string, []byte, []byte) ([]byte, ValidatedConfigurationCandidate, error)
	RemoveRaw(string) error
	PublishLocator(string) error
}

type ConfigurationReplacementLock interface {
	Release() error
}

type ConfigurationRuntimeObserver interface {
	ObserveConfigurationRuntime(context.Context, time.Time) (RuntimeObservation, error)
}

type ConfigurationApplyCommand struct {
	Requester            Requester
	ExpectedGenerationID int64
	ExpectedDigest       string
	Payload              []byte
}

type ConfigurationApplyResult struct {
	Generation ConfigurationGeneration `json:"generation"`
	Receipt    OperationReceipt        `json:"receipt"`
	NoOp       bool                    `json:"no_op"`
}

type ConfigurationConvergenceProjection struct {
	State                     ConfigurationReadiness  `json:"state"`
	Reason                    ConfigurationReason     `json:"reason"`
	NextAction                ConfigurationNextAction `json:"next_action"`
	DesiredGenerationID       int64                   `json:"desired_generation_id,omitempty"`
	DesiredDigest             string                  `json:"desired_digest,omitempty"`
	EffectiveGenerationID     int64                   `json:"effective_generation_id,omitempty"`
	LoadedConfigurationDigest string                  `json:"loaded_configuration_digest,omitempty"`
	LastMeaningfulObservation *time.Time              `json:"last_meaningful_observation,omitempty"`
}

type NewAdmissionDecision struct {
	Allowed   bool
	Reason    ConfigurationReason
	Authority ConfigurationAdmissionAuthority
}

type ConfigurationAdmissionAuthority struct {
	GenerationID     int64     `json:"generation_id"`
	Digest           string    `json:"digest"`
	AuthorityVersion int64     `json:"authority_version"`
	ValidThrough     time.Time `json:"valid_through"`
}

func (a ConfigurationAdmissionAuthority) Valid() bool {
	return a.GenerationID > 0 && a.AuthorityVersion > 0 && validAuthorityDigest(a.Digest) && !a.ValidThrough.IsZero()
}

type NewAdmissionGate interface {
	CheckNewAdmission(context.Context) (NewAdmissionDecision, error)
}

type StaticNewAdmissionGate struct{ Decision NewAdmissionDecision }

func (g StaticNewAdmissionGate) CheckNewAdmission(context.Context) (NewAdmissionDecision, error) {
	return g.Decision, nil
}

func AllowNewAdmissionForTest() NewAdmissionGate {
	return StaticNewAdmissionGate{Decision: NewAdmissionDecision{Allowed: true, Reason: ConfigurationReasonReady, Authority: ConfigurationAdmissionAuthority{GenerationID: 1, Digest: strings.Repeat("0", 64), AuthorityVersion: 1, ValidThrough: time.Now().UTC().Add(time.Hour)}}}
}

func configurationDigest(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ConfigurationEvidenceDigest gives narrow adapters a shared, versioned
// digest format without exposing raw configuration content.
func ConfigurationEvidenceDigest(parts ...string) string {
	return configurationDigest(parts...)
}

func validConfigurationCandidate(candidate ValidatedConfigurationCandidate) bool {
	return validAuthorityDigest(candidate.Digest) && candidate.Size >= 0 && candidate.Size <= 256<<10 && candidate.SchemaVersion > 0 && strings.TrimSpace(candidate.DatabasePath) != ""
}

func configurationRequester(candidate ValidatedConfigurationCandidate) (domain.GitHubUserIdentity, bool) {
	if candidate.Operator.Validate() != nil {
		return domain.GitHubUserIdentity{}, false
	}
	return candidate.Operator, true
}

func ConfigurationCompatibleWithActiveRuns(currentOperator domain.GitHubUserIdentity, candidate ValidatedConfigurationCandidate, runs []Run) error {
	if len(runs) > 0 && !currentOperator.Equal(candidate.Operator) {
		return errors.New("configuration changes controller operator authority required by an active run")
	}
	for _, run := range runs {
		configured, ok := candidate.Repositories[strings.ToLower(strings.TrimSpace(run.Repository))]
		if !ok {
			return errors.New("configuration changes authority required by an active run")
		}
		var frozen LocalRepository
		if strings.TrimSpace(run.RepositoryConfigJSON) == "" || decodeStrictJSON([]byte(run.RepositoryConfigJSON), &frozen) != nil {
			return errors.New("active run authority is unavailable")
		}
		if run.Repository != configured.CanonicalRepository || run.BaseBranch != configured.BaseBranch ||
			frozen.ProfileID != configured.ProfileID || frozen.ProfileDigest != configured.ProfileDigest ||
			frozen.RepositoryBindingDigest != configured.RepositoryBindingDigest || frozen.OriginPath != configured.OriginPath ||
			frozen.SourcePath != configured.SourcePath || frozen.RunRoot != configured.RunRoot || frozen.WorktreeRoot != configured.WorktreeRoot ||
			frozen.VerifierRegistryRef != configured.VerifierRegistryRef || frozen.GitHubAppProfileRef != configured.GitHubAppProfileRef ||
			frozen.GitHubAppID != configured.GitHubAppID || frozen.GitHubInstallationID != configured.GitHubInstallationID ||
			frozen.ExpectedRepositoryID != configured.ExpectedRepositoryID || !slices.Equal(frozen.VerifierIDs, configured.VerifierIDs) ||
			!slices.Equal(frozen.AllowedOperatorLogins, configured.AllowedOperatorLogins) || !slices.Equal(frozen.TrustedOperatorActors, configured.TrustedOperatorActors) {
			return errors.New("configuration changes authority required by an active run")
		}
	}
	return nil
}

func decodeStrictJSON(data []byte, target any) error {
	// RepositoryConfigJSON is Controller-owned immutable evidence produced by
	// json.Marshal. Re-marshalling after decode rejects trailing values without
	// importing presentation or bootstrap semantics into this package.
	if len(data) == 0 {
		return errors.New("empty JSON")
	}
	return json.Unmarshal(data, target)
}
