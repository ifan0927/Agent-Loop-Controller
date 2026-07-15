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
	leaseCtx, stopLease := startAbandonLeaseRenewal(ctx, c.store, command.RunID, owner)
	defer func() {
		stopLease()
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.store.ReleaseLease(releaseCtx, command.RunID, owner)
	}()
	ctx = leaseCtx

	current, err := c.store.GetRun(ctx, command.RunID)
	if err != nil {
		return ProductionAbandonResult{}, classifyServiceError(err)
	}
	revalidated, err := c.admission.RevalidateForAbandon(ctx, LinearRevalidateCommand{Requester: command.Requester, RunID: command.RunID, Repository: command.Repository, ExpectedState: command.ExpectedState, IdempotencyKey: command.IdempotencyKey})
	if err != nil {
		return ProductionAbandonResult{}, err
	}
	current = revalidated
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

	if renewed, err := c.store.RenewLease(ctx, command.RunID, owner, time.Now().UTC().Add(localLeaseTTL)); err != nil {
		return ProductionAbandonResult{}, classifyServiceError(err)
	} else if !renewed {
		return ProductionAbandonResult{}, serviceError(ErrorConflict, "run lease was lost before abandonment", nil)
	}
	run, idempotent, err := abandonStore.AbandonAutomaticAdmission(ctx, AutomaticAdmissionAbandonment{
		Requester:              command.Requester,
		RunID:                  command.RunID,
		Repository:             command.Repository,
		RawIssueHash:           current.RawIssueHash,
		TaskHash:               current.TaskHash,
		ProfileDigest:          current.ProfileDigest,
		RepositoryConfigDigest: AutomaticAdmissionRepositoryConfigDigest(current.RepositoryConfigJSON),
		LeaseOwner:             owner,
		ExpectedState:          command.ExpectedState,
		IdempotencyKey:         command.IdempotencyKey,
	})
	if err != nil {
		return ProductionAbandonResult{}, classifyServiceError(err)
	}
	if err := cleanupAbandonedLocalResourcesWithLease(ctx, c.store, run, cleanup, owner); err != nil {
		return ProductionAbandonResult{Action: ProductionAbandon, Run: projectRunResult(run), Idempotent: idempotent}, serviceError(ErrorConflict, "controller-owned local cleanup requires attention", err)
	}
	return ProductionAbandonResult{Action: ProductionAbandon, Run: projectRunResult(run), Idempotent: idempotent}, nil
}

func startAbandonLeaseRenewal(ctx context.Context, store RunStore, runID, owner string) (context.Context, func()) {
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
				ok, renewErr := store.RenewLease(context.Background(), runID, owner, time.Now().UTC().Add(localLeaseTTL))
				if renewErr != nil {
					cancelLease(fmt.Errorf("renew abandon run lease: %w", renewErr))
					return
				}
				if !ok {
					cancelLease(errors.New("abandon run lease ownership was lost"))
					return
				}
			}
		}
	}()
	return leaseCtx, func() {
		close(stopLease)
		cancelLease(nil)
		<-leaseDone
	}
}

func validateAbandonInspection(inspection RunInspection) error {
	if inspection.PullRequest != nil || inspection.Merge != nil || inspection.Approval != nil || inspection.ApprovalObservation != nil || len(inspection.ReviewReplies) != 0 {
		return errors.New("pull request, approval, merge, or reply evidence is retained")
	}
	for _, side := range inspection.SideEffects {
		if side.Kind != "linear_move_to_started" {
			return errors.New("external side-effect evidence is retained")
		}
		if side.Status != "observed" && side.Status != "failed" {
			return errors.New("Linear admission mutation is still in flight or unresolved")
		}
	}
	for _, cleanup := range inspection.Cleanup {
		if !isAbandonLocalCleanupEvidence(inspection.Run, cleanup) {
			return errors.New("unresolved external cleanup evidence is retained")
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

func isAbandonLocalCleanupEvidence(run Run, record CleanupRecord) bool {
	if record.RunID != run.ID {
		return false
	}
	switch record.Kind {
	case "artifact_root":
		return record.Name == run.ArtifactRoot && record.Status == "retained"
	case "worktree":
		return record.Name == run.WorktreePath && abandonLocalCleanupStatus(record.Status)
	case "branch", "local_branch":
		return record.Name == run.WorkingBranch && abandonLocalCleanupStatus(record.Status)
	case "source_checkout":
		return record.Name == sourceCheckoutCleanupIdentity && abandonSourceCheckoutCleanupStatus(record.Status)
	default:
		return false
	}
}

func abandonLocalCleanupStatus(status string) bool {
	switch status {
	case "intent", "failed", "deleted":
		return true
	default:
		return false
	}
}

func abandonSourceCheckoutCleanupStatus(status string) bool {
	switch status {
	case "intent", "failed", "synced", "skipped_attention":
		return true
	default:
		return false
	}
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
	return cleanupAbandonedLocalResourcesWithLeaseTTL(ctx, store, run, cleanup, "", localLeaseTTL)
}

func renewAbandonLease(ctx context.Context, store RunStore, runID, owner string, leaseTTL time.Duration) (time.Time, error) {
	if strings.TrimSpace(owner) == "" {
		return time.Time{}, nil
	}
	if cause := context.Cause(ctx); cause != nil {
		return time.Time{}, cause
	}
	expiresAt := time.Now().UTC().Add(leaseTTL)
	ok, err := store.RenewLease(ctx, runID, owner, expiresAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("renew abandon run lease: %w", err)
	}
	if !ok {
		return time.Time{}, errors.New("abandon run lease ownership was lost")
	}
	return expiresAt, nil
}

func abandonCleanupOperationContext(ctx context.Context, leaseOwner string, leaseExpiresAt time.Time) (context.Context, context.CancelFunc) {
	if strings.TrimSpace(leaseOwner) == "" || leaseExpiresAt.IsZero() {
		return ctx, func() {}
	}
	return context.WithDeadline(ctx, leaseExpiresAt)
}

const abandonCleanupLastErrorLimit = 512

func abandonCleanupLastError(err error) string {
	if err == nil {
		return ""
	}
	message := sanitizeUntrustedContent(err.Error())
	if message == "" {
		message = "cleanup operation failed"
	}
	if len(message) > abandonCleanupLastErrorLimit {
		message = message[:abandonCleanupLastErrorLimit] + "…"
	}
	return message
}

func upsertAbandonCleanup(ctx context.Context, store RunStore, delivery DeliveryStore, leaseOwner string, record CleanupRecord) error {
	if strings.TrimSpace(leaseOwner) == "" {
		return delivery.UpsertCleanup(ctx, record)
	}
	fenced, ok := store.(AutomaticAdmissionCleanupStore)
	if !ok {
		return errors.New("configured store cannot persist lease-fenced abandon cleanup evidence")
	}
	return fenced.UpsertAutomaticAdmissionCleanup(ctx, leaseOwner, record)
}

func markAbandonResourceDeleted(ctx context.Context, store RunStore, leaseOwner string, resource OwnedResource) error {
	if strings.TrimSpace(leaseOwner) == "" {
		return store.AddOwnedResource(ctx, resource)
	}
	fenced, ok := store.(AutomaticAdmissionCleanupStore)
	if !ok {
		return errors.New("configured store cannot persist lease-fenced abandon resource state")
	}
	return fenced.MarkAutomaticAdmissionResourceDeleted(ctx, leaseOwner, resource)
}

func persistAbandonCleanupFailure(ctx context.Context, store RunStore, delivery DeliveryStore, leaseOwner string, record CleanupRecord, cleanupErr error) error {
	auditCtx := ctx
	var cancel context.CancelFunc
	if auditCtx.Err() != nil {
		auditCtx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	record.Status = "failed"
	record.ErrorClass = classifyCleanupFailure(cleanupErr)
	record.LastError = abandonCleanupLastError(cleanupErr)
	return upsertAbandonCleanup(auditCtx, store, delivery, leaseOwner, record)
}

func abandonCleanupFailure(ctx context.Context, store RunStore, delivery DeliveryStore, leaseOwner string, record CleanupRecord, cleanupErr error) error {
	if auditErr := persistAbandonCleanupFailure(ctx, store, delivery, leaseOwner, record, cleanupErr); auditErr != nil {
		return errors.Join(cleanupErr, fmt.Errorf("persist abandon cleanup failure audit: %w", auditErr))
	}
	return cleanupErr
}

func cleanupAbandonedLocalResourcesWithLease(ctx context.Context, store RunStore, run Run, cleanup CleanupPort, leaseOwner string) error {
	return cleanupAbandonedLocalResourcesWithLeaseTTL(ctx, store, run, cleanup, leaseOwner, localLeaseTTL)
}

func cleanupAbandonedLocalResourcesWithLeaseTTL(ctx context.Context, store RunStore, run Run, cleanup CleanupPort, leaseOwner string, leaseTTL time.Duration) error {
	if _, err := renewAbandonLease(ctx, store, run.ID, leaseOwner, leaseTTL); err != nil {
		return err
	}
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
			if _, err := renewAbandonLease(ctx, store, run.ID, leaseOwner, leaseTTL); err != nil {
				return err
			}
			key := artifact.Kind + "\x00" + artifact.Name
			if record, found := progress[key]; !found || record.Status != "retained" {
				if err := upsertAbandonCleanup(ctx, store, delivery, leaseOwner, CleanupRecord{RunID: run.ID, Kind: artifact.Kind, Name: artifact.Name, Status: "retained"}); err != nil {
					return err
				}
			}
		}
		for _, resource := range inspection.Resources {
			if resource.RunID != run.ID || resource.Status != "deleted" || (resource.Kind != "worktree" && resource.Kind != "branch") {
				continue
			}
			if _, err := renewAbandonLease(ctx, store, run.ID, leaseOwner, leaseTTL); err != nil {
				return err
			}
			key := resource.Kind + "\x00" + resource.Name
			if record, found := progress[key]; !found || record.Status != "deleted" {
				if err := upsertAbandonCleanup(ctx, store, delivery, leaseOwner, CleanupRecord{RunID: run.ID, Kind: resource.Kind, Name: resource.Name, Status: "deleted"}); err != nil {
					return err
				}
			}
		}
	}
	for _, item := range resources {
		if item.resource.Status == "deleted" {
			continue
		}
		if _, err := renewAbandonLease(ctx, store, run.ID, leaseOwner, leaseTTL); err != nil {
			return err
		}
		if hasDelivery {
			if err := upsertAbandonCleanup(ctx, store, delivery, leaseOwner, CleanupRecord{RunID: run.ID, Kind: item.resource.Kind, Name: item.resource.Name, Status: "intent"}); err != nil {
				return err
			}
		}
		leaseExpiresAt, err := renewAbandonLease(ctx, store, run.ID, leaseOwner, leaseTTL)
		if err != nil {
			if hasDelivery {
				return abandonCleanupFailure(ctx, store, delivery, leaseOwner, CleanupRecord{RunID: run.ID, Kind: item.resource.Kind, Name: item.resource.Name}, err)
			}
			return err
		}
		var cleanupErr error
		operationCtx, cancelOperation := abandonCleanupOperationContext(ctx, leaseOwner, leaseExpiresAt)
		switch item.resource.Kind {
		case "worktree":
			cleanupErr = cleanup.RemoveWorktree(operationCtx, run.Repository, item.resource.Name, run.WorkingBranch, item.expectedSHA)
		case "branch":
			cleanupErr = cleanup.DeleteLocalBranch(operationCtx, run.Repository, item.resource.Name, item.expectedSHA)
		}
		cancelOperation()
		if cleanupErr != nil {
			if hasDelivery {
				return abandonCleanupFailure(ctx, store, delivery, leaseOwner, CleanupRecord{RunID: run.ID, Kind: item.resource.Kind, Name: item.resource.Name}, cleanupErr)
			}
			return cleanupErr
		}
		if _, err := renewAbandonLease(ctx, store, run.ID, leaseOwner, leaseTTL); err != nil {
			if hasDelivery {
				return abandonCleanupFailure(ctx, store, delivery, leaseOwner, CleanupRecord{RunID: run.ID, Kind: item.resource.Kind, Name: item.resource.Name}, err)
			}
			return err
		}
		if err := markAbandonResourceDeleted(ctx, store, leaseOwner, OwnedResource{RunID: run.ID, Kind: item.resource.Kind, Name: item.resource.Name, CreationEvidence: item.resource.CreationEvidence, Status: "deleted"}); err != nil {
			return err
		}
		if hasDelivery {
			if err := upsertAbandonCleanup(ctx, store, delivery, leaseOwner, CleanupRecord{RunID: run.ID, Kind: item.resource.Kind, Name: item.resource.Name, Status: "deleted"}); err != nil {
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
