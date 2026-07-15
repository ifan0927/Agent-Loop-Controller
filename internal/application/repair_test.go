package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func repairFinding(id, body string) FindingRecord {
	sum := sha256.Sum256([]byte(body))
	return FindingRecord{Source: "github_required_check", SourceID: id, Body: body, BodyDigest: hex.EncodeToString(sum[:]), HeadSHA: "head"}
}

func TestRepairableFindingsAreBoundedDeterministicAndIdempotent(t *testing.T) {
	first := repairFinding("b", "second")
	second := repairFinding("a", "first")
	resolved := repairFinding("resolved", "done")
	resolved.Resolved = true
	stale := repairFinding("stale", "old")
	stale.HeadSHA = "old"
	selected, err := RepairableFindings([]FindingRecord{first, resolved, second, first, stale}, "head")
	if err != nil || len(selected) != 2 || selected[0].SourceID != "a" || selected[1].SourceID != "b" {
		t.Fatalf("selected=%+v err=%v", selected, err)
	}
	if prompt := BuildRepairPrompt(selected); prompt == "" || len(prompt) > (MaxNormalizedFindingBodyBytes*2) {
		t.Fatalf("unexpected prompt length=%d", len(prompt))
	}
}

func TestRepairableFindingsFailClosedOnUnsupportedOrTamperedBodies(t *testing.T) {
	unsupported := repairFinding("one", "body")
	unsupported.Source = "ci_failure"
	if _, err := RepairableFindings([]FindingRecord{unsupported}, "head"); err == nil {
		t.Fatal("unsupported finding source entered repair")
	}
	tampered := repairFinding("two", "body")
	tampered.Body = "changed"
	if _, err := RepairableFindings([]FindingRecord{tampered}, "head"); err == nil {
		t.Fatal("tampered finding body entered repair")
	}
	oversize := repairFinding("three", string(make([]byte, MaxNormalizedFindingBodyBytes+1)))
	if _, err := RepairableFindings([]FindingRecord{oversize}, "head"); err == nil {
		t.Fatal("oversized finding body entered repair")
	}
	many := make([]FindingRecord, 0, MaxNormalizedFindings+1)
	for i := 0; i <= MaxNormalizedFindings; i++ {
		many = append(many, repairFinding(string(rune('a'+i)), "body"))
	}
	if _, err := RepairableFindings(many, "head"); err == nil {
		t.Fatal("unbounded finding set entered repair")
	}
}

func TestRepairableInlineFindingRequiresSelectedImmutableFeedback(t *testing.T) {
	body := "quoted review body"
	sum := sha256.Sum256([]byte(body))
	finding := FindingRecord{Source: "github_human_review_comment", SourceID: "COMMENT", ThreadID: "THREAD", Body: body, BodyDigest: hex.EncodeToString(sum[:]), HeadSHA: "head"}
	if _, err := RepairableFindings([]FindingRecord{finding}, "head"); err == nil {
		t.Fatal("fabricated inline finding entered repair")
	}
	feedback := TrustedReviewFeedbackRecord{TrustedReviewFeedback: domain.TrustedReviewFeedback{RootCommentNodeID: "COMMENT", ThreadNodeID: "THREAD", OriginalReviewHeadSHA: "head", Body: body, BodyDigest: finding.BodyDigest, Lifecycle: domain.TrustedReviewFeedbackSelectedForRepair}}
	if selected, err := RepairableFindings([]FindingRecord{finding}, "head", []TrustedReviewFeedbackRecord{feedback}); err != nil || len(selected) != 1 {
		t.Fatalf("selected=%+v err=%v", selected, err)
	}
	feedback.Lifecycle = domain.TrustedReviewFeedbackObserved
	if _, err := RepairableFindings([]FindingRecord{finding}, "head", []TrustedReviewFeedbackRecord{feedback}); err == nil {
		t.Fatal("unselected feedback entered repair")
	}
}

func TestRepairableEvidenceSynthesizesTrustedRequiredCheckFinding(t *testing.T) {
	evidence := domain.GitHubReadEvidence{Checks: []domain.GitHubCheck{{ID: "check-1", Name: "go test", Required: true, Source: "check_run", State: domain.CheckFailure, ObservedSHA: "head"}}}
	findings, selected, err := repairableEvidenceFindings(evidence, "head")
	if err != nil || len(findings) != 1 || len(selected) != 1 || selected[0].Source != "github_required_check" {
		t.Fatalf("findings=%+v selected=%+v err=%v", findings, selected, err)
	}
}

func TestRepairDeadlineUsesFirstPersistedRepairAttempt(t *testing.T) {
	now := time.Now().UTC()
	if !repairDeadlineExceeded([]Transition{{From: domain.StateRepairing, To: domain.StateExecuting, CreatedAt: now.Add(-repairDeadline)}}, now) {
		t.Fatal("deadline boundary was not enforced")
	}
	if repairDeadlineExceeded([]Transition{{From: domain.StateRepairing, To: domain.StateExecuting, CreatedAt: now.Add(-repairDeadline + time.Second)}}, now) {
		t.Fatal("repair before deadline was rejected")
	}
}

func TestOutcomeReadHonorsCancellationAndSizeBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "review-outcome.json")
	if err := os.WriteFile(path, []byte(`{"verdict":"pass","summary":"ok","reviewed_head_sha":"head","findings":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readOutcomeWithContext[domain.ReviewOutcome](canceled, path, "ignored"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled outcome read err=%v", err)
	}
	oversized := filepath.Join(t.TempDir(), "oversized-outcome.json")
	if err := os.WriteFile(oversized, make([]byte, maxStructuredOutcomeBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readOutcomeWithContext[domain.ReviewOutcome](context.Background(), oversized, "ignored"); err == nil {
		t.Fatal("oversized outcome was read")
	}
}

type repairDeadlineTestStore struct {
	RunStore
	run                    Run
	inspection             RunInspection
	inspectContextError    bool
	transitionCalls        int
	transitionContextError error
}

func (s *repairDeadlineTestStore) GetRun(context.Context, string) (Run, error) {
	return s.run, nil
}

func (s *repairDeadlineTestStore) Inspect(ctx context.Context, _ string) (RunInspection, error) {
	if s.inspectContextError && ctx.Err() != nil {
		return RunInspection{}, ctx.Err()
	}
	return s.inspection, nil
}

func (s *repairDeadlineTestStore) Transition(ctx context.Context, _ string, from, to domain.State, _, _, _ string) error {
	s.transitionCalls++
	s.transitionContextError = ctx.Err()
	if s.run.State != from {
		return errors.New("unexpected repair deadline test state")
	}
	s.run.State = to
	return nil
}

func TestExpiredRepairDeadlineUsesDetachedPersistenceAndIsIdempotent(t *testing.T) {
	store := &repairDeadlineTestStore{
		run:                 Run{ID: "run", State: domain.StateVerifying, CandidateHead: "head"},
		inspection:          RunInspection{Timeline: []Transition{{From: domain.StateRepairing, To: domain.StateExecuting, CreatedAt: time.Now().UTC().Add(-repairDeadline - time.Second)}}},
		inspectContextError: true,
	}
	controller := &LocalController{store: store}
	callerCtx, cancel := context.WithCancel(context.Background())
	cancel()
	updated, err := controller.enforceRepairDeadline(callerCtx, store.run)
	if err == nil || updated.State != domain.StateManualIntervention {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
	if store.transitionCalls != 1 || store.transitionContextError != nil {
		t.Fatalf("transitionCalls=%d transitionContextError=%v", store.transitionCalls, store.transitionContextError)
	}
	if _, err := controller.enforceRepairDeadline(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	if store.transitionCalls != 1 {
		t.Fatalf("expired deadline transition repeated: calls=%d", store.transitionCalls)
	}
}

func TestCallerCancellationPreservesRepairStateBeforePolicyDeadline(t *testing.T) {
	store := &repairDeadlineTestStore{
		run:                 Run{ID: "run", State: domain.StateVerifying, CandidateHead: "head"},
		inspection:          RunInspection{Timeline: []Transition{{From: domain.StateRepairing, To: domain.StateExecuting, CreatedAt: time.Now().UTC().Add(-repairDeadline + time.Minute)}}},
		inspectContextError: true,
	}
	controller := &LocalController{store: store}
	callerCtx, cancel := context.WithCancel(context.Background())
	cancel()
	updated, err := controller.enforceRepairDeadline(callerCtx, store.run)
	if !errors.Is(err, context.Canceled) || updated.State != domain.StateVerifying {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
	if store.transitionCalls != 0 {
		t.Fatalf("caller cancellation changed repair state: calls=%d", store.transitionCalls)
	}
}

func TestRepairDeadlineFailsClosedForMissingOrMalformedRepairAnchor(t *testing.T) {
	tests := []struct {
		name       string
		inspection RunInspection
	}{
		{name: "missing", inspection: RunInspection{}},
		{name: "malformed", inspection: RunInspection{Timeline: []Transition{{From: domain.StateRepairing, To: domain.StateExecuting}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &repairDeadlineTestStore{
				run:        Run{ID: "run", State: domain.StateVerifying, CandidateHead: "head"},
				inspection: test.inspection,
			}
			controller := &LocalController{store: store}
			updated, err := controller.enforceRepairDeadline(context.Background(), store.run)
			if err == nil || updated.State != domain.StateManualIntervention || store.transitionCalls != 1 {
				t.Fatalf("updated=%+v err=%v transitionCalls=%d", updated, err, store.transitionCalls)
			}
		})
	}
}

func TestLaterRepairingCycleRequiresOriginalRepairDeadlineAnchor(t *testing.T) {
	store := &repairDeadlineTestStore{
		run: Run{ID: "run", State: domain.StateRepairing, CandidateHead: "head"},
		inspection: RunInspection{Timeline: []Transition{
			{From: domain.StateReconcilingReviews, To: domain.StateRepairing, CreatedAt: time.Now().UTC()},
			{From: domain.StateFreshReview, To: domain.StateRepairing, CreatedAt: time.Now().UTC()},
		}},
	}
	controller := &LocalController{store: store}
	updated, err := controller.enforceRepairDeadline(context.Background(), store.run)
	if err == nil || updated.State != domain.StateManualIntervention || store.transitionCalls != 1 {
		t.Fatalf("updated=%+v err=%v transitionCalls=%d", updated, err, store.transitionCalls)
	}
}

func TestInitialRepairFreeFlowDoesNotRequireRepairDeadlineAnchor(t *testing.T) {
	store := &repairDeadlineTestStore{
		run: Run{ID: "run", State: domain.StateFreshReview, CandidateHead: "head"},
		inspection: RunInspection{Timeline: []Transition{
			{From: domain.StateProvisioning, To: domain.StateExecuting, CreatedAt: time.Now().UTC()},
			{From: domain.StateExecuting, To: domain.StateVerifying, CreatedAt: time.Now().UTC()},
			{From: domain.StateVerifying, To: domain.StateFreshReview, CreatedAt: time.Now().UTC()},
		}},
	}
	controller := &LocalController{store: store}
	updated, err := controller.enforceRepairDeadline(context.Background(), store.run)
	if err != nil || updated.State != domain.StateFreshReview || store.transitionCalls != 0 {
		t.Fatalf("updated=%+v err=%v transitionCalls=%d", updated, err, store.transitionCalls)
	}
}

func TestRepairActionContextUsesPersistedDeadline(t *testing.T) {
	store := &repairDeadlineTestStore{
		run:        Run{ID: "run", State: domain.StateFreshReview, CandidateHead: "head"},
		inspection: RunInspection{Timeline: []Transition{{From: domain.StateRepairing, To: domain.StateExecuting, CreatedAt: time.Now().UTC().Add(-repairDeadline + 20*time.Millisecond)}}},
	}
	controller := &LocalController{store: store}
	actionCtx, cancel, err := controller.boundRepairActionContext(context.Background(), store.run)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	select {
	case <-actionCtx.Done():
		if !errors.Is(actionCtx.Err(), context.DeadlineExceeded) {
			t.Fatalf("action context error=%v", actionCtx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("repair action context ignored persisted deadline")
	}
}

func TestLatestRepairStartedAtUsesNewestRepairTransition(t *testing.T) {
	first := time.Now().UTC().Add(-time.Minute)
	second := time.Now().UTC()
	timeline := []Transition{{From: domain.StateRepairing, To: domain.StateExecuting, CreatedAt: first}, {From: domain.StateRepairing, To: domain.StateExecuting, CreatedAt: second}}
	if got := latestRepairStartedAt(timeline); !got.Equal(second) {
		t.Fatalf("repair start=%s want=%s", got, second)
	}
}

func TestPostRepairEvidenceCannotReuseSameHeadVerificationOrReview(t *testing.T) {
	started := time.Now().UTC()
	head := "head"
	oldVerification := VerificationRecord{VerifierID: "verify", Phase: "candidate", VerifiedHead: head, ProcessOutcome: VerificationOutcomeExited, ExitCode: 0, EvidencePath: "old.json", CreatedAt: started.Add(-time.Second)}
	if _, ok := successfulVerificationBatchAfter([]VerificationRecord{oldVerification}, head, []string{"verify"}, started); ok {
		t.Fatal("pre-repair verification was reused")
	}
	newVerification := oldVerification
	newVerification.EvidencePath, newVerification.CreatedAt = "new.json", started
	if _, ok := successfulVerificationBatchAfter([]VerificationRecord{oldVerification, newVerification}, head, []string{"verify"}, started); !ok {
		t.Fatal("post-repair verification was not accepted")
	}
	oldReview := ReviewRecord{ID: 1, ReviewedHead: head, CreatedAt: started.Add(-time.Second)}
	if _, ok := latestReviewForHeadAfter([]ReviewRecord{oldReview}, head, started); ok {
		t.Fatal("pre-repair review was reused")
	}
	newReview := oldReview
	newReview.ID, newReview.CreatedAt = 2, started
	if got, ok := latestReviewForHeadAfter([]ReviewRecord{oldReview, newReview}, head, started); !ok || got.ID != newReview.ID {
		t.Fatalf("post-repair review=%+v ok=%t", got, ok)
	}
}

func TestCandidateAuthorizationRejectsNewestStartFailureAfterOlderSuccess(t *testing.T) {
	now := time.Now().UTC()
	old := VerificationRecord{VerifierID: "verify", Phase: "candidate", VerifiedHead: "head", ProcessOutcome: VerificationOutcomeExited, ExitCode: 0, EvidencePath: "old.json", CreatedAt: now.Add(-time.Second)}
	failed := VerificationRecord{VerifierID: "verify", Phase: "candidate", VerifiedHead: "head", ProcessOutcome: VerificationOutcomeNotStarted, FailureCategory: "process_start", ExitCode: -1, EvidencePath: "failed.json", CreatedAt: now}
	if _, ok := successfulVerificationBatch([]VerificationRecord{old, failed}, "head", []string{"verify"}); ok {
		t.Fatal("newest start failure reused older successful verification")
	}
	passed := failed
	passed.ProcessOutcome = VerificationOutcomeExited
	passed.FailureCategory = VerificationFailureNone
	passed.ExitCode = 0
	passed.EvidencePath = "passed.json"
	passed.CreatedAt = now.Add(time.Second)
	if _, ok := successfulVerificationBatch([]VerificationRecord{old, failed, passed}, "head", []string{"verify"}); !ok {
		t.Fatal("complete successful retry was not accepted")
	}
}
