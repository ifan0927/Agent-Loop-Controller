package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	approvalPath := flags.String("approval", "", "explicit simulated human approval JSON")
	runID, rest := splitLeadingRunID(args)
	if err := flags.Parse(rest); err != nil {
		return err
	}
	if runID == "" || *dbPath == "" {
		return errors.New("usage: ifan-loop local fixture-deliver <run-id> --db <controller.db> --approval <approval.json>")
	}
	store, err := sqlitestore.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	leaseOwner := fmt.Sprintf("fixture-delivery-%d", time.Now().UTC().UnixNano())
	acquired, err := store.AcquireLease(ctx, runID, leaseOwner, time.Now().UTC().Add(2*time.Minute))
	if err != nil {
		return err
	}
	if !acquired {
		return errors.New("run is actively leased by another delivery controller")
	}
	done := make(chan struct{})
	renewDone := make(chan struct{})
	var stopOnce sync.Once
	stopLease := func() {
		stopOnce.Do(func() { close(done); <-renewDone; _ = store.ReleaseLease(context.Background(), runID, leaseOwner) })
	}
	defer stopLease()
	go func() {
		defer close(renewDone)
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				ok, renewErr := store.RenewLease(context.Background(), runID, leaseOwner, time.Now().UTC().Add(2*time.Minute))
				if renewErr != nil || !ok {
					cancel()
					return
				}
			}
		}
	}()
	for {
		run, err := store.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		var repo fixtureRepository
		if err := json.Unmarshal([]byte(run.RepositoryConfigJSON), &repo); err != nil {
			return err
		}
		if err := validateDisposableFixture(repo); err != nil {
			return err
		}
		switch run.State {
		case domain.StateApprovalReady:
			controller := newLocalController(store, "codex", filepath.Dir(run.WorktreePath))
			if err := controller.ValidateApprovalReady(ctx, runID); err != nil {
				return err
			}
			if err := store.Transition(ctx, runID, run.State, domain.StatePushingBranch, "persist push intent", run.WorkingBranch, run.CandidateHead); err != nil {
				return err
			}
		case domain.StatePushingBranch:
			if err := fixturePush(ctx, store, run); err != nil {
				return err
			}
		case domain.StateBranchPushed:
			if err := store.Transition(ctx, runID, run.State, domain.StateOpeningPR, "persist PR intent", "fake-github", run.CandidateHead); err != nil {
				return err
			}
		case domain.StateOpeningPR:
			if err := fixtureOpenPR(ctx, store, run); err != nil {
				return err
			}
		case domain.StatePROpen:
			if err := store.Transition(ctx, runID, run.State, domain.StateReconcilingReviews, "poll fixture GitHub", "fixture poll", run.CandidateHead); err != nil {
				return err
			}
		case domain.StateReconcilingReviews:
			if err := fixtureReconcile(ctx, store, run); err != nil {
				return err
			}
		case domain.StateRepairing:
			inspection, err := store.Inspect(ctx, runID)
			if err != nil {
				return err
			}
			var findings []application.FindingRecord
			for _, finding := range inspection.Findings {
				if finding.HeadSHA == run.CandidateHead && !finding.Resolved && !finding.Outdated {
					findings = append(findings, finding)
				}
			}
			if len(findings) == 0 {
				return errors.New("repairing state has no persisted actionable normalized findings")
			}
			controller := newLocalController(store, "codex", filepath.Dir(run.WorktreePath))
			stopLease()
			repaired, err := controller.Repair(ctx, runID, application.BuildRepairPrompt(findings))
			if err != nil {
				return err
			}
			return printJSON(repaired)
		case domain.StateAwaitingHumanApproval:
			if *approvalPath == "" {
				return errors.New("explicit --approval is required; controller cannot approve its own PR")
			}
			approval, err := readFixtureApproval(*approvalPath)
			if err != nil {
				return err
			}
			inspection, err := store.Inspect(ctx, runID)
			if err != nil {
				return err
			}
			if inspection.PullRequest == nil {
				return errors.New("missing persisted PR")
			}
			controller := newLocalController(store, "codex", filepath.Dir(run.WorktreePath))
			if err := controller.ValidateApprovalReady(ctx, runID); err != nil {
				return err
			}
			snapshot, err := latestPersistedPassingSnapshot(inspection, run.CandidateHead)
			if err != nil {
				return err
			}
			if err := application.AuthorizeFixtureMerge(run, *inspection.PullRequest, snapshot, approval, run.CandidateHead, run.CandidateHead); err != nil {
				return err
			}
			if err := store.SaveHumanApproval(ctx, runID, approval); err != nil {
				return err
			}
			if err := store.Transition(ctx, runID, run.State, domain.StateMerging, "explicit simulated final approval bound to exact SHA", *approvalPath, run.CandidateHead); err != nil {
				return err
			}
		case domain.StateMerging:
			gateInspection, gateErr := store.Inspect(ctx, runID)
			if gateErr != nil {
				return gateErr
			}
			if gateInspection.PullRequest == nil || gateInspection.Approval == nil {
				return errors.New("merge restart lacks persisted PR or human approval")
			}
			gateSnapshot, gateErr := latestPersistedPassingSnapshot(gateInspection, run.CandidateHead)
			if gateErr != nil {
				return gateErr
			}
			controller := newLocalController(store, "codex", filepath.Dir(run.WorktreePath))
			if gateErr = controller.ValidateApprovalReady(ctx, runID); gateErr != nil {
				return gateErr
			}
			if gateErr = application.AuthorizeFixtureMerge(run, *gateInspection.PullRequest, gateSnapshot, *gateInspection.Approval, run.CandidateHead, run.CandidateHead); gateErr != nil {
				return gateErr
			}
			intent, _ := json.Marshal(map[string]any{"pr_number": 1, "head": run.CandidateHead, "base_sha": run.BaseSHA, "method": "squash"})
			if _, _, err := store.BeginSideEffect(ctx, application.SideEffectRecord{RunID: runID, Kind: "merge", IdempotencyKey: run.CandidateHead, IntentJSON: string(intent), Attempt: 1}); err != nil {
				return err
			}
			inspection, inspectErr := store.Inspect(ctx, runID)
			if inspectErr != nil {
				return inspectErr
			}
			mergeSHA, err := fixtureReconcileOrMerge(repo, run)
			if err != nil {
				return err
			}
			merge := application.MergeRecord{RunID: runID, PRNumber: 1, PreMergeSHA: run.CandidateHead, BaseSHA: run.BaseSHA, Method: "squash", MergeSHA: mergeSHA, MergedAt: time.Now().UTC()}
			if inspection.Merge != nil {
				existing := *inspection.Merge
				if existing.RunID != runID || existing.PRNumber != 1 || existing.PreMergeSHA != run.CandidateHead || existing.BaseSHA != run.BaseSHA || existing.Method != "squash" || existing.MergeSHA != mergeSHA {
					return errors.New("persisted merge evidence conflicts with reconciled merge")
				}
				merge = existing
			}
			if err := store.SaveMerge(ctx, merge); err != nil {
				return err
			}
			inspection, err = store.Inspect(ctx, runID)
			if err != nil {
				return err
			}
			for _, side := range inspection.SideEffects {
				if side.Kind == "merge" && side.IdempotencyKey == run.CandidateHead && side.Status != "observed" {
					side.Status = "observed"
					side.ResultJSON = fmt.Sprintf(`{"merge_sha":"%s"}`, mergeSHA)
					side.ObservedAt = time.Now().UTC()
					if err := store.FinishSideEffect(ctx, side); err != nil {
						return err
					}
				}
			}
			if err := store.Transition(ctx, runID, run.State, domain.StateCleaning, "fixture squash merge observed", mergeSHA, run.CandidateHead); err != nil {
				return err
			}
		case domain.StateCleaning:
			inspection, err := store.Inspect(ctx, runID)
			if err != nil {
				return err
			}
			if inspection.Merge == nil {
				return errors.New("cleanup lacks merge evidence")
			}
			if err := fixtureCleanup(ctx, store, repo, run); err != nil {
				return err
			}
			intent, _ := json.Marshal(map[string]string{"issue_id": run.IssueID, "merge_sha": inspection.Merge.MergeSHA, "expected_state": "Done via GitHub automation"})
			linear, _, err := store.BeginSideEffect(ctx, application.SideEffectRecord{RunID: runID, Kind: "linear_completion_reconciliation", IdempotencyKey: inspection.Merge.MergeSHA, IntentJSON: string(intent), Attempt: 1})
			if err != nil {
				return err
			}
			if linear.Status != "observed" {
				linear.Status = "observed"
				linear.ResultJSON = `{"status":"pending_automation","controller_write":false}`
				linear.ObservedAt = time.Now().UTC()
				if err := store.FinishSideEffect(ctx, linear); err != nil {
					return err
				}
			}
			if err := store.Transition(ctx, runID, run.State, domain.StateCompleted, "owned resources cleaned; Linear automation observation persisted", linear.ResultJSON, inspection.Merge.MergeSHA); err != nil {
				return err
			}
		case domain.StateCompleted:
			result, err := store.Inspect(ctx, runID)
			if err != nil {
				return err
			}
			return printJSON(result)
		default:
			return fmt.Errorf("fixture delivery cannot resume state %s", run.State)
		}
	}
}

func validateDisposableFixture(repo fixtureRepository) error {
	origin, err := filepath.EvalSymlinks(repo.OriginPath)
	if err != nil {
		return err
	}
	source, err := filepath.EvalSymlinks(repo.SourcePath)
	if err != nil {
		return err
	}
	root := filepath.Dir(origin)
	if filepath.Dir(source) != root || filepath.Base(origin) != "origin.git" || filepath.Base(source) != "source" || !strings.HasPrefix(filepath.Base(root), "Agent-Loop-Controller-lab.") {
		return errors.New("fixture repository is not canonically contained in a disposable lab")
	}
	configured, err := runCommand(source, "git", "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	configuredPath, err := filepath.EvalSymlinks(strings.TrimSpace(configured))
	if err != nil {
		return err
	}
	if configuredPath != origin {
		return errors.New("fixture source origin does not match disposable bare origin")
	}
	bare, err := runCommand(origin, "git", "rev-parse", "--is-bare-repository")
	if err != nil {
		return err
	}
	if strings.TrimSpace(bare) != "true" {
		return errors.New("fixture origin is not bare")
	}
	return nil
}

func fixturePush(ctx context.Context, store *sqlitestore.Store, run application.Run) error {
	controller := newLocalController(store, "codex", filepath.Dir(run.WorktreePath))
	if err := controller.ValidateApprovalReady(ctx, run.ID); err != nil {
		return err
	}
	intent, _ := json.Marshal(map[string]string{"branch": run.WorkingBranch, "head": run.CandidateHead})
	side, _, err := store.BeginSideEffect(ctx, application.SideEffectRecord{RunID: run.ID, Kind: "push", IdempotencyKey: run.CandidateHead, IntentJSON: string(intent), Attempt: 1})
	if err != nil {
		return err
	}
	remote, err := (gitadapter.Publisher{Workspace: gitadapter.Workspace{}}).RemoteSHA(ctx, run.WorktreePath, run.WorkingBranch)
	if err != nil {
		return err
	}
	if remote != "" && remote != run.CandidateHead {
		inspection, inspectErr := store.Inspect(ctx, run.ID)
		if inspectErr != nil {
			return inspectErr
		}
		if !previousObservedPush(inspection.SideEffects, remote) {
			return errors.New("remote branch SHA is not owned prior push evidence")
		}
		if _, ancestorErr := runCommand(run.WorktreePath, "git", "merge-base", "--is-ancestor", remote, run.CandidateHead); ancestorErr != nil {
			return errors.New("repair candidate is not a fast-forward of prior pushed SHA")
		}
		remote = ""
	}
	if remote == "" {
		evidence, pushErr := (gitadapter.Publisher{Workspace: gitadapter.Workspace{}, Process: processadapter.OSRunner{}}).Push(ctx, run.WorktreePath, run.WorkingBranch, run.CandidateHead, filepath.Join(run.ArtifactRoot, "push-"+run.CandidateHead+".stdout"), filepath.Join(run.ArtifactRoot, "push-"+run.CandidateHead+".stderr"))
		side.StdoutPath = evidence.Stdout
		side.StderrPath = evidence.Stderr
		if pushErr != nil {
			return pushErr
		}
	}
	side.ResultJSON = fmt.Sprintf(`{"remote_ref":"refs/heads/%s","pushed_sha":"%s"}`, run.WorkingBranch, run.CandidateHead)
	if side.Status != "observed" {
		side.Status = "observed"
		side.ObservedAt = time.Now().UTC()
		if err := store.FinishSideEffect(ctx, side); err != nil {
			return err
		}
	}
	return store.Transition(ctx, run.ID, domain.StatePushingBranch, domain.StateBranchPushed, "remote exact SHA observed", side.ResultJSON, run.CandidateHead)
}

func previousObservedPush(records []application.SideEffectRecord, sha string) bool {
	for _, record := range records {
		if record.Kind != "push" || record.Status != "observed" {
			continue
		}
		var result struct {
			PushedSHA string `json:"pushed_sha"`
		}
		if json.Unmarshal([]byte(record.ResultJSON), &result) == nil && result.PushedSHA == sha {
			return true
		}
	}
	return false
}

func fixtureOpenPR(ctx context.Context, store *sqlitestore.Store, run application.Run) error {
	var task domain.CodingTask
	if err := json.Unmarshal([]byte(run.NormalizedTaskJSON), &task); err != nil {
		return err
	}
	body, err := domain.PRBody(task, "controller-owned verifier pass", "fresh Sol review pass", run.IdempotencyKey)
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(body))
	intent, _ := json.Marshal(map[string]string{"head_branch": run.WorkingBranch, "base_branch": run.BaseBranch, "head_sha": run.CandidateHead, "body_digest": hex.EncodeToString(digest[:])})
	side, _, err := store.BeginSideEffect(ctx, application.SideEffectRecord{RunID: run.ID, Kind: "open_pr", IdempotencyKey: run.IdempotencyKey + ":" + run.CandidateHead, IntentJSON: string(intent), Attempt: 1})
	if err != nil {
		return err
	}
	pr := domain.PullRequest{Number: 1, URL: "https://fixture.invalid/pr/1", NodeID: "fixture-pr-1", HeadBranch: run.WorkingBranch, BaseBranch: run.BaseBranch, HeadSHA: run.CandidateHead, BaseSHA: run.BaseSHA, BodyDigest: hex.EncodeToString(digest[:]), OwnershipKey: run.IdempotencyKey, State: "OPEN"}
	if err := pr.ValidateOwnership(run.WorkingBranch, run.BaseBranch, run.CandidateHead, run.IdempotencyKey); err != nil {
		return err
	}
	if err := store.SavePullRequest(ctx, run.ID, pr); err != nil {
		return err
	}
	if side.Status != "observed" {
		side.Status = "observed"
		side.ResultJSON = fmt.Sprintf(`{"number":1,"node_id":"fixture-pr-1","head_sha":"%s"}`, run.CandidateHead)
		side.ObservedAt = time.Now().UTC()
		if err := store.FinishSideEffect(ctx, side); err != nil {
			return err
		}
	}
	return store.Transition(ctx, run.ID, domain.StateOpeningPR, domain.StatePROpen, "fake GitHub PR observed", pr.URL, run.CandidateHead)
}

func fixturePassingSnapshot(head string) domain.ReviewSnapshot {
	return domain.ReviewSnapshot{HeadSHA: head, RequiredChecks: []string{"fixture-go-test"}, Checks: []domain.Check{{ID: "check-1", Name: "fixture-go-test", Required: true, Status: "completed", Conclusion: "success", ObservedSHA: head}}, CodeRabbitStatus: "pass", ObservedAt: time.Now().UTC()}
}

func latestPersistedPassingSnapshot(inspection application.RunInspection, head string) (domain.ReviewSnapshot, error) {
	for index := len(inspection.Polls) - 1; index >= 0; index-- {
		poll := inspection.Polls[index]
		if poll.HeadSHA != head || poll.Status != string(domain.ReconciliationPass) {
			continue
		}
		var snapshot domain.ReviewSnapshot
		if err := json.Unmarshal([]byte(poll.SnapshotJSON), &snapshot); err != nil {
			return snapshot, err
		}
		if snapshot.HeadSHA != head || snapshot.Classify() != domain.ReconciliationPass {
			return snapshot, errors.New("persisted passing observation is invalid")
		}
		return snapshot, nil
	}
	return domain.ReviewSnapshot{}, errors.New("missing persisted passing checks and CodeRabbit observation")
}

func fixtureReconcile(ctx context.Context, store *sqlitestore.Store, run application.Run) error {
	now := time.Now().UTC()
	pending := domain.ReviewSnapshot{HeadSHA: run.CandidateHead, RequiredChecks: []string{"fixture-go-test"}, Checks: []domain.Check{{ID: "check-1", Name: "fixture-go-test", Required: true, Status: "in_progress", ObservedSHA: run.CandidateHead}}, CodeRabbitStatus: "pending", ObservedAt: now}
	passing := fixturePassingSnapshot(run.CandidateHead)
	progress, err := store.PollProgress(ctx, run.ID, 1, run.CandidateHead)
	if err != nil {
		return err
	}
	snapshots := []domain.ReviewSnapshot{pending, passing}
	if len(progress) == 1 {
		snapshots = []domain.ReviewSnapshot{passing}
	}
	github := &fixtureGitHub{snapshots: snapshots}
	status, err := application.ReconcileReviews(ctx, github, store, run.ID, 1, run.CandidateHead, application.PollPolicy{MaxAttempts: 2, Interval: 0, Deadline: time.Second}, func(context.Context, time.Duration) error { return nil })
	if err != nil {
		return err
	}
	if status == domain.ReconciliationActionable {
		return store.Transition(ctx, run.ID, domain.StateReconcilingReviews, domain.StateRepairing, "actionable normalized finding", "finding digests", run.CandidateHead)
	}
	if status != domain.ReconciliationPass {
		return fmt.Errorf("fixture reconciliation ended as %s", status)
	}
	return store.Transition(ctx, run.ID, domain.StateReconcilingReviews, domain.StateAwaitingHumanApproval, "checks and CodeRabbit pass", "fixture observations", run.CandidateHead)
}

type fixtureGitHub struct {
	snapshots []domain.ReviewSnapshot
	index     int
}

func (*fixtureGitHub) FindPullRequest(context.Context, string, string) (*domain.PullRequest, error) {
	return nil, nil
}
func (*fixtureGitHub) CreatePullRequest(context.Context, string, string, string, string, string) (domain.PullRequest, error) {
	return domain.PullRequest{}, errors.New("fixture PR creation is persisted separately")
}
func (f *fixtureGitHub) Observe(context.Context, int64, string) (domain.ReviewSnapshot, error) {
	if f.index >= len(f.snapshots) {
		return domain.ReviewSnapshot{}, errors.New("fixture observations exhausted")
	}
	value := f.snapshots[f.index]
	f.index++
	return value, nil
}
func (*fixtureGitHub) GetPullRequest(context.Context, int64) (domain.PullRequest, error) {
	return domain.PullRequest{}, errors.New("fixture PR is read from SQLite")
}
func (*fixtureGitHub) SquashMerge(context.Context, int64, string) (domain.PullRequest, error) {
	return domain.PullRequest{}, errors.New("fixture merge uses local bare origin")
}

func readFixtureApproval(path string) (domain.HumanApproval, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return domain.HumanApproval{}, err
	}
	var approval domain.HumanApproval
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&approval); err != nil {
		return approval, err
	}
	return approval, nil
}

func fixtureReconcileOrMerge(repo fixtureRepository, run application.Run) (string, error) {
	remote, err := runCommand(repo.SourcePath, "git", "ls-remote", "origin", "refs/heads/"+run.BaseBranch)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(remote)
	if len(fields) == 2 {
		baseTree, treeErr := runCommand(repo.SourcePath, "git", "show", "-s", "--format=%T", fields[0])
		if treeErr == nil {
			candidateTree, candidateErr := runCommand(repo.SourcePath, "git", "show", "-s", "--format=%T", run.CandidateHead)
			if candidateErr == nil && strings.TrimSpace(baseTree) == strings.TrimSpace(candidateTree) {
				parent, parentErr := runCommand(repo.SourcePath, "git", "show", "-s", "--format=%P", fields[0])
				if parentErr == nil && strings.TrimSpace(parent) == run.BaseSHA {
					return fields[0], nil
				}
			}
		}
	}
	return fixtureSquashMerge(repo, run)
}
func fixtureSquashMerge(repo fixtureRepository, run application.Run) (string, error) {
	remote, err := runCommand(repo.SourcePath, "git", "ls-remote", "origin", "refs/heads/"+run.BaseBranch)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(remote)
	if len(fields) != 2 || fields[0] != run.BaseSHA {
		return "", errors.New("fixture base moved before squash merge")
	}
	workingRemote, err := runCommand(repo.SourcePath, "git", "ls-remote", "origin", "refs/heads/"+run.WorkingBranch)
	if err != nil {
		return "", err
	}
	workingFields := strings.Fields(workingRemote)
	if len(workingFields) != 2 || workingFields[0] != run.CandidateHead {
		return "", errors.New("remote working branch does not match exact approved candidate")
	}
	if _, err := runCommand(repo.SourcePath, "git", "checkout", run.BaseBranch); err != nil {
		return "", err
	}
	if _, err := runCommand(repo.SourcePath, "git", "merge", "--squash", run.CandidateHead); err != nil {
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
	inspection, err := store.Inspect(ctx, run.ID)
	if err != nil {
		return err
	}
	if err := validateFixtureCleanupOwnership(run, repo, inspection.Resources); err != nil {
		return err
	}
	steps := []application.CleanupRecord{{RunID: run.ID, Kind: "worktree", Name: run.WorktreePath, Status: "intent"}, {RunID: run.ID, Kind: "remote_branch", Name: run.WorkingBranch, Status: "intent"}, {RunID: run.ID, Kind: "local_branch", Name: run.WorkingBranch, Status: "intent"}}
	for _, step := range steps {
		inspection, err := store.Inspect(ctx, run.ID)
		if err != nil {
			return err
		}
		alreadyDeleted := false
		for _, existing := range inspection.Cleanup {
			if existing.Kind == step.Kind && existing.Name == step.Name && existing.Status == "deleted" {
				alreadyDeleted = true
			}
		}
		if alreadyDeleted {
			continue
		}
		if err := store.UpsertCleanup(ctx, step); err != nil {
			return err
		}
		err = nil
		switch step.Kind {
		case "worktree":
			if _, statErr := os.Stat(run.WorktreePath); errors.Is(statErr, os.ErrNotExist) {
				err = nil
			} else {
				_, err = runCommand(repo.SourcePath, "git", "worktree", "remove", run.WorktreePath)
			}
		case "remote_branch":
			remote, remoteErr := runCommand(repo.SourcePath, "git", "ls-remote", "origin", "refs/heads/"+run.WorkingBranch)
			if remoteErr != nil {
				err = remoteErr
			} else if strings.TrimSpace(remote) == "" {
				err = nil
			} else if !strings.HasPrefix(remote, run.CandidateHead+"\t") {
				err = errors.New("remote branch no longer matches persisted candidate")
			} else {
				_, err = runCommand(repo.SourcePath, "git", "push", "origin", "--delete", "refs/heads/"+run.WorkingBranch)
			}
		case "local_branch":
			ref := "refs/heads/" + run.WorkingBranch
			var actual string
			actual, err = runCommand(repo.SourcePath, "git", "rev-parse", "--verify", ref)
			if err != nil && strings.Contains(err.Error(), "Needed a single revision") {
				err = nil
				actual = ""
			}
			if err == nil && strings.TrimSpace(actual) != run.CandidateHead {
				if strings.TrimSpace(actual) != "" {
					err = errors.New("local branch no longer matches persisted candidate")
				}
			}
			if err == nil && strings.TrimSpace(actual) != "" {
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

func validateFixtureCleanupOwnership(run application.Run, repo fixtureRepository, resources []application.OwnedResource) error {
	ownedWorktree, ownedBranch := false, false
	for _, resource := range resources {
		if resource.RunID != run.ID || resource.Status != "owned" {
			continue
		}
		if resource.Kind != "worktree" && resource.Kind != "branch" {
			continue
		}
		var evidence struct {
			OriginPath string `json:"origin_path"`
			SourcePath string `json:"source_path"`
			Path       string `json:"path"`
			Branch     string `json:"branch"`
			BaseBranch string `json:"base_branch"`
			BaseSHA    string `json:"base_sha"`
		}
		if err := json.Unmarshal([]byte(resource.CreationEvidence), &evidence); err != nil {
			return err
		}
		if evidence.OriginPath != repo.OriginPath || evidence.SourcePath != repo.SourcePath || evidence.Path != run.WorktreePath || evidence.Branch != run.WorkingBranch || evidence.BaseBranch != run.BaseBranch || evidence.BaseSHA != run.BaseSHA {
			return errors.New("cleanup ownership evidence does not match run")
		}
		if resource.Kind == "worktree" && resource.Name == run.WorktreePath {
			ownedWorktree = true
		}
		if resource.Kind == "branch" && resource.Name == run.WorkingBranch {
			ownedBranch = true
		}
	}
	if !ownedWorktree || !ownedBranch {
		return errors.New("cleanup requires durable owned worktree and branch evidence")
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
