package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

const (
	dispatchTeamID    = "123e4567-e89b-42d3-a456-426614174100"
	dispatchTodoID    = "123e4567-e89b-42d3-a456-426614174101"
	dispatchStartedID = "123e4567-e89b-42d3-a456-426614174102"
)

type dispatchScanner struct {
	scan  LinearTodoCandidateScan
	err   error
	calls int
}

func (s *dispatchScanner) ListTodoCandidates(context.Context, LinearTodoCandidateAuthority) (LinearTodoCandidateScan, []LinearRequestObservation, error) {
	s.calls++
	return s.scan, nil, s.err
}

type dispatchReader struct {
	mu      sync.Mutex
	sources map[string]LinearTaskSource
	started map[string]bool
	errs    map[string]error
	calls   []string
	err     error
}

func (r *dispatchReader) ReadIssue(_ context.Context, identifier string) (LinearTaskSource, []LinearRequestObservation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, identifier)
	if r.err != nil {
		return LinearTaskSource{}, nil, r.err
	}
	if err := r.errs[identifier]; err != nil {
		return LinearTaskSource{}, nil, err
	}
	source := r.sources[identifier]
	if r.started[identifier] {
		source.State = dispatchStartAuthority().InProgressState
		source.UpdatedAt = source.UpdatedAt.Add(time.Second)
		source.SourceRevision = source.UpdatedAt.UTC().Format(time.RFC3339Nano)
		source.ObservedAt = source.UpdatedAt
	}
	return source, []LinearRequestObservation{{Operation: "read_issue", ResponseDigest: dispatchDigest(identifier), ObservedAt: time.Now().UTC()}}, nil
}

type dispatchStarter struct {
	reader *dispatchReader
	err    error
	calls  []LinearIssueStartMutation
}

func (s *dispatchStarter) MoveReservedIssueToStarted(_ context.Context, mutation LinearIssueStartMutation) (LinearIssueStartMutationResult, []LinearRequestObservation, error) {
	s.calls = append(s.calls, mutation)
	if s.err != nil {
		return LinearIssueStartMutationResult{}, nil, s.err
	}
	s.reader.mu.Lock()
	for identifier, source := range s.reader.sources {
		if source.IssueID == mutation.IssueID {
			s.reader.started[identifier] = true
		}
	}
	s.reader.mu.Unlock()
	return LinearIssueStartMutationResult{IssueID: mutation.IssueID, State: dispatchStartAuthority().InProgressState}, nil, nil
}

type dispatchResolver struct{ repository LocalRepository }

func (r dispatchResolver) ResolveLinearAdmissionRepository(label string) (LocalRepository, bool) {
	return r.repository, label == "repo:test"
}

type dispatchRepositoryMap struct {
	repositories map[string]LocalRepository
	disabled     map[string]string
}

func (r dispatchRepositoryMap) ResolveLinearAdmissionRepository(label string) (LocalRepository, bool) {
	repository, found := r.repositories[label]
	return repository, found
}

func (r dispatchRepositoryMap) RepositoryEligibility(repository LocalRepository) RepositoryEligibility {
	if reason := r.disabled[repository.RepositoryBindingDigest]; reason != "" {
		return RepositoryEligibility{Status: RepositoryEligibilityDisabled, ReasonCode: reason}
	}
	return RepositoryEligibility{Status: RepositoryEligibilityEligible}
}

type dispatchSchedulingStore struct {
	*dispatchStore
	projection    CapacityProjection
	scheduled     []SchedulingRun
	snapshots     []QueueSnapshot
	acquireErrors []error
	acquireCalls  int
}

func (s *dispatchSchedulingStore) ConfigureHeavyCapacity(context.Context, int, string, time.Time) (CapacityProjection, error) {
	return s.projection, nil
}

func (s *dispatchSchedulingStore) ReconcileSchedulingAuthorities(context.Context, time.Time) ([]SchedulingRun, error) {
	return slices.Clone(s.scheduled), nil
}

func (s *dispatchSchedulingStore) AcquireHeavyPermit(_ context.Context, runID, owner string, now time.Time) (HeavyPermit, bool, error) {
	s.acquireCalls++
	if len(s.acquireErrors) > 0 {
		err := s.acquireErrors[0]
		s.acquireErrors = s.acquireErrors[1:]
		if err != nil {
			return HeavyPermit{}, false, err
		}
	}
	return HeavyPermit{RunID: runID, OwnerNonce: owner, Version: 1, AcquiredAt: now, UpdatedAt: now}, true, nil
}

func (s *dispatchSchedulingStore) ReleaseHeavyPermit(context.Context, HeavyPermit, string, time.Time) (bool, error) {
	return true, nil
}

func (s *dispatchSchedulingStore) DeferSchedulingRun(_ context.Context, runID string, runnableAt, _ time.Time) (bool, error) {
	for index := range s.scheduled {
		if s.scheduled[index].RunID == runID {
			s.scheduled[index].RunnableSince = runnableAt
			s.scheduled[index].SupervisorState = "external_wait"
			return true, nil
		}
	}
	return false, nil
}

func (s *dispatchSchedulingStore) Capacity(context.Context, time.Time) (CapacityProjection, error) {
	return s.projection, nil
}

func (s *dispatchSchedulingStore) SaveQueueSnapshot(_ context.Context, snapshot QueueSnapshot) error {
	s.snapshots = append(s.snapshots, snapshot)
	return nil
}

func (s *dispatchSchedulingStore) LatestQueueSnapshot(context.Context) (QueueSnapshot, bool, error) {
	if len(s.snapshots) == 0 {
		return QueueSnapshot{}, false, nil
	}
	return s.snapshots[len(s.snapshots)-1], true, nil
}

func (s *dispatchSchedulingStore) AppendSchedulingDecision(context.Context, SchedulingDecision) (bool, error) {
	return true, nil
}

type dispatchController struct{ store *dispatchStore }

func (c *dispatchController) ReconcileInterruptedRun(context.Context, string) error {
	c.store.reconcileCalls++
	return c.store.reconcileErr
}

func (c *dispatchController) StartAuthorized(ctx context.Context, _ LocalStartInput, _ func(Run) error) (Run, error) {
	if c.store.startReturned != nil {
		defer close(c.store.startReturned)
	}
	if c.store.startEntered != nil {
		close(c.store.startEntered)
	}
	if c.store.startAllow != nil {
		select {
		case <-c.store.startAllow:
		case <-ctx.Done():
			return Run{}, ctx.Err()
		}
	}
	if c.store.startErr != nil {
		return Run{}, c.store.startErr
	}
	c.store.mu.Lock()
	defer c.store.mu.Unlock()
	c.store.continues++
	c.store.run.State = domain.StateExecuting
	return c.store.run, nil
}

func (c *dispatchController) ContinueExpected(ctx context.Context, runID string, expected domain.State, key string, _ *Decision) (Run, error) {
	if c.store.startReturned != nil {
		defer close(c.store.startReturned)
	}
	if c.store.startEntered != nil {
		close(c.store.startEntered)
	}
	if c.store.startAllow != nil {
		select {
		case <-c.store.startAllow:
		case <-ctx.Done():
			return Run{}, ctx.Err()
		}
	}
	if c.store.startErr != nil {
		return Run{}, c.store.startErr
	}
	c.store.mu.Lock()
	defer c.store.mu.Unlock()
	if c.store.run.ID != runID || c.store.run.State != expected || c.store.run.IdempotencyKey != key {
		return Run{}, errors.New("unexpected controller continuation")
	}
	c.store.continues++
	c.store.run.State = domain.StateExecuting
	return c.store.run, nil
}

func (c *dispatchController) EnforceRepairDeadline(_ context.Context, runID string) (Run, error) {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()
	if c.store.run.ID != runID {
		return Run{}, errors.New("unexpected deadline preflight run")
	}
	return c.store.run, nil
}

func (c *dispatchController) BoundRepairActionContext(ctx context.Context, _ string) (context.Context, context.CancelFunc, error) {
	return ctx, func() {}, nil
}

type dispatchDriver struct {
	mu          sync.Mutex
	calls       []ProductionDriveCommand
	err         error
	beforeError func(ProductionDriveCommand)
	started     chan struct{}
	returned    chan struct{}
	allow       chan struct{}
}

func (d *dispatchDriver) Drive(ctx context.Context, command ProductionDriveCommand) (ProductionDriveResult, error) {
	if d.returned != nil {
		defer close(d.returned)
	}
	d.mu.Lock()
	d.calls = append(d.calls, command)
	started, allow := d.started, d.allow
	d.mu.Unlock()
	if started != nil {
		close(started)
	}
	if allow != nil {
		select {
		case <-allow:
		case <-ctx.Done():
			return ProductionDriveResult{}, ctx.Err()
		}
	}
	if d.err != nil {
		if d.beforeError != nil {
			d.beforeError(command)
		}
		return ProductionDriveResult{}, d.err
	}
	return ProductionDriveResult{Run: RunResult{RunID: command.RunID}, Action: ProductionStop}, nil
}

type dispatchStore struct {
	RunStore
	mu                     sync.Mutex
	now                    time.Time
	lease                  LinearTodoAdmissionLease
	releasedLease          LinearTodoAdmissionLease
	releaseDeadline        time.Time
	held                   bool
	heldErr                error
	heldWaitForCancel      bool
	heldCanceled           chan struct{}
	run                    Run
	journal                LinearTodoAdmissionJournal
	journalFound           bool
	reserveCalls           int
	adoptCalls             int
	continues              int
	reconcileCalls         int
	reconcileErr           error
	side                   SideEffectRecord
	attempts               []Attempt
	attention              []OperatorAttentionEvent
	retrySchedules         []RetrySchedule
	leaseLost              bool
	renewCalls             int
	failRenewAt            int
	renewErrAt             int
	renewEntered           chan struct{}
	renewAllow             chan struct{}
	renewWaitForCancel     bool
	renewCanceled          chan struct{}
	renewed                chan int
	postProofDrift         bool
	omitDecisionTransition bool
	reserveBlocked         chan struct{}
	reserveUnavailable     bool
	startEntered           chan struct{}
	startReturned          chan struct{}
	startAllow             chan struct{}
	startErr               error
	ciWaitActive           bool
	ciWaitClosed           int
	ciWaitClosedAt         time.Time
	attentionBeforeCIClose bool
}

func (s *dispatchStore) CloseInactiveCIWaits(_ context.Context, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ciWaitActive && s.run.State != domain.StateReconcilingReviews {
		s.ciWaitActive = false
		s.ciWaitClosed++
		s.ciWaitClosedAt = at
	}
	return nil
}

func (s *dispatchStore) AcquireLinearTodoAdmissionLease(_ context.Context, owner string, ttl time.Duration, now time.Time) (LinearTodoAdmissionLease, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held {
		return LinearTodoAdmissionLease{}, false, nil
	}
	s.held = true
	s.lease = LinearTodoAdmissionLease{Namespace: LinearTodoAdmissionLeaseNamespace, OwnerNonce: owner, Version: 1, AcquiredAt: now, RenewedAt: now, ExpiresAt: now.Add(ttl)}
	return s.lease, true, nil
}

func (s *dispatchStore) RenewLinearTodoAdmissionLease(ctx context.Context, lease LinearTodoAdmissionLease, ttl time.Duration, now time.Time) (LinearTodoAdmissionLease, bool, error) {
	s.mu.Lock()
	s.renewCalls++
	call := s.renewCalls
	renewEntered, renewAllow := s.renewEntered, s.renewAllow
	renewWaitForCancel, renewCanceled := s.renewWaitForCancel, s.renewCanceled
	s.mu.Unlock()
	if renewEntered != nil {
		close(renewEntered)
	}
	if renewWaitForCancel {
		<-ctx.Done()
		if renewCanceled != nil {
			close(renewCanceled)
		}
		return LinearTodoAdmissionLease{}, false, ctx.Err()
	}
	if renewAllow != nil {
		<-renewAllow
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.renewErrAt == call {
		return LinearTodoAdmissionLease{}, false, errors.New("lease persistence unavailable")
	}
	if s.failRenewAt == call || !s.held || s.leaseLost || lease.Namespace != s.lease.Namespace || lease.OwnerNonce != s.lease.OwnerNonce || lease.Version != s.lease.Version {
		return LinearTodoAdmissionLease{}, false, nil
	}
	if now.After(s.lease.ExpiresAt) {
		return LinearTodoAdmissionLease{}, false, nil
	}
	s.lease.Version++
	s.lease.RenewedAt, s.lease.ExpiresAt = now.UTC(), now.UTC().Add(ttl)
	if s.renewed != nil {
		select {
		case s.renewed <- call:
		default:
		}
	}
	return s.lease, true, nil
}

func (s *dispatchStore) ReleaseLinearTodoAdmissionLease(ctx context.Context, lease LinearTodoAdmissionLease) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releasedLease = lease
	s.releaseDeadline, _ = ctx.Deadline()
	if !s.held || lease.OwnerNonce != s.lease.OwnerNonce || lease.Version != s.lease.Version {
		return false, nil
	}
	s.held = false
	return true, nil
}

func (s *dispatchStore) LinearTodoAdmissionLeaseHeld(ctx context.Context, lease LinearTodoAdmissionLease, now time.Time) (bool, error) {
	s.mu.Lock()
	waitForCancel, canceled := s.heldWaitForCancel, s.heldCanceled
	s.mu.Unlock()
	if waitForCancel {
		<-ctx.Done()
		if canceled != nil {
			close(canceled)
		}
		return false, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.heldErr != nil {
		return false, s.heldErr
	}
	return s.held && !s.leaseLost && !now.After(s.lease.ExpiresAt) && lease.OwnerNonce == s.lease.OwnerNonce && lease.Version == s.lease.Version, nil
}

func (s *dispatchStore) ListNonterminalRuns(context.Context) ([]Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.run.ID == "" || s.run.State == domain.StateCompleted || s.run.State == domain.StateFailed || s.run.State == domain.StateRejected {
		return nil, nil
	}
	return []Run{s.run}, nil
}

func (s *dispatchStore) GetLinearTodoAdmissionJournal(_ context.Context, runID string) (LinearTodoAdmissionJournal, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.journalFound || s.journal.RunID != runID {
		return LinearTodoAdmissionJournal{}, false, nil
	}
	return s.journal, true, nil
}

func (s *dispatchStore) ReserveLinearTodoAdmission(_ context.Context, reservation LinearTodoAdmissionReservation) (Run, LinearTodoAdmissionJournal, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reserveCalls++
	if s.reserveBlocked != nil {
		<-s.reserveBlocked
	}
	if s.reserveUnavailable {
		return Run{}, LinearTodoAdmissionJournal{}, false, nil
	}
	if !s.held || (s.run.ID != "" && s.run.State != domain.StateCompleted && s.run.State != domain.StateFailed && s.run.State != domain.StateRejected) {
		return Run{}, LinearTodoAdmissionJournal{}, false, nil
	}
	run, err := ReservedRunFromAdmissionSnapshot(reservation.Input)
	if err != nil {
		return Run{}, LinearTodoAdmissionJournal{}, false, err
	}
	run.State = domain.StateReceived
	s.run = run
	s.journal = LinearTodoAdmissionJournal{IssueUUID: reservation.IssueUUID, RunID: run.ID, ScanDigest: reservation.ScanDigest, TaskDigest: run.TaskHash, ProfileDigest: run.ProfileDigest, Status: LinearTodoAdmissionJournalReserved, CreatedAt: s.now, UpdatedAt: s.now}
	s.journalFound = true
	return run, s.journal, true, nil
}

func (s *dispatchStore) AdoptLinearTodoAdmissionReservation(_ context.Context, reservation LinearTodoAdmissionReservation) (Run, LinearTodoAdmissionJournal, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adoptCalls++
	if !s.journalFound || reservation.Input.Task.RunID != s.run.ID || reservation.ScanDigest != s.journal.ScanDigest || reservation.IssueUUID != s.journal.IssueUUID {
		return Run{}, LinearTodoAdmissionJournal{}, false, nil
	}
	return s.run, s.journal, true, nil
}

func (s *dispatchStore) AdvanceLinearTodoAdmissionJournal(_ context.Context, transition LinearTodoAdmissionJournalTransition) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.held || s.journal.RunID != transition.RunID || s.journal.Status != transition.ExpectedStatus {
		return false, nil
	}
	s.journal.Status, s.journal.MutationIntentRef, s.journal.ReasonCode = transition.NextStatus, transition.MutationIntentRef, transition.ReasonCode
	return true, nil
}

func (s *dispatchStore) GetRun(_ context.Context, runID string) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.run.ID != runID {
		return Run{}, ErrRunNotFound
	}
	if s.postProofDrift && s.side.Status == "observed" && s.run.State != domain.StateReceived {
		drifted := s.run
		drifted.IdempotencyKey = "different-authority"
		return drifted, nil
	}
	return s.run, nil
}

func (s *dispatchStore) Inspect(_ context.Context, runID string) (RunInspection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.run.ID != runID {
		return RunInspection{}, ErrRunNotFound
	}
	inspection := RunInspection{Run: s.run, Attempts: append([]Attempt(nil), s.attempts...)}
	if s.run.State == domain.StateManualIntervention {
		inspection.Timeline = []Transition{{Sequence: 2, From: domain.StateReceived, To: domain.StateManualIntervention, Reason: "operator decision required", EvidenceReference: "linear_issue_start", CreatedAt: s.run.UpdatedAt}}
	} else if s.run.State == domain.StateAwaitingHumanDecision && !s.omitDecisionTransition {
		inspection.Timeline = []Transition{{Sequence: 3, From: domain.StateExecuting, To: domain.StateAwaitingHumanDecision, Reason: "decision required", EvidenceReference: "decision_request", CreatedAt: s.run.UpdatedAt}}
	}
	return inspection, nil
}

func (s *dispatchStore) GetRunByIdempotency(_ context.Context, key string) (Run, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.run, s.run.ID != "" && s.run.IdempotencyKey == key, nil
}

func (s *dispatchStore) BeginSideEffect(_ context.Context, side SideEffectRecord) (SideEffectRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.side.ID == 0 {
		s.side = side
		s.side.ID, s.side.Status = 1, "intent"
	}
	return s.side, false, nil
}

func (s *dispatchStore) FinishLinearIssueStartSideEffect(_ context.Context, side SideEffectRecord, expected string, attempt int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.side.Status != expected || s.side.Attempt != attempt {
		return errors.New("side effect compare and swap lost")
	}
	s.side = side
	return nil
}

func (s *dispatchStore) RetryLinearIssueStartSideEffect(context.Context, SideEffectRecord) (SideEffectRecord, bool, error) {
	return SideEffectRecord{}, false, errors.New("unexpected mutation retry")
}

func (s *dispatchStore) ClaimLinearIssueStartSideEffect(_ context.Context, side SideEffectRecord, _ time.Time) (SideEffectRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.side.Status != "intent" || side.ID != s.side.ID {
		return s.side, false, nil
	}
	s.side.Status = "in_flight"
	return s.side, true, nil
}

func (s *dispatchStore) SaveLinearRequestObservation(context.Context, string, LinearRequestObservation) error {
	return nil
}

func (s *dispatchStore) SetLastError(context.Context, string, string) error { return nil }

func (s *dispatchStore) Transition(_ context.Context, runID string, from, to domain.State, _ string, _ string, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.run.ID != runID || s.run.State != from {
		return errors.New("state transition conflict")
	}
	s.run.State, s.run.UpdatedAt = to, s.now
	return nil
}

func (s *dispatchStore) AppendOperatorAttention(_ context.Context, event OperatorAttentionEvent) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ciWaitActive {
		s.attentionBeforeCIClose = true
	}
	for _, current := range s.attention {
		if current.EventKey == event.EventKey {
			if current.PayloadDigest != event.PayloadDigest {
				return false, FormatOperatorAttentionConflict(event)
			}
			return false, nil
		}
	}
	s.attention = append(s.attention, event)
	return true, nil
}

func (s *dispatchStore) GetRetrySchedule(_ context.Context, runID, phase string) (RetrySchedule, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, schedule := range s.retrySchedules {
		if schedule.RunID == runID && schedule.Phase == phase {
			return schedule, true, nil
		}
	}
	return RetrySchedule{}, false, nil
}

func (s *dispatchStore) ListRetrySchedules(context.Context) ([]RetrySchedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RetrySchedule(nil), s.retrySchedules...), nil
}

func (s *dispatchStore) ApplyRetryFailure(_ context.Context, request RetryFailureRequest) (RetrySchedule, bool, error) {
	if err := ValidateRetryFailureRequest(request); err != nil {
		return RetrySchedule{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for index, current := range s.retrySchedules {
		if current.RunID != request.RunID || current.Phase != request.Phase {
			continue
		}
		if current.Status == RetryScheduleAttention || current.AttemptCount != request.ExpectedAttempt {
			return current, false, nil
		}
		attempt := current.AttemptCount + 1
		policy := AutomaticRetryPolicy{MaxAttempts: current.MaxAttempts, InitialDelay: current.InitialDelay, MaximumDelay: current.MaximumDelay}
		next := request.Now.Add(AutomaticRetryDelay(policy, attempt))
		schedule := current
		schedule.ControllerState, schedule.AttemptCount, schedule.UpdatedAt = string(request.ControllerState), attempt, request.Now
		schedule.FailureClass, schedule.ReasonCode = request.FailureClass, request.ReasonCode
		schedule.FailureEvidenceRef = request.FailureEvidenceRef
		if RetryFailureIsRetryable(request.FailureClass) && attempt <= schedule.MaxAttempts {
			schedule.Status, schedule.NextEligibleAt, schedule.AttentionAt = RetryScheduleScheduled, next, time.Time{}
		} else {
			schedule.Status, schedule.NextEligibleAt, schedule.AttentionAt = RetryScheduleAttention, time.Time{}, request.Now
			if RetryFailureIsRetryable(request.FailureClass) && attempt > schedule.MaxAttempts {
				schedule.ReasonCode = RetryReasonBudgetExhausted
			}
		}
		s.retrySchedules[index] = schedule
		return schedule, true, nil
	}
	policy := request.Policy.normalized()
	attempt := 1
	schedule := RetrySchedule{RunID: request.RunID, Phase: request.Phase, ControllerState: string(request.ControllerState), AttemptCount: attempt, MaxAttempts: policy.MaxAttempts, InitialDelay: policy.InitialDelay, MaximumDelay: policy.MaximumDelay, FailureClass: request.FailureClass, ReasonCode: request.ReasonCode, CreatedAt: request.Now, UpdatedAt: request.Now}
	schedule.FailureEvidenceRef = request.FailureEvidenceRef
	if RetryFailureIsRetryable(request.FailureClass) && attempt <= policy.MaxAttempts {
		schedule.Status, schedule.NextEligibleAt = RetryScheduleScheduled, request.Now.Add(AutomaticRetryDelay(policy, attempt))
	} else {
		schedule.Status, schedule.AttentionAt = RetryScheduleAttention, request.Now
	}
	s.retrySchedules = append(s.retrySchedules, schedule)
	return schedule, true, nil
}

func (s *dispatchStore) ClearRetrySchedule(_ context.Context, runID, phase string, expectedAttempt int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index, schedule := range s.retrySchedules {
		if schedule.RunID == runID && schedule.Phase == phase {
			if schedule.Status == RetryScheduleAttention || schedule.AttemptCount != expectedAttempt {
				return false, nil
			}
			s.retrySchedules = append(s.retrySchedules[:index], s.retrySchedules[index+1:]...)
			return true, nil
		}
	}
	return true, nil
}

func dispatchAuthority() LinearTodoCandidateAuthority {
	return LinearTodoCandidateAuthority{TeamID: dispatchTeamID, TeamKey: "IFAN", TodoState: LinearState{ID: dispatchTodoID, Name: "Todo", Type: "unstarted"}, InProgressState: LinearState{ID: dispatchStartedID, Name: "In Progress", Type: "started"}, MaxCandidates: 4, MaxPages: 1}
}

func dispatchStartAuthority() LinearIssueStartAuthority {
	authority := dispatchAuthority()
	return LinearIssueStartAuthority{TeamID: authority.TeamID, TeamKey: authority.TeamKey, TodoState: authority.TodoState, InProgressState: authority.InProgressState}
}

func newDispatchLab(t *testing.T, candidates ...LinearTodoCandidate) (*LinearTodoDispatcher, *dispatchStore, *dispatchScanner, *dispatchReader, *dispatchStarter, *dispatchDriver) {
	t.Helper()
	now := time.Date(2026, 7, 15, 3, 0, 0, 0, time.UTC)
	repository := LocalRepository{CanonicalRepository: "owner/repo", BaseBranch: "main", RunRoot: "/tmp/dispatch-runs", WorktreeRoot: "/tmp/dispatch-worktrees", ProfileID: "profile-owner-repo", ProfileSnapshotVersion: 1, ProfileDigest: dispatchDigest("profile"), ProfileSnapshotJSON: `{}`, RegistryVersion: 1, RegistryDigest: dispatchDigest("registry"), RepositoryBindingDigest: dispatchDigest("binding"), VerifierIDs: []string{"go-test"}, AllowedOperatorLogins: []string{"operator"}}
	reader := &dispatchReader{sources: map[string]LinearTaskSource{}, started: map[string]bool{}, errs: map[string]error{}}
	for _, candidate := range candidates {
		reader.sources[candidate.Identifier] = dispatchSource(candidate)
	}
	scanner := &dispatchScanner{scan: LinearTodoCandidateScan{Candidates: candidates, Digest: dispatchDigest("scan"), ObservedAt: now}}
	store := &dispatchStore{now: now}
	starter := &dispatchStarter{reader: reader}
	driver := &dispatchDriver{}
	policy := LinearTodoDispatchPolicy{CandidateAuthority: dispatchAuthority(), StartAuthority: dispatchStartAuthority(), LeaseTTL: time.Minute, LeaseRenewal: 20 * time.Second, OwnerNonce: "dispatch-owner", Requester: Requester{ID: "operator", Kind: "github_login"}, AttentionProfile: OperatorAttentionProfile{ID: "automation", Name: "linear"}}
	dispatcher, err := NewLinearTodoDispatcher(scanner, reader, dispatchResolver{repository: repository}, starter, store, &dispatchController{store: store}, driver, policy)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.now = func() time.Time { return now }
	return dispatcher, store, scanner, reader, starter, driver
}

func dispatchCandidate(seed, identifier string, priority int) LinearTodoCandidate {
	created := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	updated := created.Add(time.Hour)
	labels := []LinearLabel{{ID: dispatchUUID(seed + "-agent"), Name: "agent:codex"}, {ID: dispatchUUID(seed + "-repo"), Name: "repo:test"}}
	teamKey, sequence, ok := normalizedLinearIdentifier(identifier)
	if !ok {
		panic("invalid dispatch candidate identifier")
	}
	return LinearTodoCandidate{IssueID: dispatchUUID(seed), Identifier: identifier, TeamKey: teamKey, IssueSequence: sequence, Priority: priority, State: dispatchAuthority().TodoState, Cycle: LinearCycle{ID: dispatchUUID(seed + "-cycle"), Number: 1, StartsAt: created, EndsAt: created.Add(24 * time.Hour), IsActive: true}, Labels: labels, RepositoryLabels: []LinearLabel{labels[1]}, BranchName: "ifan/" + stringsToBranch(identifier), SourceRevision: updated.Format(time.RFC3339Nano), SourceDigest: dispatchDigest(seed + "-source"), CreatedAt: created, UpdatedAt: updated}
}

func dispatchSource(candidate LinearTodoCandidate) LinearTaskSource {
	return LinearTaskSource{Provider: "linear", IssueID: candidate.IssueID, Identifier: candidate.Identifier, URL: "https://linear.invalid/" + candidate.Identifier, Title: "Dispatch fixture", Description: "## Outcome\n\nDispatch exactly one task.\n\n## Acceptance Criteria\n\n- Preserve durable state.\n\n## Out of Scope\n\n- Extra candidates.", Team: LinearTeam{ID: dispatchTeamID, Key: "IFAN", Name: "I-Fan"}, State: candidate.State, Labels: append([]LinearLabel(nil), candidate.Labels...), Cycle: candidate.Cycle, BranchName: candidate.BranchName, SourceRevision: candidate.SourceRevision, CreatedAt: candidate.CreatedAt, UpdatedAt: candidate.UpdatedAt, ObservedAt: candidate.UpdatedAt}
}

func dispatchDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func dispatchUUID(value string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(value)).String()
}

func stringsToBranch(identifier string) string {
	return "task-" + identifier
}

func TestLinearTodoDispatcherSelectsOnePriorityCandidateThenStartsAndDrives(t *testing.T) {
	low, high := dispatchCandidate("low", "IFAN-11", 3), dispatchCandidate("high", "IFAN-12", 1)
	dispatcher, store, scanner, reader, starter, driver := newDispatchLab(t, low, high)
	if !validLinearTodoCandidateScan(scanner.scan, dispatchAuthority()) {
		t.Fatalf("invalid fixture scan: %+v", scanner.scan)
	}

	result, err := dispatcher.Dispatch(context.Background())
	if err != nil || result.Outcome != LinearTodoDispatchDriven || scanner.calls != 1 || store.reserveCalls != 1 || store.run.IssueID != high.Identifier || len(starter.calls) != 1 || starter.calls[0].IssueID != high.IssueID || len(driver.calls) != 1 || driver.calls[0].RunID != store.run.ID || len(store.attention) != 0 {
		t.Fatalf("result=%+v scanner=%d reserve=%d run=%+v starter=%+v driver=%+v attention=%+v err=%v", result, scanner.calls, store.reserveCalls, store.run, starter.calls, driver.calls, store.attention, err)
	}
	if result.QueueDecision == nil || result.QueueDecision.Reason != LinearTodoQueueDecisionSelectedPriority || result.QueueDecision.CandidateCount != 2 || result.QueueDecision.SelectedPriority == nil || *result.QueueDecision.SelectedPriority != 1 || result.QueueDecision.SelectedTeamKey != "IFAN" || result.QueueDecision.SelectedIssueSequence == nil || *result.QueueDecision.SelectedIssueSequence != 12 || result.QueueDecision.SelectedIssueUUID != high.IssueID || result.QueueDecision.ExistingRunPreventedScan {
		t.Fatalf("queue decision=%+v", result.QueueDecision)
	}
	if len(reader.calls) != 3 || reader.calls[0] != high.Identifier || store.journal.Status != "started" || store.continues != 1 || store.renewCalls != 7 || store.releasedLease.Version != store.lease.Version || store.held {
		t.Fatalf("reader=%v journal=%+v continues=%d renews=%d released=%+v current=%+v held=%t", reader.calls, store.journal, store.continues, store.renewCalls, store.releasedLease, store.lease, store.held)
	}
}

func TestLinearTodoDispatcherSkipsActiveAndDisabledRepositoriesForIdleRepository(t *testing.T) {
	active := dispatchCandidate("active-repository", "IFAN-101", 1)
	disabled := dispatchCandidate("disabled-repository", "IFAN-102", 2)
	idle := dispatchCandidate("idle-repository", "IFAN-103", 3)
	setRepositoryLabel := func(candidate *LinearTodoCandidate, label string) {
		candidate.RepositoryLabels[0].Name = label
		candidate.Labels[1].Name = label
	}
	setRepositoryLabel(&active, "repo:active")
	setRepositoryLabel(&disabled, "repo:disabled")
	setRepositoryLabel(&idle, "repo:idle")
	dispatcher, base, scanner, _, _, _ := newDispatchLab(t, active, disabled, idle)
	template := dispatcher.resolver.(dispatchResolver).repository
	repository := func(name string) LocalRepository {
		value := template
		value.CanonicalRepository = "owner/" + name
		value.ProfileID = "profile-" + name
		value.ProfileDigest = dispatchDigest("profile:" + name)
		value.RepositoryBindingDigest = dispatchDigest("binding:" + name)
		return value
	}
	activeRepository, disabledRepository, idleRepository := repository("active"), repository("disabled"), repository("idle")
	dispatcher.resolver = dispatchRepositoryMap{
		repositories: map[string]LocalRepository{"repo:active": activeRepository, "repo:disabled": disabledRepository, "repo:idle": idleRepository},
		disabled:     map[string]string{disabledRepository.RepositoryBindingDigest: "repository_disabled"},
	}
	scheduling := &dispatchSchedulingStore{dispatchStore: base, projection: CapacityProjection{ConfiguredCapacity: 3, EffectiveCapacity: 3, InUse: 1, Available: 2, EffectiveIdentity: "capacity-three", Version: 1, ObservedAt: base.now}}
	dispatcher.store = scheduling
	lease, acquired, err := base.AcquireLinearTodoAdmissionLease(context.Background(), "dispatch-owner", time.Minute, base.now)
	if err != nil || !acquired {
		t.Fatal(err)
	}
	selected, found, lost, err := dispatcher.readAndSelect(context.Background(), &lease, scanner.scan, []Run{{ID: "active-run", RepositoryBindingDigest: activeRepository.RepositoryBindingDigest}})
	if err != nil || !found || lost || selected.candidate.IssueID != idle.IssueID {
		t.Fatalf("selected=%+v found=%t lost=%t err=%v", selected, found, lost, err)
	}
	if len(scheduling.snapshots) != 1 || len(scheduling.snapshots[0].Candidates) != 3 {
		t.Fatalf("snapshots=%+v", scheduling.snapshots)
	}
	projections := scheduling.snapshots[0].Candidates
	if projections[0].Classification != QueueCandidateBlockedByActiveRepository || projections[1].Classification != QueueCandidateRepositoryDisabled || projections[2].Classification != QueueCandidateSelected {
		t.Fatalf("projections=%+v", projections)
	}
}

func TestLinearTodoDispatcherRevalidatesBlockedHigherPriorityCandidate(t *testing.T) {
	active := dispatchCandidate("active-drift", "IFAN-107", 1)
	idle := dispatchCandidate("idle-lower", "IFAN-108", 2)
	setRepositoryLabel := func(candidate *LinearTodoCandidate, label string) {
		candidate.RepositoryLabels[0].Name = label
		candidate.Labels[1].Name = label
	}
	setRepositoryLabel(&active, "repo:active")
	setRepositoryLabel(&idle, "repo:idle")
	dispatcher, base, scanner, reader, _, _ := newDispatchLab(t, active, idle)
	template := dispatcher.resolver.(dispatchResolver).repository
	activeRepository, idleRepository := template, template
	activeRepository.CanonicalRepository, activeRepository.ProfileID, activeRepository.RepositoryBindingDigest = "owner/active", "profile-active", dispatchDigest("binding:active")
	idleRepository.CanonicalRepository, idleRepository.ProfileID, idleRepository.RepositoryBindingDigest = "owner/idle", "profile-idle", dispatchDigest("binding:idle")
	dispatcher.resolver = dispatchRepositoryMap{repositories: map[string]LocalRepository{"repo:active": activeRepository, "repo:idle": idleRepository}}
	drifted := reader.sources[active.Identifier]
	for index := range drifted.Labels {
		if drifted.Labels[index].Name == "repo:active" {
			drifted.Labels[index].Name = "repo:idle"
		}
	}
	reader.sources[active.Identifier] = drifted
	scheduling := &dispatchSchedulingStore{dispatchStore: base, projection: CapacityProjection{ConfiguredCapacity: 2, EffectiveCapacity: 2, Available: 1, EffectiveIdentity: "capacity-two", ObservedAt: base.now}}
	dispatcher.store = scheduling
	lease, acquired, err := base.AcquireLinearTodoAdmissionLease(context.Background(), "dispatch-owner", time.Minute, base.now)
	if err != nil || !acquired {
		t.Fatal(err)
	}
	selected, found, lost, err := dispatcher.readAndSelect(context.Background(), &lease, scanner.scan, []Run{{ID: "active-run", RepositoryBindingDigest: activeRepository.RepositoryBindingDigest}})
	if !errors.Is(err, errLinearTodoSelectionAmbiguous) || found || lost || selected.candidate.IssueID != "" || len(reader.calls) == 0 || reader.calls[0] != active.Identifier {
		t.Fatalf("selected=%+v found=%t lost=%t reads=%v err=%v", selected, found, lost, reader.calls, err)
	}
}

func TestConcurrentExistingRunHonorsDurableRetryBackoff(t *testing.T) {
	candidate := dispatchCandidate("concurrent-retry", "IFAN-104", 1)
	dispatcher, base, _, _, _, driver := newDispatchLab(t, candidate)
	base.run = authorizeDispatchRun(Run{ID: "concurrent-retry-run", IssueID: candidate.Identifier, IdempotencyKey: "concurrent-retry-key", Repository: "owner/repo", RepositoryBindingDigest: dispatchDigest("concurrent-retry-binding"), State: domain.StateExecuting})
	now := base.now
	base.retrySchedules = []RetrySchedule{{RunID: base.run.ID, Phase: AutomaticRetryPhaseForRun(base.run), ControllerState: string(base.run.State), AttemptCount: 1, MaxAttempts: 3, InitialDelay: time.Second, MaximumDelay: 30 * time.Second, FailureClass: RetryFailureUnavailable, ReasonCode: RetryReasonUnavailable, Status: RetryScheduleScheduled, NextEligibleAt: now.Add(time.Minute), CreatedAt: now, UpdatedAt: now}}
	dispatcher.store = &dispatchSchedulingStore{dispatchStore: base, projection: CapacityProjection{ConfiguredCapacity: 2, EffectiveCapacity: 2, Available: 1, EffectiveIdentity: "capacity-two", ObservedAt: now}, scheduled: []SchedulingRun{{RunID: base.run.ID, RepositoryBindingDigest: base.run.RepositoryBindingDigest, State: base.run.State, RunnableSince: now, SupervisorState: "waiting", WaitingForCapacity: true}}}

	result, handled, err := dispatcher.dispatchExistingRunWithoutAdmissionLease(context.Background())
	if err != nil || !handled || result.Outcome != LinearTodoDispatchRetryWait || len(driver.calls) != 0 {
		t.Fatalf("result=%+v handled=%t driver=%+v err=%v", result, handled, driver.calls, err)
	}
}

func TestNextScheduledRunSkipsHumanAndNotYetRunnableExternalWait(t *testing.T) {
	now := time.Now().UTC()
	runs := []Run{{ID: "human"}, {ID: "external-future"}, {ID: "external-ready"}}
	scheduled := []SchedulingRun{
		{RunID: "human", RunnableSince: now.Add(-time.Minute), SupervisorState: "human_wait"},
		{RunID: "external-future", RunnableSince: now.Add(time.Minute), SupervisorState: "external_wait"},
		{RunID: "external-ready", RunnableSince: now, SupervisorState: "external_wait"},
	}
	run, found, quarantined := nextScheduledRun(runs, scheduled, nil, now)
	if !found || quarantined || run.ID != "external-ready" {
		t.Fatalf("run=%+v found=%t quarantined=%t", run, found, quarantined)
	}
}

func TestLinearTodoDispatcherProjectsPersistedFutureWakeAfterRestart(t *testing.T) {
	dispatcher, base, scanner, _, _, _ := newDispatchLab(t)
	now := base.now
	deadline := now.Add(2 * time.Minute)
	base.run = authorizeDispatchRun(Run{ID: "future-run", IssueID: "IFAN-105", IdempotencyKey: "future-key", Repository: "owner/repo", RepositoryBindingDigest: dispatchDigest("future-binding"), State: domain.StatePROpen})
	dispatcher.store = &dispatchSchedulingStore{
		dispatchStore: base,
		projection:    CapacityProjection{ConfiguredCapacity: 2, EffectiveCapacity: 2, Available: 2, EffectiveIdentity: "capacity-two", ObservedAt: now},
		scheduled:     []SchedulingRun{{RunID: base.run.ID, RepositoryBindingDigest: base.run.RepositoryBindingDigest, State: base.run.State, RunnableSince: deadline, SupervisorState: "external_wait"}},
	}

	result, err := dispatcher.Dispatch(context.Background())
	if err != nil || result.Outcome != LinearTodoDispatchNoCandidate || !result.NextRunnableAt.Equal(deadline) || scanner.calls != 1 {
		t.Fatalf("result=%+v scans=%d err=%v", result, scanner.calls, err)
	}
}

func TestLinearTodoDispatcherReconcilesStartedAttemptBeforePermitAdoption(t *testing.T) {
	candidate := dispatchCandidate("reconcile-permit", "IFAN-106", 1)
	dispatcher, base, scanner, _, _, driver := newDispatchLab(t, candidate)
	base.run = authorizeDispatchRun(Run{ID: "reconcile-run", IssueID: candidate.Identifier, IdempotencyKey: "reconcile-key", Repository: "owner/repo", RepositoryBindingDigest: dispatchDigest("reconcile-binding"), State: domain.StateExecuting})
	now := base.now
	scheduling := &dispatchSchedulingStore{
		dispatchStore: base,
		projection:    CapacityProjection{ConfiguredCapacity: 2, EffectiveCapacity: 2, InUse: 1, Available: 1, EffectiveIdentity: "capacity-two", ObservedAt: now},
		scheduled:     []SchedulingRun{{RunID: base.run.ID, RepositoryBindingDigest: base.run.RepositoryBindingDigest, State: base.run.State, RunnableSince: now, SupervisorState: "running", HasHeavyPermit: true}},
		acquireErrors: []error{ErrHeavyPermitProcessReconciliationRequired},
	}
	dispatcher.store = scheduling

	result, err := dispatcher.Dispatch(context.Background())
	if err != nil || result.Outcome != LinearTodoDispatchDriven || base.reconcileCalls != 1 || scheduling.acquireCalls != 2 || len(driver.calls) != 1 || scanner.calls != 0 {
		t.Fatalf("result=%+v reconciles=%d acquires=%d drives=%d scans=%d err=%v", result, base.reconcileCalls, scheduling.acquireCalls, len(driver.calls), scanner.calls, err)
	}
}

func TestLinearTodoDispatcherRenewalFailureStopsBeforeEachLongBoundary(t *testing.T) {
	for _, test := range []struct {
		name                               string
		failAt, scannerCalls, reserveCalls int
		starterCalls, driverCalls          int
	}{
		{name: "scan", failAt: 1},
		{name: "authoritative read", failAt: 2, scannerCalls: 1},
		{name: "start mutation", failAt: 5, scannerCalls: 1, reserveCalls: 1},
		{name: "local start", failAt: 7, scannerCalls: 1, reserveCalls: 1, starterCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := dispatchCandidate("renew-"+test.name, "IFAN-31", 1)
			dispatcher, store, scanner, _, starter, driver := newDispatchLab(t, candidate)
			store.failRenewAt = test.failAt
			result, err := dispatcher.Dispatch(context.Background())
			if err != nil || result.Outcome != LinearTodoDispatchAttention || scanner.calls != test.scannerCalls || store.reserveCalls != test.reserveCalls || len(starter.calls) != test.starterCalls || len(driver.calls) != test.driverCalls || len(store.attention) != 1 || store.attention[0].EventType != OperatorAttentionSchedulerLease || store.attention[0].ReasonCode != "lease_lost" || store.releasedLease.Version != store.lease.Version || store.held {
				t.Fatalf("result=%+v scanner=%d reserve=%d starter=%+v driver=%+v attention=%+v renews=%d released=%+v current=%+v held=%t err=%v", result, scanner.calls, store.reserveCalls, starter.calls, driver.calls, store.attention, store.renewCalls, store.releasedLease, store.lease, store.held, err)
			}
		})
	}
}

func TestLinearTodoDispatcherEqualPrioritySelectsLowestSequence(t *testing.T) {
	first, second := dispatchCandidate("first", "IFAN-12", 1), dispatchCandidate("second", "IFAN-11", 1)
	dispatcher, store, scanner, _, starter, driver := newDispatchLab(t, first, second)

	result, err := dispatcher.Dispatch(context.Background())
	if err != nil || result.Outcome != LinearTodoDispatchDriven || scanner.calls != 1 || store.reserveCalls != 1 || store.run.IssueID != second.Identifier || len(starter.calls) != 1 || starter.calls[0].IssueID != second.IssueID || len(driver.calls) != 1 || len(store.attention) != 0 {
		t.Fatalf("result=%+v reserve=%d starter=%+v driver=%+v attention=%+v err=%v", result, store.reserveCalls, starter.calls, driver.calls, store.attention, err)
	}
	if result.QueueDecision == nil || result.QueueDecision.Reason != LinearTodoQueueDecisionSelectedPriority || result.QueueDecision.CandidateCount != 2 || result.QueueDecision.SelectedIssueSequence == nil || *result.QueueDecision.SelectedIssueSequence != 11 || result.QueueDecision.SelectedIssueUUID != second.IssueID || result.QueueDecision.ExistingRunPreventedScan {
		t.Fatalf("queue decision=%+v", result.QueueDecision)
	}
}

func TestLinearTodoDispatcherSelectionIsIndependentOfCandidateOrder(t *testing.T) {
	first := dispatchCandidate("permutation-first", "IFAN-31", 2)
	selected := dispatchCandidate("permutation-selected", "IFAN-7", 1)
	third := dispatchCandidate("permutation-third", "IFAN-19", 1)
	permutations := [][]LinearTodoCandidate{
		{first, selected, third}, {first, third, selected}, {selected, first, third},
		{selected, third, first}, {third, first, selected}, {third, selected, first},
	}
	for index, candidates := range permutations {
		dispatcher, store, _, _, _, _ := newDispatchLab(t, candidates...)
		result, err := dispatcher.Dispatch(context.Background())
		if err != nil || result.Outcome != LinearTodoDispatchDriven || store.run.IssueID != selected.Identifier || result.QueueDecision == nil || result.QueueDecision.SelectedIssueUUID != selected.IssueID {
			t.Fatalf("permutation=%d candidates=%v result=%+v run=%+v err=%v", index, candidates, result, store.run, err)
		}
	}
}

func TestLinearTodoCandidateComparatorUsesUUIDAsDefensiveFinalRank(t *testing.T) {
	left, right := dispatchCandidate("uuid-left", "IFAN-8", 1), dispatchCandidate("uuid-right", "IFAN-9", 1)
	right.IssueSequence = left.IssueSequence
	if got := compareLinearTodoCandidates(left, right); got != strings.Compare(left.IssueID, right.IssueID) {
		t.Fatalf("comparison=%d left=%s right=%s", got, left.IssueID, right.IssueID)
	}
}

func TestLinearTodoDispatcherRejectsContradictoryNormalizedSequence(t *testing.T) {
	first, contradictory := dispatchCandidate("sequence-first", "IFAN-11", 1), dispatchCandidate("sequence-contradiction", "IFAN-12", 1)
	contradictory.Identifier, contradictory.IssueSequence = "IFAN-011", 11
	dispatcher, store, scanner, _, starter, driver := newDispatchLab(t, first, contradictory)

	result, err := dispatcher.Dispatch(context.Background())
	if err != nil || result.Outcome != LinearTodoDispatchAttention || scanner.calls != 1 || store.reserveCalls != 0 || len(starter.calls) != 0 || len(driver.calls) != 0 || len(store.attention) != 1 || store.attention[0].EventType != OperatorAttentionCandidateScan {
		t.Fatalf("result=%+v scans=%d reserve=%d starter=%+v driver=%+v attention=%+v err=%v", result, scanner.calls, store.reserveCalls, starter.calls, driver.calls, store.attention, err)
	}
}

func TestLinearTodoDispatcherExplicitPriorityBeatsUnprioritizedCandidate(t *testing.T) {
	unprioritized, explicit := dispatchCandidate("unprioritized", "IFAN-40", 0), dispatchCandidate("explicit", "IFAN-41", 4)
	dispatcher, store, _, _, _, _ := newDispatchLab(t, unprioritized, explicit)

	result, err := dispatcher.Dispatch(context.Background())
	if err != nil || result.Outcome != LinearTodoDispatchDriven || store.run.IssueID != explicit.Identifier || store.reserveCalls != 1 {
		t.Fatalf("result=%+v run=%+v reserve=%d err=%v", result, store.run, store.reserveCalls, err)
	}
	if result.QueueDecision == nil || result.QueueDecision.SelectedPriority == nil || *result.QueueDecision.SelectedPriority != 4 {
		t.Fatalf("queue decision=%+v", result.QueueDecision)
	}
}

func TestLinearTodoDispatcherExistingRunPreventsHigherPriorityPreemption(t *testing.T) {
	candidate := dispatchCandidate("higher", "IFAN-42", 1)
	dispatcher, store, scanner, _, _, driver := newDispatchLab(t, candidate)
	store.run = authorizeDispatchRun(Run{ID: "run-existing", IssueID: "IFAN-99", IdempotencyKey: "existing-key", Repository: "owner/repo", State: domain.StateExecuting})

	result, err := dispatcher.Dispatch(context.Background())
	if err != nil || result.Outcome != LinearTodoDispatchDriven || scanner.calls != 0 || store.reserveCalls != 0 || len(driver.calls) != 1 || store.run.IssueID != "IFAN-99" {
		t.Fatalf("result=%+v scanner=%d reserve=%d driver=%+v run=%+v err=%v", result, scanner.calls, store.reserveCalls, driver.calls, store.run, err)
	}
	if result.QueueDecision == nil || result.QueueDecision.Reason != LinearTodoQueueDecisionActiveRun || result.QueueDecision.CandidateCount != 0 || !result.QueueDecision.ExistingRunPreventedScan {
		t.Fatalf("queue decision=%+v", result.QueueDecision)
	}
}

func TestLinearTodoDispatcherCompletedRunAllowsNextPollSelection(t *testing.T) {
	first, second := dispatchCandidate("completed-first", "IFAN-43", 2), dispatchCandidate("completed-second", "IFAN-44", 3)
	dispatcher, store, scanner, reader, starter, driver := newDispatchLab(t, first)

	firstResult, err := dispatcher.Dispatch(context.Background())
	if err != nil || firstResult.Outcome != LinearTodoDispatchDriven {
		t.Fatalf("first result=%+v err=%v", firstResult, err)
	}
	firstRunID := store.run.ID
	if err := store.Transition(context.Background(), firstRunID, domain.StateExecuting, domain.StateCompleted, "fixture completed", "", ""); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.side = SideEffectRecord{}
	store.mu.Unlock()
	reader.sources[second.Identifier] = dispatchSource(second)
	scanner.scan = LinearTodoCandidateScan{Candidates: []LinearTodoCandidate{second}, Digest: dispatchDigest("completed-second-scan"), ObservedAt: store.now}

	secondResult, err := dispatcher.Dispatch(context.Background())
	if err != nil || secondResult.Outcome != LinearTodoDispatchDriven || scanner.calls != 2 || store.reserveCalls != 2 || len(starter.calls) != 2 || len(driver.calls) != 2 || store.run.ID == firstRunID || store.run.IssueID != second.Identifier {
		t.Fatalf("second result=%+v scanner=%d reserve=%d starter=%+v driver=%+v run=%+v err=%v", secondResult, scanner.calls, store.reserveCalls, starter.calls, driver.calls, store.run, err)
	}
	if secondResult.QueueDecision == nil || secondResult.QueueDecision.Reason != LinearTodoQueueDecisionSelectedPriority || secondResult.QueueDecision.CandidateCount != 1 || secondResult.QueueDecision.SelectedPriority == nil || *secondResult.QueueDecision.SelectedPriority != 3 || secondResult.QueueDecision.SelectedIssueSequence == nil || *secondResult.QueueDecision.SelectedIssueSequence != 44 {
		t.Fatalf("queue decision=%+v", secondResult.QueueDecision)
	}
}

func TestLinearTodoDispatcherNoCandidateProjectsQueueDecision(t *testing.T) {
	dispatcher, _, scanner, _, _, _ := newDispatchLab(t)

	result, err := dispatcher.Dispatch(context.Background())
	if err != nil || result.Outcome != LinearTodoDispatchNoCandidate || scanner.calls != 1 {
		t.Fatalf("result=%+v scanner=%d err=%v", result, scanner.calls, err)
	}
	if result.QueueDecision == nil || result.QueueDecision.Reason != LinearTodoQueueDecisionNoCandidate || result.QueueDecision.CandidateCount != 0 || result.QueueDecision.SelectedPriority != nil || result.QueueDecision.SelectedIssueSequence != nil || result.QueueDecision.ExistingRunPreventedScan {
		t.Fatalf("queue decision=%+v", result.QueueDecision)
	}
}

func TestLinearTodoQueueDecisionValidationIsAllowlisted(t *testing.T) {
	priority := 0
	sequence := 42
	valid := []LinearTodoQueueDecision{
		queueDecision(LinearTodoQueueDecisionNoCandidate, 0, false),
		queueDecision(LinearTodoQueueDecisionActiveRun, 0, true),
		queueDecision(LinearTodoQueueDecisionIncompleteScan, 2, false),
		queueDecision(LinearTodoQueueDecisionNoEligibleCandidate, 2, false),
		{Reason: LinearTodoQueueDecisionSelectedPriority, CandidateCount: 3, SelectedPriority: &priority, SelectedTeamKey: "IFAN", SelectedIssueSequence: &sequence, SelectedIssueUUID: dispatchUUID("decision")},
	}
	for _, decision := range valid {
		if err := decision.Validate(); err != nil {
			t.Fatalf("valid decision=%+v err=%v", decision, err)
		}
	}
	invalid := []LinearTodoQueueDecision{
		{Reason: "external-text", CandidateCount: 1},
		{Reason: LinearTodoQueueDecisionSelectedPriority, CandidateCount: 1},
		{Reason: LinearTodoQueueDecisionNoCandidate, CandidateCount: 1, SelectedPriority: &priority},
		{Reason: LinearTodoQueueDecisionNoCandidate, CandidateCount: -1},
		{Reason: LinearTodoQueueDecisionSelectedPriority, CandidateCount: 0, SelectedPriority: &priority, SelectedTeamKey: "IFAN", SelectedIssueSequence: &sequence, SelectedIssueUUID: dispatchUUID("zero-candidates")},
		{Reason: LinearTodoQueueDecisionSelectedPriority, CandidateCount: 1, SelectedPriority: &priority, SelectedTeamKey: "prose\n", SelectedIssueSequence: &sequence, SelectedIssueUUID: dispatchUUID("unsafe-team")},
		{Reason: LinearTodoQueueDecisionSelectedPriority, CandidateCount: 1, SelectedPriority: &priority, SelectedTeamKey: "IFAN", SelectedIssueSequence: &sequence, SelectedIssueUUID: dispatchUUID("selected-active"), ExistingRunPreventedScan: true},
		{Reason: LinearTodoQueueDecisionActiveRun, CandidateCount: 0, ExistingRunPreventedScan: false},
		{Reason: LinearTodoQueueDecisionActiveRun, CandidateCount: 1, ExistingRunPreventedScan: true},
	}
	for _, decision := range invalid {
		if err := decision.Validate(); err == nil {
			t.Fatalf("invalid decision accepted: %+v", decision)
		}
	}
}

func TestLinearTodoDispatcherTreatsReservationCapacityRaceAsWaiting(t *testing.T) {
	candidate := dispatchCandidate("capacity-race", "IFAN-12", 1)
	dispatcher, store, scanner, _, starter, driver := newDispatchLab(t, candidate)
	store.reserveUnavailable = true
	lease, acquired, err := store.AcquireLinearTodoAdmissionLease(context.Background(), dispatcher.policy.OwnerNonce, time.Minute, store.now)
	if err != nil || !acquired {
		t.Fatalf("lease=%+v acquired=%t err=%v", lease, acquired, err)
	}
	snapshot, repository, err := admitLinearTask(dispatchSource(candidate), dispatcher.resolver)
	if err != nil {
		t.Fatal(err)
	}
	result, err := dispatcher.reserveStartAndDrive(context.Background(), &lease, linearTodoDispatchCandidate{candidate: scanner.scan.Candidates[0], snapshot: snapshot, repository: repository}, scanner.scan.Digest)
	if err != nil || result.Outcome != LinearTodoDispatchWaiting || result.QueueDecision == nil || result.QueueDecision.Reason != LinearTodoQueueDecisionCapacityFull || len(store.attention) != 0 || len(starter.calls) != 0 || len(driver.calls) != 0 {
		t.Fatalf("result=%+v attention=%+v starter=%+v driver=%+v err=%v", result, store.attention, starter.calls, driver.calls, err)
	}
}

func TestLinearTodoDispatcherPreservesReservationCapacityDecision(t *testing.T) {
	candidate := dispatchCandidate("capacity-decision", "IFAN-75", 1)
	dispatcher, store, _, _, starter, driver := newDispatchLab(t, candidate)
	store.reserveUnavailable = true

	result, err := dispatcher.Dispatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != LinearTodoDispatchWaiting || result.QueueDecision == nil || result.QueueDecision.Reason != LinearTodoQueueDecisionCapacityFull {
		t.Fatalf("result=%+v", result)
	}
	if len(store.attention) != 0 || len(starter.calls) != 0 || len(driver.calls) != 0 {
		t.Fatalf("attention=%+v starter=%+v driver=%+v", store.attention, starter.calls, driver.calls)
	}
}

func TestLinearTodoDispatcherAdoptsReservedRunOnRestartWithoutScanning(t *testing.T) {
	candidate := dispatchCandidate("restart", "IFAN-13", 1)
	dispatcher, store, scanner, _, _, driver := newDispatchLab(t, candidate)
	inputSnapshot, repository, err := admitLinearTask(dispatchSource(candidate), dispatchResolver{repository: LocalRepository{CanonicalRepository: "owner/repo", BaseBranch: "main", RunRoot: "/tmp/dispatch-runs", WorktreeRoot: "/tmp/dispatch-worktrees", ProfileID: "profile-owner-repo", ProfileSnapshotVersion: 1, ProfileDigest: dispatchDigest("profile"), ProfileSnapshotJSON: `{}`, RegistryVersion: 1, RegistryDigest: dispatchDigest("registry"), RepositoryBindingDigest: dispatchDigest("binding"), VerifierIDs: []string{"go-test"}, AllowedOperatorLogins: []string{"operator"}}})
	if err != nil {
		t.Fatal(err)
	}
	run, err := ReservedRunFromAdmissionSnapshot(linearTodoDispatchInput(inputSnapshot, repository))
	if err != nil {
		t.Fatal(err)
	}
	run.State = domain.StateReceived
	store.run = run
	store.journalFound = true
	store.journal = LinearTodoAdmissionJournal{IssueUUID: candidate.IssueID, RunID: run.ID, ScanDigest: dispatchDigest("old-scan"), TaskDigest: run.TaskHash, ProfileDigest: run.ProfileDigest, Status: LinearTodoAdmissionJournalReserved, CreatedAt: store.now, UpdatedAt: store.now}
	if input, inputErr := linearTodoDispatchInputFromRun(run); inputErr != nil {
		t.Fatalf("restart input: %v", inputErr)
	} else if input.IdempotencyKey != run.IdempotencyKey {
		t.Fatalf("restart input key=%s run=%s", input.IdempotencyKey, run.IdempotencyKey)
	} else if !samePersistedProfile(run, input.Repository) {
		t.Fatalf("persisted profile mismatch run=%+v repository=%+v", run, input.Repository)
	}

	result, err := dispatcher.Dispatch(context.Background())
	if err != nil || result.Outcome != LinearTodoDispatchDriven || scanner.calls != 0 || store.reserveCalls != 0 || store.adoptCalls != 1 || len(driver.calls) != 1 || store.run.ID != run.ID || len(store.attention) != 0 {
		t.Fatalf("result=%+v scanner=%d reserve=%d adopt=%d driver=%+v journal=%+v side=%+v run=%+v attention=%+v err=%v", result, scanner.calls, store.reserveCalls, store.adoptCalls, driver.calls, store.journal, store.side, store.run, store.attention, err)
	}
}

func TestLinearTodoDispatcherScansNextCandidateAfterAbandonedRunWithRetainedRetryAttention(t *testing.T) {
	candidate := dispatchCandidate("after-abandon", "IFAN-74", 1)
	dispatcher, store, scanner, _, _, driver := newDispatchLab(t, candidate)
	now := store.now
	store.run = authorizeDispatchRun(Run{ID: "run-abandoned", IssueID: "IFAN-OLD", IdempotencyKey: "abandoned-key", Repository: "owner/repo", State: domain.StateFailed})
	store.retrySchedules = []RetrySchedule{{RunID: store.run.ID, Phase: "state_executing", ControllerState: string(domain.StateFailed), AttemptCount: 2, MaxAttempts: 1, InitialDelay: time.Second, MaximumDelay: 30 * time.Second, FailureClass: RetryFailureManual, ReasonCode: RetryReasonManual, Status: RetryScheduleAttention, AttentionAt: now, CreatedAt: now.Add(-time.Minute), UpdatedAt: now}}

	result, err := dispatcher.Dispatch(context.Background())
	if err != nil || result.Outcome != LinearTodoDispatchDriven || scanner.calls != 1 || store.reserveCalls != 1 || len(driver.calls) != 1 || store.run.IssueID != candidate.Identifier {
		t.Fatalf("result=%+v scanner=%d reserve=%d driver=%+v run=%+v err=%v", result, scanner.calls, store.reserveCalls, driver.calls, store.run, err)
	}
}

func TestLinearTodoDispatcherAutomaticallyResumesRunWithSupersededCIWaitSchedule(t *testing.T) {
	candidate := dispatchCandidate("ci-resume", "IFAN-77", 1)
	dispatcher, store, scanner, _, _, driver := newDispatchLab(t, candidate)
	store.run = authorizeDispatchRun(Run{ID: "run-ci-resume", IssueID: candidate.Identifier, IdempotencyKey: "ci-resume-key", Repository: "owner/repo", State: domain.StatePROpen})
	now := store.now
	store.retrySchedules = []RetrySchedule{{RunID: store.run.ID, Phase: AutomaticRetryPhaseForRun(store.run), ControllerState: string(store.run.State), AttemptCount: 1, MaxAttempts: 3, InitialDelay: time.Second, MaximumDelay: 30 * time.Second, FailureClass: RetryFailureTerminal, ReasonCode: RetryReasonTerminal, Status: RetryScheduleSuperseded, AttentionAt: now, CreatedAt: now.Add(-time.Minute), UpdatedAt: now}}
	result, err := dispatcher.Dispatch(context.Background())
	if err != nil || scanner.calls != 0 || len(driver.calls) != 1 || result.Outcome != LinearTodoDispatchDriven {
		t.Fatalf("result=%+v scanner=%d driver=%+v err=%v", result, scanner.calls, driver.calls, err)
	}
}

func TestLinearTodoDispatcherStopsForManualAndDriverConflict(t *testing.T) {
	for _, test := range []struct {
		state      domain.State
		outcome    string
		attentions int
		drives     int
	}{{domain.StateManualIntervention, LinearTodoDispatchAttention, 1, 0}, {domain.StateAwaitingHumanDecision, LinearTodoDispatchAttention, 1, 0}, {domain.StateAwaitingHumanApproval, LinearTodoDispatchDriven, 0, 1}} {
		t.Run(string(test.state), func(t *testing.T) {
			candidate := dispatchCandidate("manual", "IFAN-14", 1)
			dispatcher, store, scanner, _, _, driver := newDispatchLab(t, candidate)
			store.run = authorizeDispatchRun(Run{ID: "run-manual", IssueID: candidate.Identifier, IdempotencyKey: "manual-key", Repository: "owner/repo", State: test.state})
			store.ciWaitActive = true
			result, err := dispatcher.Dispatch(context.Background())
			if err != nil || result.Outcome != test.outcome || scanner.calls != 0 || len(driver.calls) != test.drives || len(store.attention) != test.attentions || store.ciWaitClosed != 1 || store.ciWaitClosedAt != store.now || store.attentionBeforeCIClose {
				t.Fatalf("result=%+v scanner=%d driver=%+v attention=%+v err=%v", result, scanner.calls, driver.calls, store.attention, err)
			}
			if test.state == domain.StateManualIntervention && store.attention[0].EventType != OperatorAttentionManualIntervention {
				t.Fatalf("attention=%+v", store.attention)
			}
			if test.state == domain.StateAwaitingHumanDecision && store.attention[0].EventType != OperatorAttentionHumanDecision {
				t.Fatalf("attention=%+v", store.attention)
			}
		})
	}
	t.Run("terminal restart closes residual wait before returning", func(t *testing.T) {
		for _, state := range []domain.State{domain.StateCompleted, domain.StateFailed, domain.StateRejected} {
			dispatcher, store, scanner, _, _, driver := newDispatchLab(t)
			store.run = authorizeDispatchRun(Run{ID: "run-terminal", IssueID: "IFAN-OLD", IdempotencyKey: "terminal-key", Repository: "owner/repo", State: state})
			store.ciWaitActive = true
			result, err := dispatcher.Dispatch(context.Background())
			if err != nil || result.Outcome != LinearTodoDispatchNoCandidate || scanner.calls != 1 || len(driver.calls) != 0 || store.ciWaitClosed != 1 || store.ciWaitClosedAt != store.now || store.attentionBeforeCIClose {
				t.Fatalf("state=%s result=%+v scanner=%d driver=%v close=%d err=%v", state, result, scanner.calls, driver.calls, store.ciWaitClosed, err)
			}
		}
	})
	t.Run("decision evidence drift stays parked with stable fail-closed attention", func(t *testing.T) {
		candidate := dispatchCandidate("decision-drift", "IFAN-14", 1)
		dispatcher, store, scanner, _, _, driver := newDispatchLab(t, candidate)
		store.run = authorizeDispatchRun(Run{ID: "run-decision-drift", IssueID: candidate.Identifier, IdempotencyKey: "decision-drift-key", Repository: "owner/repo", State: domain.StateAwaitingHumanDecision})
		store.omitDecisionTransition = true

		first, firstErr := dispatcher.Dispatch(context.Background())
		second, secondErr := dispatcher.Dispatch(context.Background())
		if firstErr != nil || secondErr != nil || first.Outcome != LinearTodoDispatchAttention || second.Outcome != LinearTodoDispatchAttention || scanner.calls != 0 || len(driver.calls) != 0 || store.run.State != domain.StateAwaitingHumanDecision || len(store.attention) != 1 {
			t.Fatalf("first=%+v second=%+v scanner=%d driver=%+v run=%+v attention=%+v firstErr=%v secondErr=%v", first, second, scanner.calls, driver.calls, store.run, store.attention, firstErr, secondErr)
		}
		if store.attention[0].EventType != OperatorAttentionAdmissionAuthority || store.attention[0].ReasonCode != "admission_authority_conflict" {
			t.Fatalf("attention=%+v", store.attention)
		}
	})
	t.Run("driver conflict", func(t *testing.T) {
		candidate := dispatchCandidate("conflict", "IFAN-15", 1)
		dispatcher, store, _, _, _, driver := newDispatchLab(t, candidate)
		store.run = authorizeDispatchRun(Run{ID: "run-conflict", IssueID: candidate.Identifier, IdempotencyKey: "conflict-key", Repository: "owner/repo", State: domain.StateExecuting})
		driver.err = serviceError(ErrorConflict, "driver authority changed", nil)
		result, err := dispatcher.Dispatch(context.Background())
		if err != nil || result.Outcome != LinearTodoDispatchAttention || len(driver.calls) != 1 || len(store.attention) != 1 || store.attention[0].EventType != OperatorAttentionRetry || store.attention[0].ReasonCode != RetryReasonAuthority {
			t.Fatalf("result=%+v driver=%+v attention=%+v err=%v", result, driver.calls, store.attention, err)
		}
	})
}

func TestLinearTodoDispatcherLeaseConflictAndCandidateReadFailureDoNotReserve(t *testing.T) {
	t.Run("lease conflict", func(t *testing.T) {
		candidate := dispatchCandidate("lease", "IFAN-16", 1)
		dispatcher, store, scanner, _, starter, driver := newDispatchLab(t, candidate)
		store.held = true
		result, err := dispatcher.Dispatch(context.Background())
		if err != nil || result.Outcome != LinearTodoDispatchAttention || scanner.calls != 0 || store.reserveCalls != 0 || len(starter.calls) != 0 || len(driver.calls) != 0 || len(store.attention) != 1 || store.attention[0].EventType != OperatorAttentionSchedulerLease || store.attention[0].ReasonCode != "lease_conflict" {
			t.Fatalf("result=%+v scanner=%d reserve=%d starter=%+v driver=%+v attention=%+v err=%v", result, scanner.calls, store.reserveCalls, starter.calls, driver.calls, store.attention, err)
		}
	})
	t.Run("candidate read failure", func(t *testing.T) {
		candidate := dispatchCandidate("read", "IFAN-17", 1)
		dispatcher, store, _, reader, starter, driver := newDispatchLab(t, candidate)
		reader.err = errors.New("untrusted external text")
		result, err := dispatcher.Dispatch(context.Background())
		if err != nil || result.Outcome != LinearTodoDispatchAttention || store.reserveCalls != 0 || len(starter.calls) != 0 || len(driver.calls) != 0 || len(store.attention) != 1 {
			t.Fatalf("result=%+v reserve=%d starter=%+v driver=%+v attention=%+v err=%v", result, store.reserveCalls, starter.calls, driver.calls, store.attention, err)
		}
	})
	t.Run("lease lost after complete scan", func(t *testing.T) {
		candidate := dispatchCandidate("lost-lease", "IFAN-28", 1)
		dispatcher, store, scanner, _, starter, driver := newDispatchLab(t, candidate)
		store.leaseLost = true
		result, err := dispatcher.Dispatch(context.Background())
		if err != nil || result.Outcome != LinearTodoDispatchAttention || scanner.calls != 0 || store.reserveCalls != 0 || len(starter.calls) != 0 || len(driver.calls) != 0 || len(store.attention) != 1 || store.attention[0].EventType != OperatorAttentionSchedulerLease || store.attention[0].ReasonCode != "lease_lost" {
			t.Fatalf("result=%+v scanner=%d reserve=%d starter=%+v driver=%+v attention=%+v err=%v", result, scanner.calls, store.reserveCalls, starter.calls, driver.calls, store.attention, err)
		}
	})
}

func TestLinearTodoDispatcherExcludesInvalidCandidatesBeforePrioritySelection(t *testing.T) {
	t.Run("invalid lower priority does not block unique best", func(t *testing.T) {
		best, invalid := dispatchCandidate("best", "IFAN-22", 1), dispatchCandidate("invalid-lower", "IFAN-23", 3)
		dispatcher, store, _, reader, starter, driver := newDispatchLab(t, best, invalid)
		reader.errs[invalid.Identifier] = errors.New("untrusted unavailable response")
		result, err := dispatcher.Dispatch(context.Background())
		if err != nil || result.Outcome != LinearTodoDispatchDriven || store.run.IssueID != best.Identifier || store.reserveCalls != 1 || len(starter.calls) != 1 || starter.calls[0].IssueID != best.IssueID || len(driver.calls) != 1 || len(store.attention) != 0 {
			t.Fatalf("result=%+v run=%+v reserve=%d starter=%+v driver=%+v attention=%+v err=%v", result, store.run, store.reserveCalls, starter.calls, driver.calls, store.attention, err)
		}
	})
	t.Run("higher priority source drift forces rescan", func(t *testing.T) {
		invalid, best := dispatchCandidate("invalid-higher", "IFAN-24", 1), dispatchCandidate("best-lower", "IFAN-25", 2)
		dispatcher, store, _, reader, starter, driver := newDispatchLab(t, invalid, best)
		drifted := reader.sources[invalid.Identifier]
		drifted.BranchName = "ifan/drifted-authority"
		reader.sources[invalid.Identifier] = drifted
		result, err := dispatcher.Dispatch(context.Background())
		if err != nil || result.Outcome != LinearTodoDispatchAttention || store.reserveCalls != 0 || len(starter.calls) != 0 || len(driver.calls) != 0 || len(store.attention) != 1 {
			t.Fatalf("result=%+v run=%+v reserve=%d starter=%+v driver=%+v attention=%+v err=%v", result, store.run, store.reserveCalls, starter.calls, driver.calls, store.attention, err)
		}
	})
	t.Run("re-read team UUID drift does not block valid best", func(t *testing.T) {
		invalid, best := dispatchCandidate("team-drift", "IFAN-29", 1), dispatchCandidate("team-best", "IFAN-30", 2)
		dispatcher, store, _, reader, starter, driver := newDispatchLab(t, invalid, best)
		drifted := reader.sources[invalid.Identifier]
		drifted.Team.ID = dispatchUUID("different-team")
		reader.sources[invalid.Identifier] = drifted
		result, err := dispatcher.Dispatch(context.Background())
		if err != nil || result.Outcome != LinearTodoDispatchDriven || store.run.IssueID != best.Identifier || store.reserveCalls != 1 || len(starter.calls) != 1 || starter.calls[0].IssueID != best.IssueID || len(driver.calls) != 1 || len(store.attention) != 0 {
			t.Fatalf("result=%+v run=%+v reserve=%d starter=%+v driver=%+v attention=%+v err=%v", result, store.run, store.reserveCalls, starter.calls, driver.calls, store.attention, err)
		}
	})
	t.Run("all invalid candidates require attention", func(t *testing.T) {
		first, second := dispatchCandidate("all-invalid-one", "IFAN-26", 1), dispatchCandidate("all-invalid-two", "IFAN-27", 2)
		dispatcher, store, scanner, reader, starter, driver := newDispatchLab(t, first, second)
		reader.errs[first.Identifier] = errors.New("untrusted failure one")
		drifted := reader.sources[second.Identifier]
		drifted.Labels = []LinearLabel{{ID: dispatchUUID("different"), Name: "agent:codex"}}
		reader.sources[second.Identifier] = drifted
		result, err := dispatcher.Dispatch(context.Background())
		if err != nil || result.Outcome != LinearTodoDispatchAttention || scanner.calls != 1 || store.reserveCalls != 0 || len(starter.calls) != 0 || len(driver.calls) != 0 || len(store.attention) != 1 || store.attention[0].EventType != OperatorAttentionCandidateScan || store.attention[0].ReasonCode != "incomplete_authority" {
			t.Fatalf("result=%+v scanner=%d reserve=%d starter=%+v driver=%+v attention=%+v err=%v", result, scanner.calls, store.reserveCalls, starter.calls, driver.calls, store.attention, err)
		}
		if result.QueueDecision == nil || result.QueueDecision.Reason != LinearTodoQueueDecisionIncompleteScan || result.QueueDecision.CandidateCount != 2 || result.QueueDecision.ExistingRunPreventedScan {
			t.Fatalf("queue decision=%+v", result.QueueDecision)
		}
	})
}

func TestLinearTodoDispatcherMutationAndPostStartConflictsStopWithoutAnotherCandidate(t *testing.T) {
	t.Run("mutation", func(t *testing.T) {
		first, second := dispatchCandidate("mutation-first", "IFAN-18", 1), dispatchCandidate("mutation-second", "IFAN-19", 2)
		dispatcher, store, _, _, starter, driver := newDispatchLab(t, first, second)
		starter.err = &LinearIssueStartMutationError{Class: "graphql"}
		result, err := dispatcher.Dispatch(context.Background())
		if err != nil || result.Outcome != LinearTodoDispatchAttention || store.reserveCalls != 1 || store.run.IssueID != first.Identifier || len(starter.calls) != 1 || len(driver.calls) != 0 || len(store.attention) != 1 || store.attention[0].EventType != OperatorAttentionManualIntervention || store.journal.Status != "manual_intervention" {
			t.Fatalf("result=%+v reserve=%d run=%+v starter=%+v driver=%+v journal=%+v attention=%+v err=%v", result, store.reserveCalls, store.run, starter.calls, driver.calls, store.journal, store.attention, err)
		}
	})
	t.Run("post start proof", func(t *testing.T) {
		candidate := dispatchCandidate("post-proof", "IFAN-20", 1)
		dispatcher, store, _, _, starter, driver := newDispatchLab(t, candidate)
		store.postProofDrift = true
		result, err := dispatcher.Dispatch(context.Background())
		if err != nil || result.Outcome != LinearTodoDispatchAttention || store.reserveCalls != 1 || len(starter.calls) != 1 || len(driver.calls) != 0 || len(store.attention) != 1 {
			t.Fatalf("result=%+v reserve=%d starter=%+v driver=%+v attention=%+v err=%v", result, store.reserveCalls, starter.calls, driver.calls, store.attention, err)
		}
	})
}

func TestLinearTodoDispatcherConcurrentCycleCannotReserveOrDriveSecondCandidate(t *testing.T) {
	candidate := dispatchCandidate("concurrent", "IFAN-21", 1)
	dispatcher, store, scanner, _, starter, driver := newDispatchLab(t, candidate)
	driver.started, driver.allow = make(chan struct{}), make(chan struct{})
	firstDone := make(chan struct {
		result LinearTodoDispatchResult
		err    error
	}, 1)
	go func() {
		result, err := dispatcher.Dispatch(context.Background())
		firstDone <- struct {
			result LinearTodoDispatchResult
			err    error
		}{result, err}
	}()
	select {
	case <-driver.started:
	case <-time.After(time.Second):
		t.Fatal("first cycle did not reach exact-run driver")
	}
	second, err := dispatcher.Dispatch(context.Background())
	if err != nil || second.Outcome != LinearTodoDispatchAttention || scanner.calls != 1 || store.reserveCalls != 1 || len(starter.calls) != 1 || len(driver.calls) != 1 || len(store.attention) != 1 {
		t.Fatalf("second=%+v err=%v scanner=%d reserve=%d starter=%+v driver=%+v attention=%+v", second, err, scanner.calls, store.reserveCalls, starter.calls, driver.calls, store.attention)
	}
	close(driver.allow)
	first := <-firstDone
	if first.err != nil || first.result.Outcome != LinearTodoDispatchDriven {
		t.Fatalf("first=%+v err=%v", first.result, first.err)
	}
}

func TestLinearTodoDispatcherRenewsConfiguredLeaseAcrossBlockedStartAndDriverHandoff(t *testing.T) {
	candidate := dispatchCandidate("long-start", "IFAN-34", 1)
	dispatcher, store, _, _, _, driver := newDispatchLab(t, candidate)
	store.startEntered, store.startAllow = make(chan struct{}), make(chan struct{})
	driver.started = make(chan struct{})
	ticks := make(chan time.Time)
	tickerStopped := make(chan struct{})
	var configuredInterval time.Duration
	dispatcher.leaseTicks = func(interval time.Duration) (<-chan time.Time, func()) {
		configuredInterval = interval
		return ticks, func() { close(tickerStopped) }
	}
	var nowNanos atomic.Int64
	nowNanos.Store(store.now.UnixNano())
	dispatcher.now = func() time.Time { return time.Unix(0, nowNanos.Load()).UTC() }
	store.renewed = make(chan int, 1)

	done := make(chan struct {
		result LinearTodoDispatchResult
		err    error
	}, 1)
	go func() {
		result, err := dispatcher.Dispatch(context.Background())
		done <- struct {
			result LinearTodoDispatchResult
			err    error
		}{result, err}
	}()
	select {
	case <-store.startEntered:
	case <-time.After(time.Second):
		t.Fatal("cycle did not reach initial local start")
	}
	if configuredInterval != dispatcher.policy.LeaseRenewal {
		t.Fatalf("lease ticker interval=%s configured=%s", configuredInterval, dispatcher.policy.LeaseRenewal)
	}

	for elapsed := dispatcher.policy.LeaseRenewal; elapsed <= dispatcher.policy.LeaseTTL+dispatcher.policy.LeaseRenewal; elapsed += dispatcher.policy.LeaseRenewal {
		nowNanos.Store(store.now.Add(elapsed).UnixNano())
		ticks <- time.Unix(0, nowNanos.Load()).UTC()
		select {
		case <-store.renewed:
		case <-time.After(time.Second):
			t.Fatalf("blocked initial start did not renew at elapsed=%s", elapsed)
		}
	}
	close(store.startAllow)
	select {
	case <-driver.started:
	case <-time.After(time.Second):
		t.Fatal("driver did not begin in the same dispatch after initial start")
	}
	observed := <-done
	if observed.err != nil || observed.result.Outcome != LinearTodoDispatchDriven {
		t.Fatalf("result=%+v err=%v", observed.result, observed.err)
	}
	select {
	case <-tickerStopped:
	default:
		t.Fatal("lease renewal ticker did not terminate after driver completion")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.attention) != 0 || store.held || store.releasedLease.Version != store.lease.Version {
		t.Fatalf("attention=%+v held=%t released=%+v lease=%+v", store.attention, store.held, store.releasedLease, store.lease)
	}
}

func TestLinearTodoDispatcherFailsClosedWhenLeaseRenewalFailsDuringBlockedStart(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*dispatchStore)
	}{
		{name: "ownership lost", setup: func(store *dispatchStore) { store.leaseLost = true }},
		{name: "persistence failure", setup: func(store *dispatchStore) { store.renewErrAt = store.renewCalls + 1 }},
		{name: "stale owner", setup: func(store *dispatchStore) { store.lease.OwnerNonce = "takeover-owner" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := dispatchCandidate("blocked-start-"+test.name, "IFAN-38", 1)
			dispatcher, store, _, _, _, driver := newDispatchLab(t, candidate)
			store.startEntered, store.startAllow = make(chan struct{}), make(chan struct{})
			ticks := make(chan time.Time)
			tickerStopped := make(chan struct{})
			dispatcher.leaseTicks = func(time.Duration) (<-chan time.Time, func()) {
				return ticks, func() { close(tickerStopped) }
			}
			done := make(chan struct {
				result LinearTodoDispatchResult
				err    error
			}, 1)
			go func() {
				result, err := dispatcher.Dispatch(context.Background())
				done <- struct {
					result LinearTodoDispatchResult
					err    error
				}{result, err}
			}()
			select {
			case <-store.startEntered:
			case <-time.After(time.Second):
				t.Fatal("cycle did not reach blocked initial start")
			}
			store.mu.Lock()
			test.setup(store)
			store.mu.Unlock()
			ticks <- time.Now()
			select {
			case observed := <-done:
				if observed.err != nil || observed.result.Outcome != LinearTodoDispatchAttention {
					t.Fatalf("result=%+v err=%v", observed.result, observed.err)
				}
			case <-time.After(time.Second):
				t.Fatal("lease renewal failure did not cancel blocked initial start")
			}
			select {
			case <-tickerStopped:
			default:
				t.Fatal("lease renewal ticker did not terminate after lease failure")
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			if len(driver.calls) != 0 || len(store.attention) != 1 || store.attention[0].EventType != OperatorAttentionSchedulerLease || store.attention[0].ReasonCode != "lease_lost" {
				t.Fatalf("driver=%+v attention=%+v", driver.calls, store.attention)
			}
		})
	}
}

func TestLinearTodoDispatcherJoinsInflightRenewalBeforeAcceptingDriverOutcome(t *testing.T) {
	driverFailure := errors.New("driver failed while lease renewal was in flight")
	for _, test := range []struct {
		name      string
		driverErr error
		setup     func(*dispatchStore)
	}{
		{name: "takeover after driver success", setup: func(store *dispatchStore) { store.leaseLost = true }},
		{name: "stale owner after driver success", setup: func(store *dispatchStore) { store.lease.OwnerNonce = "takeover-owner" }},
		{name: "persistence failure after driver success", setup: func(store *dispatchStore) { store.renewErrAt = store.renewCalls + 1 }},
		{name: "lease loss takes precedence over driver error", driverErr: driverFailure, setup: func(store *dispatchStore) { store.failRenewAt = store.renewCalls + 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := dispatchCandidate("inflight-renew-"+test.name, "IFAN-40", 1)
			dispatcher, store, _, _, _, driver := newDispatchLab(t, candidate)
			driver.started, driver.returned, driver.allow = make(chan struct{}), make(chan struct{}), make(chan struct{})
			driver.err = test.driverErr
			ticks := make(chan time.Time)
			dispatcher.leaseTicks = func(time.Duration) (<-chan time.Time, func()) {
				return ticks, func() {}
			}
			done := make(chan struct {
				result LinearTodoDispatchResult
				err    error
			}, 1)
			go func() {
				result, err := dispatcher.Dispatch(context.Background())
				done <- struct {
					result LinearTodoDispatchResult
					err    error
				}{result, err}
			}()
			select {
			case <-driver.started:
			case <-time.After(time.Second):
				t.Fatal("cycle did not reach exact-run driver")
			}

			store.mu.Lock()
			store.renewEntered, store.renewAllow = make(chan struct{}), make(chan struct{})
			test.setup(store)
			renewEntered, renewAllow := store.renewEntered, store.renewAllow
			store.mu.Unlock()
			ticks <- time.Now()
			select {
			case <-renewEntered:
			case <-time.After(time.Second):
				t.Fatal("lease renewal did not enter persistence CAS")
			}
			close(driver.allow)
			select {
			case <-driver.returned:
			case <-time.After(time.Second):
				t.Fatal("driver did not return while lease renewal was blocked")
			}
			select {
			case observed := <-done:
				t.Fatalf("dispatch accepted driver outcome before renewal joined: result=%+v err=%v", observed.result, observed.err)
			default:
			}

			close(renewAllow)
			select {
			case observed := <-done:
				if observed.err != nil || observed.result.Outcome != LinearTodoDispatchAttention {
					t.Fatalf("result=%+v err=%v", observed.result, observed.err)
				}
			case <-time.After(time.Second):
				t.Fatal("dispatch did not report lease attention after final CAS failed")
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			if len(store.attention) != 1 || store.attention[0].EventType != OperatorAttentionSchedulerLease || store.attention[0].ReasonCode != "lease_lost" {
				t.Fatalf("attention=%+v", store.attention)
			}
		})
	}
}

func TestLinearTodoDispatcherSettlesContextAwareRenewalWithoutFabricatingLeaseLoss(t *testing.T) {
	candidate := dispatchCandidate("context-aware-settle", "IFAN-41", 1)
	dispatcher, store, _, _, _, driver := newDispatchLab(t, candidate)
	driver.started, driver.allow = make(chan struct{}), make(chan struct{})
	ticks := make(chan time.Time)
	tickerStopped := make(chan struct{})
	dispatcher.leaseTicks = func(time.Duration) (<-chan time.Time, func()) {
		return ticks, func() { close(tickerStopped) }
	}
	done := make(chan struct {
		result LinearTodoDispatchResult
		err    error
	}, 1)
	go func() {
		result, err := dispatcher.Dispatch(context.Background())
		done <- struct {
			result LinearTodoDispatchResult
			err    error
		}{result, err}
	}()
	select {
	case <-driver.started:
	case <-time.After(time.Second):
		t.Fatal("cycle did not reach exact-run driver")
	}
	store.mu.Lock()
	store.renewEntered, store.renewCanceled = make(chan struct{}), make(chan struct{})
	store.renewWaitForCancel = true
	renewEntered, renewCanceled := store.renewEntered, store.renewCanceled
	store.mu.Unlock()
	ticks <- time.Now()
	select {
	case <-renewEntered:
	case <-time.After(time.Second):
		t.Fatal("context-aware renewal did not enter persistence")
	}
	close(driver.allow)
	select {
	case <-renewCanceled:
	case <-time.After(time.Second):
		t.Fatal("normal settlement did not cancel context-aware renewal")
	}
	select {
	case observed := <-done:
		if observed.err != nil || observed.result.Outcome != LinearTodoDispatchDriven {
			t.Fatalf("result=%+v err=%v", observed.result, observed.err)
		}
	case <-time.After(time.Second):
		t.Fatal("context-aware renewal settlement deadlocked")
	}
	select {
	case <-tickerStopped:
	default:
		t.Fatal("lease ticker did not terminate after normal settlement")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.attention) != 0 || store.held {
		t.Fatalf("attention=%+v held=%t", store.attention, store.held)
	}
}

func TestLinearTodoDispatcherSettlesStartOutcomesBeforeAcceptingThem(t *testing.T) {
	startFailure := errors.New("initial start failed while renewal was in flight")
	for _, test := range []struct {
		name              string
		startErr          error
		postProofConflict bool
		setup             func(*dispatchStore)
	}{
		{name: "start error racing takeover", startErr: startFailure, setup: func(store *dispatchStore) { store.leaseLost = true }},
		{name: "start error racing stale owner", startErr: startFailure, setup: func(store *dispatchStore) { store.lease.OwnerNonce = "takeover-owner" }},
		{name: "start error racing persistence loss", startErr: startFailure, setup: func(store *dispatchStore) { store.renewErrAt = store.renewCalls + 1 }},
		{name: "post start proof conflict racing lease loss", postProofConflict: true, setup: func(store *dispatchStore) { store.failRenewAt = store.renewCalls + 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := dispatchCandidate("settled-start-"+test.name, "IFAN-42", 1)
			dispatcher, store, _, _, _, driver := newDispatchLab(t, candidate)
			store.startEntered, store.startReturned, store.startAllow = make(chan struct{}), make(chan struct{}), make(chan struct{})
			store.startErr, store.postProofDrift = test.startErr, test.postProofConflict
			ticks := make(chan time.Time)
			dispatcher.leaseTicks = func(time.Duration) (<-chan time.Time, func()) {
				return ticks, func() {}
			}
			done := make(chan struct {
				result LinearTodoDispatchResult
				err    error
			}, 1)
			go func() {
				result, err := dispatcher.Dispatch(context.Background())
				done <- struct {
					result LinearTodoDispatchResult
					err    error
				}{result, err}
			}()
			select {
			case <-store.startEntered:
			case <-time.After(time.Second):
				t.Fatal("cycle did not reach initial start")
			}
			store.mu.Lock()
			store.renewEntered, store.renewAllow = make(chan struct{}), make(chan struct{})
			test.setup(store)
			renewEntered, renewAllow := store.renewEntered, store.renewAllow
			store.mu.Unlock()
			ticks <- time.Now()
			select {
			case <-renewEntered:
			case <-time.After(time.Second):
				t.Fatal("lease renewal did not enter persistence CAS")
			}
			close(store.startAllow)
			select {
			case <-store.startReturned:
			case <-time.After(time.Second):
				t.Fatal("initial start did not return while renewal was blocked")
			}
			select {
			case observed := <-done:
				t.Fatalf("dispatch accepted start outcome before renewal joined: result=%+v err=%v", observed.result, observed.err)
			default:
			}
			close(renewAllow)
			select {
			case observed := <-done:
				if observed.err != nil || observed.result.Outcome != LinearTodoDispatchAttention {
					t.Fatalf("result=%+v err=%v", observed.result, observed.err)
				}
			case <-time.After(time.Second):
				t.Fatal("dispatch did not report lease attention after start outcome race")
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			if len(driver.calls) != 0 || len(store.attention) != 1 || store.attention[0].EventType != OperatorAttentionSchedulerLease || store.attention[0].ReasonCode != "lease_lost" {
				t.Fatalf("driver=%+v attention=%+v", driver.calls, store.attention)
			}
		})
	}
}

func TestLinearTodoDispatcherRechecksPersistedLeaseAfterScopedWorkWithoutRenewalTick(t *testing.T) {
	workFailure := errors.New("scoped work failed after lease takeover")
	for _, test := range []struct {
		name              string
		outcome           string
		workErr           error
		postProofConflict bool
		changeAuthority   func(*dispatchStore, time.Time)
	}{
		{
			name:    "start error after expiry and takeover",
			outcome: "start",
			workErr: workFailure,
			changeAuthority: func(store *dispatchStore, now time.Time) {
				store.lease.OwnerNonce = "takeover-owner"
				store.lease.Version++
				store.lease.RenewedAt, store.lease.ExpiresAt = now, now.Add(time.Minute)
			},
		},
		{
			name:    "start error with stale owner",
			outcome: "start",
			workErr: workFailure,
			changeAuthority: func(store *dispatchStore, _ time.Time) {
				store.lease.OwnerNonce = "different-owner"
			},
		},
		{
			name:    "start error with lease recheck persistence failure",
			outcome: "start",
			workErr: workFailure,
			changeAuthority: func(store *dispatchStore, _ time.Time) {
				store.heldErr = errors.New("lease authority persistence unavailable")
			},
		},
		{
			name:              "post start proof conflict after takeover",
			outcome:           "start",
			postProofConflict: true,
			changeAuthority: func(store *dispatchStore, now time.Time) {
				store.lease.OwnerNonce = "takeover-owner"
				store.lease.Version++
				store.lease.RenewedAt, store.lease.ExpiresAt = now, now.Add(time.Minute)
			},
		},
		{
			name:    "successful start stops before driver after takeover",
			outcome: "pre-driver",
			changeAuthority: func(store *dispatchStore, now time.Time) {
				store.lease.OwnerNonce = "takeover-owner"
				store.lease.Version++
				store.lease.RenewedAt, store.lease.ExpiresAt = now, now.Add(time.Minute)
			},
		},
		{
			name:    "driver success after takeover",
			outcome: "driver",
			changeAuthority: func(store *dispatchStore, now time.Time) {
				store.lease.OwnerNonce = "takeover-owner"
				store.lease.Version++
				store.lease.RenewedAt, store.lease.ExpiresAt = now, now.Add(time.Minute)
			},
		},
		{
			name:    "driver error after takeover",
			outcome: "driver",
			workErr: workFailure,
			changeAuthority: func(store *dispatchStore, now time.Time) {
				store.lease.OwnerNonce = "takeover-owner"
				store.lease.Version++
				store.lease.RenewedAt, store.lease.ExpiresAt = now, now.Add(time.Minute)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := dispatchCandidate("held-recheck-"+test.name, "IFAN-43", 1)
			dispatcher, store, _, _, _, driver := newDispatchLab(t, candidate)
			var nowNanos atomic.Int64
			nowNanos.Store(store.now.UnixNano())
			dispatcher.now = func() time.Time { return time.Unix(0, nowNanos.Load()).UTC() }
			// No value is ever sent: ownership loss must be found by the
			// authoritative post-scope check, not by a renewal CAS.
			dispatcher.leaseTicks = func(time.Duration) (<-chan time.Time, func()) {
				return make(chan time.Time), func() {}
			}
			switch test.outcome {
			case "start", "pre-driver":
				store.startEntered, store.startAllow = make(chan struct{}), make(chan struct{})
				store.startErr, store.postProofDrift = test.workErr, test.postProofConflict
			case "driver":
				driver.started, driver.allow, driver.err = make(chan struct{}), make(chan struct{}), test.workErr
			default:
				t.Fatalf("unknown fixture outcome %q", test.outcome)
			}
			done := make(chan struct {
				result LinearTodoDispatchResult
				err    error
			}, 1)
			go func() {
				result, err := dispatcher.Dispatch(context.Background())
				done <- struct {
					result LinearTodoDispatchResult
					err    error
				}{result, err}
			}()
			if test.outcome == "start" || test.outcome == "pre-driver" {
				select {
				case <-store.startEntered:
				case <-time.After(time.Second):
					t.Fatal("cycle did not reach initial start")
				}
			} else {
				select {
				case <-driver.started:
				case <-time.After(time.Second):
					t.Fatal("cycle did not reach exact-run driver")
				}
			}
			advanced := store.now.Add(dispatcher.policy.LeaseTTL + dispatcher.policy.LeaseRenewal)
			nowNanos.Store(advanced.UnixNano())
			store.mu.Lock()
			test.changeAuthority(store, advanced)
			store.mu.Unlock()
			if test.outcome == "start" || test.outcome == "pre-driver" {
				close(store.startAllow)
			} else {
				close(driver.allow)
			}
			select {
			case observed := <-done:
				if observed.err != nil || observed.result.Outcome != LinearTodoDispatchAttention {
					t.Fatalf("result=%+v err=%v", observed.result, observed.err)
				}
			case <-time.After(time.Second):
				t.Fatal("authoritative lease recheck did not bound scoped outcome")
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			if len(store.attention) != 1 || store.attention[0].EventType != OperatorAttentionSchedulerLease || store.attention[0].ReasonCode != "lease_lost" {
				t.Fatalf("attention=%+v", store.attention)
			}
			if test.outcome != "driver" && len(driver.calls) != 0 {
				t.Fatalf("driver called after pre-driver lease loss: %+v", driver.calls)
			}
		})
	}
}

func TestLinearTodoDispatcherBoundsPersistedLeaseRecheck(t *testing.T) {
	previousTimeout := dispatchLeaseAuthorityCheckTimeout
	dispatchLeaseAuthorityCheckTimeout = 20 * time.Millisecond
	defer func() { dispatchLeaseAuthorityCheckTimeout = previousTimeout }()

	candidate := dispatchCandidate("bounded-held-recheck", "IFAN-44", 1)
	dispatcher, store, _, _, _, driver := newDispatchLab(t, candidate)
	dispatcher.leaseTicks = func(time.Duration) (<-chan time.Time, func()) {
		return make(chan time.Time), func() {}
	}
	store.heldWaitForCancel, store.heldCanceled = true, make(chan struct{})
	done := make(chan struct {
		result LinearTodoDispatchResult
		err    error
	}, 1)
	go func() {
		result, err := dispatcher.Dispatch(context.Background())
		done <- struct {
			result LinearTodoDispatchResult
			err    error
		}{result, err}
	}()
	select {
	case <-store.heldCanceled:
	case <-time.After(time.Second):
		t.Fatal("persisted lease recheck did not honor its deadline")
	}
	select {
	case observed := <-done:
		if observed.err != nil || observed.result.Outcome != LinearTodoDispatchAttention {
			t.Fatalf("result=%+v err=%v", observed.result, observed.err)
		}
	case <-time.After(time.Second):
		t.Fatal("bounded persisted lease recheck did not fail closed")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.attention) != 1 || store.attention[0].EventType != OperatorAttentionSchedulerLease || store.attention[0].ReasonCode != "lease_lost" {
		t.Fatalf("attention=%+v", store.attention)
	}
	if len(driver.calls) != 0 {
		t.Fatalf("driver called before bounded lease recheck: %+v", driver.calls)
	}
}

func TestLinearTodoDispatcherCancellationJoinsRenewerDuringBlockedStart(t *testing.T) {
	candidate := dispatchCandidate("cancel-start", "IFAN-39", 1)
	dispatcher, store, _, _, _, driver := newDispatchLab(t, candidate)
	store.startEntered, store.startAllow = make(chan struct{}), make(chan struct{})
	ticks := make(chan time.Time)
	tickerStopped := make(chan struct{})
	dispatcher.leaseTicks = func(time.Duration) (<-chan time.Time, func()) {
		return ticks, func() { close(tickerStopped) }
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := dispatcher.Dispatch(ctx)
		done <- err
	}()
	select {
	case <-store.startEntered:
	case <-time.After(time.Second):
		t.Fatal("cycle did not reach blocked initial start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("dispatch cancellation error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("caller cancellation did not cancel blocked initial start")
	}
	select {
	case <-tickerStopped:
	default:
		t.Fatal("lease renewal ticker did not terminate after caller cancellation")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(driver.calls) != 0 || len(store.attention) != 0 || store.held || store.run.State != domain.StateReceived {
		t.Fatalf("driver=%+v attention=%+v held=%t state=%s", driver.calls, store.attention, store.held, store.run.State)
	}
}

func TestLinearTodoDispatcherRenewsLeaseWhileDriveIsStillRunning(t *testing.T) {
	candidate := dispatchCandidate("long-drive", "IFAN-35", 1)
	dispatcher, store, scanner, _, starter, driver := newDispatchLab(t, candidate)
	driver.started, driver.allow = make(chan struct{}), make(chan struct{})
	ticks := make(chan time.Time, 1)
	dispatcher.leaseTicks = func(time.Duration) (<-chan time.Time, func()) { return ticks, func() {} }
	firstDone := make(chan struct {
		result LinearTodoDispatchResult
		err    error
	}, 1)
	go func() {
		result, err := dispatcher.Dispatch(context.Background())
		firstDone <- struct {
			result LinearTodoDispatchResult
			err    error
		}{result, err}
	}()
	select {
	case <-driver.started:
	case <-time.After(time.Second):
		t.Fatal("first cycle did not reach exact-run driver")
	}

	store.mu.Lock()
	store.renewed = make(chan int, 1)
	versionBefore := store.lease.Version
	store.mu.Unlock()
	ticks <- time.Now()
	select {
	case renewal := <-store.renewed:
		if renewal < 1 {
			t.Fatalf("renewal=%d", renewal)
		}
	case <-time.After(time.Second):
		t.Fatal("long-running driver did not renew scheduler lease")
	}
	store.mu.Lock()
	versionAfter := store.lease.Version
	store.mu.Unlock()
	if versionAfter <= versionBefore {
		t.Fatalf("lease version did not advance during drive: before=%d after=%d", versionBefore, versionAfter)
	}

	second, err := dispatcher.Dispatch(context.Background())
	if err != nil || second.Outcome != LinearTodoDispatchAttention || scanner.calls != 1 || store.reserveCalls != 1 || len(starter.calls) != 1 || len(driver.calls) != 1 || len(store.attention) != 1 || store.attention[0].EventType != OperatorAttentionSchedulerLease || store.attention[0].ReasonCode != "lease_conflict" {
		t.Fatalf("second=%+v err=%v scanner=%d reserve=%d starter=%+v driver=%+v attention=%+v", second, err, scanner.calls, store.reserveCalls, starter.calls, driver.calls, store.attention)
	}
	close(driver.allow)
	first := <-firstDone
	if first.err != nil || first.result.Outcome != LinearTodoDispatchDriven {
		t.Fatalf("first=%+v err=%v", first.result, first.err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.held || store.releasedLease.Version != versionAfter {
		t.Fatalf("held=%t released=%+v latest=%+v", store.held, store.releasedLease, store.lease)
	}
}

func TestLinearTodoDispatcherCancelsDriveWhenScopedLeaseRenewalIsLost(t *testing.T) {
	candidate := dispatchCandidate("lease-expiry", "IFAN-36", 1)
	dispatcher, store, _, _, _, driver := newDispatchLab(t, candidate)
	driver.started, driver.allow = make(chan struct{}), make(chan struct{})
	ticks := make(chan time.Time, 1)
	dispatcher.leaseTicks = func(time.Duration) (<-chan time.Time, func()) { return ticks, func() {} }
	done := make(chan struct {
		result LinearTodoDispatchResult
		err    error
	}, 1)
	go func() {
		result, err := dispatcher.Dispatch(context.Background())
		done <- struct {
			result LinearTodoDispatchResult
			err    error
		}{result, err}
	}()
	select {
	case <-driver.started:
	case <-time.After(time.Second):
		t.Fatal("cycle did not reach exact-run driver")
	}
	store.mu.Lock()
	store.failRenewAt = store.renewCalls + 1
	store.mu.Unlock()
	ticks <- time.Now()
	select {
	case observed := <-done:
		if observed.err != nil || observed.result.Outcome != LinearTodoDispatchAttention {
			t.Fatalf("result=%+v err=%v", observed.result, observed.err)
		}
	case <-time.After(time.Second):
		t.Fatal("lease loss did not cancel a blocked driver")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.attention) != 1 || store.attention[0].EventType != OperatorAttentionSchedulerLease || store.attention[0].ReasonCode != "lease_lost" || store.held || store.releasedLease.Version != store.lease.Version {
		t.Fatalf("attention=%+v held=%t released=%+v lease=%+v", store.attention, store.held, store.releasedLease, store.lease)
	}
	if remaining := time.Until(store.releaseDeadline); remaining <= 0 || remaining > 5*time.Second {
		t.Fatalf("lease cleanup deadline remaining=%s", remaining)
	}
}

func TestLinearTodoDispatcherCancellationJoinsRenewerAndReleasesLeaseWithoutRewritingRun(t *testing.T) {
	candidate := dispatchCandidate("operator-stop", "IFAN-37", 1)
	dispatcher, store, _, _, _, driver := newDispatchLab(t, candidate)
	driver.started, driver.allow = make(chan struct{}), make(chan struct{})
	ticks := make(chan time.Time)
	tickerStopped := make(chan struct{})
	dispatcher.leaseTicks = func(time.Duration) (<-chan time.Time, func()) {
		return ticks, func() { close(tickerStopped) }
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := dispatcher.Dispatch(ctx)
		done <- err
	}()
	select {
	case <-driver.started:
	case <-time.After(time.Second):
		t.Fatal("cycle did not reach active driver")
	}
	store.mu.Lock()
	stateBefore := store.run.State
	store.mu.Unlock()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("dispatch cancellation error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("active driver did not stop after cancellation")
	}
	select {
	case <-tickerStopped:
	default:
		t.Fatal("lease renewal ticker was not joined and stopped")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.held || store.releasedLease.Version != store.lease.Version {
		t.Fatalf("held=%t released=%+v lease=%+v", store.held, store.releasedLease, store.lease)
	}
	if remaining := time.Until(store.releaseDeadline); remaining <= 0 || remaining > 5*time.Second {
		t.Fatalf("lease cleanup deadline remaining=%s", remaining)
	}
	if store.run.State != stateBefore || store.run.State != domain.StateExecuting || len(store.retrySchedules) != 0 || len(store.attention) != 0 {
		t.Fatalf("state before=%s after=%s retries=%+v attention=%+v", stateBefore, store.run.State, store.retrySchedules, store.attention)
	}
}

func authorizeDispatchRun(run Run) Run {
	repository, _ := json.Marshal(LocalRepository{CanonicalRepository: run.Repository, ProfileID: "profile-owner-repo", AllowedOperatorLogins: []string{"operator"}})
	run.RepositoryConfigJSON = string(repository)
	run.ProfileID = "profile-owner-repo"
	if run.UpdatedAt.IsZero() {
		run.CreatedAt = time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
		run.UpdatedAt = run.CreatedAt
	}
	return run
}
