package application

import (
	"context"
	"fmt"
	"strings"

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
	if err := c.store.Transition(ctx, run.ID, domain.StateManualIntervention, domain.StateApprovalReady, "operator authorized owned pull request fast-forward recovery", "recover_owned_push:"+fmt.Sprint(pr.Number), run.CandidateHead); err != nil {
		return ProductionResult{}, classifyServiceError(err)
	}
	next, err := c.store.GetRun(ctx, run.ID)
	if err != nil {
		return ProductionResult{}, classifyServiceError(err)
	}
	return ProductionResult{Action: ProductionPush, Run: projectRunResult(next), Reason: "owned pull request recovery authorized; push will revalidate before writing"}, nil
}
