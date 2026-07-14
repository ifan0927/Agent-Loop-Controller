package sqlite_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	sqlitestore "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

// TestDurableReviewLifecycleFixtureDriver is an offline controller driver. It
// uses the production coordinator with a real SQLite store and narrow fake
// GitHub ports; no process, GitHub, Linear, or credential adapter is invoked.
func TestDurableReviewLifecycleFixtureDriver(t *testing.T) {
	t.Run("trusted root repair reply protected merge wait restart resolution retry", func(t *testing.T) {
		fixture := newLifecycleFixture(t, 1)
		fixture.admitRepairAndReply(t)
		fixture.approve(t)

		fixture.merger.rejections = []int{409}
		if _, err := fixture.coordinator.MergePullRequest(context.Background(), fixture.mergeCommand(), fixture.validator, fixture.reader, fixture.merger); err == nil {
			t.Fatal("protected merge rejection was accepted")
		}
		fixture.requireState(t, domain.StateAwaitingGitHubMergeability)
		if fixture.merger.calls != 1 {
			t.Fatalf("merge calls=%d", fixture.merger.calls)
		}

		// A new coordinator process only has the durable SQLite state. Its wait
		// observation cannot receive a merger and therefore cannot write.
		fixture.restart(t)
		beforeReads := fixture.reader.calls
		if _, err := fixture.coordinator.ReconcileGitHub(context.Background(), fixture.reconcileCommand(), fixture.reader); err != nil {
			t.Fatal(err)
		}
		fixture.requireState(t, domain.StateAwaitingGitHubMergeability)
		if fixture.reader.calls != beforeReads+1 || fixture.merger.calls != 1 {
			t.Fatalf("restart wait reads=%d merges=%d", fixture.reader.calls-beforeReads, fixture.merger.calls)
		}

		fixture.reader.setResolved(true)
		fixture.reader.advanceObservedAt()
		if _, err := fixture.coordinator.ReconcileGitHub(context.Background(), fixture.reconcileCommand(), fixture.reader); err != nil {
			inspection, _ := fixture.store.Inspect(context.Background(), fixture.run.ID)
			t.Fatalf("resolution reconcile err=%v state=%s side=%+v", err, inspection.Run.State, inspection.SideEffects)
		}
		fixture.requireState(t, domain.StateMerging)
		if _, err := fixture.coordinator.MergePullRequest(context.Background(), fixture.mergeCommand(), fixture.validator, fixture.reader, fixture.merger); err != nil {
			t.Fatal(err)
		}
		fixture.requireState(t, domain.StateAwaitingLinearCompletion)
		if fixture.merger.calls != 2 {
			t.Fatalf("guarded retry calls=%d", fixture.merger.calls)
		}
	})

	t.Run("multiple roots persist independent reply intents across restart", func(t *testing.T) {
		fixture := newLifecycleFixture(t, 2)
		fixture.admitRepairAndReply(t)
		if _, err := fixture.coordinator.ReplyReviewFeedback(context.Background(), fixture.replyCommand(), fixture.validator, fixture.reader, fixture.replies); err != nil {
			t.Fatal(err)
		}
		if got := len(fixture.replies.posts); got != 1 {
			t.Fatalf("first reply posts=%d", got)
		}
		fixture.restart(t)
		if _, err := fixture.coordinator.ReplyReviewFeedback(context.Background(), fixture.replyCommand(), fixture.validator, fixture.reader, fixture.replies); err != nil {
			t.Fatal(err)
		}
		if got := len(fixture.replies.posts); got != 2 {
			t.Fatalf("second reply posts=%d", got)
		}
		if _, err := fixture.coordinator.ReplyReviewFeedback(context.Background(), fixture.replyCommand(), fixture.validator, fixture.reader, fixture.replies); err != nil {
			t.Fatal(err)
		}
		fixture.requireState(t, domain.StateAwaitingHumanApproval)
		inspection, err := fixture.store.Inspect(context.Background(), fixture.run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(inspection.ReviewReplies) != 2 || len(inspection.SideEffects) != 2 || !slices.Equal(fixture.replies.rootIDs(), []int64{41, 42}) {
			t.Fatalf("replies=%+v side-effects=%+v roots=%v", inspection.ReviewReplies, inspection.SideEffects, fixture.replies.rootIDs())
		}
	})

	t.Run("unprotected merge succeeds on first authorized request", func(t *testing.T) {
		fixture := newLifecycleFixture(t, 1)
		fixture.admitRepairAndReply(t)
		fixture.approve(t)
		if _, err := fixture.coordinator.MergePullRequest(context.Background(), fixture.mergeCommand(), fixture.validator, fixture.reader, fixture.merger); err != nil {
			t.Fatal(err)
		}
		fixture.requireState(t, domain.StateAwaitingLinearCompletion)
		if fixture.merger.calls != 1 {
			t.Fatalf("unprotected merge calls=%d", fixture.merger.calls)
		}
	})

	for _, tc := range []struct {
		name   string
		mutate func(*lifecycleFixture)
		want   domain.State
	}{
		{"approval dismissed", func(f *lifecycleFixture) { f.reader.setApproval("PENDING") }, domain.StateAwaitingHumanApproval},
		{"new exact-head request", func(f *lifecycleFixture) { f.reader.addExactHeadChangeRequest() }, domain.StateRepairing},
		{"head drift", func(f *lifecycleFixture) { f.reader.evidence.evidence.PullRequest.HeadSHA = fixtureSHA('d') }, domain.StateManualIntervention},
		{"base drift", func(f *lifecycleFixture) { f.reader.evidence.evidence.PullRequest.BaseSHA = fixtureSHA('e') }, domain.StateManualIntervention},
	} {
		t.Run("wait rollback "+tc.name, func(t *testing.T) {
			fixture := newLifecycleFixture(t, 1)
			fixture.admitRepairAndReply(t)
			fixture.approve(t)
			fixture.merger.rejections = []int{409}
			if _, err := fixture.coordinator.MergePullRequest(context.Background(), fixture.mergeCommand(), fixture.validator, fixture.reader, fixture.merger); err == nil {
				t.Fatal("expected protected rejection")
			}
			tc.mutate(fixture)
			fixture.restart(t)
			_, _ = fixture.coordinator.ReconcileGitHub(context.Background(), fixture.reconcileCommand(), fixture.reader)
			fixture.requireState(t, tc.want)
			if tc.name == "base drift" {
				inspection, err := fixture.store.Inspect(context.Background(), fixture.run.ID)
				if err != nil || inspection.PullRequest == nil || inspection.PullRequest.BaseSHA != fixture.run.BaseSHA {
					t.Fatalf("base drift rewrote persisted target: inspection=%+v err=%v", inspection.PullRequest, err)
				}
			}
			if fixture.merger.calls != 1 {
				t.Fatalf("rollback issued duplicate merge=%d", fixture.merger.calls)
			}
		})
	}
}

type lifecycleFixture struct {
	store       *sqlitestore.Store
	controller  *fixtureController
	coordinator *application.ProductionCoordinator
	run         application.Run
	reader      *fixtureGitHubReader
	replies     *fixtureReplies
	merger      *fixtureMerger
	validator   fixtureValidator
	operator    application.Requester
	repository  application.LocalRepository
}

func newLifecycleFixture(t *testing.T, roots int) *lifecycleFixture {
	t.Helper()
	ctx := context.Background()
	store, err := sqlitestore.Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	repo := fixtureRepository()
	controller := &fixtureController{store: store, repairedHead: fixtureSHA('b')}
	source := fixtureLinearSource()
	reader := &fixtureLinearReader{source: source}
	admission, err := application.NewLinearAdmissionService(reader, fixtureResolver{repository: repo}, store, controller)
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := application.NewProductionCoordinator(admission, controller, store)
	if err != nil {
		t.Fatal(err)
	}
	operator := application.Requester{ID: "ifan0927", Kind: "github_login", DatabaseID: 1, NodeID: "USER_1", ActorType: "User"}
	started, _, err := admission.Start(ctx, application.LinearStartCommand{Requester: operator, Identifier: source.Identifier})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.GetRun(ctx, started.Run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetWorkspace(ctx, run.ID, fixtureSHA('c'), filepath.Join(t.TempDir(), "worktree")); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCandidateHead(ctx, run.ID, fixtureSHA('a')); err != nil {
		t.Fatal(err)
	}
	for _, next := range []domain.State{domain.StateAdmitting, domain.StateProvisioning, domain.StateExecuting, domain.StateVerifying, domain.StateFreshReview, domain.StateApprovalReady, domain.StatePushingBranch, domain.StateBranchPushed, domain.StateOpeningPR, domain.StatePROpen, domain.StateReconcilingReviews} {
		current, getErr := store.GetRun(ctx, run.ID)
		if getErr != nil || store.Transition(ctx, run.ID, current.State, next, "offline fixture progression", "fixture", current.CandidateHead) != nil {
			t.Fatalf("transition to %s: get=%v", next, getErr)
		}
	}
	run, err = store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	pr := fixturePR(run)
	if err := store.SavePullRequest(ctx, run.ID, pr); err != nil {
		t.Fatal(err)
	}
	gh := newFixtureGitHubReader(pr, roots)
	gh.runID = run.ID
	replies := &fixtureReplies{reader: gh}
	merger := &fixtureMerger{reader: gh}
	return &lifecycleFixture{store: store, controller: controller, coordinator: coordinator, run: run, reader: gh, replies: replies, merger: merger, validator: fixtureValidator{}, operator: operator, repository: repo}
}

func (f *lifecycleFixture) restart(t *testing.T) {
	t.Helper()
	admission, err := application.NewLinearAdmissionService(&fixtureLinearReader{source: fixtureLinearSource()}, fixtureResolver{repository: f.repository}, f.store, f.controller)
	if err != nil {
		t.Fatal(err)
	}
	f.coordinator, err = application.NewProductionCoordinator(admission, f.controller, f.store)
	if err != nil {
		t.Fatal(err)
	}
	f.run, err = f.store.GetRun(context.Background(), f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
}

func (f *lifecycleFixture) admitRepairAndReply(t *testing.T) {
	t.Helper()
	if _, err := f.coordinator.ReconcileGitHub(context.Background(), f.reconcileCommand(), f.reader); err != nil {
		inspection, _ := f.store.Inspect(context.Background(), f.run.ID)
		t.Fatalf("reconcile err=%v inspection=%+v", err, inspection)
	}
	f.requireState(t, domain.StateRepairing)
	if _, err := f.coordinator.Continue(context.Background(), application.ProductionContinueCommand{Requester: f.operator, RunID: f.run.ID, Repository: f.run.Repository, ExpectedState: domain.StateRepairing, IdempotencyKey: f.run.IdempotencyKey}); err != nil {
		t.Fatal(err)
	}
	f.run, _ = f.store.GetRun(context.Background(), f.run.ID)
	updated := fixturePR(f.run)
	if err := f.store.SavePullRequest(context.Background(), f.run.ID, updated); err != nil {
		t.Fatal(err)
	}
	f.reader.setRepairedPR(updated)
	if _, err := f.coordinator.ReconcileGitHub(context.Background(), f.reconcileCommand(), f.reader); err != nil {
		t.Fatal(err)
	}
	f.requireState(t, domain.StateReplyingReviewFeedback)
}

func (f *lifecycleFixture) approve(t *testing.T) {
	t.Helper()
	for {
		f.run, _ = f.store.GetRun(context.Background(), f.run.ID)
		if f.run.State != domain.StateReplyingReviewFeedback {
			break
		}
		if _, err := f.coordinator.ReplyReviewFeedback(context.Background(), f.replyCommand(), f.validator, f.reader, f.replies); err != nil {
			t.Fatal(err)
		}
	}
	f.requireState(t, domain.StateAwaitingHumanApproval)
	f.reader.setApproval("APPROVED")
	if _, err := f.coordinator.ReconcileGitHub(context.Background(), f.reconcileCommand(), f.reader); err != nil {
		t.Fatal(err)
	}
	f.requireState(t, domain.StateMerging)
}

func (f *lifecycleFixture) requireState(t *testing.T, want domain.State) {
	t.Helper()
	run, err := f.store.GetRun(context.Background(), f.run.ID)
	if err != nil || run.State != want {
		t.Fatalf("state=%s want=%s err=%v", run.State, want, err)
	}
	f.run = run
}

func (f *lifecycleFixture) reconcileCommand() application.ProductionReconcileCommand {
	return application.ProductionReconcileCommand{Requester: f.operator, RunID: f.run.ID, Repository: f.run.Repository, ExpectedState: f.run.State, IdempotencyKey: f.run.IdempotencyKey}
}
func (f *lifecycleFixture) replyCommand() application.ProductionReplyCommand {
	return application.ProductionReplyCommand{Requester: f.operator, RunID: f.run.ID, Repository: f.run.Repository, ExpectedState: f.run.State, IdempotencyKey: f.run.IdempotencyKey}
}
func (f *lifecycleFixture) mergeCommand() application.ProductionMergeCommand {
	return application.ProductionMergeCommand{Requester: f.operator, RunID: f.run.ID, Repository: f.run.Repository, ExpectedState: f.run.State, IdempotencyKey: f.run.IdempotencyKey}
}

type fixtureController struct {
	store        *sqlitestore.Store
	repairedHead string
}

func (c *fixtureController) StartAuthorized(ctx context.Context, input application.LocalStartInput, _ func(application.Run) error) (application.Run, error) {
	config, err := json.Marshal(input.Repository)
	if err != nil {
		return application.Run{}, err
	}
	returned, _, err := c.store.CreateRun(ctx, application.CreateRunInput{Run: application.Run{ID: input.Task.RunID, IssueID: input.Task.IssueID, IdempotencyKey: input.IdempotencyKey, SourceRevision: input.Task.SourceRevision, RawIssueJSON: string(input.RawIssueJSON), RawIssueHash: input.RawIssueHash, NormalizedTaskJSON: string(input.NormalizedJSON), TaskHash: input.TaskHash, Repository: input.Task.Repository, RepositoryConfigJSON: string(config), ProfileID: input.Repository.ProfileID, ProfileSnapshotVersion: input.Repository.ProfileSnapshotVersion, ProfileDigest: input.Repository.ProfileDigest, ProfileSnapshotJSON: input.Repository.ProfileSnapshotJSON, RegistryVersion: input.Repository.RegistryVersion, RegistryDigest: input.Repository.RegistryDigest, RepositoryBindingDigest: input.Repository.RepositoryBindingDigest, BaseBranch: input.Task.BaseBranch, WorkingBranch: input.Task.WorkingBranch, WorktreePath: filepath.Join(input.WorktreeRoot, input.Task.RunID), ArtifactRoot: filepath.Join(input.RunRoot, input.Task.RunID)}})
	return returned, err
}
func (c *fixtureController) ContinueExpected(ctx context.Context, runID string, _ domain.State, _ string, _ *application.Decision) (application.Run, error) {
	return c.store.GetRun(ctx, runID)
}
func (c *fixtureController) RepairFindings(ctx context.Context, runID string, findings []application.FindingRecord) (application.Run, error) {
	run, err := c.store.GetRun(ctx, runID)
	if err != nil || run.State != domain.StateRepairing || len(findings) == 0 {
		return application.Run{}, errors.New("fixture repair authority is incomplete")
	}
	if err := c.store.BeginRepair(ctx, run.ID, run.CandidateHead, "offline trusted review repair"); err != nil {
		return application.Run{}, err
	}
	if err := c.store.SetCandidateHead(ctx, run.ID, c.repairedHead); err != nil {
		return application.Run{}, err
	}
	if err := c.store.Transition(ctx, run.ID, domain.StateExecuting, domain.StateVerifying, "fixture verification", "fixture", c.repairedHead); err != nil {
		return application.Run{}, err
	}
	if err := c.store.Transition(ctx, run.ID, domain.StateVerifying, domain.StateFreshReview, "fixture fresh review", "fixture", c.repairedHead); err != nil {
		return application.Run{}, err
	}
	inspection, err := c.store.Inspect(ctx, run.ID)
	if err != nil {
		return application.Run{}, err
	}
	for _, feedback := range inspection.TrustedFeedback {
		if feedback.Lifecycle != domain.TrustedReviewFeedbackSelectedForRepair {
			continue
		}
		if _, changed, transitionErr := c.store.TransitionTrustedReviewFeedback(ctx, run.ID, feedback.RootCommentNodeID, domain.TrustedReviewFeedbackSelectedForRepair, domain.TrustedReviewFeedbackRepairVerified, c.repairedHead, "", 0, "", false, false); transitionErr != nil || !changed {
			return application.Run{}, errors.New("fixture repair verification transition failed")
		}
	}
	if err := c.store.Transition(ctx, run.ID, domain.StateFreshReview, domain.StateApprovalReady, "fixture fresh review passed", "fixture", c.repairedHead); err != nil {
		return application.Run{}, err
	}
	for _, next := range []domain.State{domain.StatePushingBranch, domain.StateBranchPushed, domain.StateOpeningPR, domain.StatePROpen, domain.StateReconcilingReviews} {
		if err := c.store.Transition(ctx, run.ID, map[domain.State]domain.State{domain.StatePushingBranch: domain.StateApprovalReady, domain.StateBranchPushed: domain.StatePushingBranch, domain.StateOpeningPR: domain.StateBranchPushed, domain.StatePROpen: domain.StateOpeningPR, domain.StateReconcilingReviews: domain.StatePROpen}[next], next, "fixture delivery", "fixture", c.repairedHead); err != nil {
			return application.Run{}, err
		}
	}
	return c.store.GetRun(ctx, run.ID)
}

type fixtureLinearReader struct{ source application.LinearTaskSource }

func (r *fixtureLinearReader) ReadIssue(context.Context, string) (application.LinearTaskSource, []application.LinearRequestObservation, error) {
	return r.source, nil, nil
}

type fixtureResolver struct{ repository application.LocalRepository }

func (r fixtureResolver) ResolveLinearAdmissionRepository(label string) (application.LocalRepository, bool) {
	return r.repository, label == "repo:fixture"
}

type fixtureValidator struct{}

func (fixtureValidator) ValidateApprovalReady(context.Context, string) error { return nil }

type fixtureGitHubReader struct {
	evidence applicationGitHubEvidence
	calls    int
	runID    string
}

// applicationGitHubEvidence keeps the fake's mutable transport values together
// while returning copies so a coordinator cannot mutate the next observation.
type applicationGitHubEvidence struct {
	evidence domain.GitHubReadEvidence
	handoff  domain.InlineReviewBodyHandoff
	metadata application.GitHubInstallationMetadata
}

func newFixtureGitHubReader(pr domain.PullRequest, roots int) *fixtureGitHubReader {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	operator := domain.ActorIdentity{DatabaseID: 1, NodeID: "USER_1", Login: "ifan0927", Type: "User"}
	threads := make([]domain.GitHubReviewThread, 0, roots)
	reviews := make([]domain.GitHubReview, 0, roots)
	bodies := make([]domain.InlineReviewBody, 0, roots)
	for index := 0; index < roots; index++ {
		root := int64(41 + index)
		node := "ROOT_" + string(rune('A'+index))
		review := domain.GitHubReview{DatabaseID: int64(51 + index), NodeID: "REVIEW_" + string(rune('A'+index)), State: "CHANGES_REQUESTED", CommitSHA: pr.HeadSHA, SourceAt: now, Actor: operator}
		body := "trusted root " + string(rune('A'+index))
		digest := domain.TrustedReviewFeedbackDigest(body)
		comment := domain.GitHubReviewComment{DatabaseID: root, NodeID: node, Author: &operator, Review: review, BodyDigest: digest, CreatedAt: now, UpdatedAt: now}
		threads = append(threads, domain.GitHubReviewThread{NodeID: "THREAD_" + string(rune('A'+index)), OriginalCommitSHA: pr.HeadSHA, Path: "internal/fixture.go", Comments: []domain.GitHubReviewComment{comment}})
		reviews = append(reviews, review)
		bodies = append(bodies, domain.InlineReviewBody{ThreadNodeID: threads[len(threads)-1].NodeID, CommentNodeID: node, Body: body, BodyDigest: digest})
	}
	repository := domain.RepositoryIdentity{ID: 99, NodeID: "REPO_99", Owner: "owner", Name: "repo"}
	return &fixtureGitHubReader{evidence: applicationGitHubEvidence{evidence: domain.GitHubReadEvidence{Repository: repository, PullRequest: pr, Checks: []domain.GitHubCheck{{ID: "check", Name: "fixture", Required: true, State: domain.CheckSuccess, ObservedSHA: pr.HeadSHA}}, Reviews: reviews, ReviewThreads: threads, ObservedAt: now}, handoff: domain.InlineReviewBodyHandoff{Comments: bodies}, metadata: application.GitHubInstallationMetadata{AppID: 7, InstallationID: 8, Repository: repository, TokenExpiresAt: now.Add(time.Hour), PermissionsDigest: strings.Repeat("a", 64), ObservedAt: now}}}
}

func (r *fixtureGitHubReader) Authority() application.GitHubInstallationMetadata {
	return r.evidence.metadata
}
func (r *fixtureGitHubReader) Read(context.Context, int64, string) (domain.GitHubReadEvidence, domain.InlineReviewBodyHandoff, []application.GitHubRequestObservation, application.GitHubInstallationMetadata, error) {
	r.calls++
	evidence := r.evidence.evidence
	evidence.Reviews = append([]domain.GitHubReview(nil), evidence.Reviews...)
	evidence.ReviewThreads = append([]domain.GitHubReviewThread(nil), evidence.ReviewThreads...)
	for index := range evidence.ReviewThreads {
		evidence.ReviewThreads[index].Comments = append([]domain.GitHubReviewComment(nil), evidence.ReviewThreads[index].Comments...)
	}
	handoff := domain.InlineReviewBodyHandoff{Comments: append([]domain.InlineReviewBody(nil), r.evidence.handoff.Comments...)}
	observation := application.GitHubRequestObservation{RunID: r.runID, Operation: "fixture_github_read", Category: "fixture", HTTPStatus: 200, ResponseDigest: strings.Repeat("b", 64), InstallationID: r.evidence.metadata.InstallationID, Repository: r.evidence.metadata.Repository, ObservedAt: r.evidence.evidence.ObservedAt}
	return evidence, handoff, []application.GitHubRequestObservation{observation}, r.evidence.metadata, nil
}

func (r *fixtureGitHubReader) setRepairedPR(pr domain.PullRequest) {
	r.evidence.evidence.PullRequest = pr
	r.evidence.evidence.Reviews = nil
	r.evidence.evidence.ObservedAt = r.evidence.evidence.ObservedAt.Add(time.Minute)
	r.evidence.evidence.Checks[0].ObservedSHA = pr.HeadSHA
}

func (r *fixtureGitHubReader) setApproval(state string) {
	if state == "PENDING" {
		r.evidence.evidence.Reviews = nil
		r.evidence.evidence.ObservedAt = r.evidence.evidence.ObservedAt.Add(time.Minute)
		return
	}
	operator := domain.ActorIdentity{DatabaseID: 1, NodeID: "USER_1", Login: "ifan0927", Type: "User"}
	now := r.evidence.evidence.ObservedAt.Add(time.Minute)
	r.evidence.evidence.Reviews = []domain.GitHubReview{{DatabaseID: 91, NodeID: "APPROVAL", State: state, CommitSHA: r.evidence.evidence.PullRequest.HeadSHA, SourceAt: now, Actor: operator}}
	r.evidence.evidence.ObservedAt = now
}

func (r *fixtureGitHubReader) setResolved(value bool) {
	for index := range r.evidence.evidence.ReviewThreads {
		r.evidence.evidence.ReviewThreads[index].Resolved = value
	}
}

func (r *fixtureGitHubReader) advanceObservedAt() {
	r.evidence.evidence.ObservedAt = r.evidence.evidence.ObservedAt.Add(time.Minute)
}

func (r *fixtureGitHubReader) addReply(request application.ReplyToReviewCommentRequest, reply domain.ReviewReply) {
	for index := range r.evidence.evidence.ReviewThreads {
		thread := &r.evidence.evidence.ReviewThreads[index]
		if len(thread.Comments) == 0 || thread.Comments[0].DatabaseID != request.RootCommentID {
			continue
		}
		thread.Comments = append(thread.Comments, domain.GitHubReviewComment{DatabaseID: reply.DatabaseID, NodeID: reply.NodeID, ReplyToDatabaseID: request.RootCommentID, ReplyToNodeID: thread.Comments[0].NodeID, BodyDigest: domain.TrustedReviewFeedbackDigest(request.Body), CreatedAt: reply.CreatedAt, UpdatedAt: reply.CreatedAt})
		r.evidence.handoff.Comments = append(r.evidence.handoff.Comments, domain.InlineReviewBody{ThreadNodeID: thread.NodeID, CommentNodeID: reply.NodeID, Body: request.Body, BodyDigest: domain.TrustedReviewFeedbackDigest(request.Body)})
		return
	}
}

func (r *fixtureGitHubReader) addExactHeadChangeRequest() {
	now := r.evidence.evidence.ObservedAt.Add(time.Minute)
	operator := domain.ActorIdentity{DatabaseID: 1, NodeID: "USER_1", Login: "ifan0927", Type: "User"}
	body := "new exact-head request"
	review := domain.GitHubReview{DatabaseID: 92, NodeID: "REVIEW_NEW", State: "CHANGES_REQUESTED", CommitSHA: r.evidence.evidence.PullRequest.HeadSHA, SourceAt: now, Actor: operator}
	comment := domain.GitHubReviewComment{DatabaseID: 93, NodeID: "ROOT_NEW", Author: &operator, Review: review, BodyDigest: domain.TrustedReviewFeedbackDigest(body), CreatedAt: now, UpdatedAt: now}
	r.evidence.evidence.Reviews = []domain.GitHubReview{review}
	r.evidence.evidence.ReviewThreads = append(r.evidence.evidence.ReviewThreads, domain.GitHubReviewThread{NodeID: "THREAD_NEW", OriginalCommitSHA: r.evidence.evidence.PullRequest.HeadSHA, Path: "internal/new.go", Comments: []domain.GitHubReviewComment{comment}})
	r.evidence.handoff.Comments = append(r.evidence.handoff.Comments, domain.InlineReviewBody{ThreadNodeID: "THREAD_NEW", CommentNodeID: "ROOT_NEW", Body: body, BodyDigest: comment.BodyDigest})
	r.evidence.evidence.ObservedAt = now
}

type fixtureReplies struct {
	reader *fixtureGitHubReader
	posts  []application.ReplyToReviewCommentRequest
}

func (p *fixtureReplies) FindReviewCommentReplies(context.Context, int64, int64) ([]domain.ReviewReply, error) {
	var replies []domain.ReviewReply
	for _, thread := range p.reader.evidence.evidence.ReviewThreads {
		for _, comment := range thread.Comments[1:] {
			replies = append(replies, domain.ReviewReply{DatabaseID: comment.DatabaseID, NodeID: comment.NodeID, ReplyToID: comment.ReplyToDatabaseID, MarkerDigest: domain.ReviewReplyMarkerDigest(p.bodyFor(comment.NodeID)), Actor: domain.ActorIdentity{AppID: 7}, CreatedAt: comment.CreatedAt})
		}
	}
	return replies, nil
}
func (p *fixtureReplies) ReplyToReviewComment(_ context.Context, request application.ReplyToReviewCommentRequest) (domain.ReviewReply, error) {
	p.posts = append(p.posts, request)
	reply := domain.ReviewReply{DatabaseID: int64(101 + len(p.posts)), NodeID: "REPLY_" + string(rune('A'+len(p.posts)-1)), ReplyToID: request.RootCommentID, MarkerDigest: request.MarkerDigest, Actor: domain.ActorIdentity{AppID: 7}, CreatedAt: p.reader.evidence.evidence.ObservedAt.Add(time.Minute)}
	p.reader.addReply(request, reply)
	return reply, nil
}
func (p *fixtureReplies) rootIDs() []int64 {
	result := make([]int64, len(p.posts))
	for index, post := range p.posts {
		result[index] = post.RootCommentID
	}
	return result
}
func (p *fixtureReplies) bodyFor(node string) string {
	for _, body := range p.reader.evidence.handoff.Comments {
		if body.CommentNodeID == node {
			return body.Body
		}
	}
	return ""
}

type fixtureMerger struct {
	reader     *fixtureGitHubReader
	rejections []int
	calls      int
}

func (m *fixtureMerger) SquashMerge(_ context.Context, request application.SquashMergeRequest) (domain.PullRequest, []application.GitHubRequestObservation, application.GitHubInstallationMetadata, error) {
	m.calls++
	observation := application.GitHubRequestObservation{RunID: m.reader.runID, Operation: "squash_merge_pull_request", Category: "fixture", HTTPStatus: 200, ResponseDigest: strings.Repeat("c", 64), InstallationID: m.reader.evidence.metadata.InstallationID, Repository: m.reader.evidence.metadata.Repository, ObservedAt: m.reader.evidence.evidence.ObservedAt.Add(time.Minute)}
	if len(m.rejections) > 0 {
		status := m.rejections[0]
		m.rejections = m.rejections[1:]
		observation.HTTPStatus = status
		return domain.PullRequest{}, []application.GitHubRequestObservation{observation}, m.reader.evidence.metadata, &application.MergeRejectedError{HTTPStatus: status, Operation: "squash_merge_pull_request", Cause: errors.New("fixture merge protection rejection")}
	}
	pr := m.reader.evidence.evidence.PullRequest
	pr.State, pr.Merged, pr.MergeSHA, pr.MergedAt = "closed", true, fixtureSHA('f'), observation.ObservedAt
	return pr, []application.GitHubRequestObservation{observation}, m.reader.evidence.metadata, nil
}

func fixtureRepository() application.LocalRepository {
	return application.LocalRepository{ProfileID: "fixture-profile", ProfileSnapshotVersion: 1, ProfileDigest: strings.Repeat("1", 64), ProfileSnapshotJSON: `{"fixture":true}`, RegistryVersion: 1, RegistryDigest: strings.Repeat("2", 64), RepositoryBindingDigest: strings.Repeat("3", 64), CanonicalRepository: "owner/repo", BaseBranch: "main", RunRoot: "/fixture/runs", WorktreeRoot: "/fixture/worktrees", VerifierIDs: []string{"fixture"}, GitHubAppID: 7, GitHubInstallationID: 8, ExpectedRepositoryID: 99, AllowedOperatorLogins: []string{"ifan0927"}, TrustedOperatorActors: []application.TrustedActorIdentity{{DatabaseID: 1, NodeID: "USER_1", Login: "ifan0927", Type: "User"}}}
}
func fixtureLinearSource() application.LinearTaskSource {
	now := time.Date(2026, 7, 14, 11, 0, 0, 0, time.UTC)
	return application.LinearTaskSource{Provider: "linear", IssueID: "linear-fixture-id", Identifier: "IFAN-FIXTURE", URL: "https://linear.invalid/IFAN-FIXTURE", Title: "offline fixture", Description: "## Goal\nOffline lifecycle fixture\n\n## Acceptance Criteria\n- deterministic\n\n## Out of Scope\n- live writes", Team: application.LinearTeam{ID: "team", Key: "IFAN", Name: "IFAN"}, State: application.LinearState{ID: "todo", Name: "Todo", Type: "unstarted"}, Labels: []application.LinearLabel{{ID: "agent", Name: "agent:codex"}, {ID: "repo", Name: "repo:fixture"}}, Cycle: application.LinearCycle{ID: "cycle", Number: 1, StartsAt: now, EndsAt: now.Add(time.Hour), IsActive: true}, BranchName: "ifan/fixture-review-lifecycle", SourceRevision: "fixture-v1", CreatedAt: now, UpdatedAt: now, ObservedAt: now}
}
func fixturePR(run application.Run) domain.PullRequest {
	return domain.PullRequest{Number: 7, DatabaseID: 70, NodeID: "PR_7", URL: "https://example.invalid/pull/7", HeadBranch: run.WorkingBranch, BaseBranch: run.BaseBranch, HeadSHA: run.CandidateHead, BaseSHA: run.BaseSHA, BodyDigest: "fixture-body-digest", OwnershipKey: run.IdempotencyKey, State: "open"}
}
func fixtureSHA(value rune) string { return strings.Repeat(string(value), 40) }
