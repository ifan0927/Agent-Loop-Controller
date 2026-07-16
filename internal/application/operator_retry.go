package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type OperatorRetryCommand struct {
	Requester Requester
	RunID     string
}

type OperatorRetryResult struct {
	Action         OperatorActionResult `json:"operator_action"`
	Retry          *RetrySchedule       `json:"retry_schedule,omitempty"`
	NextEligibleAt time.Time            `json:"next_eligible_at,omitempty"`
}

type OperatorRetryApply struct {
	ActionID        string
	Phase           string
	ExpectedAttempt int
	NextEligibleAt  time.Time
	AppliedAt       time.Time
	EvidenceDigest  string
}

type OperatorRetryStore interface {
	OperatorActionStore
	GetRetrySchedule(context.Context, string, string) (RetrySchedule, bool, error)
	ApplyOperatorRetry(context.Context, OperatorRetryApply) (OperatorActionRecord, RetrySchedule, bool, error)
}

type OperatorRetryRevalidator interface {
	RevalidateForOperatorRetry(context.Context, LinearRevalidateCommand) (Run, error)
}

type OperatorRetryService struct {
	store       OperatorRetryStore
	actions     *OperatorActionService
	revalidator OperatorRetryRevalidator
}

func NewOperatorRetryService(store OperatorRetryStore, revalidator OperatorRetryRevalidator) (*OperatorRetryService, error) {
	if store == nil || revalidator == nil {
		return nil, errors.New("operator retry dependencies are required")
	}
	actions, err := NewOperatorActionService(store)
	if err != nil {
		return nil, err
	}
	return &OperatorRetryService{store: store, actions: actions, revalidator: revalidator}, nil
}

func (s *OperatorRetryService) Retry(ctx context.Context, command OperatorRetryCommand) (OperatorRetryResult, error) {
	if command.RunID == "" {
		return OperatorRetryResult{}, serviceError(ErrorInvalidInput, "operator retry run is required", nil)
	}
	inspection, err := s.store.Inspect(ctx, command.RunID)
	if err != nil {
		return OperatorRetryResult{}, classifyServiceError(err)
	}
	run := inspection.Run
	if err := authorizePersistedRequester(run, command.Requester); err != nil {
		return OperatorRetryResult{}, err
	}
	phase := AutomaticRetryPhaseForRun(run)
	schedule, found := retryScheduleForPhase(inspection.RetrySchedules, phase)
	if !found || schedule.Status != RetryScheduleAttention {
		if replay, ok := latestSuccessfulOperatorRetry(inspection.OperatorActions); ok {
			replay, err = s.observeAppliedRetry(ctx, replay)
			if err != nil {
				return OperatorRetryResult{}, err
			}
			return projectOperatorRetryResult(replay, schedule, found), nil
		}
		return OperatorRetryResult{}, serviceError(ErrorConflict, "run has no supported parked retry", nil)
	}
	if err := validateOperatorRetryPlan(run, inspection, schedule); err != nil {
		return OperatorRetryResult{}, serviceError(ErrorConflict, "parked retry is not safely resumable", err)
	}
	revalidated, err := s.revalidator.RevalidateForOperatorRetry(ctx, LinearRevalidateCommand{Requester: command.Requester, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey})
	if err != nil {
		return OperatorRetryResult{}, err
	}
	if !sameOperatorRetryAuthority(run, revalidated) {
		return OperatorRetryResult{}, serviceError(ErrorConflict, "Linear revalidation returned different run authority", nil)
	}
	event, current, err := s.store.CurrentOperatorAttention(ctx, run.ID)
	if err != nil {
		return OperatorRetryResult{}, classifyServiceError(err)
	}
	if !current || event.EventType != OperatorAttentionRetry || event.EventKey == "" || event.ReasonCode != schedule.ReasonCode {
		return OperatorRetryResult{}, serviceError(ErrorConflict, "current parked retry attention changed", nil)
	}
	sequence := latestTransitionSequence(inspection.Timeline)
	action, _, err := s.actions.Prepare(ctx, OperatorActionInput{Requester: command.Requester, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, RunIdempotencyKey: run.IdempotencyKey, TransitionSequence: sequence, ActionType: OperatorActionRetry, ReasonCode: event.ReasonCode, AttentionEventKey: event.EventKey})
	if err != nil {
		return OperatorRetryResult{}, err
	}
	if action.Status == OperatorActionStatusObserved {
		return projectOperatorRetryResult(action, schedule, true), nil
	}
	nextEligible := action.ValidatedAt.UTC().Add(time.Nanosecond)
	evidence := operatorRetryEvidenceDigest(action, schedule, nextEligible)
	if action.Status == OperatorActionStatusValidated {
		action, schedule, _, err = s.store.ApplyOperatorRetry(ctx, OperatorRetryApply{ActionID: action.ActionID, Phase: schedule.Phase, ExpectedAttempt: schedule.AttemptCount, NextEligibleAt: nextEligible, AppliedAt: action.ValidatedAt, EvidenceDigest: evidence})
		if err != nil {
			return OperatorRetryResult{}, classifyServiceError(err)
		}
	}
	action, err = s.observeAppliedRetry(ctx, action)
	if err != nil {
		return OperatorRetryResult{}, err
	}
	return projectOperatorRetryResult(action, schedule, true), nil
}

func (s *OperatorRetryService) observeAppliedRetry(ctx context.Context, action OperatorActionRecord) (OperatorActionRecord, error) {
	if action.Status != OperatorActionStatusApplied {
		return action, nil
	}
	observed, _, err := s.actions.RecordObserved(ctx, OperatorActionMutationResult{ActionID: action.ActionID, ExpectedStatus: OperatorActionStatusApplied, ResultStatus: OperatorActionResultSucceeded, ResultingState: action.ResultingState, ResultingTransitionSequence: action.ResultingTransitionSequence, EvidenceDigest: operatorRetryOutcomeDigest(action), At: action.AppliedAt.Add(time.Nanosecond)})
	return observed, err
}

func sameOperatorRetryAuthority(before, after Run) bool {
	return before.ID == after.ID && before.Repository == after.Repository && before.State == after.State && before.IdempotencyKey == after.IdempotencyKey && before.IssueID == after.IssueID && before.SourceRevision == after.SourceRevision && before.RawIssueHash == after.RawIssueHash && before.TaskHash == after.TaskHash && before.ProfileDigest == after.ProfileDigest && before.RegistryDigest == after.RegistryDigest && before.RepositoryBindingDigest == after.RepositoryBindingDigest
}

func validateOperatorRetryPlan(run Run, inspection RunInspection, schedule RetrySchedule) error {
	if schedule.RunID != run.ID || schedule.Phase != AutomaticRetryPhaseForRun(run) || schedule.ControllerState != string(run.State) || schedule.Status != RetryScheduleAttention || schedule.ReasonCode != RetryReasonBudgetExhausted || (schedule.FailureClass != RetryFailureProcessStart && schedule.FailureClass != RetryFailureUnavailable) {
		return errors.New("retry schedule authority is unsupported")
	}
	switch run.State {
	case domain.StateReceived, domain.StateAdmitting, domain.StateProvisioning, domain.StateExecuting, domain.StateVerifying, domain.StateFreshReview, domain.StateRepairing, domain.StateApprovalReady:
	default:
		return errors.New("controller state is not pre-delivery operator-retryable")
	}
	for _, side := range inspection.SideEffects {
		if side.RunID == run.ID && side.Status != "observed" {
			return errors.New("unresolved external side effect prevents retry")
		}
	}
	switch schedule.FailureClass {
	case RetryFailureProcessStart:
		if !hasOperatorRetryProcessStartEvidence(inspection, schedule) {
			return errors.New("process-start retry lacks matching persisted process evidence")
		}
	case RetryFailureUnavailable:
		if run.State != domain.StateReceived && run.State != domain.StateAdmitting {
			return errors.New("unavailable retry is limited to the freshly revalidated admission boundary")
		}
	}
	return validateOperatorRetryLocalOwnership(run, inspection.Resources)
}

func hasOperatorRetryProcessStartEvidence(inspection RunInspection, schedule RetrySchedule) bool {
	kind, id, found := strings.Cut(schedule.FailureEvidenceRef, ":")
	if !found {
		return false
	}
	for _, attempt := range inspection.Attempts {
		if kind == "attempt" && strconv.FormatInt(attempt.ID, 10) == id && attempt.RunID == schedule.RunID && attempt.Status == "failed" && attempt.ErrorCategory == RetryReasonProcessStart && !attempt.FinishedAt.IsZero() && !attempt.FinishedAt.After(schedule.UpdatedAt) {
			return true
		}
	}
	for _, verification := range inspection.Verifications {
		if kind == "verification" && strconv.FormatInt(verification.ID, 10) == id && verification.RunID == schedule.RunID && verification.ProcessOutcome == VerificationOutcomeNotStarted && verification.FailureCategory == RetryReasonProcessStart && !verification.CreatedAt.IsZero() && !verification.CreatedAt.After(schedule.UpdatedAt) {
			return true
		}
	}
	return false
}

func validateOperatorRetryLocalOwnership(run Run, resources []OwnedResource) error {
	early := run.State == domain.StateReceived || run.State == domain.StateAdmitting || run.State == domain.StateProvisioning
	hasActiveLocal := false
	for _, resource := range resources {
		if resource.RunID == run.ID && resource.Status != "deleted" && (resource.Kind == "worktree" || resource.Kind == "branch") {
			hasActiveLocal = true
			break
		}
	}
	if !hasActiveLocal && early {
		return nil
	}
	if run.WorktreePath == "" {
		return errors.New("resumable local state lacks a worktree")
	}
	var repository LocalRepository
	if json.Unmarshal([]byte(run.RepositoryConfigJSON), &repository) != nil || repository.CanonicalRepository != run.Repository || repository.BaseBranch != run.BaseBranch {
		return errors.New("persisted repository authority is invalid")
	}
	want := map[string]string{"worktree": run.WorktreePath, "branch": run.WorkingBranch}
	found := map[string]OwnedResource{}
	for _, resource := range resources {
		name, relevant := want[resource.Kind]
		if !relevant || resource.RunID != run.ID || resource.Status == "deleted" {
			continue
		}
		if _, duplicate := found[resource.Kind]; duplicate || resource.Status != "owned" || resource.Name != name {
			return errors.New("local retry ownership is ambiguous")
		}
		if _, err := validateAbandonLocalResource(run, repository, resource); err != nil {
			return err
		}
		found[resource.Kind] = resource
	}
	worktree, hasWorktree := found["worktree"]
	branch, hasBranch := found["branch"]
	if !hasWorktree || !hasBranch {
		return errors.New("local retry ownership is incomplete")
	}
	if worktreeEvidenceNonce(worktree) != worktreeEvidenceNonce(branch) {
		return errors.New("local retry ownership nonces do not match")
	}
	return nil
}

func retryScheduleForPhase(schedules []RetrySchedule, phase string) (RetrySchedule, bool) {
	for _, schedule := range schedules {
		if schedule.Phase == phase {
			return schedule, true
		}
	}
	return RetrySchedule{}, false
}

func latestSuccessfulOperatorRetry(actions []OperatorActionRecord) (OperatorActionRecord, bool) {
	var latest OperatorActionRecord
	found := false
	for _, action := range actions {
		if action.ActionType == OperatorActionRetry && (action.Status == OperatorActionStatusApplied || action.Status == OperatorActionStatusObserved) && (!found || action.ReceivedAt.After(latest.ReceivedAt) || action.ReceivedAt.Equal(latest.ReceivedAt) && action.ActionID > latest.ActionID) {
			latest, found = action, true
		}
	}
	return latest, found
}

func operatorRetryEvidenceDigest(action OperatorActionRecord, schedule RetrySchedule, eligible time.Time) string {
	return digestOperatorRetry(fmt.Sprintf("apply\x00%s\x00%s\x00%d\x00%s\x00%s", action.ActionID, schedule.Phase, schedule.AttemptCount, schedule.FailureEvidenceRef, eligible.UTC().Format(time.RFC3339Nano)))
}

func operatorRetryOutcomeDigest(action OperatorActionRecord) string {
	return digestOperatorRetry("observed\x00" + action.ActionID + "\x00" + action.EvidenceDigest)
}

func digestOperatorRetry(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func projectOperatorRetryResult(action OperatorActionRecord, schedule RetrySchedule, found bool) OperatorRetryResult {
	projected := OperatorActionResult{ActionID: action.ActionID, ActionType: action.ActionType, Repository: action.Repository, ExpectedState: action.ExpectedState, TransitionSequence: action.TransitionSequence, RequesterLogin: sanitizeUntrustedContent(action.Requester.ID), RequesterDatabaseID: action.Requester.DatabaseID, RequesterNodeID: sanitizeUntrustedContent(action.Requester.NodeID), RequesterActorType: sanitizeUntrustedContent(action.Requester.ActorType), ReasonCode: action.ReasonCode, AttentionEventKey: action.AttentionEventKey, Status: action.Status, ResultStatus: action.ResultStatus, ResultingState: action.ResultingState, ResultingTransitionSequence: action.ResultingTransitionSequence, PayloadDigest: action.PayloadDigest, EvidenceDigest: action.EvidenceDigest, OutcomeDigest: action.OutcomeDigest, NextEligibleAt: action.NextEligibleAt, ReceivedAt: action.ReceivedAt, ValidatedAt: action.ValidatedAt, AppliedAt: action.AppliedAt, ObservedAt: action.ObservedAt}
	result := OperatorRetryResult{Action: projected, NextEligibleAt: action.NextEligibleAt}
	if found {
		copy := schedule
		result.Retry, result.NextEligibleAt = &copy, schedule.NextEligibleAt
	}
	return result
}
