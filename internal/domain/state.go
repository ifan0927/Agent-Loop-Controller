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
	StateRepairing             State = "repairing"
	StatePROpen                State = "pr_open"
	StateCodeRabbitReview      State = "coderabbit_review"
	StateAwaitingHumanApproval State = "awaiting_human_approval"
	StateMerging               State = "merging"
	StateCleaning              State = "cleaning"
	StateCompleted             State = "completed"
	StateFailed                State = "failed"
)

var allowedTransitions = map[State]map[State]struct{}{
	StateReceived:              set(StateAdmitting),
	StateAdmitting:             set(StateRejected, StateProvisioning, StateFailed),
	StateProvisioning:          set(StateExecuting, StateFailed),
	StateExecuting:             set(StateAwaitingHumanDecision, StateVerifying, StateFailed),
	StateAwaitingHumanDecision: set(StateExecuting, StateFailed),
	StateVerifying:             set(StateFreshReview, StateRepairing, StateFailed),
	StateFreshReview:           set(StateRepairing, StateFailed),
	StateRepairing:             set(StateExecuting, StateFailed),
	StatePROpen:                set(StateCodeRabbitReview, StateFailed),
	StateCodeRabbitReview:      set(StateAwaitingHumanApproval, StateRepairing, StateFailed),
	StateAwaitingHumanApproval: set(StateMerging, StateRepairing, StateFailed),
	StateMerging:               set(StateCleaning, StateFailed),
	StateCleaning:              set(StateCompleted, StateFailed),
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
