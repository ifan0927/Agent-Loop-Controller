package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

const (
	LinearTodoDispatchNoCandidate    = "no_candidate"
	LinearTodoDispatchDriven         = "driven"
	LinearTodoDispatchAttention      = "attention_required"
	LinearTodoDispatchWaiting        = "waiting"
	LinearTodoDispatchRetryWait      = "retry_wait"
	LinearTodoDispatchRetryScheduled = "retry_scheduled"
)

const (
	LinearTodoQueueDecisionNoCandidate        = "no_candidate"
	LinearTodoQueueDecisionActiveRun          = "active_run"
	LinearTodoQueueDecisionIncompleteScan     = "incomplete_scan"
	LinearTodoQueueDecisionSelectedPriority   = "selected_priority"
	LinearTodoQueueDecisionSchedulerAttention = "scheduler_attention"
	LinearTodoQueueDecisionRetryAttention     = "retry_attention"
)

// LinearTodoQueueDecision is sanitized, bounded evidence for one admission
// cycle. It never carries issue prose or credentials. CandidateCount is the
// number returned by the bounded candidate
// scan, before authoritative per-issue filtering.
type LinearTodoQueueDecision struct {
	Reason                   string `json:"reason"`
	CandidateCount           int    `json:"candidate_count"`
	SelectedPriority         *int   `json:"selected_priority,omitempty"`
	SelectedTeamKey          string `json:"selected_team_key,omitempty"`
	SelectedIssueSequence    *int   `json:"selected_issue_sequence,omitempty"`
	SelectedIssueUUID        string `json:"selected_issue_uuid,omitempty"`
	ExistingRunPreventedScan bool   `json:"existing_run_prevented_scan"`
}

func (d LinearTodoQueueDecision) Validate() error {
	if d.CandidateCount < 0 {
		return errors.New("queue decision candidate count is invalid")
	}
	switch d.Reason {
	case LinearTodoQueueDecisionNoCandidate, LinearTodoQueueDecisionActiveRun,
		LinearTodoQueueDecisionIncompleteScan, LinearTodoQueueDecisionSelectedPriority, LinearTodoQueueDecisionSchedulerAttention,
		LinearTodoQueueDecisionRetryAttention:
	default:
		return errors.New("queue decision reason is invalid")
	}
	if d.SelectedPriority != nil && (*d.SelectedPriority < 0 || *d.SelectedPriority > 4) {
		return errors.New("queue decision selected priority is invalid")
	}
	activeRun := d.Reason == LinearTodoQueueDecisionActiveRun
	if activeRun {
		if !d.ExistingRunPreventedScan || d.CandidateCount != 0 {
			return errors.New("active run queue decision is contradictory")
		}
	} else if d.ExistingRunPreventedScan {
		return errors.New("queue decision unexpectedly claims an active run")
	}
	if d.Reason == LinearTodoQueueDecisionNoCandidate && d.CandidateCount != 0 {
		return errors.New("no-candidate queue decision has candidates")
	}
	selected := d.Reason == LinearTodoQueueDecisionSelectedPriority
	if selected {
		if d.CandidateCount < 1 || d.SelectedPriority == nil || d.SelectedTeamKey != "IFAN" || d.SelectedIssueSequence == nil || *d.SelectedIssueSequence < 1 || !validLinearUUID(d.SelectedIssueUUID) {
			return errors.New("selected priority queue decision is missing total-order evidence")
		}
	} else if d.SelectedPriority != nil || d.SelectedTeamKey != "" || d.SelectedIssueSequence != nil || d.SelectedIssueUUID != "" {
		return errors.New("queue decision selected rank is unexpected")
	}
	return nil
}

// LinearTodoDispatchDriver is deliberately the existing exact-run driver
// boundary. A dispatch cycle cannot choose a different run through this port.
type LinearTodoDispatchDriver interface {
	Drive(context.Context, ProductionDriveCommand) (ProductionDriveResult, error)
}

type linearTodoDispatchStore interface {
	LinearTodoAdmissionStore
	linearIssueStartStore
	OperatorAttentionPublisher
	RetryScheduleStore
	InactiveCIWaitCloser
}

// InactiveCIWaitCloser repairs the narrow crash window where a run left review
// reconciliation but its exact-head wait was not yet closed. Dispatchers call
// it before any stop attention or early return that bypasses the driver.
type InactiveCIWaitCloser interface {
	CloseInactiveCIWaits(context.Context, time.Time) error
}

// LinearTodoDispatchPolicy contains controller-owned authority for one
// bounded dispatch cycle. It neither schedules a subsequent invocation nor
// contains any Linear mutation authority beyond the configured state change.
type LinearTodoDispatchPolicy struct {
	CandidateAuthority LinearTodoCandidateAuthority
	StartAuthority     LinearIssueStartAuthority
	LeaseTTL           time.Duration
	OwnerNonce         string
	Requester          Requester
	AttentionProfile   OperatorAttentionProfile
	Retry              AutomaticRetryPolicy
}

// LinearTodoDispatchResult contains sanitized control-flow evidence. The
// selected task snapshot and Linear prose are deliberately not projected.
type LinearTodoDispatchResult struct {
	Outcome       string                   `json:"outcome"`
	Run           RunResult                `json:"run,omitempty"`
	ScanDigest    string                   `json:"scan_digest,omitempty"`
	Drive         *ProductionDriveResult   `json:"drive,omitempty"`
	Retry         *RetrySchedule           `json:"retry,omitempty"`
	QueueDecision *LinearTodoQueueDecision `json:"queue_decision,omitempty"`
}

func queueDecision(reason string, candidateCount int, existingRunPreventedScan bool) LinearTodoQueueDecision {
	return LinearTodoQueueDecision{Reason: reason, CandidateCount: candidateCount, ExistingRunPreventedScan: existingRunPreventedScan}
}

func selectedPriorityQueueDecision(candidateCount int, candidate LinearTodoCandidate) LinearTodoQueueDecision {
	priority, sequence := candidate.Priority, candidate.IssueSequence
	return LinearTodoQueueDecision{Reason: LinearTodoQueueDecisionSelectedPriority, CandidateCount: candidateCount, SelectedPriority: &priority, SelectedTeamKey: candidate.TeamKey, SelectedIssueSequence: &sequence, SelectedIssueUUID: candidate.IssueID}
}

func withQueueDecision(result LinearTodoDispatchResult, decision LinearTodoQueueDecision) LinearTodoDispatchResult {
	result.QueueDecision = &decision
	return result
}

// LinearTodoDispatcher advances at most one persisted run. It is intentionally
// a single cycle: it has no poll, CLI, or transport concern. During one
// potentially long Drive call it may renew its already-held lease solely to
// fence that call; a later caller owns trigger cadence and process lifetime.
type LinearTodoDispatcher struct {
	scanner    LinearTodoCandidateScanner
	reader     LinearIssueReader
	resolver   LinearAdmissionRepositoryResolver
	starter    LinearReservedIssueStarter
	store      linearTodoDispatchStore
	controller LocalRunController
	driver     LinearTodoDispatchDriver
	policy     LinearTodoDispatchPolicy
	now        func() time.Time
	leaseTicks func(time.Duration) (<-chan time.Time, func())
}

func NewLinearTodoDispatcher(scanner LinearTodoCandidateScanner, reader LinearIssueReader, resolver LinearAdmissionRepositoryResolver, starter LinearReservedIssueStarter, store linearTodoDispatchStore, controller LocalRunController, driver LinearTodoDispatchDriver, policy LinearTodoDispatchPolicy) (*LinearTodoDispatcher, error) {
	if scanner == nil || reader == nil || resolver == nil || starter == nil || store == nil || controller == nil || driver == nil {
		return nil, errors.New("Linear Todo dispatcher dependencies are required")
	}
	if err := validateLinearTodoDispatchPolicy(policy); err != nil {
		return nil, err
	}
	policy.Retry = policy.Retry.normalized()
	return &LinearTodoDispatcher{scanner: scanner, reader: reader, resolver: resolver, starter: starter, store: store, controller: controller, driver: driver, policy: policy, now: func() time.Time { return time.Now().UTC() }, leaseTicks: newDispatchLeaseTicker}, nil
}

func validateLinearTodoDispatchPolicy(policy LinearTodoDispatchPolicy) error {
	if err := (LinearIssueStartAuthority{TeamID: policy.CandidateAuthority.TeamID, TeamKey: policy.CandidateAuthority.TeamKey, TodoState: policy.CandidateAuthority.TodoState, InProgressState: policy.CandidateAuthority.InProgressState}).validate(); err != nil {
		return errors.New("Linear Todo dispatch candidate authority is invalid")
	}
	if err := policy.StartAuthority.validate(); err != nil || policy.CandidateAuthority.TeamID != policy.StartAuthority.TeamID || policy.CandidateAuthority.TeamKey != policy.StartAuthority.TeamKey || !stateMatches(policy.CandidateAuthority.TodoState, policy.StartAuthority.TodoState) || !stateMatches(policy.CandidateAuthority.InProgressState, policy.StartAuthority.InProgressState) || policy.CandidateAuthority.MaxCandidates < 1 || policy.CandidateAuthority.MaxCandidates > 100 || policy.CandidateAuthority.MaxPages < 1 || policy.CandidateAuthority.MaxPages > 20 {
		return errors.New("Linear Todo dispatch workflow authority is invalid")
	}
	if policy.LeaseTTL < 30*time.Second || policy.LeaseTTL > MaxLinearTodoAdmissionLeaseTTL || strings.TrimSpace(policy.OwnerNonce) == "" || policy.Requester.ID == "" || policy.Requester.Kind != "github_login" {
		return errors.New("Linear Todo dispatch lease or requester authority is invalid")
	}
	if err := policy.Retry.normalized().validate(); err != nil {
		return errors.New("Linear Todo dispatch retry policy is invalid")
	}
	if _, err := CandidateScanIncompleteAttentionEvent(dispatchEvidence("policy"), policy.AttentionProfile, "incomplete_authority", dispatchEvidence("profile"), time.Unix(1, 0).UTC()); err != nil {
		return errors.New("Linear Todo dispatch operator attention profile is invalid")
	}
	return nil
}

// Dispatch performs one durable admission/recovery decision under the
// singleton lease. It never retries a scan or looks for another candidate
// after ambiguity, a failed reservation, a mutation conflict, or a driver
// conflict.
func (d *LinearTodoDispatcher) Dispatch(ctx context.Context) (LinearTodoDispatchResult, error) {
	now := d.clock()
	lease, acquired, err := d.store.AcquireLinearTodoAdmissionLease(ctx, d.policy.OwnerNonce, d.policy.LeaseTTL, now)
	if err != nil {
		return LinearTodoDispatchResult{}, classifyServiceError(err)
	}
	if !acquired {
		result, attentionErr := d.schedulerAttention(ctx, d.policy.AttentionProfile, "lease_conflict", dispatchEvidence("lease_conflict"))
		return withQueueDecision(result, queueDecision(LinearTodoQueueDecisionSchedulerAttention, 0, false)), attentionErr
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = d.store.ReleaseLinearTodoAdmissionLease(cleanup, lease)
	}()
	if err := d.store.CloseInactiveCIWaits(ctx, now); err != nil {
		return LinearTodoDispatchResult{}, classifyServiceError(err)
	}
	blocking, handled, err := d.blockingRetry(ctx)
	if err != nil {
		return LinearTodoDispatchResult{}, err
	}
	if handled {
		return withQueueDecision(blocking, queueDecision(LinearTodoQueueDecisionActiveRun, 0, true)), nil
	}

	runs, err := d.store.ListNonterminalRuns(ctx)
	if err != nil {
		return LinearTodoDispatchResult{}, classifyServiceError(err)
	}
	if len(runs) > 1 {
		result, attentionErr := d.runsAttention(ctx, runs, "admission_authority_conflict", dispatchEvidence("multiple_nonterminal_runs"))
		return withQueueDecision(result, queueDecision(LinearTodoQueueDecisionActiveRun, 0, true)), attentionErr
	}
	if len(runs) == 1 {
		run := runs[0]
		phase := AutomaticRetryPhaseForRun(run)
		schedule, scheduleFound, scheduleErr := d.store.GetRetrySchedule(ctx, run.ID, phase)
		if scheduleErr != nil {
			return LinearTodoDispatchResult{}, classifyServiceError(scheduleErr)
		}
		if scheduleFound {
			if schedule.Status == RetryScheduleAttention {
				result, attentionErr := d.retryAttention(ctx, run, schedule)
				return withQueueDecision(result, queueDecision(LinearTodoQueueDecisionActiveRun, 0, true)), attentionErr
			}
			if d.clock().Before(schedule.NextEligibleAt) {
				return withQueueDecision(retryWaitResult(run, schedule), queueDecision(LinearTodoQueueDecisionActiveRun, 0, true)), nil
			}
		}
		beforeResume, inspectErr := d.store.Inspect(ctx, run.ID)
		if inspectErr != nil {
			return LinearTodoDispatchResult{}, classifyServiceError(inspectErr)
		}
		failureCursor := retryFailureEvidenceCursorFor(beforeResume)
		result, resumeErr := d.resume(ctx, &lease, run)
		if resumeErr != nil {
			failureRun, failureRunErr := d.currentRetryRun(ctx, run)
			if failureRunErr != nil {
				return LinearTodoDispatchResult{}, failureRunErr
			}
			if scheduleFound && (failureRun.State != run.State || AutomaticRetryPhaseForRun(failureRun) != phase) {
				attention, attentionErr := d.markRetryAttention(ctx, failureRun, schedule, RetryFailureAuthority, RetryReasonAuthority)
				return withQueueDecision(attention, queueDecision(LinearTodoQueueDecisionActiveRun, 0, true)), attentionErr
			}
			retryResult, retryErr := d.handleRunFailure(ctx, failureRun, AutomaticRetryPhaseForRun(failureRun), schedule, scheduleFound, failureCursor, resumeErr)
			return withQueueDecision(retryResult, queueDecision(LinearTodoQueueDecisionActiveRun, 0, true)), retryErr
		}
		if result.Outcome == LinearTodoDispatchDriven && scheduleFound {
			if cleared, clearErr := d.store.ClearRetrySchedule(ctx, run.ID, phase, schedule.AttemptCount); clearErr != nil {
				return LinearTodoDispatchResult{}, classifyServiceError(clearErr)
			} else if !cleared {
				retryResult, retryErr := d.handleRunFailure(ctx, run, phase, schedule, scheduleFound, retryFailureEvidenceCursor{}, formatRetryScheduleConflict(run.ID, phase))
				return withQueueDecision(retryResult, queueDecision(LinearTodoQueueDecisionActiveRun, 0, true)), retryErr
			}
		}
		return withQueueDecision(result, queueDecision(LinearTodoQueueDecisionActiveRun, 0, true)), nil
	}
	if orphan, handled, orphanErr := d.orphanRetryAttention(ctx); orphanErr != nil {
		return LinearTodoDispatchResult{}, orphanErr
	} else if handled {
		return withQueueDecision(orphan, queueDecision(LinearTodoQueueDecisionRetryAttention, 0, false)), nil
	}

	if !d.renewLease(ctx, &lease) {
		result, attentionErr := d.schedulerAttention(ctx, d.policy.AttentionProfile, "lease_lost", dispatchEvidence("lease_lost_before_scan"))
		return withQueueDecision(result, queueDecision(LinearTodoQueueDecisionSchedulerAttention, 0, false)), attentionErr
	}
	scan, _, err := d.scanner.ListTodoCandidates(ctx, d.policy.CandidateAuthority)
	if err != nil || !validLinearTodoCandidateScan(scan, d.policy.CandidateAuthority) {
		return d.scanAttentionWithDecision(ctx, "incomplete_authority", dispatchEvidence("candidate_scan_incomplete"), queueDecision(LinearTodoQueueDecisionIncompleteScan, len(scan.Candidates), false))
	}
	if len(scan.Candidates) == 0 {
		return withQueueDecision(LinearTodoDispatchResult{Outcome: LinearTodoDispatchNoCandidate, ScanDigest: scan.Digest}, queueDecision(LinearTodoQueueDecisionNoCandidate, 0, false)), nil
	}

	selected, found, leaseLost := d.readAndSelect(ctx, &lease, scan)
	if leaseLost {
		result, attentionErr := d.schedulerAttention(ctx, d.policy.AttentionProfile, "lease_lost", dispatchEvidence("lease_lost_before_candidate_read", scan.Digest))
		return withQueueDecision(result, queueDecision(LinearTodoQueueDecisionSchedulerAttention, len(scan.Candidates), false)), attentionErr
	}
	if !found {
		return d.scanAttentionWithDecision(ctx, "incomplete_authority", dispatchEvidence("no_authoritatively_valid_candidate", scan.Digest), queueDecision(LinearTodoQueueDecisionIncompleteScan, len(scan.Candidates), false))
	}
	result, driveErr := d.reserveStartAndDrive(ctx, &lease, selected, scan.Digest)
	if driveErr != nil {
		run, runErr := d.currentRetryRun(ctx, mustReservedRun(selected.snapshot, selected.repository))
		if runErr != nil {
			return LinearTodoDispatchResult{}, runErr
		}
		retryResult, retryErr := d.handleRunFailure(ctx, run, AutomaticRetryPhaseForRun(run), RetrySchedule{}, false, retryFailureEvidenceCursor{}, driveErr)
		return withQueueDecision(retryResult, selectedPriorityQueueDecision(len(scan.Candidates), selected.candidate)), retryErr
	}
	return withQueueDecision(result, selectedPriorityQueueDecision(len(scan.Candidates), selected.candidate)), nil
}

type linearTodoDispatchCandidate struct {
	candidate  LinearTodoCandidate
	snapshot   linearAdmissionSnapshot
	repository LocalRepository
}

func (d *LinearTodoDispatcher) readAndSelect(ctx context.Context, lease *LinearTodoAdmissionLease, scan LinearTodoCandidateScan) (linearTodoDispatchCandidate, bool, bool) {
	var selected linearTodoDispatchCandidate
	selectedSet := false
	for _, candidate := range scan.Candidates {
		if !d.renewLease(ctx, lease) {
			return linearTodoDispatchCandidate{}, false, true
		}
		source, _, err := d.reader.ReadIssue(ctx, candidate.Identifier)
		if err != nil || !sameLinearTodoCandidateSource(candidate, source, d.policy.CandidateAuthority) {
			// A scan is complete, but a later per-issue read can legitimately be
			// stale, removed, or invalid. It excludes only this candidate; it
			// must not prevent a separately revalidated unique best candidate.
			continue
		}
		snapshot, repository, err := admitLinearTask(source, d.resolver)
		if err != nil {
			continue
		}
		current := linearTodoDispatchCandidate{candidate: candidate, snapshot: snapshot, repository: repository}
		if !selectedSet || compareLinearTodoCandidates(candidate, selected.candidate) < 0 {
			selected, selectedSet = current, true
		}
	}
	if !selectedSet {
		return linearTodoDispatchCandidate{}, false, false
	}
	return selected, true, false
}

func compareLinearTodoCandidates(left, right LinearTodoCandidate) int {
	if rank := linearTodoPriorityRank(left.Priority) - linearTodoPriorityRank(right.Priority); rank != 0 {
		return rank
	}
	if left.IssueSequence < right.IssueSequence {
		return -1
	}
	if left.IssueSequence > right.IssueSequence {
		return 1
	}
	return strings.Compare(left.IssueID, right.IssueID)
}

// Linear represents unprioritized work as zero; it is lower than all explicit
// priorities. The remaining values retain Linear's ascending priority order.
func linearTodoPriorityRank(priority int) int {
	if priority == 0 {
		return 5
	}
	return priority
}

func (d *LinearTodoDispatcher) reserveStartAndDrive(ctx context.Context, lease *LinearTodoAdmissionLease, candidate linearTodoDispatchCandidate, scanDigest string) (LinearTodoDispatchResult, error) {
	if !d.renewLease(ctx, lease) {
		return d.schedulerAttention(ctx, dispatcherProfile(candidate.repository, d.policy.AttentionProfile), "lease_lost", dispatchEvidence("lease_lost", scanDigest))
	}
	input := linearTodoDispatchInput(candidate.snapshot, candidate.repository)
	reserved, journal, created, err := d.store.ReserveLinearTodoAdmission(ctx, LinearTodoAdmissionReservation{Lease: *lease, ScanDigest: scanDigest, IssueUUID: candidate.candidate.IssueID, Input: input})
	if err != nil || !created {
		if leaseErr := d.requireLease(ctx, *lease); leaseErr != nil {
			return d.schedulerAttention(ctx, dispatcherProfile(candidate.repository, d.policy.AttentionProfile), "lease_lost", dispatchEvidence("lease_lost_during_reservation", scanDigest))
		}
		return d.runAttention(ctx, mustReservedRun(candidate.snapshot, candidate.repository), "admission_authority_conflict", dispatchEvidence("reservation_conflict", scanDigest))
	}
	return d.startAndDrive(ctx, lease, reserved, journal, input)
}

func (d *LinearTodoDispatcher) resume(ctx context.Context, lease *LinearTodoAdmissionLease, run Run) (LinearTodoDispatchResult, error) {
	if run.State == domain.StateManualIntervention {
		return d.manualInterventionAttention(ctx, run)
	}
	if run.State == domain.StateAwaitingHumanDecision {
		return d.humanDecisionAttention(ctx, run)
	}
	journal, found, err := d.store.GetLinearTodoAdmissionJournal(ctx, run.ID)
	if err != nil {
		return d.runAttention(ctx, run, "admission_authority_conflict", dispatchEvidence("journal_conflict", run.ID))
	}
	if !found {
		if run.State == domain.StateReceived {
			return d.runAttention(ctx, run, "admission_authority_conflict", dispatchEvidence("missing_reservation", run.ID))
		}
		return d.drive(ctx, lease, run)
	}
	if journal.Status == "manual_intervention" {
		return d.runAttention(ctx, run, "admission_authority_conflict", dispatchEvidence("journal_manual", run.ID, journal.ScanDigest))
	}
	if run.State != domain.StateReceived {
		return d.drive(ctx, lease, run)
	}
	if !d.renewLease(ctx, lease) {
		return d.schedulerAttention(ctx, d.profileForRun(run), "lease_lost", dispatchEvidence("lease_lost_before_adoption", run.ID))
	}
	input, err := linearTodoDispatchInputFromRun(run)
	if err != nil {
		return d.runAttention(ctx, run, "admission_authority_conflict", dispatchEvidence("persisted_reservation_invalid", run.ID))
	}
	adopted, adoptedJournal, adoptedOK, err := d.store.AdoptLinearTodoAdmissionReservation(ctx, LinearTodoAdmissionReservation{Lease: *lease, ScanDigest: journal.ScanDigest, IssueUUID: journal.IssueUUID, Input: input})
	if err != nil || !adoptedOK || adopted.ID != run.ID {
		if leaseErr := d.requireLease(ctx, *lease); leaseErr != nil {
			return d.schedulerAttention(ctx, d.profileForRun(run), "lease_lost", dispatchEvidence("lease_lost_during_adoption", run.ID))
		}
		return d.runAttention(ctx, run, "admission_authority_conflict", dispatchEvidence("reservation_adoption_conflict", run.ID, journal.ScanDigest))
	}
	return d.startAndDrive(ctx, lease, adopted, adoptedJournal, input)
}

func (d *LinearTodoDispatcher) startAndDrive(ctx context.Context, lease *LinearTodoAdmissionLease, run Run, journal LinearTodoAdmissionJournal, input LocalStartInput) (LinearTodoDispatchResult, error) {
	if journal.RunID != run.ID || journal.IssueUUID == "" || journal.ScanDigest == "" || journal.TaskDigest != run.TaskHash || journal.ProfileDigest != run.ProfileDigest {
		return d.runAttention(ctx, run, "admission_authority_conflict", dispatchEvidence("journal_run_conflict", run.ID))
	}
	if !d.renewLease(ctx, lease) {
		return d.schedulerAttention(ctx, d.profileForRun(run), "lease_lost", dispatchEvidence("lease_lost_before_start", run.ID))
	}
	if journal.Status == LinearTodoAdmissionJournalReserved {
		intent, err := linearIssueStartIntent(run, mustLinearSource(input.RawIssueJSON), d.policy.StartAuthority)
		if err != nil {
			return d.runAttention(ctx, run, "admission_authority_conflict", dispatchEvidence("mutation_intent_conflict", run.ID))
		}
		if !d.advanceJournal(ctx, *lease, run.ID, LinearTodoAdmissionJournalReserved, "mutation_intent", intent.IdempotencyDigest, "") {
			if leaseErr := d.requireLease(ctx, *lease); leaseErr != nil {
				return d.schedulerAttention(ctx, d.profileForRun(run), "lease_lost", dispatchEvidence("lease_lost_during_mutation_intent", run.ID))
			}
			return d.runAttention(ctx, run, "admission_authority_conflict", dispatchEvidence("mutation_intent_conflict", run.ID))
		}
		journal.Status, journal.MutationIntentRef = "mutation_intent", intent.IdempotencyDigest
	}
	if journal.Status != "mutation_intent" && journal.Status != "started" {
		return d.runAttention(ctx, run, "admission_authority_conflict", dispatchEvidence("journal_status_conflict", run.ID))
	}

	starter, err := NewLinearReservedIssueStartService(d.reader, d.starter, d.resolver, d.store, d.policy.StartAuthority)
	if err != nil {
		return LinearTodoDispatchResult{}, serviceError(ErrorInternal, "Linear issue start service is unavailable", err)
	}
	if !d.renewLease(ctx, lease) {
		return d.schedulerAttention(ctx, d.profileForRun(run), "lease_lost", dispatchEvidence("lease_lost_before_mutation", run.ID))
	}
	started, startErr := starter.MoveReservedIssueToStarted(ctx, MoveReservedIssueToStartedCommand{RunID: run.ID})
	if startErr != nil || started.Status != "started" {
		_ = d.advanceJournal(ctx, *lease, run.ID, "mutation_intent", "manual_intervention", "", "mutation_conflict")
		return LinearTodoDispatchResult{}, serviceError(ErrorConflict, "Linear issue start mutation authority changed", startErr)
	}
	if !d.renewLease(ctx, lease) {
		return d.schedulerAttention(ctx, d.profileForRun(run), "lease_lost", dispatchEvidence("lease_lost_after_mutation", run.ID))
	}
	if journal.Status == "mutation_intent" && !d.advanceJournal(ctx, *lease, run.ID, "mutation_intent", "started", journal.MutationIntentRef, "") {
		if leaseErr := d.requireLease(ctx, *lease); leaseErr != nil {
			return d.schedulerAttention(ctx, d.profileForRun(run), "lease_lost", dispatchEvidence("lease_lost_during_started_journal", run.ID))
		}
		return d.runAttention(ctx, run, "admission_authority_conflict", dispatchEvidence("start_journal_conflict", run.ID))
	}
	command := NewCommandService(d.controller, d.store)
	if !d.renewLease(ctx, lease) {
		return d.schedulerAttention(ctx, d.profileForRun(run), "lease_lost", dispatchEvidence("lease_lost_before_local_start", run.ID))
	}
	startedRun, err := command.Start(ctx, StartCommand{Requester: d.policy.Requester, RepositorySelection: input.Task.Repository, IdempotencyKey: input.IdempotencyKey, Input: input})
	if err != nil {
		return LinearTodoDispatchResult{}, err
	}
	persisted, err := d.store.GetRun(ctx, run.ID)
	if err != nil || persisted.ID != run.ID || persisted.Repository != run.Repository || persisted.IdempotencyKey != run.IdempotencyKey || startedRun.Run.RunID != run.ID {
		return d.runAttention(ctx, run, "admission_authority_conflict", dispatchEvidence("post_start_proof_conflict", run.ID))
	}
	return d.drive(ctx, lease, persisted)
}

func (d *LinearTodoDispatcher) drive(ctx context.Context, lease *LinearTodoAdmissionLease, run Run) (LinearTodoDispatchResult, error) {
	if !d.renewLease(ctx, lease) {
		return d.schedulerAttention(ctx, d.profileForRun(run), "lease_lost", dispatchEvidence("lease_lost_before_driver", run.ID))
	}
	driveCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	ticks, stopTicks := d.leaseTicks(d.policy.LeaseTTL / 2)
	defer stopTicks()
	driveDone := make(chan struct{})
	renewerDone := make(chan struct{})
	leaseLost := make(chan struct{}, 1)
	go func() {
		defer close(renewerDone)
		for {
			select {
			case <-driveDone:
				return
			case <-driveCtx.Done():
				return
			case <-ticks:
				if !d.renewLease(driveCtx, lease) {
					select {
					case leaseLost <- struct{}{}:
					default:
					}
					cancel(errors.New("automatic admission lease renewal was lost"))
					return
				}
			}
		}
	}()
	result, err := d.driver.Drive(driveCtx, ProductionDriveCommand{Requester: d.policy.Requester, RunID: run.ID, Repository: run.Repository, IdempotencyKey: run.IdempotencyKey})
	close(driveDone)
	<-renewerDone
	select {
	case <-leaseLost:
		return d.schedulerAttention(ctx, d.profileForRun(run), "lease_lost", dispatchEvidence("lease_lost_during_driver", run.ID))
	default:
	}
	if err != nil {
		return LinearTodoDispatchResult{}, err
	}
	return LinearTodoDispatchResult{Outcome: LinearTodoDispatchDriven, Run: projectRunResult(run), Drive: &result}, nil
}

func newDispatchLeaseTicker(interval time.Duration) (<-chan time.Time, func()) {
	ticker := time.NewTicker(interval)
	return ticker.C, ticker.Stop
}

func (d *LinearTodoDispatcher) requireLease(ctx context.Context, lease LinearTodoAdmissionLease) error {
	held, err := d.store.LinearTodoAdmissionLeaseHeld(ctx, lease, d.clock())
	if err != nil {
		return err
	}
	if !held {
		return errors.New("automatic admission lease was lost")
	}
	return nil
}

// renewLease obtains a fresh versioned capability before each potentially long
// operation. It fails closed: an unavailable or lost compare-and-swap never
// leaves a stale worker authorized to continue.
func (d *LinearTodoDispatcher) renewLease(ctx context.Context, lease *LinearTodoAdmissionLease) bool {
	if lease == nil {
		return false
	}
	next, renewed, err := d.store.RenewLinearTodoAdmissionLease(ctx, *lease, d.policy.LeaseTTL, d.clock())
	if err != nil || !renewed || next.Namespace != LinearTodoAdmissionLeaseNamespace || next.OwnerNonce != lease.OwnerNonce || next.Version <= lease.Version {
		return false
	}
	*lease = next
	return true
}

func (d *LinearTodoDispatcher) advanceJournal(ctx context.Context, lease LinearTodoAdmissionLease, runID, from, to, intent, reason string) bool {
	advanced, err := d.store.AdvanceLinearTodoAdmissionJournal(ctx, LinearTodoAdmissionJournalTransition{Lease: lease, RunID: runID, ExpectedStatus: from, NextStatus: to, MutationIntentRef: intent, ReasonCode: reason})
	return err == nil && advanced
}

func (d *LinearTodoDispatcher) scanAttention(ctx context.Context, reason, evidence string) (LinearTodoDispatchResult, error) {
	event, err := CandidateScanIncompleteAttentionEvent(evidence, d.policy.AttentionProfile, reason, evidence, d.clock())
	if err != nil {
		return LinearTodoDispatchResult{}, serviceError(ErrorInternal, "candidate scan attention is invalid", err)
	}
	return d.appendAttention(ctx, event, "")
}

func (d *LinearTodoDispatcher) scanAttentionWithDecision(ctx context.Context, reason, evidence string, decision LinearTodoQueueDecision) (LinearTodoDispatchResult, error) {
	result, attentionErr := d.scanAttention(ctx, reason, evidence)
	return withQueueDecision(result, decision), attentionErr
}

func (d *LinearTodoDispatcher) schedulerAttention(ctx context.Context, profile OperatorAttentionProfile, reason, evidence string) (LinearTodoDispatchResult, error) {
	event, err := SchedulerLeaseAttentionEvent(evidence, profile, reason, evidence, d.clock())
	if err != nil {
		return LinearTodoDispatchResult{}, serviceError(ErrorInternal, "scheduler lease attention is invalid", err)
	}
	return d.appendAttention(ctx, event, "")
}

func (d *LinearTodoDispatcher) runsAttention(ctx context.Context, runs []Run, reason, evidence string) (LinearTodoDispatchResult, error) {
	for _, run := range runs {
		if _, err := d.runAttention(ctx, run, reason, dispatchEvidence(evidence, run.ID)); err != nil {
			return LinearTodoDispatchResult{}, err
		}
	}
	return LinearTodoDispatchResult{Outcome: LinearTodoDispatchAttention}, nil
}

func (d *LinearTodoDispatcher) runAttention(ctx context.Context, run Run, reason, evidence string) (LinearTodoDispatchResult, error) {
	event, err := AdmissionAuthorityConflictAttentionEvent(run, reason, evidence, d.clock())
	if err != nil {
		return d.scanAttention(ctx, "incomplete_authority", evidence)
	}
	return d.appendAttention(ctx, event, "")
}

func (d *LinearTodoDispatcher) manualInterventionAttention(ctx context.Context, run Run) (LinearTodoDispatchResult, error) {
	inspection, err := d.store.Inspect(ctx, run.ID)
	if err != nil {
		return LinearTodoDispatchResult{}, classifyServiceError(err)
	}
	if err := publishManualInterventionAttention(ctx, run, inspection, d.store); err != nil {
		return LinearTodoDispatchResult{}, classifyServiceError(err)
	}
	return LinearTodoDispatchResult{Outcome: LinearTodoDispatchAttention, Run: projectRunResult(run)}, nil
}

func (d *LinearTodoDispatcher) humanDecisionAttention(ctx context.Context, run Run) (LinearTodoDispatchResult, error) {
	inspection, err := d.store.Inspect(ctx, run.ID)
	if err != nil {
		return LinearTodoDispatchResult{}, classifyServiceError(err)
	}
	if err := publishHumanDecisionAttention(ctx, run, inspection, d.store); err != nil {
		return d.runAttention(ctx, run, "admission_authority_conflict", dispatchEvidence("human_decision_evidence_conflict", run.ID))
	}
	return LinearTodoDispatchResult{Outcome: LinearTodoDispatchAttention, Run: projectRunResult(run)}, nil
}

func (d *LinearTodoDispatcher) appendAttention(ctx context.Context, event OperatorAttentionEvent, scanDigest string) (LinearTodoDispatchResult, error) {
	if _, err := d.store.AppendOperatorAttention(ctx, event); err != nil {
		return LinearTodoDispatchResult{}, classifyServiceError(err)
	}
	return LinearTodoDispatchResult{Outcome: LinearTodoDispatchAttention, ScanDigest: scanDigest}, nil
}

func retryWaitResult(run Run, schedule RetrySchedule) LinearTodoDispatchResult {
	return LinearTodoDispatchResult{Outcome: LinearTodoDispatchRetryWait, Run: projectRunResult(run), Retry: &schedule}
}

func retryScheduledResult(run Run, schedule RetrySchedule) LinearTodoDispatchResult {
	return LinearTodoDispatchResult{Outcome: LinearTodoDispatchRetryScheduled, Run: projectRunResult(run), Retry: &schedule}
}

// blockingRetry prevents a scheduler restart from falling through to a fresh
// Linear scan while any durable run/phase retry or attention record exists.
func (d *LinearTodoDispatcher) blockingRetry(ctx context.Context) (LinearTodoDispatchResult, bool, error) {
	schedules, err := d.store.ListRetrySchedules(ctx)
	if err != nil {
		return LinearTodoDispatchResult{}, false, classifyServiceError(err)
	}
	for _, schedule := range schedules {
		if schedule.Status == RetryScheduleSuperseded {
			continue
		}
		run, runErr := d.store.GetRun(ctx, schedule.RunID)
		if runErr != nil {
			return LinearTodoDispatchResult{}, false, classifyServiceError(runErr)
		}
		if run.State == domain.StateManualIntervention {
			result, attentionErr := d.manualInterventionAttention(ctx, run)
			return result, true, attentionErr
		}
		if run.State == domain.StateAwaitingHumanDecision {
			result, attentionErr := d.humanDecisionAttention(ctx, run)
			return result, true, attentionErr
		}
		if run.State == domain.StateCompleted {
			continue
		}
		if schedule.Status == RetryScheduleAttention {
			if retainedTerminalRetryAttention(run, schedule) {
				continue
			}
			result, attentionErr := d.retryAttention(ctx, run, schedule)
			return result, true, attentionErr
		}
		if class, reason, stop := automaticRetryStateStop(run.State); stop {
			attention, attentionErr := d.markRetryAttention(ctx, run, schedule, class, reason)
			return attention, true, attentionErr
		}
		if schedule.Phase != AutomaticRetryPhaseForRun(run) || schedule.ControllerState != string(run.State) {
			attention, attentionErr := d.markRetryAttention(ctx, run, schedule, RetryFailureAuthority, RetryReasonAuthority)
			return attention, true, attentionErr
		}
		if d.clock().Before(schedule.NextEligibleAt) {
			return retryWaitResult(run, schedule), true, nil
		}
	}
	return LinearTodoDispatchResult{}, false, nil
}

func (d *LinearTodoDispatcher) markRetryAttention(ctx context.Context, run Run, current RetrySchedule, class RetryFailureClass, reason string) (LinearTodoDispatchResult, error) {
	schedule, applied, err := d.store.ApplyRetryFailure(ctx, RetryFailureRequest{
		RunID: current.RunID, Phase: current.Phase, ControllerState: domain.State(current.ControllerState), ExpectedAttempt: current.AttemptCount,
		FailureClass: class, ReasonCode: reason, Now: d.clock(), Policy: d.policy.Retry,
	})
	if err != nil {
		return LinearTodoDispatchResult{}, classifyServiceError(err)
	}
	if !applied && schedule.Status != RetryScheduleAttention {
		return LinearTodoDispatchResult{}, formatRetryScheduleConflict(current.RunID, current.Phase)
	}
	return d.retryAttention(ctx, run, schedule)
}

func (d *LinearTodoDispatcher) retryAttention(ctx context.Context, run Run, schedule RetrySchedule) (LinearTodoDispatchResult, error) {
	event, err := AutomaticRetryAttentionEvent(run, schedule)
	if err != nil {
		return LinearTodoDispatchResult{}, serviceError(ErrorInternal, "automatic retry attention is invalid", err)
	}
	result, appendErr := d.appendAttention(ctx, event, "")
	if appendErr != nil {
		return result, appendErr
	}
	result.Retry = &schedule
	return result, nil
}

func (d *LinearTodoDispatcher) handleRunFailure(ctx context.Context, run Run, phase string, existing RetrySchedule, found bool, before retryFailureEvidenceCursor, cause error) (LinearTodoDispatchResult, error) {
	if ctx.Err() != nil {
		return LinearTodoDispatchResult{}, cause
	}
	if run.State == domain.StateManualIntervention {
		return d.manualInterventionAttention(ctx, run)
	}
	if run.State == domain.StateAwaitingHumanDecision {
		return d.humanDecisionAttention(ctx, run)
	}
	if run.State == domain.StateAwaitingHumanApproval {
		return LinearTodoDispatchResult{Outcome: LinearTodoDispatchWaiting, Run: projectRunResult(run)}, nil
	}
	if run.State == domain.StateCompleted {
		return LinearTodoDispatchResult{Outcome: LinearTodoDispatchDriven, Run: projectRunResult(run)}, nil
	}
	class, reason := ClassifyAutomaticRetryFailure(cause)
	if stoppedClass, stoppedReason, stop := automaticRetryStateStop(run.State); stop {
		class, reason = stoppedClass, stoppedReason
	}
	expected := 0
	if found {
		expected = existing.AttemptCount
	}
	evidenceRef := ""
	if class == RetryFailureProcessStart {
		inspection, inspectErr := d.store.Inspect(ctx, run.ID)
		if inspectErr != nil {
			return LinearTodoDispatchResult{}, classifyServiceError(inspectErr)
		}
		var evidenceErr error
		evidenceRef, evidenceErr = processStartFailureEvidenceAfter(inspection, before)
		if evidenceErr != nil {
			return LinearTodoDispatchResult{}, serviceError(ErrorInternal, "process-start retry lacks exact new failure evidence", evidenceErr)
		}
	}
	schedule, applied, err := d.store.ApplyRetryFailure(ctx, RetryFailureRequest{
		RunID: run.ID, Phase: phase, ControllerState: run.State, ExpectedAttempt: expected,
		FailureClass: class, FailureEvidenceRef: evidenceRef, ReasonCode: reason, Now: d.clock(), Policy: d.policy.Retry,
	})
	if err != nil {
		return LinearTodoDispatchResult{}, classifyServiceError(err)
	}
	if !applied && schedule.Status != RetryScheduleScheduled && schedule.Status != RetryScheduleAttention {
		return LinearTodoDispatchResult{}, formatRetryScheduleConflict(run.ID, phase)
	}
	if schedule.Status == RetryScheduleAttention {
		return d.retryAttention(ctx, run, schedule)
	}
	return retryScheduledResult(run, schedule), nil
}

type retryFailureEvidenceCursor struct {
	attemptID      int64
	verificationID int64
}

func retryFailureEvidenceCursorFor(inspection RunInspection) retryFailureEvidenceCursor {
	var cursor retryFailureEvidenceCursor
	for _, attempt := range inspection.Attempts {
		if attempt.ID > cursor.attemptID {
			cursor.attemptID = attempt.ID
		}
	}
	for _, verification := range inspection.Verifications {
		if verification.ID > cursor.verificationID {
			cursor.verificationID = verification.ID
		}
	}
	return cursor
}

func processStartFailureEvidenceAfter(inspection RunInspection, before retryFailureEvidenceCursor) (string, error) {
	type candidate struct {
		ref string
		at  time.Time
	}
	var latest candidate
	for _, attempt := range inspection.Attempts {
		if attempt.ID <= before.attemptID || attempt.RunID != inspection.Run.ID || attempt.Status != "failed" || attempt.ErrorCategory != RetryReasonProcessStart || attempt.FinishedAt.IsZero() {
			continue
		}
		if latest.ref == "" || attempt.FinishedAt.After(latest.at) {
			latest = candidate{ref: fmt.Sprintf("attempt:%d", attempt.ID), at: attempt.FinishedAt}
		}
	}
	for _, verification := range inspection.Verifications {
		if verification.ID <= before.verificationID || verification.RunID != inspection.Run.ID || verification.ProcessOutcome != VerificationOutcomeNotStarted || verification.FailureCategory != RetryReasonProcessStart || verification.CreatedAt.IsZero() {
			continue
		}
		if latest.ref == "" || verification.CreatedAt.After(latest.at) {
			latest = candidate{ref: fmt.Sprintf("verification:%d", verification.ID), at: verification.CreatedAt}
		}
	}
	if latest.ref == "" {
		return "", errors.New("no newly persisted process-start attempt or verification was found")
	}
	return latest.ref, nil
}

func (d *LinearTodoDispatcher) orphanRetryAttention(ctx context.Context) (LinearTodoDispatchResult, bool, error) {
	schedules, err := d.store.ListRetrySchedules(ctx)
	if err != nil {
		return LinearTodoDispatchResult{}, false, classifyServiceError(err)
	}
	for _, schedule := range schedules {
		run, runErr := d.store.GetRun(ctx, schedule.RunID)
		if runErr != nil {
			return LinearTodoDispatchResult{}, false, classifyServiceError(runErr)
		}
		if run.State == domain.StateManualIntervention {
			result, attentionErr := d.manualInterventionAttention(ctx, run)
			return result, true, attentionErr
		}
		if run.State == domain.StateAwaitingHumanDecision {
			result, attentionErr := d.humanDecisionAttention(ctx, run)
			return result, true, attentionErr
		}
		if run.State == domain.StateCompleted {
			continue
		}
		if schedule.Status == RetryScheduleAttention {
			if retainedTerminalRetryAttention(run, schedule) {
				continue
			}
			result, attentionErr := d.retryAttention(ctx, run, schedule)
			return result, true, attentionErr
		}
		if class, reason, stop := automaticRetryStateStop(run.State); stop {
			attention, attentionErr := d.markRetryAttention(ctx, run, schedule, class, reason)
			return attention, true, attentionErr
		}
		result, attentionErr := d.markRetryAttention(ctx, run, schedule, RetryFailureAuthority, RetryReasonAuthority)
		return result, true, attentionErr
	}
	return LinearTodoDispatchResult{}, false, nil
}

func retainedTerminalRetryAttention(run Run, schedule RetrySchedule) bool {
	if schedule.Status != RetryScheduleAttention {
		return false
	}
	return run.State == domain.StateFailed || run.State == domain.StateCompleted || run.State == domain.StateRejected
}

func (d *LinearTodoDispatcher) currentRetryRun(ctx context.Context, fallback Run) (Run, error) {
	run, err := d.store.GetRun(ctx, fallback.ID)
	if err != nil {
		return Run{}, classifyServiceError(err)
	}
	if run.ID != fallback.ID {
		return Run{}, serviceError(ErrorConflict, "retry run identity changed", nil)
	}
	return run, nil
}

func (d *LinearTodoDispatcher) clock() time.Time { return d.now().UTC() }

func validLinearTodoCandidateScan(scan LinearTodoCandidateScan, authority LinearTodoCandidateAuthority) bool {
	if !validOperatorAttentionDigest(scan.Digest) || scan.ObservedAt.IsZero() || len(scan.Candidates) > authority.MaxCandidates {
		return false
	}
	seenIDs, seenIdentifiers, seenSequences := map[string]bool{}, map[string]bool{}, map[int]bool{}
	for _, candidate := range scan.Candidates {
		teamKey, sequence, identifierOK := normalizedLinearIdentifier(candidate.Identifier)
		if !identifierOK || teamKey != authority.TeamKey || candidate.TeamKey != teamKey || candidate.IssueSequence != sequence || !validLinearUUID(candidate.IssueID) || candidate.Priority < 0 || candidate.Priority > 4 || !stateMatches(candidate.State, authority.TodoState) || candidate.Cycle.ID == "" || !candidate.Cycle.IsActive || strings.TrimSpace(candidate.BranchName) == "" || candidate.SourceRevision == "" || !validOperatorAttentionDigest(candidate.SourceDigest) || candidate.CreatedAt.IsZero() || candidate.UpdatedAt.IsZero() || candidate.UpdatedAt.Before(candidate.CreatedAt) || seenIDs[candidate.IssueID] || seenIdentifiers[candidate.Identifier] || seenSequences[candidate.IssueSequence] {
			return false
		}
		seenIDs[candidate.IssueID], seenIdentifiers[candidate.Identifier], seenSequences[candidate.IssueSequence] = true, true, true
	}
	return true
}

func normalizedLinearIdentifier(identifier string) (string, int, bool) {
	separator := strings.LastIndexByte(identifier, '-')
	if separator < 1 || separator == len(identifier)-1 {
		return "", 0, false
	}
	teamKey, rawSequence := identifier[:separator], identifier[separator+1:]
	if strings.TrimSpace(teamKey) != teamKey || strings.TrimSpace(rawSequence) != rawSequence {
		return "", 0, false
	}
	for _, digit := range rawSequence {
		if digit < '0' || digit > '9' {
			return "", 0, false
		}
	}
	sequence, err := strconv.Atoi(rawSequence)
	return teamKey, sequence, err == nil && sequence > 0
}

func sameLinearTodoCandidateSource(candidate LinearTodoCandidate, source LinearTaskSource, authority LinearTodoCandidateAuthority) bool {
	if source.Provider != "linear" || source.IssueID != candidate.IssueID || source.Identifier != candidate.Identifier || source.Team.ID != authority.TeamID || source.Team.Key != authority.TeamKey || source.Team.Key != candidate.TeamKey || !stateMatches(source.State, candidate.State) || source.Cycle != candidate.Cycle || source.BranchName != candidate.BranchName || !source.CreatedAt.Equal(candidate.CreatedAt) || !source.UpdatedAt.Equal(candidate.UpdatedAt) || source.SourceRevision != candidate.SourceRevision || source.SourceRevision != source.UpdatedAt.UTC().Format(time.RFC3339Nano) {
		return false
	}
	if len(source.Labels) != len(candidate.Labels) {
		return false
	}
	labels := append([]LinearLabel(nil), source.Labels...)
	want := append([]LinearLabel(nil), candidate.Labels...)
	slices.SortFunc(labels, func(left, right LinearLabel) int { return strings.Compare(left.ID, right.ID) })
	slices.SortFunc(want, func(left, right LinearLabel) int { return strings.Compare(left.ID, right.ID) })
	return slices.EqualFunc(labels, want, func(left, right LinearLabel) bool { return left == right })
}

func linearTodoDispatchInput(snapshot linearAdmissionSnapshot, repository LocalRepository) LocalStartInput {
	return LocalStartInput{Task: snapshot.Task, RawIssueJSON: snapshot.RawJSON, RawIssueHash: snapshot.RawHash, NormalizedJSON: snapshot.NormalizedJSON, TaskHash: snapshot.TaskHash, IdempotencyKey: snapshot.IdempotencyKey, Repository: repository, RunRoot: repository.RunRoot, WorktreeRoot: repository.WorktreeRoot}
}

func linearTodoDispatchInputFromRun(run Run) (LocalStartInput, error) {
	var source LinearTaskSource
	var task domain.CodingTask
	var repository LocalRepository
	if json.Unmarshal([]byte(run.RawIssueJSON), &source) != nil || json.Unmarshal([]byte(run.NormalizedTaskJSON), &task) != nil || json.Unmarshal([]byte(run.RepositoryConfigJSON), &repository) != nil || source.IssueID == "" || task.RunID != run.ID || task.IssueID != run.IssueID || task.SourceRevision != run.SourceRevision || repository.CanonicalRepository != run.Repository || filepath.Dir(run.ArtifactRoot) == "." || filepath.Dir(run.WorktreePath) == "." {
		return LocalStartInput{}, errors.New("persisted automatic admission snapshot is invalid")
	}
	// ProfileSnapshotJSON is intentionally omitted from RepositoryConfigJSON's
	// public projection. Recovery restores it only from the same persisted run
	// record so CommandService can prove the original profile snapshot exactly.
	repository.ProfileSnapshotJSON = run.ProfileSnapshotJSON
	return LocalStartInput{Task: task, RawIssueJSON: []byte(run.RawIssueJSON), RawIssueHash: run.RawIssueHash, NormalizedJSON: []byte(run.NormalizedTaskJSON), TaskHash: run.TaskHash, IdempotencyKey: run.IdempotencyKey, Repository: repository, RunRoot: filepath.Dir(run.ArtifactRoot), WorktreeRoot: filepath.Dir(run.WorktreePath)}, nil
}

func dispatcherProfile(repository LocalRepository, fallback OperatorAttentionProfile) OperatorAttentionProfile {
	if repository.ProfileID != "" && repository.CanonicalRepository != "" {
		return OperatorAttentionProfile{ID: repository.ProfileID, Name: repository.CanonicalRepository}
	}
	return fallback
}

func (d *LinearTodoDispatcher) profileForRun(run Run) OperatorAttentionProfile {
	profile, err := operatorAttentionProfileForRun(run)
	if err == nil {
		return profile
	}
	return d.policy.AttentionProfile
}

func mustReservedRun(snapshot linearAdmissionSnapshot, repository LocalRepository) Run {
	run, _ := ReservedRunFromAdmissionSnapshot(linearTodoDispatchInput(snapshot, repository))
	run.State = domain.StateReceived
	return run
}

func mustLinearSource(raw []byte) LinearTaskSource {
	var source LinearTaskSource
	_ = json.Unmarshal(raw, &source)
	return source
}

func dispatchEvidence(parts ...string) string {
	value := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(value[:])
}
