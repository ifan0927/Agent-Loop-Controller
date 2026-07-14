package application

import (
	"context"
	"errors"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

// ProductionRunReader is the narrow durable-state dependency required by the
// automatic driver. The driver never derives an action from a stale result;
// it re-reads this persisted state after every side effect.
type ProductionRunReader interface {
	GetRun(context.Context, string) (Run, error)
}

// ProductionDriverCoordinator is the set of state-bound actions that the
// automatic driver may invoke. ProductionCoordinator is its implementation;
// the interface keeps transport and adapter construction outside application
// control flow.
type ProductionDriverCoordinator interface {
	Continue(context.Context, ProductionContinueCommand) (ProductionResult, error)
	ReconcileGitHub(context.Context, ProductionReconcileCommand, GitHubReadPort) (ProductionResult, error)
	Push(context.Context, ProductionPushCommand, ApprovalValidator, BranchPublisher) (ProductionPushResult, error)
	OpenPullRequest(context.Context, ProductionOpenPullRequestCommand, ApprovalValidator, PullRequestOpener) (ProductionOpenPullRequestResult, error)
	MergePullRequest(context.Context, ProductionMergeCommand, ApprovalValidator, GitHubReadPort, SquashMerger) (ProductionMergeResult, error)
	ReconcileLinearCompletion(context.Context, ProductionLinearCompletionCommand) (ProductionLinearCompletionResult, error)
	Cleanup(context.Context, ProductionCleanupCommand, CleanupPort, SourceSyncPort) (ProductionCleanupResult, error)
}

var _ ProductionDriverCoordinator = (*ProductionCoordinator)(nil)

// ProductionDriverPorts holds only the bounded, action-specific ports needed
// once a run has reached delivery. No generic write capability is exposed.
type ProductionDriverPorts struct {
	GitHubReader      GitHubReadPort
	ApprovalValidator ApprovalValidator
	BranchPublisher   BranchPublisher
	PullRequestOpener PullRequestOpener
	SquashMerger      SquashMerger
	CleanupPort       CleanupPort
	SourceSyncPort    SourceSyncPort
}

// ProductionDriverPolicy bounds synchronous work between polls and prevents a
// zero-delay retry loop. A caller normally gives Drive a long-lived context;
// it keeps polling external pending states until that context is canceled or a
// durable human/terminal stop state is reached.
type ProductionDriverPolicy struct {
	PollInterval       time.Duration
	MaxImmediateAction int
}

func (p ProductionDriverPolicy) validate() error {
	if p.PollInterval <= 0 {
		return errors.New("production driver poll interval must be positive")
	}
	if p.MaxImmediateAction < 1 {
		return errors.New("production driver immediate action limit must be positive")
	}
	return nil
}

// ProductionWait is injected so tests and future schedulers control waiting
// without importing time-based adapters into the application layer.
type ProductionWait func(context.Context, time.Duration) error

// ProductionDriveCommand identifies one already-admitted persisted run. It
// intentionally has no Decision: awaiting_human_decision is a durable stop
// that must be resumed through the explicit decision path.
type ProductionDriveCommand struct {
	Requester      Requester
	RunID          string
	Repository     string
	IdempotencyKey string
}

// ProductionDriveResult reports why Drive stopped. Waiting GitHub/Linear
// states do not return a result: they remain inside the injected polling loop.
type ProductionDriveResult struct {
	Run        RunResult        `json:"run"`
	Action     ProductionAction `json:"action"`
	Reason     string           `json:"reason"`
	ActionsRun int              `json:"actions_run"`
}

// ProductionDriver continuously advances one run using the existing
// one-safe-action coordinator methods. It preserves every action's requester,
// expected state, and idempotency gate; it does not create a broad "run all"
// write port.
type ProductionDriver struct {
	coordinator ProductionDriverCoordinator
	runs        ProductionRunReader
	ports       ProductionDriverPorts
	policy      ProductionDriverPolicy
	wait        ProductionWait
}

func NewProductionDriver(coordinator ProductionDriverCoordinator, runs ProductionRunReader, ports ProductionDriverPorts, policy ProductionDriverPolicy, wait ProductionWait) (*ProductionDriver, error) {
	if coordinator == nil || runs == nil {
		return nil, errors.New("production driver coordinator and run reader are required")
	}
	if err := policy.validate(); err != nil {
		return nil, err
	}
	if wait == nil {
		wait = waitForProductionPoll
	}
	return &ProductionDriver{coordinator: coordinator, runs: runs, ports: ports, policy: policy, wait: wait}, nil
}

func waitForProductionPoll(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Drive returns only for durable terminal/manual/human-decision states or a
// non-retryable error. GitHub and Linear pending states are intentionally
// polled, so a trusted approval or external completion automatically resumes
// the delivery chain without an operator issuing another command.
func (d *ProductionDriver) Drive(ctx context.Context, command ProductionDriveCommand) (ProductionDriveResult, error) {
	if command.RunID == "" || command.Repository == "" || command.IdempotencyKey == "" {
		return ProductionDriveResult{}, serviceError(ErrorInvalidInput, "run, repository, and idempotency key are required", nil)
	}

	actions, immediate := 0, 0
	for {
		if err := ctx.Err(); err != nil {
			return ProductionDriveResult{}, err
		}
		run, err := d.runs.GetRun(ctx, command.RunID)
		if err != nil {
			return ProductionDriveResult{}, classifyServiceError(err)
		}
		if run.Repository != command.Repository || run.IdempotencyKey != command.IdempotencyKey {
			return ProductionDriveResult{}, serviceError(ErrorConflict, "run authority changed before automatic delivery", nil)
		}
		if err := authorizePersistedRequester(run, command.Requester); err != nil {
			return ProductionDriveResult{}, err
		}

		if run.State == domain.StateAwaitingHumanDecision {
			return d.stop(run, ProductionStop, "durable human decision is required", actions), nil
		}
		action, reason := productionNextAction(run.State)
		if action == ProductionStop {
			return d.stop(run, action, reason, actions), nil
		}
		if immediate >= d.policy.MaxImmediateAction {
			if err := d.wait(ctx, d.policy.PollInterval); err != nil {
				return ProductionDriveResult{}, err
			}
			immediate = 0
			continue
		}
		immediate++
		actions++

		poll, err := d.apply(ctx, command, run, action)
		if err != nil {
			if !retryableProductionDriverError(err) {
				if result, stopped := d.durableStop(ctx, command, actions); stopped {
					return result, nil
				}
				return ProductionDriveResult{}, err
			}
			poll = true
		}
		if !poll {
			continue
		}
		if err := d.wait(ctx, d.policy.PollInterval); err != nil {
			return ProductionDriveResult{}, err
		}
		// A wait is the polling boundary. Reset only the no-wait transition
		// guard; this permits a long-lived worker to wait for approval without
		// ever becoming a busy loop.
		immediate = 0
	}
}

// durableStop turns an action that persisted a manual/terminal transition into
// the driver's normal result rather than forcing the caller to issue a second
// status command merely to discover that the run has already stopped.
func (d *ProductionDriver) durableStop(ctx context.Context, command ProductionDriveCommand, actions int) (ProductionDriveResult, bool) {
	run, err := d.runs.GetRun(ctx, command.RunID)
	if err != nil || run.Repository != command.Repository || run.IdempotencyKey != command.IdempotencyKey {
		return ProductionDriveResult{}, false
	}
	if err := authorizePersistedRequester(run, command.Requester); err != nil {
		return ProductionDriveResult{}, false
	}
	if run.State == domain.StateAwaitingHumanDecision {
		return d.stop(run, ProductionStop, "durable human decision is required", actions), true
	}
	action, reason := productionNextAction(run.State)
	if action != ProductionStop {
		return ProductionDriveResult{}, false
	}
	return d.stop(run, action, reason, actions), true
}

func (d *ProductionDriver) stop(run Run, action ProductionAction, reason string, actions int) ProductionDriveResult {
	return ProductionDriveResult{Run: projectRunResult(run), Action: action, Reason: reason, ActionsRun: actions}
}

func (d *ProductionDriver) apply(ctx context.Context, command ProductionDriveCommand, run Run, action ProductionAction) (bool, error) {
	switch action {
	case ProductionContinueLocal:
		_, err := d.coordinator.Continue(ctx, ProductionContinueCommand{Requester: command.Requester, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey})
		return false, err
	case ProductionReconcileGitHub:
		if d.ports.GitHubReader == nil {
			return false, serviceError(ErrorInternal, "GitHub read port is required for automatic reconciliation", nil)
		}
		result, err := d.coordinator.ReconcileGitHub(ctx, ProductionReconcileCommand{Requester: command.Requester, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, d.ports.GitHubReader)
		return err == nil && result.Action == ProductionReconcileGitHub, err
	case ProductionPush:
		if d.ports.ApprovalValidator == nil || d.ports.BranchPublisher == nil {
			return false, serviceError(ErrorInternal, "approval validator and branch publisher are required for automatic push", nil)
		}
		_, err := d.coordinator.Push(ctx, ProductionPushCommand{Requester: command.Requester, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, d.ports.ApprovalValidator, d.ports.BranchPublisher)
		return false, err
	case ProductionOpenPullRequest:
		if d.ports.ApprovalValidator == nil || d.ports.PullRequestOpener == nil {
			return false, serviceError(ErrorInternal, "approval validator and pull request opener are required for automatic pull request creation", nil)
		}
		_, err := d.coordinator.OpenPullRequest(ctx, ProductionOpenPullRequestCommand{Requester: command.Requester, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, d.ports.ApprovalValidator, d.ports.PullRequestOpener)
		return false, err
	case ProductionMerge:
		if d.ports.ApprovalValidator == nil || d.ports.GitHubReader == nil || d.ports.SquashMerger == nil {
			return false, serviceError(ErrorInternal, "approval validator, GitHub reader, and squash merger are required for automatic merge", nil)
		}
		_, err := d.coordinator.MergePullRequest(ctx, ProductionMergeCommand{Requester: command.Requester, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, d.ports.ApprovalValidator, d.ports.GitHubReader, d.ports.SquashMerger)
		return false, err
	case ProductionReconcileLinear:
		result, err := d.coordinator.ReconcileLinearCompletion(ctx, ProductionLinearCompletionCommand{Requester: command.Requester, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey})
		return err == nil && result.Action == ProductionReconcileLinear, err
	case ProductionCleanup:
		if d.ports.CleanupPort == nil || d.ports.SourceSyncPort == nil {
			return false, serviceError(ErrorInternal, "cleanup and source synchronization ports are required for automatic cleanup", nil)
		}
		_, err := d.coordinator.Cleanup(ctx, ProductionCleanupCommand{Requester: command.Requester, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, d.ports.CleanupPort, d.ports.SourceSyncPort)
		return false, err
	default:
		return false, serviceError(ErrorInternal, "production action is unsupported by automatic driver", nil)
	}
}

func retryableProductionDriverError(err error) bool {
	var service *ServiceError
	return errors.As(err, &service) && service.Category == ErrorUnavailable
}
