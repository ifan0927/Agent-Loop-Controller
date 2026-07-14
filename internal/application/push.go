package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

// ApprovalValidator rechecks local, exact-HEAD approval evidence immediately
// before a production publisher can touch the remote.
type ApprovalValidator interface {
	ValidateApprovalReady(context.Context, string) error
}

// BranchPublisher is the narrow production Git boundary. Its implementation
// must only push the supplied branch/candidate pair to the owned origin.
type BranchPublisher interface {
	RemoteSHA(context.Context, string, string) (string, error)
	Push(context.Context, string, string, string, string, string) (PushEvidence, error)
}

type PushEvidence struct {
	RemoteRef  string
	SHA        string
	ExitCode   int
	StdoutPath string
	StderrPath string
}

type ProductionPushCommand struct {
	Requester      Requester
	RunID          string
	Repository     string
	ExpectedState  domain.State
	IdempotencyKey string
}

type ProductionPushResult struct {
	Action     ProductionAction `json:"action"`
	Run        RunResult        `json:"run"`
	RemoteSHA  string           `json:"remote_sha,omitempty"`
	Idempotent bool             `json:"idempotent"`
}

type pushStore interface {
	BeginSideEffect(context.Context, SideEffectRecord) (SideEffectRecord, bool, error)
	FinishSideEffect(context.Context, SideEffectRecord) error
	SavePullRequest(context.Context, string, domain.PullRequest) error
}

// Push publishes only a revalidated, exact-HEAD candidate. Its durable intent
// is committed before Git is invoked; retries reconcile the remote before any
// new push and therefore never overwrite a divergent branch.
func (c *ProductionCoordinator) Push(ctx context.Context, command ProductionPushCommand, validator ApprovalValidator, publisher BranchPublisher) (ProductionPushResult, error) {
	if validator == nil || publisher == nil {
		return ProductionPushResult{}, serviceError(ErrorInvalidInput, "push validator and publisher are required", nil)
	}
	run, err := c.admission.Revalidate(ctx, LinearRevalidateCommand{Requester: command.Requester, RunID: command.RunID, Repository: command.Repository, ExpectedState: command.ExpectedState, IdempotencyKey: command.IdempotencyKey})
	if err != nil {
		return ProductionPushResult{}, err
	}
	action, reason := productionNextAction(run.State)
	if action != ProductionPush {
		return ProductionPushResult{Action: action, Run: projectRunResult(run)}, serviceError(ErrorConflict, reason, nil)
	}
	if err := validator.ValidateApprovalReady(ctx, run.ID); err != nil {
		return ProductionPushResult{}, serviceError(ErrorConflict, "exact-HEAD approval evidence is no longer valid", err)
	}
	if run.State == domain.StateApprovalReady {
		if err := c.store.Transition(ctx, run.ID, domain.StateApprovalReady, domain.StatePushingBranch, "persist push intent", "verified candidate branch", run.CandidateHead); err != nil {
			return ProductionPushResult{}, classifyServiceError(err)
		}
		run, err = c.store.GetRun(ctx, run.ID)
		if err != nil {
			return ProductionPushResult{}, classifyServiceError(err)
		}
	}
	effects, ok := c.store.(pushStore)
	if !ok {
		return ProductionPushResult{}, serviceError(ErrorInternal, "configured store cannot persist push evidence", nil)
	}
	intent, err := json.Marshal(map[string]string{"remote_ref": "refs/heads/" + run.WorkingBranch, "candidate_sha": run.CandidateHead})
	if err != nil {
		return ProductionPushResult{}, serviceError(ErrorInternal, "encode push intent", err)
	}
	side, _, err := effects.BeginSideEffect(ctx, SideEffectRecord{RunID: run.ID, Kind: "push", IdempotencyKey: run.CandidateHead, IntentJSON: string(intent), Attempt: 1})
	if err != nil {
		return ProductionPushResult{}, classifyServiceError(err)
	}

	inspection, err := c.store.Inspect(ctx, run.ID)
	if err != nil {
		return ProductionPushResult{}, classifyServiceError(err)
	}
	existingPR, err := retainedOwnedPullRequest(inspection, run)
	if err != nil {
		return ProductionPushResult{}, c.rejectDivergentPush(ctx, run, effects, side, "")
	}
	remote, err := publisher.RemoteSHA(ctx, run.WorktreePath, run.WorkingBranch)
	if err != nil {
		return ProductionPushResult{}, c.recordPushFailure(ctx, run, effects, side, PushEvidence{}, "remote_lookup_failed", err)
	}
	if remote == run.CandidateHead {
		return c.completePush(ctx, run, effects, side, existingPR, PushEvidence{RemoteRef: "refs/heads/" + run.WorkingBranch, SHA: run.CandidateHead}, true)
	}
	if remote != "" {
		if existingPR == nil || existingPR.HeadSHA != remote {
			return ProductionPushResult{}, c.rejectDivergentPush(ctx, run, effects, side, remote)
		}
	}

	evidence, pushErr := publisher.Push(ctx, run.WorktreePath, run.WorkingBranch, run.CandidateHead, remote, run.ArtifactRoot)
	if pushErr != nil {
		remote, reconcileErr := publisher.RemoteSHA(ctx, run.WorktreePath, run.WorkingBranch)
		if reconcileErr == nil && remote == run.CandidateHead {
			return c.completePush(ctx, run, effects, side, existingPR, evidence, true)
		}
		if reconcileErr == nil && remote != "" {
			return ProductionPushResult{}, c.rejectDivergentPush(ctx, run, effects, side, remote)
		}
		return ProductionPushResult{}, c.recordPushFailure(ctx, run, effects, side, evidence, "push_failed", pushErr)
	}
	remote, err = publisher.RemoteSHA(ctx, run.WorktreePath, run.WorkingBranch)
	if err != nil {
		return ProductionPushResult{}, c.recordPushFailure(ctx, run, effects, side, evidence, "post_push_reconciliation_failed", err)
	}
	if remote != run.CandidateHead {
		if remote != "" {
			return ProductionPushResult{}, c.rejectDivergentPush(ctx, run, effects, side, remote)
		}
		return ProductionPushResult{}, c.recordPushFailure(ctx, run, effects, side, evidence, "post_push_remote_missing", errors.New("remote branch is absent after push"))
	}
	return c.completePush(ctx, run, effects, side, existingPR, evidence, false)
}

func retainedOwnedPullRequest(inspection RunInspection, run Run) (*domain.PullRequest, error) {
	if inspection.PullRequest == nil {
		return nil, nil
	}
	pr := *inspection.PullRequest
	if pr.DatabaseID < 1 || strings.TrimSpace(pr.URL) == "" || !strings.EqualFold(pr.State, "open") || pr.Merged || pr.HeadBranch != run.WorkingBranch || pr.BaseBranch != run.BaseBranch || pr.BaseSHA != run.BaseSHA || pr.OwnershipKey != run.IdempotencyKey || strings.TrimSpace(pr.BodyDigest) == "" {
		return nil, errors.New("persisted pull request is not an owned open update target")
	}
	return &pr, nil
}

func (c *ProductionCoordinator) completePush(ctx context.Context, run Run, effects pushStore, side SideEffectRecord, existingPR *domain.PullRequest, evidence PushEvidence, idempotent bool) (ProductionPushResult, error) {
	if evidence.SHA == "" {
		evidence.SHA = run.CandidateHead
	}
	if evidence.SHA != run.CandidateHead || evidence.RemoteRef != "refs/heads/"+run.WorkingBranch {
		return ProductionPushResult{}, c.recordPushFailure(ctx, run, effects, side, evidence, "invalid_push_evidence", errors.New("publisher returned mismatched evidence"))
	}
	if existingPR != nil && existingPR.HeadSHA != run.CandidateHead {
		updated := *existingPR
		updated.HeadSHA = run.CandidateHead
		if err := effects.SavePullRequest(ctx, run.ID, updated); err != nil {
			return ProductionPushResult{}, c.recordPushFailure(ctx, run, effects, side, evidence, "persist_repaired_pull_request_head_failed", err)
		}
	}
	result, err := json.Marshal(map[string]any{"remote_ref": evidence.RemoteRef, "pushed_sha": evidence.SHA, "exit_code": evidence.ExitCode})
	if err != nil {
		return ProductionPushResult{}, serviceError(ErrorInternal, "encode push result", err)
	}
	if side.Status != "observed" {
		side.Status, side.ResultJSON, side.StdoutPath, side.StderrPath, side.ObservedAt = "observed", string(result), evidence.StdoutPath, evidence.StderrPath, time.Now().UTC()
		if err := effects.FinishSideEffect(ctx, side); err != nil {
			return ProductionPushResult{}, classifyServiceError(err)
		}
	}
	ownership, err := remoteBranchOwnershipEvidence(ctx, c.store, run)
	if err != nil {
		return ProductionPushResult{}, classifyServiceError(err)
	}
	if err := c.store.AddOwnedResource(ctx, OwnedResource{RunID: run.ID, Kind: "remote_branch", Name: run.WorkingBranch, CreationEvidence: ownership, Status: "owned"}); err != nil {
		return ProductionPushResult{}, classifyServiceError(err)
	}
	if err := c.store.Transition(ctx, run.ID, domain.StatePushingBranch, domain.StateBranchPushed, "remote exact SHA observed", "push evidence", run.CandidateHead); err != nil {
		return ProductionPushResult{}, classifyServiceError(err)
	}
	next, err := c.store.GetRun(ctx, run.ID)
	if err != nil {
		return ProductionPushResult{}, classifyServiceError(err)
	}
	return ProductionPushResult{Action: ProductionStop, Run: projectRunResult(next), RemoteSHA: run.CandidateHead, Idempotent: idempotent}, nil
}

func remoteBranchOwnershipEvidence(ctx context.Context, store RunStore, run Run) (string, error) {
	inspection, err := store.Inspect(ctx, run.ID)
	if err != nil {
		return "", err
	}
	for _, resource := range inspection.Resources {
		if resource.RunID == run.ID && resource.Kind == "branch" && resource.Name == run.WorkingBranch && resource.Status == "owned" {
			_, legacy, err := cleanupResourceNonce(resource)
			if err != nil {
				return "", err
			}
			if err := validateCleanupEvidenceMode(run, resource, legacy); err != nil {
				return "", err
			}
			return resource.CreationEvidence, nil
		}
	}
	return "", errors.New("owned local branch evidence is required before remote ownership is recorded")
}

func (c *ProductionCoordinator) rejectDivergentPush(ctx context.Context, run Run, effects pushStore, side SideEffectRecord, remote string) error {
	result, _ := json.Marshal(map[string]string{"remote_ref": "refs/heads/" + run.WorkingBranch, "observed_remote_sha": remote})
	if side.Status != "observed" {
		side.Status, side.ResultJSON, side.ObservedAt = "failed", string(result), time.Now().UTC()
		if err := effects.FinishSideEffect(ctx, side); err != nil {
			return classifyServiceError(err)
		}
	}
	_ = c.store.SetLastError(ctx, run.ID, "remote branch diverged from the verified candidate")
	if err := c.store.Transition(ctx, run.ID, domain.StatePushingBranch, domain.StateManualIntervention, "remote branch diverged", "push reconciliation", run.CandidateHead); err != nil {
		return classifyServiceError(err)
	}
	return serviceError(ErrorConflict, "remote branch diverged from the verified candidate", nil)
}

func (c *ProductionCoordinator) recordPushFailure(ctx context.Context, run Run, effects pushStore, side SideEffectRecord, evidence PushEvidence, category string, cause error) error {
	result, _ := json.Marshal(map[string]any{"category": category, "exit_code": evidence.ExitCode})
	if side.Status != "observed" {
		side.Status, side.ResultJSON, side.StdoutPath, side.StderrPath, side.ObservedAt = "failed", string(result), evidence.StdoutPath, evidence.StderrPath, time.Now().UTC()
		if err := effects.FinishSideEffect(ctx, side); err != nil {
			return classifyServiceError(err)
		}
	}
	_ = c.store.SetLastError(ctx, run.ID, "push requires reconciliation before retry")
	return serviceError(ErrorUnavailable, "push did not produce matching remote evidence", cause)
}
