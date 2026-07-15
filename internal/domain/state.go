package domain

import "fmt"

type State string

const (
	StateReceived                   State = "received"
	StateAdmitting                  State = "admitting"
	StateRejected                   State = "rejected"
	StateProvisioning               State = "provisioning"
	StateExecuting                  State = "executing"
	StateAwaitingHumanDecision      State = "awaiting_human_decision"
	StateVerifying                  State = "verifying"
	StateFreshReview                State = "fresh_review"
	StateApprovalReady              State = "approval_ready"
	StatePushingBranch              State = "pushing_branch"
	StateBranchPushed               State = "branch_pushed"
	StateOpeningPR                  State = "opening_pr"
	StateRepairing                  State = "repairing"
	StatePROpen                     State = "pr_open"
	StateReconcilingReviews         State = "reconciling_reviews"
	StateReplyingReviewFeedback     State = "replying_review_feedback"
	StateAwaitingHumanApproval      State = "awaiting_human_approval"
	StateMerging                    State = "merging"
	StateAwaitingGitHubMergeability State = "awaiting_github_mergeability"
	StateAwaitingLinearCompletion   State = "awaiting_linear_completion"
	StateCleaning                   State = "cleaning"
	StateCompleted                  State = "completed"
	StateFailed                     State = "failed"
	StateManualIntervention         State = "manual_intervention"
)

var allowedTransitions = map[State]map[State]struct{}{
	StateReceived:                   set(StateAdmitting, StateFailed),
	StateAdmitting:                  set(StateRejected, StateProvisioning, StateFailed),
	StateProvisioning:               set(StateExecuting, StateFailed),
	StateExecuting:                  set(StateAwaitingHumanDecision, StateVerifying, StateFailed, StateManualIntervention),
	StateAwaitingHumanDecision:      set(StateExecuting, StateFailed),
	StateVerifying:                  set(StateFreshReview, StateRepairing, StateFailed, StateManualIntervention),
	StateFreshReview:                set(StateApprovalReady, StateRepairing, StateFailed, StateManualIntervention),
	StateApprovalReady:              set(StatePushingBranch, StateFailed, StateManualIntervention),
	StatePushingBranch:              set(StateBranchPushed, StateFailed, StateManualIntervention),
	StateBranchPushed:               set(StateOpeningPR, StateFailed, StateManualIntervention),
	StateOpeningPR:                  set(StatePROpen, StateFailed, StateManualIntervention),
	StateRepairing:                  set(StateExecuting, StateVerifying, StateFailed, StateManualIntervention),
	StatePROpen:                     set(StateReconcilingReviews, StateFailed, StateManualIntervention),
	StateReconcilingReviews:         set(StateAwaitingHumanApproval, StateReplyingReviewFeedback, StateRepairing, StateFailed, StateManualIntervention),
	StateReplyingReviewFeedback:     set(StateAwaitingHumanApproval, StateFailed, StateManualIntervention),
	StateAwaitingHumanApproval:      set(StateMerging, StateRepairing, StateReconcilingReviews, StateReplyingReviewFeedback, StateFailed, StateManualIntervention),
	StateMerging:                    set(StateAwaitingGitHubMergeability, StateAwaitingLinearCompletion, StateFailed, StateManualIntervention),
	StateAwaitingGitHubMergeability: set(StateReconcilingReviews, StateMerging, StateAwaitingHumanApproval, StateRepairing, StateFailed, StateManualIntervention),
	StateAwaitingLinearCompletion:   set(StateCleaning, StateFailed, StateManualIntervention),
	StateCleaning:                   set(StateCompleted, StateFailed, StateManualIntervention),
	// This is a narrow application-level recovery edge. It is reachable only
	// after stable Linear revalidation and retained controller-owned PR proof.
	StateManualIntervention: set(StateApprovalReady, StateFailed),
}

func CanTransition(from, to State) bool {
	_, ok := allowedTransitions[from][to]
	return ok
}

func ValidateTransition(from, to State) error {
	if !CanTransition(from, to) {
		return fmt.Errorf("invalid state transition: %s -> %s", from, to)
	}
	return nil
}

// CanRequireManualIntervention permits an external authority conflict to halt
// any non-terminal run without guessing how a human will resolve it.
func CanRequireManualIntervention(from State) bool {
	switch from {
	case StateReceived, StateAdmitting, StateProvisioning, StateExecuting, StateAwaitingHumanDecision,
		StateVerifying, StateFreshReview, StateApprovalReady, StatePushingBranch, StateBranchPushed,
		StateOpeningPR, StateRepairing, StatePROpen, StateReconcilingReviews, StateReplyingReviewFeedback, StateAwaitingHumanApproval,
		StateMerging, StateAwaitingGitHubMergeability, StateAwaitingLinearCompletion, StateCleaning:
		return true
	default:
		return false
	}
}

func set(states ...State) map[State]struct{} {
	result := make(map[State]struct{}, len(states))
	for _, state := range states {
		result[state] = struct{}{}
	}
	return result
}
