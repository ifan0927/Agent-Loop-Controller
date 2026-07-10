package domain

import "testing"

func TestPROpenIsNotAGenericStateTransition(t *testing.T) {
	if CanTransition(StateVerifying, StatePROpen) {
		t.Fatal("verification must not transition directly to PR open")
	}
	if !CanTransition(StateVerifying, StateFreshReview) {
		t.Fatal("verification must transition to fresh review")
	}
	if CanTransition(StateFreshReview, StatePROpen) {
		t.Fatal("PR open requires the guarded application-level evidence gate")
	}
}

func TestFreshReviewCanReachApprovalReadyButNotPROpen(t *testing.T) {
	if !CanTransition(StateFreshReview, StateApprovalReady) {
		t.Fatal("passing guarded review must be able to reach approval_ready")
	}
	if CanTransition(StateApprovalReady, StatePROpen) {
		t.Fatal("local approval_ready must not imply PR creation")
	}
}

func TestCodeRabbitChangeReturnsToRepair(t *testing.T) {
	if !CanTransition(StateCodeRabbitReview, StateRepairing) {
		t.Fatal("CodeRabbit findings must return the run to repair")
	}
	if CanTransition(StateRepairing, StateAwaitingHumanApproval) {
		t.Fatal("a repair must be reimplemented, verified, and freshly reviewed")
	}
}
