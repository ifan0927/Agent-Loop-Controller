package application

import (
	"context"
	"encoding/json"
	"errors"
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
	gh := &fakeGitHub{snapshots: []domain.ReviewSnapshot{{HeadSHA: "h1", RequiredChecks: []string{"test"}, CodeRabbitStatus: "pending", Checks: []domain.Check{{Name: "test", Required: true, Status: "in_progress", ObservedSHA: "h1"}}, ObservedAt: now}, {HeadSHA: "h1", RequiredChecks: []string{"test"}, CodeRabbitStatus: "pass", Checks: []domain.Check{{Name: "test", Required: true, Status: "completed", Conclusion: "success", ObservedSHA: "h1"}}, ObservedAt: now}}}
	store := &deliveryMemoryStore{}
	status, err := ReconcileReviews(context.Background(), gh, store, "run", 3, "h1", PollPolicy{MaxAttempts: 2, Interval: 0, Deadline: time.Second}, func(context.Context, time.Duration) error { return nil })
	if err != nil || status != domain.ReconciliationPass || len(store.polls) != 2 {
		t.Fatalf("status=%s polls=%d err=%v", status, len(store.polls), err)
	}
}

func TestReconciliationTimesOutAtBound(t *testing.T) {
	now := time.Now()
	pending := domain.ReviewSnapshot{HeadSHA: "h1", RequiredChecks: []string{"test"}, CodeRabbitStatus: "pending", Checks: []domain.Check{{Name: "test", Required: true, Status: "in_progress", ObservedSHA: "h1"}}, ObservedAt: now}
	gh := &fakeGitHub{snapshots: []domain.ReviewSnapshot{pending, pending}}
	store := &deliveryMemoryStore{}
	status, err := ReconcileReviews(context.Background(), gh, store, "run", 3, "h1", PollPolicy{MaxAttempts: 2, Interval: 0, Deadline: time.Second}, func(context.Context, time.Duration) error { return nil })
	if err != nil || status != domain.ReconciliationTimeout || gh.calls != 2 {
		t.Fatalf("status=%s calls=%d err=%v", status, gh.calls, err)
	}
}

func TestReconciliationRestartUsesRemainingAttemptBudget(t *testing.T) {
	now := time.Now()
	pending := domain.ReviewSnapshot{HeadSHA: "h1", RequiredChecks: []string{"test"}, CodeRabbitStatus: "pending", Checks: []domain.Check{{Name: "test", Required: true, Status: "in_progress", ObservedSHA: "h1"}}, ObservedAt: now}
	passing := domain.ReviewSnapshot{HeadSHA: "h1", RequiredChecks: []string{"test"}, CodeRabbitStatus: "pass", Checks: []domain.Check{{Name: "test", Required: true, Status: "completed", Conclusion: "success", ObservedSHA: "h1"}}, ObservedAt: now}
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

func TestCodeRabbitFindingIsNormalizedWithoutBodyExecution(t *testing.T) {
	body := "$(touch /tmp/controller-must-not-run)"
	now := time.Now()
	gh := &fakeGitHub{snapshots: []domain.ReviewSnapshot{{HeadSHA: "h1", RequiredChecks: []string{"test"}, CodeRabbitStatus: "failure", Checks: []domain.Check{{Name: "test", Required: true, Status: "completed", Conclusion: "success", ObservedSHA: "h1"}}, Findings: []domain.ExternalFinding{{SourceID: "c1", ThreadID: "t1", Source: "coderabbit", File: "a.go", Line: 3, Severity: "high", Body: body}}, ObservedAt: now}}}
	store := &deliveryMemoryStore{}
	status, err := ReconcileReviews(context.Background(), gh, store, "run", 3, "h1", PollPolicy{MaxAttempts: 1, Deadline: time.Second}, func(context.Context, time.Duration) error { return nil })
	if err != nil || status != domain.ReconciliationActionable || len(store.findings) != 1 || store.findings[0].BodyDigest == body {
		t.Fatalf("status=%s findings=%+v err=%v", status, store.findings, err)
	}
	if prompt := BuildRepairPrompt(store.findings); prompt == "" || !contains(prompt, body) || !contains(prompt, "untrusted_body=") {
		t.Fatalf("normalized untrusted body missing from repair prompt: %q", prompt)
	}
}

func TestHumanApprovalAndMergeBindExactSHA(t *testing.T) {
	run := Run{State: domain.StateAwaitingHumanApproval, CandidateHead: "h1", WorkingBranch: "ifan/one", BaseBranch: "main", BaseSHA: "b1", IdempotencyKey: "key"}
	pr := domain.PullRequest{Number: 4, NodeID: "node-4", HeadBranch: "ifan/one", BaseBranch: "main", BaseSHA: "b1", HeadSHA: "h1", BodyDigest: "digest", OwnershipKey: "key"}
	snap := domain.ReviewSnapshot{HeadSHA: "h1", RequiredChecks: []string{"test"}, CodeRabbitStatus: "pass", Checks: []domain.Check{{Name: "test", Required: true, Status: "completed", Conclusion: "success", ObservedSHA: "h1"}}}
	approval := domain.HumanApproval{PRNumber: 4, Approver: "ifan0927", Source: "github_review", ApprovedSHA: "h1", CIStatus: "pass", CodeRabbit: "pass", ReviewSHA: "h1"}
	if err := AuthorizeMerge(run, pr, snap, approval, "h1", "h1"); err != nil {
		t.Fatal(err)
	}
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

func (f *fakeCleanup) RemoveWorktree(_ context.Context, _, name string) error {
	f.calls = append(f.calls, "worktree:"+name)
	return nil
}
func (f *fakeCleanup) DeleteLocalBranch(_ context.Context, _, name string) error {
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
	run := Run{ID: "r1", Repository: "repo", BaseBranch: "main", BaseSHA: "b1", WorkingBranch: "ifan/one", CandidateHead: "h1", WorktreePath: "/tmp/w"}
	merge := MergeRecord{RunID: "r1", PreMergeSHA: "h1", Method: "squash", MergeSHA: "m1"}
	evidence := `{"path":"/tmp/w","branch":"ifan/one","base_branch":"main","base_sha":"b1"}`
	resources := []OwnedResource{{RunID: "r1", Kind: "worktree", Name: "/tmp/w", Status: "owned", CreationEvidence: evidence}, {RunID: "r1", Kind: "remote_branch", Name: "ifan/one", Status: "owned", CreationEvidence: evidence}, {RunID: "other", Kind: "local_branch", Name: "user", Status: "owned", CreationEvidence: evidence}}
	if err := CleanupOwned(context.Background(), store, port, run, merge, resources); err == nil {
		t.Fatal("expected partial cleanup error")
	}
	if len(port.calls) != 2 || len(store.cleanup) != 4 {
		t.Fatalf("calls=%v cleanup=%v", port.calls, store.cleanup)
	}
	if store.cleanup[len(store.cleanup)-1].Status != "failed" {
		t.Fatal("failed resource was not retained for restart")
	}
}

func TestCleanupRejectsReservedOrForgedResourcesWithoutCallingPort(t *testing.T) {
	store := &deliveryMemoryStore{}
	port := &fakeCleanup{}
	run := Run{ID: "r1", Repository: "repo", BaseBranch: "main", BaseSHA: "b1", WorkingBranch: "ifan/one", CandidateHead: "h1", WorktreePath: "/tmp/owned"}
	merge := MergeRecord{RunID: "r1", PreMergeSHA: "h1", Method: "squash", MergeSHA: "m1"}
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
	run := Run{ID: "r1", Repository: "repo", BaseBranch: "main", BaseSHA: "b1", WorkingBranch: "ifan/one", CandidateHead: "h1", WorktreePath: "/tmp/w"}
	merge := MergeRecord{RunID: "r1", PreMergeSHA: "h1", Method: "squash", MergeSHA: "m1"}
	evidence := `{"path":"/tmp/w","branch":"ifan/one","base_branch":"main","base_sha":"b1"}`
	resources := []OwnedResource{{RunID: "r1", Kind: "worktree", Name: "/tmp/w", Status: "owned", CreationEvidence: evidence}}
	if err := CleanupOwned(context.Background(), store, port, run, merge, resources); err != nil {
		t.Fatal(err)
	}
	if len(port.calls) != 0 {
		t.Fatalf("deleted resource retried: %v", port.calls)
	}
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
