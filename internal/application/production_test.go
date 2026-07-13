package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestProductionNextActionStopsBeforeUnimplementedWrites(t *testing.T) {
	cases := map[domain.State]ProductionAction{
		domain.StateExecuting:                ProductionContinueLocal,
		domain.StateAwaitingHumanDecision:    ProductionContinueLocal,
		domain.StatePROpen:                   ProductionReconcileGitHub,
		domain.StateApprovalReady:            ProductionPush,
		domain.StateBranchPushed:             ProductionOpenPullRequest,
		domain.StatePushingBranch:            ProductionPush,
		domain.StateAwaitingLinearCompletion: ProductionReconcileLinear,
		domain.StateManualIntervention:       ProductionStop,
		domain.StateCompleted:                ProductionStop,
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
	run              Run
	inspection       RunInspection
	side             SideEffectRecord
	transitions      []Transition
	resources        []OwnedResource
	pr               *domain.PullRequest
	merge            *MergeRecord
	github           []domain.GitHubReadEvidence
	requests         []GitHubRequestObservation
	linearCompletion []LinearCompletionObservation
	linearRequests   []LinearRequestObservation
	metadata         []GitHubInstallationMetadata
	requestErr       error
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
func (s *pushTestStore) SaveGitHubEvidence(_ context.Context, _ string, value domain.GitHubReadEvidence) error {
	s.github = append(s.github, value)
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
	remotes  []string
	reads    int
	pushes   int
	pushErr  error
	evidence PushEvidence
}

func (p *pushPublisher) RemoteSHA(context.Context, string, string) (string, error) {
	if p.reads >= len(p.remotes) {
		return "", errors.New("unexpected remote read")
	}
	value := p.remotes[p.reads]
	p.reads++
	return value, nil
}
func (p *pushPublisher) Push(context.Context, string, string, string, string, string) (PushEvidence, error) {
	p.pushes++
	return p.evidence, p.pushErr
}

func newPushCoordinator(t *testing.T, state domain.State) (*ProductionCoordinator, *pushTestStore, Run) {
	t.Helper()
	repository := LocalRepository{CanonicalRepository: "owner/repo", BaseBranch: "main", VerifierIDs: []string{"fixture-go-test"}, AllowedOperatorLogins: []string{"operator"}}
	reader := &admissionReader{source: validLinearSource()}
	snapshot, _, err := admitLinearTask(reader.source, admissionResolver{repositories: map[string]LocalRepository{"owner/repo": repository}})
	if err != nil {
		t.Fatal(err)
	}
	run := authorizeTestRun(Run{ID: snapshot.Task.RunID, IssueID: snapshot.Task.IssueID, IdempotencyKey: snapshot.IdempotencyKey, SourceRevision: snapshot.Task.SourceRevision, RawIssueJSON: string(snapshot.RawJSON), Repository: snapshot.Task.Repository, WorkingBranch: snapshot.Task.WorkingBranch, BaseBranch: "main", BaseSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", NormalizedTaskJSON: mustJSON(t, snapshot.Task), TaskHash: snapshot.TaskHash, State: state, CandidateHead: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", WorktreePath: "/owned/worktree", ArtifactRoot: "/owned/artifacts"})
	store := &pushTestStore{run: run}
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
	observations []GitHubRequestObservation
	calls        int
}

func (r *mergeReader) Authority() GitHubInstallationMetadata { return r.authority }
func (r *mergeReader) Read(_ context.Context, _ int64, _ string) (domain.GitHubReadEvidence, []GitHubRequestObservation, GitHubInstallationMetadata, error) {
	r.calls++
	return r.evidence, append([]GitHubRequestObservation(nil), r.observations...), r.authority, nil
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
	approval := domain.HumanApproval{PRNumber: pr.Number, Approver: actor.Login, Actor: actor, ReviewDatabaseID: 9, ReviewNodeID: "PRR_9", Source: "github_pull_request_review", ApprovedSHA: run.CandidateHead, CIStatus: "pass", CodeRabbit: "pass", ReviewSHA: run.CandidateHead, ApprovedAt: now, ObservedAt: now}
	binding := &SanitizedRepositoryBinding{CanonicalRepository: "owner/repo", ExpectedRepositoryID: 99, GitHubAppID: 1, GitHubInstallationID: 2, TrustedOperatorActors: []TrustedActorIdentity{{DatabaseID: actor.DatabaseID, NodeID: actor.NodeID, Login: actor.Login, Type: actor.Type}}}
	evidence := domain.GitHubReadEvidence{Repository: domain.RepositoryIdentity{ID: 99, NodeID: "REPO", Owner: "owner", Name: "repo"}, PullRequest: pr, Checks: []domain.GitHubCheck{{Name: "test", Required: true, ObservedSHA: run.CandidateHead, State: domain.CheckSuccess}}, ReviewDecision: "APPROVED", CodeRabbit: domain.CodeRabbitPass, Reviews: []domain.GitHubReview{{DatabaseID: 9, NodeID: "PRR_9", State: "APPROVED", CommitSHA: run.CandidateHead, SourceAt: now, Actor: actor}}, ObservedAt: now}
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

func TestProductionPushReconcilesSameRemoteSHAWithoutInvokingGit(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateApprovalReady)
	validator := &pushValidator{}
	publisher := &pushPublisher{remotes: []string{run.CandidateHead}}
	result, err := coordinator.Push(context.Background(), ProductionPushCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, validator, publisher)
	if err != nil || !result.Idempotent || publisher.pushes != 0 || validator.calls != 1 || store.run.State != domain.StateBranchPushed {
		t.Fatalf("result=%+v err=%v pushes=%d validations=%d state=%s", result, err, publisher.pushes, validator.calls, store.run.State)
	}
	if store.side.Status != "observed" || len(store.transitions) != 2 || len(store.resources) != 1 {
		t.Fatalf("side=%+v transitions=%+v resources=%+v", store.side, store.transitions, store.resources)
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
