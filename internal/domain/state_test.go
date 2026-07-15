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

func TestActiveRepairFlowCanRequireManualIntervention(t *testing.T) {
	for _, state := range []State{StateVerifying, StateFreshReview} {
		if !CanTransition(state, StateManualIntervention) {
			t.Fatalf("expired repair policy must stop %s", state)
		}
	}
}

func TestActionableRequiredCheckReturnsToRepair(t *testing.T) {
	if !CanTransition(StateReconcilingReviews, StateRepairing) {
		t.Fatal("actionable required checks must return the run to repair")
	}
	if CanTransition(StateRepairing, StateAwaitingHumanApproval) {
		t.Fatal("a repair must be reimplemented, verified, and freshly reviewed")
	}
}

func TestMergedRunRequiresLinearCompletionBeforeCleanup(t *testing.T) {
	if !CanTransition(StateMerging, StateAwaitingLinearCompletion) {
		t.Fatal("an observed merge must enter Linear completion reconciliation")
	}
	if CanTransition(StateMerging, StateCleaning) {
		t.Fatal("an observed merge must not bypass authoritative Linear completion")
	}
	if !CanTransition(StateAwaitingLinearCompletion, StateCleaning) || !CanTransition(StateAwaitingLinearCompletion, StateManualIntervention) {
		t.Fatal("Linear completion must either authorize cleanup or require an operator")
	}
}

func TestMergeProtectionWaitIsReadOnlyAndMustRevalidateBeforeRetry(t *testing.T) {
	if !CanTransition(StateMerging, StateAwaitingGitHubMergeability) {
		t.Fatal("a protected merge rejection must enter the narrow GitHub wait")
	}
	if !CanTransition(StateAwaitingGitHubMergeability, StateMerging) || !CanTransition(StateAwaitingGitHubMergeability, StateAwaitingHumanApproval) || !CanTransition(StateAwaitingGitHubMergeability, StateRepairing) {
		t.Fatal("mergeability wait must only resume through fresh GitHub authority")
	}
	if CanTransition(StateAwaitingGitHubMergeability, StateAwaitingLinearCompletion) {
		t.Fatal("waiting for conversation resolution must not imply a merge")
	}
}

func TestManualInterventionHasOnlyTheNarrowOwnedPushRecoveryEdge(t *testing.T) {
	if !CanTransition(StateManualIntervention, StateApprovalReady) {
		t.Fatal("owned push recovery must be able to restore the guarded push gate")
	}
	if CanTransition(StateReceived, StateFailed) || CanTransition(StateManualIntervention, StateFailed) {
		t.Fatal("operator abandonment must use its atomic application-level transition")
	}
	if CanTransition(StateManualIntervention, StatePushingBranch) || CanTransition(StateManualIntervention, StatePROpen) {
		t.Fatal("manual intervention must not resume an external write state directly")
	}
}
