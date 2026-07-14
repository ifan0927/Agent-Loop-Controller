package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type PullRequestOpenRequest struct {
	Title        string
	HeadBranch   string
	BaseBranch   string
	CandidateSHA string
	BaseSHA      string
	Body         string
	BodyDigest   string
	OwnershipKey string
}

func (r PullRequestOpenRequest) Validate() error {
	if strings.TrimSpace(r.Title) == "" || strings.TrimSpace(r.CandidateSHA) == "" || strings.TrimSpace(r.BaseSHA) == "" || strings.TrimSpace(r.Body) == "" || strings.TrimSpace(r.BodyDigest) == "" || strings.TrimSpace(r.OwnershipKey) == "" {
		return errors.New("complete pull request intent is required")
	}
	if err := domain.ValidateGitBranch(r.HeadBranch); err != nil {
		return err
	}
	if err := domain.ValidateGitBranch(r.BaseBranch); err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(r.Body))
	if r.BodyDigest != hex.EncodeToString(digest[:]) {
		return errors.New("pull request body digest does not match body")
	}
	return nil
}

// PullRequestOpener is deliberately not a generic GitHub write port. Its only
// permitted external operation is create-or-adopt for one immutable PR intent.
type PullRequestOpener interface {
	OpenPullRequest(context.Context, PullRequestOpenRequest) (domain.PullRequest, error)
}

type ProductionOpenPullRequestCommand struct {
	Requester      Requester
	RunID          string
	Repository     string
	ExpectedState  domain.State
	IdempotencyKey string
}

type ProductionOpenPullRequestResult struct {
	Action      ProductionAction `json:"action"`
	Run         RunResult        `json:"run"`
	PullRequest int64            `json:"pull_request"`
	Idempotent  bool             `json:"idempotent"`
}

type pullRequestStore interface {
	BeginSideEffect(context.Context, SideEffectRecord) (SideEffectRecord, bool, error)
	FinishSideEffect(context.Context, SideEffectRecord) error
	SavePullRequest(context.Context, string, domain.PullRequest) error
}

// OpenPullRequest persists an immutable create intent before any GitHub write.
// On restart, a stored PR is revalidated locally; otherwise the adapter first
// searches for the exact ownership marker and digest before creating anything.
func (c *ProductionCoordinator) OpenPullRequest(ctx context.Context, command ProductionOpenPullRequestCommand, validator ApprovalValidator, opener PullRequestOpener) (ProductionOpenPullRequestResult, error) {
	if validator == nil || opener == nil {
		return ProductionOpenPullRequestResult{}, serviceError(ErrorInvalidInput, "pull request validator and opener are required", nil)
	}
	run, err := c.admission.Revalidate(ctx, LinearRevalidateCommand{Requester: command.Requester, RunID: command.RunID, Repository: command.Repository, ExpectedState: command.ExpectedState, IdempotencyKey: command.IdempotencyKey})
	if err != nil {
		return ProductionOpenPullRequestResult{}, err
	}
	action, reason := productionNextAction(run.State)
	if action != ProductionOpenPullRequest {
		return ProductionOpenPullRequestResult{Action: action, Run: projectRunResult(run)}, serviceError(ErrorConflict, reason, nil)
	}
	if err := validator.ValidateApprovalReady(ctx, run.ID); err != nil {
		return ProductionOpenPullRequestResult{}, serviceError(ErrorConflict, "exact-HEAD approval evidence is no longer valid", err)
	}
	request, err := pullRequestIntent(run)
	if err != nil {
		return ProductionOpenPullRequestResult{}, serviceError(ErrorInternal, "build immutable pull request intent", err)
	}
	if run.State == domain.StateBranchPushed {
		if err := c.store.Transition(ctx, run.ID, domain.StateBranchPushed, domain.StateOpeningPR, "persist pull request intent", "exact pushed candidate", run.CandidateHead); err != nil {
			return ProductionOpenPullRequestResult{}, classifyServiceError(err)
		}
		run, err = c.store.GetRun(ctx, run.ID)
		if err != nil {
			return ProductionOpenPullRequestResult{}, classifyServiceError(err)
		}
	}
	effects, ok := c.store.(pullRequestStore)
	if !ok {
		return ProductionOpenPullRequestResult{}, serviceError(ErrorInternal, "configured store cannot persist pull request evidence", nil)
	}
	intentJSON, err := json.Marshal(map[string]any{"repository_id": repositoryID(run), "installation_id": installationID(run), "head_branch": request.HeadBranch, "base_branch": request.BaseBranch, "candidate_sha": request.CandidateSHA, "base_sha": request.BaseSHA, "body_digest": request.BodyDigest, "ownership_key": request.OwnershipKey})
	if err != nil {
		return ProductionOpenPullRequestResult{}, serviceError(ErrorInternal, "encode pull request intent", err)
	}
	side, _, err := effects.BeginSideEffect(ctx, SideEffectRecord{RunID: run.ID, Kind: "open_pull_request", IdempotencyKey: run.CandidateHead, IntentJSON: string(intentJSON), Attempt: 1})
	if err != nil {
		return ProductionOpenPullRequestResult{}, classifyServiceError(err)
	}
	inspection, err := c.store.Inspect(ctx, run.ID)
	if err != nil {
		return ProductionOpenPullRequestResult{}, classifyServiceError(err)
	}
	if inspection.PullRequest != nil {
		if err := validateRetainedOwnedPullRequest(*inspection.PullRequest, run); err != nil {
			return ProductionOpenPullRequestResult{}, c.rejectPullRequestConflict(ctx, run, effects, side, "persisted_pull_request_mismatch", err)
		}
		return c.completePullRequest(ctx, run, effects, side, *inspection.PullRequest, true)
	}
	pr, err := opener.OpenPullRequest(ctx, request)
	if err != nil {
		return ProductionOpenPullRequestResult{}, c.recordPullRequestFailure(ctx, run, effects, side, "open_or_adopt_failed", err)
	}
	if err := validateOpenedPullRequest(pr, request); err != nil {
		return ProductionOpenPullRequestResult{}, c.rejectPullRequestConflict(ctx, run, effects, side, "pull_request_mismatch", err)
	}
	return c.completePullRequest(ctx, run, effects, side, pr, false)
}

func validateRetainedOwnedPullRequest(pr domain.PullRequest, run Run) error {
	if pr.DatabaseID < 1 || strings.TrimSpace(pr.URL) == "" || !strings.EqualFold(pr.State, "open") || pr.Merged {
		return errors.New("persisted pull request is incomplete or not open")
	}
	if err := pr.ValidateOwnership(run.WorkingBranch, run.BaseBranch, run.CandidateHead, run.IdempotencyKey); err != nil {
		return err
	}
	if pr.BaseSHA != run.BaseSHA {
		return errors.New("persisted pull request base SHA does not match the run")
	}
	return nil
}

func pullRequestIntent(run Run) (PullRequestOpenRequest, error) {
	task, err := decodeTaskSnapshot(run.NormalizedTaskJSON)
	if err != nil {
		return PullRequestOpenRequest{}, err
	}
	if err := task.Validate(); err != nil {
		return PullRequestOpenRequest{}, err
	}
	body, err := domain.PRBody(task, "Controller verification passed for exact candidate "+run.CandidateHead+".", "Fresh "+run.ReviewModel+" review passed for exact candidate "+run.CandidateHead+".", run.IdempotencyKey)
	if err != nil {
		return PullRequestOpenRequest{}, err
	}
	digest := sha256.Sum256([]byte(body))
	request := PullRequestOpenRequest{Title: task.IssueID + ": " + task.Title, HeadBranch: run.WorkingBranch, BaseBranch: run.BaseBranch, CandidateSHA: run.CandidateHead, BaseSHA: run.BaseSHA, Body: body, BodyDigest: hex.EncodeToString(digest[:]), OwnershipKey: run.IdempotencyKey}
	return request, request.Validate()
}

func validateOpenedPullRequest(pr domain.PullRequest, request PullRequestOpenRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if pr.DatabaseID < 1 || strings.TrimSpace(pr.URL) == "" || !strings.EqualFold(pr.State, "open") || pr.Merged {
		return errors.New("pull request response is incomplete or not open")
	}
	if err := pr.ValidateOwnership(request.HeadBranch, request.BaseBranch, request.CandidateSHA, request.OwnershipKey); err != nil {
		return err
	}
	if pr.BaseSHA != request.BaseSHA || pr.BodyDigest != request.BodyDigest {
		return errors.New("pull request response does not match immutable intent")
	}
	return nil
}

func (c *ProductionCoordinator) completePullRequest(ctx context.Context, run Run, effects pullRequestStore, side SideEffectRecord, pr domain.PullRequest, idempotent bool) (ProductionOpenPullRequestResult, error) {
	if err := effects.SavePullRequest(ctx, run.ID, pr); err != nil {
		return ProductionOpenPullRequestResult{}, c.recordPullRequestFailure(ctx, run, effects, side, "persist_pull_request_failed", err)
	}
	result, err := json.Marshal(map[string]any{"pull_request": pr.Number, "database_id": pr.DatabaseID, "node_id": pr.NodeID, "head_sha": pr.HeadSHA, "base_sha": pr.BaseSHA, "body_digest": pr.BodyDigest})
	if err != nil {
		return ProductionOpenPullRequestResult{}, serviceError(ErrorInternal, "encode pull request result", err)
	}
	if side.Status != "observed" {
		side.Status, side.ResultJSON, side.ObservedAt = "observed", string(result), time.Now().UTC()
		if err := effects.FinishSideEffect(ctx, side); err != nil {
			return ProductionOpenPullRequestResult{}, classifyServiceError(err)
		}
	}
	if err := c.store.AddOwnedResource(ctx, OwnedResource{RunID: run.ID, Kind: "pull_request", Name: fmt.Sprint(pr.Number), CreationEvidence: "open_pull_request:" + fmt.Sprint(side.ID), Status: "owned"}); err != nil {
		return ProductionOpenPullRequestResult{}, classifyServiceError(err)
	}
	if err := c.store.Transition(ctx, run.ID, domain.StateOpeningPR, domain.StatePROpen, "owned pull request observed", fmt.Sprint(pr.Number), run.CandidateHead); err != nil {
		return ProductionOpenPullRequestResult{}, classifyServiceError(err)
	}
	next, err := c.store.GetRun(ctx, run.ID)
	if err != nil {
		return ProductionOpenPullRequestResult{}, classifyServiceError(err)
	}
	return ProductionOpenPullRequestResult{Action: ProductionReconcileGitHub, Run: projectRunResult(next), PullRequest: pr.Number, Idempotent: idempotent}, nil
}

func (c *ProductionCoordinator) recordPullRequestFailure(ctx context.Context, run Run, effects pullRequestStore, side SideEffectRecord, category string, cause error) error {
	result, _ := json.Marshal(map[string]string{"category": category})
	if side.Status != "observed" {
		side.Status, side.ResultJSON, side.ObservedAt = "failed", string(result), time.Now().UTC()
		if err := effects.FinishSideEffect(ctx, side); err != nil {
			return classifyServiceError(err)
		}
	}
	_ = c.store.SetLastError(ctx, run.ID, "pull request creation requires reconciliation before retry")
	return serviceError(ErrorUnavailable, "pull request was not observed; retry must reconcile immutable intent", cause)
}

func (c *ProductionCoordinator) rejectPullRequestConflict(ctx context.Context, run Run, effects pullRequestStore, side SideEffectRecord, category string, cause error) error {
	result, _ := json.Marshal(map[string]string{"category": category})
	if side.Status != "observed" {
		side.Status, side.ResultJSON, side.ObservedAt = "failed", string(result), time.Now().UTC()
		if err := effects.FinishSideEffect(ctx, side); err != nil {
			return classifyServiceError(err)
		}
	}
	_ = c.store.SetLastError(ctx, run.ID, "pull request evidence conflicts with immutable intent")
	if err := c.store.Transition(ctx, run.ID, domain.StateOpeningPR, domain.StateManualIntervention, "pull request evidence conflict", "open_pull_request", run.CandidateHead); err != nil {
		return classifyServiceError(err)
	}
	return serviceError(ErrorConflict, "pull request evidence conflicts with immutable intent", cause)
}

func repositoryID(run Run) int64 {
	var binding LocalRepository
	if json.Unmarshal([]byte(run.RepositoryConfigJSON), &binding) != nil {
		return 0
	}
	return binding.ExpectedRepositoryID
}

func installationID(run Run) int64 {
	var binding LocalRepository
	if json.Unmarshal([]byte(run.RepositoryConfigJSON), &binding) != nil {
		return 0
	}
	return binding.GitHubInstallationID
}
