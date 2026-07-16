package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type ErrorCategory string

const (
	ErrorInvalidInput ErrorCategory = "invalid_input"
	ErrorConflict     ErrorCategory = "conflict"
	ErrorNotFound     ErrorCategory = "not_found"
	ErrorUnavailable  ErrorCategory = "unavailable"
	ErrorInternal     ErrorCategory = "internal"
)

// ServiceError is safe for transport adapters to render. Cause is deliberately
// omitted so filesystem paths, external bodies, and credentials cannot leak.
type ServiceError struct {
	Category ErrorCategory `json:"category"`
	Message  string        `json:"message"`
	cause    error
}

func (e *ServiceError) Error() string { return fmt.Sprintf("%s: %s", e.Category, e.Message) }
func (e *ServiceError) Unwrap() error { return e.cause }

func serviceError(category ErrorCategory, message string, cause error) error {
	return &ServiceError{Category: category, Message: message, cause: cause}
}

type Requester struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	DatabaseID int64  `json:"database_id,omitempty"`
	NodeID     string `json:"node_id,omitempty"`
	ActorType  string `json:"actor_type,omitempty"`
}

func (r Requester) authorize(allowed []string, trusted []TrustedActorIdentity) error {
	if r.ID == "" || r.Kind != "github_login" {
		return serviceError(ErrorInvalidInput, "requester identity is required", nil)
	}
	if !slices.ContainsFunc(allowed, func(login string) bool { return strings.EqualFold(login, r.ID) }) {
		return serviceError(ErrorConflict, "requester is not authorized for the repository", nil)
	}
	if len(trusted) > 0 {
		if !slices.ContainsFunc(trusted, func(actor TrustedActorIdentity) bool {
			return actor.DatabaseID == r.DatabaseID && actor.NodeID == r.NodeID && strings.EqualFold(actor.Login, r.ID) && actor.Type == r.ActorType
		}) {
			return serviceError(ErrorConflict, "requester is not authorized for the repository", nil)
		}
		return nil
	}
	return nil
}

// AuthorizeRequester is the minimal adapter preflight for controller-owned
// authority data that must be loaded before other untrusted inputs are read.
func AuthorizeRequester(requester Requester, allowed []string, trusted ...TrustedActorIdentity) error {
	return requester.authorize(allowed, trusted)
}

type StartCommand struct {
	Requester           Requester       `json:"requester"`
	RepositorySelection string          `json:"repository"`
	IdempotencyKey      string          `json:"idempotency_key"`
	Input               LocalStartInput `json:"-"`
}

type ContinueCommand struct {
	Requester      Requester    `json:"requester"`
	RunID          string       `json:"run_id"`
	ExpectedState  domain.State `json:"expected_state"`
	Repository     string       `json:"repository"`
	IdempotencyKey string       `json:"idempotency_key"`
	Decision       *Decision    `json:"decision,omitempty"`
}

type CommandResult struct {
	Run RunResult `json:"run"`
}

type RunResult struct {
	RunID string `json:"run_id"`
	// IdempotencyKey is an operational compare-and-swap value, not an
	// authentication credential. It is projected only after requester
	// authorization so an operator can safely resume a persisted run.
	IdempotencyKey          string       `json:"idempotency_key"`
	IssueID                 string       `json:"issue_id"`
	Repository              string       `json:"repository"`
	ProfileID               string       `json:"profile_id"`
	ProfileSnapshotVersion  int          `json:"profile_snapshot_version"`
	ProfileDigest           string       `json:"profile_digest"`
	RegistryVersion         int          `json:"registry_version"`
	RegistryDigest          string       `json:"registry_digest"`
	RepositoryBindingDigest string       `json:"repository_binding_digest"`
	BaseBranch              string       `json:"base_branch"`
	WorkingBranch           string       `json:"working_branch"`
	BaseSHA                 string       `json:"base_sha"`
	State                   domain.State `json:"current_state"`
	CandidateHead           string       `json:"candidate_head"`
	TaskHash                string       `json:"task_snapshot_hash"`
	ImplementationModel     string       `json:"implementation_model"`
	ReviewModel             string       `json:"review_model"`
}

type LocalRunController interface {
	StartAuthorized(context.Context, LocalStartInput, func(Run) error) (Run, error)
	ContinueExpected(context.Context, string, domain.State, string, *Decision) (Run, error)
	EnforceRepairDeadline(context.Context, string) (Run, error)
	BoundRepairActionContext(context.Context, string) (context.Context, context.CancelFunc, error)
}

type CommandService struct {
	controller LocalRunController
	store      RunStore
}

var reconcileLeaseTTL = localLeaseTTL

func NewCommandService(controller LocalRunController, store RunStore) CommandService {
	return CommandService{controller: controller, store: store}
}

func (s CommandService) Start(ctx context.Context, command StartCommand) (CommandResult, error) {
	if command.IdempotencyKey == "" || command.IdempotencyKey != command.Input.IdempotencyKey {
		return CommandResult{}, serviceError(ErrorInvalidInput, "idempotency key does not match the admitted task", nil)
	}
	if command.RepositorySelection == "" || command.RepositorySelection != command.Input.Task.Repository || command.RepositorySelection != command.Input.Repository.CanonicalRepository {
		return CommandResult{}, serviceError(ErrorInvalidInput, "repository selection does not match the admitted task", nil)
	}
	existing, found, err := s.store.GetRunByIdempotency(ctx, command.IdempotencyKey)
	if err != nil {
		return CommandResult{}, classifyServiceError(err)
	}
	if found {
		if err := authorizePersistedRequester(existing, command.Requester); err != nil {
			return CommandResult{}, err
		}
		if existing.TaskHash != command.Input.TaskHash || existing.Repository != command.RepositorySelection || !samePersistedProfile(existing, command.Input.Repository) {
			return CommandResult{}, serviceError(ErrorConflict, "idempotency key belongs to a different run authority", nil)
		}
		run, err := s.controller.ContinueExpected(ctx, existing.ID, existing.State, existing.IdempotencyKey, nil)
		if err != nil {
			return CommandResult{}, classifyServiceError(err)
		}
		return CommandResult{Run: projectRunResult(run)}, nil
	}
	if err := command.Requester.authorize(command.Input.Repository.AllowedOperatorLogins, command.Input.Repository.TrustedOperatorActors); err != nil {
		return CommandResult{}, err
	}
	run, err := s.controller.StartAuthorized(ctx, command.Input, func(existing Run) error {
		return authorizePersistedRequester(existing, command.Requester)
	})
	if err != nil {
		return CommandResult{}, classifyServiceError(err)
	}
	return CommandResult{Run: projectRunResult(run)}, nil
}

func samePersistedProfile(run Run, current LocalRepository) bool {
	var persisted LocalRepository
	if err := json.Unmarshal([]byte(run.RepositoryConfigJSON), &persisted); err != nil {
		return false
	}
	return run.ProfileID == current.ProfileID && run.ProfileSnapshotVersion == current.ProfileSnapshotVersion &&
		run.ProfileDigest == current.ProfileDigest && run.ProfileSnapshotJSON == current.ProfileSnapshotJSON &&
		persisted.OriginPath == current.OriginPath && persisted.SourcePath == current.SourcePath &&
		persisted.RunRoot == current.RunRoot && persisted.WorktreeRoot == current.WorktreeRoot
}

func (s CommandService) Continue(ctx context.Context, command ContinueCommand) (CommandResult, error) {
	if command.RunID == "" || command.ExpectedState == "" || command.Repository == "" || command.IdempotencyKey == "" {
		return CommandResult{}, serviceError(ErrorInvalidInput, "run, expected state, repository, and idempotency key are required", nil)
	}
	run, err := s.store.GetRun(ctx, command.RunID)
	if err != nil {
		return CommandResult{}, classifyServiceError(err)
	}
	if run.Repository != command.Repository {
		return CommandResult{}, serviceError(ErrorConflict, "run repository does not match the request", nil)
	}
	if err := authorizePersistedRequester(run, command.Requester); err != nil {
		return CommandResult{}, err
	}
	if run.State != command.ExpectedState {
		return CommandResult{}, serviceError(ErrorConflict, "run state changed before the command was applied", nil)
	}
	if run.IdempotencyKey != command.IdempotencyKey {
		return CommandResult{}, serviceError(ErrorConflict, "run idempotency authority does not match the request", nil)
	}
	run, err = s.controller.ContinueExpected(ctx, command.RunID, command.ExpectedState, command.IdempotencyKey, command.Decision)
	if err != nil {
		return CommandResult{}, classifyServiceError(err)
	}
	return CommandResult{Run: projectRunResult(run)}, nil
}

const querySchemaVersion = "v1"

const (
	defaultRunSummaryLimit = 25
	maximumRunSummaryLimit = 100
)

type QueryInput struct {
	Requester  Requester `json:"requester"`
	RunID      string    `json:"run_id"`
	Repository string    `json:"repository"`
}

// RunSummaryQuery is a repository-scoped, bounded read request. Cursor is an
// opaque value issued by a previous RunSummaryPage.
type RunSummaryQuery struct {
	Requester  Requester `json:"requester"`
	Repository string    `json:"repository"`
	Limit      int       `json:"limit,omitempty"`
	Cursor     string    `json:"cursor,omitempty"`
}

type RunDetailQuery struct {
	Requester Requester `json:"requester"`
	RunID     string    `json:"run_id"`
}

type runSummaryCursor struct {
	Version   string    `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	RunID     string    `json:"run_id"`
}

type QueryStore interface {
	GetRun(context.Context, string) (Run, error)
	ListRuns(context.Context, string, time.Time, string, int) ([]Run, error)
	Inspect(context.Context, string) (RunInspection, error)
	OperatorAttentionQuery
}

type QueryService struct{ store QueryStore }

func NewQueryService(store QueryStore) QueryService { return QueryService{store: store} }

func (s QueryService) Status(ctx context.Context, input QueryInput) (InspectionResult, error) {
	return s.Inspect(ctx, input)
}

func (s QueryService) Inspect(ctx context.Context, input QueryInput) (InspectionResult, error) {
	if _, err := s.authorize(ctx, input); err != nil {
		return InspectionResult{}, err
	}
	inspection, err := s.store.Inspect(ctx, input.RunID)
	if err != nil {
		return InspectionResult{}, classifyServiceError(err)
	}
	attention, err := s.store.ListOperatorAttention(ctx, OperatorAttentionQueryInput{RunID: input.RunID, Limit: maxOperatorAttentionProjection})
	if err != nil {
		return InspectionResult{}, classifyServiceError(err)
	}
	inspection.OperatorAttention = attention
	return projectInspection(inspection), nil
}

// GetRunDetail reads and authorizes one run entirely through the application
// boundary so transport adapters do not render persistence aggregates.
func (s QueryService) GetRunDetail(ctx context.Context, query RunDetailQuery) (InspectionResult, error) {
	if query.RunID == "" {
		return InspectionResult{}, serviceError(ErrorInvalidInput, "run is required", nil)
	}
	run, err := s.store.GetRun(ctx, query.RunID)
	if err != nil {
		return InspectionResult{}, classifyServiceError(err)
	}
	return s.Inspect(ctx, QueryInput{Requester: query.Requester, RunID: query.RunID, Repository: run.Repository})
}

// ListRunSummaries returns a deterministic, repository-scoped page. It reads
// one extra row only to decide whether a following cursor exists.
func (s QueryService) ListRunSummaries(ctx context.Context, query RunSummaryQuery) (RunSummaryPage, error) {
	if query.Repository == "" {
		return RunSummaryPage{}, serviceError(ErrorInvalidInput, "repository is required", nil)
	}
	limit := query.Limit
	if limit == 0 {
		limit = defaultRunSummaryLimit
	}
	if limit < 1 || limit > maximumRunSummaryLimit {
		return RunSummaryPage{}, serviceError(ErrorInvalidInput, "limit must be between 1 and 100", nil)
	}
	cursor, err := decodeRunSummaryCursor(query.Cursor)
	if err != nil {
		return RunSummaryPage{}, err
	}
	runs, err := s.store.ListRuns(ctx, query.Repository, cursor.CreatedAt, cursor.RunID, limit+1)
	if err != nil {
		return RunSummaryPage{}, classifyServiceError(err)
	}
	for _, run := range runs {
		if run.Repository != query.Repository {
			return RunSummaryPage{}, serviceError(ErrorInternal, "run list repository mismatch", nil)
		}
		if err := authorizePersistedRequester(run, query.Requester); err != nil {
			return RunSummaryPage{}, err
		}
	}
	page := RunSummaryPage{SchemaVersion: querySchemaVersion, Repository: query.Repository}
	if len(runs) > limit {
		page.HasMore = true
		runs = runs[:limit]
	}
	for _, run := range runs {
		page.Runs = append(page.Runs, projectRunSummary(run))
	}
	if page.HasMore && len(runs) > 0 {
		page.NextCursor = encodeRunSummaryCursor(runSummaryCursor{Version: querySchemaVersion, CreatedAt: runs[len(runs)-1].CreatedAt, RunID: runs[len(runs)-1].ID})
	}
	return page, nil
}

func decodeRunSummaryCursor(value string) (runSummaryCursor, error) {
	if value == "" {
		return runSummaryCursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return runSummaryCursor{}, serviceError(ErrorInvalidInput, "cursor is invalid", nil)
	}
	var cursor runSummaryCursor
	if err := json.Unmarshal(raw, &cursor); err != nil || cursor.Version != querySchemaVersion || cursor.CreatedAt.IsZero() || cursor.RunID == "" {
		return runSummaryCursor{}, serviceError(ErrorInvalidInput, "cursor is invalid", nil)
	}
	return cursor, nil
}

func encodeRunSummaryCursor(cursor runSummaryCursor) string {
	raw, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func (s QueryService) authorize(ctx context.Context, input QueryInput) (Run, error) {
	if input.RunID == "" || input.Repository == "" {
		return Run{}, serviceError(ErrorInvalidInput, "run and repository are required", nil)
	}
	run, err := s.store.GetRun(ctx, input.RunID)
	if err != nil {
		return Run{}, classifyServiceError(err)
	}
	if run.Repository != input.Repository {
		return Run{}, serviceError(ErrorConflict, "run repository does not match the request", nil)
	}
	if err := authorizePersistedRequester(run, input.Requester); err != nil {
		return Run{}, err
	}
	return run, nil
}

func authorizePersistedRequester(run Run, requester Requester) error {
	var repository LocalRepository
	if err := json.Unmarshal([]byte(run.RepositoryConfigJSON), &repository); err != nil {
		return serviceError(ErrorConflict, "persisted repository authority is invalid", err)
	}
	return requester.authorize(repository.AllowedOperatorLogins, repository.TrustedOperatorActors)
}

func AuthorizePersistedRequester(run Run, requester Requester) error {
	return authorizePersistedRequester(run, requester)
}

type InspectionResult struct {
	SchemaVersion           string                          `json:"schema_version"`
	Run                     RunResult                       `json:"run"`
	RepositoryBinding       *RepositoryBindingResult        `json:"repository_binding,omitempty"`
	Timeline                []TransitionResult              `json:"state_timeline"`
	Attempts                []AttemptResult                 `json:"attempts"`
	Verifications           []VerificationResult            `json:"verifications"`
	Reviews                 []ReviewResult                  `json:"reviews"`
	Resources               []ResourceResult                `json:"owned_resources"`
	PullRequest             *PullRequestResult              `json:"pull_request,omitempty"`
	Approval                *HumanApprovalResult            `json:"human_approval,omitempty"`
	ApprovalStatus          *HumanApprovalStatusResult      `json:"human_approval_status,omitempty"`
	Merge                   *MergeRecord                    `json:"merge_result,omitempty"`
	LinearCompletion        []LinearCompletionObservation   `json:"linear_completion_observations"`
	Cleanup                 []CleanupResult                 `json:"cleanup_progress"`
	RetrySchedules          []RetrySchedule                 `json:"retry_schedules"`
	OperatorAttentionEvents []OperatorAttentionEventResult  `json:"operator_attention_events"`
	OperatorActions         []OperatorActionResult          `json:"operator_actions"`
	Checks                  []CheckResult                   `json:"checks"`
	Findings                []FindingResult                 `json:"review_findings"`
	TrustedFeedback         []TrustedFeedbackResult         `json:"trusted_review_feedback"`
	FeedbackConflicts       []TrustedFeedbackConflictResult `json:"trusted_review_feedback_conflicts"`
	Telemetry               []TelemetryResult               `json:"unknown_telemetry"`
}
type RunSummaryPage struct {
	SchemaVersion string       `json:"schema_version"`
	Repository    string       `json:"repository"`
	Runs          []RunSummary `json:"runs"`
	NextCursor    string       `json:"next_cursor,omitempty"`
	HasMore       bool         `json:"has_more"`
}
type RunSummary struct {
	RunID                  string       `json:"run_id"`
	IssueID                string       `json:"issue_id"`
	Repository             string       `json:"repository"`
	ProfileID              string       `json:"profile_id"`
	ProfileSnapshotVersion int          `json:"profile_snapshot_version"`
	ProfileDigest          string       `json:"profile_digest"`
	State                  domain.State `json:"current_state"`
	CandidateHead          string       `json:"candidate_head"`
	CreatedAt              time.Time    `json:"created_at"`
	UpdatedAt              time.Time    `json:"updated_at"`
}
type RepositoryBindingResult struct {
	ProfileID              string   `json:"profile_id"`
	ProfileSnapshotVersion int      `json:"profile_snapshot_version"`
	ProfileDigest          string   `json:"profile_digest"`
	CanonicalRepository    string   `json:"canonical_repository"`
	BaseBranch             string   `json:"base_branch"`
	VerifierRegistryRef    string   `json:"verifier_registry_ref"`
	VerifierIDs            []string `json:"verifier_ids"`
	GitHubAppID            int64    `json:"github_app_id"`
	GitHubInstallationID   int64    `json:"github_installation_id"`
	ExpectedRepositoryID   int64    `json:"expected_repository_id"`
}
type TransitionResult struct {
	Sequence  int64        `json:"sequence"`
	From      domain.State `json:"from_state"`
	To        domain.State `json:"to_state"`
	Reason    string       `json:"reason"`
	BoundHead string       `json:"bound_head"`
	CreatedAt time.Time    `json:"timestamp"`
}
type AttemptResult struct {
	Number           int       `json:"number"`
	Kind             string    `json:"kind"`
	Status           string    `json:"status"`
	RequestedModel   string    `json:"requested_model"`
	ErrorCategory    string    `json:"error_category"`
	StartedAt        time.Time `json:"started_at"`
	FinishedAt       time.Time `json:"finished_at,omitempty"`
	ExitCode         int       `json:"exit_code"`
	OutcomeHash      string    `json:"outcome_hash"`
	SessionRecorded  bool      `json:"session_recorded"`
	ArtifactRecorded bool      `json:"artifact_recorded"`
}
type VerificationResult struct {
	VerifierID      string    `json:"verifier_id"`
	Phase           string    `json:"phase"`
	VerifiedHead    string    `json:"verified_head"`
	ProcessOutcome  string    `json:"process_outcome"`
	FailureCategory string    `json:"failure_category,omitempty"`
	ExitCode        int       `json:"exit_code"`
	EvidenceHash    string    `json:"evidence_hash"`
	CreatedAt       time.Time `json:"timestamp"`
}
type ReviewResult struct {
	ReviewedHead string    `json:"reviewed_head"`
	Verdict      string    `json:"verdict"`
	OutcomeHash  string    `json:"outcome_hash"`
	CreatedAt    time.Time `json:"timestamp"`
}
type ResourceResult struct {
	Kind      string    `json:"kind"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}
type CleanupResult struct {
	Kind       string    `json:"resource_kind"`
	Status     string    `json:"status"`
	ErrorClass string    `json:"error_class,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// OperatorAttentionEventResult is a bounded read-only projection of the
// transport-neutral, sanitized attention envelope.
type OperatorAttentionEventResult struct {
	SchemaVersion         int                         `json:"schema_version"`
	EventKey              string                      `json:"event_key"`
	EventType             string                      `json:"event_type"`
	RunID                 string                      `json:"run_id,omitempty"`
	LinearIdentifier      string                      `json:"linear_identifier,omitempty"`
	RepositoryProfileID   string                      `json:"repository_profile_id"`
	RepositoryProfileName string                      `json:"repository_profile_name"`
	ControllerState       string                      `json:"controller_state"`
	Severity              string                      `json:"severity"`
	ReasonCode            string                      `json:"reason_code"`
	AllowedActions        []OperatorAttentionActionID `json:"allowed_actions"`
	PayloadDigest         string                      `json:"payload_digest"`
	EvidenceDigest        string                      `json:"evidence_digest"`
	OccurredAt            time.Time                   `json:"occurred_at"`
	ObservedAt            time.Time                   `json:"observed_at"`
}

// OperatorActionResult exposes provenance and digests without replay
// authority, raw command arguments, or run idempotency keys.
type OperatorActionResult struct {
	ActionID                    string             `json:"action_id"`
	ActionType                  OperatorActionType `json:"action_type"`
	Repository                  string             `json:"repository"`
	ExpectedState               domain.State       `json:"expected_state"`
	TransitionSequence          int64              `json:"transition_sequence"`
	RequesterLogin              string             `json:"requester_login"`
	RequesterDatabaseID         int64              `json:"requester_database_id"`
	RequesterNodeID             string             `json:"requester_node_id"`
	RequesterActorType          string             `json:"requester_actor_type"`
	ReasonCode                  string             `json:"reason_code"`
	AttentionEventKey           string             `json:"attention_event_key"`
	Status                      string             `json:"status"`
	ResultStatus                string             `json:"result_status"`
	ResultingState              domain.State       `json:"resulting_state,omitempty"`
	ResultingTransitionSequence int64              `json:"resulting_transition_sequence,omitempty"`
	PayloadDigest               string             `json:"payload_digest"`
	EvidenceDigest              string             `json:"evidence_digest,omitempty"`
	OutcomeDigest               string             `json:"outcome_digest,omitempty"`
	NextEligibleAt              time.Time          `json:"next_eligible_at,omitempty"`
	ReceivedAt                  time.Time          `json:"received_at"`
	ValidatedAt                 time.Time          `json:"validated_at"`
	AppliedAt                   time.Time          `json:"applied_at,omitempty"`
	ObservedAt                  time.Time          `json:"observed_at,omitempty"`
}
type CheckResult struct {
	Name        string    `json:"name"`
	Required    bool      `json:"required"`
	Source      string    `json:"source"`
	State       string    `json:"state"`
	ObservedSHA string    `json:"observed_sha"`
	ObservedAt  time.Time `json:"observed_at"`
}
type HumanApprovalResult struct {
	Approver    string    `json:"approver"`
	ApprovedSHA string    `json:"approved_sha"`
	SourceAt    time.Time `json:"source_timestamp"`
	ObservedAt  time.Time `json:"observation_timestamp"`
}
type HumanApprovalStatusResult struct {
	Status        string    `json:"status"`
	CandidateHead string    `json:"candidate_head"`
	ReviewHeadSHA string    `json:"review_head_sha,omitempty"`
	SourceAt      time.Time `json:"source_timestamp,omitempty"`
	ObservedAt    time.Time `json:"observation_timestamp"`
}
type FindingResult struct {
	Source       string    `json:"source"`
	SourceID     string    `json:"source_id"`
	File         string    `json:"file,omitempty"`
	Line         int       `json:"line,omitempty"`
	Severity     string    `json:"severity"`
	BodyDigest   string    `json:"body_digest"`
	Content      string    `json:"content,omitempty"`
	ContentTrust string    `json:"content_trust"`
	Resolved     bool      `json:"resolved"`
	Outdated     bool      `json:"outdated"`
	HeadSHA      string    `json:"observed_head_sha"`
	ObservedAt   time.Time `json:"observed_at"`
}

// TrustedFeedbackResult exposes durable authority markers only. The raw human
// comment remains in the dedicated bounded store and is never an inspect value.
type TrustedFeedbackResult struct {
	PRNumber              int64     `json:"pr_number"`
	PRDatabaseID          int64     `json:"pr_database_id"`
	PRNodeID              string    `json:"pr_node_id"`
	ReviewDatabaseID      int64     `json:"review_database_id"`
	ReviewNodeID          string    `json:"review_node_id"`
	ThreadNodeID          string    `json:"thread_node_id"`
	RootCommentDatabaseID int64     `json:"root_comment_database_id"`
	RootCommentNodeID     string    `json:"root_comment_node_id"`
	AuthorDatabaseID      int64     `json:"author_database_id"`
	AuthorNodeID          string    `json:"author_node_id"`
	AuthorLogin           string    `json:"author_login"`
	TrustedAuthor         bool      `json:"trusted_author"`
	OriginalHeadSHA       string    `json:"original_review_head_sha"`
	Path                  string    `json:"path,omitempty"`
	Line                  *int      `json:"line,omitempty"`
	BodyDigest            string    `json:"body_digest"`
	Lifecycle             string    `json:"lifecycle"`
	BoundRepairHead       string    `json:"bound_repair_head,omitempty"`
	ReplyIntentKey        string    `json:"reply_intent_key,omitempty"`
	ReplyDatabaseID       int64     `json:"reply_database_id,omitempty"`
	ReplyNodeID           string    `json:"reply_node_id,omitempty"`
	Resolved              bool      `json:"resolved"`
	Outdated              bool      `json:"outdated"`
	SourceAt              time.Time `json:"source_timestamp"`
	ObservedAt            time.Time `json:"observation_timestamp"`
	UpdatedAt             time.Time `json:"updated_at"`
}
type TrustedFeedbackConflictResult struct {
	RootCommentNodeID string    `json:"root_comment_node_id"`
	ObservedDigest    string    `json:"observed_body_digest"`
	ReasonCode        string    `json:"reason_code"`
	ObservedAt        time.Time `json:"observed_at"`
	OperatorAttention bool      `json:"operator_attention"`
}
type TelemetryResult struct {
	Kind       string    `json:"kind"`
	Value      string    `json:"value"`
	ObservedAt time.Time `json:"observed_at,omitempty"`
}
type PullRequestResult struct {
	Number     int64     `json:"number"`
	URL        string    `json:"url"`
	HeadBranch string    `json:"head_branch"`
	BaseBranch string    `json:"base_branch"`
	HeadSHA    string    `json:"head_sha"`
	BaseSHA    string    `json:"base_sha"`
	State      string    `json:"state"`
	Merged     bool      `json:"merged"`
	MergeSHA   string    `json:"merge_sha"`
	MergedAt   time.Time `json:"merged_at,omitempty"`
}

func projectInspection(value RunInspection) InspectionResult {
	result := InspectionResult{SchemaVersion: querySchemaVersion, Run: projectRunResult(value.Run), RepositoryBinding: projectRepositoryBinding(value.RepositoryBinding), Merge: value.Merge,
		Timeline: []TransitionResult{}, Attempts: []AttemptResult{}, Verifications: []VerificationResult{}, Reviews: []ReviewResult{}, Resources: []ResourceResult{}, LinearCompletion: append([]LinearCompletionObservation(nil), value.LinearCompletion...), Cleanup: []CleanupResult{}, RetrySchedules: append([]RetrySchedule(nil), value.RetrySchedules...), OperatorAttentionEvents: []OperatorAttentionEventResult{}, OperatorActions: []OperatorActionResult{}, Checks: []CheckResult{}, Findings: []FindingResult{}, TrustedFeedback: []TrustedFeedbackResult{}, FeedbackConflicts: []TrustedFeedbackConflictResult{}, Telemetry: []TelemetryResult{}}
	if value.Approval != nil {
		result.Approval = &HumanApprovalResult{Approver: sanitizeUntrustedContent(value.Approval.Approver), ApprovedSHA: value.Approval.ApprovedSHA, SourceAt: value.Approval.ApprovedAt, ObservedAt: value.Approval.ObservedAt}
	}
	if value.ApprovalObservation != nil {
		result.ApprovalStatus = &HumanApprovalStatusResult{Status: string(value.ApprovalObservation.Status), CandidateHead: value.ApprovalObservation.CandidateHead, ReviewHeadSHA: value.ApprovalObservation.ReviewHeadSHA, SourceAt: value.ApprovalObservation.SourceAt, ObservedAt: value.ApprovalObservation.ObservedAt}
	}
	if value.PullRequest != nil {
		v := value.PullRequest
		result.PullRequest = &PullRequestResult{v.Number, sanitizeExternalURL(v.URL), v.HeadBranch, v.BaseBranch, v.HeadSHA, v.BaseSHA, v.State, v.Merged, v.MergeSHA, v.MergedAt}
	}
	for _, v := range value.Timeline {
		result.Timeline = append(result.Timeline, TransitionResult{v.Sequence, v.From, v.To, sanitizeUntrustedContent(v.Reason), v.BoundHead, v.CreatedAt})
	}
	for _, v := range value.Attempts {
		result.Attempts = append(result.Attempts, AttemptResult{v.Number, v.Kind, v.Status, v.RequestedModel, v.ErrorCategory, v.StartedAt, v.FinishedAt, v.ExitCode, v.OutcomeHash, v.SessionID != "", v.ArtifactDir != ""})
	}
	for _, v := range value.Verifications {
		outcome := v.ProcessOutcome
		if outcome != VerificationOutcomeNotStarted && outcome != VerificationOutcomeExited && outcome != VerificationOutcomeInterrupted && outcome != VerificationOutcomeLegacy {
			outcome = "unknown"
		}
		category := v.FailureCategory
		if category != "" && category != "artifact_setup" && category != "not_started" && category != "process_start" && category != "process_interrupted" && category != "process_wait" && category != "invalid_result" && category != "unknown" && category != "legacy_evidence" {
			category = "unknown"
		}
		result.Verifications = append(result.Verifications, VerificationResult{VerifierID: v.VerifierID, Phase: v.Phase, VerifiedHead: v.VerifiedHead, ProcessOutcome: outcome, FailureCategory: category, ExitCode: v.ExitCode, EvidenceHash: v.EvidenceHash, CreatedAt: v.CreatedAt})
	}
	for _, v := range value.Reviews {
		result.Reviews = append(result.Reviews, ReviewResult{v.ReviewedHead, v.Verdict, v.OutcomeHash, v.CreatedAt})
	}
	for _, v := range value.Resources {
		result.Resources = append(result.Resources, ResourceResult{v.Kind, v.Status, v.CreatedAt})
	}
	for _, v := range value.Cleanup {
		result.Cleanup = append(result.Cleanup, CleanupResult{v.Kind, v.Status, sanitizedCleanupErrorClass(v), v.UpdatedAt})
	}
	for _, event := range value.OperatorAttention {
		profile := projectedOperatorAttentionProfile(event)
		result.OperatorAttentionEvents = append(result.OperatorAttentionEvents, OperatorAttentionEventResult{SchemaVersion: event.SchemaVersion, EventKey: event.EventKey, EventType: event.EventType, RunID: event.RunID, LinearIdentifier: event.LinearIdentifier, RepositoryProfileID: profile.ID, RepositoryProfileName: profile.Name, ControllerState: event.ControllerState, Severity: event.Severity, ReasonCode: event.ReasonCode, AllowedActions: append([]OperatorAttentionActionID{}, event.AllowedActions...), PayloadDigest: event.PayloadDigest, EvidenceDigest: event.EvidenceDigest, OccurredAt: event.OccurredAt, ObservedAt: event.ObservedAt})
	}
	for _, action := range value.OperatorActions {
		result.OperatorActions = append(result.OperatorActions, OperatorActionResult{ActionID: action.ActionID, ActionType: action.ActionType, Repository: action.Repository, ExpectedState: action.ExpectedState, TransitionSequence: action.TransitionSequence, RequesterLogin: sanitizeUntrustedContent(action.Requester.ID), RequesterDatabaseID: action.Requester.DatabaseID, RequesterNodeID: sanitizeUntrustedContent(action.Requester.NodeID), RequesterActorType: sanitizeUntrustedContent(action.Requester.ActorType), ReasonCode: action.ReasonCode, AttentionEventKey: action.AttentionEventKey, Status: action.Status, ResultStatus: action.ResultStatus, ResultingState: action.ResultingState, ResultingTransitionSequence: action.ResultingTransitionSequence, PayloadDigest: action.PayloadDigest, EvidenceDigest: action.EvidenceDigest, OutcomeDigest: action.OutcomeDigest, NextEligibleAt: action.NextEligibleAt, ReceivedAt: action.ReceivedAt, ValidatedAt: action.ValidatedAt, AppliedAt: action.AppliedAt, ObservedAt: action.ObservedAt})
	}
	for _, finding := range value.Findings {
		result.Findings = append(result.Findings, FindingResult{Source: finding.Source, SourceID: finding.SourceID,
			File: sanitizeRepositoryPath(finding.File), Line: finding.Line, Severity: finding.Severity, BodyDigest: finding.BodyDigest,
			ContentTrust: "untrusted", Resolved: finding.Resolved,
			Outdated: finding.Outdated, HeadSHA: finding.HeadSHA, ObservedAt: finding.ObservedAt})
	}
	for _, feedback := range value.TrustedFeedback {
		result.TrustedFeedback = append(result.TrustedFeedback, TrustedFeedbackResult{PRNumber: feedback.PRNumber, PRDatabaseID: feedback.PRDatabaseID, PRNodeID: feedback.PRNodeID, ReviewDatabaseID: feedback.ReviewDatabaseID, ReviewNodeID: feedback.ReviewNodeID, ThreadNodeID: feedback.ThreadNodeID, RootCommentDatabaseID: feedback.RootCommentDatabaseID, RootCommentNodeID: feedback.RootCommentNodeID, AuthorDatabaseID: feedback.Author.DatabaseID, AuthorNodeID: feedback.Author.NodeID, AuthorLogin: sanitizeUntrustedContent(feedback.Author.Login), TrustedAuthor: feedback.Author.Type == "User", OriginalHeadSHA: feedback.OriginalReviewHeadSHA, Path: sanitizeRepositoryPath(feedback.Path), Line: feedback.Line, BodyDigest: feedback.BodyDigest, Lifecycle: string(feedback.Lifecycle), BoundRepairHead: feedback.BoundRepairHead, ReplyIntentKey: sanitizeUntrustedContent(feedback.ReplyIntentKey), ReplyDatabaseID: feedback.ReplyDatabaseID, ReplyNodeID: feedback.ReplyNodeID, Resolved: feedback.Resolved, Outdated: feedback.Outdated, SourceAt: feedback.SourceAt, ObservedAt: feedback.ObservedAt, UpdatedAt: feedback.UpdatedAt})
	}
	for _, conflict := range value.FeedbackConflicts {
		result.FeedbackConflicts = append(result.FeedbackConflicts, TrustedFeedbackConflictResult{RootCommentNodeID: conflict.RootCommentNodeID, ObservedDigest: conflict.ObservedDigest, ReasonCode: conflict.ReasonCode, ObservedAt: conflict.ObservedAt, OperatorAttention: true})
	}
	appendUnknownTelemetry(&result, value)
	return result
}

const sourceCheckoutAttentionReason = "source_checkout_requires_manual_sync"

func sanitizedCleanupErrorClass(record CleanupRecord) string {
	if record.Kind == "source_checkout" && record.Status == "skipped_attention" {
		return sourceCheckoutAttentionReasonCode(record.ErrorClass)
	}
	if record.Kind == "source_checkout" && record.Status == "failed" {
		switch record.ErrorClass {
		case string(SourceSyncReasonFetchFailed), string(SourceSyncReasonGitUncertain):
			return record.ErrorClass
		default:
			return "source_checkout_retryable_failure"
		}
	}
	return record.ErrorClass
}

func sourceCheckoutAttentionReasonCode(reason string) string {
	switch reason {
	case string(SourceSyncReasonDirtySource), string(SourceSyncReasonWrongBranch), string(SourceSyncReasonDetachedHead), string(SourceSyncReasonSourceDiverged), string(SourceSyncReasonStateDrift):
		return reason
	default:
		return sourceCheckoutAttentionReason
	}
}

func projectRunSummary(run Run) RunSummary {
	return RunSummary{RunID: run.ID, IssueID: run.IssueID, Repository: run.Repository, ProfileID: run.ProfileID,
		ProfileSnapshotVersion: run.ProfileSnapshotVersion, ProfileDigest: run.ProfileDigest, State: run.State,
		CandidateHead: run.CandidateHead, CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt}
}

func projectRepositoryBinding(value *SanitizedRepositoryBinding) *RepositoryBindingResult {
	if value == nil {
		return nil
	}
	return &RepositoryBindingResult{ProfileID: value.ProfileID, ProfileSnapshotVersion: value.ProfileSnapshotVersion,
		ProfileDigest: value.ProfileDigest, CanonicalRepository: value.CanonicalRepository, BaseBranch: value.BaseBranch,
		VerifierRegistryRef: value.VerifierRegistryRef, VerifierIDs: append([]string(nil), value.VerifierIDs...), GitHubAppID: value.GitHubAppID,
		GitHubInstallationID: value.GitHubInstallationID, ExpectedRepositoryID: value.ExpectedRepositoryID}
}

func appendUnknownTelemetry(result *InspectionResult, value RunInspection) {
	if value.Run.State != "" && !knownState(value.Run.State) {
		result.Telemetry = append(result.Telemetry, TelemetryResult{Kind: "run_state", Value: sanitizeTelemetryValue(string(value.Run.State)), ObservedAt: value.Run.UpdatedAt})
	}
	for _, transition := range value.Timeline {
		if transition.From != "" && !knownState(transition.From) {
			result.Telemetry = append(result.Telemetry, TelemetryResult{Kind: "transition_from_state", Value: sanitizeTelemetryValue(string(transition.From)), ObservedAt: transition.CreatedAt})
		}
		if transition.To != "" && !knownState(transition.To) {
			result.Telemetry = append(result.Telemetry, TelemetryResult{Kind: "transition_to_state", Value: sanitizeTelemetryValue(string(transition.To)), ObservedAt: transition.CreatedAt})
		}
	}
	if value.GitHubEvidence == nil {
		return
	}
	evidence := value.GitHubEvidence
	for _, check := range evidence.Checks {
		state := string(check.State)
		if !knownCheckState(check.State) {
			state = sanitizeTelemetryValue(state)
		}
		result.Checks = append(result.Checks, CheckResult{Name: sanitizeUntrustedContent(check.Name), Required: check.Required, Source: sanitizeUntrustedContent(check.Source),
			State: state, ObservedSHA: check.ObservedSHA, ObservedAt: check.ObservedAt})
		if !knownCheckState(check.State) {
			result.Telemetry = append(result.Telemetry, TelemetryResult{Kind: "check_state", Value: sanitizeTelemetryValue(string(check.State)), ObservedAt: check.ObservedAt})
		}
	}
	for _, event := range evidence.UnknownEvents {
		result.Telemetry = append(result.Telemetry, TelemetryResult{Kind: "github_event", Value: sanitizeTelemetryValue(event), ObservedAt: evidence.ObservedAt})
	}
}

func knownState(value domain.State) bool {
	switch value {
	case domain.StateReceived, domain.StateAdmitting, domain.StateProvisioning, domain.StateExecuting, domain.StateAwaitingHumanDecision, domain.StateVerifying, domain.StateFreshReview, domain.StateApprovalReady, domain.StatePushingBranch, domain.StateBranchPushed, domain.StateOpeningPR, domain.StateRepairing, domain.StatePROpen, domain.StateReconcilingReviews, domain.StateAwaitingHumanApproval, domain.StateMerging, domain.StateAwaitingGitHubMergeability, domain.StateCleaning, domain.StateFailed, domain.StateCompleted, domain.StateRejected, domain.StateManualIntervention:
		return true
	default:
		return false
	}
}

func knownCheckState(value domain.CheckState) bool {
	switch value {
	case domain.CheckQueued, domain.CheckInProgress, domain.CheckPending, domain.CheckRequested, domain.CheckWaiting, domain.CheckSuccess, domain.CheckNeutral, domain.CheckSkipped, domain.CheckFailure, domain.CheckActionRequired, domain.CheckCancelled, domain.CheckTimedOut, domain.CheckStale, domain.CheckUnknown:
		return true
	default:
		return false
	}
}

var (
	sensitiveValuePattern = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*(?:bearer|basic|token)?\s*|(?:api[_-]?key|access[_-]?token|refresh[_-]?token|token|password|secret|credential)\s*[:=]\s*)[^\s,;]+`)
	absolutePathPattern   = regexp.MustCompile(`(^|\s)/[^\s]+`)
)

func sanitizeUntrustedContent(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || json.Valid([]byte(value)) {
		return ""
	}
	value = sensitiveValuePattern.ReplaceAllString(value, "[redacted]")
	value = absolutePathPattern.ReplaceAllString(value, "$1[redacted path]")
	if len(value) > 4096 {
		value = value[:4096] + "…"
	}
	return value
}

func sanitizeTelemetryValue(value string) string {
	if sanitized := sanitizeUntrustedContent(value); sanitized != "" {
		return sanitized
	}
	return "[untrusted structured value omitted]"
}

func sanitizeRepositoryPath(value string) string {
	if value == "" || value == "." || value == ".." || strings.TrimSpace(value) != value || strings.ContainsRune(value, '\x00') || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") || strings.HasPrefix(value, "~") || path.Clean(value) != value || strings.HasPrefix(value, "../") {
		return ""
	}
	return value
}

func sanitizeExternalURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	parsed.User, parsed.RawQuery, parsed.ForceQuery, parsed.Fragment = nil, "", false, ""
	return parsed.String()
}

func classifyServiceError(err error) error {
	var safe *ServiceError
	if errors.As(err, &safe) {
		return err
	}
	if errors.Is(err, ErrRunNotFound) {
		return serviceError(ErrorNotFound, "run was not found", err)
	}
	message := "application operation failed"
	category := ErrorInternal
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		message, category = "application operation was interrupted", ErrorUnavailable
	}
	return serviceError(category, message, err)
}

func ClassifyError(err error) error { return classifyServiceError(err) }

func SanitizeInspection(inspection *RunInspection) {
	inspection.Run = sanitizedRun(inspection.Run)
	for index := range inspection.Timeline {
		inspection.Timeline[index].EvidenceReference = ""
	}
	for index := range inspection.Attempts {
		inspection.Attempts[index].SessionID, inspection.Attempts[index].StdoutPath, inspection.Attempts[index].StderrPath = "", "", ""
		inspection.Attempts[index].OutcomePath, inspection.Attempts[index].ArtifactDir = "", ""
	}
	for index := range inspection.Verifications {
		inspection.Verifications[index].StdoutPath, inspection.Verifications[index].StderrPath, inspection.Verifications[index].EvidencePath = "", "", ""
	}
	for index := range inspection.Reviews {
		inspection.Reviews[index].SessionID, inspection.Reviews[index].OutcomePath = "", ""
	}
	for index := range inspection.Resources {
		inspection.Resources[index].Name, inspection.Resources[index].CreationEvidence = "", ""
	}
	for index := range inspection.SideEffects {
		inspection.SideEffects[index].IntentJSON, inspection.SideEffects[index].ResultJSON, inspection.SideEffects[index].StdoutPath, inspection.SideEffects[index].StderrPath = "", "", "", ""
	}
	for index := range inspection.Polls {
		inspection.Polls[index].SnapshotJSON = ""
	}
	for index := range inspection.Findings {
		inspection.Findings[index].Body, inspection.Findings[index].File = "", ""
	}
	for index := range inspection.Cleanup {
		inspection.Cleanup[index].Name, inspection.Cleanup[index].LastError = "", ""
	}
}

func sanitizedRun(run Run) Run {
	run.WorktreePath = ""
	run.ArtifactRoot = ""
	run.LastError = ""
	run.ImplementationSession = ""
	return run
}

func projectRunResult(run Run) RunResult {
	return RunResult{RunID: run.ID, IdempotencyKey: run.IdempotencyKey, IssueID: run.IssueID, Repository: run.Repository, ProfileID: run.ProfileID,
		ProfileSnapshotVersion: run.ProfileSnapshotVersion, ProfileDigest: run.ProfileDigest, RegistryVersion: run.RegistryVersion,
		RegistryDigest: run.RegistryDigest, RepositoryBindingDigest: run.RepositoryBindingDigest, BaseBranch: run.BaseBranch,
		WorkingBranch: run.WorkingBranch, BaseSHA: run.BaseSHA, State: run.State, CandidateHead: run.CandidateHead,
		TaskHash: run.TaskHash, ImplementationModel: run.ImplementationModel, ReviewModel: run.ReviewModel}
}

type ReconcileCommand struct {
	Requester           Requester                     `json:"requester"`
	RunID               string                        `json:"run_id"`
	Repository          string                        `json:"repository"`
	ExpectedState       domain.State                  `json:"expected_state"`
	IdempotencyKey      string                        `json:"idempotency_key"`
	Evidence            domain.GitHubReadEvidence     `json:"evidence"`
	Observations        []GitHubRequestObservation    `json:"-"`
	Metadata            GitHubInstallationMetadata    `json:"-"`
	TrustedFeedback     []TrustedReviewFeedbackRecord `json:"-"`
	FeedbackUnsupported bool                          `json:"-"`
	FeedbackDrift       bool                          `json:"-"`
	LinearCompleted     bool                          `json:"-"`
}

type ReconcileResult struct {
	Head   string                      `json:"reconciled_head"`
	Status domain.ReconciliationStatus `json:"reconciliation_status"`
	State  domain.State                `json:"current_state"`
	Reason string                      `json:"-"`
}

type GitHubReconcileCommand struct {
	Requester       Requester    `json:"requester"`
	RunID           string       `json:"run_id"`
	Repository      string       `json:"repository"`
	ExpectedState   domain.State `json:"expected_state"`
	IdempotencyKey  string       `json:"idempotency_key"`
	PullRequest     int64        `json:"pull_request"`
	ExpectedHead    string       `json:"expected_head"`
	LinearCompleted bool         `json:"-"`
}

func (s CommandService) ReconcileFromGitHub(ctx context.Context, command GitHubReconcileCommand, reader GitHubReadPort) (ReconcileResult, error) {
	if reader == nil || command.PullRequest < 1 || command.ExpectedHead == "" {
		return ReconcileResult{}, serviceError(ErrorInvalidInput, "GitHub reader, pull request, and expected head are required", nil)
	}
	return s.withReconcileLease(ctx, ReconcileCommand{Requester: command.Requester, RunID: command.RunID, Repository: command.Repository,
		ExpectedState: command.ExpectedState, IdempotencyKey: command.IdempotencyKey, LinearCompleted: command.LinearCompleted}, func(leaseCtx context.Context, inspection RunInspection, owner string) (ReconcileResult, error) {
		if err := validateReconcileInspection(ReconcileCommand{Requester: command.Requester, RunID: command.RunID, Repository: command.Repository, ExpectedState: command.ExpectedState, IdempotencyKey: command.IdempotencyKey, LinearCompleted: command.LinearCompleted}, inspection); err != nil {
			return ReconcileResult{}, err
		}
		if inspection.PullRequest == nil || inspection.PullRequest.Number != command.PullRequest || inspection.Run.CandidateHead != command.ExpectedHead {
			return ReconcileResult{}, serviceError(ErrorConflict, "requested PR or head does not match persisted evidence", nil)
		}
		if err := validateReaderAuthority(inspection, reader.Authority()); err != nil {
			return ReconcileResult{}, err
		}
		evidence, handoff, observations, metadata, err := reader.Read(leaseCtx, command.PullRequest, command.ExpectedHead)
		if err != nil {
			persister, ok := s.store.(interface {
				SaveGitHubReadFailure(context.Context, string, string, domain.State, string, []GitHubRequestObservation) error
			})
			if !ok {
				return ReconcileResult{}, serviceError(ErrorInternal, "reconciliation failure persistence is unavailable", nil)
			}
			auditCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if saveErr := persister.SaveGitHubReadFailure(auditCtx, command.RunID, owner, command.ExpectedState, command.IdempotencyKey, observations); saveErr != nil {
				return ReconcileResult{}, classifyServiceError(saveErr)
			}
			return ReconcileResult{}, classifyServiceError(err)
		}
		existingFeedback := make([]domain.TrustedReviewFeedback, len(inspection.TrustedFeedback))
		for index, item := range inspection.TrustedFeedback {
			existingFeedback[index] = item.TrustedReviewFeedback
		}
		if domain.TrustedFeedbackDrift(existingFeedback, evidence.PullRequest, evidence.Reviews, evidence.ReviewThreads, handoff) {
			root, digest := trustedFeedbackDriftReference(inspection.TrustedFeedback, handoff)
			persister, ok := s.store.(TrustedReviewFeedbackDriftStore)
			if !ok {
				return ReconcileResult{}, serviceError(ErrorInternal, "trusted feedback drift persistence is unavailable", nil)
			}
			if transitionErr := persister.RequireManualInterventionForTrustedFeedbackDrift(leaseCtx, command.RunID, owner, command.ExpectedState, command.IdempotencyKey, observations, evidence.PullRequest, metadata, evidence, root, digest); transitionErr != nil {
				return ReconcileResult{}, classifyServiceError(transitionErr)
			}
			return ReconcileResult{Head: inspection.Run.CandidateHead, Status: domain.ReconciliationInfrastructure, State: domain.StateManualIntervention}, nil
		}
		if err := handoff.Validate(); err != nil {
			return ReconcileResult{}, serviceError(ErrorUnavailable, "inline review body handoff is incomplete", err)
		}
		var trusted []domain.ActorIdentity
		hasChangesRequested := false
		for _, review := range evidence.Reviews {
			if review.State == "CHANGES_REQUESTED" {
				hasChangesRequested = true
				var trustErr error
				trusted, trustErr = trustedHumanActors(inspection)
				if trustErr != nil {
					return ReconcileResult{}, serviceError(ErrorConflict, "trusted human feedback identity is unavailable", trustErr)
				}
				break
			}
		}
		normalized := domain.TrustedChangesRequested{}
		if hasChangesRequested {
			var normalizeErr error
			normalized, normalizeErr = domain.NormalizeTrustedChangesRequested(evidence.PullRequest, evidence.Reviews, evidence.ReviewThreads, handoff, trusted, evidence.ObservedAt)
			if normalizeErr != nil {
				return ReconcileResult{}, serviceError(ErrorUnavailable, "inline review evidence is incomplete", normalizeErr)
			}
		}
		feedback := make([]TrustedReviewFeedbackRecord, len(normalized.Feedback))
		for index, item := range normalized.Feedback {
			feedback[index] = TrustedReviewFeedbackRecord{RunID: command.RunID, TrustedReviewFeedback: item}
		}
		drift := domain.TrustedFeedbackDrift(existingFeedback, evidence.PullRequest, evidence.Reviews, evidence.ReviewThreads, handoff)
		full := ReconcileCommand{Requester: command.Requester, RunID: command.RunID, Repository: command.Repository, ExpectedState: command.ExpectedState,
			IdempotencyKey: command.IdempotencyKey, Evidence: evidence, Observations: observations, Metadata: metadata, TrustedFeedback: feedback, FeedbackUnsupported: normalized.Unsupported, FeedbackDrift: drift, LinearCompleted: command.LinearCompleted}
		full.Evidence.Findings = normalized.Findings
		return s.reconcileLocked(leaseCtx, full, inspection, owner)
	})
}

// reconcileMergeabilityChangesRequested reuses the ordinary trusted-feedback
// normalization and atomic repair selection while the run still holds the
// mergeability reconciliation lease. A GitHub conversation resolution wait
// must never turn a new exact-head change request into a passive approval wait.
func (s CommandService) reconcileMergeabilityChangesRequested(ctx context.Context, command ReconcileCommand, inspection RunInspection, owner string, evidence domain.GitHubReadEvidence, handoff domain.InlineReviewBodyHandoff, observations []GitHubRequestObservation, metadata GitHubInstallationMetadata) (ReconcileResult, error) {
	existingFeedback := make([]domain.TrustedReviewFeedback, len(inspection.TrustedFeedback))
	for index, item := range inspection.TrustedFeedback {
		existingFeedback[index] = item.TrustedReviewFeedback
	}
	trusted, err := trustedHumanActors(inspection)
	if err != nil {
		return ReconcileResult{}, serviceError(ErrorConflict, "trusted human feedback identity is unavailable", err)
	}
	normalized, err := domain.NormalizeTrustedChangesRequested(evidence.PullRequest, evidence.Reviews, evidence.ReviewThreads, handoff, trusted, evidence.ObservedAt)
	if err != nil {
		return ReconcileResult{}, serviceError(ErrorUnavailable, "inline review evidence is incomplete", err)
	}
	feedback := make([]TrustedReviewFeedbackRecord, len(normalized.Feedback))
	for index, item := range normalized.Feedback {
		feedback[index] = TrustedReviewFeedbackRecord{RunID: command.RunID, TrustedReviewFeedback: item}
	}
	full := ReconcileCommand{Requester: command.Requester, RunID: command.RunID, Repository: command.Repository, ExpectedState: command.ExpectedState,
		IdempotencyKey: command.IdempotencyKey, Evidence: evidence, Observations: observations, Metadata: metadata, TrustedFeedback: feedback,
		FeedbackUnsupported: normalized.Unsupported, FeedbackDrift: domain.TrustedFeedbackDrift(existingFeedback, evidence.PullRequest, evidence.Reviews, evidence.ReviewThreads, handoff)}
	full.Evidence.Findings = normalized.Findings
	return s.reconcileLocked(ctx, full, inspection, owner)
}

func trustedFeedbackDriftReference(existing []TrustedReviewFeedbackRecord, handoff domain.InlineReviewBodyHandoff) (string, string) {
	bodies := make(map[string]string, len(handoff.Comments))
	for _, body := range handoff.Comments {
		bodies[body.CommentNodeID] = body.BodyDigest
	}
	for _, feedback := range existing {
		if feedback.Lifecycle != domain.TrustedReviewFeedbackObserved && feedback.Lifecycle != domain.TrustedReviewFeedbackSelectedForRepair {
			continue
		}
		if observed := bodies[feedback.RootCommentNodeID]; observed != "" {
			return feedback.RootCommentNodeID, observed
		}
		return feedback.RootCommentNodeID, feedback.BodyDigest
	}
	return "", ""
}

func validateReaderAuthority(inspection RunInspection, authority GitHubInstallationMetadata) error {
	if inspection.Run.ProfileSnapshotVersion < 1 || inspection.Run.ProfileID == "" || inspection.Run.ProfileDigest == "" || inspection.Run.ProfileSnapshotJSON == "" || inspection.RepositoryBinding == nil {
		return serviceError(ErrorConflict, "persisted repository profile evidence is legacy-insufficient", nil)
	}
	if inspection.GitHubInstallation != nil {
		persisted := inspection.GitHubInstallation
		if authority.AppID != persisted.AppID || authority.InstallationID != persisted.InstallationID || authority.Repository.ID != persisted.Repository.ID ||
			!strings.EqualFold(authority.Repository.Owner, persisted.Repository.Owner) || !strings.EqualFold(authority.Repository.Name, persisted.Repository.Name) {
			return serviceError(ErrorConflict, "GitHub reader authority mismatch", nil)
		}
		return nil
	}
	parts := strings.Split(inspection.RepositoryBinding.CanonicalRepository, "/")
	if len(parts) != 2 || authority.AppID != inspection.RepositoryBinding.GitHubAppID || authority.InstallationID != inspection.RepositoryBinding.GitHubInstallationID ||
		authority.Repository.ID != inspection.RepositoryBinding.ExpectedRepositoryID || !strings.EqualFold(authority.Repository.Owner, parts[0]) || !strings.EqualFold(authority.Repository.Name, parts[1]) {
		return serviceError(ErrorConflict, "GitHub reader authority mismatch", nil)
	}
	return nil
}

func (s CommandService) Reconcile(ctx context.Context, command ReconcileCommand) (ReconcileResult, error) {
	return s.withReconcileLease(ctx, command, func(leaseCtx context.Context, inspection RunInspection, owner string) (ReconcileResult, error) {
		return s.reconcileLocked(leaseCtx, command, inspection, owner)
	})
}

func (s CommandService) withReconcileLease(ctx context.Context, command ReconcileCommand, apply func(context.Context, RunInspection, string) (ReconcileResult, error)) (ReconcileResult, error) {
	if command.RunID == "" || command.Repository == "" || command.ExpectedState == "" || command.IdempotencyKey == "" {
		return ReconcileResult{}, serviceError(ErrorInvalidInput, "run, expected state, repository, and idempotency key are required", nil)
	}
	preflightRun, err := s.store.GetRun(ctx, command.RunID)
	if err != nil {
		return ReconcileResult{}, classifyServiceError(err)
	}
	if err := authorizePersistedRequester(preflightRun, command.Requester); err != nil {
		return ReconcileResult{}, err
	}
	if preflightRun.Repository != command.Repository || preflightRun.State != command.ExpectedState || preflightRun.IdempotencyKey != command.IdempotencyKey {
		return ReconcileResult{}, serviceError(ErrorConflict, "run authority or state changed before reconciliation", nil)
	}
	owner, err := randomIdentifier("reconcile-")
	if err != nil {
		return ReconcileResult{}, classifyServiceError(err)
	}
	acquired, err := s.store.AcquireLease(ctx, command.RunID, owner, time.Now().UTC().Add(reconcileLeaseTTL))
	if err != nil {
		return ReconcileResult{}, classifyServiceError(err)
	}
	if !acquired {
		return ReconcileResult{}, serviceError(ErrorConflict, "run is actively leased", nil)
	}
	leaseCtx, cancelLease := context.WithCancelCause(ctx)
	stopLease := make(chan struct{})
	leaseDone := make(chan struct{})
	go func() {
		defer close(leaseDone)
		ticker := time.NewTicker(reconcileLeaseTTL / 3)
		defer ticker.Stop()
		for {
			select {
			case <-stopLease:
				return
			case <-leaseCtx.Done():
				return
			case <-ticker.C:
				ok, renewErr := s.store.RenewLease(context.Background(), command.RunID, owner, time.Now().UTC().Add(reconcileLeaseTTL))
				if renewErr != nil {
					cancelLease(fmt.Errorf("renew run lease: %w", renewErr))
					return
				}
				if !ok {
					cancelLease(errors.New("run lease ownership was lost"))
					return
				}
			}
		}
	}()
	defer func() {
		close(stopLease)
		cancelLease(nil)
		<-leaseDone
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.store.ReleaseLease(releaseCtx, command.RunID, owner)
	}()
	inspection, err := s.store.Inspect(leaseCtx, command.RunID)
	if err != nil {
		return ReconcileResult{}, classifyServiceError(err)
	}
	result, err := apply(leaseCtx, inspection, owner)
	if err == nil && context.Cause(leaseCtx) != nil {
		return ReconcileResult{}, classifyServiceError(context.Cause(leaseCtx))
	}
	return result, err
}

func (s CommandService) reconcileLocked(ctx context.Context, command ReconcileCommand, inspection RunInspection, owner string) (ReconcileResult, error) {
	run := inspection.Run
	if err := validateReconcileInspection(command, inspection); err != nil {
		return ReconcileResult{}, err
	}
	var expectedRepository domain.RepositoryIdentity
	if inspection.GitHubInstallation != nil {
		expectedRepository = inspection.GitHubInstallation.Repository
	} else if inspection.RepositoryBinding != nil {
		parts := strings.Split(inspection.RepositoryBinding.CanonicalRepository, "/")
		if len(parts) != 2 {
			return ReconcileResult{}, serviceError(ErrorConflict, "persisted repository identity is invalid", nil)
		}
		expectedRepository = domain.RepositoryIdentity{ID: inspection.RepositoryBinding.ExpectedRepositoryID, Owner: parts[0], Name: parts[1]}
	} else {
		return ReconcileResult{}, serviceError(ErrorConflict, "persisted repository authority is required", nil)
	}
	if err := ReconcileGitHubRead(expectedRepository, *inspection.PullRequest, run.WorkingBranch, run.BaseBranch, run.CandidateHead, run.BaseSHA, run.IdempotencyKey, inspection.PullRequest.BodyDigest, command.Evidence); err != nil {
		return ReconcileResult{}, serviceError(ErrorConflict, "external evidence does not match the expected run", err)
	}
	if inspection.GitHubInstallation != nil {
		persisted := inspection.GitHubInstallation
		if command.Metadata.AppID != persisted.AppID || command.Metadata.InstallationID != persisted.InstallationID || command.Metadata.Repository != persisted.Repository {
			return ReconcileResult{}, serviceError(ErrorConflict, "GitHub installation authority mismatch", nil)
		}
	} else if inspection.RepositoryBinding == nil || command.Metadata.AppID != inspection.RepositoryBinding.GitHubAppID || command.Metadata.InstallationID != inspection.RepositoryBinding.GitHubInstallationID || command.Metadata.Repository != command.Evidence.Repository {
		return ReconcileResult{}, serviceError(ErrorConflict, "GitHub installation authority mismatch", nil)
	}
	status := command.Evidence.DeliveryStatus()
	var approvalObservation *domain.HumanApprovalObservation
	var approval *domain.HumanApproval
	if run.State == domain.StateAwaitingHumanApproval {
		trusted, err := trustedHumanActors(inspection)
		if err != nil {
			return ReconcileResult{}, serviceError(ErrorConflict, "trusted human approval identity is unavailable", err)
		}
		observed, normalized, err := domain.NormalizeHumanApproval(command.Evidence.PullRequest, command.Evidence.Reviews, trusted, command.Evidence.ObservedAt)
		if err != nil {
			return ReconcileResult{}, serviceError(ErrorConflict, "human approval evidence is ambiguous", err)
		}
		approvalObservation, approval = &observed, normalized
	}
	if status == domain.ReconciliationActionable {
		findings, _, err := repairableEvidenceFindings(command.Evidence, run.CandidateHead, selectedFeedbackForAdmission(command.TrustedFeedback))
		if err == nil {
			command.Evidence.Findings = findings
		}
	}
	next, reason := nextGitHubReconciliationState(run.State, command.Evidence, status)
	if command.FeedbackUnsupported || command.FeedbackDrift || trustedFeedbackConflicts(inspection.TrustedFeedback, command.TrustedFeedback) {
		next, reason = domain.StateManualIntervention, "trusted inline change request requires manual intervention"
	}
	if command.LinearCompleted && !command.Evidence.PullRequest.Merged {
		next, reason = domain.StateManualIntervention, "Linear issue completed before a controller-owned merge was observed"
	}
	if len(command.TrustedFeedback) > 0 && next != domain.StateManualIntervention && (run.State == domain.StateReconcilingReviews || run.State == domain.StateAwaitingHumanApproval || run.State == domain.StateAwaitingGitHubMergeability) {
		next, reason = domain.StateRepairing, "trusted exact-head inline change request requires repair"
	}
	if status == domain.ReconciliationActionable && next == domain.StateRepairing {
		if _, _, err := repairableEvidenceFindings(command.Evidence, run.CandidateHead, selectedFeedbackForAdmission(command.TrustedFeedback)); err != nil {
			next = domain.StateManualIntervention
			reason = "GitHub evidence has unsupported actionable findings"
		}
	}
	if next != domain.StateManualIntervention && shouldEnterReviewReply(run.State, status, inspection.TrustedFeedback, run.CandidateHead) {
		next, reason = domain.StateReplyingReviewFeedback, "verified trusted inline repair requires a controller reply"
	}
	if !command.LinearCompleted && next != domain.StateReplyingReviewFeedback && run.State == domain.StateAwaitingHumanApproval && status == domain.ReconciliationPass && approvalObservation != nil && approvalObservation.Status == domain.HumanApprovalApproved && approval != nil {
		if err := approval.Authorizes(command.Evidence.PullRequest, run.CandidateHead); err != nil {
			return ReconcileResult{}, serviceError(ErrorConflict, "human approval is not bound to the exact final head", err)
		}
		next, reason = domain.StateMerging, "trusted human approval is bound to the exact final head"
	}
	persister, ok := s.store.(interface {
		SaveGitHubReadSuccess(context.Context, string, string, domain.State, string, []GitHubRequestObservation, domain.PullRequest, GitHubInstallationMetadata, domain.GitHubReadEvidence, []TrustedReviewFeedbackRecord, *domain.HumanApprovalObservation, *domain.HumanApproval, domain.State, string) error
	})
	if !ok {
		return ReconcileResult{}, serviceError(ErrorInternal, "reconciliation persistence is unavailable", nil)
	}
	if err := persister.SaveGitHubReadSuccess(ctx, run.ID, owner, command.ExpectedState, command.IdempotencyKey, command.Observations, command.Evidence.PullRequest, command.Metadata, command.Evidence, command.TrustedFeedback, approvalObservation, approval, next, reason); err != nil {
		return ReconcileResult{}, classifyServiceError(err)
	}
	return ReconcileResult{Head: run.CandidateHead, Status: status, State: next, Reason: reason}, nil
}

func trustedFeedbackConflicts(existing, observed []TrustedReviewFeedbackRecord) bool {
	byRoot := make(map[string]TrustedReviewFeedbackRecord, len(existing))
	for _, item := range existing {
		byRoot[item.RootCommentNodeID] = item
	}
	for _, item := range observed {
		if prior, found := byRoot[item.RootCommentNodeID]; found && !prior.ImmutableEqual(item.TrustedReviewFeedback) {
			return true
		}
	}
	return false
}

func selectedFeedbackForAdmission(observed []TrustedReviewFeedbackRecord) []TrustedReviewFeedbackRecord {
	result := append([]TrustedReviewFeedbackRecord(nil), observed...)
	for index := range result {
		result[index].Lifecycle = domain.TrustedReviewFeedbackSelectedForRepair
	}
	return result
}

func hasOutstandingReviewReply(items []TrustedReviewFeedbackRecord, head string) bool {
	for _, item := range items {
		if (item.Lifecycle == domain.TrustedReviewFeedbackRepairVerified || item.Lifecycle == domain.TrustedReviewFeedbackReplyPending) && !item.Resolved && item.BoundRepairHead == head {
			return true
		}
	}
	return false
}

func shouldEnterReviewReply(current domain.State, status domain.ReconciliationStatus, items []TrustedReviewFeedbackRecord, head string) bool {
	return status == domain.ReconciliationPass && domain.CanTransition(current, domain.StateReplyingReviewFeedback) && hasOutstandingReviewReply(items, head)
}

func trustedHumanActors(inspection RunInspection) ([]domain.ActorIdentity, error) {
	if inspection.RepositoryBinding == nil || len(inspection.RepositoryBinding.TrustedOperatorActors) == 0 {
		return nil, errors.New("persisted repository profile has no trusted human actor")
	}
	actors := make([]domain.ActorIdentity, 0, len(inspection.RepositoryBinding.TrustedOperatorActors))
	for _, actor := range inspection.RepositoryBinding.TrustedOperatorActors {
		actors = append(actors, domain.ActorIdentity{DatabaseID: actor.DatabaseID, NodeID: actor.NodeID, Login: actor.Login, Type: actor.Type})
	}
	return actors, nil
}

func nextGitHubReconciliationState(current domain.State, evidence domain.GitHubReadEvidence, status domain.ReconciliationStatus) (domain.State, string) {
	if current != domain.StatePROpen && current != domain.StateReconcilingReviews && current != domain.StateAwaitingHumanApproval && current != domain.StateAwaitingGitHubMergeability {
		return current, "GitHub evidence recorded outside the production delivery gate"
	}
	if !strings.EqualFold(evidence.PullRequest.State, "open") || evidence.PullRequest.Merged {
		return domain.StateManualIntervention, "GitHub pull request closed or merged outside the controller gate"
	}
	if current == domain.StatePROpen {
		return domain.StateReconcilingReviews, "GitHub evidence collection started"
	}
	switch status {
	case domain.ReconciliationPass:
		return domain.StateAwaitingHumanApproval, "required checks passed"
	case domain.ReconciliationActionable:
		return domain.StateRepairing, "GitHub evidence has actionable review or check findings"
	case domain.ReconciliationPending, domain.ReconciliationInfrastructure:
		return domain.StateReconcilingReviews, "GitHub evidence is pending or incomplete"
	default:
		return domain.StateReconcilingReviews, "GitHub evidence has an unknown reconciliation status"
	}
}

func validateReconcileInspection(command ReconcileCommand, inspection RunInspection) error {
	run := inspection.Run
	if err := authorizePersistedRequester(run, command.Requester); err != nil {
		return err
	}
	if command.Repository == "" || run.Repository != command.Repository || command.ExpectedState == "" || run.State != command.ExpectedState || command.IdempotencyKey == "" || run.IdempotencyKey != command.IdempotencyKey {
		return serviceError(ErrorConflict, "run authority or state changed before reconciliation", nil)
	}
	if inspection.PullRequest == nil {
		return serviceError(ErrorConflict, "persisted pull request identity is required", nil)
	}
	return nil
}
