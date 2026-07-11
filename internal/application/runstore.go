package application

import (
	"context"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type Run struct {
	ID                      string       `json:"run_id"`
	IssueID                 string       `json:"issue_id"`
	IdempotencyKey          string       `json:"idempotency_key"`
	SourceRevision          string       `json:"source_revision"`
	RawIssueJSON            string       `json:"-"`
	RawIssueHash            string       `json:"raw_issue_hash"`
	NormalizedTaskJSON      string       `json:"-"`
	TaskHash                string       `json:"task_snapshot_hash"`
	Repository              string       `json:"repository"`
	RepositoryConfigJSON    string       `json:"-"`
	RegistryVersion         int          `json:"registry_version"`
	RegistryDigest          string       `json:"registry_digest"`
	RepositoryBindingDigest string       `json:"repository_binding_digest"`
	BaseBranch              string       `json:"base_branch"`
	WorkingBranch           string       `json:"working_branch"`
	BaseSHA                 string       `json:"base_sha"`
	WorktreePath            string       `json:"worktree_path"`
	ArtifactRoot            string       `json:"artifact_root"`
	State                   domain.State `json:"current_state"`
	CandidateHead           string       `json:"candidate_head"`
	ImplementationSession   string       `json:"implementation_session_id"`
	ImplementationModel     string       `json:"implementation_model"`
	ReviewModel             string       `json:"review_model"`
	LastError               string       `json:"last_durable_error"`
	LeaseOwner              string       `json:"-"`
	LeaseExpiresAt          time.Time    `json:"-"`
	CreatedAt               time.Time    `json:"created_at"`
	UpdatedAt               time.Time    `json:"updated_at"`
}

type CreateRunInput struct {
	Run
}

type Transition struct {
	Sequence          int64        `json:"sequence"`
	From              domain.State `json:"from_state"`
	To                domain.State `json:"to_state"`
	Reason            string       `json:"reason"`
	EvidenceReference string       `json:"evidence_reference"`
	BoundHead         string       `json:"bound_head"`
	CreatedAt         time.Time    `json:"timestamp"`
}

type Attempt struct {
	ID             int64     `json:"attempt_id"`
	RunID          string    `json:"run_id"`
	Number         int       `json:"number"`
	Kind           string    `json:"kind"`
	Status         string    `json:"status"`
	SessionID      string    `json:"codex_session_id"`
	RequestedModel string    `json:"requested_model"`
	StartedAt      time.Time `json:"started_at"`
	FinishedAt     time.Time `json:"finished_at,omitempty"`
	ExitCode       int       `json:"exit_code"`
	StdoutPath     string    `json:"stdout_path"`
	StderrPath     string    `json:"stderr_path"`
	StdoutHash     string    `json:"stdout_hash"`
	StderrHash     string    `json:"stderr_hash"`
	StdoutSize     int64     `json:"stdout_size"`
	StderrSize     int64     `json:"stderr_size"`
	OutcomePath    string    `json:"outcome_path"`
	OutcomeHash    string    `json:"outcome_hash"`
	ArtifactDir    string    `json:"artifact_directory"`
	ErrorCategory  string    `json:"error_category"`
}

type VerificationRecord struct {
	ID           int64     `json:"verification_id"`
	RunID        string    `json:"run_id"`
	AttemptID    int64     `json:"attempt_id,omitempty"`
	VerifierID   string    `json:"verifier_id"`
	Phase        string    `json:"phase"`
	VerifiedHead string    `json:"verified_head"`
	ExitCode     int       `json:"exit_code"`
	StdoutPath   string    `json:"stdout_path"`
	StderrPath   string    `json:"stderr_path"`
	StdoutHash   string    `json:"stdout_hash"`
	StderrHash   string    `json:"stderr_hash"`
	StdoutSize   int64     `json:"stdout_size"`
	StderrSize   int64     `json:"stderr_size"`
	EvidencePath string    `json:"evidence_path"`
	EvidenceHash string    `json:"evidence_hash"`
	CreatedAt    time.Time `json:"timestamp"`
}

type ReviewRecord struct {
	ID           int64     `json:"review_id"`
	RunID        string    `json:"run_id"`
	AttemptID    int64     `json:"attempt_id"`
	SessionID    string    `json:"review_session_id"`
	ReviewedHead string    `json:"reviewed_head"`
	Verdict      string    `json:"verdict"`
	OutcomePath  string    `json:"outcome_path"`
	OutcomeHash  string    `json:"outcome_hash"`
	CreatedAt    time.Time `json:"timestamp"`
}

type OwnedResource struct {
	ID               int64     `json:"resource_id"`
	RunID            string    `json:"run_id"`
	Kind             string    `json:"kind"`
	Name             string    `json:"name"`
	CreationEvidence string    `json:"creation_evidence"`
	Status           string    `json:"ownership_status"`
	CreatedAt        time.Time `json:"created_at"`
}

type RunInspection struct {
	Run                Run                         `json:"run"`
	RepositoryBinding  *SanitizedRepositoryBinding `json:"repository_binding,omitempty"`
	Timeline           []Transition                `json:"state_timeline"`
	Attempts           []Attempt                   `json:"attempts"`
	Verifications      []VerificationRecord        `json:"verifications"`
	Reviews            []ReviewRecord              `json:"reviews"`
	Resources          []OwnedResource             `json:"owned_resources"`
	SideEffects        []SideEffectRecord          `json:"external_side_effects"`
	PullRequest        *domain.PullRequest         `json:"pull_request,omitempty"`
	Polls              []PollObservation           `json:"poll_observations"`
	Findings           []FindingRecord             `json:"normalized_review_findings"`
	Approval           *domain.HumanApproval       `json:"human_approval,omitempty"`
	Merge              *MergeRecord                `json:"merge_result,omitempty"`
	Cleanup            []CleanupRecord             `json:"cleanup_progress"`
	GitHubInstallation *GitHubInstallationMetadata `json:"github_installation,omitempty"`
	GitHubRequests     []GitHubRequestObservation  `json:"github_request_observations"`
	GitHubEvidence     *domain.GitHubReadEvidence  `json:"github_read_evidence,omitempty"`
}

type SanitizedRepositoryBinding struct {
	CanonicalRepository   string   `json:"canonical_repository"`
	BaseBranch            string   `json:"base_branch"`
	VerifierRegistryRef   string   `json:"verifier_registry_ref"`
	VerifierIDs           []string `json:"verifier_ids"`
	GitHubAppProfileRef   string   `json:"github_app_profile_ref"`
	GitHubInstallationID  int64    `json:"github_installation_id"`
	ExpectedRepositoryID  int64    `json:"expected_repository_id"`
	AllowedOperatorLogins []string `json:"allowed_operator_logins"`
}

type RunStore interface {
	CreateRun(context.Context, CreateRunInput) (Run, bool, error)
	GetRun(context.Context, string) (Run, error)
	Transition(context.Context, string, domain.State, domain.State, string, string, string) error
	SetWorkspace(context.Context, string, string, string) error
	SetImplementationSession(context.Context, string, string) error
	SetCandidateHead(context.Context, string, string) error
	BeginRepair(context.Context, string, string, string) error
	SetLastError(context.Context, string, string) error
	AcquireLease(context.Context, string, string, time.Time) (bool, error)
	RenewLease(context.Context, string, string, time.Time) (bool, error)
	ReleaseLease(context.Context, string, string) error
	BeginAttempt(context.Context, string, string, string, string) (Attempt, error)
	FinishAttempt(context.Context, Attempt) error
	SaveVerification(context.Context, VerificationRecord) error
	SaveReview(context.Context, ReviewRecord) error
	AddOwnedResource(context.Context, OwnedResource) error
	Inspect(context.Context, string) (RunInspection, error)
}
