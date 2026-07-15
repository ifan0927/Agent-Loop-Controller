package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestProductionNextActionStopsBeforeUnimplementedWrites(t *testing.T) {
	cases := map[domain.State]ProductionAction{
		domain.StateExecuting:                  ProductionContinueLocal,
		domain.StateAwaitingHumanDecision:      ProductionContinueLocal,
		domain.StatePROpen:                     ProductionReconcileGitHub,
		domain.StateApprovalReady:              ProductionPush,
		domain.StateBranchPushed:               ProductionOpenPullRequest,
		domain.StatePushingBranch:              ProductionPush,
		domain.StateAwaitingGitHubMergeability: ProductionReconcileGitHub,
		domain.StateAwaitingLinearCompletion:   ProductionReconcileLinear,
		domain.StateManualIntervention:         ProductionStop,
		domain.StateCompleted:                  ProductionStop,
	}
	for state, want := range cases {
		if got, _ := productionNextAction(state); got != want {
			t.Fatalf("state %s action=%s want=%s", state, got, want)
		}
	}
}

type repairingController struct {
	serviceController
	findings []FindingRecord
}

func (c *repairingController) RepairFindings(_ context.Context, _ string, findings []FindingRecord) (Run, error) {
	c.findings = append([]FindingRecord(nil), findings...)
	updated := c.run
	updated.State = domain.StateApprovalReady
	return updated, nil
}

func TestProductionContinueUsesOnlyPersistedRepairFindings(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateRepairing)
	finding := repairFinding("finding-1", "quoted untrusted review text")
	store.inspection = RunInspection{Run: run, Findings: []FindingRecord{finding}}
	controller := &repairingController{serviceController: serviceController{run: run}}
	coordinator.controller = controller
	result, err := coordinator.Continue(context.Background(), ProductionContinueCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey})
	if err != nil || len(controller.findings) != 1 || result.Action != ProductionPush {
		t.Fatalf("result=%+v findings=%+v err=%v", result, controller.findings, err)
	}
}

type pushTestStore struct {
	admissionStore
	run               Run
	inspection        RunInspection
	side              SideEffectRecord
	transitions       []Transition
	resources         []OwnedResource
	pr                *domain.PullRequest
	merge             *MergeRecord
	github            []domain.GitHubReadEvidence
	savedFeedback     []TrustedReviewFeedbackRecord
	requests          []GitHubRequestObservation
	linearCompletion  []LinearCompletionObservation
	linearRequests    []LinearRequestObservation
	metadata          []GitHubInstallationMetadata
	cleanup           []CleanupRecord
	cleanupWrites     int
	cleanupFailAt     int
	requestErr        error
	genericReadErr    error
	manualTargetErr   error
	genericReadCalls  int
	manualTargetCalls int
	mergePolicyErr    error
	finalizeErr       error
	finalizeCalls     int
	completion        ReviewReplyCompletion
	leaseMu           sync.Mutex
	leaseHeld         bool
	leaseLost         bool
}

func (s *pushTestStore) AcquireLease(context.Context, string, string, time.Time) (bool, error) {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	if s.leaseHeld {
		return false, nil
	}
	s.leaseHeld = true
	return true, nil
}

func (s *pushTestStore) ReleaseLease(context.Context, string, string) error {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	s.leaseHeld = false
	return nil
}

func (s *pushTestStore) RenewLease(context.Context, string, string, time.Time) (bool, error) {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	return s.leaseHeld && !s.leaseLost, nil
}

func (s *pushTestStore) GetRun(context.Context, string) (Run, error) { return s.run, nil }
func (s *pushTestStore) Transition(_ context.Context, _ string, from, to domain.State, reason, evidence, head string) error {
	if s.run.State != from {
		return errors.New("unexpected transition source")
	}
	s.run.State = to
	s.transitions = append(s.transitions, Transition{From: from, To: to, Reason: reason, EvidenceReference: evidence, BoundHead: head})
	return nil
}
func (s *pushTestStore) SetLastError(_ context.Context, _ string, value string) error {
	s.run.LastError = value
	return nil
}
func (s *pushTestStore) AddOwnedResource(_ context.Context, value OwnedResource) error {
	s.resources = append(s.resources, value)
	return nil
}
func (s *pushTestStore) Inspect(context.Context, string) (RunInspection, error) {
	result := s.inspection
	if result.Run.ID == "" {
		result.Run = s.run
	}
	if result.PullRequest == nil && s.pr != nil {
		result.PullRequest = s.pr
	}
	if result.Merge == nil && s.merge != nil {
		result.Merge = s.merge
	}
	if s.side.ID != 0 && len(result.SideEffects) == 0 {
		result.SideEffects = []SideEffectRecord{s.side}
	}
	if len(result.LinearCompletion) == 0 {
		result.LinearCompletion = append([]LinearCompletionObservation(nil), s.linearCompletion...)
	}
	if len(result.Resources) == 0 {
		result.Resources = append([]OwnedResource(nil), s.resources...)
	}
	if len(result.Cleanup) == 0 {
		result.Cleanup = append([]CleanupRecord(nil), s.cleanup...)
	}
	return result, nil
}
func (s *pushTestStore) BeginSideEffect(_ context.Context, value SideEffectRecord) (SideEffectRecord, bool, error) {
	if s.side.ID == 0 {
		value.ID, value.Status = 1, "intent"
		s.side = value
		return value, true, nil
	}
	if s.side.IntentJSON != value.IntentJSON || s.side.IdempotencyKey != value.IdempotencyKey {
		return SideEffectRecord{}, false, errors.New("conflicting side effect")
	}
	return s.side, false, nil
}
func (s *pushTestStore) FinishSideEffect(_ context.Context, value SideEffectRecord) error {
	s.side = value
	return nil
}
func (s *pushTestStore) RetryMergeSideEffect(_ context.Context, value SideEffectRecord) (SideEffectRecord, bool, error) {
	if s.side.ID != value.ID || s.side.Status != "failed" || !mergePolicySideEffect(s.side) {
		return s.side, false, nil
	}
	s.side.Status, s.side.Attempt, s.side.ResultJSON = "intent", s.side.Attempt+1, `{"category":"merge_policy_retry_claimed"}`
	return s.side, true, nil
}
func (s *pushTestStore) RecordMergePolicyPending(_ context.Context, runID string, value SideEffectRecord, head string) error {
	if s.mergePolicyErr != nil {
		return s.mergePolicyErr
	}
	if runID != s.run.ID || head != s.run.CandidateHead || s.run.State != domain.StateMerging || s.side.ID != value.ID || value.Status != "failed" || !mergePolicySideEffect(value) {
		return errors.New("unexpected merge policy pending record")
	}
	s.side = value
	s.run.State = domain.StateAwaitingGitHubMergeability
	s.run.LastError = "merge_policy_pending"
	s.transitions = append(s.transitions, Transition{From: domain.StateMerging, To: domain.StateAwaitingGitHubMergeability, Reason: "GitHub merge protection awaits human thread resolution", EvidenceReference: "merge_policy_pending", BoundHead: head})
	return nil
}
func (s *pushTestStore) BeginReviewReplySideEffect(_ context.Context, _ string, value SideEffectRecord) (SideEffectRecord, bool, error) {
	return s.BeginSideEffect(context.Background(), value)
}
func (s *pushTestStore) FinishReviewReplySideEffect(_ context.Context, _ string, value SideEffectRecord) error {
	return s.FinishSideEffect(context.Background(), value)
}
func (s *pushTestStore) RetryReviewReplySideEffect(_ context.Context, _ string, value SideEffectRecord, maximum int) (SideEffectRecord, bool, error) {
	if s.side.ID != value.ID || s.side.Status != "failed" || s.side.Attempt >= maximum {
		return s.side, false, nil
	}
	s.side.Status, s.side.Attempt, s.side.ResultJSON = "intent", s.side.Attempt+1, ""
	return s.side, true, nil
}
func (s *pushTestStore) TransitionReviewReplyFeedback(_ context.Context, _ string, _ string, root string, expected, next domain.TrustedReviewFeedbackLifecycle, head, intent string, replyID int64, replyNode string, resolved, outdated bool) (TrustedReviewFeedbackRecord, bool, error) {
	return s.TransitionTrustedReviewFeedback(context.Background(), "", root, expected, next, head, intent, replyID, replyNode, resolved, outdated)
}
func (s *pushTestStore) ResolveReviewReplyFeedback(_ context.Context, _ string, _ string, feedback TrustedReviewFeedbackRecord, head string, outdated bool, observations []GitHubRequestObservation) (bool, error) {
	_, changed, err := s.TransitionTrustedReviewFeedback(context.Background(), "", feedback.RootCommentNodeID, feedback.Lifecycle, domain.TrustedReviewFeedbackResolved, head, feedback.ReplyIntentKey, 0, "", true, outdated)
	if err == nil && changed {
		s.requests = append(s.requests, observations...)
	}
	return changed, err
}
func (s *pushTestStore) TransitionReviewReplyRun(_ context.Context, _ string, _ string, expected, next domain.State, reason, evidence, head string) error {
	return s.Transition(context.Background(), "", expected, next, reason, evidence, head)
}
func (s *pushTestStore) SavePullRequest(_ context.Context, _ string, value domain.PullRequest) error {
	if s.pr != nil && (s.pr.Number != value.Number || s.pr.NodeID != value.NodeID) {
		return errors.New("conflicting pull request")
	}
	copy := value
	s.pr = &copy
	return nil
}
func (s *pushTestStore) SaveMerge(_ context.Context, value MergeRecord) error {
	if s.merge != nil && *s.merge != value {
		return errors.New("conflicting merge")
	}
	copy := value
	s.merge = &copy
	return nil
}
func (*pushTestStore) SaveReviewReplyEvidence(context.Context, ReviewReplyEvidence) error { return nil }
func (s *pushTestStore) FinalizeReviewReply(_ context.Context, value ReviewReplyCompletion) (bool, error) {
	s.finalizeCalls++
	s.completion = value
	if s.finalizeErr != nil {
		return false, s.finalizeErr
	}
	for index := range s.inspection.TrustedFeedback {
		if s.inspection.TrustedFeedback[index].RootCommentNodeID == value.Feedback.RootCommentNodeID {
			s.inspection.TrustedFeedback[index].Lifecycle = domain.TrustedReviewFeedbackReplied
			s.inspection.TrustedFeedback[index].ReplyDatabaseID, s.inspection.TrustedFeedback[index].ReplyNodeID = value.Reply.DatabaseID, value.Reply.NodeID
		}
	}
	s.inspection.ReviewReplies = append(s.inspection.ReviewReplies, ReviewReplyEvidence{RunID: value.Feedback.RunID, RootCommentNodeID: value.Feedback.RootCommentNodeID, PullRequestNumber: value.Feedback.PRNumber, RootCommentID: value.Feedback.RootCommentDatabaseID, RepairedHead: value.Head, MarkerDigest: value.Side.IdempotencyKey, ReplyDatabaseID: value.Reply.DatabaseID, ReplyNodeID: value.Reply.NodeID, AppID: value.Reply.Actor.AppID, ObservedAt: value.Reply.CreatedAt})
	s.side.Status = "observed"
	return true, nil
}
func (s *pushTestStore) TransitionTrustedReviewFeedback(_ context.Context, _ string, root string, expected, next domain.TrustedReviewFeedbackLifecycle, head, intent string, replyID int64, replyNode string, resolved, outdated bool) (TrustedReviewFeedbackRecord, bool, error) {
	for index := range s.inspection.TrustedFeedback {
		item := &s.inspection.TrustedFeedback[index]
		if item.RootCommentNodeID != root {
			continue
		}
		if item.Lifecycle != expected {
			return *item, false, nil
		}
		item.Lifecycle, item.Resolved, item.Outdated = next, resolved, outdated
		if head != "" {
			item.BoundRepairHead = head
		}
		if intent != "" {
			item.ReplyIntentKey = intent
		}
		if replyID != 0 {
			item.ReplyDatabaseID, item.ReplyNodeID = replyID, replyNode
		}
		return *item, true, nil
	}
	return TrustedReviewFeedbackRecord{}, false, errors.New("feedback missing")
}
func (*pushTestStore) SavePollObservation(context.Context, PollObservation) error { return nil }
func (*pushTestStore) SaveFinding(context.Context, FindingRecord) error           { return nil }
func (*pushTestStore) SaveHumanApproval(context.Context, string, domain.HumanApproval) error {
	return nil
}
func (*pushTestStore) PollProgress(context.Context, string, int64, string) ([]PollObservation, error) {
	return nil, nil
}
func (s *pushTestStore) SaveGitHubInstallation(_ context.Context, _ string, value GitHubInstallationMetadata) error {
	s.metadata = append(s.metadata, value)
	return nil
}
func (s *pushTestStore) SaveGitHubRequest(_ context.Context, value GitHubRequestObservation) error {
	if s.requestErr != nil {
		return s.requestErr
	}
	s.requests = append(s.requests, value)
	return nil
}
func (s *pushTestStore) SaveReviewReplyObservations(_ context.Context, _ string, _ string, values []GitHubRequestObservation) error {
	s.requests = append(s.requests, values...)
	return nil
}
func (s *pushTestStore) SaveGitHubEvidence(_ context.Context, _ string, value domain.GitHubReadEvidence) error {
	s.github = append(s.github, value)
	return nil
}
func (s *pushTestStore) SaveGitHubReadSuccess(_ context.Context, _ string, _ string, expected domain.State, key string, observations []GitHubRequestObservation, pr domain.PullRequest, metadata GitHubInstallationMetadata, evidence domain.GitHubReadEvidence, feedback []TrustedReviewFeedbackRecord, observed *domain.HumanApprovalObservation, approval *domain.HumanApproval, next domain.State, reason string) error {
	s.genericReadCalls++
	if s.run.State != expected || s.run.IdempotencyKey != key {
		return errors.New("unexpected GitHub read authority")
	}
	if s.genericReadErr != nil {
		return s.genericReadErr
	}
	s.requests = append(s.requests, observations...)
	s.metadata = append(s.metadata, metadata)
	s.github = append(s.github, evidence)
	s.savedFeedback = append([]TrustedReviewFeedbackRecord(nil), feedback...)
	s.pr = &pr
	s.inspection.PullRequest, s.inspection.GitHubEvidence = &pr, &evidence
	s.inspection.ApprovalObservation, s.inspection.Approval = observed, approval
	if next != expected {
		return s.Transition(context.Background(), "", expected, next, reason, "github_read_evidence", s.run.CandidateHead)
	}
	return nil
}
func (s *pushTestStore) SaveGitHubManualPRTargetDrift(_ context.Context, _ string, _ string, expected domain.State, key string, _ domain.RepositoryIdentity, persisted domain.PullRequest, observations []GitHubRequestObservation, metadata GitHubInstallationMetadata, evidence domain.GitHubReadEvidence, reason string) error {
	s.manualTargetCalls++
	if s.run.State != expected || s.run.IdempotencyKey != key || s.pr == nil || *s.pr != persisted {
		return errors.New("unexpected manual target-drift authority")
	}
	if s.manualTargetErr != nil {
		return s.manualTargetErr
	}
	s.requests = append(s.requests, observations...)
	s.metadata = append(s.metadata, metadata)
	s.github = append(s.github, evidence)
	s.inspection.GitHubEvidence = &evidence
	return s.Transition(context.Background(), "", expected, domain.StateManualIntervention, reason, "github_read_evidence", persisted.HeadSHA)
}
func (s *pushTestStore) SaveGitHubReadFailure(_ context.Context, _ string, _ string, expected domain.State, key string, observations []GitHubRequestObservation) error {
	if s.run.State != expected || s.run.IdempotencyKey != key {
		return errors.New("unexpected GitHub read failure authority")
	}
	s.requests = append(s.requests, observations...)
	return nil
}
func (s *pushTestStore) SaveLinearCompletionObservation(_ context.Context, value LinearCompletionObservation) error {
	value.ID = int64(len(s.linearCompletion) + 1)
	s.linearCompletion = append(s.linearCompletion, value)
	return nil
}
func (s *pushTestStore) SaveLinearRequestObservation(_ context.Context, _ string, value LinearRequestObservation) error {
	s.linearRequests = append(s.linearRequests, value)
	return nil
}

func (s *pushTestStore) UpsertCleanup(_ context.Context, value CleanupRecord) error {
	s.cleanupWrites++
	if s.cleanupFailAt > 0 && s.cleanupWrites == s.cleanupFailAt {
		return errors.New("cleanup persistence unavailable")
	}
	for index := range s.cleanup {
		if s.cleanup[index].RunID == value.RunID && s.cleanup[index].Kind == value.Kind && s.cleanup[index].Name == value.Name {
			s.cleanup[index] = value
			return nil
		}
	}
	s.cleanup = append(s.cleanup, value)
	return nil
}

func (s *pushTestStore) CleanupProgress(_ context.Context, runID string) ([]CleanupRecord, error) {
	var result []CleanupRecord
	for _, item := range s.cleanup {
		if item.RunID == runID {
			result = append(result, item)
		}
	}
	return result, nil
}

type pushValidator struct{ calls int }

func (v *pushValidator) ValidateApprovalReady(context.Context, string) error {
	v.calls++
	return nil
}

type failingPushValidator struct{ pushValidator }

func (v *failingPushValidator) ValidateApprovalReady(context.Context, string) error {
	v.calls++
	return errors.New("verification or fresh review is stale")
}

type pushPublisher struct {
	remotes        []string
	reads          int
	pushes         int
	expectedRemote string
	pushErr        error
	evidence       PushEvidence
}

func (p *pushPublisher) RemoteSHA(context.Context, string, string) (string, error) {
	if p.reads >= len(p.remotes) {
		return "", errors.New("unexpected remote read")
	}
	value := p.remotes[p.reads]
	p.reads++
	return value, nil
}
func (p *pushPublisher) Push(_ context.Context, _ string, _ string, _ string, expectedRemote string, _ string) (PushEvidence, error) {
	p.pushes++
	p.expectedRemote = expectedRemote
	return p.evidence, p.pushErr
}

func newPushCoordinator(t *testing.T, state domain.State) (*ProductionCoordinator, *pushTestStore, Run) {
	t.Helper()
	repository := LocalRepository{CanonicalRepository: "owner/repo", SourcePath: "/owned/source", OriginPath: "/owned/origin", BaseBranch: "main", VerifierIDs: []string{"fixture-go-test"}, AllowedOperatorLogins: []string{"operator"}}
	reader := &admissionReader{source: validLinearSource()}
	snapshot, _, err := admitLinearTask(reader.source, admissionResolver{repositories: map[string]LocalRepository{"owner/repo": repository}})
	if err != nil {
		t.Fatal(err)
	}
	run := authorizeTestRun(Run{ID: snapshot.Task.RunID, IssueID: snapshot.Task.IssueID, IdempotencyKey: snapshot.IdempotencyKey, SourceRevision: snapshot.Task.SourceRevision, RawIssueJSON: string(snapshot.RawJSON), RawIssueHash: snapshot.RawHash, Repository: snapshot.Task.Repository, RepositoryConfigJSON: mustJSON(t, repository), WorkingBranch: snapshot.Task.WorkingBranch, BaseBranch: "main", BaseSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", NormalizedTaskJSON: mustJSON(t, snapshot.Task), TaskHash: snapshot.TaskHash, State: state, CandidateHead: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", WorktreePath: "/owned/worktree", ArtifactRoot: "/owned/artifacts"})
	run.RepositoryConfigJSON = mustJSON(t, repository)
	store := &pushTestStore{run: run}
	branchEvidence := `{"source_path":"/owned/source","origin_path":"/owned/origin","path":"` + run.WorktreePath + `","branch":"` + run.WorkingBranch + `","base_branch":"` + run.BaseBranch + `","base_sha":"` + run.BaseSHA + `","nonce":"nonce"}`
	store.resources = []OwnedResource{{RunID: run.ID, Kind: "branch", Name: run.WorkingBranch, CreationEvidence: branchEvidence, Status: "owned"}}
	store.admissionStore = admissionStore{serviceStore: serviceStore{run: run}}
	admission, err := NewLinearAdmissionService(reader, admissionResolver{repositories: map[string]LocalRepository{"owner/repo": repository}}, store, &serviceController{run: run})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewProductionCoordinator(admission, &serviceController{run: run}, store)
	if err != nil {
		t.Fatal(err)
	}
	return coordinator, store, run
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

type pullRequestOpener struct {
	requests []PullRequestOpenRequest
	response domain.PullRequest
	err      error
}

func (o *pullRequestOpener) OpenPullRequest(_ context.Context, request PullRequestOpenRequest) (domain.PullRequest, error) {
	o.requests = append(o.requests, request)
	return o.response, o.err
}

func ownedPullRequest(request PullRequestOpenRequest) domain.PullRequest {
	return domain.PullRequest{Number: 7, DatabaseID: 70, URL: "https://example.invalid/pull/7", NodeID: "PR_7", HeadBranch: request.HeadBranch, BaseBranch: request.BaseBranch, HeadSHA: request.CandidateSHA, BaseSHA: request.BaseSHA, BodyDigest: request.BodyDigest, OwnershipKey: request.OwnershipKey, State: "open"}
}

func TestProductionOpenPullRequestPersistsIntentBeforeOneOwnedPR(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateBranchPushed)
	request, err := pullRequestIntent(run)
	if err != nil {
		t.Fatal(err)
	}
	opener := &pullRequestOpener{response: ownedPullRequest(request)}
	result, err := coordinator.OpenPullRequest(context.Background(), ProductionOpenPullRequestCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, &pushValidator{}, opener)
	if err != nil || result.Action != ProductionReconcileGitHub || result.PullRequest != 7 || result.Idempotent || len(opener.requests) != 1 || store.run.State != domain.StatePROpen || store.pr == nil {
		t.Fatalf("result=%+v err=%v requests=%d state=%s pr=%+v", result, err, len(opener.requests), store.run.State, store.pr)
	}
	if store.side.Kind != "open_pull_request" || store.side.Status != "observed" || !strings.Contains(store.side.IntentJSON, `"candidate_sha":"`+run.CandidateHead+`"`) || strings.Contains(store.side.IntentJSON, "/owned/") {
		t.Fatalf("side=%+v", store.side)
	}
}

func TestProductionOpenPullRequestRecoversPersistedPRWithoutSecondWrite(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateOpeningPR)
	request, err := pullRequestIntent(run)
	if err != nil {
		t.Fatal(err)
	}
	pr := ownedPullRequest(request)
	store.pr = &pr
	opener := &pullRequestOpener{}
	result, err := coordinator.OpenPullRequest(context.Background(), ProductionOpenPullRequestCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, &pushValidator{}, opener)
	if err != nil || !result.Idempotent || len(opener.requests) != 0 || store.run.State != domain.StatePROpen {
		t.Fatalf("result=%+v err=%v requests=%d state=%s", result, err, len(opener.requests), store.run.State)
	}
}

func TestProductionOpenPullRequestReusesOwnedPullRequestAfterRepair(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateBranchPushed)
	pr := ownedPullRequest(PullRequestOpenRequest{HeadBranch: run.WorkingBranch, BaseBranch: run.BaseBranch, CandidateSHA: run.CandidateHead, BaseSHA: run.BaseSHA, BodyDigest: "initial-body", OwnershipKey: run.IdempotencyKey})
	store.pr = &pr
	opener := &pullRequestOpener{}
	result, err := coordinator.OpenPullRequest(context.Background(), ProductionOpenPullRequestCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, &pushValidator{}, opener)
	if err != nil || !result.Idempotent || len(opener.requests) != 0 || store.run.State != domain.StatePROpen || store.pr == nil || store.pr.BodyDigest != "initial-body" {
		t.Fatalf("result=%+v err=%v requests=%d state=%s pr=%+v", result, err, len(opener.requests), store.run.State, store.pr)
	}
}

func TestProductionOpenPullRequestRejectsMismatchedResponseAndPreservesRetryEvidence(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateBranchPushed)
	request, err := pullRequestIntent(run)
	if err != nil {
		t.Fatal(err)
	}
	pr := ownedPullRequest(request)
	pr.BodyDigest = "wrong"
	opener := &pullRequestOpener{response: pr}
	_, err = coordinator.OpenPullRequest(context.Background(), ProductionOpenPullRequestCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, &pushValidator{}, opener)
	if err == nil || store.run.State != domain.StateManualIntervention || store.side.Status != "failed" || store.pr != nil {
		t.Fatalf("err=%v state=%s side=%+v pr=%+v", err, store.run.State, store.side, store.pr)
	}
}

func TestProductionOpenPullRequestLeavesFailedIntentForExplicitReconciliation(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateBranchPushed)
	opener := &pullRequestOpener{err: errors.New("interrupted request")}
	_, err := coordinator.OpenPullRequest(context.Background(), ProductionOpenPullRequestCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, &pushValidator{}, opener)
	if err == nil || store.run.State != domain.StateOpeningPR || store.side.Status != "failed" || store.pr != nil {
		t.Fatalf("err=%v state=%s side=%+v pr=%+v", err, store.run.State, store.side, store.pr)
	}
}

type mergeReader struct {
	authority    GitHubInstallationMetadata
	evidence     domain.GitHubReadEvidence
	handoff      domain.InlineReviewBodyHandoff
	observations []GitHubRequestObservation
	err          error
	errorAt      int
	errorHandoff domain.InlineReviewBodyHandoff
	calls        int
}

func (r *mergeReader) Authority() GitHubInstallationMetadata { return r.authority }
func (r *mergeReader) Read(_ context.Context, _ int64, _ string) (domain.GitHubReadEvidence, domain.InlineReviewBodyHandoff, []GitHubRequestObservation, GitHubInstallationMetadata, error) {
	r.calls++
	if r.err != nil && (r.errorAt == 0 || r.errorAt == r.calls) {
		return r.evidence, r.errorHandoff, append([]GitHubRequestObservation(nil), r.observations...), r.authority, r.err
	}
	return r.evidence, r.handoff, append([]GitHubRequestObservation(nil), r.observations...), r.authority, nil
}

type mergeWriter struct {
	response domain.PullRequest
	err      error
	calls    int
}

func (w *mergeWriter) SquashMerge(_ context.Context, _ SquashMergeRequest) (domain.PullRequest, []GitHubRequestObservation, GitHubInstallationMetadata, error) {
	w.calls++
	return w.response, []GitHubRequestObservation{{Operation: "squash_merge", Category: "pull_request", ResponseDigest: "digest", ObservedAt: time.Now().UTC()}}, GitHubInstallationMetadata{AppID: 1, InstallationID: 2, Repository: domain.RepositoryIdentity{ID: 99, Owner: "owner", Name: "repo"}}, w.err
}

func newMergeCoordinator(t *testing.T) (*ProductionCoordinator, *pushTestStore, Run, *mergeReader, *mergeWriter) {
	t.Helper()
	coordinator, store, run := newPushCoordinator(t, domain.StateMerging)
	pr := domain.PullRequest{Number: 7, DatabaseID: 70, URL: "https://example.invalid/pull/7", NodeID: "PR_7", HeadBranch: run.WorkingBranch, BaseBranch: run.BaseBranch, HeadSHA: run.CandidateHead, BaseSHA: run.BaseSHA, BodyDigest: "body", OwnershipKey: run.IdempotencyKey, State: "open"}
	now := time.Date(2026, 7, 13, 2, 0, 0, 0, time.UTC)
	actor := domain.ActorIdentity{DatabaseID: 33, NodeID: "USER_33", Login: "ifan0927", Type: "User"}
	approval := domain.HumanApproval{PRNumber: pr.Number, Approver: actor.Login, Actor: actor, ReviewDatabaseID: 9, ReviewNodeID: "PRR_9", Source: "github_pull_request_review", ApprovedSHA: run.CandidateHead, CIStatus: "pass", ReviewSHA: run.CandidateHead, ApprovedAt: now, ObservedAt: now}
	binding := &SanitizedRepositoryBinding{CanonicalRepository: "owner/repo", ExpectedRepositoryID: 99, GitHubAppID: 1, GitHubInstallationID: 2, TrustedOperatorActors: []TrustedActorIdentity{{DatabaseID: actor.DatabaseID, NodeID: actor.NodeID, Login: actor.Login, Type: actor.Type}}}
	evidence := domain.GitHubReadEvidence{Repository: domain.RepositoryIdentity{ID: 99, NodeID: "REPO", Owner: "owner", Name: "repo"}, PullRequest: pr, Checks: []domain.GitHubCheck{{Name: "test", Required: true, ObservedSHA: run.CandidateHead, State: domain.CheckSuccess}}, Reviews: []domain.GitHubReview{{DatabaseID: 9, NodeID: "PRR_9", State: "APPROVED", CommitSHA: run.CandidateHead, SourceAt: now, Actor: actor}}, ObservedAt: now}
	store.pr = &pr
	store.inspection = RunInspection{Run: run, RepositoryBinding: binding, PullRequest: &pr, Approval: &approval}
	reader := &mergeReader{authority: GitHubInstallationMetadata{AppID: 1, InstallationID: 2, Repository: evidence.Repository}, evidence: evidence, observations: []GitHubRequestObservation{{Operation: "merge_preflight", Category: "pull_request", ResponseDigest: "digest", ObservedAt: time.Now().UTC()}}}
	merged := pr
	merged.State, merged.Merged, merged.MergeSHA, merged.MergedAt = "closed", true, "merge", now.Add(time.Minute)
	writer := &mergeWriter{response: merged}
	return coordinator, store, run, reader, writer
}

func mergeCommand(run Run) ProductionMergeCommand {
	return ProductionMergeCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}
}

func TestProductionMergePersistsIntentAndObservedSquashResult(t *testing.T) {
	coordinator, store, run, reader, writer := newMergeCoordinator(t)
	result, err := coordinator.MergePullRequest(context.Background(), mergeCommand(run), &pushValidator{}, reader, writer)
	if err != nil || result.Action != ProductionStop || result.MergeSHA != "merge" || result.Idempotent || writer.calls != 1 || store.run.State != domain.StateAwaitingLinearCompletion || store.merge == nil {
		t.Fatalf("result=%+v err=%v calls=%d state=%s merge=%+v", result, err, writer.calls, store.run.State, store.merge)
	}
	if store.side.Kind != "squash_merge" || store.side.Status != "observed" || !strings.Contains(store.side.IntentJSON, `"merge_method":"squash"`) || len(store.github) != 1 || len(store.requests) != 2 {
		t.Fatalf("side=%+v github=%d requests=%d", store.side, len(store.github), len(store.requests))
	}
}

func TestProductionMergeReconcilesLostSuccessWithoutSecondWrite(t *testing.T) {
	coordinator, store, run, reader, writer := newMergeCoordinator(t)
	writer.err = errors.New("response lost after GitHub accepted merge")
	if _, err := coordinator.MergePullRequest(context.Background(), mergeCommand(run), &pushValidator{}, reader, writer); err == nil || writer.calls != 1 || store.run.State != domain.StateMerging || store.side.Status != "failed" {
		t.Fatalf("err=%v calls=%d state=%s side=%+v", err, writer.calls, store.run.State, store.side)
	}
	merged := writer.response
	reader.evidence.PullRequest = merged
	result, err := coordinator.MergePullRequest(context.Background(), mergeCommand(store.run), &pushValidator{}, reader, writer)
	if err != nil || !result.Idempotent || writer.calls != 1 || store.run.State != domain.StateAwaitingLinearCompletion || store.merge == nil || store.merge.MergeSHA != "merge" {
		t.Fatalf("result=%+v err=%v calls=%d state=%s merge=%+v", result, err, writer.calls, store.run.State, store.merge)
	}
}

func TestProductionLinearCompletionPersistsExactMergeBoundCompletedEvidence(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateAwaitingLinearCompletion)
	mergedAt := time.Date(2026, 7, 13, 3, 0, 0, 0, time.UTC)
	store.merge = &MergeRecord{RunID: run.ID, PRNumber: 7, PreMergeSHA: run.CandidateHead, BaseSHA: run.BaseSHA, Method: "squash", MergeSHA: "merge", MergedAt: mergedAt}
	source := validLinearSource()
	source.State = LinearState{ID: "done", Name: "Done", Type: "completed"}
	source.UpdatedAt, source.ObservedAt = mergedAt.Add(time.Minute), mergedAt.Add(2*time.Minute)
	source.SourceRevision = source.UpdatedAt.Format(time.RFC3339Nano)
	coordinator.admission.reader = &admissionReader{source: source}
	result, err := coordinator.ReconcileLinearCompletion(context.Background(), ProductionLinearCompletionCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey})
	if err != nil || result.Action != ProductionStop || result.Status != LinearCompletionCompleted || store.run.State != domain.StateCleaning || len(store.linearCompletion) != 1 {
		t.Fatalf("result=%+v err=%v state=%s observations=%+v", result, err, store.run.State, store.linearCompletion)
	}
	got := store.linearCompletion[0]
	if got.MergeSHA != "merge" || got.LinearIssueID != "linear-id" || got.Status != LinearCompletionCompleted || got.SourceRevision != source.SourceRevision {
		t.Fatalf("completion observation=%+v", got)
	}
}

func TestProductionCleanupRequiresCompletedLinearEvidenceAndFinishesOwnedResources(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateCleaning)
	mergedAt := time.Date(2026, 7, 13, 3, 0, 0, 0, time.UTC)
	store.merge = &MergeRecord{RunID: run.ID, PRNumber: 7, PreMergeSHA: run.CandidateHead, BaseSHA: run.BaseSHA, Method: "squash", MergeSHA: "merge", MergedAt: mergedAt}
	evidence := `{"source_path":"/owned/source","origin_path":"/owned/origin","path":"` + run.WorktreePath + `","branch":"` + run.WorkingBranch + `","base_branch":"` + run.BaseBranch + `","base_sha":"` + run.BaseSHA + `","nonce":"nonce"}`
	artifact := `{"path":"/owned/artifacts","attempts_path":"/owned/artifacts/attempts","run_root":"/owned","nonce":"artifact-nonce","task_hash":"` + run.TaskHash + `"}`
	store.resources = []OwnedResource{{RunID: run.ID, Kind: "artifact_root", Name: run.ArtifactRoot, Status: "owned", CreationEvidence: artifact}, {RunID: run.ID, Kind: "worktree", Name: run.WorktreePath, Status: "owned", CreationEvidence: evidence}, {RunID: run.ID, Kind: "branch", Name: run.WorkingBranch, Status: "owned", CreationEvidence: evidence}, {RunID: run.ID, Kind: "remote_branch", Name: run.WorkingBranch, Status: "owned", CreationEvidence: evidence}}
	store.linearCompletion = []LinearCompletionObservation{{RunID: run.ID, MergeSHA: "merge", Status: LinearCompletionCompleted, ObservedAt: mergedAt.Add(time.Minute)}}
	port := &fakeCleanup{}
	source := &fakeSourceSync{result: SourceSyncResult{Status: SourceSyncSynced, Outcome: SourceSyncAlreadyAtTarget, MergeSHA: "merge"}}
	result, err := coordinator.Cleanup(context.Background(), ProductionCleanupCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, port, source)
	if err != nil || result.Action != ProductionStop || store.run.State != domain.StateCompleted || len(port.calls) != 3 {
		t.Fatalf("result=%+v err=%v state=%s calls=%v", result, err, store.run.State, port.calls)
	}
	if len(store.cleanup) != 5 || store.cleanup[0].Kind != "source_checkout" || store.cleanup[0].Status != "synced" || store.cleanup[1].Status != "retained" {
		t.Fatalf("cleanup=%+v", store.cleanup)
	}
	for _, resource := range store.resources {
		if resource.Kind == "source_checkout" {
			t.Fatalf("source checkout must not become an owned resource: %+v", store.resources)
		}
	}
}

func TestProductionCleanupRejectsMissingLinearCompletionWithoutCallingAdapter(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateCleaning)
	store.merge = &MergeRecord{RunID: run.ID, PreMergeSHA: run.CandidateHead, Method: "squash", MergeSHA: "merge", MergedAt: time.Now().UTC()}
	port := &fakeCleanup{}
	source := &fakeSourceSync{result: SourceSyncResult{Status: SourceSyncSynced, Outcome: SourceSyncAlreadyAtTarget, MergeSHA: "merge"}}
	_, err := coordinator.Cleanup(context.Background(), ProductionCleanupCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, port, source)
	if err == nil || len(port.calls) != 0 || len(source.calls) != 0 || store.run.State != domain.StateCleaning {
		t.Fatalf("err=%v cleanup=%v source=%v state=%s", err, port.calls, source.calls, store.run.State)
	}
}

func TestProductionCleanupStopsBeforeOwnedDeletesWhenSourceResultPersistenceFails(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateCleaning)
	mergedAt := time.Date(2026, 7, 13, 3, 0, 0, 0, time.UTC)
	store.merge = &MergeRecord{RunID: run.ID, PreMergeSHA: run.CandidateHead, Method: "squash", MergeSHA: "merge", MergedAt: mergedAt}
	evidence := `{"source_path":"/owned/source","origin_path":"/owned/origin","path":"` + run.WorktreePath + `","branch":"` + run.WorkingBranch + `","base_branch":"` + run.BaseBranch + `","base_sha":"` + run.BaseSHA + `","nonce":"nonce"}`
	artifact := `{"path":"/owned/artifacts","attempts_path":"/owned/artifacts/attempts","run_root":"/owned","nonce":"artifact-nonce","task_hash":"` + run.TaskHash + `"}`
	store.resources = []OwnedResource{{RunID: run.ID, Kind: "artifact_root", Name: run.ArtifactRoot, Status: "owned", CreationEvidence: artifact}, {RunID: run.ID, Kind: "worktree", Name: run.WorktreePath, Status: "owned", CreationEvidence: evidence}, {RunID: run.ID, Kind: "branch", Name: run.WorkingBranch, Status: "owned", CreationEvidence: evidence}, {RunID: run.ID, Kind: "remote_branch", Name: run.WorkingBranch, Status: "owned", CreationEvidence: evidence}}
	store.linearCompletion = []LinearCompletionObservation{{RunID: run.ID, MergeSHA: "merge", Status: LinearCompletionCompleted, ObservedAt: mergedAt.Add(time.Minute)}}
	store.cleanupFailAt = 2
	cleanup := &fakeCleanup{}
	source := &fakeSourceSync{result: SourceSyncResult{Status: SourceSyncSynced, Outcome: SourceSyncFastForwarded, MergeSHA: "merge"}}
	_, err := coordinator.Cleanup(context.Background(), ProductionCleanupCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, cleanup, source)
	if err == nil || len(source.calls) != 1 || len(cleanup.calls) != 0 || len(store.cleanup) != 1 || store.cleanup[0].Status != "intent" {
		t.Fatalf("err=%v source=%+v owned=%+v cleanup=%+v", err, source.calls, cleanup.calls, store.cleanup)
	}
}

type fakeSourceSync struct {
	result SourceSyncResult
	err    error
	calls  []SourceSyncRequest
}

func (f *fakeSourceSync) Sync(_ context.Context, request SourceSyncRequest) (SourceSyncResult, error) {
	f.calls = append(f.calls, request)
	return f.result, f.err
}

func TestProductionLinearCompletionTimesOutWithoutFabricatingSuccess(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateAwaitingLinearCompletion)
	mergedAt := time.Date(2026, 7, 13, 3, 0, 0, 0, time.UTC)
	store.merge = &MergeRecord{RunID: run.ID, PRNumber: 7, PreMergeSHA: run.CandidateHead, BaseSHA: run.BaseSHA, Method: "squash", MergeSHA: "merge", MergedAt: mergedAt}
	source := validLinearSource()
	source.State = LinearState{ID: "started", Name: "In Progress", Type: "started"}
	source.UpdatedAt, source.ObservedAt = mergedAt.Add(time.Minute), mergedAt.Add(time.Minute)
	source.SourceRevision = source.UpdatedAt.Format(time.RFC3339Nano)
	coordinator.admission.reader = &admissionReader{source: source}
	for attempt := 1; attempt <= MaxLinearCompletionObservations; attempt++ {
		result, err := coordinator.ReconcileLinearCompletion(context.Background(), ProductionLinearCompletionCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: store.run.State, IdempotencyKey: run.IdempotencyKey})
		if err != nil {
			t.Fatal(err)
		}
		if attempt < MaxLinearCompletionObservations && (result.Status != LinearCompletionPending || store.run.State != domain.StateAwaitingLinearCompletion) {
			t.Fatalf("attempt=%d result=%+v state=%s", attempt, result, store.run.State)
		}
	}
	if store.run.State != domain.StateManualIntervention || len(store.linearCompletion) != MaxLinearCompletionObservations || store.linearCompletion[len(store.linearCompletion)-1].Status != LinearCompletionTimeout {
		t.Fatalf("state=%s observations=%+v", store.run.State, store.linearCompletion)
	}
}

func TestProductionLinearCompletionRejectsCanceledIssue(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateAwaitingLinearCompletion)
	mergedAt := time.Date(2026, 7, 13, 3, 0, 0, 0, time.UTC)
	store.merge = &MergeRecord{RunID: run.ID, PRNumber: 7, PreMergeSHA: run.CandidateHead, BaseSHA: run.BaseSHA, Method: "squash", MergeSHA: "merge", MergedAt: mergedAt}
	source := validLinearSource()
	source.State = LinearState{ID: "cancelled", Name: "Canceled", Type: "canceled"}
	source.UpdatedAt, source.ObservedAt = mergedAt.Add(time.Minute), mergedAt.Add(time.Minute)
	source.SourceRevision = source.UpdatedAt.Format(time.RFC3339Nano)
	coordinator.admission.reader = &admissionReader{source: source}
	result, err := coordinator.ReconcileLinearCompletion(context.Background(), ProductionLinearCompletionCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey})
	if err != nil || result.Status != LinearCompletionCanceled || store.run.State != domain.StateManualIntervention {
		t.Fatalf("result=%+v err=%v state=%s", result, err, store.run.State)
	}
}

func TestProductionLinearCompletionRejectsWrongPersistedIssueIdentity(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateAwaitingLinearCompletion)
	mergedAt := time.Date(2026, 7, 13, 3, 0, 0, 0, time.UTC)
	store.merge = &MergeRecord{RunID: run.ID, PRNumber: 7, PreMergeSHA: run.CandidateHead, BaseSHA: run.BaseSHA, Method: "squash", MergeSHA: "merge", MergedAt: mergedAt}
	source := validLinearSource()
	source.IssueID = "different-linear-id"
	source.State = LinearState{ID: "done", Name: "Done", Type: "completed"}
	source.UpdatedAt, source.ObservedAt = mergedAt.Add(time.Minute), mergedAt.Add(time.Minute)
	source.SourceRevision = source.UpdatedAt.Format(time.RFC3339Nano)
	coordinator.admission.reader = &admissionReader{source: source}
	result, err := coordinator.ReconcileLinearCompletion(context.Background(), ProductionLinearCompletionCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey})
	if err != nil || result.Status != LinearCompletionInvalid || store.run.State != domain.StateManualIntervention {
		t.Fatalf("result=%+v err=%v state=%s", result, err, store.run.State)
	}
}

func TestProductionMergeClosedUnmergedFailsClosedBeforeWrite(t *testing.T) {
	coordinator, store, run, reader, writer := newMergeCoordinator(t)
	reader.evidence.PullRequest.State = "closed"
	if _, err := coordinator.MergePullRequest(context.Background(), mergeCommand(run), &pushValidator{}, reader, writer); err == nil || writer.calls != 0 || store.run.State != domain.StateManualIntervention {
		t.Fatalf("err=%v calls=%d state=%s", err, writer.calls, store.run.State)
	}
}

func TestProductionMergeRejectedResultPersistsGitHubTelemetry(t *testing.T) {
	coordinator, store, run, reader, writer := newMergeCoordinator(t)
	writer.err = &MergeRejectedError{Cause: errors.New("GitHub rejected merge")}
	if _, err := coordinator.MergePullRequest(context.Background(), mergeCommand(run), &pushValidator{}, reader, writer); err == nil || writer.calls != 1 || store.run.State != domain.StateManualIntervention || len(store.requests) != 2 {
		t.Fatalf("err=%v calls=%d state=%s requests=%d", err, writer.calls, store.run.State, len(store.requests))
	}
}

func TestProductionMergeRejectedResultFailsClosedEvenWhenRequestTelemetryCannotPersist(t *testing.T) {
	coordinator, store, run, reader, writer := newMergeCoordinator(t)
	reader.observations = nil
	store.requestErr = errors.New("telemetry storage unavailable")
	writer.err = &MergeRejectedError{Cause: errors.New("GitHub rejected merge")}
	if _, err := coordinator.MergePullRequest(context.Background(), mergeCommand(run), &pushValidator{}, reader, writer); err == nil || writer.calls != 1 || store.run.State != domain.StateManualIntervention || store.side.Status != "failed" {
		t.Fatalf("err=%v calls=%d state=%s side=%+v", err, writer.calls, store.run.State, store.side)
	}
}

func TestProductionMergeabilityWaitPollsWithoutMergeAndRetriesOnlyAfterResolution(t *testing.T) {
	coordinator, store, run, reader, _ := newMergeCoordinator(t)
	store.run.State, run.State = domain.StateAwaitingGitHubMergeability, domain.StateAwaitingGitHubMergeability
	configureMergeabilityFixture(t, store, run, reader)
	setMergePolicyPendingSide(t, store, run, reader)

	command := ProductionReconcileCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}
	for attempt := 0; attempt < 2; attempt++ {
		result, err := coordinator.ReconcileGitHub(context.Background(), command, reader)
		if err != nil || result.Action != ProductionReconcileGitHub || store.run.State != domain.StateAwaitingGitHubMergeability {
			t.Fatalf("attempt=%d result=%+v err=%v state=%s last=%q transitions=%+v", attempt, result, err, store.run.State, store.run.LastError, store.transitions)
		}
	}
	if reader.calls != 2 || len(store.transitions) != 0 {
		t.Fatalf("wait must only reread GitHub: calls=%d transitions=%+v", reader.calls, store.transitions)
	}

	reader.evidence.ReviewThreads[0].Resolved = true
	reader.evidence.ObservedAt = reader.evidence.ObservedAt.Add(time.Minute)
	result, err := coordinator.ReconcileGitHub(context.Background(), command, reader)
	if err != nil || result.Action != ProductionMerge || store.run.State != domain.StateMerging || reader.calls != 3 {
		t.Fatalf("result=%+v err=%v state=%s calls=%d", result, err, store.run.State, reader.calls)
	}
}

func TestProductionMergePolicyResolutionWithAdvancedObservationRestartsAndRetriesOnce(t *testing.T) {
	coordinator, store, run, reader, writer := newMergeCoordinator(t)
	configureMergeabilityFixture(t, store, run, reader)
	writer.err = &MergeRejectedError{HTTPStatus: 409, Operation: "squash_merge_pull_request", Cause: errors.New("merge protection rejected unresolved thread")}
	if _, err := coordinator.MergePullRequest(context.Background(), mergeCommand(run), &pushValidator{}, reader, writer); err == nil || store.run.State != domain.StateAwaitingGitHubMergeability || writer.calls != 1 {
		t.Fatalf("initial protected rejection err=%v state=%s writes=%d", err, store.run.State, writer.calls)
	}

	// A recovered process must only read while waiting. The later resolution
	// observation is distinct from, and newer than, the rejected observation.
	store.inspection.Run = store.run
	restarted, err := NewProductionCoordinator(coordinator.admission, coordinator.controller, store)
	if err != nil {
		t.Fatal(err)
	}
	reader.evidence.ReviewThreads[0].Resolved = true
	reader.evidence.ObservedAt = reader.evidence.ObservedAt.Add(time.Minute)
	command := ProductionReconcileCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: store.run.State, IdempotencyKey: run.IdempotencyKey}
	result, err := restarted.ReconcileGitHub(context.Background(), command, reader)
	if err != nil || result.Action != ProductionMerge || store.run.State != domain.StateMerging || writer.calls != 1 {
		t.Fatalf("resolution result=%+v err=%v state=%s writes=%d", result, err, store.run.State, writer.calls)
	}

	writer.err = nil
	store.inspection.Run = store.run
	merged, err := restarted.MergePullRequest(context.Background(), mergeCommand(store.run), &pushValidator{}, reader, writer)
	if err != nil || merged.Action != ProductionStop || store.run.State != domain.StateAwaitingLinearCompletion || writer.calls != 2 || store.side.Attempt != 2 || store.side.Status != "observed" {
		t.Fatalf("guarded retry result=%+v err=%v state=%s writes=%d side=%+v", merged, err, store.run.State, writer.calls, store.side)
	}
}

func TestProductionMergeabilityResolutionDriftRemainsManual(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*mergeReader)
	}{
		{name: "topology", mutate: func(reader *mergeReader) {
			followup := domain.GitHubReviewComment{DatabaseID: 12, NodeID: "FOLLOWUP", ReplyToDatabaseID: 10, ReplyToNodeID: "ROOT", BodyDigest: domain.TrustedReviewFeedbackDigest("untrusted follow-up"), CreatedAt: reader.evidence.ObservedAt, UpdatedAt: reader.evidence.ObservedAt}
			reader.evidence.ReviewThreads[0].Comments = append(reader.evidence.ReviewThreads[0].Comments, followup)
			reader.handoff.Comments = append(reader.handoff.Comments, domain.InlineReviewBody{ThreadNodeID: reader.evidence.ReviewThreads[0].NodeID, CommentNodeID: followup.NodeID, Body: "untrusted follow-up", BodyDigest: followup.BodyDigest})
		}},
		{name: "actor identity", mutate: func(reader *mergeReader) {
			actor := *reader.evidence.ReviewThreads[0].Comments[0].Author
			actor.NodeID = "LOOKALIKE"
			reader.evidence.ReviewThreads[0].Comments[0].Author = &actor
		}},
		{name: "root identity", mutate: func(reader *mergeReader) {
			reader.evidence.ReviewThreads[0].Comments[0].NodeID = "OTHER_ROOT"
		}},
		{name: "reply identity", mutate: func(reader *mergeReader) {
			reader.evidence.ReviewThreads[0].Comments[1].NodeID = "OTHER_REPLY"
		}},
		{name: "reply body", mutate: func(reader *mergeReader) {
			reader.handoff.Comments[1].Body = "changed reply body"
			reader.handoff.Comments[1].BodyDigest = domain.TrustedReviewFeedbackDigest(reader.handoff.Comments[1].Body)
			reader.evidence.ReviewThreads[0].Comments[1].BodyDigest = reader.handoff.Comments[1].BodyDigest
		}},
		{name: "outdated", mutate: func(reader *mergeReader) {
			reader.evidence.ReviewThreads[0].Outdated = true
		}},
		{name: "head", mutate: func(reader *mergeReader) {
			reader.evidence.PullRequest.HeadSHA = strings.Repeat("b", 40)
		}},
		{name: "base", mutate: func(reader *mergeReader) {
			reader.evidence.PullRequest.BaseBranch = "release"
			reader.evidence.PullRequest.BaseSHA = strings.Repeat("c", 40)
		}},
		{name: "ownership", mutate: func(reader *mergeReader) {
			reader.evidence.PullRequest.OwnershipKey = "different-controller"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			coordinator, store, run, reader, writer := newMergeCoordinator(t)
			store.run.State, run.State = domain.StateAwaitingGitHubMergeability, domain.StateAwaitingGitHubMergeability
			configureMergeabilityFixture(t, store, run, reader)
			setMergePolicyPendingSide(t, store, run, reader)
			store.inspection.Run = store.run
			reader.evidence.ReviewThreads[0].Resolved = true
			reader.evidence.ObservedAt = reader.evidence.ObservedAt.Add(time.Minute)
			tc.mutate(reader)

			_, err := coordinator.ReconcileGitHub(context.Background(), ProductionReconcileCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: store.run.State, IdempotencyKey: run.IdempotencyKey}, reader)
			if err == nil || store.run.State != domain.StateManualIntervention || writer.calls != 0 || store.pr == nil || *store.pr != *store.inspection.PullRequest {
				t.Fatalf("err=%v state=%s writes=%d", err, store.run.State, writer.calls)
			}
		})
	}
}

func TestProductionMergeabilityTargetDriftUsesSpecialPersistence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*mergeReader)
	}{
		{name: "base", mutate: func(reader *mergeReader) {
			reader.evidence.PullRequest.BaseBranch, reader.evidence.PullRequest.BaseSHA = "release", strings.Repeat("c", 40)
		}},
		{name: "head", mutate: func(reader *mergeReader) {
			reader.evidence.PullRequest.HeadBranch, reader.evidence.PullRequest.HeadSHA = "other-feature", strings.Repeat("b", 40)
		}},
		{name: "ownership", mutate: func(reader *mergeReader) { reader.evidence.PullRequest.OwnershipKey = "other-controller" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			coordinator, store, run, reader, writer := newMergeCoordinator(t)
			store.run.State, run.State = domain.StateAwaitingGitHubMergeability, domain.StateAwaitingGitHubMergeability
			configureMergeabilityFixture(t, store, run, reader)
			setMergePolicyPendingSide(t, store, run, reader)
			store.inspection.Run = store.run
			persisted := *store.pr
			store.genericReadErr = errors.New("generic persistence must not receive target-only drift")
			reader.evidence.ReviewThreads[0].Resolved = true
			reader.evidence.ObservedAt = reader.evidence.ObservedAt.Add(time.Minute)
			tc.mutate(reader)

			_, err := coordinator.ReconcileGitHub(context.Background(), ProductionReconcileCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: store.run.State, IdempotencyKey: run.IdempotencyKey}, reader)
			if err == nil || store.run.State != domain.StateManualIntervention || writer.calls != 0 || store.manualTargetCalls != 1 || store.genericReadCalls != 0 || store.pr == nil || *store.pr != persisted {
				t.Fatalf("target drift route err=%v state=%s writes=%d special=%d generic=%d persisted=%+v", err, store.run.State, writer.calls, store.manualTargetCalls, store.genericReadCalls, store.pr)
			}
		})
	}
}

func TestProductionMergeabilityNonTargetDriftUsesGenericPersistenceAndFails(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*mergeReader)
	}{
		{name: "repository", mutate: func(reader *mergeReader) { reader.evidence.Repository.ID = 100 }},
		{name: "body", mutate: func(reader *mergeReader) { reader.evidence.PullRequest.BodyDigest = "other-body" }},
		{name: "pull request identity", mutate: func(reader *mergeReader) { reader.evidence.PullRequest.URL = "https://example.invalid/pr/other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			coordinator, store, run, reader, writer := newMergeCoordinator(t)
			store.run.State, run.State = domain.StateAwaitingGitHubMergeability, domain.StateAwaitingGitHubMergeability
			configureMergeabilityFixture(t, store, run, reader)
			setMergePolicyPendingSide(t, store, run, reader)
			store.inspection.Run = store.run
			persisted := *store.pr
			store.genericReadErr = errors.New("generic persistence rejected immutable drift")
			reader.evidence.ReviewThreads[0].Resolved = true
			reader.evidence.ObservedAt = reader.evidence.ObservedAt.Add(time.Minute)
			tc.mutate(reader)

			_, err := coordinator.ReconcileGitHub(context.Background(), ProductionReconcileCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: store.run.State, IdempotencyKey: run.IdempotencyKey}, reader)
			if err == nil || store.run.State != domain.StateAwaitingGitHubMergeability || writer.calls != 0 || store.manualTargetCalls != 0 || store.genericReadCalls != 1 || store.pr == nil || *store.pr != persisted {
				t.Fatalf("non-target drift route err=%v state=%s writes=%d special=%d generic=%d persisted=%+v", err, store.run.State, writer.calls, store.manualTargetCalls, store.genericReadCalls, store.pr)
			}
		})
	}
}

func TestProductionMergeabilityResolutionRequiresNewerObservation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		observe func(time.Time) time.Time
	}{
		{name: "same observation", observe: func(observed time.Time) time.Time { return observed }},
		{name: "older observation", observe: func(observed time.Time) time.Time { return observed.Add(-time.Minute) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			coordinator, store, run, reader, writer := newMergeCoordinator(t)
			store.run.State, run.State = domain.StateAwaitingGitHubMergeability, domain.StateAwaitingGitHubMergeability
			configureMergeabilityFixture(t, store, run, reader)
			setMergePolicyPendingSide(t, store, run, reader)
			store.inspection.Run = store.run
			reader.evidence.ReviewThreads[0].Resolved = true
			reader.evidence.ObservedAt = tc.observe(reader.evidence.ObservedAt)

			_, err := coordinator.ReconcileGitHub(context.Background(), ProductionReconcileCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: store.run.State, IdempotencyKey: run.IdempotencyKey}, reader)
			if err == nil || store.run.State != domain.StateManualIntervention || writer.calls != 0 {
				t.Fatalf("err=%v state=%s writes=%d", err, store.run.State, writer.calls)
			}
		})
	}
}

func TestProductionMergePolicyRejectionEntersDurableReadOnlyWait(t *testing.T) {
	coordinator, store, run, reader, writer := newMergeCoordinator(t)
	configureMergeabilityFixture(t, store, run, reader)
	writer.err = &MergeRejectedError{HTTPStatus: 409, Operation: "squash_merge_pull_request", Cause: errors.New("merge protection rejected unresolved thread")}
	result, err := coordinator.MergePullRequest(context.Background(), mergeCommand(run), &pushValidator{}, reader, writer)
	if err == nil || result.Action != ProductionReconcileGitHub || writer.calls != 1 || reader.calls != 2 || store.run.State != domain.StateAwaitingGitHubMergeability {
		t.Fatalf("result=%+v err=%v writes=%d reads=%d state=%s", result, err, writer.calls, reader.calls, store.run.State)
	}
	if store.side.Status != "failed" || !strings.Contains(store.side.ResultJSON, `"category":"merge_policy_pending"`) {
		t.Fatalf("merge policy wait did not retain sanitized result: %+v", store.side)
	}
	if _, err := coordinator.MergePullRequest(context.Background(), mergeCommand(store.run), &pushValidator{}, reader, writer); err == nil || writer.calls != 1 {
		t.Fatalf("restart must not issue another merge while waiting: err=%v writes=%d", err, writer.calls)
	}
}

func TestProductionMergePolicyRetryReturnsToReadOnlyWaitWhenThreadReopens(t *testing.T) {
	coordinator, store, run, reader, writer := newMergeCoordinator(t)
	configureMergeabilityFixture(t, store, run, reader)
	writer.err = &MergeRejectedError{HTTPStatus: 409, Operation: "squash_merge_pull_request", Cause: errors.New("merge protection rejected unresolved thread")}
	if _, err := coordinator.MergePullRequest(context.Background(), mergeCommand(run), &pushValidator{}, reader, writer); err == nil || store.run.State != domain.StateAwaitingGitHubMergeability || writer.calls != 1 {
		t.Fatalf("initial wait err=%v state=%s writes=%d", err, store.run.State, writer.calls)
	}
	reader.evidence.ReviewThreads[0].Resolved = true
	reader.evidence.ObservedAt = reader.evidence.ObservedAt.Add(time.Minute)
	store.inspection.Run = store.run
	if result, err := coordinator.ReconcileGitHub(context.Background(), ProductionReconcileCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: store.run.State, IdempotencyKey: run.IdempotencyKey}, reader); err != nil || result.Action != ProductionMerge || store.run.State != domain.StateMerging {
		t.Fatalf("resolution result=%+v err=%v state=%s", result, err, store.run.State)
	}
	// The conversation can reopen after the resolution poll and before the
	// guarded retry's own fresh read. That retry must not issue a merge write.
	reader.evidence.ReviewThreads[0].Resolved = false
	store.inspection.Run = store.run
	result, err := coordinator.MergePullRequest(context.Background(), mergeCommand(store.run), &pushValidator{}, reader, writer)
	if err == nil || result.Action != ProductionReconcileGitHub || store.run.State != domain.StateAwaitingGitHubMergeability || writer.calls != 1 {
		t.Fatalf("result=%+v err=%v state=%s writes=%d", result, err, store.run.State, writer.calls)
	}
}

func TestProductionMergePolicyRetryFailsClosedWhenFollowupChangesTopology(t *testing.T) {
	coordinator, store, run, reader, writer := newMergeCoordinator(t)
	configureMergeabilityFixture(t, store, run, reader)
	writer.err = &MergeRejectedError{HTTPStatus: 409, Operation: "squash_merge_pull_request", Cause: errors.New("merge protection rejected unresolved thread")}
	if _, err := coordinator.MergePullRequest(context.Background(), mergeCommand(run), &pushValidator{}, reader, writer); err == nil || store.run.State != domain.StateAwaitingGitHubMergeability || writer.calls != 1 {
		t.Fatalf("initial wait err=%v state=%s writes=%d", err, store.run.State, writer.calls)
	}
	reader.evidence.ReviewThreads[0].Resolved = true
	reader.evidence.ObservedAt = reader.evidence.ObservedAt.Add(time.Minute)
	store.inspection.Run = store.run
	if _, err := coordinator.ReconcileGitHub(context.Background(), ProductionReconcileCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: store.run.State, IdempotencyKey: run.IdempotencyKey}, reader); err != nil || store.run.State != domain.StateMerging {
		t.Fatalf("resolution err=%v state=%s", err, store.run.State)
	}
	// A human follow-up is retained only as a digest. It changes the tracked
	// topology, so a retry must stop before claiming the merge side effect.
	followup := domain.GitHubReviewComment{DatabaseID: 12, NodeID: "FOLLOWUP", ReplyToDatabaseID: 10, ReplyToNodeID: "ROOT", BodyDigest: domain.TrustedReviewFeedbackDigest("untrusted follow-up"), CreatedAt: reader.evidence.ObservedAt, UpdatedAt: reader.evidence.ObservedAt}
	reader.evidence.ReviewThreads[0].Comments = append(reader.evidence.ReviewThreads[0].Comments, followup)
	reader.handoff.Comments = append(reader.handoff.Comments, domain.InlineReviewBody{ThreadNodeID: reader.evidence.ReviewThreads[0].NodeID, CommentNodeID: followup.NodeID, Body: "untrusted follow-up", BodyDigest: followup.BodyDigest})
	store.inspection.Run = store.run
	if _, err := coordinator.MergePullRequest(context.Background(), mergeCommand(store.run), &pushValidator{}, reader, writer); err == nil || store.run.State != domain.StateManualIntervention || writer.calls != 1 {
		t.Fatalf("topology drift err=%v state=%s writes=%d", err, store.run.State, writer.calls)
	}
}

func TestProductionMergePolicyPendingPersistenceFailureCannotBypassOnRestart(t *testing.T) {
	coordinator, store, run, reader, writer := newMergeCoordinator(t)
	configureMergeabilityFixture(t, store, run, reader)
	writer.err = &MergeRejectedError{HTTPStatus: 409, Operation: "squash_merge_pull_request", Cause: errors.New("merge protection rejected unresolved thread")}
	store.mergePolicyErr = errors.New("injected pending-policy transaction failure")
	if _, err := coordinator.MergePullRequest(context.Background(), mergeCommand(run), &pushValidator{}, reader, writer); err == nil || store.run.State != domain.StateMerging || store.side.Status != "intent" || writer.calls != 1 {
		t.Fatalf("failed persistence err=%v state=%s side=%+v writes=%d", err, store.run.State, store.side, writer.calls)
	}

	// Emulate a recovered controller process after the tracked thread resolves.
	// The original merge intent has no durable policy outcome, so it must be
	// escalated instead of issuing a second merge write.
	store.mergePolicyErr = nil
	reader.evidence.ReviewThreads[0].Resolved = true
	store.inspection.Run = store.run
	restarted, err := NewProductionCoordinator(coordinator.admission, coordinator.controller, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.MergePullRequest(context.Background(), mergeCommand(store.run), &pushValidator{}, reader, writer); err == nil || store.run.State != domain.StateManualIntervention || writer.calls != 1 {
		t.Fatalf("restart err=%v state=%s writes=%d", err, store.run.State, writer.calls)
	}
}

func TestProductionMergePolicyRereadFailureDoesNotValidateHandoffOrEnterManual(t *testing.T) {
	coordinator, store, run, reader, writer := newMergeCoordinator(t)
	configureMergeabilityFixture(t, store, run, reader)
	writer.err = &MergeRejectedError{HTTPStatus: 409, Operation: "squash_merge_pull_request", Cause: errors.New("merge protection rejected unresolved thread")}
	reader.err, reader.errorAt = errors.New("temporary GitHub read failure"), 2
	reader.errorHandoff = domain.InlineReviewBodyHandoff{Comments: []domain.InlineReviewBody{{ThreadNodeID: "", CommentNodeID: "", Body: "", BodyDigest: ""}}}
	if _, err := coordinator.MergePullRequest(context.Background(), mergeCommand(run), &pushValidator{}, reader, writer); err == nil || writer.calls != 1 || reader.calls != 2 || store.run.State != domain.StateMerging || store.side.Status != "failed" || !strings.Contains(store.side.ResultJSON, `"category":"merge_response_unavailable"`) {
		t.Fatalf("err=%v writes=%d reads=%d state=%s side=%+v", err, writer.calls, reader.calls, store.run.State, store.side)
	}
}

func TestProductionMergeabilityWaitExitsForApprovalAndAuthorityChanges(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mutate    func(*mergeReader)
		wantState domain.State
		wantError bool
	}{
		{name: "approval dismissed", mutate: func(reader *mergeReader) { reader.evidence.Reviews[0].State = "DISMISSED" }, wantState: domain.StateAwaitingHumanApproval},
		{name: "new change request", mutate: addExactHeadTrustedChangeRequest, wantState: domain.StateRepairing},
		{name: "head drift", mutate: func(reader *mergeReader) { reader.evidence.PullRequest.HeadSHA = strings.Repeat("b", 40) }, wantState: domain.StateManualIntervention, wantError: true},
		{name: "deleted reply", mutate: func(reader *mergeReader) {
			reader.evidence.ReviewThreads[0].Comments = reader.evidence.ReviewThreads[0].Comments[:1]
		}, wantState: domain.StateManualIntervention, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			coordinator, store, run, reader, _ := newMergeCoordinator(t)
			store.run.State, run.State = domain.StateAwaitingGitHubMergeability, domain.StateAwaitingGitHubMergeability
			configureMergeabilityFixture(t, store, run, reader)
			setMergePolicyPendingSide(t, store, run, reader)
			tc.mutate(reader)
			result, err := coordinator.ReconcileGitHub(context.Background(), ProductionReconcileCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, reader)
			if (err != nil) != tc.wantError || store.run.State != tc.wantState {
				t.Fatalf("result=%+v err=%v state=%s transitions=%+v feedback=%+v", result, err, store.run.State, store.transitions, store.inspection.TrustedFeedback)
			}
			if tc.wantState == domain.StateRepairing && result.Action != ProductionContinueLocal {
				t.Fatalf("new trusted change request must enter repair, result=%+v", result)
			}
			if tc.wantState == domain.StateRepairing && (len(store.savedFeedback) != 1 || store.savedFeedback[0].RootCommentNodeID != "NEW_ROOT") {
				t.Fatalf("new trusted change request was not normalized for repair: %+v", store.savedFeedback)
			}
		})
	}
}

func addExactHeadTrustedChangeRequest(reader *mergeReader) {
	now := reader.evidence.ObservedAt.Add(time.Minute)
	actor := reader.evidence.Reviews[0].Actor
	body := "new exact-head change request"
	digest := domain.TrustedReviewFeedbackDigest(body)
	review := domain.GitHubReview{DatabaseID: 44, NodeID: "NEW_REVIEW", State: "CHANGES_REQUESTED", CommitSHA: reader.evidence.PullRequest.HeadSHA, SourceAt: now, Actor: actor}
	reader.evidence.Reviews = []domain.GitHubReview{review}
	reader.evidence.ReviewThreads = append(reader.evidence.ReviewThreads, domain.GitHubReviewThread{NodeID: "NEW_THREAD", OriginalCommitSHA: reader.evidence.PullRequest.HeadSHA, Comments: []domain.GitHubReviewComment{{DatabaseID: 45, NodeID: "NEW_ROOT", Author: &actor, Review: review, BodyDigest: digest, CreatedAt: now, UpdatedAt: now}}})
	reader.handoff.Comments = append(reader.handoff.Comments, domain.InlineReviewBody{ThreadNodeID: "NEW_THREAD", CommentNodeID: "NEW_ROOT", Body: body, BodyDigest: digest})
}

func configureMergeabilityFixture(t *testing.T, store *pushTestStore, run Run, reader *mergeReader) {
	t.Helper()
	inspection, threadEvidence, handoff := mergePolicyThreadFixture(t)
	feedback := &inspection.TrustedFeedback[0]
	feedback.RunID = run.ID
	feedback.PRNumber, feedback.PRDatabaseID, feedback.PRNodeID = store.pr.Number, store.pr.DatabaseID, store.pr.NodeID
	feedback.OriginalReviewHeadSHA, feedback.BoundRepairHead = run.CandidateHead, run.CandidateHead
	inspection.ReviewReplies[0].RunID = run.ID
	inspection.ReviewReplies[0].PullRequestNumber, inspection.ReviewReplies[0].RepairedHead = store.pr.Number, run.CandidateHead
	marker, markerDigest, err := domain.ReviewReplyMarker(run.ID, feedback.PRNumber, feedback.ThreadNodeID, feedback.RootCommentDatabaseID, feedback.RootCommentNodeID, feedback.BodyDigest, feedback.BoundRepairHead)
	if err != nil {
		t.Fatal(err)
	}
	replyBody, err := domain.ReviewReplyBody(feedback.BoundRepairHead, marker)
	if err != nil {
		t.Fatal(err)
	}
	feedback.ReplyIntentKey, inspection.ReviewReplies[0].MarkerDigest = markerDigest, markerDigest
	handoff.Comments[1].Body = replyBody
	handoff.Comments[1].BodyDigest = domain.TrustedReviewFeedbackDigest(replyBody)
	threadEvidence.ReviewThreads[0].Comments[1].BodyDigest = handoff.Comments[1].BodyDigest
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	actor := feedback.Author
	approval := domain.HumanApproval{PRNumber: store.pr.Number, Approver: actor.Login, Actor: actor, ReviewDatabaseID: 9, ReviewNodeID: "APPROVAL", Source: "github_pull_request_review", ApprovedSHA: run.CandidateHead, CIStatus: "pass", ReviewSHA: run.CandidateHead, ApprovedAt: now, ObservedAt: now}
	store.inspection.Run = run
	store.inspection.TrustedFeedback, store.inspection.ReviewReplies = inspection.TrustedFeedback, inspection.ReviewReplies
	store.inspection.Approval = &approval
	store.inspection.RepositoryBinding.TrustedOperatorActors = []TrustedActorIdentity{{DatabaseID: actor.DatabaseID, NodeID: actor.NodeID, Login: actor.Login, Type: actor.Type}}
	threadEvidence.Repository, threadEvidence.PullRequest = reader.evidence.Repository, *store.pr
	threadEvidence.Checks = []domain.GitHubCheck{{Name: "test", Required: true, ObservedSHA: run.CandidateHead, State: domain.CheckSuccess}}
	threadEvidence.Reviews = []domain.GitHubReview{{DatabaseID: 9, NodeID: "APPROVAL", State: "APPROVED", CommitSHA: run.CandidateHead, SourceAt: now, Actor: actor}}
	threadEvidence.ObservedAt = now
	reader.evidence, reader.handoff = threadEvidence, handoff
	if threads, err := controllerRepliedMergeThreads(store.inspection, reader.evidence, reader.handoff); err != nil || len(threads) != 1 {
		t.Fatalf("fixture topology threads=%+v err=%v", threads, err)
	}
}

func setMergePolicyPendingSide(t *testing.T, store *pushTestStore, run Run, reader *mergeReader) {
	t.Helper()
	threads, err := controllerRepliedMergeThreads(store.inspection, reader.evidence, reader.handoff)
	if err != nil {
		t.Fatal(err)
	}
	result, err := mergePolicyPendingResult(run.CandidateHead, threads)
	if err != nil {
		t.Fatal(err)
	}
	store.side = SideEffectRecord{ID: 1, RunID: run.ID, Kind: "squash_merge", IdempotencyKey: run.CandidateHead, Status: "failed", ResultJSON: string(result), Attempt: 1}
}

func TestProductionPushReconcilesSameRemoteSHAWithoutInvokingGit(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateApprovalReady)
	validator := &pushValidator{}
	publisher := &pushPublisher{remotes: []string{run.CandidateHead}}
	result, err := coordinator.Push(context.Background(), ProductionPushCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, validator, publisher)
	if err != nil || !result.Idempotent || publisher.pushes != 0 || validator.calls != 1 || store.run.State != domain.StateBranchPushed {
		t.Fatalf("result=%+v err=%v pushes=%d validations=%d state=%s", result, err, publisher.pushes, validator.calls, store.run.State)
	}
	if store.side.Status != "observed" || len(store.transitions) != 2 || len(store.resources) != 2 || store.resources[1].CreationEvidence != store.resources[0].CreationEvidence {
		t.Fatalf("side=%+v transitions=%+v resources=%+v", store.side, store.transitions, store.resources)
	}
}

func TestProductionPushFastForwardsAnOwnedPullRequestBranch(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateApprovalReady)
	oldHead := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	pr := domain.PullRequest{Number: 7, DatabaseID: 70, URL: "https://example.invalid/pull/7", NodeID: "PR_7", HeadBranch: run.WorkingBranch, BaseBranch: run.BaseBranch, HeadSHA: oldHead, BaseSHA: run.BaseSHA, BodyDigest: "initial-body", OwnershipKey: run.IdempotencyKey, State: "open"}
	store.pr = &pr
	publisher := &pushPublisher{remotes: []string{oldHead, run.CandidateHead}, evidence: PushEvidence{RemoteRef: "refs/heads/" + run.WorkingBranch, SHA: run.CandidateHead}}
	result, err := coordinator.Push(context.Background(), ProductionPushCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, &pushValidator{}, publisher)
	if err != nil || result.Idempotent || publisher.pushes != 1 || publisher.expectedRemote != oldHead || store.run.State != domain.StateBranchPushed || store.pr == nil || store.pr.HeadSHA != run.CandidateHead {
		t.Fatalf("result=%+v err=%v pushes=%d expected=%s state=%s pr=%+v", result, err, publisher.pushes, publisher.expectedRemote, store.run.State, store.pr)
	}
}

func TestProductionRecoverOwnedPushRestoresOnlyOwnedPRPushGate(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateManualIntervention)
	oldHead := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	pr := domain.PullRequest{Number: 7, DatabaseID: 70, URL: "https://example.invalid/pull/7", NodeID: "PR_7", HeadBranch: run.WorkingBranch, BaseBranch: run.BaseBranch, HeadSHA: oldHead, BaseSHA: run.BaseSHA, BodyDigest: "initial-body", OwnershipKey: run.IdempotencyKey, State: "open"}
	store.pr = &pr

	result, err := coordinator.RecoverOwnedPush(context.Background(), ProductionRecoverOwnedPushCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: domain.StateManualIntervention, IdempotencyKey: run.IdempotencyKey})
	if err != nil || result.Action != ProductionPush || store.run.State != domain.StateApprovalReady || len(store.transitions) != 1 {
		t.Fatalf("result=%+v err=%v state=%s transitions=%+v", result, err, store.run.State, store.transitions)
	}
	transition := store.transitions[0]
	if transition.From != domain.StateManualIntervention || transition.To != domain.StateApprovalReady || transition.EvidenceReference != "recover_owned_push:7" || transition.BoundHead != run.CandidateHead {
		t.Fatalf("transition=%+v", transition)
	}
}

func TestProductionRecoverOwnedPushRejectsMissingOwnedPR(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateManualIntervention)
	_, err := coordinator.RecoverOwnedPush(context.Background(), ProductionRecoverOwnedPushCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: domain.StateManualIntervention, IdempotencyKey: run.IdempotencyKey})
	if err == nil || store.run.State != domain.StateManualIntervention || len(store.transitions) != 0 {
		t.Fatalf("err=%v state=%s transitions=%+v", err, store.run.State, store.transitions)
	}
}

func TestProductionPushSupportsPreNonceLocalBranchEvidence(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateApprovalReady)
	store.resources[0].CreationEvidence = `{"source_path":"/owned/source","origin_path":"/owned/origin","path":"` + run.WorktreePath + `","branch":"` + run.WorkingBranch + `","base_branch":"` + run.BaseBranch + `","base_sha":"` + run.BaseSHA + `"}`
	publisher := &pushPublisher{remotes: []string{run.CandidateHead}}
	result, err := coordinator.Push(context.Background(), ProductionPushCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, &pushValidator{}, publisher)
	if err != nil || !result.Idempotent || store.run.State != domain.StateBranchPushed || len(store.resources) != 2 || store.resources[1].CreationEvidence != store.resources[0].CreationEvidence {
		t.Fatalf("result=%+v err=%v state=%s resources=%+v", result, err, store.run.State, store.resources)
	}
}

func TestProductionPushRejectsDivergentRemoteSHA(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateApprovalReady)
	publisher := &pushPublisher{remotes: []string{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}
	_, err := coordinator.Push(context.Background(), ProductionPushCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, &pushValidator{}, publisher)
	if err == nil || publisher.pushes != 0 || store.run.State != domain.StateManualIntervention || store.side.Status != "failed" {
		t.Fatalf("err=%v pushes=%d state=%s side=%+v", err, publisher.pushes, store.run.State, store.side)
	}
}

func TestProductionPushReconcilesCrashAfterRemoteAcceptedPush(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateApprovalReady)
	publisher := &pushPublisher{remotes: []string{"", run.CandidateHead}, pushErr: errors.New("simulated controller crash after git accepted push"), evidence: PushEvidence{RemoteRef: "refs/heads/" + run.WorkingBranch, SHA: run.CandidateHead, ExitCode: -1}}
	result, err := coordinator.Push(context.Background(), ProductionPushCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, &pushValidator{}, publisher)
	if err != nil || !result.Idempotent || publisher.pushes != 1 || store.run.State != domain.StateBranchPushed || store.side.Status != "observed" {
		t.Fatalf("result=%+v err=%v pushes=%d state=%s side=%+v", result, err, publisher.pushes, store.run.State, store.side)
	}
}

func TestProductionPushRecoversIntentPersistedBeforeGit(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StatePushingBranch)
	publisher := &pushPublisher{remotes: []string{"", run.CandidateHead}, evidence: PushEvidence{RemoteRef: "refs/heads/" + run.WorkingBranch, SHA: run.CandidateHead}}
	result, err := coordinator.Push(context.Background(), ProductionPushCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, &pushValidator{}, publisher)
	if err != nil || result.Idempotent || publisher.pushes != 1 || store.run.State != domain.StateBranchPushed || store.side.Status != "observed" {
		t.Fatalf("result=%+v err=%v pushes=%d state=%s side=%+v", result, err, publisher.pushes, store.run.State, store.side)
	}
}

func TestProductionPushDoesNotPersistIntentBeforeExactHeadGatePasses(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateApprovalReady)
	validator := &failingPushValidator{}
	publisher := &pushPublisher{remotes: []string{run.CandidateHead}}
	_, err := coordinator.Push(context.Background(), ProductionPushCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, validator, publisher)
	if err == nil || validator.calls != 1 || publisher.pushes != 0 || store.side.ID != 0 || store.run.State != domain.StateApprovalReady {
		t.Fatalf("err=%v validations=%d pushes=%d side=%+v state=%s", err, validator.calls, publisher.pushes, store.side, store.run.State)
	}
}

func TestProductionPushIntentContainsOnlySanitizedEvidence(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StatePushingBranch)
	publisher := &pushPublisher{remotes: []string{run.CandidateHead}}
	_, err := coordinator.Push(context.Background(), ProductionPushCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, &pushValidator{}, publisher)
	if err != nil || strings.Contains(store.side.IntentJSON, "/owned/") || strings.Contains(store.side.ResultJSON, "/owned/") || store.side.ObservedAt.IsZero() || store.side.ObservedAt.After(time.Now().UTC().Add(time.Second)) {
		t.Fatalf("err=%v side=%+v", err, store.side)
	}
}

func TestProductionContinueRevalidatesLinearBeforeLocalController(t *testing.T) {
	repository := LocalRepository{CanonicalRepository: "owner/repo", BaseBranch: "main", VerifierIDs: []string{"fixture-go-test"}, AllowedOperatorLogins: []string{"operator"}}
	reader := &admissionReader{source: validLinearSource()}
	snapshot, _, err := admitLinearTask(reader.source, admissionResolver{repositories: map[string]LocalRepository{"owner/repo": repository}})
	if err != nil {
		t.Fatal(err)
	}
	run := authorizeTestRun(Run{ID: snapshot.Task.RunID, IssueID: snapshot.Task.IssueID, IdempotencyKey: snapshot.IdempotencyKey, SourceRevision: snapshot.Task.SourceRevision, Repository: snapshot.Task.Repository, WorkingBranch: snapshot.Task.WorkingBranch, TaskHash: snapshot.TaskHash, State: domain.StateExecuting})
	store := &admissionStore{serviceStore: serviceStore{run: run}}
	admissionController := &admissionController{}
	admission, err := NewLinearAdmissionService(reader, admissionResolver{repositories: map[string]LocalRepository{"owner/repo": repository}}, store, admissionController)
	if err != nil {
		t.Fatal(err)
	}
	local := &serviceController{run: run}
	coordinator, err := NewProductionCoordinator(admission, local, store)
	if err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.Continue(context.Background(), ProductionContinueCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey})
	if err != nil || reader.calls != 1 || local.continued != 1 || result.Action != ProductionContinueLocal {
		t.Fatalf("result=%+v err=%v reads=%d continues=%d", result, err, reader.calls, local.continued)
	}
}

func TestProductionContinueStopsOnLinearDriftBeforeLocalController(t *testing.T) {
	repository := LocalRepository{CanonicalRepository: "owner/repo", BaseBranch: "main", VerifierIDs: []string{"fixture-go-test"}, AllowedOperatorLogins: []string{"operator"}}
	reader := &admissionReader{source: validLinearSource()}
	snapshot, _, err := admitLinearTask(reader.source, admissionResolver{repositories: map[string]LocalRepository{"owner/repo": repository}})
	if err != nil {
		t.Fatal(err)
	}
	run := authorizeTestRun(Run{ID: snapshot.Task.RunID, IssueID: snapshot.Task.IssueID, IdempotencyKey: snapshot.IdempotencyKey, SourceRevision: snapshot.Task.SourceRevision, Repository: snapshot.Task.Repository, WorkingBranch: snapshot.Task.WorkingBranch, TaskHash: snapshot.TaskHash, State: domain.StateExecuting})
	reader.source.SourceRevision = "changed"
	store := &admissionStore{serviceStore: serviceStore{run: run}}
	admission, err := NewLinearAdmissionService(reader, admissionResolver{repositories: map[string]LocalRepository{"owner/repo": repository}}, store, &admissionController{})
	if err != nil {
		t.Fatal(err)
	}
	local := &serviceController{run: run}
	coordinator, err := NewProductionCoordinator(admission, local, store)
	if err != nil {
		t.Fatal(err)
	}
	_, err = coordinator.Continue(context.Background(), ProductionContinueCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey})
	if err == nil || !strings.Contains(err.Error(), "human decision") || !store.marked || local.continued != 0 {
		t.Fatalf("err=%v marked=%t continues=%d", err, store.marked, local.continued)
	}
}
