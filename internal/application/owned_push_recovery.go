package application

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

// ProductionRecoverOwnedPushCommand is the explicit, operator-authorized
// recovery for a push that halted after a repair produced a new candidate for
// an existing controller-owned pull request.
type ProductionRecoverOwnedPushCommand struct {
	Requester      Requester
	RunID          string
	Repository     string
	ExpectedState  domain.State
	IdempotencyKey string
}

// RecoverOwnedPush restores only a halted owned-PR fast-forward to the
// already-verified push gate. It never invokes Git or GitHub writes. The next
// driver push revalidates exact-HEAD approval, reads the current remote, and
// uses its normal fast-forward lease before changing the branch.
func (c *ProductionCoordinator) RecoverOwnedPush(ctx context.Context, command ProductionRecoverOwnedPushCommand) (ProductionResult, error) {
	if command.ExpectedState != domain.StateManualIntervention {
		return ProductionResult{}, serviceError(ErrorInvalidInput, "owned push recovery requires manual_intervention", nil)
	}
	run, err := c.admission.RevalidateOwnedPushRecovery(ctx, LinearRevalidateCommand{
		Requester:      command.Requester,
		RunID:          command.RunID,
		Repository:     command.Repository,
		ExpectedState:  command.ExpectedState,
		IdempotencyKey: command.IdempotencyKey,
	})
	if err != nil {
		return ProductionResult{}, err
	}
	if run.State != domain.StateManualIntervention || strings.TrimSpace(run.CandidateHead) == "" {
		return ProductionResult{}, serviceError(ErrorConflict, "owned push recovery requires a halted verified candidate", nil)
	}
	inspection, err := c.store.Inspect(ctx, run.ID)
	if err != nil {
		return ProductionResult{}, classifyServiceError(err)
	}
	pr, err := retainedOwnedPullRequest(inspection, run)
	if err != nil || pr == nil || strings.TrimSpace(pr.HeadSHA) == "" {
		return ProductionResult{}, serviceError(ErrorConflict, "owned push recovery requires an existing open controller-owned pull request", err)
	}
	action, actions, err := c.prepareLegalRecoveryAction(ctx, command.Requester, run, inspection, OperatorActionRecoverOwnedPush)
	if err != nil {
		return ProductionResult{}, err
	}
	if err := c.store.Transition(ctx, run.ID, domain.StateManualIntervention, domain.StateApprovalReady, "operator authorized owned pull request fast-forward recovery", "recover_owned_push:"+fmt.Sprint(pr.Number), run.CandidateHead); err != nil {
		return ProductionResult{}, classifyServiceError(err)
	}
	next, err := c.store.GetRun(ctx, run.ID)
	if err != nil {
		return ProductionResult{}, classifyServiceError(err)
	}
	if action != nil {
		if err := settleLegalRecoveryAction(ctx, actions, *action, next, latestTransitionSequenceForRun(ctx, c.store, run.ID), "recover-owned-push"); err != nil {
			return ProductionResult{}, err
		}
	}
	return ProductionResult{Action: ProductionPush, Run: projectRunResult(next), Reason: "owned pull request recovery authorized; push will revalidate before writing"}, nil
}

func (c *ProductionCoordinator) prepareLegalRecoveryAction(ctx context.Context, requester Requester, run Run, inspection RunInspection, actionType OperatorActionType) (*OperatorActionRecord, *OperatorActionService, error) {
	store, ok := c.store.(OperatorActionStore)
	if !ok {
		// Compatibility-only in-memory stores do not persist production state.
		return nil, nil, nil
	}
	event, found, err := store.CurrentOperatorAttention(ctx, run.ID)
	if err != nil {
		return nil, nil, classifyServiceError(err)
	}
	if !found || !slices.Contains(legalActionIDsForInspection(run, inspection, event), OperatorAttentionActionID(actionType)) {
		return nil, nil, serviceError(ErrorConflict, "recovery is not a current legal action", nil)
	}
	actions, err := NewOperatorActionService(store)
	if err != nil {
		return nil, nil, serviceError(ErrorInternal, "recovery operation receipt is unavailable", err)
	}
	action, _, err := actions.Prepare(ctx, OperatorActionInput{Requester: requester, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, RunIdempotencyKey: run.IdempotencyKey, TransitionSequence: latestTransitionSequence(inspection.Timeline), ActionType: actionType, ReasonCode: event.ReasonCode, AttentionEventKey: event.EventKey})
	if err != nil {
		return nil, nil, err
	}
	return &action, actions, nil
}

func settleLegalRecoveryAction(ctx context.Context, actions *OperatorActionService, action OperatorActionRecord, run Run, sequence int64, label string) error {
	if actions == nil || sequence < 1 {
		return serviceError(ErrorInternal, "recovery receipt settlement evidence is unavailable", nil)
	}
	if action.Status == OperatorActionStatusValidated {
		evidence := digestText(label + "-applied-v1\x00" + action.ActionID + "\x00" + string(run.State) + "\x00" + fmt.Sprint(sequence))
		var err error
		action, _, err = actions.RecordApplied(ctx, OperatorActionMutationResult{ActionID: action.ActionID, ExpectedStatus: OperatorActionStatusValidated, ResultStatus: OperatorActionResultApplied, ResultingState: run.State, ResultingTransitionSequence: sequence, EvidenceDigest: evidence, At: time.Now().UTC()})
		if err != nil {
			return err
		}
	}
	if action.Status == OperatorActionStatusApplied {
		outcome := digestText(label + "-observed-v1\x00" + action.ActionID + "\x00" + action.EvidenceDigest)
		_, _, err := actions.RecordObserved(ctx, OperatorActionMutationResult{ActionID: action.ActionID, ExpectedStatus: OperatorActionStatusApplied, ResultStatus: OperatorActionResultSucceeded, ResultingState: action.ResultingState, ResultingTransitionSequence: action.ResultingTransitionSequence, EvidenceDigest: outcome, At: time.Now().UTC()})
		return err
	}
	return nil
}

func latestTransitionSequenceForRun(ctx context.Context, store RunStore, runID string) int64 {
	inspection, err := store.Inspect(ctx, runID)
	if err != nil {
		return 0
	}
	return latestTransitionSequence(inspection.Timeline)
}
