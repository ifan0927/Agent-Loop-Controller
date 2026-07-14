package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type CleanupPort interface {
	RemoveWorktree(context.Context, string, string, string, string) error
	DeleteLocalBranch(context.Context, string, string, string) error
	DeleteRemoteBranch(context.Context, string, string, string) error
}

const sourceCheckoutCleanupIdentity = "configured_source_checkout"

var errSourceSyncRetryable = errors.New("source checkout synchronization is retryable")

// SyncSourceCheckout persists a source-sync intent before the narrow Git port
// is invoked. Source checkout state is deliberately separate from owned
// resources: it is an operator checkout, never a controller-owned deletion
// target.
func SyncSourceCheckout(ctx context.Context, store DeliveryStore, port SourceSyncPort, run Run, merge MergeRecord) error {
	if port == nil {
		return errors.New("source synchronization port is required")
	}
	if merge.RunID != run.ID || merge.PreMergeSHA != run.CandidateHead || merge.Method != "squash" || merge.MergeSHA == "" || merge.MergedAt.IsZero() {
		return errors.New("source synchronization requires persisted squash-merge evidence for the exact candidate")
	}
	if run.State != domain.StateCleaning {
		return errors.New("source synchronization requires cleaning state")
	}
	var repository LocalRepository
	if err := json.Unmarshal([]byte(run.RepositoryConfigJSON), &repository); err != nil || repository.CanonicalRepository != run.Repository || repository.BaseBranch != run.BaseBranch || strings.TrimSpace(repository.SourcePath) == "" || strings.TrimSpace(repository.OriginPath) == "" {
		return errors.New("persisted source synchronization authority is invalid")
	}
	progress, err := store.CleanupProgress(ctx, run.ID)
	if err != nil {
		return err
	}
	for _, record := range progress {
		if record.Kind != "source_checkout" || record.Name != sourceCheckoutCleanupIdentity {
			continue
		}
		if record.Status == "synced" || record.Status == "skipped_attention" {
			return nil
		}
	}
	intent := CleanupRecord{RunID: run.ID, Kind: "source_checkout", Name: sourceCheckoutCleanupIdentity, Status: "intent"}
	if err := store.UpsertCleanup(ctx, intent); err != nil {
		return err
	}
	result, err := port.Sync(ctx, SourceSyncRequest{Repository: run.Repository, SourcePath: repository.SourcePath, OriginPath: repository.OriginPath, BaseBranch: run.BaseBranch, MergeSHA: merge.MergeSHA})
	if err != nil {
		return fmt.Errorf("source synchronization adapter: %w", err)
	}
	status, reason, err := sourceSyncCleanupResult(result, merge.MergeSHA)
	if err != nil {
		return err
	}
	if err := store.UpsertCleanup(ctx, CleanupRecord{RunID: run.ID, Kind: "source_checkout", Name: sourceCheckoutCleanupIdentity, Status: status, ErrorClass: reason}); err != nil {
		return err
	}
	if status == "failed" {
		return errSourceSyncRetryable
	}
	return nil
}

func sourceSyncCleanupResult(result SourceSyncResult, mergeSHA string) (string, string, error) {
	if result.MergeSHA != mergeSHA {
		return "", "", errors.New("source synchronization result is not bound to the persisted merge")
	}
	switch result.Status {
	case SourceSyncSynced:
		if result.Reason != SourceSyncReasonNone || (result.Outcome != SourceSyncFastForwarded && result.Outcome != SourceSyncAlreadyAtTarget && result.Outcome != SourceSyncAlreadyContainsTarget) {
			return "", "", errors.New("source synchronization result is invalid")
		}
		return "synced", "", nil
	case SourceSyncSkippedAttention:
		if result.Outcome != SourceSyncNotApplied {
			return "", "", errors.New("source synchronization result is invalid")
		}
		switch result.Reason {
		case SourceSyncReasonDirtySource, SourceSyncReasonWrongBranch, SourceSyncReasonDetachedHead, SourceSyncReasonSourceDiverged, SourceSyncReasonStateDrift:
			return "skipped_attention", string(result.Reason), nil
		default:
			return "", "", errors.New("source synchronization result is invalid")
		}
	case SourceSyncRetryableFailure:
		if result.Outcome != SourceSyncNotApplied {
			return "", "", errors.New("source synchronization result is invalid")
		}
		switch result.Reason {
		case SourceSyncReasonFetchFailed, SourceSyncReasonGitUncertain:
			return "failed", string(result.Reason), nil
		default:
			return "", "", errors.New("source synchronization result is invalid")
		}
	default:
		return "", "", errors.New("source synchronization result is invalid")
	}
}

func CleanupOwned(ctx context.Context, store DeliveryStore, port CleanupPort, run Run, merge MergeRecord, resources []OwnedResource) error {
	if merge.RunID != run.ID || merge.PreMergeSHA != run.CandidateHead || merge.Method != "squash" || merge.MergeSHA == "" || merge.MergedAt.IsZero() {
		return errors.New("cleanup requires persisted squash-merge evidence for the exact candidate")
	}
	if run.State != domain.StateCleaning {
		return errors.New("cleanup requires cleaning state")
	}
	selection, err := selectCleanupResources(run, resources)
	if err != nil {
		return err
	}
	var partial []error
	progress, err := store.CleanupProgress(ctx, run.ID)
	if err != nil {
		return err
	}
	completed := map[string]bool{}
	for _, item := range progress {
		if item.Status == "deleted" || item.Status == "retained" {
			completed[item.Kind+"\x00"+item.Name] = true
		}
	}
	for _, resource := range selection.resources {
		if completed[resource.Kind+"\x00"+resource.Name] {
			continue
		}
		if err := validateCleanupEvidenceMode(run, resource, selection.legacy); err != nil {
			return err
		}
		result := CleanupRecord{RunID: run.ID, Kind: resource.Kind, Name: resource.Name, Status: "intent"}
		if err := store.UpsertCleanup(ctx, result); err != nil {
			return err
		}
		var err error
		switch resource.Kind {
		case "artifact_root":
			// Artifact collection is intentionally out of scope. Recording retention is
			// durable evidence that this attempt deliberately leaves audit artifacts in place.
			result.Status = "retained"
		case "worktree":
			err = port.RemoveWorktree(ctx, run.Repository, resource.Name, run.WorkingBranch, run.CandidateHead)
		case "branch", "local_branch":
			if resource.Name == run.BaseBranch {
				err = errors.New("refusing to delete base branch")
			} else if resource.Name != run.WorkingBranch {
				err = errors.New("local branch ownership mismatch")
			} else {
				err = port.DeleteLocalBranch(ctx, run.Repository, resource.Name, run.CandidateHead)
			}
		case "remote_branch":
			if resource.Name == run.BaseBranch {
				err = errors.New("refusing to delete base branch")
			} else if resource.Name != run.WorkingBranch {
				err = errors.New("remote branch ownership mismatch")
			} else {
				err = port.DeleteRemoteBranch(ctx, run.Repository, resource.Name, run.CandidateHead)
			}
		default:
			err = fmt.Errorf("unsupported cleanup resource kind %s", resource.Kind)
		}
		if err != nil {
			result.Status = "failed"
			result.ErrorClass = classifyCleanupFailure(err)
			result.LastError = err.Error()
			partial = append(partial, err)
		} else if result.Status != "retained" {
			result.Status = "deleted"
		}
		if saveErr := store.UpsertCleanup(ctx, result); saveErr != nil {
			return saveErr
		}
	}
	return errors.Join(partial...)
}

type cleanupResourceSelection struct {
	resources []OwnedResource
	legacy    bool
}

func selectCleanupResources(run Run, resources []OwnedResource) (cleanupResourceSelection, error) {
	wanted := map[string]string{
		"artifact_root": run.ArtifactRoot,
		"worktree":      run.WorktreePath,
		"branch":        run.WorkingBranch,
		"remote_branch": run.WorkingBranch,
	}
	selected := make(map[string]OwnedResource, len(wanted))
	for _, resource := range resources {
		if resource.RunID != run.ID || resource.Status == "deleted" {
			continue
		}
		name, relevant := wanted[resource.Kind]
		if !relevant {
			continue
		}
		if resource.Status != "owned" || resource.Name != name {
			return cleanupResourceSelection{}, fmt.Errorf("cleanup resource %s is not durably owned", resource.Kind)
		}
		if _, exists := selected[resource.Kind]; exists {
			return cleanupResourceSelection{}, fmt.Errorf("duplicate cleanup ownership resource %s", resource.Kind)
		}
		selected[resource.Kind] = resource
	}
	for kind := range wanted {
		if _, found := selected[kind]; !found {
			return cleanupResourceSelection{}, fmt.Errorf("cleanup requires owned %s resource", kind)
		}
	}
	worktreeNonce, legacy, err := cleanupResourceNonce(selected["worktree"])
	if err != nil {
		return cleanupResourceSelection{}, err
	}
	for _, kind := range []string{"branch", "remote_branch"} {
		nonce, resourceLegacy, err := cleanupResourceNonce(selected[kind])
		if err != nil || resourceLegacy != legacy || (!legacy && nonce != worktreeNonce) {
			return cleanupResourceSelection{}, errors.New("cleanup ownership nonce mismatch")
		}
	}
	// Removing the worktree first makes local branch removal possible. Keep the
	// remote branch independent so a restart can complete any safe remainder.
	localBranch := selected["branch"]
	localBranch.Kind = "local_branch"
	return cleanupResourceSelection{resources: []OwnedResource{selected["artifact_root"], selected["worktree"], selected["remote_branch"], localBranch}, legacy: legacy}, nil
}

func cleanupResourceNonce(resource OwnedResource) (string, bool, error) {
	if resource.Kind == "remote_branch" && isLegacyPushEvidence(resource.CreationEvidence) {
		return "", true, nil
	}
	var evidence struct {
		Nonce string `json:"nonce"`
	}
	if err := json.Unmarshal([]byte(resource.CreationEvidence), &evidence); err != nil {
		return "", false, errors.New("cleanup ownership nonce is invalid")
	}
	nonce := strings.TrimSpace(evidence.Nonce)
	return nonce, nonce == "", nil
}

func validateCleanupEvidence(run Run, resource OwnedResource) error {
	return validateCleanupEvidenceMode(run, resource, false)
}

func validateCleanupEvidenceMode(run Run, resource OwnedResource, legacy bool) error {
	if legacy && resource.Kind == "remote_branch" && isLegacyPushEvidence(resource.CreationEvidence) {
		return nil
	}
	var evidence struct {
		OriginPath string `json:"origin_path"`
		SourcePath string `json:"source_path"`
		Path       string `json:"path"`
		Branch     string `json:"branch"`
		BaseBranch string `json:"base_branch"`
		BaseSHA    string `json:"base_sha"`
		Nonce      string `json:"nonce"`
	}
	if err := json.Unmarshal([]byte(resource.CreationEvidence), &evidence); err != nil {
		return fmt.Errorf("decode cleanup ownership evidence: %w", err)
	}
	switch resource.Kind {
	case "artifact_root":
		var artifact struct {
			Path         string `json:"path"`
			AttemptsPath string `json:"attempts_path"`
			RunRoot      string `json:"run_root"`
			Nonce        string `json:"nonce"`
			TaskHash     string `json:"task_hash"`
		}
		if err := json.Unmarshal([]byte(resource.CreationEvidence), &artifact); err != nil {
			return fmt.Errorf("decode artifact ownership evidence: %w", err)
		}
		if resource.Name != run.ArtifactRoot || artifact.Path != run.ArtifactRoot || artifact.AttemptsPath == "" || artifact.RunRoot == "" || strings.TrimSpace(artifact.Nonce) == "" || artifact.TaskHash != run.TaskHash {
			return errors.New("artifact cleanup ownership evidence mismatch")
		}
	case "worktree":
		if resource.Name != run.WorktreePath || evidence.Path != run.WorktreePath || evidence.Branch != run.WorkingBranch || evidence.BaseBranch != run.BaseBranch || evidence.BaseSHA != run.BaseSHA || strings.TrimSpace(evidence.OriginPath) == "" || strings.TrimSpace(evidence.SourcePath) == "" || (!legacy && strings.TrimSpace(evidence.Nonce) == "") {
			return errors.New("worktree cleanup ownership evidence mismatch")
		}
	case "branch", "local_branch", "remote_branch":
		if resource.Name != run.WorkingBranch || evidence.Branch != run.WorkingBranch || evidence.BaseBranch != run.BaseBranch || evidence.BaseSHA != run.BaseSHA || strings.TrimSpace(evidence.OriginPath) == "" || strings.TrimSpace(evidence.SourcePath) == "" || (!legacy && strings.TrimSpace(evidence.Nonce) == "") {
			return errors.New("branch cleanup ownership evidence mismatch")
		}
	default:
		return fmt.Errorf("unsupported cleanup resource kind %s", resource.Kind)
	}
	return nil
}

func isLegacyPushEvidence(value string) bool {
	if !strings.HasPrefix(value, "push:") {
		return false
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(value, "push:"), 10, 64)
	return err == nil && id > 0
}

func classifyCleanupFailure(err error) string {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "dirty"):
		return "dirty_resource"
	case strings.Contains(message, "mismatch"), strings.Contains(message, "ownership"), strings.Contains(message, "canonical"), strings.Contains(message, "refusing"):
		return "ownership_mismatch"
	case strings.Contains(message, "remote") || strings.Contains(message, "lease"):
		return "remote_conflict"
	default:
		return "operation_failed"
	}
}

type ProductionCleanupCommand struct {
	Requester      Requester
	RunID          string
	Repository     string
	ExpectedState  domain.State
	IdempotencyKey string
}

type ProductionCleanupResult struct {
	Action ProductionAction `json:"action"`
	Run    RunResult        `json:"run"`
}

// Cleanup performs no Linear re-read. Linear completion is the preceding,
// separately persisted gate and its expected automation revision would make a
// normal admission revalidation reject a legitimate completed run.
func (c *ProductionCoordinator) Cleanup(ctx context.Context, command ProductionCleanupCommand, port CleanupPort, sourceSync SourceSyncPort) (ProductionCleanupResult, error) {
	if port == nil || sourceSync == nil || command.RunID == "" || command.Repository == "" || command.ExpectedState == "" || command.IdempotencyKey == "" {
		return ProductionCleanupResult{}, serviceError(ErrorInvalidInput, "cleanup command and adapter are required", nil)
	}
	run, err := c.store.GetRun(ctx, command.RunID)
	if err != nil {
		return ProductionCleanupResult{}, classifyServiceError(err)
	}
	if run.Repository != command.Repository || run.State != command.ExpectedState || run.IdempotencyKey != command.IdempotencyKey {
		return ProductionCleanupResult{}, serviceError(ErrorConflict, "run authority or state changed before cleanup", nil)
	}
	if err := authorizePersistedRequester(run, command.Requester); err != nil {
		return ProductionCleanupResult{}, err
	}
	if action, reason := productionNextAction(run.State); action != ProductionCleanup {
		return ProductionCleanupResult{Action: action, Run: projectRunResult(run)}, serviceError(ErrorConflict, reason, nil)
	}
	delivery, ok := c.store.(DeliveryStore)
	if !ok {
		return ProductionCleanupResult{}, serviceError(ErrorInternal, "configured store cannot persist cleanup evidence", nil)
	}
	inspection, err := c.store.Inspect(ctx, run.ID)
	if err != nil {
		return ProductionCleanupResult{}, classifyServiceError(err)
	}
	if err := validateCleanupGate(run, inspection); err != nil {
		return ProductionCleanupResult{}, serviceError(ErrorConflict, "cleanup gate evidence is incomplete", err)
	}
	if err := SyncSourceCheckout(ctx, delivery, sourceSync, run, *inspection.Merge); err != nil {
		if errors.Is(err, errSourceSyncRetryable) {
			return ProductionCleanupResult{}, serviceError(ErrorUnavailable, "source checkout synchronization is incomplete; retry only the pending source step", err)
		}
		return ProductionCleanupResult{}, serviceError(ErrorConflict, "source checkout synchronization could not be authorized", err)
	}
	if err := CleanupOwned(ctx, delivery, port, run, *inspection.Merge, inspection.Resources); err != nil {
		return ProductionCleanupResult{}, serviceError(ErrorUnavailable, "owned cleanup is incomplete; retry only records still pending", err)
	}
	if err := c.store.Transition(ctx, run.ID, domain.StateCleaning, domain.StateCompleted, "owned cleanup completed", inspection.Merge.MergeSHA, run.CandidateHead); err != nil {
		return ProductionCleanupResult{}, classifyServiceError(err)
	}
	next, err := c.store.GetRun(ctx, run.ID)
	if err != nil {
		return ProductionCleanupResult{}, classifyServiceError(err)
	}
	return ProductionCleanupResult{Action: ProductionStop, Run: projectRunResult(next)}, nil
}

func validateCleanupGate(run Run, inspection RunInspection) error {
	if inspection.Merge == nil || inspection.Merge.RunID != run.ID || inspection.Merge.PreMergeSHA != run.CandidateHead || inspection.Merge.Method != "squash" || inspection.Merge.MergeSHA == "" || inspection.Merge.MergedAt.IsZero() {
		return errors.New("exact squash merge evidence is required")
	}
	if len(inspection.LinearCompletion) == 0 {
		return errors.New("Linear completion evidence is required")
	}
	last := inspection.LinearCompletion[len(inspection.LinearCompletion)-1]
	if last.RunID != run.ID || last.MergeSHA != inspection.Merge.MergeSHA || last.Status != LinearCompletionCompleted || last.ObservedAt.IsZero() {
		return errors.New("completed Linear evidence does not authorize cleanup")
	}
	return nil
}
