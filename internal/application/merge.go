package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
type MergeRejectedError struct{ Cause error }

func (e *MergeRejectedError) Error() string { return e.Cause.Error() }
func (e *MergeRejectedError) Unwrap() error { return e.Cause }

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
	evidence, observations, metadata, readErr := reader.Read(ctx, request.PullRequest, request.ExpectedHeadSHA)
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
	side, _, err := stores.BeginSideEffect(ctx, SideEffectRecord{RunID: run.ID, Kind: "squash_merge", IdempotencyKey: run.CandidateHead, IntentJSON: string(intentJSON), Attempt: 1})
	if err != nil {
		return ProductionMergeResult{}, classifyServiceError(err)
	}
	merged, mergeObservations, mergeMetadata, mergeErr := merger.SquashMerge(ctx, request)
	persistErr := persistMergeRequests(ctx, stores, run.ID, mergeObservations, mergeMetadata)
	if mergeErr != nil {
		var rejected *MergeRejectedError
		if errors.As(mergeErr, &rejected) {
			return ProductionMergeResult{}, c.recordMergeRejected(ctx, run, stores, side, rejected)
		}
		return ProductionMergeResult{}, c.recordMergeUncertain(ctx, run, stores, side, mergeErr)
	}
	if persistErr != nil {
		return ProductionMergeResult{}, classifyServiceError(persistErr)
	}
	return c.completeObservedMerge(ctx, run, inspection, request, merged, stores, side, false)
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
		return errors.New("required checks or CodeRabbit are not passing for the exact head")
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
	if err := c.store.Transition(ctx, run.ID, domain.StateMerging, domain.StateCleaning, "GitHub squash merge observed", merge.MergeSHA, run.CandidateHead); err != nil {
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
