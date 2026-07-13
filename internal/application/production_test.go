package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestProductionNextActionStopsBeforeUnimplementedWrites(t *testing.T) {
	cases := map[domain.State]ProductionAction{
		domain.StateExecuting:             ProductionContinueLocal,
		domain.StateAwaitingHumanDecision: ProductionContinueLocal,
		domain.StatePROpen:                ProductionReconcileGitHub,
		domain.StateApprovalReady:         ProductionPush,
		domain.StatePushingBranch:         ProductionPush,
		domain.StateManualIntervention:    ProductionStop,
		domain.StateCompleted:             ProductionStop,
	}
	for state, want := range cases {
		if got, _ := productionNextAction(state); got != want {
			t.Fatalf("state %s action=%s want=%s", state, got, want)
		}
	}
}

type pushTestStore struct {
	admissionStore
	run         Run
	side        SideEffectRecord
	transitions []Transition
	resources   []OwnedResource
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
	run := authorizeTestRun(Run{ID: snapshot.Task.RunID, IssueID: snapshot.Task.IssueID, IdempotencyKey: snapshot.IdempotencyKey, SourceRevision: snapshot.Task.SourceRevision, Repository: snapshot.Task.Repository, WorkingBranch: snapshot.Task.WorkingBranch, TaskHash: snapshot.TaskHash, State: state, CandidateHead: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", WorktreePath: "/owned/worktree", ArtifactRoot: "/owned/artifacts"})
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
