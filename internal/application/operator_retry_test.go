package application

import (
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestOperatorRetryPlanAllowlistRejectsHumanAndUnresolvedAuthority(t *testing.T) {
	now := time.Now().UTC()
	run := Run{ID: "run", State: domain.StateReceived}
	schedule := RetrySchedule{RunID: run.ID, Phase: AutomaticRetryPhaseForRun(run), ControllerState: string(run.State), AttemptCount: 4, MaxAttempts: 3, InitialDelay: time.Second, MaximumDelay: 30 * time.Second, FailureClass: RetryFailureProcessStart, FailureEvidenceRef: "attempt:7", ReasonCode: RetryReasonBudgetExhausted, Status: RetryScheduleAttention, AttentionAt: now, CreatedAt: now.Add(-time.Minute), UpdatedAt: now}
	processEvidence := Attempt{ID: 7, RunID: run.ID, Status: "failed", ErrorCategory: RetryReasonProcessStart, FinishedAt: now.Add(-time.Second)}
	if err := validateOperatorRetryPlan(run, RunInspection{Run: run, Attempts: []Attempt{processEvidence}}, schedule); err != nil {
		t.Fatalf("supported plan rejected: %v", err)
	}
	if err := validateOperatorRetryPlan(run, RunInspection{Run: run}, schedule); err == nil {
		t.Fatal("process-start schedule without persisted process evidence was operator-retryable")
	}
	staleEvidence := processEvidence
	staleEvidence.ID = 6
	if err := validateOperatorRetryPlan(run, RunInspection{Run: run, Attempts: []Attempt{staleEvidence}}, schedule); err == nil {
		t.Fatal("stale process-start evidence satisfied a different schedule reference")
	}

	human := run
	human.State = domain.StateAwaitingHumanDecision
	humanSchedule := schedule
	humanSchedule.Phase = AutomaticRetryPhaseForRun(human)
	humanSchedule.ControllerState = string(human.State)
	if err := validateOperatorRetryPlan(human, RunInspection{Run: human}, humanSchedule); err == nil {
		t.Fatal("human decision state was operator-retryable")
	}

	authority := schedule
	authority.FailureClass = RetryFailureAuthority
	if err := validateOperatorRetryPlan(run, RunInspection{Run: run, Attempts: []Attempt{processEvidence}}, authority); err == nil {
		t.Fatal("authority failure was operator-retryable")
	}

	inspection := RunInspection{Run: run, Attempts: []Attempt{processEvidence}, SideEffects: []SideEffectRecord{{RunID: run.ID, Status: "intent"}}}
	if err := validateOperatorRetryPlan(run, inspection, schedule); err == nil {
		t.Fatal("unresolved side effect was operator-retryable")
	}

	run.WorktreePath = "/planned"
	if err := validateOperatorRetryPlan(run, RunInspection{Run: run, Attempts: []Attempt{processEvidence}}, schedule); err != nil {
		t.Fatalf("planned pre-provisioning path required premature ownership: %v", err)
	}
	inspection = RunInspection{Run: run, Attempts: []Attempt{processEvidence}, Resources: []OwnedResource{{RunID: run.ID, Kind: "worktree", Status: "owned", Name: "/unexpected"}}}
	if err := validateOperatorRetryPlan(run, inspection, schedule); err == nil {
		t.Fatal("unexpected pre-provisioning ownership was operator-retryable")
	}
	if err := validateOperatorRetryPlan(Run{ID: "run", State: domain.StateExecuting}, RunInspection{}, RetrySchedule{RunID: "run", Phase: "state_executing", ControllerState: "executing", Status: RetryScheduleAttention, ReasonCode: RetryReasonBudgetExhausted, FailureClass: RetryFailureUnavailable}); err == nil {
		t.Fatal("unavailable failure outside fresh admission boundary was operator-retryable")
	}
	if err := validateOperatorRetryPlan(Run{ID: "run", State: domain.StateProvisioning}, RunInspection{}, RetrySchedule{RunID: "run", Phase: "state_provisioning", ControllerState: "provisioning", Status: RetryScheduleAttention, ReasonCode: RetryReasonBudgetExhausted, FailureClass: RetryFailureUnavailable}); err == nil {
		t.Fatal("unavailable failure during provisioning was operator-retryable")
	}
	for _, state := range []domain.State{domain.StatePushingBranch, domain.StateBranchPushed, domain.StateOpeningPR, domain.StatePROpen, domain.StateReconcilingReviews, domain.StateReplyingReviewFeedback} {
		deliveryRun := run
		deliveryRun.State = state
		deliverySchedule := schedule
		deliverySchedule.Phase = AutomaticRetryPhaseForRun(deliveryRun)
		deliverySchedule.ControllerState = string(state)
		if err := validateOperatorRetryPlan(deliveryRun, RunInspection{Run: deliveryRun, Attempts: []Attempt{processEvidence}}, deliverySchedule); err == nil {
			t.Fatalf("delivery state %s was operator-retryable without fresh GitHub authority", state)
		}
	}
}
