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

type abandonCleanupFake struct {
	calls []string
	err   error
}

type abandonCoordinatorStore struct {
	*pushTestStore
	abandonCalls int
}

func (s *abandonCoordinatorStore) AbandonAutomaticAdmission(_ context.Context, request AutomaticAdmissionAbandonment) (Run, bool, error) {
	if s.run.State == domain.StateFailed {
		return s.run, true, nil
	}
	if s.run.State != request.ExpectedState || s.run.IdempotencyKey != request.IdempotencyKey {
		return Run{}, false, errors.New("abandon compare failed")
	}
	s.abandonCalls++
	s.run.State = domain.StateFailed
	s.run.LastError = AutomaticAdmissionAbandonTransition
	return s.run, false, nil
}

func (f *abandonCleanupFake) RemoveWorktree(context.Context, string, string, string, string) error {
	f.calls = append(f.calls, "worktree")
	return f.err
}

func (f *abandonCleanupFake) DeleteLocalBranch(context.Context, string, string, string) error {
	f.calls = append(f.calls, "branch")
	return f.err
}

func (f *abandonCleanupFake) DeleteRemoteBranch(context.Context, string, string, string) error {
	f.calls = append(f.calls, "remote")
	return errors.New("remote cleanup is outside abandon scope")
}

func TestAbandonLocalCleanupRetainsArtifactAndUsesOnlyOwnedLocalResources(t *testing.T) {
	repository := LocalRepository{CanonicalRepository: "owner/repo", SourcePath: "/owned/source", OriginPath: "/owned/origin", BaseBranch: "main"}
	run := abandonCleanupRun(t, repository)
	evidence := `{"source_path":"/owned/source","origin_path":"/owned/origin","path":"/owned/worktree","branch":"ifan/one","base_branch":"main","base_sha":"base","nonce":"nonce"}`
	store := &pushTestStore{run: run, resources: []OwnedResource{
		{RunID: run.ID, Kind: "artifact_root", Name: run.ArtifactRoot, CreationEvidence: `{"path":"/owned/artifacts","attempts_path":"/owned/artifacts/attempts","run_root":"/owned","nonce":"artifact","task_hash":"task"}`, Status: "owned"},
		{RunID: run.ID, Kind: "worktree", Name: run.WorktreePath, CreationEvidence: evidence, Status: "owned"},
		{RunID: run.ID, Kind: "branch", Name: run.WorkingBranch, CreationEvidence: evidence, Status: "owned"},
	}}
	cleanup := &abandonCleanupFake{}
	if err := cleanupAbandonedLocalResources(context.Background(), store, run, cleanup); err != nil {
		t.Fatal(err)
	}
	if len(cleanup.calls) != 2 || cleanup.calls[0] != "worktree" || cleanup.calls[1] != "branch" {
		t.Fatalf("cleanup calls=%v", cleanup.calls)
	}
	if len(store.cleanup) != 3 {
		t.Fatalf("cleanup audit=%+v", store.cleanup)
	}
	for _, item := range store.cleanup {
		if item.Kind == "artifact_root" && item.Status != "retained" {
			t.Fatalf("artifact cleanup status=%+v", item)
		}
	}
	for _, resource := range store.resources {
		if (resource.Kind == "worktree" || resource.Kind == "branch") && resource.Status == "owned" {
			// The in-memory production fixture intentionally appends ownership updates;
			// the SQLite adapter performs the same update in place.
			continue
		}
		if resource.Kind == "artifact_root" && resource.Status != "owned" {
			t.Fatalf("artifact ownership changed=%+v", resource)
		}
	}
}

func TestProductionAbandonRevalidatesBeforeDurableMutationAndCleansLocally(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StateManualIntervention)
	wrapped := &abandonCoordinatorStore{pushTestStore: store}
	coordinator.store = wrapped
	cleanup := &abandonCleanupFake{}
	result, err := coordinator.Abandon(context.Background(), ProductionAbandonCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, cleanup)
	if err != nil || result.Action != ProductionAbandon || result.Run.State != domain.StateFailed || result.Idempotent || wrapped.abandonCalls != 1 {
		t.Fatalf("result=%+v err=%v abandonCalls=%d", result, err, wrapped.abandonCalls)
	}
	if cleanup.calls == nil || len(cleanup.calls) != 1 || cleanup.calls[0] != "branch" {
		t.Fatalf("cleanup calls=%v", cleanup.calls)
	}
}

func TestAbandonLocalCleanupRejectsForgedOwnershipBeforeGit(t *testing.T) {
	repository := LocalRepository{CanonicalRepository: "owner/repo", SourcePath: "/owned/source", OriginPath: "/owned/origin", BaseBranch: "main"}
	run := abandonCleanupRun(t, repository)
	store := &pushTestStore{run: run, resources: []OwnedResource{{RunID: run.ID, Kind: "worktree", Name: "/other/worktree", CreationEvidence: `{"source_path":"/owned/source","origin_path":"/owned/origin","path":"/other/worktree","branch":"ifan/one","base_branch":"main","base_sha":"base","nonce":"nonce"}`, Status: "owned"}}}
	cleanup := &abandonCleanupFake{}
	if err := cleanupAbandonedLocalResources(context.Background(), store, run, cleanup); err == nil || len(cleanup.calls) != 0 {
		t.Fatalf("forged cleanup err=%v calls=%v", err, cleanup.calls)
	}
}

func TestSelectAbandonLocalResourcesRequiresExactNamesAndSharedNonce(t *testing.T) {
	repository := LocalRepository{CanonicalRepository: "owner/repo", SourcePath: "/owned/source", OriginPath: "/owned/origin", BaseBranch: "main"}
	run := abandonCleanupRun(t, repository)
	reserved, err := json.Marshal(WorktreeSpec{SourcePath: repository.SourcePath, OriginPath: repository.OriginPath, Path: run.WorktreePath, Branch: run.WorkingBranch, BaseBranch: run.BaseBranch, Nonce: "nonce"})
	if err != nil {
		t.Fatal(err)
	}
	resources := []OwnedResource{
		{RunID: run.ID, Kind: "worktree", Name: run.WorktreePath, CreationEvidence: string(reserved), Status: "reserved"},
		{RunID: run.ID, Kind: "branch", Name: run.WorkingBranch, CreationEvidence: string(reserved), Status: "reserved"},
	}
	if selected, err := selectAbandonLocalResources(run, resources); err != nil || len(selected) != 2 {
		t.Fatalf("reserved ownership selected=%+v err=%v", selected, err)
	}
	resources[1].Name = "ifan/other"
	if _, err := selectAbandonLocalResources(run, resources); err == nil {
		t.Fatal("mismatched local resource name was accepted")
	}
	resources[1].Name = run.WorkingBranch
	resources[1].CreationEvidence = strings.Replace(string(reserved), `"nonce":"nonce"`, `"nonce":"other"`, 1)
	if _, err := selectAbandonLocalResources(run, resources); err == nil {
		t.Fatal("mismatched local ownership nonce was accepted")
	}
}

func TestValidateAbandonInspectionRejectsExternalDeliveryEvidence(t *testing.T) {
	run := Run{ID: "run", State: domain.StateManualIntervention}
	for _, side := range []SideEffectRecord{{Kind: "push", Status: "failed"}, {Kind: "squash_merge", Status: "intent"}, {Kind: "linear_move_to_started", Status: "in_flight"}} {
		if err := validateAbandonInspection(RunInspection{Run: run, SideEffects: []SideEffectRecord{side}}); err == nil {
			t.Fatalf("side effect was accepted: %+v", side)
		}
	}
	if err := validateAbandonInspection(RunInspection{Run: run, PullRequest: &domain.PullRequest{Number: 1}}); err == nil {
		t.Fatal("pull request evidence was accepted")
	}
}

func abandonCleanupRun(t *testing.T, repository LocalRepository) Run {
	t.Helper()
	raw, _ := json.Marshal(repository)
	return Run{ID: "run-abandon-cleanup", Repository: repository.CanonicalRepository, RepositoryConfigJSON: string(raw), BaseBranch: repository.BaseBranch, WorkingBranch: "ifan/one", BaseSHA: "base", CandidateHead: "candidate", WorktreePath: "/owned/worktree", ArtifactRoot: "/owned/artifacts", TaskHash: "task", State: domain.StateFailed, UpdatedAt: time.Now().UTC()}
}
