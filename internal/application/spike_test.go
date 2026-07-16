package application

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/codex"
	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/verifier"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type fakeSpikeGit struct {
	branch string
	head   string
	status string
}

func (g *fakeSpikeGit) Head(context.Context, string) (string, error)                     { return g.head, nil }
func (g *fakeSpikeGit) Branch(context.Context, string) (string, error)                   { return g.branch, nil }
func (g *fakeSpikeGit) Status(context.Context, string) (string, error)                   { return g.status, nil }
func (g *fakeSpikeGit) ValidateRemoteBase(context.Context, string, string, string) error { return nil }
func (g *fakeSpikeGit) CommitCandidate(context.Context, string, string) (string, error) {
	g.head = "candidate"
	g.status = ""
	return g.head, nil
}

type fakeSpikeVerifier struct {
	git                   *fakeSpikeGit
	candidateHeadOverride string
}

type branchSwitchingVerifier struct {
	git *fakeSpikeGit
}

func (v branchSwitchingVerifier) Run(_ context.Context, _ []string, _, _, label string) (verifier.Evidence, error) {
	if label == "precommit" {
		v.git.branch = "other/branch"
	}
	return verifier.Evidence{VerifiedHeadSHA: v.git.head}, nil
}

func (v fakeSpikeVerifier) Run(_ context.Context, _ []string, _, _, label string) (verifier.Evidence, error) {
	head := v.git.head
	if label == "candidate" && v.candidateHeadOverride != "" {
		head = v.candidateHeadOverride
	}
	return verifier.Evidence{VerifiedHeadSHA: head}, nil
}

type fakeSpikeCodex struct {
	review       domain.ReviewOutcome
	reviewHook   func()
	reviewPrompt *string
}

func (f fakeSpikeCodex) Preflight(context.Context, string, string) (codex.PreflightEvidence, error) {
	return codex.PreflightEvidence{Version: "codex-cli fake"}, nil
}

func (f fakeSpikeCodex) Implementation(context.Context, codex.CommandSpec, string) (codex.StructuredResult[domain.AgentOutcome], error) {
	return codex.StructuredResult[domain.AgentOutcome]{
		SessionID: "implementation-session",
		Outcome:   domain.AgentOutcome{Status: domain.AgentCompleted, Summary: "implemented"},
	}, nil
}

func (f fakeSpikeCodex) Review(_ context.Context, spec codex.CommandSpec, _ string) (codex.StructuredResult[domain.ReviewOutcome], error) {
	if f.reviewPrompt != nil {
		*f.reviewPrompt = spec.Stdin
	}
	if f.reviewHook != nil {
		f.reviewHook()
	}
	return codex.StructuredResult[domain.ReviewOutcome]{SessionID: "review-session", Outcome: f.review}, nil
}

func TestSpikeRejectsReviewFindings(t *testing.T) {
	finding := domain.ReviewFinding{ID: "f1", Severity: "high", Title: "Bug", Body: "Fix it"}
	_, err := runFakeSpike(t, fakeSpikeCodex{review: domain.ReviewOutcome{
		Verdict: domain.ReviewFindings, Summary: "finding", ReviewedHeadSHA: "candidate", Findings: []domain.ReviewFinding{finding},
	}}, "")
	if err == nil || !strings.Contains(err.Error(), "passing fresh review") {
		t.Fatalf("error = %v", err)
	}
}

func TestSpikeRejectsReviewedSHAMismatch(t *testing.T) {
	_, err := runFakeSpike(t, passingReview("old"), "")
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error = %v", err)
	}
}

func TestSpikeRejectsVerificationSHAMismatch(t *testing.T) {
	_, err := runFakeSpike(t, passingReview("candidate"), "old")
	if err == nil || !strings.Contains(err.Error(), "verification head") {
		t.Fatalf("error = %v", err)
	}
}

func TestSpikeDetectsReviewMutation(t *testing.T) {
	git := &fakeSpikeGit{branch: validTask().WorkingBranch, head: "base"}
	codexFake := passingReview("candidate")
	codexFake.reviewHook = func() { git.status = " M changed.go" }
	_, err := runFakeSpikeWithGit(t, git, codexFake, "")
	if err == nil || !strings.Contains(err.Error(), "mutated") {
		t.Fatalf("error = %v", err)
	}
}

func TestSpikeRejectsVerifierBranchSwitch(t *testing.T) {
	git := &fakeSpikeGit{branch: validTask().WorkingBranch, head: "base"}
	workspace := t.TempDir()
	artifacts := t.TempDir()
	spike := NewSpike("codex", passingReview("candidate"), branchSwitchingVerifier{git: git}, git)
	_, err := spike.Run(context.Background(), validTask(), workspace, artifacts)
	if err == nil || !strings.Contains(err.Error(), "after pre-commit verification") {
		t.Fatalf("error = %v", err)
	}
}

func TestSpikeApprovalReadyHappyPath(t *testing.T) {
	prompt := ""
	codexFake := passingReview("candidate")
	codexFake.reviewPrompt = &prompt
	result, artifacts, err := runFakeSpikeWithArtifacts(t, codexFake, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "approval_ready_simulation" || result.CandidateHeadSHA != "candidate" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(artifacts, "approval-ready.json")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "authoritative") || !strings.Contains(prompt, "candidate") {
		t.Fatalf("fresh review prompt lacks exact-head verification evidence: %s", prompt)
	}
}

func passingReview(head string) fakeSpikeCodex {
	return fakeSpikeCodex{review: domain.ReviewOutcome{Verdict: domain.ReviewPass, Summary: "ready", ReviewedHeadSHA: head}}
}

func runFakeSpike(t *testing.T, codexFake fakeSpikeCodex, verificationOverride string) (SpikeResult, error) {
	result, _, err := runFakeSpikeWithArtifacts(t, codexFake, verificationOverride)
	return result, err
}

func runFakeSpikeWithArtifacts(t *testing.T, codexFake fakeSpikeCodex, verificationOverride string) (SpikeResult, string, error) {
	git := &fakeSpikeGit{branch: validTask().WorkingBranch, head: "base"}
	result, artifacts, err := runFakeSpikeWithGitAndArtifacts(t, git, codexFake, verificationOverride)
	return result, artifacts, err
}

func runFakeSpikeWithGit(t *testing.T, git *fakeSpikeGit, codexFake fakeSpikeCodex, verificationOverride string) (SpikeResult, error) {
	result, _, err := runFakeSpikeWithGitAndArtifacts(t, git, codexFake, verificationOverride)
	return result, err
}

func runFakeSpikeWithGitAndArtifacts(t *testing.T, git *fakeSpikeGit, codexFake fakeSpikeCodex, verificationOverride string) (SpikeResult, string, error) {
	t.Helper()
	workspace := t.TempDir()
	artifacts := t.TempDir()
	spike := NewSpike("codex", codexFake, fakeSpikeVerifier{git: git, candidateHeadOverride: verificationOverride}, git)
	result, err := spike.Run(context.Background(), validTask(), workspace, artifacts)
	return result, artifacts, err
}
