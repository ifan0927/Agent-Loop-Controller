package application

import (
	"context"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

const RetiredCIWaitRecoveryProgressReason = "retired_ci_wait_recovery_unresolved"

// RunProgressEvidence is the bounded historical evidence needed to decide
// whether a run may make ordinary progress. Counts are retained separately so
// duplicate legacy authorities fail closed without returning unbounded rows.
type RunProgressEvidence struct {
	CIWaitRecoveryAttentionCount int
	CIWaitRecoveryAttention      *OperatorAttentionEvent
	CIWaitRecoveryActionCount    int
	CIWaitRecoveryAction         *OperatorActionRecord
	CIWaitRecoveryReceiptCount   int
	CIWaitRecoveryReceipt        *OperationReceipt
	ReceiptSourceActionID        string
	RetrySchedule                *RetrySchedule
}

// RunProgressEvidenceStore supplies one read-only snapshot. Implementations
// must not repair, advance, or otherwise settle historical evidence while
// answering this query.
type RunProgressEvidenceStore interface {
	ReadRunProgressEvidence(context.Context, string) (RunProgressEvidence, error)
}

type RunProgressAdmission struct {
	Allowed bool
	Reason  string
}

// EvaluateRunProgressAdmission permits a run with no retired recovery history,
// or one whose unique historical recovery is already fully and consistently
// settled. Every partial, duplicate, or contradictory legacy shape fails
// closed and remains read-only.
func EvaluateRunProgressAdmission(ctx context.Context, store RunProgressEvidenceStore, run Run) (RunProgressAdmission, error) {
	evidence, err := store.ReadRunProgressEvidence(ctx, run.ID)
	if err != nil {
		return RunProgressAdmission{}, err
	}
	if evidence.CIWaitRecoveryAttentionCount == 0 && evidence.CIWaitRecoveryActionCount == 0 && evidence.CIWaitRecoveryReceiptCount == 0 && evidence.RetrySchedule == nil {
		return RunProgressAdmission{Allowed: true}, nil
	}
	if settledHistoricalCIWaitRecovery(run, evidence) {
		return RunProgressAdmission{Allowed: true}, nil
	}
	return RunProgressAdmission{Reason: RetiredCIWaitRecoveryProgressReason}, nil
}

func settledHistoricalCIWaitRecovery(run Run, evidence RunProgressEvidence) bool {
	if evidence.CIWaitRecoveryAttentionCount != 1 || evidence.CIWaitRecoveryActionCount != 1 || evidence.CIWaitRecoveryReceiptCount != 1 || evidence.CIWaitRecoveryAttention == nil || evidence.CIWaitRecoveryAction == nil || evidence.CIWaitRecoveryReceipt == nil || evidence.RetrySchedule == nil {
		return false
	}
	attention, action, receipt, schedule := *evidence.CIWaitRecoveryAttention, *evidence.CIWaitRecoveryAction, *evidence.CIWaitRecoveryReceipt, *evidence.RetrySchedule
	if attention.EventType != OperatorAttentionCIWaitRecovery || attention.RunID != run.ID || attention.RepositoryProfileID != run.ProfileID || attention.RepositoryProfileName != run.Repository || attention.ControllerState != string(action.ExpectedState) || attention.ReasonCode != "legacy_ci_topology_drift" || len(attention.AllowedActions) != 1 || attention.AllowedActions[0] != OperatorAttentionActionRecoverCIWait || attention.EventKey != action.AttentionEventKey {
		return false
	}
	if action.ActionType != OperatorActionRecoverCIWait || action.RunID != run.ID || action.Repository != run.Repository || action.RunIdempotencyKey != run.IdempotencyKey || action.ExpectedState != domain.StatePROpen && action.ExpectedState != domain.StateReconcilingReviews || action.ReasonCode != attention.ReasonCode || action.Status != OperatorActionStatusObserved || action.ResultStatus != OperatorActionResultSucceeded || action.ResultingState != action.ExpectedState || action.ResultingTransitionSequence != action.TransitionSequence {
		return false
	}
	phase := AutomaticRetryPhaseForRun(Run{State: action.ExpectedState})
	if schedule.RunID != run.ID || schedule.Phase != phase || schedule.ControllerState != string(action.ExpectedState) || schedule.Status != RetryScheduleSuperseded || schedule.FailureClass != RetryFailureTerminal || schedule.ReasonCode != RetryReasonTerminal || schedule.FailureEvidenceRef != "" || attention.EvidenceDigest != retryAttentionDigest(schedule) || !sameTime(attention.OccurredAt, schedule.AttentionAt) || !sameTime(attention.ObservedAt, schedule.AttentionAt) {
		return false
	}
	binding := run.RepositoryBindingDigest
	if !validAuthorityDigest(binding) {
		binding = LegacyRunAuthorityDigest(run.Repository)
	}
	expectedReceipt, err := OperationReceiptForOperatorAction(action, binding)
	if err != nil || evidence.ReceiptSourceActionID != action.ActionID || receipt.OperationType != OperationRecoverCIWait || receipt.Scope != ScopeRun || receipt.TargetID != run.ID || receipt.Phase != OperationPhaseObserved || receipt.Outcome != OperationOutcomeSucceeded || !sameOperationReceipt(receipt, expectedReceipt) {
		return false
	}
	return true
}

func sameOperationReceipt(left, right OperationReceipt) bool {
	return left.OperationID == right.OperationID && left.OperationType == right.OperationType && left.Scope == right.Scope && left.TargetID == right.TargetID && left.Requester.Equal(right.Requester) && left.RequestDigest == right.RequestDigest && left.ExpectedAuthorityDigest == right.ExpectedAuthorityDigest && left.TargetBindingDigest == right.TargetBindingDigest && left.Phase == right.Phase && left.Outcome == right.Outcome && left.ResultingAuthorityDigest == right.ResultingAuthorityDigest && left.ResultingState == right.ResultingState && left.ResultingVersion == right.ResultingVersion && left.EvidenceDigest == right.EvidenceDigest && left.ResultDigest == right.ResultDigest && left.AuthorityKey == right.AuthorityKey && left.OperationAnchorDigest == right.OperationAnchorDigest && sameTime(left.AcceptedAt, right.AcceptedAt) && sameTime(left.AppliedAt, right.AppliedAt) && sameTime(left.SettledAt, right.SettledAt)
}

func sameTime(left, right time.Time) bool {
	return left.IsZero() && right.IsZero() || left.Equal(right)
}
