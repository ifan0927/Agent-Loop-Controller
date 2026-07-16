package application

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type ExternalMergeCandidateValidator interface {
	ValidateExternalMergeCandidate(context.Context, string) error
}

type ExternalMergeVerificationRequest struct {
	Repository   string
	SourcePath   string
	OriginPath   string
	BaseBranch   string
	CandidateSHA string
	MergeSHA     string
}

type ExternalMergeVerification struct {
	CandidateSHA string `json:"candidate_sha"`
	MergeSHA     string `json:"merge_sha"`
	BaseSHA      string `json:"remote_base_sha"`
	TreeSHA      string `json:"tree_sha"`
}

type ExternalMergeVerifier interface {
	Verify(context.Context, ExternalMergeVerificationRequest) (ExternalMergeVerification, error)
}

type ProductionAcceptExternalMergeCommand struct {
	Requester      Requester
	RunID          string
	Repository     string
	ExpectedState  domain.State
	IdempotencyKey string
}

type ProductionAcceptExternalMergeResult struct {
	Action       ProductionAction          `json:"action"`
	Run          RunResult                 `json:"run"`
	PullRequest  int64                     `json:"pull_request"`
	MergeSHA     string                    `json:"merge_sha"`
	Verification ExternalMergeVerification `json:"verification"`
	Idempotent   bool                      `json:"idempotent"`
}

func (c *ProductionCoordinator) AcceptExternalMerge(ctx context.Context, command ProductionAcceptExternalMergeCommand, validator ExternalMergeCandidateValidator, verifier ExternalMergeVerifier) (ProductionAcceptExternalMergeResult, error) {
	if validator == nil || verifier == nil || command.RunID == "" || command.Repository == "" || command.ExpectedState == "" || command.IdempotencyKey == "" {
		return ProductionAcceptExternalMergeResult{}, serviceError(ErrorInvalidInput, "external merge command and validators are required", nil)
	}
	run, err := c.store.GetRun(ctx, command.RunID)
	if err != nil {
		return ProductionAcceptExternalMergeResult{}, classifyServiceError(err)
	}
	if run.Repository != command.Repository || run.State != command.ExpectedState || run.IdempotencyKey != command.IdempotencyKey {
		return ProductionAcceptExternalMergeResult{}, serviceError(ErrorConflict, "run authority or state changed before external merge acceptance", nil)
	}
	if err := authorizePersistedRequester(run, command.Requester); err != nil {
		return ProductionAcceptExternalMergeResult{}, err
	}
	if run.State != domain.StateManualIntervention {
		return ProductionAcceptExternalMergeResult{}, serviceError(ErrorConflict, "external merge acceptance requires manual_intervention", nil)
	}
	if err := validator.ValidateExternalMergeCandidate(ctx, run.ID); err != nil {
		return ProductionAcceptExternalMergeResult{}, serviceError(ErrorConflict, "exact local candidate evidence is no longer valid", err)
	}
	inspection, err := c.store.Inspect(ctx, run.ID)
	if err != nil {
		return ProductionAcceptExternalMergeResult{}, classifyServiceError(err)
	}
	pr, err := validateExternalMergeEvidence(run, inspection)
	if err != nil {
		return ProductionAcceptExternalMergeResult{}, serviceError(ErrorConflict, "persisted external merge evidence is incomplete", err)
	}
	var repository LocalRepository
	if json.Unmarshal([]byte(run.RepositoryConfigJSON), &repository) != nil || repository.CanonicalRepository != run.Repository || repository.BaseBranch != run.BaseBranch || repository.SourcePath == "" || repository.OriginPath == "" {
		return ProductionAcceptExternalMergeResult{}, serviceError(ErrorConflict, "persisted repository authority is invalid", nil)
	}
	verified, err := verifier.Verify(ctx, ExternalMergeVerificationRequest{Repository: run.Repository, SourcePath: repository.SourcePath, OriginPath: repository.OriginPath, BaseBranch: run.BaseBranch, CandidateSHA: run.CandidateHead, MergeSHA: pr.MergeSHA})
	if err != nil {
		return ProductionAcceptExternalMergeResult{}, serviceError(ErrorConflict, "external merge Git authority could not be proven", err)
	}
	if verified.CandidateSHA != run.CandidateHead || verified.MergeSHA != pr.MergeSHA || !validFullSHA(verified.BaseSHA) || !validFullSHA(verified.TreeSHA) {
		return ProductionAcceptExternalMergeResult{}, serviceError(ErrorConflict, "external merge verification result is not bound to the run", nil)
	}
	merge := MergeRecord{RunID: run.ID, PRNumber: pr.Number, PreMergeSHA: run.CandidateHead, BaseSHA: run.BaseSHA, Method: "external", MergeSHA: pr.MergeSHA, MergedAt: pr.MergedAt.UTC()}
	delivery, ok := c.store.(DeliveryStore)
	if !ok {
		return ProductionAcceptExternalMergeResult{}, serviceError(ErrorInternal, "configured store cannot persist merge evidence", nil)
	}
	if inspection.Merge != nil && *inspection.Merge != merge {
		return ProductionAcceptExternalMergeResult{}, serviceError(ErrorConflict, "persisted merge evidence conflicts with external merge", nil)
	}
	idempotent := inspection.Merge != nil
	if err := delivery.SaveMerge(ctx, merge); err != nil {
		return ProductionAcceptExternalMergeResult{}, classifyServiceError(err)
	}
	if err := c.store.Transition(ctx, run.ID, domain.StateManualIntervention, domain.StateAwaitingLinearCompletion, "operator accepted exact-tree external merge", "external_merge:"+merge.MergeSHA, run.CandidateHead); err != nil {
		return ProductionAcceptExternalMergeResult{}, classifyServiceError(err)
	}
	updated, err := c.store.GetRun(ctx, run.ID)
	if err != nil {
		return ProductionAcceptExternalMergeResult{}, classifyServiceError(err)
	}
	return ProductionAcceptExternalMergeResult{Action: ProductionReconcileLinear, Run: projectRunResult(updated), PullRequest: pr.Number, MergeSHA: merge.MergeSHA, Verification: verified, Idempotent: idempotent}, nil
}

func validateExternalMergeEvidence(run Run, inspection RunInspection) (domain.PullRequest, error) {
	if len(inspection.Timeline) == 0 {
		return domain.PullRequest{}, errors.New("manual intervention transition is missing")
	}
	last := inspection.Timeline[len(inspection.Timeline)-1]
	if last.To != domain.StateManualIntervention || !strings.Contains(last.Reason, "outside the controller gate") || last.BoundHead != run.CandidateHead {
		return domain.PullRequest{}, errors.New("manual intervention was not caused by an observed external merge")
	}
	if inspection.PullRequest == nil || inspection.Approval == nil || inspection.GitHubEvidence == nil {
		return domain.PullRequest{}, errors.New("pull request, approval, and GitHub check evidence are required")
	}
	pr := *inspection.PullRequest
	if err := pr.ValidateOwnership(run.WorkingBranch, run.BaseBranch, run.CandidateHead, run.IdempotencyKey); err != nil {
		return domain.PullRequest{}, err
	}
	if !pr.Merged || !strings.EqualFold(pr.State, "closed") || !validFullSHA(pr.MergeSHA) || pr.MergedAt.IsZero() || pr.BaseSHA != run.BaseSHA {
		return domain.PullRequest{}, errors.New("merged pull request identity is incomplete")
	}
	evidence := inspection.GitHubEvidence
	if evidence.PullRequest.Number != pr.Number || evidence.PullRequest.HeadSHA != run.CandidateHead || evidence.PullRequest.MergeSHA != pr.MergeSHA || !evidence.PullRequest.Merged || evidence.RequiredChecksStatus() != domain.ReconciliationPass || len(evidence.UnknownEvents) != 0 {
		return domain.PullRequest{}, errors.New("required GitHub checks are not passing for the exact merged head")
	}
	if err := inspection.Approval.Authorizes(pr, run.CandidateHead); err != nil {
		return domain.PullRequest{}, fmt.Errorf("trusted human approval is invalid: %w", err)
	}
	return pr, nil
}

func validFullSHA(value string) bool {
	if len(value) != 40 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
