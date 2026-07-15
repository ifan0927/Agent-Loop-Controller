package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

// ProductionAbandonCommand is the requester-authorized terminal action for an
// automatic-admission run that has not entered recoverable PR delivery work.
type ProductionAbandonCommand struct {
	Requester      Requester
	RunID          string
	Repository     string
	ExpectedState  domain.State
	IdempotencyKey string
}

type ProductionAbandonResult struct {
	Action     ProductionAction `json:"action"`
	Run        RunResult        `json:"run"`
	Idempotent bool             `json:"idempotent"`
}

type abandonLocalResource struct {
	resource    OwnedResource
	expectedSHA string
}

// Abandon performs the durable terminal CAS first, then retries only the
// controller-owned local cleanup boundaries. It never writes Linear, GitHub,
// or a remote Git branch.
func (c *ProductionCoordinator) Abandon(ctx context.Context, command ProductionAbandonCommand, cleanup CleanupPort) (ProductionAbandonResult, error) {
	if cleanup == nil {
		return ProductionAbandonResult{}, serviceError(ErrorInvalidInput, "abandon cleanup port is required", nil)
	}
	if strings.TrimSpace(command.RunID) == "" || strings.TrimSpace(command.Repository) == "" || strings.TrimSpace(command.IdempotencyKey) == "" {
		return ProductionAbandonResult{}, serviceError(ErrorInvalidInput, "run, expected state, repository, and idempotency key are required", nil)
	}
	if command.ExpectedState != domain.StateReceived && command.ExpectedState != domain.StateAdmitting && command.ExpectedState != domain.StateManualIntervention && command.ExpectedState != domain.StateFailed {
		return ProductionAbandonResult{}, serviceError(ErrorInvalidInput, "automatic run abandonment requires received, admitting, manual_intervention, or failed", nil)
	}
	abandonStore, ok := c.store.(AutomaticAdmissionAbandonStore)
	if !ok {
		return ProductionAbandonResult{}, serviceError(ErrorInternal, "configured store cannot persist automatic abandonment", nil)
	}

	preflight, err := c.store.GetRun(ctx, command.RunID)
	if err != nil {
		return ProductionAbandonResult{}, classifyServiceError(err)
	}
	if preflight.Repository != command.Repository || preflight.IdempotencyKey != command.IdempotencyKey {
		return ProductionAbandonResult{}, serviceError(ErrorConflict, "run authority does not match the abandonment request", nil)
	}
	if err := authorizePersistedRequester(preflight, command.Requester); err != nil {
		return ProductionAbandonResult{}, err
	}

	owner, err := randomIdentifier("abandon-")
	if err != nil {
		return ProductionAbandonResult{}, classifyServiceError(err)
	}
	acquired, err := c.store.AcquireLease(ctx, command.RunID, owner, time.Now().UTC().Add(localLeaseTTL))
	if err != nil {
		return ProductionAbandonResult{}, classifyServiceError(err)
	}
	if !acquired {
		return ProductionAbandonResult{}, serviceError(ErrorConflict, "run is actively leased", nil)
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.store.ReleaseLease(releaseCtx, command.RunID, owner)
	}()

	current, err := c.store.GetRun(ctx, command.RunID)
	if err != nil {
		return ProductionAbandonResult{}, classifyServiceError(err)
	}
	if current.State != domain.StateFailed {
		if _, err := c.admission.RevalidateForAbandon(ctx, LinearRevalidateCommand{Requester: command.Requester, RunID: command.RunID, Repository: command.Repository, ExpectedState: command.ExpectedState, IdempotencyKey: command.IdempotencyKey}); err != nil {
			return ProductionAbandonResult{}, err
		}
	}
	inspection, err := c.store.Inspect(ctx, command.RunID)
	if err != nil {
		return ProductionAbandonResult{}, classifyServiceError(err)
	}
	if err := validateAbandonInspection(inspection); err != nil {
		return ProductionAbandonResult{}, serviceError(ErrorConflict, "automatic run abandonment is blocked by retained delivery evidence", err)
	}
	if _, err := selectAbandonLocalResources(inspection.Run, inspection.Resources); err != nil {
		return ProductionAbandonResult{}, serviceError(ErrorConflict, "automatic run local ownership evidence is insufficient", err)
	}

	run, idempotent, err := abandonStore.AbandonAutomaticAdmission(ctx, AutomaticAdmissionAbandonment{RunID: command.RunID, ExpectedState: command.ExpectedState, IdempotencyKey: command.IdempotencyKey})
	if err != nil {
		return ProductionAbandonResult{}, classifyServiceError(err)
	}
	if err := cleanupAbandonedLocalResources(ctx, c.store, run, cleanup); err != nil {
		return ProductionAbandonResult{Action: ProductionAbandon, Run: projectRunResult(run), Idempotent: idempotent}, serviceError(ErrorConflict, "controller-owned local cleanup requires attention", err)
	}
	return ProductionAbandonResult{Action: ProductionAbandon, Run: projectRunResult(run), Idempotent: idempotent}, nil
}

func validateAbandonInspection(inspection RunInspection) error {
	if inspection.PullRequest != nil || inspection.Merge != nil || inspection.Approval != nil || inspection.ApprovalObservation != nil {
		return errors.New("pull request, approval, or merge evidence is retained")
	}
	for _, side := range inspection.SideEffects {
		switch side.Kind {
		case "push", "open_pull_request", "squash_merge", "merge":
			return errors.New("push, pull request, or merge intent is retained")
		case "linear_move_to_started":
			if side.Status == "intent" || side.Status == "in_flight" {
				return errors.New("Linear admission mutation is still in flight")
			}
		}
	}
	for _, resource := range inspection.Resources {
		if resource.Status == "deleted" {
			continue
		}
		if resource.Kind == "remote_branch" || resource.Kind == "pull_request" {
			return errors.New("remote branch or pull request ownership is retained")
		}
	}
	return nil
}

func selectAbandonLocalResources(run Run, resources []OwnedResource) ([]abandonLocalResource, error) {
	var repository LocalRepository
	hasLocal := false
	for _, resource := range resources {
		if resource.RunID == run.ID && (resource.Kind == "worktree" || resource.Kind == "branch") && resource.Status != "deleted" {
			hasLocal = true
			break
		}
	}
	if hasLocal {
		if err := json.Unmarshal([]byte(run.RepositoryConfigJSON), &repository); err != nil {
			return nil, errors.New("persisted repository authority is invalid")
		}
		if repository.CanonicalRepository != run.Repository || repository.BaseBranch != run.BaseBranch {
			return nil, errors.New("persisted repository authority does not match the run")
		}
	}
	selected := make(map[string]abandonLocalResource, 2)
	for _, resource := range resources {
		if resource.RunID != run.ID || resource.Status == "deleted" {
			continue
		}
		switch resource.Kind {
		case "artifact_root":
			if resource.Name != run.ArtifactRoot || (resource.Status != "owned" && resource.Status != "reserved") {
				return nil, errors.New("artifact ownership evidence does not match the run")
			}
		case "worktree", "branch":
			if resource.Status != "owned" && resource.Status != "reserved" {
				return nil, fmt.Errorf("local %s ownership status is not removable", resource.Kind)
			}
			if (resource.Kind == "worktree" && resource.Name != run.WorktreePath) || (resource.Kind == "branch" && resource.Name != run.WorkingBranch) {
				return nil, fmt.Errorf("local %s ownership name does not match the run", resource.Kind)
			}
			if _, found := selected[resource.Kind]; found {
				return nil, fmt.Errorf("duplicate local %s ownership", resource.Kind)
			}
			evidence, err := validateAbandonLocalResource(run, repository, resource)
			if err != nil {
				return nil, err
			}
			selected[resource.Kind] = abandonLocalResource{resource: resource, expectedSHA: evidence.BaseSHA}
		default:
			return nil, fmt.Errorf("unsupported active owned resource %s", resource.Kind)
		}
	}
	ordered := make([]abandonLocalResource, 0, 2)
	if worktree, found := selected["worktree"]; found {
		ordered = append(ordered, worktree)
	}
	if branch, found := selected["branch"]; found {
		ordered = append(ordered, branch)
	}
	if run.CandidateHead != "" {
		for index := range ordered {
			ordered[index].expectedSHA = run.CandidateHead
		}
	}
	if worktree, worktreeFound := selected["worktree"]; worktreeFound {
		if branch, branchFound := selected["branch"]; branchFound && worktreeEvidenceNonce(worktree.resource) != worktreeEvidenceNonce(branch.resource) {
			return nil, errors.New("local worktree and branch ownership nonces do not match")
		}
	}
	return ordered, nil
}

type abandonOwnershipEvidence struct {
	SourcePath string `json:"source_path"`
	OriginPath string `json:"origin_path"`
	Path       string `json:"path"`
	Branch     string `json:"branch"`
	BaseBranch string `json:"base_branch"`
	BaseSHA    string `json:"base_sha"`
	Nonce      string `json:"nonce"`
}

func validateAbandonLocalResource(run Run, repository LocalRepository, resource OwnedResource) (abandonOwnershipEvidence, error) {
	var evidence abandonOwnershipEvidence
	if err := json.Unmarshal([]byte(resource.CreationEvidence), &evidence); err != nil {
		return evidence, errors.New("local ownership evidence is invalid")
	}
	if !filepath.IsAbs(evidence.SourcePath) || !filepath.IsAbs(run.WorktreePath) || evidence.SourcePath != repository.SourcePath || strings.TrimSpace(evidence.OriginPath) == "" || evidence.OriginPath != repository.OriginPath || evidence.Path != run.WorktreePath || evidence.Branch != run.WorkingBranch || evidence.BaseBranch != run.BaseBranch || strings.TrimSpace(evidence.Nonce) == "" {
		return evidence, errors.New("local ownership evidence does not match the persisted repository and run")
	}
	if err := domain.ValidateGitBranch(run.WorkingBranch); err != nil {
		return evidence, errors.New("persisted working branch is invalid")
	}
	if resource.Status == "owned" && (strings.TrimSpace(evidence.BaseSHA) == "" || strings.TrimSpace(run.BaseSHA) == "" || evidence.BaseSHA != run.BaseSHA) {
		return evidence, errors.New("owned local resource lacks the persisted base SHA")
	}
	if evidence.BaseSHA != "" && run.BaseSHA != "" && evidence.BaseSHA != run.BaseSHA {
		return evidence, errors.New("local ownership base SHA does not match the run")
	}
	return evidence, nil
}

func worktreeEvidenceNonce(resource OwnedResource) string {
	var evidence struct {
		Nonce string `json:"nonce"`
	}
	if json.Unmarshal([]byte(resource.CreationEvidence), &evidence) != nil {
		return ""
	}
	return strings.TrimSpace(evidence.Nonce)
}

func cleanupAbandonedLocalResources(ctx context.Context, store RunStore, run Run, cleanup CleanupPort) error {
	inspection, err := store.Inspect(ctx, run.ID)
	if err != nil {
		return err
	}
	resources, err := selectAbandonLocalResources(run, inspection.Resources)
	if err != nil {
		return err
	}
	delivery, hasDelivery := store.(DeliveryStore)
	progress := map[string]CleanupRecord{}
	if hasDelivery {
		records, err := delivery.CleanupProgress(ctx, run.ID)
		if err != nil {
			return err
		}
		for _, record := range records {
			progress[record.Kind+"\x00"+record.Name] = record
		}
		if artifact, found := findAbandonResource(inspection.Resources, run.ID, "artifact_root", run.ArtifactRoot); found && artifact.Status != "deleted" {
			key := artifact.Kind + "\x00" + artifact.Name
			if record, found := progress[key]; !found || record.Status != "retained" {
				if err := delivery.UpsertCleanup(ctx, CleanupRecord{RunID: run.ID, Kind: artifact.Kind, Name: artifact.Name, Status: "retained"}); err != nil {
					return err
				}
			}
		}
		for _, resource := range inspection.Resources {
			if resource.RunID != run.ID || resource.Status != "deleted" || (resource.Kind != "worktree" && resource.Kind != "branch") {
				continue
			}
			key := resource.Kind + "\x00" + resource.Name
			if record, found := progress[key]; !found || record.Status != "deleted" {
				if err := delivery.UpsertCleanup(ctx, CleanupRecord{RunID: run.ID, Kind: resource.Kind, Name: resource.Name, Status: "deleted"}); err != nil {
					return err
				}
			}
		}
	}
	for _, item := range resources {
		if item.resource.Status == "deleted" {
			continue
		}
		if hasDelivery {
			if err := delivery.UpsertCleanup(ctx, CleanupRecord{RunID: run.ID, Kind: item.resource.Kind, Name: item.resource.Name, Status: "intent"}); err != nil {
				return err
			}
		}
		var cleanupErr error
		switch item.resource.Kind {
		case "worktree":
			cleanupErr = cleanup.RemoveWorktree(ctx, run.Repository, item.resource.Name, run.WorkingBranch, item.expectedSHA)
		case "branch":
			cleanupErr = cleanup.DeleteLocalBranch(ctx, run.Repository, item.resource.Name, item.expectedSHA)
		}
		if cleanupErr != nil {
			if hasDelivery {
				_ = delivery.UpsertCleanup(ctx, CleanupRecord{RunID: run.ID, Kind: item.resource.Kind, Name: item.resource.Name, Status: "failed", ErrorClass: classifyCleanupFailure(cleanupErr)})
			}
			return cleanupErr
		}
		if err := store.AddOwnedResource(ctx, OwnedResource{RunID: run.ID, Kind: item.resource.Kind, Name: item.resource.Name, CreationEvidence: item.resource.CreationEvidence, Status: "deleted"}); err != nil {
			return err
		}
		if hasDelivery {
			if err := delivery.UpsertCleanup(ctx, CleanupRecord{RunID: run.ID, Kind: item.resource.Kind, Name: item.resource.Name, Status: "deleted"}); err != nil {
				return err
			}
		}
	}
	return nil
}

func findAbandonResource(resources []OwnedResource, runID, kind, name string) (OwnedResource, bool) {
	for _, resource := range resources {
		if resource.RunID == runID && resource.Kind == kind && resource.Name == name {
			return resource, true
		}
	}
	return OwnedResource{}, false
}
