package application

import (
	"context"
	"strings"
	"testing"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestProductionNextActionStopsBeforeUnimplementedWrites(t *testing.T) {
	cases := map[domain.State]ProductionAction{
		domain.StateExecuting:             ProductionContinueLocal,
		domain.StateAwaitingHumanDecision: ProductionContinueLocal,
		domain.StatePROpen:                ProductionReconcileGitHub,
		domain.StateApprovalReady:         ProductionStop,
		domain.StatePushingBranch:         ProductionStop,
		domain.StateManualIntervention:    ProductionStop,
		domain.StateCompleted:             ProductionStop,
	}
	for state, want := range cases {
		if got, _ := productionNextAction(state); got != want {
			t.Fatalf("state %s action=%s want=%s", state, got, want)
		}
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
