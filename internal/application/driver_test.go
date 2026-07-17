package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type driverRunReader struct {
	run           Run
	attention     []OperatorAttentionEvent
	appendErr     error
	pr            *domain.PullRequest
	evidence      *domain.GitHubReadEvidence
	ciObserved    int
	ciClosed      int
	ciAt          time.Time
	ciEvaluated   time.Time
	ciWarnWhenDue bool
}

func (r *driverRunReader) GetRun(context.Context, string) (Run, error) { return r.run, nil }
func (r *driverRunReader) Inspect(context.Context, string) (RunInspection, error) {
	inspection := RunInspection{Run: r.run, PullRequest: r.pr, GitHubEvidence: r.evidence}
	if r.run.State == domain.StateManualIntervention {
		inspection.Timeline = []Transition{{Sequence: 7, From: domain.StateMerging, To: domain.StateManualIntervention, Reason: "authority conflict", EvidenceReference: "controller_evidence", BoundHead: r.run.CandidateHead, CreatedAt: time.Date(2026, 7, 16, 1, 0, 0, 0, time.UTC)}}
	} else if r.run.State == domain.StateAwaitingHumanDecision {
		inspection.Timeline = []Transition{{Sequence: 3, From: domain.StateExecuting, To: domain.StateAwaitingHumanDecision, Reason: "decision required", EvidenceReference: "decision_request", CreatedAt: time.Date(2026, 7, 16, 1, 0, 0, 0, time.UTC)}}
	}
	return inspection, nil
}
func (r *driverRunReader) ObserveCIWait(_ context.Context, runID string, prNumber int64, head, profile string, threshold time.Duration, at, evaluated time.Time) (CIWaitEvidence, error) {
	r.ciObserved++
	if r.ciAt.IsZero() {
		r.ciAt = at
	}
	r.ciEvaluated = evaluated
	wait := CIWaitEvidence{RunID: runID, PRNumber: prNumber, HeadSHA: head, ProfileDigest: profile, FirstSeenAt: r.ciAt}
	if r.ciWarnWhenDue && !evaluated.Before(r.ciAt.Add(threshold)) {
		wait.WarningAt = r.ciAt.Add(threshold)
	}
	return wait, nil
}

func TestProductionDriverUsesPersistedGitHubObservationAsCrashStableCIAnchor(t *testing.T) {
	run := driverRun(domain.StateReconcilingReviews)
	run.CandidateHead, run.ProfileDigest = "head", "profile"
	pr := domain.PullRequest{Number: 7, HeadSHA: "head", State: "open"}
	observed := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	evidence := domain.GitHubReadEvidence{PullRequest: pr, Checks: []domain.GitHubCheck{{Name: "test", Required: true, ObservedSHA: "head", State: domain.CheckInProgress}}, ObservedAt: observed}
	reader := &driverRunReader{run: run, pr: &pr, evidence: &evidence}
	driver := newDriverForTest(t, reader, &driverCoordinator{runs: reader}, nil)
	driver.now = func() time.Time { return observed.Add(time.Hour) }
	if err := driver.reconcileCIWait(context.Background(), run.ID); err != nil || reader.ciAt != observed {
		t.Fatalf("anchor=%s want=%s err=%v", reader.ciAt, observed, err)
	}
}

func TestProductionDriverTracksRequiredCheckLifecycleUntilSuccess(t *testing.T) {
	run := driverRun(domain.StateReconcilingReviews)
	run.CandidateHead, run.ProfileDigest = "head", "profile"
	pr := domain.PullRequest{Number: 7, HeadSHA: "head", State: "open"}
	first := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	evidence := domain.GitHubReadEvidence{PullRequest: pr, UnknownEvents: []string{"missing_required_check:test"}, ObservedAt: first}
	reader := &driverRunReader{run: run, pr: &pr, evidence: &evidence}
	driver := newDriverForTest(t, reader, &driverCoordinator{runs: reader}, nil)
	if err := driver.reconcileCIWait(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	for index, state := range []domain.CheckState{domain.CheckQueued, domain.CheckInProgress} {
		evidence.UnknownEvents = nil
		evidence.Checks = []domain.GitHubCheck{{Name: "test", Required: true, ObservedSHA: "head", State: state}}
		evidence.ObservedAt = first.Add(time.Duration(index+1) * time.Minute)
		if err := driver.reconcileCIWait(context.Background(), run.ID); err != nil {
			t.Fatal(err)
		}
	}
	evidence.Checks[0].State = domain.CheckSuccess
	evidence.ObservedAt = first.Add(3 * time.Minute)
	if err := driver.reconcileCIWait(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	if reader.ciObserved != 3 || reader.ciClosed != 1 || !reader.ciAt.Equal(first) {
		t.Fatalf("observed=%d closed=%d first=%s want=%s", reader.ciObserved, reader.ciClosed, reader.ciAt, first)
	}
}

func TestProductionDriverWarnsFromWallClockDuringGitHubOutage(t *testing.T) {
	run := driverRun(domain.StateReconcilingReviews)
	run.CandidateHead, run.ProfileDigest = "head", "profile"
	pr := domain.PullRequest{Number: 7, HeadSHA: "head", State: "open"}
	first := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	evidence := domain.GitHubReadEvidence{PullRequest: pr, Checks: []domain.GitHubCheck{{Name: "test", Required: true, ObservedSHA: "head", State: domain.CheckInProgress}}, ObservedAt: first}
	reader := &driverRunReader{run: run, pr: &pr, evidence: &evidence, ciWarnWhenDue: true}
	driver := newDriverForTest(t, reader, &driverCoordinator{runs: reader}, nil)
	evaluated := first
	driver.now = func() time.Time { return evaluated }
	if err := driver.reconcileCIWait(context.Background(), run.ID); err != nil || len(reader.attention) != 0 {
		t.Fatalf("initial attention=%v err=%v", reader.attention, err)
	}
	evaluated = first.Add(21 * time.Minute)
	if err := driver.reconcileCIWait(context.Background(), run.ID); err != nil || len(reader.attention) != 1 {
		t.Fatalf("threshold attention=%v err=%v", reader.attention, err)
	}
	evaluated = first.Add(time.Hour)
	if err := driver.reconcileCIWait(context.Background(), run.ID); err != nil || len(reader.attention) != 1 || !reader.ciAt.Equal(first) || !reader.ciEvaluated.Equal(evaluated) {
		t.Fatalf("replay attention=%v first=%s evaluated=%s err=%v", reader.attention, reader.ciAt, reader.ciEvaluated, err)
	}
}

func TestProductionDriverRestartClosesResidualCIWaitBeforeAnyDispatch(t *testing.T) {
	for _, state := range []domain.State{domain.StateRepairing, domain.StateManualIntervention, domain.StateCompleted} {
		t.Run(string(state), func(t *testing.T) {
			reader := &driverRunReader{run: driverRun(state)}
			coordinator := &driverCoordinator{runs: reader}
			if state == domain.StateRepairing {
				coordinator.apply = func(action ProductionAction) error {
					if action != ProductionContinueLocal {
						t.Fatalf("action=%s", action)
					}
					reader.run.State = domain.StateManualIntervention
					return nil
				}
			}
			driver := newDriverForTest(t, reader, coordinator, nil)
			if _, err := driver.Drive(context.Background(), driverCommand()); err != nil {
				t.Fatal(err)
			}
			if reader.ciClosed == 0 {
				t.Fatal("residual CI wait was not closed before dispatch/stop")
			}
		})
	}
}
func (r *driverRunReader) CloseCIWaits(context.Context, string, time.Time) error {
	r.ciClosed++
	return nil
}
func (r *driverRunReader) AppendOperatorAttention(_ context.Context, event OperatorAttentionEvent) (bool, error) {
	if r.appendErr != nil {
		return false, r.appendErr
	}
	for _, current := range r.attention {
		if current.EventKey == event.EventKey {
			if current.PayloadDigest != event.PayloadDigest {
				return false, FormatOperatorAttentionConflict(event)
			}
			return false, nil
		}
	}
	r.attention = append(r.attention, event)
	return true, nil
}

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

func (c *driverCoordinator) ReplyReviewFeedback(context.Context, ProductionReplyCommand, ApprovalValidator, GitHubReadPort, ReviewCommentReplyPort) (ProductionReplyResult, error) {
	return ProductionReplyResult{Action: ProductionStop}, c.record(ProductionReplyReviewFeedback)
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

type driverReviewReplyPort struct{}

func (driverReviewReplyPort) FindReviewCommentReplies(context.Context, int64, int64) ([]domain.ReviewReply, error) {
	return nil, nil
}
func (driverReviewReplyPort) ReplyToReviewComment(context.Context, ReplyToReviewCommentRequest) (domain.ReviewReply, error) {
	return domain.ReviewReply{}, nil
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
	now := time.Date(2026, 7, 16, 1, 0, 0, 0, time.UTC)
	return authorizeTestRun(Run{ID: "run", Repository: "owner/repo", IdempotencyKey: "key", State: state, CreatedAt: now, UpdatedAt: now})
}

func newDriverForTest(t *testing.T, reader *driverRunReader, coordinator *driverCoordinator, wait ProductionWait) *ProductionDriver {
	t.Helper()
	driver, err := NewProductionDriver(coordinator, reader, reader, reader, ProductionDriverPorts{
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
	if err != nil || result.Action != ProductionStop || result.Run.State != domain.StateAwaitingHumanDecision || len(coordinator.calls) != 0 || len(reader.attention) != 1 || reader.attention[0].EventType != OperatorAttentionHumanDecision || !equalOperatorAttentionActions(reader.attention[0].AllowedActions, []OperatorAttentionActionID{OperatorAttentionActionDecide}) {
		t.Fatalf("result=%+v calls=%v err=%v", result, coordinator.calls, err)
	}
}

func TestProductionDriverDoesNotTrackCIWaitAfterChecksPass(t *testing.T) {
	run := driverRun(domain.StateReconcilingReviews)
	run.CandidateHead, run.ProfileDigest = "head", "profile"
	pr := domain.PullRequest{Number: 7, HeadSHA: "head", State: "open"}
	evidence := domain.GitHubReadEvidence{PullRequest: pr, Checks: []domain.GitHubCheck{{Name: "test", Required: true, ObservedSHA: "head", State: domain.CheckSuccess}}}
	reader := &driverRunReader{run: run, pr: &pr, evidence: &evidence}
	coordinator := &driverCoordinator{runs: reader}
	driver := newDriverForTest(t, reader, coordinator, nil)
	driver.now = func() time.Time { return time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC) }
	if err := driver.reconcileCIWait(context.Background(), run.ID); err != nil || reader.ciObserved != 0 || reader.ciClosed != 1 {
		t.Fatalf("observed=%d closed=%d err=%v", reader.ciObserved, reader.ciClosed, err)
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
	waits := []time.Duration{}
	driver := newDriverForTest(t, reader, coordinator, func(_ context.Context, interval time.Duration) error {
		waits = append(waits, interval)
		return nil
	})

	result, err := driver.Drive(context.Background(), driverCommand())
	want := []ProductionAction{ProductionReconcileGitHub, ProductionReconcileGitHub, ProductionReconcileGitHub, ProductionMerge, ProductionReconcileLinear, ProductionCleanup}
	if err != nil || result.Run.State != domain.StateCompleted || result.Action != ProductionStop || len(waits) != 2 || waits[0] != time.Second || waits[1] != time.Second || len(coordinator.calls) != len(want) {
		t.Fatalf("result=%+v calls=%v waits=%v err=%v", result, coordinator.calls, waits, err)
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

func TestProductionDriverUsesDeliveryCadenceForEveryPendingAuthority(t *testing.T) {
	for _, state := range []domain.State{
		domain.StatePROpen,
		domain.StateReconcilingReviews,
		domain.StateAwaitingHumanApproval,
		domain.StateAwaitingGitHubMergeability,
		domain.StateAwaitingLinearCompletion,
	} {
		t.Run(string(state), func(t *testing.T) {
			reader := &driverRunReader{run: driverRun(state)}
			coordinator := &driverCoordinator{runs: reader}
			calls := 0
			coordinator.apply = func(action ProductionAction) error {
				calls++
				if state == domain.StateAwaitingLinearCompletion && action != ProductionReconcileLinear {
					t.Fatalf("action=%s", action)
				}
				if state != domain.StateAwaitingLinearCompletion && action != ProductionReconcileGitHub {
					t.Fatalf("action=%s", action)
				}
				if calls == 2 {
					reader.run.State = domain.StateManualIntervention
				}
				return nil
			}
			const deliveryCadence = 37 * time.Second
			waits := []time.Duration{}
			driver, err := NewProductionDriver(coordinator, reader, reader, reader, ProductionDriverPorts{GitHubReader: driverGitHubReader{}}, ProductionDriverPolicy{PollInterval: deliveryCadence, MaxImmediateAction: 8}, func(_ context.Context, interval time.Duration) error {
				waits = append(waits, interval)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := driver.Drive(context.Background(), driverCommand())
			if err != nil || result.Run.State != domain.StateManualIntervention || calls != 2 || len(waits) != 1 || waits[0] != deliveryCadence {
				t.Fatalf("result=%+v calls=%d waits=%v err=%v", result, calls, waits, err)
			}
		})
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
	driver, err := NewProductionDriver(coordinator, reader, reader, reader, ProductionDriverPorts{
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
	if len(reader.attention) != 1 || reader.attention[0].EventType != OperatorAttentionManualIntervention {
		t.Fatalf("attention=%+v", reader.attention)
	}
}

func TestProductionDriverRetriesEveryUnavailableActionAtDeliveryCadence(t *testing.T) {
	cases := []struct {
		name   string
		state  domain.State
		action ProductionAction
	}{
		{"continue local", domain.StateExecuting, ProductionContinueLocal},
		{"GitHub reconcile", domain.StateReconcilingReviews, ProductionReconcileGitHub},
		{"review reply", domain.StateReplyingReviewFeedback, ProductionReplyReviewFeedback},
		{"push", domain.StateApprovalReady, ProductionPush},
		{"pull request creation", domain.StateBranchPushed, ProductionOpenPullRequest},
		{"merge", domain.StateMerging, ProductionMerge},
		{"Linear completion", domain.StateAwaitingLinearCompletion, ProductionReconcileLinear},
		{"cleanup and source sync", domain.StateCleaning, ProductionCleanup},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &driverRunReader{run: driverRun(tc.state)}
			coordinator := &driverCoordinator{runs: reader}
			attempts := 0
			coordinator.apply = func(action ProductionAction) error {
				if action != tc.action {
					t.Fatalf("action=%s want=%s", action, tc.action)
				}
				attempts++
				if attempts == 1 {
					return serviceError(ErrorUnavailable, "temporary action failure", errors.New("transport"))
				}
				reader.run.State = domain.StateManualIntervention
				return nil
			}
			const deliveryCadence = 41 * time.Second
			waits := []time.Duration{}
			driver, err := NewProductionDriver(coordinator, reader, reader, reader, ProductionDriverPorts{
				GitHubReader:       driverGitHubReader{},
				ReviewCommentReply: driverReviewReplyPort{},
				ApprovalValidator:  driverApprovalValidator{},
				BranchPublisher:    &driverBranchPublisher{},
				PullRequestOpener:  &driverPullRequestOpener{},
				SquashMerger:       driverMerger{},
				CleanupPort:        driverCleanupPort{},
				SourceSyncPort:     driverSourceSyncPort{},
			}, ProductionDriverPolicy{PollInterval: deliveryCadence, MaxImmediateAction: 8}, func(_ context.Context, interval time.Duration) error {
				waits = append(waits, interval)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := driver.Drive(context.Background(), driverCommand())
			if err != nil || attempts != 2 || len(waits) != 1 || waits[0] != deliveryCadence || result.Run.State != domain.StateManualIntervention {
				t.Fatalf("result=%+v attempts=%d waits=%v err=%v", result, attempts, waits, err)
			}
		})
	}
}

func TestProductionDriverUnavailableRetryHonorsCancellation(t *testing.T) {
	reader := &driverRunReader{run: driverRun(domain.StateReconcilingReviews)}
	coordinator := &driverCoordinator{runs: reader, apply: func(ProductionAction) error {
		return serviceError(ErrorUnavailable, "temporary GitHub read failure", errors.New("transport"))
	}}
	ctx, cancel := context.WithCancel(context.Background())
	driver := newDriverForTest(t, reader, coordinator, func(_ context.Context, interval time.Duration) error {
		if interval != time.Second {
			t.Fatalf("interval=%s", interval)
		}
		cancel()
		return context.Canceled
	})
	_, err := driver.Drive(ctx, driverCommand())
	if !errors.Is(err, context.Canceled) || len(coordinator.calls) != 1 {
		t.Fatalf("err=%v calls=%v", err, coordinator.calls)
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
	if err != nil || result.Run.State != domain.StateManualIntervention || result.Action != ProductionStop || result.ActionsRun != 1 || len(reader.attention) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestProductionDriverManualStopPublicationIsIdempotentAndRequired(t *testing.T) {
	reader := &driverRunReader{run: driverRun(domain.StateManualIntervention)}
	coordinator := &driverCoordinator{runs: reader}
	driver := newDriverForTest(t, reader, coordinator, nil)
	for attempt := 0; attempt < 2; attempt++ {
		result, err := driver.Drive(context.Background(), driverCommand())
		if err != nil || result.Run.State != domain.StateManualIntervention {
			t.Fatalf("attempt=%d result=%+v err=%v", attempt, result, err)
		}
	}
	if len(reader.attention) != 1 || reader.attention[0].EventType != OperatorAttentionManualIntervention {
		t.Fatalf("attention=%+v", reader.attention)
	}

	reader.attention, reader.appendErr = nil, errors.New("publisher unavailable")
	if _, err := driver.Drive(context.Background(), driverCommand()); err == nil || reader.run.State != domain.StateManualIntervention {
		t.Fatalf("state=%s err=%v", reader.run.State, err)
	}
}

func TestProductionDriverWaitsByReadingOnlyForGitHubMergeability(t *testing.T) {
	reader := &driverRunReader{run: driverRun(domain.StateAwaitingGitHubMergeability)}
	coordinator := &driverCoordinator{runs: reader}
	coordinator.apply = func(action ProductionAction) error {
		if action != ProductionReconcileGitHub {
			t.Fatalf("unexpected action %s", action)
		}
		reader.run.State = domain.StateManualIntervention
		return nil
	}
	driver := newDriverForTest(t, reader, coordinator, func(context.Context, time.Duration) error {
		t.Fatal("read-only reconciliation moved to a durable stop and must not poll")
		return nil
	})

	result, err := driver.Drive(context.Background(), driverCommand())
	if err != nil || result.Run.State != domain.StateManualIntervention || len(coordinator.calls) != 1 || coordinator.calls[0] != ProductionReconcileGitHub {
		t.Fatalf("result=%+v calls=%v err=%v", result, coordinator.calls, err)
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
	_, err := NewProductionDriver(coordinator, reader, reader, reader, ProductionDriverPorts{}, ProductionDriverPolicy{MaxImmediateAction: 1}, nil)
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
	driver, err := NewProductionDriver(coordinator, reader, reader, reader, ProductionDriverPorts{}, ProductionDriverPolicy{PollInterval: time.Second, MaxImmediateAction: 1}, func(_ context.Context, interval time.Duration) error {
		if interval != time.Second {
			t.Fatalf("interval=%s", interval)
		}
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

func TestProductionPollWaitReturnsPromptlyWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	err := waitForProductionPoll(ctx, time.Minute)
	if !errors.Is(err, context.Canceled) || time.Since(started) > 100*time.Millisecond {
		t.Fatalf("err=%v elapsed=%s", err, time.Since(started))
	}
}
