package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

func (r Requester) authorize(allowed []string) error {
	if r.ID == "" || r.Kind != "github_login" {
		return serviceError(ErrorInvalidInput, "requester identity is required", nil)
	}
	if !slices.Contains(allowed, r.ID) {
		return serviceError(ErrorConflict, "requester is not authorized for the repository", nil)
	}
	return nil
}

// AuthorizeRequester is the minimal adapter preflight for controller-owned
// authority data that must be loaded before other untrusted inputs are read.
func AuthorizeRequester(requester Requester, allowed []string) error {
	return requester.authorize(allowed)
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
	RunID                   string       `json:"run_id"`
	IssueID                 string       `json:"issue_id"`
	Repository              string       `json:"repository"`
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
		if existing.TaskHash != command.Input.TaskHash || existing.Repository != command.RepositorySelection {
			return CommandResult{}, serviceError(ErrorConflict, "idempotency key belongs to a different run authority", nil)
		}
		run, err := s.controller.ContinueExpected(ctx, existing.ID, existing.State, existing.IdempotencyKey, nil)
		if err != nil {
			return CommandResult{}, classifyServiceError(err)
		}
		return CommandResult{Run: projectRunResult(run)}, nil
	}
	if err := command.Requester.authorize(command.Input.Repository.AllowedOperatorLogins); err != nil {
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

type QueryInput struct {
	Requester  Requester `json:"requester"`
	RunID      string    `json:"run_id"`
	Repository string    `json:"repository"`
}

type QueryService struct{ store RunStore }

func NewQueryService(store RunStore) QueryService { return QueryService{store: store} }

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
	return projectInspection(inspection), nil
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
	return requester.authorize(repository.AllowedOperatorLogins)
}

type InspectionResult struct {
	Run               RunResult                   `json:"run"`
	RepositoryBinding *SanitizedRepositoryBinding `json:"repository_binding,omitempty"`
	Timeline          []TransitionResult          `json:"state_timeline"`
	Attempts          []AttemptResult             `json:"attempts"`
	Verifications     []VerificationResult        `json:"verifications"`
	Reviews           []ReviewResult              `json:"reviews"`
	Resources         []ResourceResult            `json:"owned_resources"`
	PullRequest       *PullRequestResult          `json:"pull_request,omitempty"`
	Approval          *domain.HumanApproval       `json:"human_approval,omitempty"`
	Merge             *MergeRecord                `json:"merge_result,omitempty"`
	Cleanup           []CleanupResult             `json:"cleanup_progress"`
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
	Number         int       `json:"number"`
	Kind           string    `json:"kind"`
	Status         string    `json:"status"`
	RequestedModel string    `json:"requested_model"`
	ErrorCategory  string    `json:"error_category"`
	StartedAt      time.Time `json:"started_at"`
	FinishedAt     time.Time `json:"finished_at,omitempty"`
	ExitCode       int       `json:"exit_code"`
	OutcomeHash    string    `json:"outcome_hash"`
}
type VerificationResult struct {
	VerifierID   string    `json:"verifier_id"`
	Phase        string    `json:"phase"`
	VerifiedHead string    `json:"verified_head"`
	ExitCode     int       `json:"exit_code"`
	EvidenceHash string    `json:"evidence_hash"`
	CreatedAt    time.Time `json:"timestamp"`
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
	Kind      string    `json:"resource_kind"`
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updated_at"`
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
	result := InspectionResult{Run: projectRunResult(value.Run), RepositoryBinding: value.RepositoryBinding, Approval: value.Approval, Merge: value.Merge}
	if value.PullRequest != nil {
		v := value.PullRequest
		result.PullRequest = &PullRequestResult{v.Number, v.URL, v.HeadBranch, v.BaseBranch, v.HeadSHA, v.BaseSHA, v.State, v.Merged, v.MergeSHA, v.MergedAt}
	}
	for _, v := range value.Timeline {
		result.Timeline = append(result.Timeline, TransitionResult{v.Sequence, v.From, v.To, v.Reason, v.BoundHead, v.CreatedAt})
	}
	for _, v := range value.Attempts {
		result.Attempts = append(result.Attempts, AttemptResult{v.Number, v.Kind, v.Status, v.RequestedModel, v.ErrorCategory, v.StartedAt, v.FinishedAt, v.ExitCode, v.OutcomeHash})
	}
	for _, v := range value.Verifications {
		result.Verifications = append(result.Verifications, VerificationResult{v.VerifierID, v.Phase, v.VerifiedHead, v.ExitCode, v.EvidenceHash, v.CreatedAt})
	}
	for _, v := range value.Reviews {
		result.Reviews = append(result.Reviews, ReviewResult{v.ReviewedHead, v.Verdict, v.OutcomeHash, v.CreatedAt})
	}
	for _, v := range value.Resources {
		result.Resources = append(result.Resources, ResourceResult{v.Kind, v.Status, v.CreatedAt})
	}
	for _, v := range value.Cleanup {
		result.Cleanup = append(result.Cleanup, CleanupResult{v.Kind, v.Status, v.UpdatedAt})
	}
	return result
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
	return RunResult{RunID: run.ID, IssueID: run.IssueID, Repository: run.Repository, RegistryVersion: run.RegistryVersion,
		RegistryDigest: run.RegistryDigest, RepositoryBindingDigest: run.RepositoryBindingDigest, BaseBranch: run.BaseBranch,
		WorkingBranch: run.WorkingBranch, BaseSHA: run.BaseSHA, State: run.State, CandidateHead: run.CandidateHead,
		TaskHash: run.TaskHash, ImplementationModel: run.ImplementationModel, ReviewModel: run.ReviewModel}
}

type ReconcileCommand struct {
	Requester      Requester                  `json:"requester"`
	RunID          string                     `json:"run_id"`
	Repository     string                     `json:"repository"`
	ExpectedState  domain.State               `json:"expected_state"`
	IdempotencyKey string                     `json:"idempotency_key"`
	Evidence       domain.GitHubReadEvidence  `json:"evidence"`
	Observations   []GitHubRequestObservation `json:"-"`
	Metadata       GitHubInstallationMetadata `json:"-"`
}

type ReconcileResult struct {
	Head string `json:"reconciled_head"`
}

type GitHubReconcileCommand struct {
	Requester      Requester    `json:"requester"`
	RunID          string       `json:"run_id"`
	Repository     string       `json:"repository"`
	ExpectedState  domain.State `json:"expected_state"`
	IdempotencyKey string       `json:"idempotency_key"`
	PullRequest    int64        `json:"pull_request"`
	ExpectedHead   string       `json:"expected_head"`
}

func (s CommandService) ReconcileFromGitHub(ctx context.Context, command GitHubReconcileCommand, reader GitHubReadPort) (ReconcileResult, error) {
	if reader == nil || command.PullRequest < 1 || command.ExpectedHead == "" {
		return ReconcileResult{}, serviceError(ErrorInvalidInput, "GitHub reader, pull request, and expected head are required", nil)
	}
	return s.withReconcileLease(ctx, ReconcileCommand{Requester: command.Requester, RunID: command.RunID, Repository: command.Repository,
		ExpectedState: command.ExpectedState, IdempotencyKey: command.IdempotencyKey}, func(leaseCtx context.Context, inspection RunInspection, owner string) (ReconcileResult, error) {
		if err := validateReconcileInspection(ReconcileCommand{Requester: command.Requester, RunID: command.RunID, Repository: command.Repository, ExpectedState: command.ExpectedState, IdempotencyKey: command.IdempotencyKey}, inspection); err != nil {
			return ReconcileResult{}, err
		}
		if inspection.PullRequest == nil || inspection.PullRequest.Number != command.PullRequest || inspection.Run.CandidateHead != command.ExpectedHead {
			return ReconcileResult{}, serviceError(ErrorConflict, "requested PR or head does not match persisted evidence", nil)
		}
		evidence, observations, metadata, err := reader.Read(leaseCtx, command.PullRequest, command.ExpectedHead)
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
		full := ReconcileCommand{Requester: command.Requester, RunID: command.RunID, Repository: command.Repository, ExpectedState: command.ExpectedState,
			IdempotencyKey: command.IdempotencyKey, Evidence: evidence, Observations: observations, Metadata: metadata}
		return s.reconcileLocked(leaseCtx, full, inspection, owner)
	})
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
	} else if inspection.RepositoryBinding == nil || command.Metadata.AppID < 1 || command.Metadata.InstallationID != inspection.RepositoryBinding.GitHubInstallationID || command.Metadata.Repository != command.Evidence.Repository {
		return ReconcileResult{}, serviceError(ErrorConflict, "GitHub installation authority mismatch", nil)
	}
	persister, ok := s.store.(interface {
		SaveGitHubReadSuccess(context.Context, string, string, domain.State, string, []GitHubRequestObservation, domain.PullRequest, GitHubInstallationMetadata, domain.GitHubReadEvidence) error
	})
	if !ok {
		return ReconcileResult{}, serviceError(ErrorInternal, "reconciliation persistence is unavailable", nil)
	}
	if err := persister.SaveGitHubReadSuccess(ctx, run.ID, owner, command.ExpectedState, command.IdempotencyKey, command.Observations, command.Evidence.PullRequest, command.Metadata, command.Evidence); err != nil {
		return ReconcileResult{}, classifyServiceError(err)
	}
	return ReconcileResult{Head: run.CandidateHead}, nil
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
