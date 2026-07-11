package application

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/codex"
	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/verifier"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

const candidateCommitSubject = "Controller-owned local candidate"
const localLeaseTTL = 45 * time.Second

type LocalRepository struct {
	Label       string   `json:"label"`
	OriginPath  string   `json:"origin_path"`
	SourcePath  string   `json:"source_path"`
	BaseBranch  string   `json:"base_branch"`
	VerifierIDs []string `json:"verifier_ids"`
}

type LocalStartInput struct {
	Task           domain.CodingTask
	RawIssueJSON   []byte
	RawIssueHash   string
	NormalizedJSON []byte
	TaskHash       string
	IdempotencyKey string
	Repository     LocalRepository
	RunRoot        string
	WorktreeRoot   string
}

type Decision struct {
	ChoiceID     string `json:"choice_id"`
	Instructions string `json:"instructions"`
}

type persistedDecisionEvidence struct {
	Path               string   `json:"path"`
	Hash               string   `json:"sha256"`
	Decision           Decision `json:"decision"`
	RequestOutcomePath string   `json:"request_outcome_path"`
	RequestOutcomeHash string   `json:"request_outcome_hash"`
}

type artifactOwnership struct {
	Path         string `json:"path"`
	AttemptsPath string `json:"attempts_path"`
	RunRoot      string `json:"run_root"`
	Nonce        string `json:"nonce"`
	TaskHash     string `json:"task_hash"`
}

type DurableCodex interface {
	Preflight(context.Context, string) (codex.PreflightEvidence, error)
	Implementation(context.Context, codex.CommandSpec, string) (codex.StructuredResult[domain.AgentOutcome], error)
	Resume(context.Context, codex.CommandSpec, string) (codex.StructuredResult[domain.AgentOutcome], error)
	Review(context.Context, codex.CommandSpec, string) (codex.StructuredResult[domain.ReviewOutcome], error)
}

type DurableGit interface {
	Head(context.Context, string) (string, error)
	Branch(context.Context, string) (string, error)
	Status(context.Context, string) (string, error)
	ValidateRemoteBase(context.Context, string, string, string) error
	CommitCandidate(context.Context, string, string) (string, error)
	CommitMetadata(context.Context, string, string) (string, string, error)
}

type LocalController struct {
	store        RunStore
	worktrees    WorktreeProvisioner
	codex        DurableCodex
	verify       VerificationRunner
	git          DurableGit
	commands     codex.CommandBuilder
	planner      Planner
	worktreeRoot string
}

func NewLocalController(store RunStore, worktrees WorktreeProvisioner, executor DurableCodex,
	verification VerificationRunner, git DurableGit, codexBinary, worktreeRoot string) *LocalController {
	return &LocalController{store: store, worktrees: worktrees, codex: executor, verify: verification, git: git,
		commands: codex.NewCommandBuilder(codexBinary), planner: NewPlanner(codexBinary), worktreeRoot: worktreeRoot}
}

func (c *LocalController) Start(ctx context.Context, input LocalStartInput) (Run, error) {
	if err := input.Task.Validate(); err != nil {
		return Run{}, err
	}
	if !filepath.IsAbs(input.RunRoot) || !filepath.IsAbs(input.WorktreeRoot) {
		return Run{}, errors.New("run and worktree roots must be absolute")
	}
	canonicalRuns, err := existingDirectory(input.RunRoot)
	if err != nil {
		return Run{}, fmt.Errorf("resolve run root: %w", err)
	}
	canonicalWorktrees, err := existingDirectory(input.WorktreeRoot)
	if err != nil {
		return Run{}, fmt.Errorf("resolve worktree root: %w", err)
	}
	overlapped, err := directoriesOverlap(canonicalRuns, canonicalWorktrees)
	if err != nil {
		return Run{}, err
	}
	if overlapped {
		return Run{}, errors.New("run and worktree roots must not overlap")
	}
	input.RunRoot, input.WorktreeRoot = canonicalRuns, canonicalWorktrees
	if input.Task.Repository != input.Repository.Label || input.Task.BaseBranch != input.Repository.BaseBranch {
		return Run{}, errors.New("task repository/base does not match the registry snapshot")
	}
	repositoryJSON, err := json.Marshal(input.Repository)
	if err != nil {
		return Run{}, err
	}
	artifactRoot := filepath.Join(input.RunRoot, input.Task.RunID)
	run, _, err := c.store.CreateRun(ctx, CreateRunInput{Run: Run{ID: input.Task.RunID, IssueID: input.Task.IssueID,
		IdempotencyKey: input.IdempotencyKey, SourceRevision: input.Task.SourceRevision, RawIssueJSON: string(input.RawIssueJSON),
		RawIssueHash: input.RawIssueHash, NormalizedTaskJSON: string(input.NormalizedJSON), TaskHash: input.TaskHash,
		Repository: input.Task.Repository, RepositoryConfigJSON: string(repositoryJSON), BaseBranch: input.Task.BaseBranch,
		WorkingBranch: input.Task.WorkingBranch, WorktreePath: filepath.Join(input.WorktreeRoot, input.Task.RunID), ArtifactRoot: artifactRoot,
		ImplementationModel: codex.ImplementationModel, ReviewModel: codex.ReviewModel}})
	if err != nil {
		return Run{}, err
	}
	c.worktreeRoot = input.WorktreeRoot
	if err := c.ensureArtifactRoot(ctx, run); err != nil {
		_ = c.store.SetLastError(ctx, run.ID, err.Error())
		return Run{}, err
	}
	if err := c.materializeSnapshots(run); err != nil {
		_ = c.store.SetLastError(ctx, run.ID, err.Error())
		return Run{}, err
	}
	return c.Continue(ctx, run.ID, nil)
}

func (c *LocalController) Continue(ctx context.Context, runID string, decision *Decision) (Run, error) {
	owner, err := randomIdentifier("controller-")
	if err != nil {
		return Run{}, err
	}
	acquired, err := c.store.AcquireLease(ctx, runID, owner, time.Now().UTC().Add(localLeaseTTL))
	if err != nil {
		return Run{}, fmt.Errorf("acquire run lease: %w", err)
	}
	if !acquired {
		return Run{}, errors.New("run is actively leased by another controller process")
	}
	leaseCtx, cancelLease := context.WithCancelCause(ctx)
	stopLease := make(chan struct{})
	leaseDone := make(chan struct{})
	go func() {
		defer close(leaseDone)
		ticker := time.NewTicker(localLeaseTTL / 3)
		defer ticker.Stop()
		for {
			select {
			case <-stopLease:
				return
			case <-leaseCtx.Done():
				return
			case <-ticker.C:
				ok, renewErr := c.store.RenewLease(context.Background(), runID, owner, time.Now().UTC().Add(localLeaseTTL))
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
		_ = c.store.ReleaseLease(releaseCtx, runID, owner)
	}()
	ctx = leaseCtx
	for steps := 0; steps < 20; steps++ {
		run, err := c.store.GetRun(ctx, runID)
		if err != nil {
			return Run{}, err
		}
		if err := c.ensureArtifactRoot(ctx, run); err != nil {
			_ = c.store.SetLastError(ctx, run.ID, err.Error())
			return run, err
		}
		if err := c.materializeSnapshots(run); err != nil {
			_ = c.store.SetLastError(ctx, run.ID, err.Error())
			return run, err
		}
		switch run.State {
		case domain.StateReceived:
			err = c.store.Transition(ctx, run.ID, domain.StateReceived, domain.StateAdmitting, "validated simulated issue snapshot", "coding-task.json", "")
		case domain.StateAdmitting:
			err = c.store.Transition(ctx, run.ID, domain.StateAdmitting, domain.StateProvisioning, "begin dedicated worktree provisioning", "repository registry snapshot", "")
		case domain.StateProvisioning:
			err = c.provision(ctx, run)
		case domain.StateExecuting:
			err = c.execute(ctx, run, decision)
			decision = nil
		case domain.StateAwaitingHumanDecision:
			if decision == nil {
				return run, nil
			}
			err = c.acceptDecision(ctx, run, *decision)
		case domain.StateVerifying:
			err = c.verifyCandidate(ctx, run)
		case domain.StateFreshReview:
			err = c.freshReview(ctx, run)
		case domain.StateApprovalReady:
			err = c.validateApproval(ctx, run)
			if err == nil {
				return run, nil
			}
			_ = c.store.Transition(ctx, run.ID, domain.StateApprovalReady, domain.StateFailed, "approval evidence invalidated", err.Error(), run.CandidateHead)
			_ = c.store.SetLastError(ctx, run.ID, err.Error())
			return c.store.GetRun(ctx, run.ID)
		case domain.StateFailed, domain.StateCompleted, domain.StateRejected:
			return run, nil
		default:
			return run, fmt.Errorf("local controller cannot continue state %s", run.State)
		}
		if err != nil {
			_ = c.store.SetLastError(ctx, run.ID, err.Error())
			persisted, getErr := c.store.GetRun(ctx, run.ID)
			if getErr != nil {
				return Run{}, errors.Join(err, getErr)
			}
			return persisted, err
		}
	}
	return Run{}, errors.New("local controller exceeded transition safety limit")
}

func (c *LocalController) ensureArtifactRoot(ctx context.Context, run Run) error {
	inspection, err := c.store.Inspect(ctx, run.ID)
	if err != nil {
		return err
	}
	var ownership artifactOwnership
	resourceFound := false
	resourceStatus := ""
	for _, resource := range inspection.Resources {
		if resource.Kind == "artifact_root" && resource.Name == run.ArtifactRoot {
			if err := json.Unmarshal([]byte(resource.CreationEvidence), &ownership); err != nil {
				return fmt.Errorf("decode artifact ownership: %w", err)
			}
			resourceFound = true
			resourceStatus = resource.Status
		}
	}
	_, pathErr := os.Lstat(run.ArtifactRoot)
	pathExists := pathErr == nil
	if pathErr != nil && !errors.Is(pathErr, os.ErrNotExist) {
		return pathErr
	}
	if !resourceFound {
		if pathExists {
			return errors.New("artifact root existed before controller ownership reservation")
		}
		nonce, err := randomIdentifier("artifact-")
		if err != nil {
			return err
		}
		ownership = artifactOwnership{Path: run.ArtifactRoot, AttemptsPath: filepath.Join(run.ArtifactRoot, "attempts"), RunRoot: filepath.Dir(run.ArtifactRoot), Nonce: nonce, TaskHash: run.TaskHash}
		data, _ := json.Marshal(ownership)
		if err := c.store.AddOwnedResource(ctx, OwnedResource{RunID: run.ID, Kind: "artifact_root", Name: run.ArtifactRoot, CreationEvidence: string(data), Status: "reserved"}); err != nil {
			return err
		}
		resourceFound = true
		resourceStatus = "reserved"
	}
	if ownership.Path != run.ArtifactRoot || ownership.AttemptsPath != filepath.Join(run.ArtifactRoot, "attempts") || ownership.RunRoot != filepath.Dir(run.ArtifactRoot) || ownership.TaskHash != run.TaskHash || strings.TrimSpace(ownership.Nonce) == "" {
		return errors.New("artifact ownership evidence does not match run")
	}
	if !pathExists {
		if resourceStatus == "owned" {
			return errors.New("owned artifact root is missing")
		}
		if err := os.Mkdir(run.ArtifactRoot, 0o700); err != nil {
			return fmt.Errorf("create owned artifact root: %w", err)
		}
	}
	rootInfo, err := os.Lstat(run.ArtifactRoot)
	if err != nil {
		return err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("artifact root must be a real directory")
	}
	marker := filepath.Join(run.ArtifactRoot, ".controller-owned.json")
	if _, err := os.Lstat(marker); errors.Is(err, os.ErrNotExist) {
		entries, readErr := os.ReadDir(run.ArtifactRoot)
		if readErr != nil {
			return readErr
		}
		if len(entries) != 0 {
			return errors.New("reserved artifact root contains unexpected content before ownership marker")
		}
		data, _ := json.Marshal(ownership)
		if err := writeExclusive(marker, data); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if _, err := os.Lstat(ownership.AttemptsPath); errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(ownership.AttemptsPath, 0o700); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err := validateArtifactOwnership(ownership, run); err != nil {
		return err
	}
	data, _ := json.Marshal(ownership)
	return c.store.AddOwnedResource(ctx, OwnedResource{RunID: run.ID, Kind: "artifact_root", Name: run.ArtifactRoot, CreationEvidence: string(data), Status: "owned"})
}

func validateArtifactOwnership(ownership artifactOwnership, run Run) error {
	runRootInfo, err := os.Lstat(ownership.RunRoot)
	if err != nil {
		return err
	}
	if !runRootInfo.IsDir() || runRootInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("run root must remain a real directory")
	}
	canonicalRunRoot, err := filepath.EvalSymlinks(ownership.RunRoot)
	if err != nil {
		return err
	}
	if canonicalRunRoot != ownership.RunRoot {
		return errors.New("run root no longer matches its canonical ownership path")
	}
	rootInfo, err := os.Lstat(ownership.Path)
	if err != nil {
		return err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("artifact root must be a real directory")
	}
	root, err := filepath.EvalSymlinks(ownership.Path)
	if err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(ownership.Path))
	if err != nil {
		return err
	}
	if filepath.Dir(root) != parent || filepath.Base(root) != run.ID {
		return errors.New("artifact root escapes its canonical run root")
	}
	attemptsInfo, err := os.Lstat(ownership.AttemptsPath)
	if err != nil {
		return err
	}
	if !attemptsInfo.IsDir() || attemptsInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("attempts path must be a real directory")
	}
	attempts, err := filepath.EvalSymlinks(ownership.AttemptsPath)
	if err != nil {
		return err
	}
	if filepath.Dir(attempts) != root {
		return errors.New("attempts path escapes owned artifact root")
	}
	marker := filepath.Join(root, ".controller-owned.json")
	info, err := os.Lstat(marker)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("artifact ownership marker must be a regular file")
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		return err
	}
	var actual artifactOwnership
	if err := json.Unmarshal(data, &actual); err != nil {
		return err
	}
	if actual != ownership {
		return errors.New("artifact ownership marker mismatch")
	}
	return nil
}

func (c *LocalController) materializeSnapshots(run Run) error {
	if bytesHash([]byte(run.RawIssueJSON)) != run.RawIssueHash {
		return errors.New("raw simulated issue hash mismatch")
	}
	if bytesHash([]byte(run.NormalizedTaskJSON)) != run.TaskHash {
		return errors.New("normalized task snapshot hash mismatch")
	}
	if err := validateArtifactOwnershipFromStoreless(run); err != nil {
		return err
	}
	for name, data := range map[string][]byte{"simulated-issue.json": []byte(run.RawIssueJSON), "coding-task.json": []byte(run.NormalizedTaskJSON)} {
		path := filepath.Join(run.ArtifactRoot, name)
		info, lstatErr := os.Lstat(path)
		if lstatErr == nil {
			if !info.Mode().IsRegular() {
				return errors.New("snapshot artifact must be a regular file")
			}
			existing, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !bytes.Equal(existing, data) {
				return fmt.Errorf("snapshot artifact conflict: %s", path)
			}
			continue
		} else if !errors.Is(lstatErr, os.ErrNotExist) {
			return lstatErr
		}
		if err := writeExclusive(path, data); err != nil {
			return err
		}
	}
	return nil
}

func validateArtifactOwnershipFromStoreless(run Run) error {
	rootInfo, err := os.Lstat(run.ArtifactRoot)
	if err != nil {
		return err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("artifact root must be a real directory")
	}
	marker := filepath.Join(run.ArtifactRoot, ".controller-owned.json")
	markerInfo, err := os.Lstat(marker)
	if err != nil {
		return err
	}
	if !markerInfo.Mode().IsRegular() {
		return errors.New("artifact ownership marker must be a regular file")
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		return err
	}
	var ownership artifactOwnership
	if err := json.Unmarshal(data, &ownership); err != nil {
		return err
	}
	return validateArtifactOwnership(ownership, run)
}

func (c *LocalController) provision(ctx context.Context, run Run) error {
	var repository LocalRepository
	if err := json.Unmarshal([]byte(run.RepositoryConfigJSON), &repository); err != nil {
		return fmt.Errorf("decode repository snapshot: %w", err)
	}
	path := run.WorktreePath
	if path == "" {
		path = filepath.Join(c.worktreeRoot, run.ID)
	}
	spec := WorktreeSpec{SourcePath: repository.SourcePath, OriginPath: repository.OriginPath, BaseBranch: run.BaseBranch, Branch: run.WorkingBranch, Path: path}
	inspection, err := c.store.Inspect(ctx, run.ID)
	if err != nil {
		return err
	}
	reservedByRun := false
	for _, resource := range inspection.Resources {
		if resource.Kind == "worktree" && resource.Name == path {
			reservedByRun = true
		}
	}
	_, pathErr := os.Lstat(path)
	pathExists := pathErr == nil
	if pathErr != nil && !errors.Is(pathErr, os.ErrNotExist) {
		return pathErr
	}
	if pathExists && run.BaseSHA == "" && !reservedByRun {
		return errors.New("worktree path existed before controller ownership reservation")
	}
	requestEvidence, _ := json.Marshal(spec)
	for _, resource := range []OwnedResource{{RunID: run.ID, Kind: "branch", Name: run.WorkingBranch, CreationEvidence: string(requestEvidence), Status: "reserved"}, {RunID: run.ID, Kind: "worktree", Name: path, CreationEvidence: string(requestEvidence), Status: "reserved"}} {
		if err := c.store.AddOwnedResource(ctx, resource); err != nil {
			return fmt.Errorf("reserve %s ownership: %w", resource.Kind, err)
		}
	}
	if run.WorktreePath != "" && run.BaseSHA != "" {
		record := WorktreeRecord{SourcePath: repository.SourcePath, OriginPath: repository.OriginPath, Path: run.WorktreePath, Branch: run.WorkingBranch, BaseBranch: run.BaseBranch, BaseSHA: run.BaseSHA}
		if err := c.worktrees.ValidateOwned(ctx, record); err != nil {
			return err
		}
		return c.store.Transition(ctx, run.ID, domain.StateProvisioning, domain.StateExecuting, "reused owned dedicated worktree", run.WorktreePath, run.BaseSHA)
	}
	if pathExists {
		baseSHA, err := c.git.Head(ctx, path)
		if err != nil {
			return err
		}
		record := WorktreeRecord{SourcePath: repository.SourcePath, OriginPath: repository.OriginPath, Path: path, Branch: run.WorkingBranch, BaseBranch: run.BaseBranch, BaseSHA: baseSHA}
		if err := c.worktrees.ValidateOwned(ctx, record); err != nil {
			return fmt.Errorf("recover provisioned worktree: %w", err)
		}
		if err := c.store.SetWorkspace(ctx, run.ID, record.BaseSHA, record.Path); err != nil {
			return err
		}
		evidence, _ := json.Marshal(record)
		for _, resource := range []OwnedResource{{RunID: run.ID, Kind: "branch", Name: run.WorkingBranch, CreationEvidence: string(evidence), Status: "owned"}, {RunID: run.ID, Kind: "worktree", Name: path, CreationEvidence: string(evidence), Status: "owned"}} {
			if err := c.store.AddOwnedResource(ctx, resource); err != nil {
				return err
			}
		}
		return c.store.Transition(ctx, run.ID, domain.StateProvisioning, domain.StateExecuting, "recovered provisioned owned worktree", path, record.BaseSHA)
	}
	record, err := c.worktrees.Provision(ctx, spec)
	if err != nil {
		return err
	}
	if err := c.store.SetWorkspace(ctx, run.ID, record.BaseSHA, record.Path); err != nil {
		return err
	}
	evidence, _ := json.Marshal(record)
	for _, resource := range []OwnedResource{{RunID: run.ID, Kind: "branch", Name: run.WorkingBranch, CreationEvidence: string(evidence), Status: "owned"}, {RunID: run.ID, Kind: "worktree", Name: record.Path, CreationEvidence: string(evidence), Status: "owned"}} {
		if err := c.store.AddOwnedResource(ctx, resource); err != nil {
			return err
		}
	}
	return c.store.Transition(ctx, run.ID, domain.StateProvisioning, domain.StateExecuting, "provisioned owned dedicated worktree", record.Path, record.BaseSHA)
}

func (c *LocalController) execute(ctx context.Context, run Run, decision *Decision) error {
	if run.ImplementationModel != codex.ImplementationModel {
		return fmt.Errorf("run has missing or unsupported implementation model evidence: %q", run.ImplementationModel)
	}
	inspection, err := c.store.Inspect(ctx, run.ID)
	if err != nil {
		return err
	}
	if run.ImplementationSession != "" && decision == nil {
		persisted, found, loadErr := findPersistedDecision(inspection)
		if loadErr != nil {
			return loadErr
		}
		if found {
			decision = &persisted
		}
	}
	recoveryResume := false
	for _, attempt := range inspection.Attempts {
		if attempt.ErrorCategory == "controller_restart_session_recovered" {
			recoveryResume = true
		}
	}
	for i := len(inspection.Attempts) - 1; i >= 0; i-- {
		attempt := inspection.Attempts[i]
		if attempt.Kind != "implementation" && attempt.Kind != "resume" {
			continue
		}
		if attempt.RequestedModel != run.ImplementationModel {
			return errors.New("implementation session/model attempt evidence conflict")
		}
		if attempt.Status == "started" {
			stdoutPath := filepath.Join(attempt.ArtifactDir, "implementation.stdout.jsonl")
			stderrPath := filepath.Join(attempt.ArtifactDir, "implementation.stderr.txt")
			sessionID, recoverErr := codex.ExtractSessionIDFile(stdoutPath)
			if recoverErr != nil {
				attempt.Status = "failed"
				attempt.FinishedAt = time.Now().UTC()
				attempt.ExitCode = -1
				attempt.ErrorCategory = "controller_restart_missing_session"
				attempt.StdoutPath = stdoutPath
				attempt.StderrPath = stderrPath
				_ = c.populateAttemptCaptureDigests(&attempt)
				if finishErr := c.store.FinishAttempt(ctx, attempt); finishErr != nil {
					return errors.Join(recoverErr, finishErr)
				}
				return fmt.Errorf("interrupted attempt has no recoverable explicit session ID: %w", recoverErr)
			}
			if err := c.store.SetImplementationSession(ctx, run.ID, sessionID); err != nil {
				return err
			}
			attempt.Status = "failed"
			attempt.FinishedAt = time.Now().UTC()
			attempt.ExitCode = -1
			attempt.SessionID = sessionID
			attempt.StdoutPath = stdoutPath
			attempt.StderrPath = stderrPath
			attempt.ErrorCategory = "controller_restart_session_recovered"
			if err := c.populateAttemptCaptureDigests(&attempt); err != nil {
				return err
			}
			if err := c.store.FinishAttempt(ctx, attempt); err != nil {
				return err
			}
			run.ImplementationSession = sessionID
			recoveryResume = true
			break
		}
		if attempt.Status == "succeeded" && decision == nil {
			outcome, err := readOutcome[domain.AgentOutcome](attempt.OutcomePath, attempt.OutcomeHash)
			if err != nil {
				return err
			}
			return c.applyImplementationOutcome(ctx, run, outcome, attempt.OutcomePath)
		}
		break
	}
	task, err := decodeTaskSnapshot(run.NormalizedTaskJSON)
	if err != nil {
		return err
	}
	kind := "implementation"
	if run.ImplementationSession != "" {
		kind = "resume"
		if decision == nil && !recoveryResume {
			return errors.New("explicit resume requires persisted decision evidence")
		}
	}
	directory, err := newArtifactDirectoryPath(run.ArtifactRoot, kind)
	if err != nil {
		return err
	}
	attempt, err := c.store.BeginAttempt(ctx, run.ID, kind, run.ImplementationModel, directory)
	if err != nil {
		return err
	}
	attempt.StdoutPath = filepath.Join(directory, "implementation.stdout.jsonl")
	attempt.StderrPath = filepath.Join(directory, "implementation.stderr.txt")
	if err := os.Mkdir(directory, 0o700); err != nil {
		return c.failAttempt(ctx, attempt, "artifact_creation", err)
	}
	plan, err := c.planner.Build(task, run.WorktreePath, directory)
	if err != nil {
		return c.failAttempt(ctx, attempt, "plan", err)
	}
	if err := MaterializeArtifacts(plan.Artifacts); err != nil {
		return c.failAttempt(ctx, attempt, "artifact_materialization", err)
	}
	if _, err := c.codex.Preflight(ctx, directory); err != nil {
		return c.failAttempt(ctx, attempt, "codex_preflight", err)
	}
	var result codex.StructuredResult[domain.AgentOutcome]
	if kind == "implementation" {
		result, err = c.codex.Implementation(ctx, c.commands.Implementation(task, run.WorktreePath, directory), directory)
	} else {
		instructions := "Controller restarted after an interrupted attempt. Inspect the current owned worktree, continue the same task safely, and return a new structured outcome."
		if decision != nil {
			instructions = fmt.Sprintf("Human decision: %s\n\n%s", decision.ChoiceID, decision.Instructions)
		}
		spec, specErr := c.commands.Resume(run.ImplementationSession, run.ImplementationModel, run.WorktreePath, directory, instructions)
		if specErr != nil {
			return c.failAttempt(ctx, attempt, "resume_command", specErr)
		}
		result, err = c.codex.Resume(ctx, spec, directory)
	}
	if err != nil {
		return c.failAttempt(ctx, attempt, "codex_execution", err)
	}
	if run.ImplementationSession != "" && result.SessionID != run.ImplementationSession {
		return c.failAttempt(ctx, attempt, "session_mismatch", errors.New("resume returned a different session ID"))
	}
	if err := c.store.SetImplementationSession(ctx, run.ID, result.SessionID); err != nil {
		return err
	}
	attempt.Status = "succeeded"
	attempt.SessionID = result.SessionID
	attempt.FinishedAt = time.Now().UTC()
	attempt.ExitCode = result.Process.ExitCode
	attempt.StdoutPath = result.Process.StdoutPath
	attempt.StderrPath = result.Process.StderrPath
	if err := c.populateAttemptCaptureDigests(&attempt); err != nil {
		return c.failAttempt(ctx, attempt, "capture_digest", err)
	}
	attempt.OutcomePath = filepath.Join(directory, "implementation-outcome.json")
	attempt.OutcomeHash, err = fileHash(attempt.OutcomePath)
	if err != nil {
		return c.failAttempt(ctx, attempt, "outcome_hash", err)
	}
	if err := c.store.FinishAttempt(ctx, attempt); err != nil {
		return err
	}
	run.ImplementationSession = result.SessionID
	return c.applyImplementationOutcome(ctx, run, result.Outcome, attempt.OutcomePath)
}

func (c *LocalController) applyImplementationOutcome(ctx context.Context, run Run, outcome domain.AgentOutcome, evidence string) error {
	switch outcome.Status {
	case domain.AgentCompleted:
		return c.store.Transition(ctx, run.ID, domain.StateExecuting, domain.StateVerifying, "implementation outcome completed", evidence, "")
	case domain.AgentNeedsHumanDecision:
		return c.store.Transition(ctx, run.ID, domain.StateExecuting, domain.StateAwaitingHumanDecision, "implementation requires a human decision", evidence, "")
	case domain.AgentBlocked, domain.AgentFailed:
		return c.store.Transition(ctx, run.ID, domain.StateExecuting, domain.StateFailed, "implementation stopped", evidence, "")
	default:
		return fmt.Errorf("unsupported implementation outcome: %s", outcome.Status)
	}
}

func (c *LocalController) acceptDecision(ctx context.Context, run Run, decision Decision) error {
	if strings.TrimSpace(decision.ChoiceID) == "" || strings.TrimSpace(decision.Instructions) == "" {
		return errors.New("decision choice_id and instructions are required")
	}
	inspection, err := c.store.Inspect(ctx, run.ID)
	if err != nil {
		return err
	}
	var outcome domain.AgentOutcome
	var sourceAttempt Attempt
	found := false
	for i := len(inspection.Attempts) - 1; i >= 0; i-- {
		attempt := inspection.Attempts[i]
		if (attempt.Kind == "implementation" || attempt.Kind == "resume") && attempt.Status == "succeeded" {
			outcome, err = readOutcome[domain.AgentOutcome](attempt.OutcomePath, attempt.OutcomeHash)
			sourceAttempt = attempt
			found = true
			break
		}
	}
	if err != nil {
		return err
	}
	if !found || outcome.DecisionRequest == nil {
		return errors.New("missing persisted decision request evidence")
	}
	valid := false
	for _, option := range outcome.DecisionRequest.Options {
		if option.ID == decision.ChoiceID {
			valid = true
		}
	}
	if !valid {
		return errors.New("decision choice_id is not an offered option")
	}
	data, _ := json.MarshalIndent(decision, "", "  ")
	path := filepath.Join(run.ArtifactRoot, fmt.Sprintf("decision-%d.json", len(inspection.Attempts)))
	if err := writeExclusive(path, data); err != nil {
		return err
	}
	evidenceData, _ := json.Marshal(persistedDecisionEvidence{Path: path, Hash: bytesHash(data), Decision: decision, RequestOutcomePath: sourceAttempt.OutcomePath, RequestOutcomeHash: sourceAttempt.OutcomeHash})
	return c.store.Transition(ctx, run.ID, domain.StateAwaitingHumanDecision, domain.StateExecuting, "accepted simulated human decision", string(evidenceData), "")
}

func (c *LocalController) verifyCandidate(ctx context.Context, run Run) error {
	if err := c.validateWorkspace(ctx, run, false); err != nil {
		return err
	}
	inspection, err := c.store.Inspect(ctx, run.ID)
	if err != nil {
		return err
	}
	status, err := c.git.Status(ctx, run.WorktreePath)
	if err != nil {
		return err
	}
	head, err := c.git.Head(ctx, run.WorktreePath)
	if err != nil {
		return err
	}
	if run.CandidateHead == "" {
		if strings.TrimSpace(status) == "" {
			if head == run.BaseSHA {
				return errors.New("completed implementation produced no candidate changes")
			}
			parent, subject, metaErr := c.git.CommitMetadata(ctx, run.WorktreePath, head)
			if metaErr != nil {
				return metaErr
			}
			if parent != run.BaseSHA || subject != candidateCommitSubject {
				return errors.New("unpersisted HEAD is not a recoverable controller candidate")
			}
			run.CandidateHead = head
		} else {
			if head != run.BaseSHA {
				return errors.New("Codex changed HEAD; candidate commits are controller-owned")
			}
			before := status
			if _, err := c.runVerification(ctx, run, "precommit"); err != nil {
				return err
			}
			if err := c.validateWorkspace(ctx, run, false); err != nil {
				return err
			}
			after, err := c.git.Status(ctx, run.WorktreePath)
			if err != nil {
				return err
			}
			if after != before {
				return errors.New("pre-commit verifier mutated the worktree")
			}
			candidate, err := c.git.CommitCandidate(ctx, run.WorktreePath, candidateCommitSubject)
			if err != nil {
				return err
			}
			run.CandidateHead = candidate
		}
		if err := c.store.SetCandidateHead(ctx, run.ID, run.CandidateHead); err != nil {
			return err
		}
	}
	if head, err = c.git.Head(ctx, run.WorktreePath); err != nil || head != run.CandidateHead {
		if err != nil {
			return err
		}
		return errors.New("persisted candidate HEAD does not match current HEAD")
	}
	if status, err = c.git.Status(ctx, run.WorktreePath); err != nil || strings.TrimSpace(status) != "" {
		if err != nil {
			return err
		}
		return errors.New("candidate worktree is not clean")
	}
	if batch, ok := successfulVerificationBatch(inspection.Verifications, run.CandidateHead, taskVerifierIDs(run.NormalizedTaskJSON)); ok {
		if err := validateVerificationBatch(batch, run.CandidateHead); err != nil {
			return err
		}
	} else if _, err := c.runVerification(ctx, run, "candidate"); err != nil {
		return err
	}
	if err := c.validateWorkspace(ctx, run, true); err != nil {
		return err
	}
	return c.store.Transition(ctx, run.ID, domain.StateVerifying, domain.StateFreshReview, "candidate verified at exact HEAD", "candidate verification", run.CandidateHead)
}

func (c *LocalController) runVerification(ctx context.Context, run Run, phase string) (verifier.Evidence, error) {
	task, err := decodeTaskSnapshot(run.NormalizedTaskJSON)
	if err != nil {
		return verifier.Evidence{}, err
	}
	directory, err := newArtifactDirectory(run.ArtifactRoot, "verification-"+phase)
	if err != nil {
		return verifier.Evidence{}, err
	}
	evidence, runErr := c.verify.Run(ctx, task.VerifierIDs, run.WorktreePath, directory, phase)
	path := filepath.Join(directory, phase+"-verification.json")
	hash, hashErr := fileHash(path)
	if hashErr != nil {
		return evidence, errors.Join(runErr, hashErr)
	}
	for _, check := range evidence.Checks {
		stdoutHash, stdoutSize, stdoutErr := fileDigest(check.StdoutPath)
		stderrHash, stderrSize, stderrErr := fileDigest(check.StderrPath)
		if stdoutErr != nil || stderrErr != nil {
			return evidence, errors.Join(runErr, stdoutErr, stderrErr)
		}
		if err := c.store.SaveVerification(ctx, VerificationRecord{RunID: run.ID, VerifierID: check.VerifierID, Phase: phase, VerifiedHead: evidence.VerifiedHeadSHA, ExitCode: check.ExitCode, StdoutPath: check.StdoutPath, StderrPath: check.StderrPath, StdoutHash: stdoutHash, StderrHash: stderrHash, StdoutSize: stdoutSize, StderrSize: stderrSize, EvidencePath: path, EvidenceHash: hash}); err != nil {
			return evidence, errors.Join(runErr, err)
		}
	}
	return evidence, runErr
}

func (c *LocalController) freshReview(ctx context.Context, run Run) error {
	if run.ReviewModel != codex.ReviewModel {
		return fmt.Errorf("run has missing or unsupported review model evidence: %q", run.ReviewModel)
	}
	if err := c.validateWorkspace(ctx, run, true); err != nil {
		return err
	}
	inspection, err := c.store.Inspect(ctx, run.ID)
	if err != nil {
		return err
	}
	if record, ok := latestReviewForHead(inspection.Reviews, run.CandidateHead); ok {
		outcome, err := readOutcome[domain.ReviewOutcome](record.OutcomePath, record.OutcomeHash)
		if err != nil {
			return err
		}
		if err := validateReviewAttempt(inspection.Attempts, record, run.ReviewModel); err != nil {
			return err
		}
		if outcome.Verdict != domain.ReviewFailed {
			return c.authorizeReview(ctx, run, outcome, record.OutcomePath, inspection)
		}
	}
	task, err := decodeTaskSnapshot(run.NormalizedTaskJSON)
	if err != nil {
		return err
	}
	directory, err := newArtifactDirectoryPath(run.ArtifactRoot, "review")
	if err != nil {
		return err
	}
	attempt, err := c.store.BeginAttempt(ctx, run.ID, "review", run.ReviewModel, directory)
	if err != nil {
		return err
	}
	attempt.StdoutPath = filepath.Join(directory, "review.stdout.jsonl")
	attempt.StderrPath = filepath.Join(directory, "review.stderr.txt")
	if err := os.Mkdir(directory, 0o700); err != nil {
		return c.failAttempt(ctx, attempt, "artifact_creation", err)
	}
	plan, err := c.planner.Build(task, run.WorktreePath, directory)
	if err != nil {
		return c.failAttempt(ctx, attempt, "plan", err)
	}
	if err := MaterializeArtifacts(plan.Artifacts); err != nil {
		return c.failAttempt(ctx, attempt, "artifact_materialization", err)
	}
	if _, err := c.codex.Preflight(ctx, directory); err != nil {
		return c.failAttempt(ctx, attempt, "codex_preflight", err)
	}
	spec := c.commands.FreshReview(task, run.WorktreePath, directory)
	spec.Stdin += fmt.Sprintf("\nController candidate HEAD: %s\nController verification is authoritative for this exact HEAD.\n", run.CandidateHead)
	result, err := c.codex.Review(ctx, spec, directory)
	if err != nil {
		return c.failAttempt(ctx, attempt, "codex_review", err)
	}
	attempt.Status = "succeeded"
	attempt.SessionID = result.SessionID
	attempt.FinishedAt = time.Now().UTC()
	attempt.ExitCode = result.Process.ExitCode
	attempt.StdoutPath = result.Process.StdoutPath
	attempt.StderrPath = result.Process.StderrPath
	if err := c.populateAttemptCaptureDigests(&attempt); err != nil {
		return c.failAttempt(ctx, attempt, "capture_digest", err)
	}
	attempt.OutcomePath = filepath.Join(directory, "review-outcome.json")
	attempt.OutcomeHash, err = fileHash(attempt.OutcomePath)
	if err != nil {
		return c.failAttempt(ctx, attempt, "outcome_hash", err)
	}
	if err := c.store.FinishAttempt(ctx, attempt); err != nil {
		return err
	}
	record := ReviewRecord{RunID: run.ID, AttemptID: attempt.ID, SessionID: result.SessionID, ReviewedHead: result.Outcome.ReviewedHeadSHA, Verdict: string(result.Outcome.Verdict), OutcomePath: attempt.OutcomePath, OutcomeHash: attempt.OutcomeHash}
	if err := c.store.SaveReview(ctx, record); err != nil {
		return err
	}
	inspection, err = c.store.Inspect(ctx, run.ID)
	if err != nil {
		return err
	}
	return c.authorizeReview(ctx, run, result.Outcome, attempt.OutcomePath, inspection)
}

func (c *LocalController) authorizeReview(ctx context.Context, run Run, outcome domain.ReviewOutcome, evidence string, inspection RunInspection) error {
	if outcome.Verdict != domain.ReviewPass {
		return fmt.Errorf("fresh review stopped with verdict %s", outcome.Verdict)
	}
	batch, ok := successfulVerificationBatch(inspection.Verifications, run.CandidateHead, taskVerifierIDs(run.NormalizedTaskJSON))
	if !ok {
		return errors.New("candidate verification evidence is incomplete")
	}
	if err := validateVerificationBatch(batch, run.CandidateHead); err != nil {
		return err
	}
	reviewValidated := false
	for _, record := range inspection.Reviews {
		if record.ReviewedHead == run.CandidateHead && record.OutcomePath == evidence {
			if err := validateReviewAttempt(inspection.Attempts, record, run.ReviewModel); err != nil {
				return err
			}
			reviewValidated = true
		}
	}
	if !reviewValidated {
		return errors.New("review attempt evidence is missing")
	}
	head, err := c.git.Head(ctx, run.WorktreePath)
	if err != nil {
		return err
	}
	if err := AuthorizePROpen(domain.StateFreshReview, PROpenEvidence{Review: outcome, CurrentHeadSHA: head, VerificationHeadSHA: run.CandidateHead}); err != nil {
		return err
	}
	if err := c.validateWorkspace(ctx, run, true); err != nil {
		return err
	}
	return c.store.Transition(ctx, run.ID, domain.StateFreshReview, domain.StateApprovalReady, "fresh structured review passed guarded authorization", evidence, run.CandidateHead)
}

func (c *LocalController) validateApproval(ctx context.Context, run Run) error {
	inspection, err := c.store.Inspect(ctx, run.ID)
	if err != nil {
		return err
	}
	if err := c.validateWorkspace(ctx, run, true); err != nil {
		return err
	}
	batch, ok := successfulVerificationBatch(inspection.Verifications, run.CandidateHead, taskVerifierIDs(run.NormalizedTaskJSON))
	if !ok {
		return errors.New("candidate verification evidence is incomplete")
	}
	if err := validateVerificationBatch(batch, run.CandidateHead); err != nil {
		return err
	}
	if review, ok := latestReviewForHead(inspection.Reviews, run.CandidateHead); ok {
		if review.Verdict == string(domain.ReviewPass) {
			if err := validateReviewAttempt(inspection.Attempts, review, run.ReviewModel); err != nil {
				return err
			}
			outcome, err := readOutcome[domain.ReviewOutcome](review.OutcomePath, review.OutcomeHash)
			if err != nil {
				return err
			}
			if outcome.ReviewedHeadSHA == run.CandidateHead {
				return nil
			}
		}
	}
	return errors.New("passing exact-HEAD review evidence is missing")
}

func latestReviewForHead(records []ReviewRecord, head string) (ReviewRecord, bool) {
	var latest ReviewRecord
	found := false
	for _, record := range records {
		if record.ReviewedHead == head && (!found || record.ID > latest.ID) {
			latest = record
			found = true
		}
	}
	return latest, found
}

func validateReviewAttempt(attempts []Attempt, review ReviewRecord, expectedModel string) error {
	for _, attempt := range attempts {
		if attempt.ID != review.AttemptID {
			continue
		}
		if attempt.Kind != "review" || attempt.Status != "succeeded" || attempt.SessionID != review.SessionID || attempt.OutcomePath != review.OutcomePath || attempt.RequestedModel != expectedModel {
			return errors.New("review attempt evidence does not match review record")
		}
		stdoutHash, stdoutSize, stdoutErr := fileDigest(attempt.StdoutPath)
		stderrHash, stderrSize, stderrErr := fileDigest(attempt.StderrPath)
		if stdoutErr != nil || stderrErr != nil {
			return errors.Join(stdoutErr, stderrErr)
		}
		if stdoutHash != attempt.StdoutHash || stderrHash != attempt.StderrHash || stdoutSize != attempt.StdoutSize || stderrSize != attempt.StderrSize {
			return errors.New("review output digest or size mismatch")
		}
		return nil
	}
	return errors.New("review attempt evidence is missing")
}

func (c *LocalController) validateWorkspace(ctx context.Context, run Run, requireClean bool) error {
	var repository LocalRepository
	if err := json.Unmarshal([]byte(run.RepositoryConfigJSON), &repository); err != nil {
		return err
	}
	record := WorktreeRecord{SourcePath: repository.SourcePath, OriginPath: repository.OriginPath, Path: run.WorktreePath, Branch: run.WorkingBranch, BaseBranch: run.BaseBranch, BaseSHA: run.BaseSHA}
	if err := c.worktrees.ValidateOwned(ctx, record); err != nil {
		return fmt.Errorf("worktree ownership: %w", err)
	}
	branch, err := c.git.Branch(ctx, run.WorktreePath)
	if err != nil {
		return err
	}
	if branch != run.WorkingBranch {
		return errors.New("persisted branch does not match current branch")
	}
	head, err := c.git.Head(ctx, run.WorktreePath)
	if err != nil {
		return err
	}
	if run.CandidateHead != "" && head != run.CandidateHead {
		return errors.New("persisted HEAD does not match current HEAD")
	}
	if err := c.git.ValidateRemoteBase(ctx, run.WorktreePath, run.BaseBranch, head); err != nil {
		return err
	}
	if requireClean {
		status, err := c.git.Status(ctx, run.WorktreePath)
		if err != nil {
			return err
		}
		if strings.TrimSpace(status) != "" {
			return errors.New("worktree has tracked, untracked, or ignored mutation")
		}
	}
	return nil
}

func (c *LocalController) failAttempt(ctx context.Context, attempt Attempt, category string, cause error) error {
	attempt.Status = "failed"
	attempt.FinishedAt = time.Now().UTC()
	attempt.ExitCode = -1
	attempt.ErrorCategory = category
	if attempt.StdoutPath != "" && attempt.StderrPath != "" {
		_ = c.populateAttemptCaptureDigests(&attempt)
	}
	_ = c.store.FinishAttempt(ctx, attempt)
	return cause
}

func (c *LocalController) populateAttemptCaptureDigests(attempt *Attempt) error {
	var err error
	attempt.StdoutHash, attempt.StdoutSize, err = fileDigest(attempt.StdoutPath)
	if err != nil {
		return err
	}
	attempt.StderrHash, attempt.StderrSize, err = fileDigest(attempt.StderrPath)
	return err
}

func decodeTaskSnapshot(value string) (domain.CodingTask, error) {
	var task domain.CodingTask
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&task); err != nil {
		return task, err
	}
	if err := task.Validate(); err != nil {
		return task, err
	}
	return task, nil
}
func newArtifactDirectoryPath(root, kind string) (string, error) {
	value, err := randomIdentifier(kind + "-")
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "attempts", value), nil
}

func randomIdentifier(prefix string) (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(value[:]), nil
}
func newArtifactDirectory(root, kind string) (string, error) {
	path, err := newArtifactDirectoryPath(root, kind)
	if err != nil {
		return "", err
	}
	parentInfo, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	if !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("attempt parent must be a real directory")
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		return "", err
	}
	return path, nil
}
func fileHash(path string) (string, error) {
	hash, _, err := fileDigest(path)
	return hash, err
}

func fileDigest(path string) (string, int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() {
		return "", 0, errors.New("artifact must be a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	digest := sha256.New()
	size, err := io.Copy(digest, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(digest.Sum(nil)), size, nil
}

func bytesHash(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func writeExclusive(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}
func readOutcome[T any](path, wantHash string) (T, error) {
	var value T
	hash, err := fileHash(path)
	if err != nil {
		return value, err
	}
	if hash != wantHash {
		return value, errors.New("persisted outcome hash mismatch")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return value, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, err
	}
	switch typed := any(value).(type) {
	case domain.AgentOutcome:
		err = typed.Validate()
	case domain.ReviewOutcome:
		err = typed.Validate()
	}
	return value, err
}
func taskVerifierIDs(snapshot string) []string {
	task, err := decodeTaskSnapshot(snapshot)
	if err != nil {
		return nil
	}
	return task.VerifierIDs
}

func findPersistedDecision(inspection RunInspection) (Decision, bool, error) {
	for i := len(inspection.Timeline) - 1; i >= 0; i-- {
		transition := inspection.Timeline[i]
		if transition.From != domain.StateAwaitingHumanDecision || transition.To != domain.StateExecuting {
			continue
		}
		var evidence persistedDecisionEvidence
		if err := json.Unmarshal([]byte(transition.EvidenceReference), &evidence); err != nil {
			return Decision{}, false, fmt.Errorf("decode persisted decision evidence: %w", err)
		}
		data, err := os.ReadFile(evidence.Path)
		if err != nil {
			return Decision{}, false, fmt.Errorf("read persisted decision: %w", err)
		}
		if bytesHash(data) != evidence.Hash {
			return Decision{}, false, errors.New("persisted decision hash mismatch")
		}
		var decision Decision
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&decision); err != nil {
			return Decision{}, false, fmt.Errorf("decode persisted decision: %w", err)
		}
		if strings.TrimSpace(decision.ChoiceID) == "" || strings.TrimSpace(decision.Instructions) == "" {
			return Decision{}, false, errors.New("persisted decision is incomplete")
		}
		if decision != evidence.Decision {
			return Decision{}, false, errors.New("persisted decision does not match SQLite evidence")
		}
		attemptFound := false
		var requestOutcome domain.AgentOutcome
		for _, attempt := range inspection.Attempts {
			if attempt.OutcomePath == evidence.RequestOutcomePath && attempt.OutcomeHash == evidence.RequestOutcomeHash && attempt.Status == "succeeded" {
				requestOutcome, err = readOutcome[domain.AgentOutcome](attempt.OutcomePath, attempt.OutcomeHash)
				if err != nil {
					return Decision{}, false, err
				}
				attemptFound = true
			}
		}
		if !attemptFound || requestOutcome.DecisionRequest == nil {
			return Decision{}, false, errors.New("persisted decision request binding is missing")
		}
		choiceAllowed := false
		for _, option := range requestOutcome.DecisionRequest.Options {
			if option.ID == decision.ChoiceID {
				choiceAllowed = true
			}
		}
		if !choiceAllowed {
			return Decision{}, false, errors.New("persisted decision choice is not in the bound request")
		}
		return decision, true, nil
	}
	return Decision{}, false, nil
}
func successfulVerificationBatch(records []VerificationRecord, head string, ids []string) ([]VerificationRecord, bool) {
	if len(ids) == 0 {
		return nil, false
	}
	groups := make(map[string][]VerificationRecord)
	var order []string
	for _, record := range records {
		if record.Phase != "candidate" || record.VerifiedHead != head || record.EvidencePath == "" {
			continue
		}
		if _, ok := groups[record.EvidencePath]; !ok {
			order = append(order, record.EvidencePath)
		}
		groups[record.EvidencePath] = append(groups[record.EvidencePath], record)
	}
	var selected []VerificationRecord
	for _, path := range order {
		group := groups[path]
		success := true
		for _, record := range group {
			if record.ExitCode != 0 {
				success = false
			}
		}
		for _, id := range ids {
			found := false
			for _, record := range group {
				if record.VerifierID == id && record.ExitCode == 0 {
					found = true
				}
			}
			if !found {
				success = false
			}
		}
		if success {
			selected = group
		}
	}
	return selected, len(selected) > 0
}
func validateVerificationBatch(records []VerificationRecord, head string) error {
	if len(records) == 0 {
		return errors.New("verification batch is empty")
	}
	evidencePath := records[0].EvidencePath
	for _, record := range records {
		if record.Phase != "candidate" || record.VerifiedHead != head || record.EvidencePath != evidencePath {
			return errors.New("verification batch identity mismatch")
		}
		hash, err := fileHash(record.EvidencePath)
		if err != nil {
			return err
		}
		if hash != record.EvidenceHash || record.ExitCode != 0 {
			return errors.New("malformed candidate verification evidence")
		}
		stdoutHash, stdoutSize, stdoutErr := fileDigest(record.StdoutPath)
		stderrHash, stderrSize, stderrErr := fileDigest(record.StderrPath)
		if stdoutErr != nil || stderrErr != nil {
			return errors.Join(stdoutErr, stderrErr)
		}
		if stdoutHash != record.StdoutHash || stderrHash != record.StderrHash || stdoutSize != record.StdoutSize || stderrSize != record.StderrSize {
			return errors.New("verification output digest or size mismatch")
		}
	}
	return nil
}
