package application

import (
	"context"
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
	gh := &fakeGitHub{snapshots: []domain.ReviewSnapshot{{HeadSHA: "h1", CodeRabbitStatus: "pending", Checks: []domain.Check{{Required: true, Status: "in_progress"}}, ObservedAt: now}, {HeadSHA: "h1", CodeRabbitStatus: "pass", Checks: []domain.Check{{Required: true, Status: "completed", Conclusion: "success"}}, ObservedAt: now}}}
	store := &deliveryMemoryStore{}
	status, err := ReconcileReviews(context.Background(), gh, store, "run", 3, "h1", PollPolicy{MaxAttempts: 2, Interval: 0, Deadline: time.Second}, func(context.Context, time.Duration) error { return nil })
	if err != nil || status != domain.ReconciliationPass || len(store.polls) != 2 {
		t.Fatalf("status=%s polls=%d err=%v", status, len(store.polls), err)
	}
}

func TestReconciliationTimesOutAtBound(t *testing.T) {
	now := time.Now()
	pending := domain.ReviewSnapshot{HeadSHA: "h1", CodeRabbitStatus: "pending", ObservedAt: now}
	gh := &fakeGitHub{snapshots: []domain.ReviewSnapshot{pending, pending}}
	store := &deliveryMemoryStore{}
	status, err := ReconcileReviews(context.Background(), gh, store, "run", 3, "h1", PollPolicy{MaxAttempts: 2, Interval: 0, Deadline: time.Second}, func(context.Context, time.Duration) error { return nil })
	if err != nil || status != domain.ReconciliationTimeout || gh.calls != 2 {
		t.Fatalf("status=%s calls=%d err=%v", status, gh.calls, err)
	}
}

func TestCodeRabbitFindingIsNormalizedWithoutBodyExecution(t *testing.T) {
	body := "$(touch /tmp/controller-must-not-run)"
	now := time.Now()
	gh := &fakeGitHub{snapshots: []domain.ReviewSnapshot{{HeadSHA: "h1", CodeRabbitStatus: "failure", Findings: []domain.ExternalFinding{{SourceID: "c1", ThreadID: "t1", Source: "coderabbit", File: "a.go", Line: 3, Severity: "high", Body: body}}, ObservedAt: now}}}
	store := &deliveryMemoryStore{}
	status, err := ReconcileReviews(context.Background(), gh, store, "run", 3, "h1", PollPolicy{MaxAttempts: 1, Deadline: time.Second}, func(context.Context, time.Duration) error { return nil })
	if err != nil || status != domain.ReconciliationActionable || len(store.findings) != 1 || store.findings[0].BodyDigest == body {
		t.Fatalf("status=%s findings=%+v err=%v", status, store.findings, err)
	}
	if prompt := BuildRepairPrompt(store.findings); prompt == "" || contains(prompt, body) {
		t.Fatalf("untrusted body entered repair prompt: %q", prompt)
	}
}

func TestHumanApprovalAndMergeBindExactSHA(t *testing.T) {
	run := Run{State: domain.StateAwaitingHumanApproval, CandidateHead: "h1"}
	pr := domain.PullRequest{Number: 4, HeadSHA: "h1"}
	snap := domain.ReviewSnapshot{HeadSHA: "h1", CodeRabbitStatus: "pass", Checks: []domain.Check{{Required: true, Status: "completed", Conclusion: "success"}}}
	approval := domain.HumanApproval{PRNumber: 4, Approver: "I-Fan", Source: "github_review", ApprovedSHA: "h1", CIStatus: "pass", CodeRabbit: "pass", ReviewSHA: "h1"}
	if err := AuthorizeMerge(run, pr, snap, approval, "h1", "h1"); err != nil {
		t.Fatal(err)
	}
	approval.ApprovedSHA = "old"
	if err := AuthorizeMerge(run, pr, snap, approval, "h1", "h1"); err == nil {
		t.Fatal("stale approval authorized merge")
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
	run := Run{ID: "r1", Repository: "repo", BaseBranch: "main", WorkingBranch: "ifan/one", CandidateHead: "h1"}
	merge := MergeRecord{RunID: "r1", PreMergeSHA: "h1", Method: "squash", MergeSHA: "m1"}
	resources := []OwnedResource{{RunID: "r1", Kind: "worktree", Name: "/tmp/w", Status: "owned"}, {RunID: "r1", Kind: "remote_branch", Name: "ifan/one", Status: "owned"}, {RunID: "other", Kind: "local_branch", Name: "user", Status: "owned"}}
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

func contains(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
