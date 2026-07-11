package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	gitadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/git"
	processadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/process"
	sqlitestore "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type fixtureRepository struct {
	OriginPath string `json:"origin_path"`
	SourcePath string `json:"source_path"`
}

func localFixtureDeliver(args []string) error {
	flags := flag.NewFlagSet("local fixture-deliver", flag.ContinueOnError)
	dbPath := flags.String("db", "", "SQLite controller database")
	runID, rest := splitLeadingRunID(args)
	if err := flags.Parse(rest); err != nil {
		return err
	}
	if runID == "" || *dbPath == "" {
		return errors.New("usage: ifan-loop local fixture-deliver <run-id> --db <controller.db>")
	}
	store, err := sqlitestore.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	ctx := context.Background()
	run, err := store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.State != domain.StateApprovalReady {
		return fmt.Errorf("fixture delivery requires approval_ready, got %s", run.State)
	}
	inspection, err := store.Inspect(ctx, runID)
	if err != nil {
		return err
	}
	if err := fixtureExactHeadGate(run, inspection); err != nil {
		return err
	}
	var repo fixtureRepository
	if err := json.Unmarshal([]byte(run.RepositoryConfigJSON), &repo); err != nil {
		return err
	}
	if !strings.Contains(repo.OriginPath, "Agent-Loop-Controller-lab.") {
		return errors.New("fixture delivery refuses non-lab origin")
	}
	if err := store.Transition(ctx, runID, domain.StateApprovalReady, domain.StatePushingBranch, "persist push intent", run.WorkingBranch, run.CandidateHead); err != nil {
		return err
	}
	intent, _ := json.Marshal(map[string]string{"branch": run.WorkingBranch, "head": run.CandidateHead})
	side, _, err := store.BeginSideEffect(ctx, application.SideEffectRecord{RunID: runID, Kind: "push", IdempotencyKey: run.CandidateHead, IntentJSON: string(intent), Attempt: 1})
	if err != nil {
		return err
	}
	remote, err := gitadapter.Publisher{Workspace: gitadapter.Workspace{}}.RemoteSHA(ctx, run.WorktreePath, run.WorkingBranch)
	if err != nil {
		return err
	}
	if remote != "" && remote != run.CandidateHead {
		return errors.New("remote branch SHA conflicts with candidate")
	}
	if remote == "" {
		evidence, err := gitadapter.Publisher{Workspace: gitadapter.Workspace{}, Process: processadapter.OSRunner{}}.Push(ctx, run.WorktreePath, run.WorkingBranch, run.CandidateHead, filepath.Join(run.ArtifactRoot, "push.stdout"), filepath.Join(run.ArtifactRoot, "push.stderr"))
		side.StdoutPath = evidence.Stdout
		side.StderrPath = evidence.Stderr
		if err != nil {
			return err
		}
	}
	side.Status = "observed"
	side.ResultJSON = fmt.Sprintf(`{"remote_ref":"refs/heads/%s","pushed_sha":"%s"}`, run.WorkingBranch, run.CandidateHead)
	side.ObservedAt = time.Now().UTC()
	if err := store.FinishSideEffect(ctx, side); err != nil {
		return err
	}
	if err := store.Transition(ctx, runID, domain.StatePushingBranch, domain.StateBranchPushed, "remote exact SHA observed", side.ResultJSON, run.CandidateHead); err != nil {
		return err
	}
	if err := store.Transition(ctx, runID, domain.StateBranchPushed, domain.StateOpeningPR, "persist PR intent", "fake-github", run.CandidateHead); err != nil {
		return err
	}
	var task domain.CodingTask
	if err := json.Unmarshal([]byte(run.NormalizedTaskJSON), &task); err != nil {
		return err
	}
	body, err := domain.PRBody(task, "controller-owned verifier pass", "fresh Sol review pass", run.IdempotencyKey)
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(body))
	pr := domain.PullRequest{Number: 1, URL: "https://fixture.invalid/pr/1", NodeID: "fixture-pr-1", HeadBranch: run.WorkingBranch, BaseBranch: run.BaseBranch, HeadSHA: run.CandidateHead, BaseSHA: run.BaseSHA, BodyDigest: hex.EncodeToString(digest[:]), OwnershipKey: run.IdempotencyKey, State: "OPEN"}
	if err := store.SavePullRequest(ctx, runID, pr); err != nil {
		return err
	}
	if err := store.Transition(ctx, runID, domain.StateOpeningPR, domain.StatePROpen, "fake GitHub PR observed", pr.URL, run.CandidateHead); err != nil {
		return err
	}
	if err := store.Transition(ctx, runID, domain.StatePROpen, domain.StateReconcilingReviews, "poll fixture GitHub", "fixture poll 1", run.CandidateHead); err != nil {
		return err
	}
	now := time.Now().UTC()
	pending := application.PollObservation{RunID: runID, PRNumber: 1, Attempt: 1, HeadSHA: run.CandidateHead, Status: "pending", SnapshotJSON: `{"coderabbit_status":"pending"}`, ObservedAt: now}
	passing := application.PollObservation{RunID: runID, PRNumber: 1, Attempt: 2, HeadSHA: run.CandidateHead, Status: "pass", SnapshotJSON: `{"checks":"pass","coderabbit_status":"pass"}`, ObservedAt: now}
	if err := store.SavePollObservation(ctx, pending); err != nil {
		return err
	}
	if err := store.SavePollObservation(ctx, passing); err != nil {
		return err
	}
	if err := store.Transition(ctx, runID, domain.StateReconcilingReviews, domain.StateAwaitingHumanApproval, "checks and CodeRabbit pass", "fixture observations", run.CandidateHead); err != nil {
		return err
	}
	approval := domain.HumanApproval{PRNumber: 1, Approver: "I-Fan (simulated fixture)", Source: "fixture-explicit-approval", ApprovedSHA: run.CandidateHead, CIStatus: "pass", CodeRabbit: "pass", ReviewSHA: run.CandidateHead, ApprovedAt: now}
	if err := store.SaveHumanApproval(ctx, runID, approval); err != nil {
		return err
	}
	if err := store.Transition(ctx, runID, domain.StateAwaitingHumanApproval, domain.StateMerging, "simulated final approval bound to exact SHA", "fixture approval", run.CandidateHead); err != nil {
		return err
	}
	mergeSHA, err := fixtureSquashMerge(repo, run)
	if err != nil {
		return err
	}
	merge := application.MergeRecord{RunID: runID, PRNumber: 1, PreMergeSHA: run.CandidateHead, BaseSHA: run.BaseSHA, Method: "squash", MergeSHA: mergeSHA, MergedAt: time.Now().UTC()}
	if err := store.SaveMerge(ctx, merge); err != nil {
		return err
	}
	if err := store.Transition(ctx, runID, domain.StateMerging, domain.StateCleaning, "fixture squash merge observed", mergeSHA, run.CandidateHead); err != nil {
		return err
	}
	if err := fixtureCleanup(ctx, store, repo, run); err != nil {
		return err
	}
	if err := store.Transition(ctx, runID, domain.StateCleaning, domain.StateCompleted, "owned fixture resources cleaned", "fixture cleanup", mergeSHA); err != nil {
		return err
	}
	result, err := store.Inspect(ctx, runID)
	if err != nil {
		return err
	}
	return printJSON(result)
}

func fixtureExactHeadGate(run application.Run, inspection application.RunInspection) error {
	latestVerification, latestReview := "", ""
	for _, v := range inspection.Verifications {
		if v.ExitCode == 0 && v.VerifiedHead == run.CandidateHead {
			latestVerification = v.VerifiedHead
		}
	}
	for _, r := range inspection.Reviews {
		if r.Verdict == "pass" && r.ReviewedHead == run.CandidateHead {
			latestReview = r.ReviewedHead
		}
	}
	if latestVerification != run.CandidateHead || latestReview != run.CandidateHead {
		return errors.New("candidate lacks exact-HEAD verification and fresh review")
	}
	return nil
}
func fixtureSquashMerge(repo fixtureRepository, run application.Run) (string, error) {
	if _, err := runCommand(repo.SourcePath, "git", "fetch", "origin", run.WorkingBranch); err != nil {
		return "", err
	}
	if _, err := runCommand(repo.SourcePath, "git", "checkout", run.BaseBranch); err != nil {
		return "", err
	}
	if _, err := runCommand(repo.SourcePath, "git", "merge", "--squash", "refs/remotes/origin/"+run.WorkingBranch); err != nil {
		return "", err
	}
	if _, err := runCommand(repo.SourcePath, "git", "commit", "-m", "Fixture squash merge"); err != nil {
		return "", err
	}
	sha, err := runCommand(repo.SourcePath, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if _, err := runCommand(repo.SourcePath, "git", "push", "origin", "refs/heads/"+run.BaseBranch+":refs/heads/"+run.BaseBranch); err != nil {
		return "", err
	}
	return strings.TrimSpace(sha), nil
}
func fixtureCleanup(ctx context.Context, store *sqlitestore.Store, repo fixtureRepository, run application.Run) error {
	steps := []application.CleanupRecord{{RunID: run.ID, Kind: "worktree", Name: run.WorktreePath, Status: "intent"}, {RunID: run.ID, Kind: "remote_branch", Name: run.WorkingBranch, Status: "intent"}, {RunID: run.ID, Kind: "local_branch", Name: run.WorkingBranch, Status: "intent"}}
	for _, step := range steps {
		if err := store.UpsertCleanup(ctx, step); err != nil {
			return err
		}
		var err error
		switch step.Kind {
		case "worktree":
			_, err = runCommand(repo.SourcePath, "git", "worktree", "remove", run.WorktreePath)
		case "remote_branch":
			_, err = runCommand(repo.SourcePath, "git", "push", "origin", "--delete", "refs/heads/"+run.WorkingBranch)
		case "local_branch":
			ref := "refs/heads/" + run.WorkingBranch
			var actual string
			actual, err = runCommand(repo.SourcePath, "git", "rev-parse", "--verify", ref)
			if err == nil && strings.TrimSpace(actual) != run.CandidateHead {
				err = errors.New("local branch no longer matches persisted candidate")
			}
			if err == nil {
				_, err = runCommand(repo.SourcePath, "git", "update-ref", "-d", ref, run.CandidateHead)
			}
		}
		if err != nil {
			step.Status = "failed"
			step.LastError = err.Error()
		} else {
			step.Status = "deleted"
		}
		if save := store.UpsertCleanup(ctx, step); save != nil {
			return save
		}
		if err != nil {
			return err
		}
	}
	return nil
}
func runCommand(directory, program string, args ...string) (string, error) {
	command := exec.Command(program, args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %v: %w: %s", program, args, err, output)
	}
	return string(output), nil
}
