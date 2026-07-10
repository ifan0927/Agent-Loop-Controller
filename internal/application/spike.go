package application

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/codex"
	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/verifier"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type CodexExecutor interface {
	Preflight(context.Context, string) (codex.PreflightEvidence, error)
	Implementation(context.Context, codex.CommandSpec, string) (codex.StructuredResult[domain.AgentOutcome], error)
	Review(context.Context, codex.CommandSpec, string) (codex.StructuredResult[domain.ReviewOutcome], error)
}

type VerificationRunner interface {
	Run(context.Context, []string, string, string, string) (verifier.Evidence, error)
}

type GitWorkspace interface {
	Head(context.Context, string) (string, error)
	Branch(context.Context, string) (string, error)
	Status(context.Context, string) (string, error)
	ValidateRemoteBase(context.Context, string, string, string) error
	CommitCandidate(context.Context, string, string) (string, error)
}

type Spike struct {
	planner Planner
	codex   CodexExecutor
	verify  VerificationRunner
	git     GitWorkspace
}

type SpikeResult struct {
	Status                  string                  `json:"status"`
	CandidateHeadSHA        string                  `json:"candidate_head_sha"`
	VerificationHeadSHA     string                  `json:"verification_head_sha"`
	ImplementationSessionID string                  `json:"implementation_session_id"`
	ReviewSessionID         string                  `json:"review_session_id"`
	CodexPreflight          codex.PreflightEvidence `json:"codex_preflight"`
	Review                  domain.ReviewOutcome    `json:"review"`
}

func NewSpike(binary string, executor CodexExecutor, verification VerificationRunner, workspace GitWorkspace) Spike {
	return Spike{planner: NewPlanner(binary), codex: executor, verify: verification, git: workspace}
}

func (s Spike) Run(ctx context.Context, task domain.CodingTask, workspace, artifacts string) (SpikeResult, error) {
	plan, err := s.planner.Build(task, workspace, artifacts)
	if err != nil {
		return SpikeResult{}, fmt.Errorf("plan spike: %w", err)
	}
	if err := MaterializeArtifacts(plan.Artifacts); err != nil {
		return SpikeResult{}, fmt.Errorf("materialize artifacts: %w", err)
	}
	preflight, err := s.codex.Preflight(ctx, artifacts)
	if err != nil {
		return SpikeResult{}, fmt.Errorf("Codex preflight: %w", err)
	}
	branch, err := s.git.Branch(ctx, workspace)
	if err != nil {
		return SpikeResult{}, fmt.Errorf("read working branch: %w", err)
	}
	if branch != task.WorkingBranch {
		return SpikeResult{}, fmt.Errorf("working branch = %s, want %s", branch, task.WorkingBranch)
	}
	implementationBaseHead, err := s.git.Head(ctx, workspace)
	if err != nil {
		return SpikeResult{}, fmt.Errorf("read implementation base HEAD: %w", err)
	}
	if err := s.git.ValidateRemoteBase(ctx, workspace, task.BaseBranch, implementationBaseHead); err != nil {
		return SpikeResult{}, fmt.Errorf("validate implementation base: %w", err)
	}
	initialStatus, err := s.git.Status(ctx, workspace)
	if err != nil {
		return SpikeResult{}, fmt.Errorf("read initial worktree status: %w", err)
	}
	if strings.TrimSpace(initialStatus) != "" {
		return SpikeResult{}, fmt.Errorf("implementation worktree must start clean")
	}
	implementation, err := s.codex.Implementation(ctx, plan.Implementation, artifacts)
	if err != nil {
		return SpikeResult{}, fmt.Errorf("implementation: %w", err)
	}
	if implementation.Outcome.Status != domain.AgentCompleted {
		return SpikeResult{}, fmt.Errorf("implementation stopped with status %s: %s", implementation.Outcome.Status, implementation.Outcome.Summary)
	}
	afterImplementationHead, err := s.git.Head(ctx, workspace)
	if err != nil {
		return SpikeResult{}, fmt.Errorf("read HEAD after implementation: %w", err)
	}
	if afterImplementationHead != implementationBaseHead {
		return SpikeResult{}, fmt.Errorf("Codex implementation changed HEAD; candidate commits are controller-owned")
	}
	if err := s.requireWorkingBranch(ctx, workspace, task.WorkingBranch, "after implementation"); err != nil {
		return SpikeResult{}, err
	}
	implementationStatus, err := s.git.Status(ctx, workspace)
	if err != nil {
		return SpikeResult{}, fmt.Errorf("read implementation status: %w", err)
	}
	if _, err := s.verify.Run(ctx, task.VerifierIDs, workspace, artifacts, "precommit"); err != nil {
		return SpikeResult{}, fmt.Errorf("pre-commit verification: %w", err)
	}
	afterPrecommitVerificationStatus, err := s.git.Status(ctx, workspace)
	if err != nil {
		return SpikeResult{}, fmt.Errorf("read status after pre-commit verification: %w", err)
	}
	if afterPrecommitVerificationStatus != implementationStatus {
		return SpikeResult{}, fmt.Errorf("pre-commit verifier mutated the implementation worktree")
	}
	if err := s.requireWorkingBranch(ctx, workspace, task.WorkingBranch, "after pre-commit verification"); err != nil {
		return SpikeResult{}, err
	}
	candidateHead, err := s.git.CommitCandidate(ctx, workspace, "Phase 1A fixture candidate")
	if err != nil {
		return SpikeResult{}, err
	}
	status, err := s.git.Status(ctx, workspace)
	if err != nil {
		return SpikeResult{}, fmt.Errorf("status after candidate commit: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		return SpikeResult{}, fmt.Errorf("candidate worktree is not clean after commit")
	}
	verification, err := s.verify.Run(ctx, task.VerifierIDs, workspace, artifacts, "candidate")
	if err != nil {
		return SpikeResult{}, fmt.Errorf("candidate verification: %w", err)
	}
	if verification.VerifiedHeadSHA != candidateHead {
		return SpikeResult{}, fmt.Errorf("controller verification head does not match candidate HEAD")
	}
	if err := s.requireWorkingBranch(ctx, workspace, task.WorkingBranch, "after candidate verification"); err != nil {
		return SpikeResult{}, err
	}
	beforeReviewHead, err := s.git.Head(ctx, workspace)
	if err != nil {
		return SpikeResult{}, fmt.Errorf("head before fresh review: %w", err)
	}
	beforeReviewStatus, err := s.git.Status(ctx, workspace)
	if err != nil {
		return SpikeResult{}, fmt.Errorf("status before fresh review: %w", err)
	}
	if beforeReviewHead != candidateHead || strings.TrimSpace(beforeReviewStatus) != "" {
		return SpikeResult{}, fmt.Errorf("candidate must be clean and unchanged before fresh review")
	}
	if err := s.git.ValidateRemoteBase(ctx, workspace, task.BaseBranch, candidateHead); err != nil {
		return SpikeResult{}, fmt.Errorf("validate candidate base: %w", err)
	}
	plan.FreshReview.Stdin += fmt.Sprintf(`
Controller candidate HEAD (review exactly this commit): %s

Controller-owned verification already passed for this exact HEAD using verifier
IDs: %s. That head-bound evidence is authoritative. Continue to report every
real code finding, but do not return failed solely because the read-only review
sandbox cannot rerun a command that needs to create build caches or temporary
files.
`, candidateHead, strings.Join(task.VerifierIDs, ", "))
	review, err := s.codex.Review(ctx, plan.FreshReview, artifacts)
	if err != nil {
		return SpikeResult{}, fmt.Errorf("fresh review: %w", err)
	}
	afterHead, err := s.git.Head(ctx, workspace)
	if err != nil {
		return SpikeResult{}, fmt.Errorf("head after fresh review: %w", err)
	}
	afterStatus, err := s.git.Status(ctx, workspace)
	if err != nil {
		return SpikeResult{}, fmt.Errorf("status after fresh review: %w", err)
	}
	if afterHead != candidateHead || strings.TrimSpace(afterStatus) != "" {
		return SpikeResult{}, fmt.Errorf("fresh review mutated the candidate worktree")
	}
	if err := s.requireWorkingBranch(ctx, workspace, task.WorkingBranch, "after fresh review"); err != nil {
		return SpikeResult{}, err
	}
	if err := AuthorizePROpen(domain.StateFreshReview, PROpenEvidence{
		Review: review.Outcome, CurrentHeadSHA: afterHead, VerificationHeadSHA: verification.VerifiedHeadSHA,
	}); err != nil {
		return SpikeResult{}, fmt.Errorf("approval-ready authorization: %w", err)
	}
	result := SpikeResult{
		Status: "approval_ready_simulation", CandidateHeadSHA: candidateHead,
		VerificationHeadSHA:     verification.VerifiedHeadSHA,
		ImplementationSessionID: implementation.SessionID, ReviewSessionID: review.SessionID,
		CodexPreflight: preflight, Review: review.Outcome,
	}
	if err := writeSpikeResult(filepath.Join(artifacts, "approval-ready.json"), result); err != nil {
		return SpikeResult{}, err
	}
	return result, nil
}

func (s Spike) requireWorkingBranch(ctx context.Context, workspace, expected, phase string) error {
	branch, err := s.git.Branch(ctx, workspace)
	if err != nil {
		return fmt.Errorf("read working branch %s: %w", phase, err)
	}
	if branch != expected {
		return fmt.Errorf("working branch %s = %s, want %s", phase, branch, expected)
	}
	return nil
}

func writeSpikeResult(path string, result SpikeResult) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create approval-ready evidence: %w", err)
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	encodeErr := encoder.Encode(result)
	closeErr := file.Close()
	if encodeErr != nil {
		return encodeErr
	}
	return closeErr
}
