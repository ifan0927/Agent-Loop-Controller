package domain

import "fmt"

type State string

const (
	StateReceived              State = "received"
	StateAdmitting             State = "admitting"
	StateRejected              State = "rejected"
	StateProvisioning          State = "provisioning"
	StateExecuting             State = "executing"
	StateAwaitingHumanDecision State = "awaiting_human_decision"
	StateVerifying             State = "verifying"
	StateFreshReview           State = "fresh_review"
	StateApprovalReady         State = "approval_ready"
	StatePushingBranch         State = "pushing_branch"
	StateBranchPushed          State = "branch_pushed"
	StateOpeningPR             State = "opening_pr"
	StateRepairing             State = "repairing"
	StatePROpen                State = "pr_open"
	StateReconcilingReviews    State = "reconciling_reviews"
	StateAwaitingHumanApproval State = "awaiting_human_approval"
	StateMerging               State = "merging"
	StateCleaning              State = "cleaning"
	StateCompleted             State = "completed"
	StateFailed                State = "failed"
	StateManualIntervention    State = "manual_intervention"
)

var allowedTransitions = map[State]map[State]struct{}{
	StateReceived:              set(StateAdmitting),
	StateAdmitting:             set(StateRejected, StateProvisioning, StateFailed),
	StateProvisioning:          set(StateExecuting, StateFailed),
	StateExecuting:             set(StateAwaitingHumanDecision, StateVerifying, StateFailed),
	StateAwaitingHumanDecision: set(StateExecuting, StateFailed),
	StateVerifying:             set(StateFreshReview, StateRepairing, StateFailed),
	StateFreshReview:           set(StateApprovalReady, StateRepairing, StateFailed),
	StateApprovalReady:         set(StatePushingBranch, StateFailed, StateManualIntervention),
	StatePushingBranch:         set(StateBranchPushed, StateFailed, StateManualIntervention),
	StateBranchPushed:          set(StateOpeningPR, StateFailed, StateManualIntervention),
	StateOpeningPR:             set(StatePROpen, StateFailed, StateManualIntervention),
	StateRepairing:             set(StateExecuting, StateVerifying, StateFailed, StateManualIntervention),
	StatePROpen:                set(StateReconcilingReviews, StateFailed, StateManualIntervention),
	StateReconcilingReviews:    set(StateAwaitingHumanApproval, StateRepairing, StateFailed, StateManualIntervention),
	StateAwaitingHumanApproval: set(StateMerging, StateRepairing, StateReconcilingReviews, StateFailed, StateManualIntervention),
	StateMerging:               set(StateCleaning, StateFailed, StateManualIntervention),
	StateCleaning:              set(StateCompleted, StateFailed, StateManualIntervention),
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

func set(states ...State) map[State]struct{} {
	result := make(map[State]struct{}, len(states))
	for _, state := range states {
		result[state] = struct{}{}
	}
	return result
}
