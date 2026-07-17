package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type CIWaitRecoveryCommand struct {
	Requester Requester
	RunID     string
}

type CIWaitRecoveryApply struct {
	ActionID        string
	Phase           string
	ExpectedAttempt int
	AppliedAt       time.Time
	EvidenceDigest  string
	Observations    []GitHubRequestObservation
	Metadata        GitHubInstallationMetadata
	GitHubEvidence  domain.GitHubReadEvidence
}

type CIWaitRecoveryStore interface {
	OperatorActionStore
	OperatorAttentionPublisher
	ApplyCIWaitRecovery(context.Context, CIWaitRecoveryApply) (OperatorActionRecord, RetrySchedule, bool, error)
}

type CIWaitRecoveryService struct {
	store       CIWaitRecoveryStore
	revalidator CIWaitRecoveryLinearRevalidator
	actions     *OperatorActionService
	now         func() time.Time
}

type CIWaitLocalAuthorityPort interface {
	ValidateOwned(context.Context, WorktreeRecord) error
	Head(context.Context, string) (string, error)
	Branch(context.Context, string) (string, error)
}

type CIWaitRecoveryLinearRevalidator interface {
	RevalidateForCIWaitRecovery(context.Context, LinearRevalidateCommand) (Run, error)
}

func NewCIWaitRecoveryService(store CIWaitRecoveryStore, revalidator CIWaitRecoveryLinearRevalidator) (*CIWaitRecoveryService, error) {
	if store == nil || revalidator == nil {
		return nil, errors.New("CI wait recovery dependencies are required")
	}
	actions, err := NewOperatorActionService(store)
	if err != nil {
		return nil, err
	}
	return &CIWaitRecoveryService{store: store, revalidator: revalidator, actions: actions, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (s *CIWaitRecoveryService) Recover(ctx context.Context, command CIWaitRecoveryCommand, reader GitHubReadPort, local CIWaitLocalAuthorityPort) (OperatorRetryResult, error) {
	if command.RunID == "" || reader == nil || local == nil {
		return OperatorRetryResult{}, serviceError(ErrorInvalidInput, "CI wait recovery run, GitHub reader, and local authority are required", nil)
	}
	inspection, err := s.store.Inspect(ctx, command.RunID)
	if err != nil {
		return OperatorRetryResult{}, classifyServiceError(err)
	}
	run := inspection.Run
	if err := authorizePersistedRequester(run, command.Requester); err != nil {
		return OperatorRetryResult{}, err
	}
	schedule, found := retryScheduleForPhase(inspection.RetrySchedules, AutomaticRetryPhaseForRun(run))
	if !found || validateLegacyCIWaitRecovery(inspection, schedule) != nil {
		if replay, ok := latestSuccessfulCIWaitRecovery(inspection.OperatorActions); ok {
			if replay.Status == OperatorActionStatusApplied {
				replay, err = s.observeRecovery(ctx, replay)
				if err != nil {
					return OperatorRetryResult{}, err
				}
			}
			return OperatorRetryResult{Action: projectOperatorActionResult(replay), Retry: &schedule}, nil
		}
		return OperatorRetryResult{}, serviceError(ErrorConflict, "run has no recoverable legacy CI wait", nil)
	}
	if err := freshCIWaitLocalAuthority(ctx, run, inspection.Resources, local); err != nil {
		return OperatorRetryResult{}, serviceError(ErrorConflict, "CI wait recovery local authority changed", err)
	}
	revalidated, err := s.revalidator.RevalidateForCIWaitRecovery(ctx, LinearRevalidateCommand{Requester: command.Requester, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey})
	if err != nil || !sameOperatorRetryAuthority(run, revalidated) {
		if err == nil {
			err = errors.New("Linear authority changed")
		}
		return OperatorRetryResult{}, serviceError(ErrorConflict, "CI wait recovery Linear authority changed", err)
	}
	if err := validateReaderAuthority(inspection, reader.Authority()); err != nil {
		return OperatorRetryResult{}, err
	}
	evidence, _, observations, metadata, err := reader.Read(ctx, inspection.PullRequest.Number, run.CandidateHead)
	if err != nil {
		return OperatorRetryResult{}, serviceError(ErrorUnavailable, "fresh CI wait recovery GitHub read failed", err)
	}
	if err := validateReaderAuthority(inspection, metadata); err != nil {
		return OperatorRetryResult{}, err
	}
	var expected domain.RepositoryIdentity
	if inspection.GitHubInstallation != nil {
		expected = inspection.GitHubInstallation.Repository
	} else {
		expected = metadata.Repository
	}
	if err := ReconcileGitHubRead(expected, *inspection.PullRequest, run.WorkingBranch, run.BaseBranch, run.CandidateHead, run.BaseSHA, run.IdempotencyKey, inspection.PullRequest.BodyDigest, evidence); err != nil {
		return OperatorRetryResult{}, serviceError(ErrorConflict, "fresh CI wait recovery GitHub authority changed", err)
	}
	if !strings.EqualFold(evidence.PullRequest.State, "open") || evidence.PullRequest.Merged {
		return OperatorRetryResult{}, serviceError(ErrorConflict, "fresh CI wait recovery pull request is no longer open", nil)
	}
	switch evidence.RequiredChecksStatus() {
	case domain.ReconciliationPass, domain.ReconciliationActionable, domain.ReconciliationPending:
	default:
		return OperatorRetryResult{}, serviceError(ErrorConflict, "fresh CI wait recovery required-check evidence is incomplete", nil)
	}
	for _, observation := range observations {
		if observation.HTTPStatus < 200 || observation.HTTPStatus >= 300 || observation.ErrorClass != "" {
			return OperatorRetryResult{}, serviceError(ErrorConflict, "fresh CI wait recovery read is incomplete", nil)
		}
	}
	now := s.now()
	event, err := CIWaitRecoveryAttentionEvent(run, schedule)
	if err != nil {
		return OperatorRetryResult{}, serviceError(ErrorInternal, "CI wait recovery attention is invalid", err)
	}
	if _, err := s.store.AppendOperatorAttention(ctx, event); err != nil {
		return OperatorRetryResult{}, classifyServiceError(err)
	}
	action, _, err := s.actions.Prepare(ctx, OperatorActionInput{Requester: command.Requester, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, RunIdempotencyKey: run.IdempotencyKey, TransitionSequence: latestTransitionSequence(inspection.Timeline), ActionType: OperatorActionRecoverCIWait, ReasonCode: event.ReasonCode, AttentionEventKey: event.EventKey})
	if err != nil {
		return OperatorRetryResult{}, err
	}
	digest := ciWaitRecoveryEvidenceDigest(run, action, schedule, metadata, evidence, observations)
	if action.Status == OperatorActionStatusValidated {
		action, schedule, _, err = s.store.ApplyCIWaitRecovery(ctx, CIWaitRecoveryApply{ActionID: action.ActionID, Phase: schedule.Phase, ExpectedAttempt: schedule.AttemptCount, AppliedAt: now, EvidenceDigest: digest, Observations: observations, Metadata: metadata, GitHubEvidence: evidence})
		if err != nil {
			return OperatorRetryResult{}, classifyServiceError(err)
		}
	}
	if action.Status == OperatorActionStatusApplied {
		action, err = s.observeRecovery(ctx, action)
		if err != nil {
			return OperatorRetryResult{}, err
		}
	}
	return OperatorRetryResult{Action: projectOperatorActionResult(action), Retry: &schedule}, nil
}

func (s *CIWaitRecoveryService) observeRecovery(ctx context.Context, action OperatorActionRecord) (OperatorActionRecord, error) {
	observed, _, err := s.actions.RecordObserved(ctx, OperatorActionMutationResult{ActionID: action.ActionID, ExpectedStatus: OperatorActionStatusApplied, ResultStatus: OperatorActionResultSucceeded, ResultingState: action.ResultingState, ResultingTransitionSequence: action.ResultingTransitionSequence, EvidenceDigest: digestOperatorRetry("ci-wait-recovery-observed\x00" + action.ActionID + "\x00" + action.EvidenceDigest), At: action.AppliedAt.Add(time.Nanosecond)})
	return observed, err
}

func freshCIWaitLocalAuthority(ctx context.Context, run Run, resources []OwnedResource, local CIWaitLocalAuthorityPort) error {
	var repository LocalRepository
	if json.Unmarshal([]byte(run.RepositoryConfigJSON), &repository) != nil {
		return errors.New("persisted repository authority is invalid")
	}
	var worktree, branch *OwnedResource
	for index := range resources {
		resource := &resources[index]
		if resource.RunID != run.ID || resource.Status != "owned" {
			continue
		}
		switch resource.Kind {
		case "worktree":
			worktree = resource
		case "branch":
			branch = resource
		}
	}
	if worktree == nil || branch == nil {
		return errors.New("fresh local ownership is incomplete")
	}
	evidence, err := validateAbandonLocalResource(run, repository, *worktree)
	if err != nil {
		return err
	}
	if _, err := validateAbandonLocalResource(run, repository, *branch); err != nil || worktreeEvidenceNonce(*worktree) != worktreeEvidenceNonce(*branch) {
		return errors.New("fresh local ownership evidence is inconsistent")
	}
	record := WorktreeRecord{SourcePath: evidence.SourcePath, OriginPath: evidence.OriginPath, Path: evidence.Path, Branch: evidence.Branch, BaseBranch: evidence.BaseBranch, BaseSHA: evidence.BaseSHA, Nonce: evidence.Nonce}
	if err := local.ValidateOwned(ctx, record); err != nil {
		return err
	}
	branchName, err := local.Branch(ctx, run.WorktreePath)
	if err != nil || branchName != run.WorkingBranch {
		return errors.New("fresh local branch authority changed")
	}
	head, err := local.Head(ctx, run.WorktreePath)
	if err != nil || head != run.CandidateHead {
		return errors.New("fresh local candidate head changed")
	}
	return nil
}

func validateLegacyCIWaitRecovery(inspection RunInspection, schedule RetrySchedule) error {
	run := inspection.Run
	if (run.State != domain.StatePROpen && run.State != domain.StateReconcilingReviews) || inspection.PullRequest == nil || inspection.PullRequest.HeadSHA != run.CandidateHead || schedule.RunID != run.ID || schedule.Phase != AutomaticRetryPhaseForRun(run) || schedule.ControllerState != string(run.State) || schedule.Status != RetryScheduleAttention || schedule.FailureClass != RetryFailureTerminal || schedule.ReasonCode != RetryReasonTerminal || schedule.FailureEvidenceRef != "" {
		return errors.New("legacy terminal read schedule is not exact")
	}
	if err := validateLegacyCheckTopologyTrace(inspection); err != nil {
		return err
	}
	for _, side := range inspection.SideEffects {
		if side.RunID == run.ID && side.Status != "observed" {
			return errors.New("unresolved side effect prevents recovery")
		}
	}
	return validateOperatorRetryLocalOwnership(run, inspection.Resources)
}

func validateLegacyCheckTopologyTrace(inspection RunInspection) error {
	expected := []struct{ operation, category string }{
		{"mint_installation_token", "REST"}, {"repository", "REST"}, {"pull_request", "REST"}, {"required_checks", "REST"}, {"check_runs", "REST"}, {"commit_statuses", "REST"},
		{"ReadPullRequestReviews", "GraphQL"}, {"ReadPullRequestReviewThreads", "GraphQL"}, {"ReadPullRequestReviews", "GraphQL"}, {"ReadPullRequestReviewThreads", "GraphQL"},
		{"required_checks", "REST"}, {"check_runs", "REST"}, {"commit_statuses", "REST"},
	}
	observed := inspection.GitHubRequests
	if len(observed) != len(expected) {
		return errors.New("legacy check-topology trace length is not exact")
	}
	var authority GitHubInstallationMetadata
	if inspection.GitHubInstallation != nil {
		authority = *inspection.GitHubInstallation
	} else if inspection.RepositoryBinding != nil {
		parts := strings.Split(inspection.RepositoryBinding.CanonicalRepository, "/")
		if len(parts) != 2 {
			return errors.New("legacy GitHub authority is incomplete")
		}
		authority = GitHubInstallationMetadata{InstallationID: inspection.RepositoryBinding.GitHubInstallationID, Repository: domain.RepositoryIdentity{ID: inspection.RepositoryBinding.ExpectedRepositoryID, Owner: parts[0], Name: parts[1]}}
	} else {
		return errors.New("legacy GitHub authority is missing")
	}
	responseDigests := make([]string, 0, len(observed))
	for index, observation := range observed {
		if observation.HTTPStatus < 200 || observation.HTTPStatus >= 300 || observation.ErrorClass != "" || observation.InstallationID != authority.InstallationID || observation.Repository.ID != authority.Repository.ID || !strings.EqualFold(observation.Repository.Owner, authority.Repository.Owner) || !strings.EqualFold(observation.Repository.Name, authority.Repository.Name) {
			return errors.New("legacy check-topology trace authority is inconsistent")
		}
		if observation.Operation != expected[index].operation || observation.Category != expected[index].category {
			return errors.New("legacy check-topology trace order is not exact")
		}
		responseDigests = append(responseDigests, observation.ResponseDigest)
	}
	if strings.Join(responseDigests, ":") != legacyCIWaitIncidentResponseDigestAggregate {
		return errors.New("legacy check-topology incident fingerprint does not match")
	}
	return nil
}

// legacyCIWaitIncidentResponseDigestAggregate is the immutable sanitized
// response-digest sequence for the single observed incident authorized by
// issue 77. It is controller-owned, ordered by persisted observation ID, and
// cannot be supplied or widened by a recovery caller.
const legacyCIWaitIncidentResponseDigestAggregate = "f313d215450aa284616b2031540f8b7cf31e7b5f3de31e50b504a0674f6f118d:" +
	"93e138943fe3ece0156b98af27789349cf0d303afd9d3572b05a080f8140c28f:" +
	"fe948b5491d93557ebe2aecbb3514049cbc6e72dcb239c7c3c313b8662db829f:" +
	"8c16ceb806a5d40af04b9d82d8b55bd0c5931c564fc662f3ec1112f2afc92a5c:" +
	"e83cfac35507982a40cfeb776ce85aa0367e7646f81fe857df4a6c841ea8a170:" +
	"ddd23f311811419db33d89d933e439f916d798db42edfd4faa1b5025a9553baa:" +
	"d71fbbca7b00b96f6096445c507271d6be99e3e91962c0a6346ad3e5e0ed5f35:" +
	"cfa99e63e5d5e169ebb931b4e2c477ac1458503170fa058dd8f8f912a338af98:" +
	"d71fbbca7b00b96f6096445c507271d6be99e3e91962c0a6346ad3e5e0ed5f35:" +
	"cfa99e63e5d5e169ebb931b4e2c477ac1458503170fa058dd8f8f912a338af98:" +
	"8c16ceb806a5d40af04b9d82d8b55bd0c5931c564fc662f3ec1112f2afc92a5c:" +
	"765c5bb8dc07c6b456b63eea494709f8c505f735a6a2ea6043587aff74dbac6d:" +
	"ddd23f311811419db33d89d933e439f916d798db42edfd4faa1b5025a9553baa"

func latestSuccessfulCIWaitRecovery(actions []OperatorActionRecord) (OperatorActionRecord, bool) {
	for index := len(actions) - 1; index >= 0; index-- {
		if actions[index].ActionType == OperatorActionRecoverCIWait && (actions[index].Status == OperatorActionStatusApplied || actions[index].Status == OperatorActionStatusObserved) {
			return actions[index], true
		}
	}
	return OperatorActionRecord{}, false
}

func ciWaitRecoveryEvidenceDigest(run Run, action OperatorActionRecord, schedule RetrySchedule, metadata GitHubInstallationMetadata, evidence domain.GitHubReadEvidence, observations []GitHubRequestObservation) string {
	evidenceJSON, _ := json.Marshal(evidence)
	evidenceSum := sha256.Sum256(evidenceJSON)
	type observationDigestInput struct {
		Operation, Category, ErrorClass, ResponseDigest, ObservedAt string
		HTTPStatus                                                  int
		InstallationID                                              int64
		Repository                                                  domain.RepositoryIdentity
	}
	ordered := make([]observationDigestInput, len(observations))
	for index, observation := range observations {
		ordered[index] = observationDigestInput{Operation: observation.Operation, Category: observation.Category, HTTPStatus: observation.HTTPStatus, ErrorClass: observation.ErrorClass, ResponseDigest: observation.ResponseDigest, InstallationID: observation.InstallationID, Repository: observation.Repository, ObservedAt: observation.ObservedAt.UTC().Format(time.RFC3339Nano)}
	}
	payload := struct {
		Version int
		Action  struct {
			ID, Type, ExpectedState string
			TransitionSequence      int64
		}
		Schedule struct {
			RunID, Phase, ControllerState, FailureClass, ReasonCode, AttentionAt string
			AttemptCount                                                         int
		}
		Binding struct {
			Repository, ProfileDigest, WorkingBranch, BaseBranch, BaseSHA, CandidateHead string
			PRNumber                                                                     int64
		}
		Metadata struct {
			AppID, InstallationID         int64
			Repository                    domain.RepositoryIdentity
			PermissionsDigest, ObservedAt string
		}
		GitHubEvidenceDigest string
		Observations         []observationDigestInput
	}{Version: 1, GitHubEvidenceDigest: hex.EncodeToString(evidenceSum[:]), Observations: ordered}
	payload.Action.ID, payload.Action.Type, payload.Action.ExpectedState, payload.Action.TransitionSequence = action.ActionID, string(action.ActionType), string(action.ExpectedState), action.TransitionSequence
	payload.Schedule.RunID, payload.Schedule.Phase, payload.Schedule.ControllerState, payload.Schedule.AttemptCount = schedule.RunID, schedule.Phase, schedule.ControllerState, schedule.AttemptCount
	payload.Schedule.FailureClass, payload.Schedule.ReasonCode, payload.Schedule.AttentionAt = string(schedule.FailureClass), schedule.ReasonCode, schedule.AttentionAt.UTC().Format(time.RFC3339Nano)
	payload.Binding.Repository, payload.Binding.ProfileDigest, payload.Binding.WorkingBranch = run.Repository, run.ProfileDigest, run.WorkingBranch
	payload.Binding.BaseBranch, payload.Binding.BaseSHA, payload.Binding.CandidateHead, payload.Binding.PRNumber = run.BaseBranch, run.BaseSHA, run.CandidateHead, evidence.PullRequest.Number
	payload.Metadata.AppID, payload.Metadata.InstallationID, payload.Metadata.Repository = metadata.AppID, metadata.InstallationID, metadata.Repository
	payload.Metadata.PermissionsDigest, payload.Metadata.ObservedAt = metadata.PermissionsDigest, metadata.ObservedAt.UTC().Format(time.RFC3339Nano)
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
