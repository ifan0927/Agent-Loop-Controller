package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

// SquashMergeRequest is the immutable, conditional write intent for one
// controller-owned pull request. No other merge method is representable.
type SquashMergeRequest struct {
	PullRequest     int64
	HeadBranch      string
	BaseBranch      string
	ExpectedHeadSHA string
	ExpectedBaseSHA string
	OwnershipKey    string
}

func (r SquashMergeRequest) Validate() error {
	if r.PullRequest < 1 || strings.TrimSpace(r.ExpectedHeadSHA) == "" || strings.TrimSpace(r.ExpectedBaseSHA) == "" || strings.TrimSpace(r.OwnershipKey) == "" {
		return errors.New("complete squash merge intent is required")
	}
	if err := domain.ValidateGitBranch(r.HeadBranch); err != nil {
		return err
	}
	return domain.ValidateGitBranch(r.BaseBranch)
}

type SquashMerger interface {
	SquashMerge(context.Context, SquashMergeRequest) (domain.PullRequest, []GitHubRequestObservation, GitHubInstallationMetadata, error)
}

// MergeRejectedError means GitHub definitively rejected a conditional merge or
// the adapter observed immutable target drift. It is distinct from a lost or
// unavailable response, which must be reconciled on a later invocation.
type MergeRejectedError struct {
	HTTPStatus int
	Operation  string
	Cause      error
}

func (e *MergeRejectedError) Error() string { return e.Cause.Error() }
func (e *MergeRejectedError) Unwrap() error { return e.Cause }

// MergePolicyThread is a digest-only observation of a controller-replied
// thread. It deliberately does not retain or interpret any follow-up body.
type MergePolicyThread struct {
	ThreadNodeID      string    `json:"thread_node_id"`
	RootCommentNodeID string    `json:"root_comment_node_id"`
	RootCommentID     int64     `json:"root_comment_id"`
	ReplyNodeID       string    `json:"reply_node_id"`
	ReplyID           int64     `json:"reply_id"`
	Resolved          bool      `json:"resolved"`
	Outdated          bool      `json:"outdated"`
	TopologyDigest    string    `json:"topology_digest"`
	ObservedAt        time.Time `json:"observed_at"`
}

type ProductionMergeCommand struct {
	Requester      Requester
	RunID          string
	Repository     string
	ExpectedState  domain.State
	IdempotencyKey string
}

type ProductionMergeResult struct {
	Action      ProductionAction `json:"action"`
	Run         RunResult        `json:"run"`
	PullRequest int64            `json:"pull_request"`
	MergeSHA    string           `json:"merge_sha"`
	Idempotent  bool             `json:"idempotent"`
}

type mergeStore interface {
	BeginSideEffect(context.Context, SideEffectRecord) (SideEffectRecord, bool, error)
	RetryMergeSideEffect(context.Context, SideEffectRecord) (SideEffectRecord, bool, error)
	RecordMergePolicyPending(context.Context, string, SideEffectRecord, string) error
	FinishSideEffect(context.Context, SideEffectRecord) error
	SaveMerge(context.Context, MergeRecord) error
	SaveGitHubInstallation(context.Context, string, GitHubInstallationMetadata) error
	SaveGitHubRequest(context.Context, GitHubRequestObservation) error
	SaveGitHubEvidence(context.Context, string, domain.GitHubReadEvidence) error
}

// MergePullRequest is the only production merge coordinator. It re-reads all
// GitHub gates immediately before persisting intent and issuing a conditional
// squash merge, then records observed GitHub state rather than trusting a write
// response alone.
func (c *ProductionCoordinator) MergePullRequest(ctx context.Context, command ProductionMergeCommand, validator ApprovalValidator, reader GitHubReadPort, merger SquashMerger) (ProductionMergeResult, error) {
	if validator == nil || reader == nil || merger == nil {
		return ProductionMergeResult{}, serviceError(ErrorInvalidInput, "merge validator, reader, and merger are required", nil)
	}
	run, err := c.admission.Revalidate(ctx, LinearRevalidateCommand{Requester: command.Requester, RunID: command.RunID, Repository: command.Repository, ExpectedState: command.ExpectedState, IdempotencyKey: command.IdempotencyKey})
	if err != nil {
		return ProductionMergeResult{}, err
	}
	action, reason := productionNextAction(run.State)
	if action != ProductionMerge {
		return ProductionMergeResult{Action: action, Run: projectRunResult(run)}, serviceError(ErrorConflict, reason, nil)
	}
	if err := validator.ValidateApprovalReady(ctx, run.ID); err != nil {
		return ProductionMergeResult{}, serviceError(ErrorConflict, "fresh exact-HEAD local evidence is no longer valid", err)
	}
	stores, ok := c.store.(mergeStore)
	if !ok {
		return ProductionMergeResult{}, serviceError(ErrorInternal, "configured store cannot persist merge evidence", nil)
	}
	inspection, err := c.store.Inspect(ctx, run.ID)
	if err != nil {
		return ProductionMergeResult{}, classifyServiceError(err)
	}
	if inspection.PullRequest == nil || inspection.Approval == nil {
		return ProductionMergeResult{}, serviceError(ErrorConflict, "persisted pull request and trusted human approval are required", nil)
	}
	if err := validateReaderAuthority(inspection, reader.Authority()); err != nil {
		return ProductionMergeResult{}, err
	}
	request := SquashMergeRequest{PullRequest: inspection.PullRequest.Number, HeadBranch: run.WorkingBranch, BaseBranch: run.BaseBranch, ExpectedHeadSHA: run.CandidateHead, ExpectedBaseSHA: run.BaseSHA, OwnershipKey: run.IdempotencyKey}
	if err := request.Validate(); err != nil {
		return ProductionMergeResult{}, serviceError(ErrorInternal, "build immutable squash merge intent", err)
	}
	evidence, handoff, observations, metadata, readErr := reader.Read(ctx, request.PullRequest, request.ExpectedHeadSHA)
	if handoffErr := handoff.Validate(); handoffErr != nil {
		return ProductionMergeResult{}, serviceError(ErrorUnavailable, "inline review body handoff is incomplete", handoffErr)
	}
	if err := persistMergeRead(ctx, stores, run.ID, observations, metadata, evidence); err != nil {
		return ProductionMergeResult{}, classifyServiceError(err)
	}
	if readErr != nil {
		return ProductionMergeResult{}, serviceError(ErrorUnavailable, "unable to re-read GitHub merge gates", readErr)
	}
	if evidence.PullRequest.Merged {
		return c.reconcileObservedMerge(ctx, run, inspection, request, evidence.PullRequest, stores)
	}
	if !strings.EqualFold(evidence.PullRequest.State, "open") {
		return ProductionMergeResult{}, c.rejectMergeConflict(ctx, run, "pull request closed without merge", nil)
	}
	if err := authorizeProductionMerge(run, inspection, request, evidence); err != nil {
		return ProductionMergeResult{}, c.rejectMergeConflict(ctx, run, "fresh GitHub merge gate failed", err)
	}
	intentJSON, err := json.Marshal(map[string]any{"repository_id": repositoryID(run), "installation_id": installationID(run), "pull_request": request.PullRequest, "head_branch": request.HeadBranch, "base_branch": request.BaseBranch, "expected_head_sha": request.ExpectedHeadSHA, "expected_base_sha": request.ExpectedBaseSHA, "merge_method": "squash"})
	if err != nil {
		return ProductionMergeResult{}, serviceError(ErrorInternal, "encode squash merge intent", err)
	}
	side, created, err := stores.BeginSideEffect(ctx, SideEffectRecord{RunID: run.ID, Kind: "squash_merge", IdempotencyKey: run.CandidateHead, IntentJSON: string(intentJSON), Attempt: 1})
	if err != nil {
		return ProductionMergeResult{}, classifyServiceError(err)
	}
	if !created && side.Status == "failed" && mergePolicySideEffect(side) {
		persisted, pendingErr := mergePolicyPendingThreads(side, run.CandidateHead)
		if pendingErr != nil {
			return ProductionMergeResult{}, c.recordMergeRejected(ctx, run, stores, side, pendingErr)
		}
		threads, topologyErr := controllerRepliedMergeThreads(inspection, evidence, handoff)
		if topologyErr != nil || len(threads) == 0 {
			if topologyErr == nil {
				topologyErr = errors.New("merge policy retry has no tracked controller reply")
			}
			return ProductionMergeResult{}, c.recordMergeRejected(ctx, run, stores, side, topologyErr)
		}
		if !sameMergePolicyTopology(persisted, threads) {
			return ProductionMergeResult{}, c.recordMergeRejected(ctx, run, stores, side, errors.New("controller reply topology changed since merge policy rejection"))
		}
		if hasUnresolvedMergeThread(threads) {
			return c.recordMergePolicyPending(ctx, run, stores, side, threads)
		}
		side, created, err = stores.RetryMergeSideEffect(ctx, side)
		if err != nil {
			return ProductionMergeResult{}, classifyServiceError(err)
		}
		if !created {
			return ProductionMergeResult{}, serviceError(ErrorUnavailable, "merge policy retry is already claimed; reconcile GitHub before retrying", nil)
		}
	}
	if !created && side.Status == "intent" {
		return ProductionMergeResult{}, c.recordMergeRejected(ctx, run, stores, side, errors.New("persisted squash merge intent has no durable outcome"))
	}
	merged, mergeObservations, mergeMetadata, mergeErr := merger.SquashMerge(ctx, request)
	persistErr := persistMergeRequests(ctx, stores, run.ID, mergeObservations, mergeMetadata)
	if mergeErr != nil {
		var rejected *MergeRejectedError
		if errors.As(mergeErr, &rejected) {
			if persistErr != nil {
				return ProductionMergeResult{}, c.recordMergeRejected(ctx, run, stores, side, persistErr)
			}
			if mergePolicyStatus(rejected, mergeObservations) {
				return c.awaitMergePolicyResolution(ctx, run, inspection, request, stores, side, reader)
			}
			return ProductionMergeResult{}, c.recordMergeRejected(ctx, run, stores, side, rejected)
		}
		return ProductionMergeResult{}, c.recordMergeUncertain(ctx, run, stores, side, mergeErr)
	}
	if persistErr != nil {
		return ProductionMergeResult{}, classifyServiceError(persistErr)
	}
	return c.completeObservedMerge(ctx, run, inspection, request, merged, stores, side, false)
}

func mergePolicySideEffect(side SideEffectRecord) bool {
	var result struct {
		Category string `json:"category"`
	}
	return json.Unmarshal([]byte(side.ResultJSON), &result) == nil && result.Category == "merge_policy_pending"
}

func mergePolicyPendingResult(head string, threads []MergePolicyThread) ([]byte, error) {
	return json.Marshal(struct {
		Category string              `json:"category"`
		Head     string              `json:"head"`
		Threads  []MergePolicyThread `json:"threads"`
	}{Category: "merge_policy_pending", Head: head, Threads: threads})
}

func mergePolicyPendingThreads(side SideEffectRecord, expectedHead string) ([]MergePolicyThread, error) {
	var result struct {
		Category string              `json:"category"`
		Head     string              `json:"head"`
		Threads  []MergePolicyThread `json:"threads"`
	}
	if err := json.Unmarshal([]byte(side.ResultJSON), &result); err != nil {
		return nil, errors.New("persisted merge policy result is malformed")
	}
	if result.Category != "merge_policy_pending" || result.Head != expectedHead || len(result.Threads) == 0 || !hasUnresolvedMergeThread(result.Threads) {
		return nil, errors.New("persisted merge policy result is incomplete")
	}
	seen := make(map[string]struct{}, len(result.Threads))
	for _, thread := range result.Threads {
		if thread.ThreadNodeID == "" || thread.RootCommentNodeID == "" || thread.RootCommentID < 1 || thread.ReplyNodeID == "" || thread.ReplyID < 1 || thread.TopologyDigest == "" {
			return nil, errors.New("persisted merge policy thread topology is incomplete")
		}
		if _, exists := seen[thread.ThreadNodeID]; exists {
			return nil, errors.New("persisted merge policy thread topology is ambiguous")
		}
		seen[thread.ThreadNodeID] = struct{}{}
	}
	return result.Threads, nil
}

// sameMergePolicyTopology accepts only the resolution bit changing between the
// rejected merge observation and the retry read. A changed reply, follow-up,
// identity, or outdated flag is authority drift and must not reach the retry
// compare-and-swap.
func sameMergePolicyTopology(persisted, fresh []MergePolicyThread) bool {
	if len(persisted) == 0 || len(persisted) != len(fresh) {
		return false
	}
	byThread := make(map[string]MergePolicyThread, len(persisted))
	for _, thread := range persisted {
		if _, exists := byThread[thread.ThreadNodeID]; exists {
			return false
		}
		byThread[thread.ThreadNodeID] = thread
	}
	for _, thread := range fresh {
		previous, exists := byThread[thread.ThreadNodeID]
		if !exists || previous.RootCommentNodeID != thread.RootCommentNodeID || previous.RootCommentID != thread.RootCommentID || previous.ReplyNodeID != thread.ReplyNodeID || previous.ReplyID != thread.ReplyID || previous.TopologyDigest != thread.TopologyDigest || previous.Outdated != thread.Outdated {
			return false
		}
	}
	return true
}

func mergePolicyStatus(rejected *MergeRejectedError, observations []GitHubRequestObservation) bool {
	if rejected != nil && rejected.Operation == "squash_merge_pull_request" && (rejected.HTTPStatus == 405 || rejected.HTTPStatus == 409 || rejected.HTTPStatus == 422) {
		return true
	}
	for _, observation := range observations {
		if observation.Operation != "squash_merge_pull_request" {
			continue
		}
		switch observation.HTTPStatus {
		case 405, 409, 422:
			return true
		}
	}
	return false
}

// awaitMergePolicyResolution performs the required post-rejection fresh read.
// A merge rejection alone is never evidence of a branch-protection policy.
func (c *ProductionCoordinator) awaitMergePolicyResolution(ctx context.Context, run Run, inspection RunInspection, request SquashMergeRequest, store mergeStore, side SideEffectRecord, reader GitHubReadPort) (ProductionMergeResult, error) {
	evidence, handoff, observations, metadata, readErr := reader.Read(ctx, request.PullRequest, request.ExpectedHeadSHA)
	if readErr != nil {
		if err := persistMergeRequests(ctx, store, run.ID, observations, metadata); err != nil {
			return ProductionMergeResult{}, c.recordMergeUncertain(ctx, run, store, side, err)
		}
		return ProductionMergeResult{}, c.recordMergeUncertain(ctx, run, store, side, readErr)
	}
	if handoffErr := handoff.Validate(); handoffErr != nil {
		return ProductionMergeResult{}, c.recordMergeRejected(ctx, run, store, side, handoffErr)
	}
	if err := persistMergeRead(ctx, store, run.ID, observations, metadata, evidence); err != nil {
		return ProductionMergeResult{}, c.recordMergeRejected(ctx, run, store, side, err)
	}
	if err := validateReaderAuthority(inspection, metadata); err != nil {
		return ProductionMergeResult{}, c.recordMergeRejected(ctx, run, store, side, err)
	}
	if err := authorizeProductionMerge(run, inspection, request, evidence); err != nil {
		return ProductionMergeResult{}, c.recordMergeRejected(ctx, run, store, side, err)
	}
	threads, err := controllerRepliedMergeThreads(inspection, evidence, handoff)
	if err != nil || len(threads) == 0 || !hasUnresolvedMergeThread(threads) {
		if err == nil {
			err = errors.New("merge rejection has no tracked unresolved controller reply")
		}
		return ProductionMergeResult{}, c.recordMergeRejected(ctx, run, store, side, err)
	}
	return c.recordMergePolicyPending(ctx, run, store, side, threads)
}

func (c *ProductionCoordinator) recordMergePolicyPending(ctx context.Context, run Run, store mergeStore, side SideEffectRecord, threads []MergePolicyThread) (ProductionMergeResult, error) {
	result, err := mergePolicyPendingResult(run.CandidateHead, threads)
	if err != nil {
		return ProductionMergeResult{}, serviceError(ErrorInternal, "encode pending merge policy observation", err)
	}
	side.Status, side.ResultJSON, side.ObservedAt = "failed", string(result), time.Now().UTC()
	// The policy evidence and wait state must commit together. Otherwise a
	// restart could observe a wait state without the policy topology that
	// authorizes its guarded retry.
	if err := store.RecordMergePolicyPending(ctx, run.ID, side, run.CandidateHead); err != nil {
		return ProductionMergeResult{}, classifyServiceError(err)
	}
	next, err := c.store.GetRun(ctx, run.ID)
	if err != nil {
		return ProductionMergeResult{}, classifyServiceError(err)
	}
	return ProductionMergeResult{Action: ProductionReconcileGitHub, Run: projectRunResult(next), PullRequest: sideEffectPullRequest(side)}, serviceError(ErrorUnavailable, "GitHub merge protection is awaiting human thread resolution", nil)
}

// reconcileMergeability observes GitHub only. It has no merger dependency, so
// a restart in the wait state cannot issue another merge request.
func (c *ProductionCoordinator) reconcileMergeability(ctx context.Context, command ProductionReconcileCommand, run Run, reader GitHubReadPort) (ProductionResult, error) {
	if reader == nil {
		return ProductionResult{}, serviceError(ErrorInvalidInput, "GitHub reader is required", nil)
	}
	result, err := c.commands.withReconcileLease(ctx, ReconcileCommand{Requester: command.Requester, RunID: command.RunID, Repository: command.Repository, ExpectedState: command.ExpectedState, IdempotencyKey: command.IdempotencyKey}, func(leaseCtx context.Context, inspection RunInspection, owner string) (ReconcileResult, error) {
		if err := validateReconcileInspection(ReconcileCommand{Requester: command.Requester, RunID: command.RunID, Repository: command.Repository, ExpectedState: command.ExpectedState, IdempotencyKey: command.IdempotencyKey}, inspection); err != nil {
			return ReconcileResult{}, err
		}
		if inspection.Run.State != domain.StateAwaitingGitHubMergeability {
			return ReconcileResult{}, serviceError(ErrorConflict, "run is not awaiting GitHub mergeability", nil)
		}
		side, found := mergeSideEffect(inspection.SideEffects, inspection.Run.CandidateHead)
		if !found || side.Status != "failed" {
			return ReconcileResult{}, c.mergeabilityManual(leaseCtx, inspection.Run, "merge policy wait lacks a durable failed side effect")
		}
		if _, err := mergePolicyPendingThreads(side, inspection.Run.CandidateHead); err != nil {
			return ReconcileResult{}, c.mergeabilityManual(leaseCtx, inspection.Run, "merge policy wait lacks durable topology evidence")
		}
		if err := validateReaderAuthority(inspection, reader.Authority()); err != nil {
			return ReconcileResult{}, c.mergeabilityManual(leaseCtx, inspection.Run, "GitHub mergeability reader authority drifted")
		}
		evidence, handoff, observations, metadata, readErr := reader.Read(leaseCtx, inspection.PullRequest.Number, inspection.Run.CandidateHead)
		if readErr != nil {
			if err := c.saveMergeabilityReadFailure(leaseCtx, inspection.Run, owner, observations); err != nil {
				return ReconcileResult{}, err
			}
			return ReconcileResult{}, serviceError(ErrorUnavailable, "GitHub mergeability read is unavailable", readErr)
		}
		if err := handoff.Validate(); err != nil {
			return ReconcileResult{}, c.mergeabilityManualAfterRead(leaseCtx, inspection.Run, owner, observations, metadata, evidence, "GitHub mergeability topology is incomplete")
		}
		if err := validateReaderAuthority(inspection, metadata); err != nil {
			return ReconcileResult{}, c.mergeabilityManualAfterRead(leaseCtx, inspection.Run, owner, observations, metadata, evidence, "GitHub mergeability metadata authority drifted")
		}
		request := SquashMergeRequest{PullRequest: inspection.PullRequest.Number, HeadBranch: inspection.Run.WorkingBranch, BaseBranch: inspection.Run.BaseBranch, ExpectedHeadSHA: inspection.Run.CandidateHead, ExpectedBaseSHA: inspection.Run.BaseSHA, OwnershipKey: inspection.Run.IdempotencyKey}
		if err := request.Validate(); err != nil {
			return ReconcileResult{}, serviceError(ErrorInternal, "build immutable squash merge intent", err)
		}
		expectedRepository, err := mergeExpectedRepository(inspection)
		if err != nil || ReconcileGitHubRead(expectedRepository, *inspection.PullRequest, request.HeadBranch, request.BaseBranch, request.ExpectedHeadSHA, request.ExpectedBaseSHA, request.OwnershipKey, inspection.PullRequest.BodyDigest, evidence) != nil {
			return ReconcileResult{}, c.mergeabilityManualAfterRead(leaseCtx, inspection.Run, owner, observations, metadata, evidence, "GitHub mergeability authority drifted")
		}
		if evidence.DeliveryStatus() != domain.ReconciliationPass {
			if err := c.saveMergeabilityRead(leaseCtx, inspection.Run, owner, observations, metadata, evidence, nil, nil, domain.StateReconcilingReviews, "GitHub merge gates changed while awaiting thread resolution"); err != nil {
				return ReconcileResult{}, err
			}
			return ReconcileResult{Head: inspection.Run.CandidateHead, Status: evidence.DeliveryStatus(), State: domain.StateReconcilingReviews}, nil
		}
		trusted, err := trustedHumanActors(inspection)
		if err != nil {
			return ReconcileResult{}, c.mergeabilityManualAfterRead(leaseCtx, inspection.Run, owner, observations, metadata, evidence, "trusted human approval identity is unavailable")
		}
		approvalObservation, approval, err := domain.NormalizeHumanApproval(evidence.PullRequest, evidence.Reviews, trusted, evidence.ObservedAt)
		if err != nil || approvalObservation.Status == domain.HumanApprovalAmbiguous || approvalObservation.Status == domain.HumanApprovalUntrustedActor {
			return ReconcileResult{}, c.mergeabilityManualAfterRead(leaseCtx, inspection.Run, owner, observations, metadata, evidence, "GitHub mergeability approval is ambiguous")
		}
		if approvalObservation.Status == domain.HumanApprovalChangesRequested {
			return c.commands.reconcileMergeabilityChangesRequested(leaseCtx, ReconcileCommand{Requester: command.Requester, RunID: command.RunID, Repository: command.Repository, ExpectedState: command.ExpectedState, IdempotencyKey: command.IdempotencyKey}, inspection, owner, evidence, handoff, observations, metadata)
		}
		if approvalObservation.Status == domain.HumanApprovalDismissed || approvalObservation.Status == domain.HumanApprovalPending || approvalObservation.Status == domain.HumanApprovalStaleHead {
			if err := c.saveMergeabilityRead(leaseCtx, inspection.Run, owner, observations, metadata, evidence, &approvalObservation, approval, domain.StateAwaitingHumanApproval, "GitHub approval changed while awaiting thread resolution"); err != nil {
				return ReconcileResult{}, err
			}
			return ReconcileResult{Head: inspection.Run.CandidateHead, Status: evidence.DeliveryStatus(), State: domain.StateAwaitingHumanApproval}, nil
		}
		if approval == nil || approvalObservation.Status != domain.HumanApprovalApproved {
			return ReconcileResult{}, c.mergeabilityManualAfterRead(leaseCtx, inspection.Run, owner, observations, metadata, evidence, "GitHub mergeability approval is incomplete")
		}
		if err := authorizeProductionMerge(inspection.Run, inspection, request, evidence); err != nil {
			return ReconcileResult{}, c.mergeabilityManualAfterRead(leaseCtx, inspection.Run, owner, observations, metadata, evidence, "GitHub mergeability approval authority drifted")
		}
		threads, err := controllerRepliedMergeThreads(inspection, evidence, handoff)
		if err != nil || len(threads) == 0 {
			return ReconcileResult{}, c.mergeabilityManualAfterRead(leaseCtx, inspection.Run, owner, observations, metadata, evidence, "controller reply topology is ambiguous")
		}
		next, reason := domain.StateAwaitingGitHubMergeability, "controller-replied thread remains unresolved"
		if !hasUnresolvedMergeThread(threads) {
			next, reason = domain.StateMerging, "controller-replied threads resolved; guarded merge may be retried"
		}
		if err := c.saveMergeabilityRead(leaseCtx, inspection.Run, owner, observations, metadata, evidence, &approvalObservation, approval, next, reason); err != nil {
			return ReconcileResult{}, err
		}
		return ReconcileResult{Head: inspection.Run.CandidateHead, Status: evidence.DeliveryStatus(), State: next}, nil
	})
	if err != nil {
		return ProductionResult{}, err
	}
	next, reason := productionNextAction(result.State)
	updated, err := c.store.GetRun(ctx, run.ID)
	if err != nil {
		return ProductionResult{}, classifyServiceError(err)
	}
	return ProductionResult{Action: next, Run: projectRunResult(updated), Head: result.Head, Reason: reason}, nil
}

func (c *ProductionCoordinator) saveMergeabilityRead(ctx context.Context, run Run, owner string, observations []GitHubRequestObservation, metadata GitHubInstallationMetadata, evidence domain.GitHubReadEvidence, approvalObservation *domain.HumanApprovalObservation, approval *domain.HumanApproval, next domain.State, reason string) error {
	persister, ok := c.store.(interface {
		SaveGitHubReadSuccess(context.Context, string, string, domain.State, string, []GitHubRequestObservation, domain.PullRequest, GitHubInstallationMetadata, domain.GitHubReadEvidence, []TrustedReviewFeedbackRecord, *domain.HumanApprovalObservation, *domain.HumanApproval, domain.State, string) error
	})
	if !ok {
		return serviceError(ErrorInternal, "mergeability reconciliation persistence is unavailable", nil)
	}
	if err := persister.SaveGitHubReadSuccess(ctx, run.ID, owner, domain.StateAwaitingGitHubMergeability, run.IdempotencyKey, observations, evidence.PullRequest, metadata, evidence, nil, approvalObservation, approval, next, reason); err != nil {
		return classifyServiceError(err)
	}
	if next == domain.StateAwaitingGitHubMergeability {
		_ = c.store.SetLastError(ctx, run.ID, "merge_policy_pending")
	}
	return nil
}

func (c *ProductionCoordinator) saveMergeabilityReadFailure(ctx context.Context, run Run, owner string, observations []GitHubRequestObservation) error {
	persister, ok := c.store.(interface {
		SaveGitHubReadFailure(context.Context, string, string, domain.State, string, []GitHubRequestObservation) error
	})
	if !ok {
		return serviceError(ErrorInternal, "mergeability failure persistence is unavailable", nil)
	}
	if err := persister.SaveGitHubReadFailure(ctx, run.ID, owner, domain.StateAwaitingGitHubMergeability, run.IdempotencyKey, observations); err != nil {
		return classifyServiceError(err)
	}
	return nil
}

func (c *ProductionCoordinator) mergeabilityManualAfterRead(ctx context.Context, run Run, owner string, observations []GitHubRequestObservation, metadata GitHubInstallationMetadata, evidence domain.GitHubReadEvidence, reason string) error {
	if err := c.saveMergeabilityRead(ctx, run, owner, observations, metadata, evidence, nil, nil, domain.StateManualIntervention, reason); err != nil {
		return err
	}
	return serviceError(ErrorConflict, "GitHub mergeability requires manual intervention", nil)
}

func (c *ProductionCoordinator) mergeabilityManual(ctx context.Context, run Run, reason string) error {
	_ = c.store.SetLastError(ctx, run.ID, reason)
	if err := c.store.Transition(ctx, run.ID, domain.StateAwaitingGitHubMergeability, domain.StateManualIntervention, reason, "merge_policy_pending", run.CandidateHead); err != nil {
		return classifyServiceError(err)
	}
	return serviceError(ErrorConflict, "GitHub mergeability requires manual intervention", nil)
}

func sideEffectPullRequest(side SideEffectRecord) int64 {
	var intent struct {
		PullRequest int64 `json:"pull_request"`
	}
	_ = json.Unmarshal([]byte(side.IntentJSON), &intent)
	return intent.PullRequest
}

func hasUnresolvedMergeThread(threads []MergePolicyThread) bool {
	for _, thread := range threads {
		if !thread.Resolved {
			return true
		}
	}
	return false
}

// controllerRepliedMergeThreads verifies only persisted controller replies.
// Human follow-up text contributes a digest to topology evidence, never input
// to an implementation or merge decision.
func controllerRepliedMergeThreads(inspection RunInspection, evidence domain.GitHubReadEvidence, handoff domain.InlineReviewBodyHandoff) ([]MergePolicyThread, error) {
	if err := handoff.Validate(); err != nil {
		return nil, err
	}
	replies := make(map[string]ReviewReplyEvidence, len(inspection.ReviewReplies))
	for _, reply := range inspection.ReviewReplies {
		if reply.RunID != inspection.Run.ID || reply.RootCommentNodeID == "" || reply.PullRequestNumber < 1 || reply.RootCommentID < 1 || reply.ReplyDatabaseID < 1 || reply.ReplyNodeID == "" || reply.AppID < 1 || reply.ObservedAt.IsZero() {
			return nil, errors.New("persisted controller reply evidence is incomplete")
		}
		if _, exists := replies[reply.RootCommentNodeID]; exists {
			return nil, errors.New("persisted controller reply evidence is ambiguous")
		}
		replies[reply.RootCommentNodeID] = reply
	}
	bodies := make(map[string]domain.InlineReviewBody, len(handoff.Comments))
	for _, body := range handoff.Comments {
		bodies[body.CommentNodeID] = body
	}
	var result []MergePolicyThread
	for _, feedback := range inspection.TrustedFeedback {
		if feedback.Lifecycle != domain.TrustedReviewFeedbackReplied {
			continue
		}
		reply, found := replies[feedback.RootCommentNodeID]
		if !found || reply.PullRequestNumber != feedback.PRNumber || reply.RootCommentID != feedback.RootCommentDatabaseID || reply.RepairedHead != feedback.BoundRepairHead || reply.MarkerDigest != feedback.ReplyIntentKey || reply.ReplyDatabaseID != feedback.ReplyDatabaseID || reply.ReplyNodeID != feedback.ReplyNodeID {
			return nil, errors.New("persisted controller reply evidence drifted")
		}
		thread, found := mergeThreadByID(evidence.ReviewThreads, feedback.ThreadNodeID)
		if !found || !mergeThreadRootMatches(thread, feedback, bodies) {
			return nil, errors.New("controller reply thread authority drifted")
		}
		if !mergeThreadContainsReply(inspection.Run.ID, thread, feedback, reply, bodies) {
			return nil, errors.New("controller reply topology drifted")
		}
		digest, err := mergeThreadTopologyDigest(thread)
		if err != nil {
			return nil, err
		}
		result = append(result, MergePolicyThread{ThreadNodeID: thread.NodeID, RootCommentNodeID: feedback.RootCommentNodeID, RootCommentID: feedback.RootCommentDatabaseID, ReplyNodeID: reply.ReplyNodeID, ReplyID: reply.ReplyDatabaseID, Resolved: thread.Resolved, Outdated: thread.Outdated, TopologyDigest: digest, ObservedAt: evidence.ObservedAt.UTC()})
	}
	return result, nil
}

func mergeThreadByID(threads []domain.GitHubReviewThread, id string) (domain.GitHubReviewThread, bool) {
	var found *domain.GitHubReviewThread
	for index := range threads {
		if threads[index].NodeID != id {
			continue
		}
		if found != nil {
			return domain.GitHubReviewThread{}, false
		}
		found = &threads[index]
	}
	if found == nil {
		return domain.GitHubReviewThread{}, false
	}
	return *found, true
}

func mergeThreadRootMatches(thread domain.GitHubReviewThread, feedback TrustedReviewFeedbackRecord, bodies map[string]domain.InlineReviewBody) bool {
	if thread.OriginalCommitSHA != feedback.OriginalReviewHeadSHA || thread.Path != feedback.Path || !sameReplyLine(thread.Line, feedback.Line) {
		return false
	}
	for _, comment := range thread.Comments {
		if comment.NodeID != feedback.RootCommentNodeID || comment.DatabaseID != feedback.RootCommentDatabaseID {
			continue
		}
		body, found := bodies[comment.NodeID]
		return found && body.ThreadNodeID == thread.NodeID && body.BodyDigest == feedback.BodyDigest && comment.BodyDigest == feedback.BodyDigest && comment.ReplyToDatabaseID == 0 && comment.ReplyToNodeID == "" && sameReplyActor(comment.Author, feedback.Author) && comment.Review.DatabaseID == feedback.ReviewDatabaseID && comment.Review.NodeID == feedback.ReviewNodeID && comment.Review.State == "CHANGES_REQUESTED" && comment.Review.CommitSHA == feedback.OriginalReviewHeadSHA && sameReplyActor(&comment.Review.Actor, feedback.Author)
	}
	return false
}

func mergeThreadContainsReply(runID string, thread domain.GitHubReviewThread, feedback TrustedReviewFeedbackRecord, reply ReviewReplyEvidence, bodies map[string]domain.InlineReviewBody) bool {
	marker, markerDigest, err := domain.ReviewReplyMarker(runID, feedback.PRNumber, feedback.ThreadNodeID, feedback.RootCommentDatabaseID, feedback.RootCommentNodeID, feedback.BodyDigest, feedback.BoundRepairHead)
	if err != nil || markerDigest != reply.MarkerDigest {
		return false
	}
	expectedBody, err := domain.ReviewReplyBody(feedback.BoundRepairHead, marker)
	if err != nil {
		return false
	}
	expectedDigest := domain.TrustedReviewFeedbackDigest(expectedBody)
	for _, comment := range thread.Comments {
		if comment.NodeID != reply.ReplyNodeID || comment.DatabaseID != reply.ReplyDatabaseID {
			continue
		}
		body, found := bodies[comment.NodeID]
		return found && body.ThreadNodeID == thread.NodeID && body.BodyDigest == expectedDigest && comment.BodyDigest == expectedDigest && comment.ReplyToNodeID != "" && comment.ReplyToDatabaseID == reply.RootCommentID && domain.ReviewReplyMarkerDigest(body.Body) == reply.MarkerDigest
	}
	return false
}

func mergeThreadTopologyDigest(thread domain.GitHubReviewThread) (string, error) {
	if thread.NodeID == "" || len(thread.Comments) == 0 || len(thread.Comments) > 100 {
		return "", errors.New("merge policy thread topology is incomplete")
	}
	comments := append([]domain.GitHubReviewComment(nil), thread.Comments...)
	sort.Slice(comments, func(left, right int) bool { return comments[left].NodeID < comments[right].NodeID })
	payload, err := json.Marshal(struct {
		ThreadID string                       `json:"thread_id"`
		Comments []domain.GitHubReviewComment `json:"comments"`
	}{ThreadID: thread.NodeID, Comments: comments})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func authorizeProductionMerge(run Run, inspection RunInspection, request SquashMergeRequest, evidence domain.GitHubReadEvidence) error {
	if inspection.PullRequest == nil || inspection.Approval == nil {
		return errors.New("persisted pull request and approval are required")
	}
	expectedRepository, err := mergeExpectedRepository(inspection)
	if err != nil {
		return err
	}
	if err := ReconcileGitHubRead(expectedRepository, *inspection.PullRequest, request.HeadBranch, request.BaseBranch, request.ExpectedHeadSHA, request.ExpectedBaseSHA, request.OwnershipKey, inspection.PullRequest.BodyDigest, evidence); err != nil {
		return err
	}
	if evidence.DeliveryStatus() != domain.ReconciliationPass {
		return errors.New("required checks are not passing for the exact head")
	}
	trusted, err := trustedHumanActors(inspection)
	if err != nil {
		return err
	}
	_, approval, err := domain.NormalizeHumanApproval(evidence.PullRequest, evidence.Reviews, trusted, evidence.ObservedAt)
	if err != nil {
		return err
	}
	if approval == nil {
		return errors.New("trusted human approval is not currently present")
	}
	if err := approval.Authorizes(evidence.PullRequest, run.CandidateHead); err != nil {
		return err
	}
	if err := inspection.Approval.Authorizes(evidence.PullRequest, run.CandidateHead); err != nil {
		return fmt.Errorf("persisted human approval is no longer valid: %w", err)
	}
	return nil
}

func mergeExpectedRepository(inspection RunInspection) (domain.RepositoryIdentity, error) {
	if inspection.GitHubInstallation != nil {
		return inspection.GitHubInstallation.Repository, nil
	}
	if inspection.RepositoryBinding == nil {
		return domain.RepositoryIdentity{}, errors.New("persisted GitHub repository authority is required")
	}
	parts := strings.Split(inspection.RepositoryBinding.CanonicalRepository, "/")
	if len(parts) != 2 || inspection.RepositoryBinding.ExpectedRepositoryID < 1 {
		return domain.RepositoryIdentity{}, errors.New("persisted GitHub repository authority is invalid")
	}
	return domain.RepositoryIdentity{ID: inspection.RepositoryBinding.ExpectedRepositoryID, Owner: parts[0], Name: parts[1]}, nil
}

func persistMergeRead(ctx context.Context, store mergeStore, runID string, observations []GitHubRequestObservation, metadata GitHubInstallationMetadata, evidence domain.GitHubReadEvidence) error {
	if err := persistMergeRequests(ctx, store, runID, observations, metadata); err != nil {
		return err
	}
	if evidence.Repository.ID == 0 {
		return nil
	}
	return store.SaveGitHubEvidence(ctx, runID, evidence)
}

func persistMergeRequests(ctx context.Context, store mergeStore, runID string, observations []GitHubRequestObservation, metadata GitHubInstallationMetadata) error {
	if metadata.AppID > 0 {
		if err := store.SaveGitHubInstallation(ctx, runID, metadata); err != nil {
			return err
		}
	}
	for _, observation := range observations {
		observation.RunID = runID
		if err := store.SaveGitHubRequest(ctx, observation); err != nil {
			return err
		}
	}
	return nil
}

func (c *ProductionCoordinator) reconcileObservedMerge(ctx context.Context, run Run, inspection RunInspection, request SquashMergeRequest, observed domain.PullRequest, store mergeStore) (ProductionMergeResult, error) {
	side, ok := mergeSideEffect(inspection.SideEffects, run.CandidateHead)
	if !ok {
		return ProductionMergeResult{}, c.rejectMergeConflict(ctx, run, "pull request merged without controller merge intent", nil)
	}
	return c.completeObservedMerge(ctx, run, inspection, request, observed, store, side, true)
}

func (c *ProductionCoordinator) completeObservedMerge(ctx context.Context, run Run, inspection RunInspection, request SquashMergeRequest, observed domain.PullRequest, store mergeStore, side SideEffectRecord, idempotent bool) (ProductionMergeResult, error) {
	if err := validateObservedSquashMerge(request, observed); err != nil {
		return ProductionMergeResult{}, c.rejectMergeConflict(ctx, run, "merged pull request conflicts with immutable intent", err)
	}
	merge := MergeRecord{RunID: run.ID, PRNumber: request.PullRequest, PreMergeSHA: request.ExpectedHeadSHA, BaseSHA: request.ExpectedBaseSHA, Method: "squash", MergeSHA: observed.MergeSHA, MergedAt: observed.MergedAt.UTC()}
	if inspection.Merge != nil {
		if *inspection.Merge != merge {
			return ProductionMergeResult{}, c.rejectMergeConflict(ctx, run, "persisted merge evidence conflicts with GitHub observation", nil)
		}
		merge = *inspection.Merge
	}
	if err := store.SaveMerge(ctx, merge); err != nil {
		return ProductionMergeResult{}, classifyServiceError(err)
	}
	if side.Status != "observed" {
		result, err := json.Marshal(map[string]string{"merge_method": "squash", "merge_sha": merge.MergeSHA, "merged_at": merge.MergedAt.Format(time.RFC3339Nano)})
		if err != nil {
			return ProductionMergeResult{}, serviceError(ErrorInternal, "encode squash merge result", err)
		}
		side.Status, side.ResultJSON, side.ObservedAt = "observed", string(result), time.Now().UTC()
		if err := store.FinishSideEffect(ctx, side); err != nil {
			return ProductionMergeResult{}, classifyServiceError(err)
		}
	}
	if err := c.store.Transition(ctx, run.ID, domain.StateMerging, domain.StateAwaitingLinearCompletion, "GitHub squash merge observed; awaiting Linear completion", merge.MergeSHA, run.CandidateHead); err != nil {
		return ProductionMergeResult{}, classifyServiceError(err)
	}
	next, err := c.store.GetRun(ctx, run.ID)
	if err != nil {
		return ProductionMergeResult{}, classifyServiceError(err)
	}
	return ProductionMergeResult{Action: ProductionStop, Run: projectRunResult(next), PullRequest: request.PullRequest, MergeSHA: merge.MergeSHA, Idempotent: idempotent}, nil
}

func validateObservedSquashMerge(request SquashMergeRequest, observed domain.PullRequest) error {
	if observed.Number != request.PullRequest || observed.HeadSHA != request.ExpectedHeadSHA || observed.HeadBranch != request.HeadBranch || observed.BaseBranch != request.BaseBranch || observed.OwnershipKey != request.OwnershipKey {
		return errors.New("merged pull request identity does not match immutable intent")
	}
	if !strings.EqualFold(observed.State, "closed") || !observed.Merged || strings.TrimSpace(observed.MergeSHA) == "" || observed.MergedAt.IsZero() {
		return errors.New("GitHub did not provide a complete merged pull request observation")
	}
	return nil
}

func mergeSideEffect(records []SideEffectRecord, head string) (SideEffectRecord, bool) {
	for _, record := range records {
		if record.Kind == "squash_merge" && record.IdempotencyKey == head {
			return record, true
		}
	}
	return SideEffectRecord{}, false
}

func (c *ProductionCoordinator) recordMergeUncertain(ctx context.Context, run Run, store mergeStore, side SideEffectRecord, cause error) error {
	if side.Status != "observed" {
		result, _ := json.Marshal(map[string]string{"category": "merge_response_unavailable"})
		side.Status, side.ResultJSON, side.ObservedAt = "failed", string(result), time.Now().UTC()
		if err := store.FinishSideEffect(ctx, side); err != nil {
			return classifyServiceError(err)
		}
	}
	_ = c.store.SetLastError(ctx, run.ID, "squash merge outcome requires GitHub reconciliation before retry")
	return serviceError(ErrorUnavailable, "squash merge was not observed; retry must re-read GitHub state", cause)
}

func (c *ProductionCoordinator) recordMergeRejected(ctx context.Context, run Run, store mergeStore, side SideEffectRecord, cause error) error {
	if side.Status != "observed" {
		result, _ := json.Marshal(map[string]string{"category": "merge_rejected"})
		side.Status, side.ResultJSON, side.ObservedAt = "failed", string(result), time.Now().UTC()
		if err := store.FinishSideEffect(ctx, side); err != nil {
			return classifyServiceError(err)
		}
	}
	return c.rejectMergeConflict(ctx, run, "GitHub rejected or invalidated the squash merge", cause)
}

func (c *ProductionCoordinator) rejectMergeConflict(ctx context.Context, run Run, reason string, cause error) error {
	_ = c.store.SetLastError(ctx, run.ID, reason)
	if err := c.store.Transition(ctx, run.ID, domain.StateMerging, domain.StateManualIntervention, reason, "squash_merge", run.CandidateHead); err != nil {
		return classifyServiceError(err)
	}
	return serviceError(ErrorConflict, reason, cause)
}
