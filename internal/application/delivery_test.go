package application

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type deliveryMemoryStore struct {
	polls    []PollObservation
	findings []FindingRecord
	cleanup  []CleanupRecord
}

func (*deliveryMemoryStore) BeginSideEffect(context.Context, SideEffectRecord) (SideEffectRecord, bool, error) {
	panic("unused")
}
func (*deliveryMemoryStore) FinishSideEffect(context.Context, SideEffectRecord) error {
	panic("unused")
}
func (*deliveryMemoryStore) SavePullRequest(context.Context, string, domain.PullRequest) error {
	panic("unused")
}
func (s *deliveryMemoryStore) SavePollObservation(_ context.Context, v PollObservation) error {
	s.polls = append(s.polls, v)
	return nil
}
func (s *deliveryMemoryStore) SaveFinding(_ context.Context, v FindingRecord) error {
	s.findings = append(s.findings, v)
	return nil
}
func (*deliveryMemoryStore) SaveHumanApproval(context.Context, string, domain.HumanApproval) error {
	panic("unused")
}
func (*deliveryMemoryStore) SaveMerge(context.Context, MergeRecord) error { panic("unused") }
func (s *deliveryMemoryStore) UpsertCleanup(_ context.Context, value CleanupRecord) error {
	s.cleanup = append(s.cleanup, value)
	return nil
}
func (s *deliveryMemoryStore) CleanupProgress(context.Context, string) ([]CleanupRecord, error) {
	return append([]CleanupRecord(nil), s.cleanup...), nil
}
func (s *deliveryMemoryStore) PollProgress(_ context.Context, runID string, pr int64, head string) ([]PollObservation, error) {
	var result []PollObservation
	for _, item := range s.polls {
		if item.RunID == runID && item.PRNumber == pr && item.HeadSHA == head {
			result = append(result, item)
		}
	}
	return result, nil
}

type fakeGitHub struct {
	snapshots []domain.ReviewSnapshot
	calls     int
}

func (*fakeGitHub) FindPullRequest(context.Context, string, string) (*domain.PullRequest, error) {
	panic("unused")
}
func (*fakeGitHub) CreatePullRequest(context.Context, string, string, string, string, string) (domain.PullRequest, error) {
	panic("unused")
}
func (f *fakeGitHub) Observe(context.Context, int64, string) (domain.ReviewSnapshot, error) {
	i := f.calls
	f.calls++
	if i >= len(f.snapshots) {
		return domain.ReviewSnapshot{}, errors.New("unexpected poll")
	}
	return f.snapshots[i], nil
}
func (*fakeGitHub) GetPullRequest(context.Context, int64) (domain.PullRequest, error) {
	panic("unused")
}
func (*fakeGitHub) SquashMerge(context.Context, int64, string) (domain.PullRequest, error) {
	panic("unused")
}

func TestBoundedReconciliationPersistsPendingAndPass(t *testing.T) {
	now := time.Now()
	gh := &fakeGitHub{snapshots: []domain.ReviewSnapshot{{HeadSHA: "h1", RequiredChecks: []string{"test"}, Checks: []domain.Check{{Name: "test", Required: true, Status: "in_progress", ObservedSHA: "h1"}}, ObservedAt: now}, {HeadSHA: "h1", RequiredChecks: []string{"test"}, Checks: []domain.Check{{Name: "test", Required: true, Status: "completed", Conclusion: "success", ObservedSHA: "h1"}}, ObservedAt: now}}}
	store := &deliveryMemoryStore{}
	status, err := ReconcileReviews(context.Background(), gh, store, "run", 3, "h1", PollPolicy{MaxAttempts: 2, Interval: 0, Deadline: time.Second}, func(context.Context, time.Duration) error { return nil })
	if err != nil || status != domain.ReconciliationPass || len(store.polls) != 2 {
		t.Fatalf("status=%s polls=%d err=%v", status, len(store.polls), err)
	}
}

func TestReconciliationTimesOutAtBound(t *testing.T) {
	now := time.Now()
	pending := domain.ReviewSnapshot{HeadSHA: "h1", RequiredChecks: []string{"test"}, Checks: []domain.Check{{Name: "test", Required: true, Status: "in_progress", ObservedSHA: "h1"}}, ObservedAt: now}
	gh := &fakeGitHub{snapshots: []domain.ReviewSnapshot{pending, pending}}
	store := &deliveryMemoryStore{}
	status, err := ReconcileReviews(context.Background(), gh, store, "run", 3, "h1", PollPolicy{MaxAttempts: 2, Interval: 0, Deadline: time.Second}, func(context.Context, time.Duration) error { return nil })
	if err != nil || status != domain.ReconciliationTimeout || gh.calls != 2 {
		t.Fatalf("status=%s calls=%d err=%v", status, gh.calls, err)
	}
}

func TestReconciliationRestartUsesRemainingAttemptBudget(t *testing.T) {
	now := time.Now()
	pending := domain.ReviewSnapshot{HeadSHA: "h1", RequiredChecks: []string{"test"}, Checks: []domain.Check{{Name: "test", Required: true, Status: "in_progress", ObservedSHA: "h1"}}, ObservedAt: now}
	passing := domain.ReviewSnapshot{HeadSHA: "h1", RequiredChecks: []string{"test"}, Checks: []domain.Check{{Name: "test", Required: true, Status: "completed", Conclusion: "success", ObservedSHA: "h1"}}, ObservedAt: now}
	store := &deliveryMemoryStore{}
	first := &fakeGitHub{snapshots: []domain.ReviewSnapshot{pending}}
	status, err := ReconcileReviews(context.Background(), first, store, "run", 3, "h1", PollPolicy{MaxAttempts: 1, Deadline: time.Second}, func(context.Context, time.Duration) error { return nil })
	if err != nil || status != domain.ReconciliationTimeout {
		t.Fatalf("first status=%s err=%v", status, err)
	}
	second := &fakeGitHub{snapshots: []domain.ReviewSnapshot{passing}}
	status, err = ReconcileReviews(context.Background(), second, store, "run", 3, "h1", PollPolicy{MaxAttempts: 2, Deadline: time.Second}, func(context.Context, time.Duration) error { return nil })
	if err != nil || status != domain.ReconciliationPass || second.calls != 1 {
		t.Fatalf("restart status=%s calls=%d err=%v", status, second.calls, err)
	}
	third := &fakeGitHub{snapshots: []domain.ReviewSnapshot{passing}}
	status, err = ReconcileReviews(context.Background(), third, store, "run", 3, "h1", PollPolicy{MaxAttempts: 2, Deadline: time.Second}, func(context.Context, time.Duration) error { return nil })
	if err != nil || status != domain.ReconciliationPass || third.calls != 0 {
		t.Fatalf("replayed pass status=%s calls=%d err=%v", status, third.calls, err)
	}
}

func TestHumanApprovalAndMergeBindExactSHA(t *testing.T) {
	run := Run{State: domain.StateAwaitingHumanApproval, CandidateHead: "h1", WorkingBranch: "ifan/one", BaseBranch: "main", BaseSHA: "b1", IdempotencyKey: "key"}
	pr := domain.PullRequest{Number: 4, NodeID: "node-4", HeadBranch: "ifan/one", BaseBranch: "main", BaseSHA: "b1", HeadSHA: "h1", BodyDigest: "digest", OwnershipKey: "key"}
	snap := domain.ReviewSnapshot{HeadSHA: "h1", RequiredChecks: []string{"test"}, Checks: []domain.Check{{Name: "test", Required: true, Status: "completed", Conclusion: "success", ObservedSHA: "h1"}}}
	now := time.Now().UTC()
	approval := domain.HumanApproval{PRNumber: 4, Approver: "ifan0927", Actor: domain.ActorIdentity{DatabaseID: 33, NodeID: "USER_33", Login: "ifan0927", Type: "User"}, ReviewDatabaseID: 55, ReviewNodeID: "PRR_55", Source: "github_pull_request_review", ApprovedSHA: "h1", CIStatus: "pass", ReviewSHA: "h1", ApprovedAt: now, ObservedAt: now}
	if err := AuthorizeMerge(run, pr, snap, approval, "h1", "h1"); err != nil {
		t.Fatal(err)
	}
	run.State = domain.StateMerging
	if err := AuthorizeMerge(run, pr, snap, approval, "h1", "h1"); err != nil {
		t.Fatalf("merging restart gate: %v", err)
	}
	run.State = domain.StateAwaitingHumanApproval
	approval.ApprovedSHA = "old"
	if err := AuthorizeMerge(run, pr, snap, approval, "h1", "h1"); err == nil {
		t.Fatal("stale approval authorized merge")
	}
	approval.ApprovedSHA = "h1"
	pr.BaseBranch = "other"
	if err := AuthorizeMerge(run, pr, snap, approval, "h1", "h1"); err == nil {
		t.Fatal("wrong PR ownership authorized merge")
	}
	pr.BaseBranch = "main"
	approval.Source = "fixture_explicit_approval"
	if err := AuthorizeMerge(run, pr, snap, approval, "h1", "h1"); err == nil {
		t.Fatal("fixture evidence authorized production merge")
	}
	if err := AuthorizeFixtureMerge(run, pr, snap, approval, "h1", "h1"); err != nil {
		t.Fatalf("fixture gate: %v", err)
	}
}

type fakeCleanup struct {
	failRemote bool
	calls      []string
}

func (f *fakeCleanup) RemoveWorktree(_ context.Context, _, name, _, _ string) error {
	f.calls = append(f.calls, "worktree:"+name)
	return nil
}
func (f *fakeCleanup) DeleteLocalBranch(_ context.Context, _, name, _ string) error {
	f.calls = append(f.calls, "local:"+name)
	return nil
}
func (f *fakeCleanup) DeleteRemoteBranch(_ context.Context, _, name, _ string) error {
	f.calls = append(f.calls, "remote:"+name)
	if f.failRemote {
		return errors.New("temporary remote failure")
	}
	return nil
}

func TestCleanupOnlyOwnedResourcesPersistsPartialFailure(t *testing.T) {
	store := &deliveryMemoryStore{}
	port := &fakeCleanup{failRemote: true}
	run, merge, resources := ownedCleanupFixture()
	if err := CleanupOwned(context.Background(), store, port, run, merge, resources); err == nil {
		t.Fatal("expected partial cleanup error")
	}
	if len(port.calls) != 3 || len(store.cleanup) != 8 {
		t.Fatalf("calls=%v cleanup=%v", port.calls, store.cleanup)
	}
	if store.cleanup[5].Status != "failed" || store.cleanup[5].ErrorClass != "remote_conflict" {
		t.Fatal("failed resource was not retained for restart")
	}
}

func TestCleanupRejectsReservedOrForgedResourcesWithoutCallingPort(t *testing.T) {
	store := &deliveryMemoryStore{}
	port := &fakeCleanup{}
	run, merge, _ := ownedCleanupFixture()
	run.WorktreePath = "/tmp/owned"
	reserved := []OwnedResource{{RunID: "r1", Kind: "worktree", Name: "/tmp/owned", Status: "reserved", CreationEvidence: `{"path":"/tmp/owned","branch":"ifan/one","base_branch":"main","base_sha":"b1"}`}}
	if err := CleanupOwned(context.Background(), store, port, run, merge, reserved); err == nil {
		t.Fatal("reserved resource must be rejected")
	}
	if len(port.calls) != 0 {
		t.Fatal("cleanup port called for reserved resource")
	}
	forged := []OwnedResource{{RunID: "r1", Kind: "worktree", Name: "/tmp/other", Status: "owned", CreationEvidence: `{"path":"/tmp/other","branch":"ifan/one","base_branch":"main","base_sha":"b1"}`}}
	if err := CleanupOwned(context.Background(), store, port, run, merge, forged); err == nil {
		t.Fatal("forged path must be rejected")
	}
	if len(port.calls) != 0 {
		t.Fatal("cleanup port called for forged resource")
	}
}

func TestCleanupRestartSkipsPersistedDeletedResource(t *testing.T) {
	store := &deliveryMemoryStore{cleanup: []CleanupRecord{{RunID: "r1", Kind: "worktree", Name: "/tmp/w", Status: "deleted"}}}
	port := &fakeCleanup{}
	run, merge, resources := ownedCleanupFixture()
	if err := CleanupOwned(context.Background(), store, port, run, merge, resources); err != nil {
		t.Fatal(err)
	}
	if len(port.calls) != 2 {
		t.Fatalf("deleted worktree retried or remaining resources skipped: %v", port.calls)
	}
}

func TestCleanupRetryTouchesOnlyTheFailedOwnedDeleteBoundary(t *testing.T) {
	for _, kind := range []string{"worktree", "remote_branch", "local_branch"} {
		t.Run(kind, func(t *testing.T) {
			store := &deliveryMemoryStore{}
			first := &boundaryCleanup{fail: kind}
			run, merge, resources := ownedCleanupFixture()
			if err := CleanupOwned(context.Background(), store, first, run, merge, resources); err == nil {
				t.Fatal("expected owned delete failure")
			}
			second := &boundaryCleanup{}
			if err := CleanupOwned(context.Background(), store, second, run, merge, resources); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(second.calls, []string{kind}) {
				t.Fatalf("restart calls=%v, want only %s", second.calls, kind)
			}
		})
	}
}

type boundaryCleanup struct {
	fail  string
	calls []string
}

func (f *boundaryCleanup) RemoveWorktree(context.Context, string, string, string, string) error {
	f.calls = append(f.calls, "worktree")
	if f.fail == "worktree" {
		return errors.New("temporary worktree failure")
	}
	return nil
}

func (f *boundaryCleanup) DeleteLocalBranch(context.Context, string, string, string) error {
	f.calls = append(f.calls, "local_branch")
	if f.fail == "local_branch" {
		return errors.New("temporary local branch failure")
	}
	return nil
}

func (f *boundaryCleanup) DeleteRemoteBranch(context.Context, string, string, string) error {
	f.calls = append(f.calls, "remote_branch")
	if f.fail == "remote_branch" {
		return errors.New("temporary remote branch failure")
	}
	return nil
}

func TestCleanupSupportsPreNonceOwnedResources(t *testing.T) {
	store := &deliveryMemoryStore{}
	port := &fakeCleanup{}
	run, merge, resources := ownedCleanupFixture()
	legacy := `{"source_path":"/tmp/source","origin_path":"/tmp/origin","path":"/tmp/w","branch":"ifan/one","base_branch":"main","base_sha":"b1"}`
	resources[1].CreationEvidence = legacy
	resources[2].CreationEvidence = legacy
	resources[3].CreationEvidence = "push:1"
	if err := CleanupOwned(context.Background(), store, port, run, merge, resources); err != nil {
		t.Fatalf("legacy cleanup failed: %v", err)
	}
	if len(port.calls) != 3 {
		t.Fatalf("calls=%v", port.calls)
	}
}

func ownedCleanupFixture() (Run, MergeRecord, []OwnedResource) {
	run := Run{ID: "r1", State: domain.StateCleaning, Repository: "repo", BaseBranch: "main", BaseSHA: "b1", WorkingBranch: "ifan/one", CandidateHead: "h1", WorktreePath: "/tmp/w", ArtifactRoot: "/tmp/artifacts", TaskHash: "task-hash"}
	merge := MergeRecord{RunID: "r1", PreMergeSHA: "h1", Method: "squash", MergeSHA: "m1", MergedAt: time.Now().UTC()}
	evidence := `{"source_path":"/tmp/source","origin_path":"/tmp/origin","path":"/tmp/w","branch":"ifan/one","base_branch":"main","base_sha":"b1","nonce":"nonce"}`
	artifact := `{"path":"/tmp/artifacts","attempts_path":"/tmp/artifacts/attempts","run_root":"/tmp","nonce":"artifact-nonce","task_hash":"task-hash"}`
	return run, merge, []OwnedResource{{RunID: "r1", Kind: "artifact_root", Name: run.ArtifactRoot, Status: "owned", CreationEvidence: artifact}, {RunID: "r1", Kind: "worktree", Name: run.WorktreePath, Status: "owned", CreationEvidence: evidence}, {RunID: "r1", Kind: "branch", Name: run.WorkingBranch, Status: "owned", CreationEvidence: evidence}, {RunID: "r1", Kind: "remote_branch", Name: run.WorkingBranch, Status: "owned", CreationEvidence: evidence}}
}

func contains(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}

func TestLatestRepairBaseUsesNewestRollover(t *testing.T) {
	timeline := []Transition{{From: domain.StateRepairing, To: domain.StateExecuting, BoundHead: "old"}, {From: domain.StateRepairing, To: domain.StateExecuting, BoundHead: "new"}}
	if got := latestRepairBase(timeline); got != "new" {
		t.Fatalf("repair base=%s", got)
	}
}

func TestPersistedRepairPromptSurvivesRestartAndRejectsTampering(t *testing.T) {
	prompt := "repair normalized finding"
	data, _ := json.Marshal(struct {
		Prompt string `json:"normalized_prompt"`
		Hash   string `json:"prompt_hash"`
	}{prompt, bytesHash([]byte(prompt))})
	timeline := []Transition{{From: domain.StateRepairing, To: domain.StateExecuting, EvidenceReference: string(data)}}
	got, found, err := findPersistedRepair(timeline)
	if err != nil || !found || got != prompt {
		t.Fatalf("got=%q found=%v err=%v", got, found, err)
	}
	timeline[0].EvidenceReference = `{"normalized_prompt":"changed","prompt_hash":"wrong"}`
	if _, _, err := findPersistedRepair(timeline); err == nil {
		t.Fatal("tampered repair prompt accepted")
	}
}
