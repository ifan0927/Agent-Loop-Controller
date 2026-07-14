package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type driverRunReader struct{ run Run }

func (r *driverRunReader) GetRun(context.Context, string) (Run, error) { return r.run, nil }

type driverCoordinator struct {
	runs            *driverRunReader
	calls           []ProductionAction
	apply           func(ProductionAction) error
	branchPublisher BranchPublisher
	pullRequestOpen PullRequestOpener
	sourceSync      SourceSyncPort
}

func (c *driverCoordinator) record(action ProductionAction) error {
	c.calls = append(c.calls, action)
	if c.apply != nil {
		return c.apply(action)
	}
	return nil
}

func (c *driverCoordinator) Continue(context.Context, ProductionContinueCommand) (ProductionResult, error) {
	return ProductionResult{}, c.record(ProductionContinueLocal)
}

func (c *driverCoordinator) ReconcileGitHub(context.Context, ProductionReconcileCommand, GitHubReadPort) (ProductionResult, error) {
	err := c.record(ProductionReconcileGitHub)
	action, _ := productionNextAction(c.runs.run.State)
	return ProductionResult{Action: action, Run: projectRunResult(c.runs.run)}, err
}

func (c *driverCoordinator) Push(_ context.Context, _ ProductionPushCommand, _ ApprovalValidator, publisher BranchPublisher) (ProductionPushResult, error) {
	c.branchPublisher = publisher
	return ProductionPushResult{}, c.record(ProductionPush)
}

func (c *driverCoordinator) OpenPullRequest(_ context.Context, _ ProductionOpenPullRequestCommand, _ ApprovalValidator, opener PullRequestOpener) (ProductionOpenPullRequestResult, error) {
	c.pullRequestOpen = opener
	return ProductionOpenPullRequestResult{}, c.record(ProductionOpenPullRequest)
}

func (c *driverCoordinator) MergePullRequest(context.Context, ProductionMergeCommand, ApprovalValidator, GitHubReadPort, SquashMerger) (ProductionMergeResult, error) {
	return ProductionMergeResult{}, c.record(ProductionMerge)
}

func (c *driverCoordinator) ReconcileLinearCompletion(context.Context, ProductionLinearCompletionCommand) (ProductionLinearCompletionResult, error) {
	err := c.record(ProductionReconcileLinear)
	action, _ := productionNextAction(c.runs.run.State)
	return ProductionLinearCompletionResult{Action: action, Run: projectRunResult(c.runs.run)}, err
}

func (c *driverCoordinator) Cleanup(_ context.Context, _ ProductionCleanupCommand, _ CleanupPort, sourceSync SourceSyncPort) (ProductionCleanupResult, error) {
	c.sourceSync = sourceSync
	err := c.record(ProductionCleanup)
	action, _ := productionNextAction(c.runs.run.State)
	return ProductionCleanupResult{Action: action, Run: projectRunResult(c.runs.run)}, err
}

type driverGitHubReader struct{}

func (driverGitHubReader) Authority() GitHubInstallationMetadata { return GitHubInstallationMetadata{} }
func (driverGitHubReader) Read(context.Context, int64, string) (domain.GitHubReadEvidence, domain.InlineReviewBodyHandoff, []GitHubRequestObservation, GitHubInstallationMetadata, error) {
	return domain.GitHubReadEvidence{}, domain.InlineReviewBodyHandoff{}, nil, GitHubInstallationMetadata{}, nil
}

type driverApprovalValidator struct{}

func (driverApprovalValidator) ValidateApprovalReady(context.Context, string) error { return nil }

type driverBranchPublisher struct{}

func (*driverBranchPublisher) RemoteSHA(context.Context, string, string) (string, error) {
	return "", nil
}
func (*driverBranchPublisher) Push(context.Context, string, string, string, string, string) (PushEvidence, error) {
	return PushEvidence{}, nil
}

type driverPullRequestOpener struct{}

func (*driverPullRequestOpener) OpenPullRequest(context.Context, PullRequestOpenRequest) (domain.PullRequest, error) {
	return domain.PullRequest{}, nil
}

type driverMerger struct{}

func (driverMerger) SquashMerge(context.Context, SquashMergeRequest) (domain.PullRequest, []GitHubRequestObservation, GitHubInstallationMetadata, error) {
	return domain.PullRequest{}, nil, GitHubInstallationMetadata{}, nil
}

type driverCleanupPort struct{}

func (driverCleanupPort) RemoveWorktree(context.Context, string, string, string, string) error {
	return nil
}
func (driverCleanupPort) DeleteLocalBranch(context.Context, string, string, string) error { return nil }
func (driverCleanupPort) DeleteRemoteBranch(context.Context, string, string, string) error {
	return nil
}

type driverSourceSyncPort struct{}

func (driverSourceSyncPort) Sync(context.Context, SourceSyncRequest) (SourceSyncResult, error) {
	return SourceSyncResult{}, nil
}

func driverRun(state domain.State) Run {
	return authorizeTestRun(Run{ID: "run", Repository: "owner/repo", IdempotencyKey: "key", State: state})
}

func newDriverForTest(t *testing.T, reader *driverRunReader, coordinator *driverCoordinator, wait ProductionWait) *ProductionDriver {
	t.Helper()
	driver, err := NewProductionDriver(coordinator, reader, ProductionDriverPorts{
		GitHubReader:      driverGitHubReader{},
		ApprovalValidator: driverApprovalValidator{},
		SquashMerger:      driverMerger{},
		CleanupPort:       driverCleanupPort{},
		SourceSyncPort:    driverSourceSyncPort{},
	}, ProductionDriverPolicy{PollInterval: time.Second, MaxImmediateAction: 8}, wait)
	if err != nil {
		t.Fatal(err)
	}
	return driver
}

func driverCommand() ProductionDriveCommand {
	return ProductionDriveCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: "run", Repository: "owner/repo", IdempotencyKey: "key"}
}

func TestProductionDriverStopsAtHumanDecisionWithoutInvokingAction(t *testing.T) {
	reader := &driverRunReader{run: driverRun(domain.StateAwaitingHumanDecision)}
	coordinator := &driverCoordinator{runs: reader}
	driver := newDriverForTest(t, reader, coordinator, func(context.Context, time.Duration) error {
		t.Fatal("driver must not poll while awaiting a decision")
		return nil
	})

	result, err := driver.Drive(context.Background(), driverCommand())
	if err != nil || result.Action != ProductionStop || result.Run.State != domain.StateAwaitingHumanDecision || len(coordinator.calls) != 0 {
		t.Fatalf("result=%+v calls=%v err=%v", result, coordinator.calls, err)
	}
}

func TestProductionDriverPollsForApprovalThenContinuesThroughCleanup(t *testing.T) {
	reader := &driverRunReader{run: driverRun(domain.StatePROpen)}
	coordinator := &driverCoordinator{runs: reader}
	githubReads := 0
	coordinator.apply = func(action ProductionAction) error {
		switch action {
		case ProductionReconcileGitHub:
			githubReads++
			switch githubReads {
			case 1:
				reader.run.State = domain.StateReconcilingReviews
			case 2:
				reader.run.State = domain.StateAwaitingHumanApproval
			case 3:
				reader.run.State = domain.StateMerging
			default:
				t.Fatalf("unexpected GitHub reconciliation %d", githubReads)
			}
		case ProductionMerge:
			reader.run.State = domain.StateAwaitingLinearCompletion
		case ProductionReconcileLinear:
			reader.run.State = domain.StateCleaning
		case ProductionCleanup:
			reader.run.State = domain.StateCompleted
		default:
			t.Fatalf("unexpected action %s", action)
		}
		return nil
	}
	waits := 0
	driver := newDriverForTest(t, reader, coordinator, func(context.Context, time.Duration) error {
		waits++
		return nil
	})

	result, err := driver.Drive(context.Background(), driverCommand())
	want := []ProductionAction{ProductionReconcileGitHub, ProductionReconcileGitHub, ProductionReconcileGitHub, ProductionMerge, ProductionReconcileLinear, ProductionCleanup}
	if err != nil || result.Run.State != domain.StateCompleted || result.Action != ProductionStop || waits != 2 || len(coordinator.calls) != len(want) {
		t.Fatalf("result=%+v calls=%v waits=%d err=%v", result, coordinator.calls, waits, err)
	}
	for index, action := range want {
		if coordinator.calls[index] != action {
			t.Fatalf("call %d=%s want=%s", index, coordinator.calls[index], action)
		}
	}
	if coordinator.sourceSync == nil {
		t.Fatal("driver did not compose the source synchronization port")
	}
}

func TestProductionDriverDispatchesPushAndPullRequestPortsInOrder(t *testing.T) {
	reader := &driverRunReader{run: driverRun(domain.StateApprovalReady)}
	coordinator := &driverCoordinator{runs: reader}
	coordinator.apply = func(action ProductionAction) error {
		switch action {
		case ProductionPush:
			reader.run.State = domain.StateBranchPushed
		case ProductionOpenPullRequest:
			reader.run.State = domain.StatePROpen
		case ProductionReconcileGitHub:
			reader.run.State = domain.StateManualIntervention
		default:
			t.Fatalf("unexpected action %s", action)
		}
		return nil
	}
	publisher := &driverBranchPublisher{}
	opener := &driverPullRequestOpener{}
	driver, err := NewProductionDriver(coordinator, reader, ProductionDriverPorts{
		GitHubReader:      driverGitHubReader{},
		ApprovalValidator: driverApprovalValidator{},
		BranchPublisher:   publisher,
		PullRequestOpener: opener,
	}, ProductionDriverPolicy{PollInterval: time.Second, MaxImmediateAction: 8}, func(context.Context, time.Duration) error {
		t.Fatal("reconciliation moved to a durable manual stop and must not poll")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := driver.Drive(context.Background(), driverCommand())
	want := []ProductionAction{ProductionPush, ProductionOpenPullRequest, ProductionReconcileGitHub}
	if err != nil || result.Run.State != domain.StateManualIntervention || len(coordinator.calls) != len(want) {
		t.Fatalf("result=%+v calls=%v err=%v", result, coordinator.calls, err)
	}
	for index, action := range want {
		if coordinator.calls[index] != action {
			t.Fatalf("call %d=%s want=%s", index, coordinator.calls[index], action)
		}
	}
	if coordinator.branchPublisher != publisher || coordinator.pullRequestOpen != opener {
		t.Fatalf("ports were not dispatched: publisher=%T opener=%T", coordinator.branchPublisher, coordinator.pullRequestOpen)
	}
}

func TestProductionDriverRetriesUnavailableReconciliationOnlyAfterWait(t *testing.T) {
	reader := &driverRunReader{run: driverRun(domain.StateReconcilingReviews)}
	coordinator := &driverCoordinator{runs: reader}
	reads := 0
	coordinator.apply = func(action ProductionAction) error {
		if action != ProductionReconcileGitHub {
			t.Fatalf("unexpected action %s", action)
		}
		reads++
		if reads == 1 {
			return serviceError(ErrorUnavailable, "temporary GitHub read failure", errors.New("transport"))
		}
		reader.run.State = domain.StateManualIntervention
		return nil
	}
	waits := 0
	driver := newDriverForTest(t, reader, coordinator, func(context.Context, time.Duration) error {
		waits++
		return nil
	})

	result, err := driver.Drive(context.Background(), driverCommand())
	if err != nil || reads != 2 || waits != 1 || result.Run.State != domain.StateManualIntervention || result.Action != ProductionStop {
		t.Fatalf("result=%+v reads=%d waits=%d err=%v", result, reads, waits, err)
	}
}

func TestProductionDriverReturnsDurableManualStopAfterConflict(t *testing.T) {
	reader := &driverRunReader{run: driverRun(domain.StateMerging)}
	coordinator := &driverCoordinator{runs: reader}
	coordinator.apply = func(action ProductionAction) error {
		if action != ProductionMerge {
			t.Fatalf("unexpected action %s", action)
		}
		reader.run.State = domain.StateManualIntervention
		return serviceError(ErrorConflict, "GitHub merge gate changed", errors.New("drift"))
	}
	driver := newDriverForTest(t, reader, coordinator, func(context.Context, time.Duration) error {
		t.Fatal("manual intervention must stop rather than poll")
		return nil
	})

	result, err := driver.Drive(context.Background(), driverCommand())
	if err != nil || result.Run.State != domain.StateManualIntervention || result.Action != ProductionStop || result.ActionsRun != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestProductionDriverRequiresPersistedRequesterAuthorization(t *testing.T) {
	reader := &driverRunReader{run: driverRun(domain.StateAwaitingHumanDecision)}
	coordinator := &driverCoordinator{runs: reader}
	driver := newDriverForTest(t, reader, coordinator, nil)
	command := driverCommand()
	command.Requester.ID = "intruder"
	_, err := driver.Drive(context.Background(), command)
	var safe *ServiceError
	if !errors.As(err, &safe) || safe.Category != ErrorConflict || len(coordinator.calls) != 0 {
		t.Fatalf("err=%v calls=%v", err, coordinator.calls)
	}
}

func TestNewProductionDriverRejectsBusyLoopPolicy(t *testing.T) {
	reader := &driverRunReader{run: driverRun(domain.StateCompleted)}
	coordinator := &driverCoordinator{runs: reader}
	_, err := NewProductionDriver(coordinator, reader, ProductionDriverPorts{}, ProductionDriverPolicy{MaxImmediateAction: 1}, nil)
	if err == nil {
		t.Fatal("expected non-positive poll interval to be rejected")
	}
}

func TestProductionDriverWaitsAfterImmediateActionLimit(t *testing.T) {
	reader := &driverRunReader{run: driverRun(domain.StateRepairing)}
	coordinator := &driverCoordinator{runs: reader}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waits := 0
	driver, err := NewProductionDriver(coordinator, reader, ProductionDriverPorts{}, ProductionDriverPolicy{PollInterval: time.Second, MaxImmediateAction: 1}, func(context.Context, time.Duration) error {
		waits++
		cancel()
		return context.Canceled
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = driver.Drive(ctx, driverCommand())
	if !errors.Is(err, context.Canceled) || waits != 1 || len(coordinator.calls) != 1 || coordinator.calls[0] != ProductionContinueLocal {
		t.Fatalf("err=%v waits=%d calls=%v", err, waits, coordinator.calls)
	}
}
