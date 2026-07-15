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
	ProfileID               string                 `json:"profile_id"`
	ProfileSnapshotVersion  int                    `json:"profile_snapshot_version"`
	ProfileDigest           string                 `json:"profile_digest"`
	ProfileSnapshotJSON     string                 `json:"-"`
	RegistryVersion         int                    `json:"registry_version"`
	RegistryDigest          string                 `json:"registry_digest"`
	RepositoryBindingDigest string                 `json:"repository_binding_digest"`
	CanonicalRepository     string                 `json:"canonical_repository"`
	LinearLabel             string                 `json:"linear_label"`
	OriginPath              string                 `json:"origin_path"`
	SourcePath              string                 `json:"source_path"`
	RunRoot                 string                 `json:"run_root"`
	WorktreeRoot            string                 `json:"worktree_root"`
	BaseBranch              string                 `json:"base_branch"`
	VerifierRegistryRef     string                 `json:"verifier_registry_ref"`
	VerifierIDs             []string               `json:"verifier_ids"`
	GitHubAppProfileRef     string                 `json:"github_app_profile_ref"`
	GitHubAppID             int64                  `json:"github_app_id"`
	GitHubInstallationID    int64                  `json:"github_installation_id"`
	ExpectedRepositoryID    int64                  `json:"expected_repository_id"`
	AllowedOperatorLogins   []string               `json:"allowed_operator_logins"`
	TrustedOperatorActors   []TrustedActorIdentity `json:"trusted_operator_actors"`
}

type TrustedActorIdentity struct {
	DatabaseID int64  `json:"database_id"`
	NodeID     string `json:"node_id"`
	Login      string `json:"login"`
	Type       string `json:"type"`
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
	return c.StartAuthorized(ctx, input, nil)
}

func (c *LocalController) StartAuthorized(ctx context.Context, input LocalStartInput, authorizeExisting func(Run) error) (Run, error) {
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
	runInput, err := ReservedRunFromAdmissionSnapshot(input)
	if err != nil {
		return Run{}, err
	}
	run, created, err := c.store.CreateRun(ctx, CreateRunInput{Run: runInput})
	if err != nil {
		return Run{}, err
	}
	if !created && authorizeExisting != nil {
		if err := authorizeExisting(run); err != nil {
			return Run{}, err
		}
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
	return c.continueExpected(ctx, runID, "", "", decision)
}

func (c *LocalController) ContinueExpected(ctx context.Context, runID string, expectedState domain.State, idempotencyKey string, decision *Decision) (Run, error) {
	if expectedState == "" || idempotencyKey == "" {
		return Run{}, errors.New("expected state and idempotency key are required")
	}
	return c.continueExpected(ctx, runID, expectedState, idempotencyKey, decision)
}

func (c *LocalController) continueExpected(ctx context.Context, runID string, expectedState domain.State, idempotencyKey string, decision *Decision) (Run, error) {
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
	if expectedState != "" {
		run, err := c.store.GetRun(ctx, runID)
		if err != nil {
			return Run{}, err
		}
		if run.State != expectedState || run.IdempotencyKey != idempotencyKey {
			return Run{}, errors.New("run state or idempotency authority changed before the command was applied")
		}
	}
	for steps := 0; steps < 20; steps++ {
		run, err := c.store.GetRun(ctx, runID)
		if err != nil {
			return Run{}, err
		}
		if stopped, deadlineErr := c.enforceRepairDeadline(ctx, run); deadlineErr != nil {
			return stopped, deadlineErr
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
		case domain.StateRepairing:
			// Repair selection is owned by ProductionCoordinator: it revalidates the
			// persisted Linear source before passing only persisted normalized findings
			// into the bounded implementation-session resume path.
			return run, nil
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
	inspection, err := c.store.Inspect(ctx, run.ID)
	if err != nil {
		return err
	}
	nonce, err := persistedWorktreeNonce(inspection.Resources, path, run.WorkingBranch)
	if err != nil {
		return err
	}
	if nonce == "" {
		nonce, err = randomIdentifier("")
		if err != nil {
			return fmt.Errorf("generate worktree ownership nonce: %w", err)
		}
	}
	spec := WorktreeSpec{SourcePath: repository.SourcePath, OriginPath: repository.OriginPath, BaseBranch: run.BaseBranch, Branch: run.WorkingBranch, Path: path, Nonce: nonce}
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
		record := WorktreeRecord{SourcePath: repository.SourcePath, OriginPath: repository.OriginPath, Path: run.WorktreePath, Branch: run.WorkingBranch, BaseBranch: run.BaseBranch, BaseSHA: run.BaseSHA, Nonce: nonce}
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
		record := WorktreeRecord{SourcePath: repository.SourcePath, OriginPath: repository.OriginPath, Path: path, Branch: run.WorkingBranch, BaseBranch: run.BaseBranch, BaseSHA: baseSHA, Nonce: nonce}
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

func persistedWorktreeNonce(resources []OwnedResource, path, branch string) (string, error) {
	var nonce string
	for _, resource := range resources {
		if (resource.Kind != "worktree" || resource.Name != path) && (resource.Kind != "branch" || resource.Name != branch) {
			continue
		}
		var evidence struct {
			Nonce string `json:"nonce"`
		}
		if err := json.Unmarshal([]byte(resource.CreationEvidence), &evidence); err != nil {
			return "", fmt.Errorf("decode persisted %s ownership evidence: %w", resource.Kind, err)
		}
		current := strings.TrimSpace(evidence.Nonce)
		if current == "" {
			continue
		}
		if nonce != "" && nonce != current {
			return "", errors.New("persisted worktree ownership nonce mismatch")
		}
		nonce = current
	}
	return nonce, nil
}

func (c *LocalController) execute(ctx context.Context, run Run, decision *Decision) error {
	if err := validateRunModelPolicy(run); err != nil {
		return err
	}
	inspection, err := c.store.Inspect(ctx, run.ID)
	if err != nil {
		return err
	}
	hasPersistedRepair := false
	var repairStartedAt time.Time
	if _, found, repairErr := findPersistedRepair(inspection.Timeline); repairErr != nil {
		return repairErr
	} else if found {
		hasPersistedRepair = true
		repairStartedAt = latestRepairStartedAt(inspection.Timeline)
		deadline, expired := repairDeadlineAt(inspection.Timeline, time.Now().UTC())
		if expired {
			if transitionErr := c.store.Transition(ctx, run.ID, domain.StateExecuting, domain.StateManualIntervention, "repair policy deadline exceeded", "repair resume exceeded controller deadline", latestRepairBase(inspection.Timeline)); transitionErr != nil {
				return errors.Join(errors.New("bounded repair deadline exceeded; manual intervention required"), transitionErr)
			}
			return errors.New("bounded repair deadline exceeded; manual intervention required")
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	if run.ImplementationSession != "" && decision == nil {
		persisted, found, loadErr := findPersistedDecision(inspection)
		if loadErr != nil {
			return loadErr
		}
		if found {
			decision = &persisted
		}
		if decision == nil {
			_, repairFound, repairErr := findPersistedRepair(inspection.Timeline)
			if repairErr != nil {
				return repairErr
			}
			if repairFound {
				repair, promptErr := repairPromptForPersistedFindings(inspection)
				if promptErr != nil {
					return promptErr
				}
				decision = &Decision{ChoiceID: "controller-normalized-review-findings", Instructions: repair}
			}
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
		if attempt.Status == "succeeded" && (decision == nil || hasPersistedRepair && !attempt.StartedAt.Before(repairStartedAt)) {
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
		repairBase := latestRepairBase(inspection.Timeline)
		if strings.TrimSpace(status) == "" {
			if head == run.BaseSHA {
				return errors.New("completed implementation produced no candidate changes")
			}
			if repairBase != "" && head == repairBase {
				// A bounded repair may correctly conclude that its persisted finding
				// no longer needs a source change. Reuse only the exact repair base;
				// the verification and fresh-review gates revalidate exact-HEAD
				// evidence before any delivery action can resume.
				run.CandidateHead = repairBase
			} else {
				parent, subject, metaErr := c.git.CommitMetadata(ctx, run.WorktreePath, head)
				if metaErr != nil {
					return metaErr
				}
				expectedParent := run.BaseSHA
				if repairBase != "" {
					expectedParent = repairBase
				}
				if parent != expectedParent || subject != candidateCommitSubject {
					return errors.New("unpersisted HEAD is not a recoverable controller candidate")
				}
				run.CandidateHead = head
			}
		} else {
			expectedHead := run.BaseSHA
			if repairBase != "" {
				expectedHead = repairBase
			}
			if head != expectedHead {
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
	postRepairStarted := latestRepairStartedAt(inspection.Timeline)
	if batch, ok := successfulVerificationBatchAfter(inspection.Verifications, run.CandidateHead, taskVerifierIDs(run.NormalizedTaskJSON), postRepairStarted); ok && postRepairStarted.IsZero() {
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
	if err := validateRunModelPolicy(run); err != nil {
		return err
	}
	if err := c.validateWorkspace(ctx, run, true); err != nil {
		return err
	}
	inspection, err := c.store.Inspect(ctx, run.ID)
	if err != nil {
		return err
	}
	if record, ok := latestReviewForHeadAfter(inspection.Reviews, run.CandidateHead, latestRepairStartedAt(inspection.Timeline)); ok {
		outcome, err := readOutcome[domain.ReviewOutcome](record.OutcomePath, record.OutcomeHash)
		if err != nil {
			return err
		}
		if err := validateReviewAttempt(inspection.Attempts, record, run.ReviewModel); err != nil {
			return err
		}
		if outcome.Verdict != domain.ReviewFailed {
			return c.applyFreshReviewOutcome(ctx, run, outcome, record.OutcomePath, inspection)
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
	return c.applyFreshReviewOutcome(ctx, run, result.Outcome, attempt.OutcomePath, inspection)
}

func (c *LocalController) applyFreshReviewOutcome(ctx context.Context, run Run, outcome domain.ReviewOutcome, evidence string, inspection RunInspection) error {
	switch outcome.Verdict {
	case domain.ReviewPass:
		return c.authorizeReview(ctx, run, outcome, evidence, inspection)
	case domain.ReviewFindings:
		if outcome.ReviewedHeadSHA != run.CandidateHead {
			return errors.New("fresh review findings do not match the candidate HEAD")
		}
		reviewValidated := false
		for _, record := range inspection.Reviews {
			if record.ReviewedHead != run.CandidateHead || record.OutcomePath != evidence {
				continue
			}
			if err := validateReviewAttempt(inspection.Attempts, record, run.ReviewModel); err != nil {
				return err
			}
			reviewValidated = true
		}
		if !reviewValidated {
			return errors.New("fresh review finding attempt evidence is missing")
		}
		findings, err := normalizeFreshReviewFindings(run, outcome)
		if err != nil {
			return err
		}
		persister, ok := c.store.(interface {
			SaveFinding(context.Context, FindingRecord) error
		})
		if !ok {
			return errors.New("fresh review finding persistence is unavailable")
		}
		for _, finding := range findings {
			if err := persister.SaveFinding(ctx, finding); err != nil {
				return err
			}
		}
		return c.store.Transition(ctx, run.ID, domain.StateFreshReview, domain.StateRepairing, "fresh structured review findings persisted", evidence, run.CandidateHead)
	case domain.ReviewFailed:
		return fmt.Errorf("fresh review stopped with verdict %s", outcome.Verdict)
	default:
		return fmt.Errorf("unsupported fresh review verdict %s", outcome.Verdict)
	}
}

const freshReviewFindingSource = "controller_fresh_review"

func normalizeFreshReviewFindings(run Run, outcome domain.ReviewOutcome) ([]FindingRecord, error) {
	if outcome.Verdict != domain.ReviewFindings || outcome.ReviewedHeadSHA != run.CandidateHead {
		return nil, errors.New("fresh review finding authority is incomplete")
	}
	if len(outcome.Findings) > MaxNormalizedFindings {
		return nil, errors.New("fresh review findings exceed controller count bounds")
	}
	findings := make([]FindingRecord, 0, len(outcome.Findings))
	seen := make(map[string]struct{}, len(outcome.Findings))
	for _, finding := range outcome.Findings {
		sourceID := "fresh-review:" + strings.TrimSpace(finding.ID)
		if strings.ContainsRune(sourceID, '\x00') {
			return nil, errors.New("fresh review finding ID contains a NUL byte")
		}
		if _, duplicate := seen[sourceID]; duplicate {
			return nil, errors.New("fresh review findings contain a duplicate ID")
		}
		seen[sourceID] = struct{}{}
		body := fmt.Sprintf("Fresh independent review finding (%s): %s\n\n%s", finding.Severity, finding.Title, finding.Body)
		if len([]byte(body)) > MaxNormalizedFindingBodyBytes || strings.ContainsRune(body, '\x00') {
			return nil, errors.New("fresh review finding body exceeds controller bounds")
		}
		record := FindingRecord{
			RunID:      run.ID,
			SourceID:   sourceID,
			Source:     freshReviewFindingSource,
			Severity:   finding.Severity,
			Body:       body,
			BodyDigest: bytesHash([]byte(body)),
			HeadSHA:    run.CandidateHead,
			ObservedAt: time.Now().UTC(),
		}
		if finding.File != nil {
			record.File = *finding.File
		}
		if finding.Line != nil {
			record.Line = *finding.Line
		}
		findings = append(findings, record)
	}
	if len(findings) == 0 {
		return nil, errors.New("fresh review findings are empty")
	}
	if len([]byte(BuildRepairPrompt(findings))) > MaxRepairPromptBytes {
		return nil, errors.New("fresh review findings exceed controller aggregate bounds")
	}
	return findings, nil
}

func (c *LocalController) authorizeReview(ctx context.Context, run Run, outcome domain.ReviewOutcome, evidence string, inspection RunInspection) error {
	if outcome.Verdict != domain.ReviewPass {
		return fmt.Errorf("fresh review stopped with verdict %s", outcome.Verdict)
	}
	batch, ok := successfulVerificationBatchAfter(inspection.Verifications, run.CandidateHead, taskVerifierIDs(run.NormalizedTaskJSON), latestRepairStartedAt(inspection.Timeline))
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
	if repairBase := latestRepairBase(inspection.Timeline); repairBase != "" {
		needsBinding := false
		for _, feedback := range inspection.TrustedFeedback {
			needsBinding = needsBinding || (feedback.Lifecycle == domain.TrustedReviewFeedbackSelectedForRepair && feedback.OriginalReviewHeadSHA == repairBase)
		}
		if !needsBinding {
			return c.store.Transition(ctx, run.ID, domain.StateFreshReview, domain.StateApprovalReady, "fresh structured review passed guarded authorization", evidence, run.CandidateHead)
		}
		transitions, ok := c.store.(interface {
			TransitionTrustedReviewFeedback(context.Context, string, string, domain.TrustedReviewFeedbackLifecycle, domain.TrustedReviewFeedbackLifecycle, string, string, int64, string, bool, bool) (TrustedReviewFeedbackRecord, bool, error)
		})
		if !ok {
			return errors.New("trusted feedback lifecycle persistence is unavailable")
		}
		for _, feedback := range inspection.TrustedFeedback {
			if feedback.Lifecycle != domain.TrustedReviewFeedbackSelectedForRepair || feedback.OriginalReviewHeadSHA != repairBase {
				continue
			}
			if _, changed, err := transitions.TransitionTrustedReviewFeedback(ctx, run.ID, feedback.RootCommentNodeID, domain.TrustedReviewFeedbackSelectedForRepair, domain.TrustedReviewFeedbackRepairVerified, run.CandidateHead, "", 0, "", false, false); err != nil || !changed {
				if err != nil {
					return err
				}
				return errors.New("trusted feedback repair verification compare failed")
			}
		}
	}
	return c.store.Transition(ctx, run.ID, domain.StateFreshReview, domain.StateApprovalReady, "fresh structured review passed guarded authorization", evidence, run.CandidateHead)
}

func (c *LocalController) validateApproval(ctx context.Context, run Run) error {
	if err := validateRunModelPolicy(run); err != nil {
		return err
	}
	inspection, err := c.store.Inspect(ctx, run.ID)
	if err != nil {
		return err
	}
	if err := c.validateWorkspace(ctx, run, true); err != nil {
		return err
	}
	batch, ok := successfulVerificationBatchAfter(inspection.Verifications, run.CandidateHead, taskVerifierIDs(run.NormalizedTaskJSON), latestRepairStartedAt(inspection.Timeline))
	if !ok {
		return errors.New("candidate verification evidence is incomplete")
	}
	if err := validateVerificationBatch(batch, run.CandidateHead); err != nil {
		return err
	}
	if review, ok := latestReviewForHeadAfter(inspection.Reviews, run.CandidateHead, latestRepairStartedAt(inspection.Timeline)); ok {
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

// ValidateApprovalReady revalidates the complete persisted model, verification,
// review, artifact, and exact-HEAD evidence before an external publisher runs.
func (c *LocalController) ValidateApprovalReady(ctx context.Context, runID string) error {
	run, err := c.store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.State != domain.StateApprovalReady && run.State != domain.StatePushingBranch && run.State != domain.StateBranchPushed && run.State != domain.StateOpeningPR && run.State != domain.StateReplyingReviewFeedback && run.State != domain.StateAwaitingHumanApproval && run.State != domain.StateMerging {
		return fmt.Errorf("delivery approval validation cannot authorize state %s", run.State)
	}
	return c.validateApproval(ctx, run)
}

// Repair resumes the persisted Terra implementation session with only
// controller-normalized review data, then runs the ordinary verification and
// fresh Sol review pipeline through Continue.
func (c *LocalController) Repair(ctx context.Context, runID, normalizedPrompt string) (Run, error) {
	return c.repair(ctx, runID, normalizedPrompt, nil)
}

// RepairFindings resumes the persisted Terra session using a deterministic,
// bounded selection of controller-owned normalized findings.
func (c *LocalController) RepairFindings(ctx context.Context, runID string, findings []FindingRecord) (Run, error) {
	run, err := c.store.GetRun(ctx, runID)
	if err != nil {
		return Run{}, err
	}
	inspection, inspectErr := c.store.Inspect(ctx, runID)
	if inspectErr != nil {
		return run, inspectErr
	}
	selected, err := RepairableFindings(findings, run.CandidateHead, inspection.TrustedFeedback)
	if err != nil {
		if run.State == domain.StateRepairing {
			_ = c.store.Transition(ctx, runID, domain.StateRepairing, domain.StateManualIntervention, "unsupported actionable review findings", "repair finding selection failed", run.CandidateHead)
			updated, getErr := c.store.GetRun(ctx, runID)
			if getErr == nil {
				return updated, err
			}
		}
		return run, err
	}
	evidence := repairEvidenceFor(selected)
	return c.repair(ctx, runID, evidence.Prompt, &evidence)
}

func (c *LocalController) repair(ctx context.Context, runID, normalizedPrompt string, persisted *repairEvidence) (Run, error) {
	if strings.TrimSpace(normalizedPrompt) == "" {
		return Run{}, errors.New("normalized repair prompt must not be blank")
	}
	run, err := c.store.GetRun(ctx, runID)
	if err != nil {
		return Run{}, err
	}
	if run.State != domain.StateRepairing {
		return run, fmt.Errorf("repair requires repairing state, got %s", run.State)
	}
	if err := validateRunModelPolicy(run); err != nil {
		return run, err
	}
	inspection, err := c.store.Inspect(ctx, runID)
	if err != nil {
		return run, err
	}
	if repairDeadlineExceeded(inspection.Timeline, time.Now().UTC()) {
		err := errors.New("bounded repair deadline exceeded; manual intervention required")
		if transitionErr := c.store.Transition(ctx, runID, domain.StateRepairing, domain.StateManualIntervention, "repair policy deadline exceeded", err.Error(), run.CandidateHead); transitionErr != nil {
			return run, errors.Join(err, transitionErr)
		}
		updated, getErr := c.store.GetRun(ctx, runID)
		if getErr != nil {
			return run, errors.Join(err, getErr)
		}
		return updated, err
	}
	task, err := decodeTaskSnapshot(run.NormalizedTaskJSON)
	if err != nil {
		return run, err
	}
	count := 0
	for _, transition := range inspection.Timeline {
		if transition.From == domain.StateRepairing && transition.To == domain.StateExecuting {
			count++
		}
	}
	if count >= task.Policy.MaxRepairAttempts {
		err := errors.New("bounded repair attempts exhausted; manual intervention required")
		if transitionErr := c.store.Transition(ctx, runID, domain.StateRepairing, domain.StateManualIntervention, "repair policy exhausted", err.Error(), run.CandidateHead); transitionErr != nil {
			return run, errors.Join(err, transitionErr)
		}
		updated, getErr := c.store.GetRun(ctx, runID)
		if getErr != nil {
			return run, errors.Join(err, getErr)
		}
		return updated, err
	}
	if persisted == nil {
		fallback, persistErr := c.persistLegacyRepairInput(ctx, run, normalizedPrompt)
		if persistErr != nil {
			return run, persistErr
		}
		persisted = &fallback
	}
	evidenceData, _ := json.Marshal(persisted)
	if err := c.store.BeginRepair(ctx, runID, run.CandidateHead, string(evidenceData)); err != nil {
		return run, err
	}
	postBeginInspection, inspectErr := c.store.Inspect(ctx, runID)
	if inspectErr != nil {
		return run, inspectErr
	}
	deadline, _ := repairDeadlineAt(postBeginInspection.Timeline, time.Now().UTC())
	repairCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	updated, continueErr := c.Continue(repairCtx, runID, &Decision{ChoiceID: "controller-normalized-review-findings", Instructions: normalizedPrompt})
	// Continue may unwind through either the caller deadline or the repair policy
	// deadline. Re-read with a short detached context so only the persisted policy
	// clock—not a shorter caller context—can authorize manual intervention.
	recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer recoveryCancel()
	if persistedRun, getErr := c.store.GetRun(recoveryCtx, runID); getErr == nil {
		updated = persistedRun
	}
	if recoveredInspection, getErr := c.store.Inspect(recoveryCtx, runID); getErr == nil && repairDeadlineExceeded(recoveredInspection.Timeline, time.Now().UTC()) {
		stopped, deadlineErr := c.persistExpiredRepairDeadline(recoveryCtx, updated, "repair execution exceeded controller deadline")
		if deadlineErr != nil {
			return stopped, errors.Join(continueErr, deadlineErr)
		}
		return stopped, continueErr
	}
	return updated, continueErr
}

func (c *LocalController) enforceRepairDeadline(ctx context.Context, run Run) (Run, error) {
	if !domain.CanRequireManualIntervention(run.State) {
		return run, nil
	}
	inspection, err := c.store.Inspect(ctx, run.ID)
	if err != nil {
		return run, err
	}
	if !repairDeadlineExceeded(inspection.Timeline, time.Now().UTC()) {
		return run, nil
	}
	return c.persistExpiredRepairDeadline(ctx, run, "repair workflow exceeded controller deadline")
}

func (c *LocalController) persistExpiredRepairDeadline(ctx context.Context, run Run, evidence string) (Run, error) {
	if !domain.CanRequireManualIntervention(run.State) {
		return run, nil
	}
	err := errors.New("bounded repair deadline exceeded; manual intervention required")
	if transitionErr := c.store.Transition(ctx, run.ID, run.State, domain.StateManualIntervention, "repair policy deadline exceeded", evidence, run.CandidateHead); transitionErr != nil {
		return run, errors.Join(err, transitionErr)
	}
	updated, getErr := c.store.GetRun(ctx, run.ID)
	if getErr != nil {
		return run, errors.Join(err, getErr)
	}
	return updated, err
}

// persistLegacyRepairInput keeps the deprecated direct Repair path resumable
// without putting raw prompt text in transition evidence. New GitHub feedback
// repairs use RepairFindings; this bounded record exists solely for the
// fixture-compatible direct path.
func (c *LocalController) persistLegacyRepairInput(ctx context.Context, run Run, prompt string) (repairEvidence, error) {
	if len([]byte(prompt)) > MaxRepairPromptBytes || strings.ContainsRune(prompt, '\x00') {
		return repairEvidence{}, errors.New("legacy repair prompt exceeds controller bounds")
	}
	digest := bytesHash([]byte(prompt))
	record := FindingRecord{RunID: run.ID, Source: "controller_legacy_repair", SourceID: "legacy-repair:" + digest, Body: prompt, BodyDigest: digest, HeadSHA: run.CandidateHead, ObservedAt: time.Now().UTC()}
	persister, ok := c.store.(interface {
		SaveFinding(context.Context, FindingRecord) error
	})
	if !ok {
		return repairEvidence{}, errors.New("legacy repair input persistence is unavailable")
	}
	if err := persister.SaveFinding(ctx, record); err != nil {
		return repairEvidence{}, err
	}
	return repairEvidence{Prompt: prompt, Hash: digest, Findings: []repairFindingReference{{Source: record.Source, SourceID: record.SourceID, BodyDigest: record.BodyDigest, HeadSHA: record.HeadSHA}}}, nil
}

func findPersistedRepair(timeline []Transition) (string, bool, error) {
	for index := len(timeline) - 1; index >= 0; index-- {
		item := timeline[index]
		if item.From != domain.StateRepairing || item.To != domain.StateExecuting {
			continue
		}
		var evidence repairEvidence
		if err := json.Unmarshal([]byte(item.EvidenceReference), &evidence); err != nil {
			return "", false, err
		}
		if strings.TrimSpace(evidence.Hash) == "" {
			return "", false, errors.New("persisted repair prompt evidence is invalid")
		}
		return "", true, nil
	}
	return "", false, nil
}

func repairPromptForPersistedFindings(inspection RunInspection) (string, error) {
	var expected repairEvidence
	found := false
	for index := len(inspection.Timeline) - 1; index >= 0; index-- {
		item := inspection.Timeline[index]
		if item.From != domain.StateRepairing || item.To != domain.StateExecuting {
			continue
		}
		if err := json.Unmarshal([]byte(item.EvidenceReference), &expected); err != nil {
			return "", err
		}
		found = true
		break
	}
	if !found || len(expected.Findings) == 0 {
		return "", errors.New("persisted repair evidence is incomplete")
	}
	if len(expected.Findings) == 1 && expected.Findings[0].Source == "controller_legacy_repair" {
		ref := expected.Findings[0]
		for _, finding := range inspection.Findings {
			if finding.Source == ref.Source && finding.SourceID == ref.SourceID && finding.HeadSHA == ref.HeadSHA && finding.BodyDigest == ref.BodyDigest && bytesHash([]byte(finding.Body)) == ref.BodyDigest {
				return finding.Body, nil
			}
		}
		return "", errors.New("persisted legacy repair input is unavailable")
	}
	selected, err := RepairableFindings(inspection.Findings, latestRepairBase(inspection.Timeline), inspection.TrustedFeedback)
	if err != nil {
		return "", err
	}
	actual := repairEvidenceFor(selected)
	if actual.Hash != expected.Hash || !sameRepairFindingReferences(actual.Findings, expected.Findings) {
		return "", errors.New("persisted repair finding authority changed")
	}
	return actual.Prompt, nil
}

func sameRepairFindingReferences(a, b []repairFindingReference) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

func latestRepairBase(timeline []Transition) string {
	for index := len(timeline) - 1; index >= 0; index-- {
		item := timeline[index]
		if item.From == domain.StateRepairing && item.To == domain.StateExecuting {
			return item.BoundHead
		}
	}
	return ""
}

func latestRepairStartedAt(timeline []Transition) time.Time {
	for index := len(timeline) - 1; index >= 0; index-- {
		item := timeline[index]
		if item.From == domain.StateRepairing && item.To == domain.StateExecuting {
			return item.CreatedAt
		}
	}
	return time.Time{}
}

func validateRunModelPolicy(run Run) error {
	if run.ImplementationModel != codex.ImplementationModel {
		return fmt.Errorf("run has missing or unsupported implementation model evidence: %q", run.ImplementationModel)
	}
	if run.ReviewModel != codex.ReviewModel {
		return fmt.Errorf("run has missing or unsupported review model evidence: %q", run.ReviewModel)
	}
	return nil
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

func latestReviewForHeadAfter(records []ReviewRecord, head string, notBefore time.Time) (ReviewRecord, bool) {
	var latest ReviewRecord
	found := false
	for _, record := range records {
		if record.ReviewedHead != head || (!notBefore.IsZero() && record.CreatedAt.Before(notBefore)) || (found && record.ID <= latest.ID) {
			continue
		}
		latest, found = record, true
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

func successfulVerificationBatchAfter(records []VerificationRecord, head string, ids []string, notBefore time.Time) ([]VerificationRecord, bool) {
	if notBefore.IsZero() {
		return successfulVerificationBatch(records, head, ids)
	}
	filtered := make([]VerificationRecord, 0, len(records))
	for _, record := range records {
		if !record.CreatedAt.Before(notBefore) {
			filtered = append(filtered, record)
		}
	}
	return successfulVerificationBatch(filtered, head, ids)
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
