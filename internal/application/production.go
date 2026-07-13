package application

import (
	"context"
	"errors"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

// ProductionAction identifies the one safe action that a manually triggered
// production controller invocation may take for the persisted run state.
type ProductionAction string

const (
	ProductionContinueLocal   ProductionAction = "continue_local"
	ProductionReconcileGitHub ProductionAction = "reconcile_github_read"
	ProductionPush            ProductionAction = "push_verified_branch"
	ProductionOpenPullRequest ProductionAction = "open_pull_request"
	ProductionStop            ProductionAction = "stop"
)

type ProductionContinueCommand struct {
	Requester      Requester
	RunID          string
	Repository     string
	ExpectedState  domain.State
	IdempotencyKey string
	Decision       *Decision
}

type ProductionReconcileCommand struct {
	Requester      Requester
	RunID          string
	Repository     string
	ExpectedState  domain.State
	IdempotencyKey string
}

type ProductionResult struct {
	Action ProductionAction `json:"action"`
	Run    RunResult        `json:"run"`
	Head   string           `json:"reconciled_head,omitempty"`
	Reason string           `json:"reason,omitempty"`
}

// ProductionCoordinator composes existing application services without adding
// transport or adapter details to the domain. It first revalidates the immutable
// Linear source, then derives one legal action from durable state.
type ProductionCoordinator struct {
	admission *LinearAdmissionService
	commands  CommandService
	store     RunStore
}

func NewProductionCoordinator(admission *LinearAdmissionService, controller LocalRunController, store RunStore) (*ProductionCoordinator, error) {
	if admission == nil || controller == nil || store == nil {
		return nil, errors.New("production coordinator dependencies are required")
	}
	return &ProductionCoordinator{admission: admission, commands: NewCommandService(controller, store), store: store}, nil
}

func (c *ProductionCoordinator) Continue(ctx context.Context, command ProductionContinueCommand) (ProductionResult, error) {
	run, err := c.admission.Revalidate(ctx, LinearRevalidateCommand{Requester: command.Requester, RunID: command.RunID, Repository: command.Repository, ExpectedState: command.ExpectedState, IdempotencyKey: command.IdempotencyKey})
	if err != nil {
		return ProductionResult{}, err
	}
	action, reason := productionNextAction(run.State)
	if action != ProductionContinueLocal {
		return ProductionResult{Action: action, Run: projectRunResult(run), Reason: reason}, nil
	}
	result, err := c.commands.Continue(ctx, ContinueCommand{Requester: command.Requester, RunID: command.RunID, Repository: command.Repository, ExpectedState: command.ExpectedState, IdempotencyKey: command.IdempotencyKey, Decision: command.Decision})
	if err != nil {
		return ProductionResult{}, err
	}
	next, reason := productionNextAction(result.Run.State)
	return ProductionResult{Action: next, Run: result.Run, Reason: reason}, nil
}

func (c *ProductionCoordinator) ReconcileGitHub(ctx context.Context, command ProductionReconcileCommand, reader GitHubReadPort) (ProductionResult, error) {
	run, err := c.admission.Revalidate(ctx, LinearRevalidateCommand{Requester: command.Requester, RunID: command.RunID, Repository: command.Repository, ExpectedState: command.ExpectedState, IdempotencyKey: command.IdempotencyKey})
	if err != nil {
		return ProductionResult{}, err
	}
	action, reason := productionNextAction(run.State)
	if action != ProductionReconcileGitHub {
		return ProductionResult{Action: action, Run: projectRunResult(run), Reason: reason}, nil
	}
	inspection, err := c.store.Inspect(ctx, run.ID)
	if err != nil {
		return ProductionResult{}, classifyServiceError(err)
	}
	if inspection.PullRequest == nil {
		return ProductionResult{}, serviceError(ErrorConflict, "persisted pull request identity is required", nil)
	}
	result, err := c.commands.ReconcileFromGitHub(ctx, GitHubReconcileCommand{Requester: command.Requester, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey, PullRequest: inspection.PullRequest.Number, ExpectedHead: run.CandidateHead}, reader)
	if err != nil {
		return ProductionResult{}, err
	}
	return ProductionResult{Action: ProductionReconcileGitHub, Run: projectRunResult(run), Head: result.Head}, nil
}

func productionNextAction(state domain.State) (ProductionAction, string) {
	switch state {
	case domain.StateReceived, domain.StateAdmitting, domain.StateProvisioning, domain.StateExecuting,
		domain.StateAwaitingHumanDecision, domain.StateVerifying, domain.StateFreshReview:
		return ProductionContinueLocal, "local controller evidence is resumable"
	case domain.StatePROpen, domain.StateReconcilingReviews, domain.StateAwaitingHumanApproval:
		return ProductionReconcileGitHub, "persisted pull request requires fresh GitHub read evidence"
	case domain.StateApprovalReady, domain.StatePushingBranch:
		return ProductionPush, "verified candidate may be reconciled with its owned working branch"
	case domain.StateBranchPushed, domain.StateOpeningPR:
		return ProductionOpenPullRequest, "pushed exact candidate may open its one owned pull request"
	case
		domain.StateRepairing, domain.StateMerging, domain.StateCleaning:
		return ProductionStop, "the next external write lifecycle is not implemented by this controller version"
	case domain.StateManualIntervention:
		return ProductionStop, "durable evidence requires a human decision"
	default:
		return ProductionStop, "run is terminal or unsupported for reconciliation"
	}
}
