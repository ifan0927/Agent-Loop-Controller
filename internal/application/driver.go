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

// ProductionAttentionEvidenceReader supplies the transition timeline needed
// to bind a manual stop event to exact durable evidence.
type ProductionAttentionEvidenceReader interface {
	Inspect(context.Context, string) (RunInspection, error)
}

// ProductionDriverCoordinator is the set of state-bound actions that the
// automatic driver may invoke. ProductionCoordinator is its implementation;
// the interface keeps transport and adapter construction outside application
// control flow.
type ProductionDriverCoordinator interface {
	Continue(context.Context, ProductionContinueCommand) (ProductionResult, error)
	ReconcileGitHub(context.Context, ProductionReconcileCommand, GitHubReadPort) (ProductionResult, error)
	ReplyReviewFeedback(context.Context, ProductionReplyCommand, ApprovalValidator, GitHubReadPort, ReviewCommentReplyPort) (ProductionReplyResult, error)
	Push(context.Context, ProductionPushCommand, ApprovalValidator, BranchPublisher) (ProductionPushResult, error)
	OpenPullRequest(context.Context, ProductionOpenPullRequestCommand, ApprovalValidator, PullRequestOpener) (ProductionOpenPullRequestResult, error)
	MergePullRequest(context.Context, ProductionMergeCommand, ApprovalValidator, GitHubReadPort, SquashMerger) (ProductionMergeResult, error)
	ReconcileLinearCompletion(context.Context, ProductionLinearCompletionCommand) (ProductionLinearCompletionResult, error)
	Cleanup(context.Context, ProductionCleanupCommand, CleanupPort, SourceSyncPort) (ProductionCleanupResult, error)
}

type ProductionHeavyWorkScheduler interface {
	AcquireHeavyPermit(context.Context, string, string, time.Time) (HeavyPermit, bool, error)
	ReleaseHeavyPermit(context.Context, HeavyPermit, string, time.Time) (bool, error)
}

var _ ProductionDriverCoordinator = (*ProductionCoordinator)(nil)

// ProductionDriverPorts holds only the bounded, action-specific ports needed
// once a run has reached delivery. No generic write capability is exposed.
type ProductionDriverPorts struct {
	GitHubReader       GitHubReadPort
	ReviewCommentReply ReviewCommentReplyPort
	ApprovalValidator  ApprovalValidator
	BranchPublisher    BranchPublisher
	PullRequestOpener  PullRequestOpener
	SquashMerger       SquashMerger
	CleanupPort        CleanupPort
	SourceSyncPort     SourceSyncPort
	HeavyWorkScheduler ProductionHeavyWorkScheduler
}

// ProductionDriverPolicy bounds synchronous work between polls and prevents a
// zero-delay retry loop. A caller normally gives Drive a long-lived context;
// it keeps polling external pending states until that context is canceled or a
// durable human/terminal stop state is reached.
type ProductionDriverPolicy struct {
	PollInterval         time.Duration
	MaxImmediateAction   int
	CISlowThreshold      time.Duration
	ReturnOnExternalWait bool
	HeavyPermitOwner     string
}

func (p ProductionDriverPolicy) validate() error {
	if p.PollInterval <= 0 {
		return errors.New("production driver poll interval must be positive")
	}
	if p.MaxImmediateAction < 1 {
		return errors.New("production driver immediate action limit must be positive")
	}
	if p.CISlowThreshold != 0 && (p.CISlowThreshold < time.Minute || p.CISlowThreshold > 24*time.Hour) {
		return errors.New("production driver CI slow threshold must be between 1m and 24h")
	}
	return nil
}

type CIWaitEvidenceStore interface {
	ObserveCIWait(context.Context, string, int64, string, string, time.Duration, time.Time, time.Time) (CIWaitEvidence, error)
	CloseCIWaits(context.Context, string, time.Time) error
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
	attention   ProductionAttentionEvidenceReader
	publisher   OperatorAttentionPublisher
	ports       ProductionDriverPorts
	policy      ProductionDriverPolicy
	wait        ProductionWait
	now         func() time.Time
}

func NewProductionDriver(coordinator ProductionDriverCoordinator, runs ProductionRunReader, attention ProductionAttentionEvidenceReader, publisher OperatorAttentionPublisher, ports ProductionDriverPorts, policy ProductionDriverPolicy, wait ProductionWait) (*ProductionDriver, error) {
	if coordinator == nil || runs == nil || attention == nil || publisher == nil {
		return nil, errors.New("production driver coordinator, run reader, attention evidence reader, and publisher are required")
	}
	if err := policy.validate(); err != nil {
		return nil, err
	}
	if wait == nil {
		wait = waitForProductionPoll
	}
	if policy.CISlowThreshold == 0 {
		policy.CISlowThreshold = 20 * time.Minute
	}
	return &ProductionDriver{coordinator: coordinator, runs: runs, attention: attention, publisher: publisher, ports: ports, policy: policy, wait: wait, now: func() time.Time { return time.Now().UTC() }}, nil
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
	var permit HeavyPermit
	releasePermit := func(reason string) {
		if permit.RunID == "" || d.ports.HeavyWorkScheduler == nil {
			return
		}
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if released, _ := d.ports.HeavyWorkScheduler.ReleaseHeavyPermit(releaseCtx, permit, reason, d.now()); released {
			permit = HeavyPermit{}
		}
	}
	defer releasePermit("shutdown_after_process_exit")
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
		if HeavyWorkRequired(run.State) && d.ports.HeavyWorkScheduler != nil {
			if d.policy.HeavyPermitOwner == "" {
				return ProductionDriveResult{}, serviceError(ErrorInternal, "heavy-work permit owner is required", nil)
			}
			if permit.RunID == "" {
				acquired, held, acquireErr := d.ports.HeavyWorkScheduler.AcquireHeavyPermit(ctx, run.ID, d.policy.HeavyPermitOwner, d.now())
				if errors.Is(acquireErr, ErrHeavyPermitProcessReconciliationRequired) {
					reconciler, ok := d.coordinator.(InterruptedRunReconciler)
					if !ok {
						return ProductionDriveResult{}, serviceError(ErrorInternal, "interrupted heavy work cannot be reconciled", nil)
					}
					if reconcileErr := reconciler.ReconcileInterruptedRun(ctx, run.ID); reconcileErr != nil {
						return ProductionDriveResult{}, classifyServiceError(reconcileErr)
					}
					acquired, held, acquireErr = d.ports.HeavyWorkScheduler.AcquireHeavyPermit(ctx, run.ID, d.policy.HeavyPermitOwner, d.now())
				}
				if acquireErr != nil {
					return ProductionDriveResult{}, classifyServiceError(acquireErr)
				}
				if !held {
					if waitErr := d.wait(ctx, d.policy.PollInterval); waitErr != nil {
						return ProductionDriveResult{}, waitErr
					}
					continue
				}
				permit = acquired
			}
		} else if permit.RunID != "" {
			releasePermit(productionPermitReleaseReason(run.State))
		}
		// Reconcile wait evidence before every dispatch decision, including a
		// durable stop. This closes a residual wait after a crash that happened
		// between a state transition and the prior loop's post-action close.
		if waitErr := d.reconcileCIWait(ctx, command.RunID); waitErr != nil {
			return ProductionDriveResult{}, classifyServiceError(waitErr)
		}

		if run.State == domain.StateAwaitingHumanDecision {
			return d.stop(ctx, run, ProductionStop, "durable human decision is required", actions)
		}
		action, reason := productionNextAction(run.State)
		if action == ProductionStop {
			return d.stop(ctx, run, action, reason, actions)
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
		actionCtx := withHeavyPermitOwner(ctx, d.policy.HeavyPermitOwner)
		poll, err := d.apply(actionCtx, command, run, action)
		if err != nil {
			if !retryableProductionDriverError(err) {
				if action == ProductionReconcileGitHub {
					if closeErr := d.closeCIWait(ctx, command.RunID); closeErr != nil {
						return ProductionDriveResult{}, classifyServiceError(closeErr)
					}
				}
				if result, stopped, stopErr := d.durableStop(ctx, command, actions); stopped {
					return result, stopErr
				}
				return ProductionDriveResult{}, err
			}
			if d.policy.ReturnOnExternalWait {
				return ProductionDriveResult{}, err
			}
			poll = true
		}
		if err == nil && action == ProductionReconcileGitHub {
			if waitErr := d.reconcileCIWait(ctx, command.RunID); waitErr != nil {
				return ProductionDriveResult{}, classifyServiceError(waitErr)
			}
		}
		if !poll {
			continue
		}
		if d.policy.ReturnOnExternalWait {
			waiting, readErr := d.runs.GetRun(ctx, command.RunID)
			if readErr != nil {
				return ProductionDriveResult{}, classifyServiceError(readErr)
			}
			if !HeavyWorkRequired(waiting.State) {
				releasePermit(productionPermitReleaseReason(waiting.State))
			}
			next, nextReason := productionNextAction(waiting.State)
			return ProductionDriveResult{Run: projectRunResult(waiting), Action: next, Reason: nextReason, ActionsRun: actions}, nil
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

func productionPermitReleaseReason(state domain.State) string {
	if TerminalRunState(state) {
		return "terminal"
	}
	if state == domain.StateManualIntervention {
		return "manual_intervention"
	}
	if state == domain.StateAwaitingHumanDecision {
		return "human_wait"
	}
	return "external_wait"
}

func (d *ProductionDriver) reconcileCIWait(ctx context.Context, runID string) error {
	store, ok := d.publisher.(CIWaitEvidenceStore)
	if !ok {
		return nil
	}
	inspection, err := d.attention.Inspect(ctx, runID)
	if err != nil {
		return err
	}
	run := inspection.Run
	if run.State != domain.StateReconcilingReviews || inspection.PullRequest == nil || inspection.GitHubEvidence == nil || inspection.GitHubEvidence.PullRequest.HeadSHA != run.CandidateHead || !inspection.GitHubEvidence.RequiredChecksWaiting() {
		return store.CloseCIWaits(ctx, run.ID, d.now())
	}
	// The persisted GitHub observation is the deterministic crash/restart
	// anchor. A crash after evidence persistence but before this upsert cannot
	// reset the wait to a later wall-clock time.
	wait, err := store.ObserveCIWait(ctx, run.ID, inspection.PullRequest.Number, run.CandidateHead, run.ProfileDigest, d.policy.CISlowThreshold, inspection.GitHubEvidence.ObservedAt, d.now())
	if err != nil || wait.WarningAt.IsZero() {
		return err
	}
	event, err := CISlowAttentionEvent(run, wait)
	if err != nil {
		return err
	}
	_, err = d.publisher.AppendOperatorAttention(ctx, event)
	return err
}

func (d *ProductionDriver) closeCIWait(ctx context.Context, runID string) error {
	store, ok := d.publisher.(CIWaitEvidenceStore)
	if !ok {
		return nil
	}
	return store.CloseCIWaits(ctx, runID, d.now())
}

// durableStop turns an action that persisted a manual/terminal transition into
// the driver's normal result rather than forcing the caller to issue a second
// status command merely to discover that the run has already stopped.
func (d *ProductionDriver) durableStop(ctx context.Context, command ProductionDriveCommand, actions int) (ProductionDriveResult, bool, error) {
	run, err := d.runs.GetRun(ctx, command.RunID)
	if err != nil || run.Repository != command.Repository || run.IdempotencyKey != command.IdempotencyKey {
		return ProductionDriveResult{}, false, nil
	}
	if err := authorizePersistedRequester(run, command.Requester); err != nil {
		return ProductionDriveResult{}, false, nil
	}
	if run.State == domain.StateAwaitingHumanDecision {
		result, stopErr := d.stop(ctx, run, ProductionStop, "durable human decision is required", actions)
		return result, true, stopErr
	}
	action, reason := productionNextAction(run.State)
	if action != ProductionStop {
		return ProductionDriveResult{}, false, nil
	}
	result, stopErr := d.stop(ctx, run, action, reason, actions)
	return result, true, stopErr
}

func (d *ProductionDriver) stop(ctx context.Context, run Run, action ProductionAction, reason string, actions int) (ProductionDriveResult, error) {
	result := ProductionDriveResult{Run: projectRunResult(run), Action: action, Reason: reason, ActionsRun: actions}
	if run.State != domain.StateManualIntervention && run.State != domain.StateAwaitingHumanDecision {
		return result, nil
	}
	inspection, err := d.attention.Inspect(ctx, run.ID)
	if err != nil {
		return ProductionDriveResult{}, classifyServiceError(err)
	}
	if run.State == domain.StateAwaitingHumanDecision {
		err = publishHumanDecisionAttention(ctx, run, inspection, d.publisher)
	} else {
		err = publishManualInterventionAttention(ctx, run, inspection, d.publisher)
	}
	if err != nil {
		return ProductionDriveResult{}, classifyServiceError(err)
	}
	return result, nil
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
	case ProductionReplyReviewFeedback:
		if d.ports.ApprovalValidator == nil || d.ports.GitHubReader == nil || d.ports.ReviewCommentReply == nil {
			return false, serviceError(ErrorInternal, "approval validator, GitHub reader, and review reply port are required for automatic review replies", nil)
		}
		_, err := d.coordinator.ReplyReviewFeedback(ctx, ProductionReplyCommand{Requester: command.Requester, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, d.ports.ApprovalValidator, d.ports.GitHubReader, d.ports.ReviewCommentReply)
		return false, err
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
