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

func TestCodeRabbitChangeReturnsToRepair(t *testing.T) {
	if !CanTransition(StateCodeRabbitReview, StateRepairing) {
		t.Fatal("CodeRabbit findings must return the run to repair")
	}
	if CanTransition(StateRepairing, StateAwaitingHumanApproval) {
		t.Fatal("a repair must be reimplemented, verified, and freshly reviewed")
	}
}
