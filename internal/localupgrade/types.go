package localupgrade

import (
	"context"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/buildidentity"
)

const (
	journalSchemaVersion = 3
	neutralLaunchdLabel  = "io.agent-loop-controller.worker"
	legacyLaunchdLabel   = "com.ifan.agent-loop-controller.worker"
)

type PrepareRequest struct {
	Revision   string
	Supervisor string
	BinaryPath string
	ConfigPath string
}

type SuccessorPrepareRequest struct {
	PredecessorUpgradeID string
	Revision             string
}

type SuccessorRecoveryPreviewRequest struct {
	PredecessorUpgradeID string
	Revision             string
}

type SuccessorRecoverPrepareRequest struct {
	PredecessorUpgradeID        string
	Revision                    string
	PreviewDigest               string
	DatabaseRelocationConfirmed bool
	FullBackupConfirmed         bool
}

// SuccessorRecoveryPreview is deliberately narrower than the private recovery
// evidence. Filesystem identities, paths, configuration digests, and database
// contents never cross the local-upgrade presentation boundary.
type SuccessorRecoveryPreview struct {
	UpgradeID             string   `json:"upgrade_id"`
	State                 string   `json:"state"`
	Reason                string   `json:"reason"`
	SuccessorRevision     string   `json:"successor_revision"`
	PreviewDigest         string   `json:"preview_digest"`
	RequiredConfirmations []string `json:"required_confirmations"`
}

type Result struct {
	UpgradeID            string   `json:"upgrade_id"`
	State                string   `json:"state"`
	Reason               string   `json:"reason"`
	NextAction           string   `json:"next_action"`
	UpgradeHealth        string   `json:"upgrade_health"`
	ControllerReadiness  string   `json:"controller_readiness"`
	Supervisor           string   `json:"supervisor,omitempty"`
	CandidateBuild       string   `json:"candidate_build_identity,omitempty"`
	PredecessorUpgradeID string   `json:"predecessor_upgrade_id,omitempty"`
	SuccessorUpgradeID   string   `json:"successor_upgrade_id,omitempty"`
	BootstrapIntent      bool     `json:"bootstrap_intent"`
	RequiresSudo         bool     `json:"requires_sudo,omitempty"`
	BootstrapInstruction []string `json:"bootstrap_instruction,omitempty"`
}

type binaryEvidence struct {
	Digest        string             `json:"digest"`
	Size          int64              `json:"size"`
	Mode          uint32             `json:"mode"`
	GoVersion     string             `json:"go_version,omitempty"`
	ModulePath    string             `json:"module_path,omitempty"`
	GoVCSRevision string             `json:"go_vcs_revision,omitempty"`
	GoVCSTime     string             `json:"go_vcs_time,omitempty"`
	GoVCSModified bool               `json:"go_vcs_modified"`
	Build         buildidentity.Info `json:"build"`
	Structured    bool               `json:"structured"`
	LegacyVersion string             `json:"legacy_version,omitempty"`
}

type databaseEvidence struct {
	Device        uint64 `json:"device"`
	Inode         uint64 `json:"inode"`
	SchemaVersion int    `json:"schema_version"`
}

type replacementDatabaseVerification struct {
	ContentDigest             string `json:"content_digest"`
	AuthorityDigest           string `json:"authority_digest"`
	SchemaVersion             int    `json:"schema_version"`
	IntegrityOK               bool   `json:"integrity_ok"`
	ForeignKeysOK             bool   `json:"foreign_keys_ok"`
	BindingMatches            bool   `json:"binding_matches"`
	DesiredConfigurationMatch bool   `json:"desired_configuration_match"`
	ReadinessReason           string `json:"readiness_reason"`
}

type databaseRecoveryEvidence struct {
	Version                     int                             `json:"version"`
	PreviewDigest               string                          `json:"preview_digest"`
	OldDatabase                 databaseEvidence                `json:"old_database"`
	ReplacementDatabase         databaseEvidence                `json:"replacement_database"`
	Verification                replacementDatabaseVerification `json:"verification"`
	SuccessorRevision           string                          `json:"successor_revision"`
	DatabaseRelocationConfirmed bool                            `json:"database_relocation_confirmed"`
	FullBackupConfirmed         bool                            `json:"full_backup_confirmed"`
	IntentAt                    time.Time                       `json:"intent_at"`
	LocatorPublishedAt          *time.Time                      `json:"locator_published_at,omitempty"`
}

type candidateManifest struct {
	SchemaVersion int              `json:"schema_version"`
	Revision      string           `json:"revision"`
	Candidate     binaryEvidence   `json:"candidate"`
	Previous      binaryEvidence   `json:"previous"`
	Database      databaseEvidence `json:"database"`
	ConfigDigest  string           `json:"configuration_digest"`
	PreparedAt    time.Time        `json:"prepared_at"`
}

type journal struct {
	SchemaVersion     int                       `json:"schema_version"`
	UpgradeID         string                    `json:"upgrade_id"`
	Phase             string                    `json:"phase"`
	Supervisor        string                    `json:"supervisor"`
	Revision          string                    `json:"revision"`
	BinaryPath        string                    `json:"binary_path"`
	ConfigPath        string                    `json:"configuration_path"`
	DatabasePath      string                    `json:"database_path"`
	ConfigDigest      string                    `json:"configuration_digest"`
	Candidate         binaryEvidence            `json:"candidate"`
	Previous          binaryEvidence            `json:"previous"`
	Database          databaseEvidence          `json:"database"`
	SnapshotDigest    string                    `json:"snapshot_digest,omitempty"`
	FailureReason     string                    `json:"failure_reason,omitempty"`
	PredecessorID     string                    `json:"predecessor_upgrade_id,omitempty"`
	SuccessorID       string                    `json:"successor_upgrade_id,omitempty"`
	SuccessorRevision string                    `json:"successor_revision,omitempty"`
	DatabaseRecovery  *databaseRecoveryEvidence `json:"database_recovery,omitempty"`
	CreatedAt         time.Time                 `json:"created_at"`
	UpdatedAt         time.Time                 `json:"updated_at"`
	BootstrapIntentAt *time.Time                `json:"bootstrap_intent_at,omitempty"`
	CompletedAt       *time.Time                `json:"completed_at,omitempty"`
	SupersededAt      *time.Time                `json:"superseded_at,omitempty"`
}

type commandResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

type commandRunner interface {
	Run(context.Context, string, string, ...string) (commandResult, error)
}
