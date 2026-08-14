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
	"sync"
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

var errDispatchLeaseRenewalSettled = errors.New("automatic admission lease renewal settled")

var dispatchLeaseAuthorityCheckTimeout = 5 * time.Second

const (
	LinearTodoQueueDecisionNoCandidate         = "no_candidate"
	LinearTodoQueueDecisionActiveRun           = "active_run"
	LinearTodoQueueDecisionIncompleteScan      = "incomplete_scan"
	LinearTodoQueueDecisionSelectedPriority    = "selected_priority"
	LinearTodoQueueDecisionSchedulerAttention  = "scheduler_attention"
	LinearTodoQueueDecisionRetryAttention      = "retry_attention"
	LinearTodoQueueDecisionCapacityFull        = "capacity_full"
	LinearTodoQueueDecisionAdmissionBusy       = "admission_busy"
	LinearTodoQueueDecisionNoEligibleCandidate = "no_eligible_candidate"
	LinearTodoQueueDecisionConfigurationFenced = "configuration_fenced"
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
		LinearTodoQueueDecisionRetryAttention, LinearTodoQueueDecisionCapacityFull, LinearTodoQueueDecisionAdmissionBusy,
		LinearTodoQueueDecisionNoEligibleCandidate, LinearTodoQueueDecisionConfigurationFenced:
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

func schedulingStore(store linearTodoDispatchStore) (SchedulingAuthorityStore, bool) {
	scheduler, ok := store.(SchedulingAuthorityStore)
	return scheduler, ok
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
	CandidateAuthority   LinearTodoCandidateAuthority
	StartAuthority       LinearIssueStartAuthority
	LeaseTTL             time.Duration
	LeaseRenewal         time.Duration
	OwnerNonce           string
	Requester            Requester
	AttentionProfile     OperatorAttentionProfile
	Retry                AutomaticRetryPolicy
	ExternalPollInterval time.Duration
	AdmissionGate        NewAdmissionGate
}

// LinearTodoDispatchResult contains sanitized control-flow evidence. The
// selected task snapshot and Linear prose are deliberately not projected.
type LinearTodoDispatchResult struct {
	Outcome        string                   `json:"outcome"`
	Run            RunResult                `json:"run,omitempty"`
	ScanDigest     string                   `json:"scan_digest,omitempty"`
	Drive          *ProductionDriveResult   `json:"drive,omitempty"`
	Retry          *RetrySchedule           `json:"retry,omitempty"`
	QueueDecision  *LinearTodoQueueDecision `json:"queue_decision,omitempty"`
	NextRunnableAt time.Time                `json:"next_runnable_at,omitempty"`
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
// potentially long local start and Drive handoff it renews its already-held
// lease solely to fence that work; a later caller owns trigger cadence and
// process lifetime.
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
	activeMu   sync.Mutex
	activeRuns map[string]struct{}
}

func NewLinearTodoDispatcher(scanner LinearTodoCandidateScanner, reader LinearIssueReader, resolver LinearAdmissionRepositoryResolver, starter LinearReservedIssueStarter, store linearTodoDispatchStore, controller LocalRunController, driver LinearTodoDispatchDriver, policy LinearTodoDispatchPolicy) (*LinearTodoDispatcher, error) {
	if scanner == nil || reader == nil || resolver == nil || starter == nil || store == nil || controller == nil || driver == nil || policy.AdmissionGate == nil {
		return nil, errors.New("Linear Todo dispatcher dependencies are required")
	}
	if err := validateLinearTodoDispatchPolicy(policy); err != nil {
		return nil, err
	}
	policy.Retry = policy.Retry.normalized()
	if policy.ExternalPollInterval <= 0 {
		policy.ExternalPollInterval = 30 * time.Second
	}
	return &LinearTodoDispatcher{scanner: scanner, reader: reader, resolver: resolver, starter: starter, store: store, controller: controller, driver: driver, policy: policy, now: func() time.Time { return time.Now().UTC() }, leaseTicks: newDispatchLeaseTicker, activeRuns: map[string]struct{}{}}, nil
}

func validateLinearTodoDispatchPolicy(policy LinearTodoDispatchPolicy) error {
	if err := (LinearIssueStartAuthority{TeamID: policy.CandidateAuthority.TeamID, TeamKey: policy.CandidateAuthority.TeamKey, TodoState: policy.CandidateAuthority.TodoState, InProgressState: policy.CandidateAuthority.InProgressState}).validate(); err != nil {
		return errors.New("Linear Todo dispatch candidate authority is invalid")
	}
	if err := policy.StartAuthority.validate(); err != nil || policy.CandidateAuthority.TeamID != policy.StartAuthority.TeamID || policy.CandidateAuthority.TeamKey != policy.StartAuthority.TeamKey || !stateMatches(policy.CandidateAuthority.TodoState, policy.StartAuthority.TodoState) || !stateMatches(policy.CandidateAuthority.InProgressState, policy.StartAuthority.InProgressState) || policy.CandidateAuthority.MaxCandidates < 1 || policy.CandidateAuthority.MaxCandidates > 100 || policy.CandidateAuthority.MaxPages < 1 || policy.CandidateAuthority.MaxPages > 20 {
		return errors.New("Linear Todo dispatch workflow authority is invalid")
	}
	if policy.LeaseTTL < 30*time.Second || policy.LeaseTTL > MaxLinearTodoAdmissionLeaseTTL ||
		policy.LeaseRenewal <= 0 || policy.LeaseRenewal > policy.LeaseTTL/2 ||
		strings.TrimSpace(policy.OwnerNonce) == "" || policy.Requester.ID == "" || policy.Requester.Kind != "github_login" {
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

// Dispatch performs one durable admission/recovery decision under the short
// global admission lease. It never retries a scan or looks for another candidate
// after ambiguity, a failed reservation, a mutation conflict, or a driver
// conflict.
func (d *LinearTodoDispatcher) Dispatch(ctx context.Context) (LinearTodoDispatchResult, error) {
	now := d.clock()
	// The in-process claim is checked before the short persisted admission
	// lease so sibling per-run supervisors can make progress concurrently.
	if _, concurrencyEnabled := schedulingStore(d.store); concurrencyEnabled && len(d.activeRunIDs()) > 0 {
		if result, handled, err := d.dispatchExistingRunWithoutAdmissionLease(ctx); handled {
			return result, err
		}
	}
	lease, acquired, err := d.store.AcquireLinearTodoAdmissionLease(ctx, d.policy.OwnerNonce, d.policy.LeaseTTL, now)
	if err != nil {
		return LinearTodoDispatchResult{}, classifyServiceError(err)
	}
	if !acquired {
		if _, concurrencyEnabled := schedulingStore(d.store); concurrencyEnabled {
			return withQueueDecision(LinearTodoDispatchResult{Outcome: LinearTodoDispatchWaiting}, queueDecision(LinearTodoQueueDecisionAdmissionBusy, 0, false)), nil
		}
		result, attentionErr := d.schedulerAttention(ctx, d.policy.AttentionProfile, "lease_conflict", dispatchEvidence("lease_conflict"))
		return withQueueDecision(result, queueDecision(LinearTodoQueueDecisionSchedulerAttention, 0, false)), attentionErr
	}
	defer func() {
		if lease.Namespace == "" {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = d.store.ReleaseLinearTodoAdmissionLease(cleanup, lease)
	}()
	if err := d.store.CloseInactiveCIWaits(ctx, now); err != nil {
		return LinearTodoDispatchResult{}, classifyServiceError(err)
	}
	var scheduled []SchedulingRun
	if scheduler, ok := schedulingStore(d.store); ok {
		scheduled, err = scheduler.ReconcileSchedulingAuthorities(ctx, now)
		if err != nil {
			result, attentionErr := d.schedulerAttention(ctx, d.policy.AttentionProfile, "scheduling_authority_conflict", dispatchEvidence("scheduling_reconciliation_conflict"))
			return withQueueDecision(result, queueDecision(LinearTodoQueueDecisionSchedulerAttention, 0, false)), attentionErr
		}
	}
	if _, concurrencyEnabled := schedulingStore(d.store); !concurrencyEnabled {
		blocking, handled, retryErr := d.blockingRetry(ctx)
		if retryErr != nil {
			return LinearTodoDispatchResult{}, retryErr
		}
		if handled {
			return withQueueDecision(blocking, queueDecision(LinearTodoQueueDecisionActiveRun, 0, true)), nil
		}
	}

	runs, err := d.store.ListNonterminalRuns(ctx)
	if err != nil {
		return LinearTodoDispatchResult{}, classifyServiceError(err)
	}
	var persistedNextRunnableAt time.Time
	if len(runs) > 0 {
		activeRuns := d.activeRunIDs()
		run, found, quarantined := nextScheduledRun(runs, scheduled, activeRuns, d.clock())
		if quarantined {
			result, attentionErr := d.runsAttention(ctx, runs, "repository_authority_conflict", dispatchEvidence("duplicate_nonterminal_repository"))
			return withQueueDecision(result, queueDecision(LinearTodoQueueDecisionActiveRun, 0, true)), attentionErr
		}
		if !found {
			persistedNextRunnableAt = earliestScheduledDeadline(scheduled, activeRuns, d.clock())
			capacity, capacityErr := d.capacity(ctx)
			if capacityErr != nil {
				return LinearTodoDispatchResult{}, capacityErr
			}
			if capacity.Available == 0 {
				result := LinearTodoDispatchResult{Outcome: LinearTodoDispatchWaiting, NextRunnableAt: persistedNextRunnableAt}
				return withQueueDecision(result, queueDecision(LinearTodoQueueDecisionActiveRun, 0, true)), nil
			}
		} else {
			if !d.claimRun(run.ID) {
				return withQueueDecision(LinearTodoDispatchResult{Outcome: LinearTodoDispatchWaiting}, queueDecision(LinearTodoQueueDecisionActiveRun, 0, true)), nil
			}
			defer d.releaseRunClaim(run.ID)
			permit, permitHeld, permitErr := d.prepareRunPermit(ctx, run)
			if permitErr != nil {
				result, attentionErr := d.runAttention(ctx, run, "scheduling_authority_conflict", dispatchEvidence("heavy_permit_conflict", run.ID))
				return withQueueDecision(result, queueDecision(LinearTodoQueueDecisionActiveRun, 0, true)), attentionErr
			}
			if HeavyWorkRequired(run.State) && !permitHeld {
				return withQueueDecision(LinearTodoDispatchResult{Outcome: LinearTodoDispatchWaiting, Run: projectRunResult(run)}, queueDecision(LinearTodoQueueDecisionActiveRun, 0, true)), nil
			}
			phase := AutomaticRetryPhaseForRun(run)
			schedule, scheduleFound, scheduleErr := d.store.GetRetrySchedule(ctx, run.ID, phase)
			if scheduleErr != nil {
				return LinearTodoDispatchResult{}, classifyServiceError(scheduleErr)
			}
			if scheduleFound {
				if schedule.Status == RetryScheduleAttention {
					result, attentionErr := d.retryAttention(ctx, run, schedule)
					d.releaseRunPermit(ctx, permit, run, result)
					return withQueueDecision(result, queueDecision(LinearTodoQueueDecisionActiveRun, 0, true)), attentionErr
				}
				if d.clock().Before(schedule.NextEligibleAt) {
					result := retryWaitResult(run, schedule)
					d.releaseRunPermit(ctx, permit, run, result)
					if err := d.deferRetryRun(ctx, run.ID, schedule.NextEligibleAt); err != nil {
						return LinearTodoDispatchResult{}, err
					}
					return withQueueDecision(result, queueDecision(LinearTodoQueueDecisionActiveRun, 0, true)), nil
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
				d.releaseRunPermit(ctx, permit, run, retryResult)
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
			d.releaseRunPermit(ctx, permit, run, result)
			if err := d.deferExternalRun(ctx, run, &result); err != nil {
				return LinearTodoDispatchResult{}, err
			}
			return withQueueDecision(result, queueDecision(LinearTodoQueueDecisionActiveRun, 0, true)), nil
		}
	}
	if _, concurrencyEnabled := schedulingStore(d.store); !concurrencyEnabled {
		if orphan, handled, orphanErr := d.orphanRetryAttention(ctx); orphanErr != nil {
			return LinearTodoDispatchResult{}, orphanErr
		} else if handled {
			return withQueueDecision(orphan, queueDecision(LinearTodoQueueDecisionRetryAttention, 0, false)), nil
		}
	}
	capacity, capacityErr := d.capacity(ctx)
	if capacityErr != nil {
		return LinearTodoDispatchResult{}, capacityErr
	}
	if capacity.Available == 0 {
		result := LinearTodoDispatchResult{Outcome: LinearTodoDispatchWaiting, NextRunnableAt: persistedNextRunnableAt}
		return withQueueDecision(result, queueDecision(LinearTodoQueueDecisionCapacityFull, 0, false)), nil
	}
	admission, gateErr := d.policy.AdmissionGate.CheckNewAdmission(ctx)
	if gateErr != nil || !admission.Allowed {
		result := LinearTodoDispatchResult{Outcome: LinearTodoDispatchWaiting, NextRunnableAt: persistedNextRunnableAt}
		return withQueueDecision(result, queueDecision(LinearTodoQueueDecisionConfigurationFenced, 0, false)), nil
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
		if scheduler, ok := schedulingStore(d.store); ok {
			capacity, capacityErr := d.capacity(ctx)
			if capacityErr != nil {
				return LinearTodoDispatchResult{}, capacityErr
			}
			if snapshotErr := scheduler.SaveQueueSnapshot(ctx, QueueSnapshot{Digest: scan.Digest, ObservedAt: scan.ObservedAt, EffectiveCapacityIdentity: capacity.EffectiveIdentity}); snapshotErr != nil {
				return d.scanAttentionWithDecision(ctx, "incomplete_authority", dispatchEvidence("queue_snapshot_persistence_failed", scan.Digest), queueDecision(LinearTodoQueueDecisionIncompleteScan, 0, false))
			}
		}
		result := LinearTodoDispatchResult{Outcome: LinearTodoDispatchNoCandidate, ScanDigest: scan.Digest, NextRunnableAt: persistedNextRunnableAt}
		return withQueueDecision(result, queueDecision(LinearTodoQueueDecisionNoCandidate, 0, false)), nil
	}

	selected, found, leaseLost, selectionErr := d.readAndSelect(ctx, &lease, scan, runs)
	if selectionErr != nil {
		evidence := "queue_snapshot_persistence_failed"
		if errors.Is(selectionErr, errLinearTodoSelectionAmbiguous) {
			evidence = "authoritative_candidate_ambiguous"
		}
		return d.scanAttentionWithDecision(ctx, "incomplete_authority", dispatchEvidence(evidence, scan.Digest), queueDecision(LinearTodoQueueDecisionIncompleteScan, len(scan.Candidates), false))
	}
	if leaseLost {
		result, attentionErr := d.schedulerAttention(ctx, d.policy.AttentionProfile, "lease_lost", dispatchEvidence("lease_lost_before_candidate_read", scan.Digest))
		return withQueueDecision(result, queueDecision(LinearTodoQueueDecisionSchedulerAttention, len(scan.Candidates), false)), attentionErr
	}
	if !found {
		if _, concurrencyEnabled := schedulingStore(d.store); concurrencyEnabled {
			result := LinearTodoDispatchResult{Outcome: LinearTodoDispatchWaiting, ScanDigest: scan.Digest, NextRunnableAt: persistedNextRunnableAt}
			return withQueueDecision(result, queueDecision(LinearTodoQueueDecisionNoEligibleCandidate, len(scan.Candidates), false)), nil
		}
		return d.scanAttentionWithDecision(ctx, "incomplete_authority", dispatchEvidence("no_authoritatively_valid_candidate", scan.Digest), queueDecision(LinearTodoQueueDecisionIncompleteScan, len(scan.Candidates), false))
	}
	result, driveErr := d.reserveStartAndDrive(ctx, &lease, selected, scan.Digest)
	if driveErr != nil {
		run, runErr := d.currentRetryRun(ctx, mustReservedRun(selected.snapshot, selected.repository))
		if runErr != nil {
			return LinearTodoDispatchResult{}, runErr
		}
		retryResult, retryErr := d.handleRunFailure(ctx, run, AutomaticRetryPhaseForRun(run), RetrySchedule{}, false, retryFailureEvidenceCursor{}, driveErr)
		if retryResult.QueueDecision == nil {
			retryResult = withQueueDecision(retryResult, selectedPriorityQueueDecision(len(scan.Candidates), selected.candidate))
		}
		retryResult.NextRunnableAt = earlierRunnableAt(retryResult.NextRunnableAt, persistedNextRunnableAt)
		return retryResult, retryErr
	}
	if result.QueueDecision == nil {
		result = withQueueDecision(result, selectedPriorityQueueDecision(len(scan.Candidates), selected.candidate))
	}
	result.NextRunnableAt = earlierRunnableAt(result.NextRunnableAt, persistedNextRunnableAt)
	return result, nil
}

func earlierRunnableAt(left, right time.Time) time.Time {
	if left.IsZero() || !right.IsZero() && right.Before(left) {
		return right
	}
	return left
}

func (d *LinearTodoDispatcher) dispatchExistingRunWithoutAdmissionLease(ctx context.Context) (LinearTodoDispatchResult, bool, error) {
	runs, err := d.store.ListNonterminalRuns(ctx)
	if err != nil {
		return LinearTodoDispatchResult{}, true, classifyServiceError(err)
	}
	runnable := runs[:0]
	for _, run := range runs {
		if run.State != domain.StateReceived {
			runnable = append(runnable, run)
		}
	}
	runs = runnable
	if len(runs) == 0 {
		return LinearTodoDispatchResult{}, false, nil
	}
	scheduled := []SchedulingRun(nil)
	if scheduler, ok := schedulingStore(d.store); ok {
		scheduled, err = scheduler.ReconcileSchedulingAuthorities(ctx, d.clock())
		if err != nil {
			return LinearTodoDispatchResult{}, true, classifyServiceError(err)
		}
	}
	run, found, quarantined := nextScheduledRun(runs, scheduled, d.activeRunIDs(), d.clock())
	if quarantined {
		result, attentionErr := d.runsAttention(ctx, runs, "repository_authority_conflict", dispatchEvidence("duplicate_nonterminal_repository"))
		return withQueueDecision(result, queueDecision(LinearTodoQueueDecisionActiveRun, 0, true)), true, attentionErr
	}
	if !found && len(runs) > 0 {
		// Every existing run may already be claimed by a sibling supervisor. The
		// caller must continue to the short admission path so an idle repository
		// can use remaining capacity.
		return LinearTodoDispatchResult{}, false, nil
	}
	if !found || !d.claimRun(run.ID) {
		return LinearTodoDispatchResult{}, false, nil
	}
	defer d.releaseRunClaim(run.ID)
	permit, held, permitErr := d.prepareRunPermit(ctx, run)
	if permitErr != nil {
		return LinearTodoDispatchResult{}, true, classifyServiceError(permitErr)
	}
	if HeavyWorkRequired(run.State) && !held {
		return withQueueDecision(LinearTodoDispatchResult{Outcome: LinearTodoDispatchWaiting, Run: projectRunResult(run)}, queueDecision(LinearTodoQueueDecisionActiveRun, 0, true)), true, nil
	}
	phase := AutomaticRetryPhaseForRun(run)
	schedule, scheduleFound, scheduleErr := d.store.GetRetrySchedule(ctx, run.ID, phase)
	if scheduleErr != nil {
		return LinearTodoDispatchResult{}, true, classifyServiceError(scheduleErr)
	}
	if scheduleFound {
		if schedule.Status == RetryScheduleAttention {
			result, attentionErr := d.retryAttention(ctx, run, schedule)
			d.releaseRunPermit(ctx, permit, run, result)
			return withQueueDecision(result, queueDecision(LinearTodoQueueDecisionActiveRun, 0, true)), true, attentionErr
		}
		if d.clock().Before(schedule.NextEligibleAt) {
			result := retryWaitResult(run, schedule)
			d.releaseRunPermit(ctx, permit, run, result)
			if err := d.deferRetryRun(ctx, run.ID, schedule.NextEligibleAt); err != nil {
				return LinearTodoDispatchResult{}, true, err
			}
			return withQueueDecision(result, queueDecision(LinearTodoQueueDecisionActiveRun, 0, true)), true, nil
		}
	}
	beforeResume, inspectErr := d.store.Inspect(ctx, run.ID)
	if inspectErr != nil {
		return LinearTodoDispatchResult{}, true, classifyServiceError(inspectErr)
	}
	failureCursor := retryFailureEvidenceCursorFor(beforeResume)
	result, driveErr := d.driveDirect(ctx, run)
	if driveErr != nil {
		failureRun, failureRunErr := d.currentRetryRun(ctx, run)
		if failureRunErr != nil {
			return LinearTodoDispatchResult{}, true, failureRunErr
		}
		retryResult, retryErr := d.handleRunFailure(ctx, failureRun, AutomaticRetryPhaseForRun(failureRun), schedule, scheduleFound, failureCursor, driveErr)
		d.releaseRunPermit(ctx, permit, run, retryResult)
		return withQueueDecision(retryResult, queueDecision(LinearTodoQueueDecisionActiveRun, 0, true)), true, retryErr
	}
	if result.Outcome == LinearTodoDispatchDriven && scheduleFound {
		if cleared, clearErr := d.store.ClearRetrySchedule(ctx, run.ID, phase, schedule.AttemptCount); clearErr != nil {
			return LinearTodoDispatchResult{}, true, classifyServiceError(clearErr)
		} else if !cleared {
			retryResult, retryErr := d.handleRunFailure(ctx, run, phase, schedule, scheduleFound, retryFailureEvidenceCursor{}, formatRetryScheduleConflict(run.ID, phase))
			return withQueueDecision(retryResult, queueDecision(LinearTodoQueueDecisionActiveRun, 0, true)), true, retryErr
		}
	}
	d.releaseRunPermit(ctx, permit, run, result)
	if err := d.deferExternalRun(ctx, run, &result); err != nil {
		return LinearTodoDispatchResult{}, true, err
	}
	return withQueueDecision(result, queueDecision(LinearTodoQueueDecisionActiveRun, 0, true)), true, nil
}

func nextScheduledRun(runs []Run, scheduled []SchedulingRun, active map[string]struct{}, now time.Time) (Run, bool, bool) {
	byID := make(map[string]Run, len(runs))
	for _, run := range runs {
		byID[run.ID] = run
	}
	if len(scheduled) == 0 {
		for _, run := range runs {
			if _, busy := active[run.ID]; !busy {
				return run, true, false
			}
		}
		return Run{}, false, false
	}
	for _, item := range scheduled {
		if _, busy := active[item.RunID]; busy {
			continue
		}
		if item.Quarantined {
			return Run{}, false, true
		}
	}
	slices.SortFunc(scheduled, func(left, right SchedulingRun) int {
		if order := left.RunnableSince.Compare(right.RunnableSince); order != 0 {
			return order
		}
		return strings.Compare(left.RunID, right.RunID)
	})
	for _, item := range scheduled {
		if _, busy := active[item.RunID]; busy {
			continue
		}
		if item.RunnableSince.After(now) {
			continue
		}
		if item.SupervisorState == "waiting" || item.SupervisorState == "running" || item.SupervisorState == "external_wait" {
			if run, ok := byID[item.RunID]; ok {
				return run, true, false
			}
		}
	}
	return Run{}, false, false
}

func earliestScheduledDeadline(scheduled []SchedulingRun, active map[string]struct{}, now time.Time) time.Time {
	var earliest time.Time
	for _, item := range scheduled {
		if _, busy := active[item.RunID]; busy || item.Quarantined || !item.RunnableSince.After(now) {
			continue
		}
		if item.SupervisorState != "waiting" && item.SupervisorState != "running" && item.SupervisorState != "external_wait" {
			continue
		}
		if earliest.IsZero() || item.RunnableSince.Before(earliest) {
			earliest = item.RunnableSince
		}
	}
	return earliest
}

func (d *LinearTodoDispatcher) deferExternalRun(ctx context.Context, run Run, result *LinearTodoDispatchResult) error {
	state := run.State
	if result.Drive != nil && result.Drive.Run.State != "" {
		state = result.Drive.Run.State
	} else if result.Run.State != "" {
		state = result.Run.State
	}
	if HeavyWorkRequired(state) || TerminalRunState(state) || state == domain.StateAwaitingHumanDecision || state == domain.StateManualIntervention {
		return nil
	}
	scheduler, ok := schedulingStore(d.store)
	if !ok {
		return nil
	}
	now := d.clock()
	runnableAt := now.Add(d.policy.ExternalPollInterval)
	deferred, err := scheduler.DeferSchedulingRun(ctx, run.ID, runnableAt, now)
	if err != nil {
		return classifyServiceError(err)
	}
	if !deferred {
		return serviceError(ErrorConflict, "external wait scheduling authority changed", nil)
	}
	result.NextRunnableAt = runnableAt
	return nil
}

func (d *LinearTodoDispatcher) deferRetryRun(ctx context.Context, runID string, runnableAt time.Time) error {
	scheduler, ok := schedulingStore(d.store)
	if !ok {
		return nil
	}
	now := d.clock()
	deferred, err := scheduler.DeferSchedulingRun(ctx, runID, runnableAt, now)
	if err != nil {
		return classifyServiceError(err)
	}
	if !deferred {
		return serviceError(ErrorConflict, "retry scheduling authority changed", nil)
	}
	return nil
}

func (d *LinearTodoDispatcher) activeRunIDs() map[string]struct{} {
	d.activeMu.Lock()
	defer d.activeMu.Unlock()
	result := make(map[string]struct{}, len(d.activeRuns))
	for runID := range d.activeRuns {
		result[runID] = struct{}{}
	}
	return result
}

func (d *LinearTodoDispatcher) claimRun(runID string) bool {
	d.activeMu.Lock()
	defer d.activeMu.Unlock()
	if _, exists := d.activeRuns[runID]; exists {
		return false
	}
	d.activeRuns[runID] = struct{}{}
	return true
}

func (d *LinearTodoDispatcher) releaseRunClaim(runID string) {
	d.activeMu.Lock()
	defer d.activeMu.Unlock()
	delete(d.activeRuns, runID)
}

func (d *LinearTodoDispatcher) capacity(ctx context.Context) (CapacityProjection, error) {
	if scheduler, ok := schedulingStore(d.store); ok {
		projection, err := scheduler.Capacity(ctx, d.clock())
		if err != nil {
			return CapacityProjection{}, classifyServiceError(err)
		}
		return projection, nil
	}
	return CapacityProjection{ConfiguredCapacity: 1, EffectiveCapacity: 1, Available: 1, EffectiveIdentity: "legacy-singleton", ObservedAt: d.clock()}, nil
}

func (d *LinearTodoDispatcher) acquireRunPermit(ctx context.Context, run Run) (HeavyPermit, bool, error) {
	if !HeavyWorkRequired(run.State) {
		return HeavyPermit{}, false, nil
	}
	scheduler, ok := schedulingStore(d.store)
	if !ok {
		return HeavyPermit{}, true, nil
	}
	return scheduler.AcquireHeavyPermit(ctx, run.ID, d.policy.OwnerNonce, d.clock())
}

func (d *LinearTodoDispatcher) prepareRunPermit(ctx context.Context, run Run) (HeavyPermit, bool, error) {
	permit, held, err := d.acquireRunPermit(ctx, run)
	if errors.Is(err, ErrHeavyPermitProcessReconciliationRequired) {
		reconciler, ok := d.controller.(InterruptedRunReconciler)
		if !ok {
			return HeavyPermit{}, false, err
		}
		if reconcileErr := reconciler.ReconcileInterruptedRun(ctx, run.ID); reconcileErr != nil {
			return HeavyPermit{}, false, reconcileErr
		}
		return d.acquireRunPermit(ctx, run)
	}
	if err != nil || !held {
		return permit, held, err
	}
	if reconciler, ok := d.controller.(InterruptedRunReconciler); ok {
		if reconcileErr := reconciler.ReconcileInterruptedRun(ctx, run.ID); reconcileErr != nil {
			d.releaseRunPermit(ctx, permit, run, LinearTodoDispatchResult{Outcome: LinearTodoDispatchWaiting, Run: projectRunResult(run)})
			return HeavyPermit{}, false, reconcileErr
		}
	}
	return permit, true, nil
}

func (d *LinearTodoDispatcher) releaseRunPermit(ctx context.Context, permit HeavyPermit, run Run, result LinearTodoDispatchResult) {
	if permit.RunID == "" {
		return
	}
	state := run.State
	if result.Drive != nil && result.Drive.Run.State != "" {
		state = result.Drive.Run.State
	} else if result.Run.State != "" {
		state = result.Run.State
	}
	reason := "external_wait"
	if TerminalRunState(state) {
		reason = "terminal"
	} else if state == domain.StateManualIntervention {
		reason = "manual_intervention"
	} else if state == domain.StateAwaitingHumanDecision || state == domain.StateAwaitingHumanApproval {
		reason = "human_wait"
	} else if result.Outcome == LinearTodoDispatchRetryWait || result.Outcome == LinearTodoDispatchRetryScheduled || result.Outcome == LinearTodoDispatchAttention && result.Retry != nil {
		reason = "retry_delay"
	} else if HeavyWorkRequired(state) {
		return
	}
	scheduler, ok := schedulingStore(d.store)
	if !ok {
		return
	}
	releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = scheduler.ReleaseHeavyPermit(releaseCtx, permit, reason, d.clock())
}

type linearTodoDispatchCandidate struct {
	candidate  LinearTodoCandidate
	snapshot   linearAdmissionSnapshot
	repository LocalRepository
}

var errLinearTodoSelectionAmbiguous = errors.New("Linear Todo selection requires attention")

func (d *LinearTodoDispatcher) readAndSelect(ctx context.Context, lease *LinearTodoAdmissionLease, scan LinearTodoCandidateScan, runs []Run) (linearTodoDispatchCandidate, bool, bool, error) {
	candidates := slices.Clone(scan.Candidates)
	slices.SortFunc(candidates, compareLinearTodoCandidates)
	activeRepositories := make(map[string]struct{}, len(runs))
	for _, run := range runs {
		activeRepositories[run.RepositoryBindingDigest] = struct{}{}
	}
	capacity, capacityErr := d.capacity(ctx)
	if capacityErr != nil {
		return linearTodoDispatchCandidate{}, false, false, capacityErr
	}
	projections := make([]QueueCandidateProjection, 0, len(candidates))
	var selected linearTodoDispatchCandidate
	selectedSet := false
	stopSelection := false
	for _, candidate := range candidates {
		projection := QueueCandidateProjection{IssueUUID: candidate.IssueID, TeamKey: candidate.TeamKey, IssueSequence: candidate.IssueSequence, Priority: candidate.Priority, Classification: QueueCandidateWaiting, ReasonCode: "awaiting_authoritative_read"}
		repository, repositoryFound := candidateAdmissionRepository(candidate, d.resolver)
		blockedByRepository := false
		if repositoryFound {
			projection.RepositoryProfileID = repository.ProfileID
			projection.RepositoryBindingDigest = repository.RepositoryBindingDigest
			_, blockedByRepository = activeRepositories[repository.RepositoryBindingDigest]
			if eligibilityProvider, ok := d.resolver.(LinearAdmissionRepositoryEligibility); ok {
				eligibility := eligibilityProvider.RepositoryEligibility(repository)
				if eligibility.Validate() != nil {
					projection.Classification, projection.ReasonCode = QueueCandidateAmbiguous, "repository_eligibility_ambiguous"
					projections = append(projections, projection)
					stopSelection = true
					break
				}
				if eligibility.Status == RepositoryEligibilityDisabled {
					projection.Classification, projection.ReasonCode = QueueCandidateRepositoryDisabled, eligibility.ReasonCode
					projections = append(projections, projection)
					continue
				}
			}
		} else {
			projection.Classification, projection.ReasonCode = QueueCandidateInvalid, "repository_binding_invalid"
			projections = append(projections, projection)
			continue
		}
		if selectedSet || stopSelection {
			projections = append(projections, projection)
			continue
		}
		if !d.renewLease(ctx, lease) {
			return linearTodoDispatchCandidate{}, false, true, nil
		}
		var source LinearTaskSource
		var readErr error
		for attempt := 0; attempt < 3; attempt++ {
			source, _, readErr = d.reader.ReadIssue(ctx, candidate.Identifier)
			if readErr == nil {
				break
			}
		}
		if readErr != nil {
			projection.Classification, projection.ReasonCode = QueueCandidateAmbiguous, "authoritative_read_ambiguous"
			projections = append(projections, projection)
			stopSelection = true
			break
		}
		if !sameLinearTodoCandidateSource(candidate, source, d.policy.CandidateAuthority) {
			if definitiveCandidateIdentityInvalid(candidate, source, d.policy.CandidateAuthority) {
				projection.Classification, projection.ReasonCode = QueueCandidateInvalid, "definitive_source_invalidity"
				projections = append(projections, projection)
				continue
			}
			projection.Classification, projection.ReasonCode = QueueCandidateDrift, "ranking_or_source_drift"
			projections = append(projections, projection)
			stopSelection = true
			break
		}
		snapshot, authoritativeRepository, err := admitLinearTask(source, d.resolver)
		if err != nil {
			projection.Classification, projection.ReasonCode = QueueCandidateInvalid, "definitive_admission_invalidity"
			projections = append(projections, projection)
			continue
		}
		if authoritativeRepository.RepositoryBindingDigest != repository.RepositoryBindingDigest {
			projection.Classification, projection.ReasonCode = QueueCandidateDrift, "repository_source_drift"
			projections = append(projections, projection)
			stopSelection = true
			break
		}
		projection.RepositoryProfileID = authoritativeRepository.ProfileID
		projection.RepositoryBindingDigest = authoritativeRepository.RepositoryBindingDigest
		if blockedByRepository {
			projection.Classification, projection.ReasonCode = QueueCandidateBlockedByActiveRepository, "active_repository_slot"
			projections = append(projections, projection)
			continue
		}
		projection.Classification, projection.ReasonCode = QueueCandidateSelected, "highest_ranked_eligible"
		projections = append(projections, projection)
		selected = linearTodoDispatchCandidate{candidate: candidate, snapshot: snapshot, repository: authoritativeRepository}
		selectedSet = true
	}
	for len(projections) < len(candidates) {
		candidate := candidates[len(projections)]
		projections = append(projections, QueueCandidateProjection{IssueUUID: candidate.IssueID, TeamKey: candidate.TeamKey, IssueSequence: candidate.IssueSequence, Priority: candidate.Priority, Classification: QueueCandidateWaiting, ReasonCode: "lower_ranked"})
	}
	if scheduler, ok := schedulingStore(d.store); ok {
		if err := scheduler.SaveQueueSnapshot(ctx, QueueSnapshot{Digest: scan.Digest, ObservedAt: scan.ObservedAt, EffectiveCapacityIdentity: capacity.EffectiveIdentity, Candidates: projections}); err != nil {
			return linearTodoDispatchCandidate{}, false, false, err
		}
	}
	if !selectedSet {
		if stopSelection {
			return linearTodoDispatchCandidate{}, false, false, errLinearTodoSelectionAmbiguous
		}
		return linearTodoDispatchCandidate{}, false, false, nil
	}
	return selected, true, false, nil
}

func definitiveCandidateIdentityInvalid(candidate LinearTodoCandidate, source LinearTaskSource, authority LinearTodoCandidateAuthority) bool {
	if source.Provider != "linear" || source.Team.ID != authority.TeamID || source.Team.Key != authority.TeamKey || source.IssueID != candidate.IssueID || source.Identifier != candidate.Identifier {
		return true
	}
	// Leaving the configured Todo state or current cycle is definitive
	// ineligibility, not ranking drift. This commonly happens after a sibling
	// dispatch moved the prior winner to In Progress from the same scan.
	if !stateMatches(source.State, authority.TodoState) || !source.Cycle.IsActive || source.Cycle.ID == "" {
		return true
	}
	return false
}

func candidateAdmissionRepository(candidate LinearTodoCandidate, resolver LinearAdmissionRepositoryResolver) (LocalRepository, bool) {
	var selected LocalRepository
	found := false
	for _, label := range candidate.RepositoryLabels {
		repository, ok := resolver.ResolveLinearAdmissionRepository(label.Name)
		if !ok {
			continue
		}
		if found && repository.RepositoryBindingDigest != selected.RepositoryBindingDigest {
			return LocalRepository{}, false
		}
		selected, found = repository, true
	}
	return selected, found
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
	if !d.claimRun(candidate.snapshot.Task.RunID) {
		return LinearTodoDispatchResult{Outcome: LinearTodoDispatchWaiting}, nil
	}
	defer d.releaseRunClaim(candidate.snapshot.Task.RunID)
	if !d.renewLease(ctx, lease) {
		return d.schedulerAttention(ctx, dispatcherProfile(candidate.repository, d.policy.AttentionProfile), "lease_lost", dispatchEvidence("lease_lost", scanDigest))
	}
	input := linearTodoDispatchInput(candidate.snapshot, candidate.repository)
	capacity, capacityErr := d.capacity(ctx)
	if capacityErr != nil {
		return LinearTodoDispatchResult{}, capacityErr
	}
	decisionID := digestLinear([]byte(strings.Join([]string{scanDigest, candidate.candidate.IssueID, candidate.repository.RepositoryBindingDigest, strconv.FormatInt(lease.Version, 10)}, ":")))
	scheduling := SchedulingReservation{OwnerNonce: d.policy.OwnerNonce, CapacityIdentity: capacity.EffectiveIdentity, Capacity: capacity.EffectiveCapacity, RunnableSince: d.clock(), DecisionID: decisionID, IssueSequence: candidate.candidate.IssueSequence, Priority: candidate.candidate.Priority, RepositoryProfileID: candidate.repository.ProfileID}
	reserved, journal, created, err := d.store.ReserveLinearTodoAdmission(ctx, LinearTodoAdmissionReservation{Lease: *lease, ScanDigest: scanDigest, IssueUUID: candidate.candidate.IssueID, Input: input, Scheduling: scheduling})
	if err != nil {
		if leaseErr := d.requireLease(ctx, *lease); leaseErr != nil {
			return d.schedulerAttention(ctx, dispatcherProfile(candidate.repository, d.policy.AttentionProfile), "lease_lost", dispatchEvidence("lease_lost_during_reservation", scanDigest))
		}
		return LinearTodoDispatchResult{}, classifyServiceError(err)
	}
	if !created {
		return withQueueDecision(LinearTodoDispatchResult{Outcome: LinearTodoDispatchWaiting, ScanDigest: scanDigest}, queueDecision(LinearTodoQueueDecisionCapacityFull, 0, false)), nil
	}
	permit, _, permitErr := d.acquireRunPermit(ctx, reserved)
	if permitErr != nil {
		return d.runAttention(ctx, reserved, "scheduling_authority_conflict", dispatchEvidence("initial_heavy_permit_conflict", reserved.ID))
	}
	result, startErr := d.startAndDrive(ctx, lease, reserved, journal, input)
	if startErr != nil && permit.RunID != "" {
		if scheduler, ok := schedulingStore(d.store); ok {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = scheduler.ReleaseHeavyPermit(releaseCtx, permit, "shutdown_after_process_exit", d.clock())
		}
	}
	d.releaseRunPermit(ctx, permit, reserved, result)
	if startErr == nil {
		if err := d.deferExternalRun(ctx, reserved, &result); err != nil {
			return LinearTodoDispatchResult{}, err
		}
	}
	return result, startErr
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
	if _, concurrencyEnabled := schedulingStore(d.store); !concurrencyEnabled {
		leaseScope := d.startLeaseRenewal(ctx, lease)
		defer leaseScope.settle()
		startedRun, err := command.Start(leaseScope.ctx, StartCommand{Requester: d.policy.Requester, RepositorySelection: input.Task.Repository, IdempotencyKey: input.IdempotencyKey, Input: input})
		if err != nil {
			if result, handled, settleErr := d.settleLeaseScope(ctx, leaseScope, run, "lease_lost_during_local_start"); handled {
				return result, settleErr
			}
			return LinearTodoDispatchResult{}, err
		}
		persisted, err := d.store.GetRun(leaseScope.ctx, run.ID)
		if err != nil || persisted.ID != run.ID || persisted.Repository != run.Repository || persisted.IdempotencyKey != run.IdempotencyKey || startedRun.Run.RunID != run.ID {
			if result, handled, settleErr := d.settleLeaseScope(ctx, leaseScope, run, "lease_lost_after_local_start"); handled {
				return result, settleErr
			}
			return d.runAttention(ctx, run, "admission_authority_conflict", dispatchEvidence("post_start_proof_conflict", run.ID))
		}
		return d.driveWithLeaseScope(ctx, leaseScope, persisted)
	}
	if !d.releaseAdmissionLease(ctx, lease) {
		return d.schedulerAttention(ctx, d.profileForRun(run), "lease_lost", dispatchEvidence("lease_release_before_local_start", run.ID))
	}
	startedRun, err := command.Start(withHeavyPermitOwner(ctx, d.policy.OwnerNonce), StartCommand{Requester: d.policy.Requester, RepositorySelection: input.Task.Repository, IdempotencyKey: input.IdempotencyKey, Input: input})
	if err != nil {
		return LinearTodoDispatchResult{}, err
	}
	persisted, err := d.store.GetRun(ctx, run.ID)
	if err != nil || persisted.ID != run.ID || persisted.Repository != run.Repository || persisted.IdempotencyKey != run.IdempotencyKey || startedRun.Run.RunID != run.ID {
		return d.runAttention(ctx, run, "admission_authority_conflict", dispatchEvidence("post_start_proof_conflict", run.ID))
	}
	return d.driveDirect(ctx, persisted)
}

func (d *LinearTodoDispatcher) drive(ctx context.Context, lease *LinearTodoAdmissionLease, run Run) (LinearTodoDispatchResult, error) {
	if !d.renewLease(ctx, lease) {
		return d.schedulerAttention(ctx, d.profileForRun(run), "lease_lost", dispatchEvidence("lease_lost_before_driver", run.ID))
	}
	if _, concurrencyEnabled := schedulingStore(d.store); !concurrencyEnabled {
		leaseScope := d.startLeaseRenewal(ctx, lease)
		defer leaseScope.settle()
		return d.driveWithLeaseScope(ctx, leaseScope, run)
	}
	if !d.releaseAdmissionLease(ctx, lease) {
		return d.schedulerAttention(ctx, d.profileForRun(run), "lease_lost", dispatchEvidence("lease_release_before_driver", run.ID))
	}
	return d.driveDirect(ctx, run)
}

func (d *LinearTodoDispatcher) releaseAdmissionLease(ctx context.Context, lease *LinearTodoAdmissionLease) bool {
	if lease == nil || lease.Namespace == "" {
		return false
	}
	released, err := d.store.ReleaseLinearTodoAdmissionLease(ctx, *lease)
	if err != nil || !released {
		return false
	}
	*lease = LinearTodoAdmissionLease{}
	return true
}

func (d *LinearTodoDispatcher) driveDirect(ctx context.Context, run Run) (LinearTodoDispatchResult, error) {
	result, err := d.driver.Drive(ctx, ProductionDriveCommand{Requester: d.policy.Requester, RunID: run.ID, Repository: run.Repository, IdempotencyKey: run.IdempotencyKey})
	if err != nil {
		return LinearTodoDispatchResult{}, err
	}
	return LinearTodoDispatchResult{Outcome: LinearTodoDispatchDriven, Run: projectRunResult(run), Drive: &result}, nil
}

type dispatchLeaseScope struct {
	ctx         context.Context
	cancelWork  context.CancelCauseFunc
	cancelRenew context.CancelCauseFunc
	done        chan struct{}
	leaseLost   chan struct{}
	stopTicks   func()
	settleOnce  sync.Once
	settled     bool
	authority   chan struct{}
	leaseMu     sync.Mutex
	lease       LinearTodoAdmissionLease
	targetLease *LinearTodoAdmissionLease
}

func (d *LinearTodoDispatcher) startLeaseRenewal(ctx context.Context, lease *LinearTodoAdmissionLease) *dispatchLeaseScope {
	workCtx, cancelWork := context.WithCancelCause(ctx)
	renewCtx, cancelRenew := context.WithCancelCause(workCtx)
	ticks, stopTicks := d.leaseTicks(d.policy.LeaseRenewal)
	scope := &dispatchLeaseScope{
		ctx:         workCtx,
		cancelWork:  cancelWork,
		cancelRenew: cancelRenew,
		done:        make(chan struct{}),
		leaseLost:   make(chan struct{}),
		stopTicks:   stopTicks,
		authority:   make(chan struct{}, 1),
		lease:       *lease,
		targetLease: lease,
	}
	scope.authority <- struct{}{}
	go func() {
		defer close(scope.done)
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticks:
				if !scope.acquireAuthority(renewCtx) {
					return
				}
				attemptCtx, cancelAttempt := context.WithTimeout(renewCtx, d.policy.LeaseRenewal)
				next, renewed, err := d.renewLeaseAttempt(attemptCtx, scope.currentLease())
				cancelAttempt()
				if err != nil && context.Cause(renewCtx) == errDispatchLeaseRenewalSettled && errors.Is(err, context.Canceled) {
					scope.releaseAuthority()
					return
				}
				if err != nil || !renewed {
					scope.releaseAuthority()
					close(scope.leaseLost)
					cancelWork(errors.New("automatic admission lease renewal was lost"))
					return
				}
				scope.updateLease(next)
				scope.releaseAuthority()
			}
		}
	}()
	return scope
}

func (s *dispatchLeaseScope) lost() bool {
	select {
	case <-s.leaseLost:
		return true
	default:
		return false
	}
}

func (s *dispatchLeaseScope) currentLease() LinearTodoAdmissionLease {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	return s.lease
}

func (s *dispatchLeaseScope) updateLease(lease LinearTodoAdmissionLease) {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	s.lease = lease
}

func (s *dispatchLeaseScope) acquireAuthority(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-s.authority:
		return true
	}
}

func (s *dispatchLeaseScope) releaseAuthority() {
	s.authority <- struct{}{}
}

func (s *dispatchLeaseScope) settle() bool {
	s.settleOnce.Do(func() {
		s.stopTicks()
		s.cancelRenew(errDispatchLeaseRenewalSettled)
		<-s.done
		s.leaseMu.Lock()
		*s.targetLease = s.lease
		s.leaseMu.Unlock()
		s.settled = true
	})
	return s.settled
}

func (d *LinearTodoDispatcher) settleLeaseScope(ctx context.Context, leaseScope *dispatchLeaseScope, run Run, evidence string) (LinearTodoDispatchResult, bool, error) {
	settled := leaseScope.settle()
	if err := ctx.Err(); err != nil {
		return LinearTodoDispatchResult{}, true, err
	}
	if !settled || leaseScope.lost() {
		result, err := d.schedulerAttention(ctx, d.profileForRun(run), "lease_lost", dispatchEvidence(evidence, run.ID))
		return result, true, err
	}
	held, heldErr := d.scopedLeaseHeld(ctx, leaseScope)
	if err := ctx.Err(); err != nil {
		return LinearTodoDispatchResult{}, true, err
	}
	if heldErr != nil || !held {
		result, err := d.schedulerAttention(ctx, d.profileForRun(run), "lease_lost", dispatchEvidence(evidence, run.ID))
		return result, true, err
	}
	return LinearTodoDispatchResult{}, false, nil
}

func (d *LinearTodoDispatcher) scopedLeaseHeld(ctx context.Context, leaseScope *dispatchLeaseScope) (bool, error) {
	heldCtx, cancelHeld := context.WithTimeout(ctx, dispatchLeaseAuthorityCheckTimeout)
	defer cancelHeld()
	if !leaseScope.acquireAuthority(heldCtx) {
		return false, heldCtx.Err()
	}
	defer leaseScope.releaseAuthority()
	return d.store.LinearTodoAdmissionLeaseHeld(heldCtx, leaseScope.currentLease(), d.clock())
}

func (d *LinearTodoDispatcher) driveWithLeaseScope(ctx context.Context, leaseScope *dispatchLeaseScope, run Run) (LinearTodoDispatchResult, error) {
	if leaseScope.lost() || leaseScope.ctx.Err() != nil {
		if result, handled, settleErr := d.settleLeaseScope(ctx, leaseScope, run, "lease_lost_before_driver"); handled {
			return result, settleErr
		}
	}
	held, heldErr := d.scopedLeaseHeld(ctx, leaseScope)
	if err := ctx.Err(); err != nil {
		leaseScope.settle()
		return LinearTodoDispatchResult{}, err
	}
	if heldErr != nil || !held {
		leaseScope.settle()
		result, err := d.schedulerAttention(ctx, d.profileForRun(run), "lease_lost", dispatchEvidence("lease_lost_before_driver", run.ID))
		return result, err
	}
	result, err := d.driver.Drive(leaseScope.ctx, ProductionDriveCommand{Requester: d.policy.Requester, RunID: run.ID, Repository: run.Repository, IdempotencyKey: run.IdempotencyKey})
	// Stop new ticks and join an already-running renewal before accepting the
	// driver outcome. Lease loss is authoritative even when Drive returned
	// success or an unrelated error while the final CAS was still in flight.
	if settled, handled, settleErr := d.settleLeaseScope(ctx, leaseScope, run, "lease_lost_during_driver"); handled {
		return settled, settleErr
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
	next, renewed, err := d.renewLeaseAttempt(ctx, *lease)
	if err != nil || !renewed {
		return false
	}
	*lease = next
	return true
}

func (d *LinearTodoDispatcher) renewLeaseAttempt(ctx context.Context, lease LinearTodoAdmissionLease) (LinearTodoAdmissionLease, bool, error) {
	next, renewed, err := d.store.RenewLinearTodoAdmissionLease(ctx, lease, d.policy.LeaseTTL, d.clock())
	if err != nil || !renewed || next.Namespace != LinearTodoAdmissionLeaseNamespace || next.OwnerNonce != lease.OwnerNonce || next.Version <= lease.Version {
		return LinearTodoAdmissionLease{}, false, err
	}
	return next, true, nil
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
	return LinearTodoDispatchResult{Outcome: LinearTodoDispatchRetryWait, Run: projectRunResult(run), Retry: &schedule, NextRunnableAt: schedule.NextEligibleAt}
}

func retryScheduledResult(run Run, schedule RetrySchedule) LinearTodoDispatchResult {
	return LinearTodoDispatchResult{Outcome: LinearTodoDispatchRetryScheduled, Run: projectRunResult(run), Retry: &schedule, NextRunnableAt: schedule.NextEligibleAt}
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
