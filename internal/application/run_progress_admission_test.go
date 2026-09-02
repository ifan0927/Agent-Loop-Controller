package application

import (
	"context"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type runProgressEvidenceFixture struct {
	evidence RunProgressEvidence
	reads    int
}

func (s *runProgressEvidenceFixture) ReadRunProgressEvidence(context.Context, string) (RunProgressEvidence, error) {
	s.reads++
	return s.evidence, nil
}

func TestRunProgressAdmissionRequiresCompleteUniqueHistoricalRecovery(t *testing.T) {
	run, complete := settledCIWaitRecoveryEvidence(t)
	validatedAction := *complete.CIWaitRecoveryAction
	validatedAction.Status = OperatorActionStatusValidated
	validatedAction.ResultStatus = OperatorActionResultPending
	validatedAction.ResultingState = ""
	validatedAction.ResultingTransitionSequence = 0
	validatedAction.EvidenceDigest = ""
	validatedAction.OutcomeDigest = ""
	validatedAction.AppliedAt = time.Time{}
	validatedAction.ObservedAt = time.Time{}
	validatedReceipt, err := OperationReceiptForOperatorAction(validatedAction, run.RepositoryBindingDigest)
	if err != nil {
		t.Fatal(err)
	}
	appliedAction := *complete.CIWaitRecoveryAction
	appliedAction.Status = OperatorActionStatusApplied
	appliedAction.ResultStatus = OperatorActionResultApplied
	appliedAction.OutcomeDigest = ""
	appliedAction.ObservedAt = time.Time{}
	appliedReceipt, err := OperationReceiptForOperatorAction(appliedAction, run.RepositoryBindingDigest)
	if err != nil {
		t.Fatal(err)
	}
	pendingReceipt := appliedReceipt
	mismatchedReceipt, err := OperationReceiptForOperatorAction(*complete.CIWaitRecoveryAction, LegacyRunAuthorityDigest("other/repository"))
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		evidence RunProgressEvidence
		allowed  bool
	}{
		{name: "no historical recovery", allowed: true},
		{name: "attention only", evidence: RunProgressEvidence{CIWaitRecoveryAttentionCount: 1, CIWaitRecoveryAttention: complete.CIWaitRecoveryAttention}},
		{name: "validated action", evidence: RunProgressEvidence{CIWaitRecoveryAttentionCount: 1, CIWaitRecoveryAttention: complete.CIWaitRecoveryAttention, CIWaitRecoveryActionCount: 1, CIWaitRecoveryAction: &validatedAction, CIWaitRecoveryReceiptCount: 1, CIWaitRecoveryReceipt: &validatedReceipt, ReceiptSourceActionID: validatedAction.ActionID, RetrySchedule: complete.RetrySchedule}},
		{name: "applied action", evidence: RunProgressEvidence{CIWaitRecoveryAttentionCount: 1, CIWaitRecoveryAttention: complete.CIWaitRecoveryAttention, CIWaitRecoveryActionCount: 1, CIWaitRecoveryAction: &appliedAction, CIWaitRecoveryReceiptCount: 1, CIWaitRecoveryReceipt: &appliedReceipt, ReceiptSourceActionID: appliedAction.ActionID, RetrySchedule: complete.RetrySchedule}},
		{name: "observed missing receipt", evidence: RunProgressEvidence{CIWaitRecoveryAttentionCount: 1, CIWaitRecoveryAttention: complete.CIWaitRecoveryAttention, CIWaitRecoveryActionCount: 1, CIWaitRecoveryAction: complete.CIWaitRecoveryAction, RetrySchedule: complete.RetrySchedule}},
		{name: "observed pending receipt", evidence: RunProgressEvidence{CIWaitRecoveryAttentionCount: 1, CIWaitRecoveryAttention: complete.CIWaitRecoveryAttention, CIWaitRecoveryActionCount: 1, CIWaitRecoveryAction: complete.CIWaitRecoveryAction, CIWaitRecoveryReceiptCount: 1, CIWaitRecoveryReceipt: &pendingReceipt, ReceiptSourceActionID: complete.CIWaitRecoveryAction.ActionID, RetrySchedule: complete.RetrySchedule}},
		{name: "receipt source mismatch", evidence: RunProgressEvidence{CIWaitRecoveryAttentionCount: 1, CIWaitRecoveryAttention: complete.CIWaitRecoveryAttention, CIWaitRecoveryActionCount: 1, CIWaitRecoveryAction: complete.CIWaitRecoveryAction, CIWaitRecoveryReceiptCount: 1, CIWaitRecoveryReceipt: complete.CIWaitRecoveryReceipt, ReceiptSourceActionID: "operator-action-other", RetrySchedule: complete.RetrySchedule}},
		{name: "receipt authority mismatch", evidence: RunProgressEvidence{CIWaitRecoveryAttentionCount: 1, CIWaitRecoveryAttention: complete.CIWaitRecoveryAttention, CIWaitRecoveryActionCount: 1, CIWaitRecoveryAction: complete.CIWaitRecoveryAction, CIWaitRecoveryReceiptCount: 1, CIWaitRecoveryReceipt: &mismatchedReceipt, ReceiptSourceActionID: complete.CIWaitRecoveryAction.ActionID, RetrySchedule: complete.RetrySchedule}},
		{name: "multiple attention", evidence: func() RunProgressEvidence { value := complete; value.CIWaitRecoveryAttentionCount = 2; return value }()},
		{name: "multiple action", evidence: func() RunProgressEvidence { value := complete; value.CIWaitRecoveryActionCount = 2; return value }()},
		{name: "multiple receipt", evidence: func() RunProgressEvidence { value := complete; value.CIWaitRecoveryReceiptCount = 2; return value }()},
		{name: "fully settled", evidence: complete, allowed: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			store := &runProgressEvidenceFixture{evidence: test.evidence}
			admission, err := EvaluateRunProgressAdmission(context.Background(), store, run)
			if err != nil || admission.Allowed != test.allowed || store.reads != 1 {
				t.Fatalf("admission=%+v reads=%d err=%v", admission, store.reads, err)
			}
			if !test.allowed && admission.Reason != RetiredCIWaitRecoveryProgressReason {
				t.Fatalf("reason=%q", admission.Reason)
			}
		})
	}
}

func TestProductionDriverFailsBeforeAnyMutationForUnresolvedHistoricalRecovery(t *testing.T) {
	run := driverRun(domain.StatePROpen)
	run.ProfileID = "repository-profile:owner/repo"
	reader := &driverRunReader{run: run, progress: RunProgressEvidence{CIWaitRecoveryAttentionCount: 1, CIWaitRecoveryAttention: &OperatorAttentionEvent{EventType: OperatorAttentionCIWaitRecovery}}}
	coordinator := &driverCoordinator{runs: reader}
	scheduler := &driverHeavyScheduler{held: true}
	waits := 0
	driver := newDriverForTest(t, reader, coordinator, func(context.Context, time.Duration) error {
		waits++
		return nil
	})
	driver.ports.HeavyWorkScheduler = scheduler

	if _, err := driver.Drive(context.Background(), driverCommand()); err == nil {
		t.Fatal("unresolved historical recovery was allowed to progress")
	}
	if len(coordinator.calls) != 0 || reader.ciObserved != 0 || reader.ciClosed != 0 || scheduler.acquireCalls != 0 || scheduler.releaseCalls != 0 || waits != 0 {
		t.Fatalf("coordinator=%v ciObserved=%d ciClosed=%d acquire=%d release=%d waits=%d", coordinator.calls, reader.ciObserved, reader.ciClosed, scheduler.acquireCalls, scheduler.releaseCalls, waits)
	}
}

func TestProductionCoordinatorLowLevelProgressFailsBeforeExternalReads(t *testing.T) {
	coordinator, store, run := newPushCoordinator(t, domain.StatePROpen)
	store.progress = RunProgressEvidence{CIWaitRecoveryAttentionCount: 1, CIWaitRecoveryAttention: &OperatorAttentionEvent{EventType: OperatorAttentionCIWaitRecovery}}
	linear := coordinator.admission.reader.(*admissionReader)
	controller := coordinator.controller.(*serviceController)
	continueCommand := ProductionContinueCommand{Requester: Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}
	if _, err := coordinator.Continue(context.Background(), continueCommand); err == nil {
		t.Fatal("low-level continue allowed unresolved historical recovery")
	}
	reconcileCommand := ProductionReconcileCommand{Requester: continueCommand.Requester, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}
	if _, err := coordinator.ReconcileGitHub(context.Background(), reconcileCommand, driverGitHubReader{}); err == nil {
		t.Fatal("low-level GitHub reconciliation allowed unresolved historical recovery")
	}
	if linear.calls != 0 || controller.continued != 0 {
		t.Fatalf("linear reads=%d controller continues=%d", linear.calls, controller.continued)
	}
}

func settledCIWaitRecoveryEvidence(t *testing.T) (Run, RunProgressEvidence) {
	t.Helper()
	now := time.Date(2026, 9, 2, 2, 0, 0, 0, time.UTC)
	run := Run{ID: "run-retired-progress", Repository: "owner/repo", ProfileID: "repository-profile:owner/repo", IdempotencyKey: "run-key", RepositoryBindingDigest: LegacyRunAuthorityDigest("owner/repo"), State: domain.StatePROpen}
	schedule := RetrySchedule{RunID: run.ID, Phase: AutomaticRetryPhaseForRun(run), ControllerState: string(run.State), AttemptCount: 1, MaxAttempts: 3, InitialDelay: time.Second, MaximumDelay: 30 * time.Second, FailureClass: RetryFailureTerminal, ReasonCode: RetryReasonTerminal, Status: RetryScheduleSuperseded, AttentionAt: now, CreatedAt: now, UpdatedAt: now.Add(2 * time.Minute)}
	attention := OperatorAttentionEvent{EventKey: "automation:run-retired-progress:ci_wait_recovery", EventType: OperatorAttentionCIWaitRecovery, RunID: run.ID, RepositoryProfileID: run.ProfileID, RepositoryProfileName: run.Repository, ControllerState: string(run.State), ReasonCode: "legacy_ci_topology_drift", AllowedActions: []OperatorAttentionActionID{OperatorAttentionActionRecoverCIWait}, EvidenceDigest: retryAttentionDigest(schedule), OccurredAt: now, ObservedAt: now}
	input := OperatorActionInput{Requester: Requester{ID: "operator", Kind: "github_login", DatabaseID: 33, NodeID: "MDQ6VXNlcjMz", ActorType: "User"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, RunIdempotencyKey: run.IdempotencyKey, TransitionSequence: 10, ActionType: OperatorActionRecoverCIWait, ReasonCode: attention.ReasonCode, AttentionEventKey: attention.EventKey, RequestDigest: NoOperationInputDigest()}
	action := newOperatorActionRecord(input, now.Add(time.Minute))
	action.Status = OperatorActionStatusObserved
	action.ResultStatus = OperatorActionResultSucceeded
	action.ResultingState = run.State
	action.ResultingTransitionSequence = input.TransitionSequence
	action.EvidenceDigest = LegacyRunAuthorityDigest("recovery/evidence")
	action.OutcomeDigest = LegacyRunAuthorityDigest("recovery/outcome")
	action.AppliedAt = now.Add(2 * time.Minute)
	action.ObservedAt = now.Add(3 * time.Minute)
	if err := ValidateOperatorActionRecord(action); err != nil {
		t.Fatal(err)
	}
	receipt, err := OperationReceiptForOperatorAction(action, run.RepositoryBindingDigest)
	if err != nil {
		t.Fatal(err)
	}
	return run, RunProgressEvidence{CIWaitRecoveryAttentionCount: 1, CIWaitRecoveryAttention: &attention, CIWaitRecoveryActionCount: 1, CIWaitRecoveryAction: &action, CIWaitRecoveryReceiptCount: 1, CIWaitRecoveryReceipt: &receipt, ReceiptSourceActionID: action.ActionID, RetrySchedule: &schedule}
}
