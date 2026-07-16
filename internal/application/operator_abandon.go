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
	Action           ProductionAction `json:"action"`
	Run              RunResult        `json:"run"`
	Idempotent       bool             `json:"idempotent"`
	ResidueAttention bool             `json:"residue_attention"`
}

type abandonLocalResource struct {
	resource    OwnedResource
	expectedSHA string
}

type abandonRemoteDeletionReplay struct {
	resource OwnedResource
}

type abandonGitHubEvidenceStore interface {
	GitHubEvidenceStore
	SavePullRequest(context.Context, string, domain.PullRequest) error
}

// Abandon records operator intent, applies guarded best-effort cleanup, and
// then terminalizes the run regardless of cleanup residue. It never writes
// Linear or claims that retained external resources were deleted.
func (c *ProductionCoordinator) Abandon(ctx context.Context, command ProductionAbandonCommand, cleanup CleanupPort, childStopper AutomaticAdmissionChildStopper, readers ...GitHubReadPort) (ProductionAbandonResult, error) {
	if cleanup == nil {
		return ProductionAbandonResult{}, serviceError(ErrorInvalidInput, "abandon cleanup port is required", nil)
	}
	if childStopper == nil {
		return ProductionAbandonResult{}, serviceError(ErrorInvalidInput, "abandon child stopper is required", nil)
	}
	if len(readers) > 1 {
		return ProductionAbandonResult{}, serviceError(ErrorInvalidInput, "at most one abandon GitHub reader is allowed", nil)
	}
	if strings.TrimSpace(command.RunID) == "" || strings.TrimSpace(command.Repository) == "" || strings.TrimSpace(command.IdempotencyKey) == "" {
		return ProductionAbandonResult{}, serviceError(ErrorInvalidInput, "run, expected state, repository, and idempotency key are required", nil)
	}
	if !GracefulAbandonState(command.ExpectedState) {
		return ProductionAbandonResult{}, serviceError(ErrorInvalidInput, "run state is not eligible for graceful abandonment", nil)
	}
	abandonStore, ok := c.store.(AutomaticAdmissionAbandonStore)
	if !ok {
		return ProductionAbandonResult{}, serviceError(ErrorInternal, "configured store cannot persist automatic abandonment", nil)
	}
	actionStore, ok := c.store.(OperatorActionStore)
	if !ok {
		return ProductionAbandonResult{}, serviceError(ErrorInternal, "configured store cannot persist operator action provenance", nil)
	}
	actions, err := NewOperatorActionService(actionStore)
	if err != nil {
		return ProductionAbandonResult{}, serviceError(ErrorInternal, "operator action service is unavailable", err)
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
	if preflight.State == domain.StateFailed && command.ExpectedState == domain.StateFailed {
		terminalInspection, err := c.store.Inspect(ctx, command.RunID)
		if err != nil {
			return ProductionAbandonResult{}, classifyServiceError(err)
		}
		action, err := prepareGracefulAbandonAction(ctx, actions, terminalInspection, command.Requester)
		if err != nil {
			return ProductionAbandonResult{}, err
		}
		residueAttention, err := hasAbandonResidueAttention(ctx, c.store, command.RunID)
		if err != nil {
			return ProductionAbandonResult{}, classifyServiceError(err)
		}
		if !residueAttention {
			action, err = recordGracefulAbandonApplied(ctx, actions, c.store, action, preflight)
			if err != nil {
				return ProductionAbandonResult{}, err
			}
			if err := recordGracefulAbandonObserved(ctx, actions, action, false); err != nil {
				return ProductionAbandonResult{}, err
			}
			return ProductionAbandonResult{Action: ProductionAbandon, Run: projectRunResult(preflight), Idempotent: true}, nil
		}
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
		return ProductionAbandonResult{}, serviceError(ErrorConflict, "automatic run abandonment is blocked by possible external merge authority", err)
	}
	action, err := prepareGracefulAbandonAction(ctx, actions, inspection, command.Requester)
	if err != nil {
		return ProductionAbandonResult{}, err
	}
	// Once operator intent is durable, request cancellation is cleanup residue,
	// not authority to strand the singleton slot. Cleanup gets a narrower budget
	// than terminalization, while lease loss still cancels every subsequent
	// action.
	stopLease()
	cleanupBudget := c.gracefulAbandonCleanupBudget()
	completionBase, cancelCompletion := context.WithTimeout(context.Background(), cleanupBudget+4*localLeaseTTL)
	defer cancelCompletion()
	leaseCtx, stopLease = startAbandonLeaseRenewal(completionBase, c.store, command.RunID, owner)
	cleanupCtx, cancelCleanup := context.WithTimeout(leaseCtx, cleanupBudget)
	defer cancelCleanup()
	ctx = cleanupCtx
	remoteDeletionCandidate, remoteDeletionIntent, replayCleanupErr := inspectAbandonRemoteDeletionIntent(ctx, c.store, inspection)
	if inspection.PullRequest != nil {
		if len(readers) != 1 || readers[0] == nil {
			replayCleanupErr = errors.Join(replayCleanupErr, errors.New("graceful abandonment lacks a fresh GitHub reader for the persisted pull request"))
		} else {
			persistedInspection := inspection
			refreshed, refreshErr := refreshAbandonGitHubEvidence(ctx, c.store, inspection, readers[0])
			if refreshErr != nil {
				if refreshed.GitHubEvidence != nil && refreshed.GitHubEvidence.PullRequest.Merged && validateAbandonMergedGitHubRead(refreshed, readers[0].Authority()) == nil {
					if journalErr := recordGracefulAbandonBlocked(ctx, actions, action, refreshed); journalErr != nil {
						return ProductionAbandonResult{}, serviceError(ErrorInternal, "merged abandonment result could not be persisted", journalErr)
					}
					return ProductionAbandonResult{}, serviceError(ErrorConflict, "graceful abandonment is blocked because the pull request is merged", refreshErr)
				}
				replayCleanupErr = errors.Join(replayCleanupErr, refreshErr)
			} else {
				inspection = refreshed
			}
			if inspection.Run.ID == "" {
				inspection = persistedInspection
			}
		}
	}
	attempts, ok := c.store.(AutomaticAdmissionAttemptStopStore)
	if !ok {
		return ProductionAbandonResult{}, serviceError(ErrorInternal, "configured store cannot stop abandon child attempts", nil)
	}
	var childStopErr error
	for _, attempt := range inspection.Attempts {
		if attempt.RunID == command.RunID && attempt.Status == "started" {
			childStopErr = errors.Join(childStopErr, childStopper.StopAttempt(ctx, attempt.ArtifactDir, attempt.ProcessControlKey))
		}
	}
	if childStopErr == nil {
		if _, err := attempts.StopAutomaticAdmissionAttempts(ctx, command.RunID, owner, time.Now().UTC()); err != nil {
			childStopErr = errors.Join(childStopErr, err)
		}
	}
	cleanupErr := errors.Join(replayCleanupErr, childStopErr)
	if childStopErr == nil {
		cleanupErr = errors.Join(cleanupErr, cleanupAbandonedLocalResourcesWithLeaseMode(ctx, c.store, current, cleanup, owner, inspection.PullRequest != nil, false))
	}
	if inspection.PullRequest != nil && len(readers) == 1 && readers[0] != nil {
		var remoteDeletionReplay *abandonRemoteDeletionReplay
		if childStopErr == nil && remoteDeletionCandidate != nil {
			remoteDeletionReplay, err = observeAbandonRemoteDeletionReplay(ctx, c.store, inspection.Run, *remoteDeletionCandidate, cleanup, owner)
			if err != nil {
				cleanupErr = errors.Join(cleanupErr, err)
			}
		}
		finalInspection, finalReadErr := refreshAbandonGitHubEvidence(ctx, c.store, inspection, readers[0])
		finalStatusValidated := finalReadErr == nil
		if finalReadErr != nil {
			cleanupErr = errors.Join(cleanupErr, finalReadErr)
			if remoteDeletionReplay != nil && validateAbandonPostDeleteGitHubRead(finalInspection, readers[0].Authority()) == nil {
				finalStatusValidated = true
			}
		}
		if finalStatusValidated && childStopErr == nil {
			inspection = finalInspection
			if remoteDeletionReplay != nil {
				if err := applyAbandonRemoteDeletionReplay(ctx, c.store, inspection.Run, *remoteDeletionReplay, owner); err != nil {
					cleanupErr = errors.Join(cleanupErr, err)
				}
			}
			if !remoteDeletionIntent {
				cleanupErr = errors.Join(cleanupErr, cleanupAbandonedLocalResourcesWithLeaseMode(ctx, c.store, current, cleanup, owner, false, true))
			}
			cleanupErr = errors.Join(cleanupErr, reconcileAbandonPullRequestAfterCleanup(ctx, c.store, inspection, owner))
		}
	}

	cleanupContextErr := ctx.Err()
	cancelCleanup()
	ctx = leaseCtx
	cleanupErr = errors.Join(cleanupErr, cleanupContextErr)
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
	action, err = recordGracefulAbandonApplied(ctx, actions, c.store, action, run)
	if err != nil {
		return ProductionAbandonResult{Action: ProductionAbandon, Run: projectRunResult(run), Idempotent: idempotent}, err
	}
	residue := cleanupErr != nil
	if retained, retainedErr := hasAbandonResidueAttention(ctx, c.store, run.ID); retainedErr != nil {
		return ProductionAbandonResult{Action: ProductionAbandon, Run: projectRunResult(run), Idempotent: idempotent, ResidueAttention: residue}, classifyServiceError(retainedErr)
	} else if retained {
		residue = true
	}
	if residue {
		if publishErr := publishAbandonResidueAttention(ctx, c.store, run, cleanupErr); publishErr != nil {
			return ProductionAbandonResult{Action: ProductionAbandon, Run: projectRunResult(run), Idempotent: idempotent, ResidueAttention: true}, serviceError(ErrorInternal, "abandon cleanup residue attention could not be persisted", errors.Join(cleanupErr, publishErr))
		}
	}
	if err := recordGracefulAbandonObserved(ctx, actions, action, residue); err != nil {
		return ProductionAbandonResult{Action: ProductionAbandon, Run: projectRunResult(run), Idempotent: idempotent, ResidueAttention: residue}, err
	}
	return ProductionAbandonResult{Action: ProductionAbandon, Run: projectRunResult(run), Idempotent: idempotent, ResidueAttention: residue}, nil
}

func (c *ProductionCoordinator) gracefulAbandonCleanupBudget() time.Duration {
	if c.abandonCleanupTTL > 0 {
		return c.abandonCleanupTTL
	}
	return 4 * localLeaseTTL
}

func reconcileAbandonPullRequestAfterCleanup(ctx context.Context, store RunStore, inspection RunInspection, leaseOwner string) error {
	delivery, ok := store.(DeliveryStore)
	if !ok {
		return errors.New("configured store cannot persist pull request cleanup adoption")
	}
	if _, err := renewAbandonLease(ctx, store, inspection.Run.ID, leaseOwner, localLeaseTTL); err != nil {
		return err
	}
	valid, residue := validAbandonPullRequestResources(inspection)
	for _, resource := range inspection.Resources {
		if resource.RunID != inspection.Run.ID || resource.Kind != "pull_request" || resource.Status == "deleted" {
			continue
		}
		status := "retained"
		if valid[resource.Name] && inspection.PullRequest != nil && !inspection.PullRequest.Merged && strings.EqualFold(inspection.PullRequest.State, "closed") {
			if err := markAbandonResourceDeleted(ctx, store, leaseOwner, OwnedResource{RunID: inspection.Run.ID, Kind: resource.Kind, Name: resource.Name, CreationEvidence: resource.CreationEvidence, Status: "deleted"}); err != nil {
				residue = append(residue, err)
			} else {
				status = "deleted"
			}
		}
		if err := upsertAbandonCleanup(ctx, store, delivery, leaseOwner, CleanupRecord{RunID: inspection.Run.ID, Kind: resource.Kind, Name: resource.Name, Status: status}); err != nil {
			return err
		}
		if status == "retained" {
			residue = append(residue, errors.New("unmerged pull request is retained for operator cleanup"))
		}
	}
	return errors.Join(residue...)
}

func refreshAbandonGitHubEvidence(ctx context.Context, store RunStore, inspection RunInspection, reader GitHubReadPort) (RunInspection, error) {
	githubStore, ok := store.(abandonGitHubEvidenceStore)
	if !ok {
		return RunInspection{}, serviceError(ErrorInternal, "configured store cannot persist abandon GitHub evidence", nil)
	}
	expectedRepository, err := mergeExpectedRepository(inspection)
	if err != nil {
		return RunInspection{}, serviceError(ErrorConflict, "persisted GitHub repository authority is invalid", err)
	}
	evidence, _, observations, metadata, readErr := reader.Read(ctx, inspection.PullRequest.Number, inspection.Run.CandidateHead)
	inspection.GitHubEvidence = &evidence
	if err := persistAbandonGitHubRequests(ctx, githubStore, inspection.Run.ID, observations); err != nil {
		return RunInspection{}, serviceError(ErrorInternal, "fresh abandon GitHub request evidence could not be persisted", err)
	}
	if readErr != nil {
		return inspection, serviceError(ErrorUnavailable, "fresh abandon GitHub read failed", readErr)
	}
	if err := validateAbandonGitHubAuthority(inspection, metadata, expectedRepository); err != nil {
		return RunInspection{}, serviceError(ErrorConflict, "fresh abandon GitHub authority does not match the run", err)
	}
	if err := ReconcileGitHubRead(expectedRepository, *inspection.PullRequest, inspection.Run.WorkingBranch, inspection.Run.BaseBranch, inspection.Run.CandidateHead, inspection.Run.BaseSHA, inspection.Run.IdempotencyKey, inspection.PullRequest.BodyDigest, evidence); err != nil {
		return RunInspection{}, serviceError(ErrorConflict, "fresh abandon GitHub evidence does not match the run", err)
	}
	if evidence.PullRequest.Merged {
		return inspection, serviceError(ErrorConflict, "graceful abandonment is blocked because the pull request is merged", nil)
	}
	if !strings.EqualFold(evidence.PullRequest.State, "open") && !strings.EqualFold(evidence.PullRequest.State, "closed") {
		return RunInspection{}, serviceError(ErrorConflict, "fresh pull request state is not safely classifiable", nil)
	}
	if metadata.AppID > 0 {
		if err := githubStore.SaveGitHubInstallation(ctx, inspection.Run.ID, metadata); err != nil {
			return RunInspection{}, serviceError(ErrorInternal, "fresh abandon GitHub authority could not be persisted", err)
		}
	}
	if err := githubStore.SaveGitHubEvidence(ctx, inspection.Run.ID, evidence); err != nil {
		return RunInspection{}, serviceError(ErrorInternal, "fresh abandon GitHub evidence could not be persisted", err)
	}
	if err := githubStore.SavePullRequest(ctx, inspection.Run.ID, evidence.PullRequest); err != nil {
		return RunInspection{}, serviceError(ErrorConflict, "fresh pull request state could not be adopted", err)
	}
	inspection.PullRequest = &evidence.PullRequest
	inspection.GitHubEvidence = &evidence
	return inspection, nil
}

func inspectAbandonRemoteDeletionIntent(ctx context.Context, store RunStore, inspection RunInspection) (*abandonRemoteDeletionReplay, bool, error) {
	delivery, ok := store.(DeliveryStore)
	if !ok {
		return nil, false, nil
	}
	progress, err := delivery.CleanupProgress(ctx, inspection.Run.ID)
	if err != nil {
		return nil, false, err
	}
	intent := false
	for _, record := range progress {
		if record.RunID == inspection.Run.ID && record.Kind == "remote_branch" && record.Name == inspection.Run.WorkingBranch && record.Status == "intent" {
			intent = true
			break
		}
	}
	if !intent {
		return nil, false, nil
	}
	if inspection.PullRequest == nil || inspection.PullRequest.Merged {
		return nil, true, errors.New("remote deletion intent lacks unmerged pull request authority")
	}
	validPullRequests, residue := validAbandonPullRequestResources(inspection)
	if len(residue) != 0 || !validPullRequests[fmt.Sprint(inspection.PullRequest.Number)] {
		return nil, true, errors.New("remote deletion intent lacks exact owned pull request authority")
	}
	resources, classifyResidue := classifyAbandonLocalResources(inspection.Run, inspection.Resources)
	if len(classifyResidue) != 0 {
		return nil, true, errors.Join(classifyResidue...)
	}
	var remote *abandonLocalResource
	for index := range resources {
		if resources[index].resource.Kind == "remote_branch" {
			remote = &resources[index]
			break
		}
	}
	if remote == nil {
		return nil, true, errors.New("remote deletion intent lacks exact owned remote resource authority")
	}
	return &abandonRemoteDeletionReplay{resource: remote.resource}, true, nil
}

func observeAbandonRemoteDeletionReplay(ctx context.Context, store RunStore, run Run, candidate abandonRemoteDeletionReplay, cleanup CleanupPort, leaseOwner string) (*abandonRemoteDeletionReplay, error) {
	reconciler, ok := cleanup.(CleanupReconciliationPort)
	if !ok {
		return nil, errors.New("remote deletion intent cannot be reconciled without repeating a delete")
	}
	if _, err := renewAbandonLease(ctx, store, run.ID, leaseOwner, localLeaseTTL); err != nil {
		return nil, err
	}
	absent, err := reconciler.CleanupResourceAbsent(ctx, run.Repository, candidate.resource.Kind, candidate.resource.Name)
	if err != nil {
		return nil, err
	}
	if !absent {
		return nil, errors.New("remote deletion intent remains unresolved while the remote ref exists")
	}
	return &candidate, nil
}

func applyAbandonRemoteDeletionReplay(ctx context.Context, store RunStore, run Run, replay abandonRemoteDeletionReplay, leaseOwner string) error {
	delivery, ok := store.(DeliveryStore)
	if !ok {
		return errors.New("configured store cannot persist remote deletion replay")
	}
	if _, err := renewAbandonLease(ctx, store, run.ID, leaseOwner, localLeaseTTL); err != nil {
		return err
	}
	deleted := OwnedResource{RunID: run.ID, Kind: replay.resource.Kind, Name: replay.resource.Name, CreationEvidence: replay.resource.CreationEvidence, Status: "deleted"}
	if err := markAbandonResourceDeleted(ctx, store, leaseOwner, deleted); err != nil {
		return err
	}
	return upsertAbandonCleanup(ctx, store, delivery, leaseOwner, CleanupRecord{RunID: run.ID, Kind: replay.resource.Kind, Name: replay.resource.Name, Status: "deleted"})
}

func validateAbandonPostDeleteGitHubRead(inspection RunInspection, metadata GitHubInstallationMetadata) error {
	if inspection.PullRequest == nil || inspection.GitHubEvidence == nil {
		return errors.New("post-delete GitHub status evidence is unavailable")
	}
	expectedRepository, err := mergeExpectedRepository(inspection)
	if err != nil {
		return err
	}
	if err := validateAbandonGitHubAuthority(inspection, metadata, expectedRepository); err != nil {
		return err
	}
	got := inspection.GitHubEvidence
	expected := inspection.PullRequest
	if got.Repository.ID != expectedRepository.ID || !strings.EqualFold(got.Repository.Owner, expectedRepository.Owner) || !strings.EqualFold(got.Repository.Name, expectedRepository.Name) {
		return errors.New("post-delete GitHub repository identity mismatch")
	}
	if got.PullRequest.Number != expected.Number || got.PullRequest.DatabaseID != expected.DatabaseID || got.PullRequest.NodeID != expected.NodeID || got.PullRequest.URL != expected.URL {
		return errors.New("post-delete pull request identity mismatch")
	}
	if got.PullRequest.Merged || (!strings.EqualFold(got.PullRequest.State, "open") && !strings.EqualFold(got.PullRequest.State, "closed")) {
		return errors.New("post-delete pull request state is not safely classifiable")
	}
	if got.PullRequest.HeadBranch != inspection.Run.WorkingBranch || got.PullRequest.BaseBranch != inspection.Run.BaseBranch || got.PullRequest.BaseSHA != inspection.Run.BaseSHA || got.PullRequest.BodyDigest != expected.BodyDigest || got.PullRequest.OwnershipKey != inspection.Run.IdempotencyKey {
		return errors.New("post-delete pull request ownership mismatch")
	}
	if got.PullRequest.HeadSHA != "" && got.PullRequest.HeadSHA != inspection.Run.CandidateHead {
		return errors.New("post-delete pull request head drift")
	}
	return nil
}

func validateAbandonMergedGitHubRead(inspection RunInspection, metadata GitHubInstallationMetadata) error {
	if inspection.PullRequest == nil || inspection.GitHubEvidence == nil || !inspection.GitHubEvidence.PullRequest.Merged {
		return errors.New("merged GitHub evidence is unavailable")
	}
	expectedRepository, err := mergeExpectedRepository(inspection)
	if err != nil {
		return err
	}
	if err := validateAbandonGitHubAuthority(inspection, metadata, expectedRepository); err != nil {
		return err
	}
	return ReconcileGitHubRead(expectedRepository, *inspection.PullRequest, inspection.Run.WorkingBranch, inspection.Run.BaseBranch, inspection.Run.CandidateHead, inspection.Run.BaseSHA, inspection.Run.IdempotencyKey, inspection.PullRequest.BodyDigest, *inspection.GitHubEvidence)
}

func persistAbandonGitHubRequests(ctx context.Context, store abandonGitHubEvidenceStore, runID string, observations []GitHubRequestObservation) error {
	for _, observation := range observations {
		observation.RunID = runID
		if err := store.SaveGitHubRequest(ctx, observation); err != nil {
			return err
		}
	}
	return nil
}

func validateAbandonGitHubAuthority(inspection RunInspection, metadata GitHubInstallationMetadata, expected domain.RepositoryIdentity) error {
	if metadata.Repository.ID != expected.ID || !strings.EqualFold(metadata.Repository.Owner, expected.Owner) || !strings.EqualFold(metadata.Repository.Name, expected.Name) {
		return errors.New("GitHub repository authority mismatch")
	}
	if inspection.GitHubInstallation != nil {
		persisted := inspection.GitHubInstallation
		if metadata.AppID != persisted.AppID || metadata.InstallationID != persisted.InstallationID {
			return errors.New("GitHub installation authority mismatch")
		}
		return nil
	}
	if inspection.RepositoryBinding == nil || metadata.AppID != inspection.RepositoryBinding.GitHubAppID || metadata.InstallationID != inspection.RepositoryBinding.GitHubInstallationID {
		return errors.New("GitHub installation authority mismatch")
	}
	return nil
}

func prepareGracefulAbandonAction(ctx context.Context, actions *OperatorActionService, inspection RunInspection, requester Requester) (OperatorActionRecord, error) {
	if inspection.Run.State == domain.StateFailed {
		for index := len(inspection.OperatorActions) - 1; index >= 0; index-- {
			action := inspection.OperatorActions[index]
			if action.ActionType == OperatorActionAbandon && action.RunID == inspection.Run.ID && action.Repository == inspection.Run.Repository && action.RunIdempotencyKey == inspection.Run.IdempotencyKey && sameAbandonRequester(action.Requester, requester) {
				return action, nil
			}
		}
		return OperatorActionRecord{}, serviceError(ErrorConflict, "terminal run lacks graceful-abandon action provenance", nil)
	}
	event, found, err := actions.store.CurrentOperatorAttention(ctx, inspection.Run.ID)
	if err != nil {
		return OperatorActionRecord{}, classifyServiceError(err)
	}
	if !found || event.EventKey == "" || event.ControllerState != string(inspection.Run.State) {
		return OperatorActionRecord{}, serviceError(ErrorConflict, "run has no current parked abandon authority", nil)
	}
	action, _, err := actions.Prepare(ctx, OperatorActionInput{Requester: requester, RunID: inspection.Run.ID, Repository: inspection.Run.Repository, ExpectedState: inspection.Run.State, RunIdempotencyKey: inspection.Run.IdempotencyKey, TransitionSequence: latestTransitionSequence(inspection.Timeline), ActionType: OperatorActionAbandon, ReasonCode: event.ReasonCode, AttentionEventKey: event.EventKey})
	return action, err
}

func sameAbandonRequester(left, right Requester) bool {
	return strings.EqualFold(left.ID, right.ID) && left.Kind == right.Kind && left.DatabaseID == right.DatabaseID && left.NodeID == right.NodeID && left.ActorType == right.ActorType
}

func recordGracefulAbandonApplied(ctx context.Context, actions *OperatorActionService, store RunStore, action OperatorActionRecord, run Run) (OperatorActionRecord, error) {
	if action.Status != OperatorActionStatusValidated {
		return action, nil
	}
	inspection, err := store.Inspect(ctx, run.ID)
	if err != nil {
		return OperatorActionRecord{}, classifyServiceError(err)
	}
	at := run.UpdatedAt.UTC()
	if at.Before(action.ValidatedAt) {
		at = action.ValidatedAt
	}
	evidence := bytesHash([]byte("operator-abandon-applied\x00" + action.ActionID + "\x00" + fmt.Sprint(latestTransitionSequence(inspection.Timeline))))
	applied, _, err := actions.RecordApplied(ctx, OperatorActionMutationResult{ActionID: action.ActionID, ExpectedStatus: OperatorActionStatusValidated, ResultStatus: OperatorActionResultApplied, ResultingState: domain.StateFailed, ResultingTransitionSequence: latestTransitionSequence(inspection.Timeline), EvidenceDigest: evidence, At: at})
	return applied, err
}

func recordGracefulAbandonObserved(ctx context.Context, actions *OperatorActionService, action OperatorActionRecord, residue bool) error {
	if action.Status != OperatorActionStatusApplied {
		return nil
	}
	outcome := bytesHash([]byte(fmt.Sprintf("operator-abandon-observed\x00%s\x00%t", action.ActionID, residue)))
	_, _, err := actions.RecordObserved(ctx, OperatorActionMutationResult{ActionID: action.ActionID, ExpectedStatus: OperatorActionStatusApplied, ResultStatus: OperatorActionResultSucceeded, ResultingState: action.ResultingState, ResultingTransitionSequence: action.ResultingTransitionSequence, EvidenceDigest: outcome, At: action.AppliedAt.Add(time.Nanosecond)})
	return err
}

func recordGracefulAbandonBlocked(ctx context.Context, actions *OperatorActionService, action OperatorActionRecord, inspection RunInspection) error {
	if action.Status == OperatorActionStatusValidated {
		at := time.Now().UTC()
		if at.Before(action.ValidatedAt) {
			at = action.ValidatedAt
		}
		evidence := bytesHash([]byte("operator-abandon-blocked\x00" + action.ActionID + "\x00" + fmt.Sprint(latestTransitionSequence(inspection.Timeline))))
		applied, _, err := actions.RecordApplied(ctx, OperatorActionMutationResult{ActionID: action.ActionID, ExpectedStatus: OperatorActionStatusValidated, ResultStatus: OperatorActionResultApplied, ResultingState: inspection.Run.State, ResultingTransitionSequence: latestTransitionSequence(inspection.Timeline), EvidenceDigest: evidence, At: at})
		if err != nil {
			return err
		}
		action = applied
	}
	if action.Status != OperatorActionStatusApplied {
		return nil
	}
	outcome := bytesHash([]byte("operator-abandon-blocked-merged\x00" + action.ActionID))
	_, _, err := actions.RecordObserved(ctx, OperatorActionMutationResult{ActionID: action.ActionID, ExpectedStatus: OperatorActionStatusApplied, ResultStatus: OperatorActionResultFailed, ResultingState: action.ResultingState, ResultingTransitionSequence: action.ResultingTransitionSequence, EvidenceDigest: outcome, At: action.AppliedAt.Add(time.Nanosecond)})
	return err
}

func publishAbandonResidueAttention(ctx context.Context, store RunStore, run Run, cleanupErr error) error {
	publisher, ok := store.(OperatorAttentionPublisher)
	if !ok {
		return errors.New("configured store cannot persist cleanup residue attention")
	}
	if retained, err := hasAbandonResidueAttention(ctx, store, run.ID); err != nil {
		return err
	} else if retained {
		return nil
	}
	inspection, err := store.Inspect(ctx, run.ID)
	if err != nil {
		return err
	}
	evidence := bytesHash([]byte(fmt.Sprintf("abandon-cleanup-residue\x00%s\x00%d\x00%s", run.ID, len(inspection.Cleanup), classifyCleanupFailure(cleanupErr))))
	event, err := CleanupResidueAttentionEvent(run, latestTransitionSequence(inspection.Timeline), evidence, time.Now().UTC())
	if err != nil {
		return err
	}
	_, err = publisher.AppendOperatorAttention(ctx, event)
	return err
}

func hasAbandonResidueAttention(ctx context.Context, store RunStore, runID string) (bool, error) {
	query, ok := store.(CurrentOperatorAttentionQuery)
	if !ok {
		return false, errors.New("configured store cannot read cleanup residue attention")
	}
	event, found, err := query.CurrentOperatorAttention(ctx, runID)
	if err != nil {
		return false, err
	}
	return found && event.RunID == runID && event.EventType == OperatorAttentionCleanupResidue && event.ControllerState == string(domain.StateFailed), nil
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
	if inspection.Merge != nil || inspection.PullRequest != nil && inspection.PullRequest.Merged {
		return errors.New("external merge evidence is retained")
	}
	for _, side := range inspection.SideEffects {
		if side.Kind == "squash_merge" {
			return errors.New("merge side-effect authority is ambiguous")
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
	case "remote_branch":
		return record.Name == run.WorkingBranch && abandonRemoteCleanupStatus(record.Status)
	case "pull_request":
		return record.Status == "retained" || record.Status == "deleted"
	case "source_checkout":
		return record.Name == sourceCheckoutCleanupIdentity && abandonSourceCheckoutCleanupStatus(record.Status)
	default:
		return false
	}
}

func abandonRemoteCleanupStatus(status string) bool {
	switch status {
	case "intent", "failed", "deleted", "retained":
		return true
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
		if resource.RunID == run.ID && (resource.Kind == "worktree" || resource.Kind == "branch" || resource.Kind == "remote_branch") && resource.Status != "deleted" {
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
	selected := make(map[string]abandonLocalResource, 3)
	for _, resource := range resources {
		if resource.RunID != run.ID || resource.Status == "deleted" {
			continue
		}
		switch resource.Kind {
		case "artifact_root":
			if resource.Name != run.ArtifactRoot || (resource.Status != "owned" && resource.Status != "reserved") {
				return nil, errors.New("artifact ownership evidence does not match the run")
			}
		case "worktree", "branch", "remote_branch":
			if resource.Status != "owned" && resource.Status != "reserved" {
				return nil, fmt.Errorf("local %s ownership status is not removable", resource.Kind)
			}
			if (resource.Kind == "worktree" && resource.Name != run.WorktreePath) || ((resource.Kind == "branch" || resource.Kind == "remote_branch") && resource.Name != run.WorkingBranch) {
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
		case "pull_request":
			if resource.Status != "owned" && resource.Status != "reserved" {
				return nil, errors.New("pull request ownership status is not adoptable")
			}
			if resource.Name == "" || !strings.HasPrefix(resource.CreationEvidence, "open_pull_request:") {
				return nil, errors.New("pull request ownership evidence is invalid")
			}
		default:
			return nil, fmt.Errorf("unsupported active owned resource %s", resource.Kind)
		}
	}
	ordered := make([]abandonLocalResource, 0, 3)
	if worktree, found := selected["worktree"]; found {
		ordered = append(ordered, worktree)
	}
	if branch, found := selected["branch"]; found {
		ordered = append(ordered, branch)
	}
	if remote, found := selected["remote_branch"]; found {
		ordered = append(ordered, remote)
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

func classifyAbandonLocalResources(run Run, resources []OwnedResource) ([]abandonLocalResource, []error) {
	selected := make(map[string]abandonLocalResource, 3)
	seen := make(map[string]bool, 3)
	invalid := make(map[string]bool, 3)
	var residue []error
	for _, resource := range resources {
		if resource.RunID != run.ID || resource.Status == "deleted" || resource.Kind == "artifact_root" || resource.Kind == "pull_request" {
			continue
		}
		if resource.Kind != "worktree" && resource.Kind != "branch" && resource.Kind != "remote_branch" {
			residue = append(residue, fmt.Errorf("unsupported active owned resource %s", resource.Kind))
			continue
		}
		items, err := selectAbandonLocalResources(run, []OwnedResource{resource})
		if err != nil {
			residue = append(residue, err)
			continue
		}
		if len(items) != 1 {
			residue = append(residue, fmt.Errorf("local %s ownership is not classifiable", resource.Kind))
			continue
		}
		if seen[resource.Kind] {
			delete(selected, resource.Kind)
			invalid[resource.Kind] = true
			residue = append(residue, fmt.Errorf("duplicate local %s ownership", resource.Kind))
			continue
		}
		seen[resource.Kind] = true
		if !invalid[resource.Kind] {
			selected[resource.Kind] = items[0]
		}
	}
	if worktree, found := selected["worktree"]; found {
		if branch, branchFound := selected["branch"]; branchFound && worktreeEvidenceNonce(worktree.resource) != worktreeEvidenceNonce(branch.resource) {
			delete(selected, "worktree")
			delete(selected, "branch")
			residue = append(residue, errors.New("local worktree and branch ownership nonces do not match"))
		}
	}
	ordered := make([]abandonLocalResource, 0, len(selected))
	for _, kind := range []string{"worktree", "branch", "remote_branch"} {
		if item, found := selected[kind]; found {
			ordered = append(ordered, item)
		}
	}
	return ordered, residue
}

func validAbandonPullRequestResources(inspection RunInspection) (map[string]bool, []error) {
	valid := make(map[string]bool)
	var residue []error
	for _, resource := range inspection.Resources {
		if resource.RunID != inspection.Run.ID || resource.Kind != "pull_request" || resource.Status == "deleted" {
			continue
		}
		if inspection.PullRequest == nil || resource.Name != fmt.Sprint(inspection.PullRequest.Number) || resource.Status != "owned" && resource.Status != "reserved" || !strings.HasPrefix(resource.CreationEvidence, "open_pull_request:") {
			residue = append(residue, errors.New("pull request ownership evidence does not match the run"))
			continue
		}
		sideID := strings.TrimPrefix(resource.CreationEvidence, "open_pull_request:")
		matched := false
		for _, side := range inspection.SideEffects {
			if fmt.Sprint(side.ID) == sideID && side.Kind == "open_pull_request" && side.Status == "observed" {
				matched = true
				break
			}
		}
		if !matched {
			residue = append(residue, errors.New("pull request ownership side effect is unavailable"))
			continue
		}
		valid[resource.Name] = true
	}
	if inspection.PullRequest != nil && !valid[fmt.Sprint(inspection.PullRequest.Number)] {
		residue = append(residue, errors.New("persisted pull request lacks safe ownership evidence"))
	}
	return valid, residue
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
	return cleanupAbandonedLocalResourcesWithLeaseTTL(ctx, store, run, cleanup, "", localLeaseTTL, false, false)
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
	return cleanupAbandonedLocalResourcesWithLeaseTTL(ctx, store, run, cleanup, leaseOwner, localLeaseTTL, false, false)
}

func cleanupAbandonedLocalResourcesWithLeaseMode(ctx context.Context, store RunStore, run Run, cleanup CleanupPort, leaseOwner string, deferRemote, remoteOnly bool) error {
	return cleanupAbandonedLocalResourcesWithLeaseTTL(ctx, store, run, cleanup, leaseOwner, localLeaseTTL, deferRemote, remoteOnly)
}

func cleanupAbandonedLocalResourcesWithLeaseTTL(ctx context.Context, store RunStore, run Run, cleanup CleanupPort, leaseOwner string, leaseTTL time.Duration, deferRemote, remoteOnly bool) error {
	if _, err := renewAbandonLease(ctx, store, run.ID, leaseOwner, leaseTTL); err != nil {
		return err
	}
	inspection, err := store.Inspect(ctx, run.ID)
	if err != nil {
		return err
	}
	resources, partial := classifyAbandonLocalResources(run, inspection.Resources)
	validPullRequests, pullRequestResidue := validAbandonPullRequestResources(inspection)
	partial = append(partial, pullRequestResidue...)
	var retainedRemoteWithoutSafePR *abandonLocalResource
	safeRemoteAuthority := inspection.PullRequest != nil &&
		!inspection.PullRequest.Merged &&
		strings.EqualFold(inspection.PullRequest.State, "open") &&
		validPullRequests[fmt.Sprint(inspection.PullRequest.Number)]
	if !safeRemoteAuthority {
		filtered := resources[:0]
		for index := range resources {
			if resources[index].resource.Kind == "remote_branch" {
				retained := resources[index]
				retainedRemoteWithoutSafePR = &retained
				partial = append(partial, errors.New("remote branch is retained because no fresh open unmerged owned pull request authority exists"))
				continue
			}
			filtered = append(filtered, resources[index])
		}
		resources = filtered
	}
	if remoteOnly {
		filtered := resources[:0]
		for _, resource := range resources {
			if resource.resource.Kind == "remote_branch" {
				filtered = append(filtered, resource)
			}
		}
		resources = filtered
	}
	remoteCleanupPlanned := false
	for _, resource := range resources {
		if resource.resource.Kind == "remote_branch" {
			remoteCleanupPlanned = true
			break
		}
	}
	if deferRemote && remoteCleanupPlanned {
		filtered := resources[:0]
		for _, resource := range resources {
			if resource.resource.Kind != "remote_branch" {
				filtered = append(filtered, resource)
			}
		}
		resources = filtered
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
		if retainedRemoteWithoutSafePR != nil {
			resource := retainedRemoteWithoutSafePR.resource
			if err := upsertAbandonCleanup(ctx, store, delivery, leaseOwner, CleanupRecord{RunID: run.ID, Kind: resource.Kind, Name: resource.Name, Status: "retained"}); err != nil {
				return err
			}
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
			if resource.RunID != run.ID || resource.Kind != "pull_request" || resource.Status == "deleted" {
				continue
			}
			status := "retained"
			if validPullRequests[resource.Name] && inspection.PullRequest != nil && !inspection.PullRequest.Merged && strings.EqualFold(inspection.PullRequest.State, "closed") {
				status = "deleted"
				if err := markAbandonResourceDeleted(ctx, store, leaseOwner, OwnedResource{RunID: run.ID, Kind: resource.Kind, Name: resource.Name, CreationEvidence: resource.CreationEvidence, Status: "deleted"}); err != nil {
					partial = append(partial, err)
					status = "retained"
				}
			}
			if err := upsertAbandonCleanup(ctx, store, delivery, leaseOwner, CleanupRecord{RunID: run.ID, Kind: resource.Kind, Name: resource.Name, Status: status}); err != nil {
				return err
			}
			if status == "retained" && !(inspection.PullRequest != nil && strings.EqualFold(inspection.PullRequest.State, "open") && remoteCleanupPlanned) {
				partial = append(partial, errors.New("unmerged pull request is retained for operator cleanup"))
			}
		}
		for _, resource := range inspection.Resources {
			if resource.RunID != run.ID || resource.Status != "deleted" || (resource.Kind != "worktree" && resource.Kind != "branch" && resource.Kind != "remote_branch") {
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
		key := item.resource.Kind + "\x00" + item.resource.Name
		if record, found := progress[key]; found && record.Status == "intent" {
			reconciler, ok := cleanup.(CleanupReconciliationPort)
			if !ok {
				partial = append(partial, errors.New("cleanup intent cannot be reconciled without repeating a delete"))
				continue
			}
			absent, reconcileErr := reconciler.CleanupResourceAbsent(ctx, run.Repository, item.resource.Kind, item.resource.Name)
			if reconcileErr != nil {
				if hasDelivery {
					reconcileErr = abandonCleanupFailure(ctx, store, delivery, leaseOwner, CleanupRecord{RunID: run.ID, Kind: item.resource.Kind, Name: item.resource.Name}, reconcileErr)
				}
				partial = append(partial, reconcileErr)
				continue
			}
			if absent {
				if err := markAbandonResourceDeleted(ctx, store, leaseOwner, OwnedResource{RunID: run.ID, Kind: item.resource.Kind, Name: item.resource.Name, CreationEvidence: item.resource.CreationEvidence, Status: "deleted"}); err != nil {
					return err
				}
				if hasDelivery {
					if err := upsertAbandonCleanup(ctx, store, delivery, leaseOwner, CleanupRecord{RunID: run.ID, Kind: item.resource.Kind, Name: item.resource.Name, Status: "deleted"}); err != nil {
						return err
					}
				}
				continue
			}
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
		case "remote_branch":
			cleanupErr = cleanup.DeleteRemoteBranch(operationCtx, run.Repository, item.resource.Name, item.expectedSHA)
		}
		cancelOperation()
		if cleanupErr != nil {
			if hasDelivery {
				cleanupErr = abandonCleanupFailure(ctx, store, delivery, leaseOwner, CleanupRecord{RunID: run.ID, Kind: item.resource.Kind, Name: item.resource.Name}, cleanupErr)
			}
			partial = append(partial, cleanupErr)
			continue
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
	return errors.Join(partial...)
}

func findAbandonResource(resources []OwnedResource, runID, kind, name string) (OwnedResource, bool) {
	for _, resource := range resources {
		if resource.RunID == runID && resource.Kind == kind && resource.Name == name {
			return resource, true
		}
	}
	return OwnedResource{}, false
}
