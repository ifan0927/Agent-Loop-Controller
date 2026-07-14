package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
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

type dispatchController struct{ store *dispatchStore }

func (c *dispatchController) StartAuthorized(_ context.Context, _ LocalStartInput, _ func(Run) error) (Run, error) {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()
	c.store.continues++
	c.store.run.State = domain.StateExecuting
	return c.store.run, nil
}

func (c *dispatchController) ContinueExpected(_ context.Context, runID string, expected domain.State, key string, _ *Decision) (Run, error) {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()
	if c.store.run.ID != runID || c.store.run.State != expected || c.store.run.IdempotencyKey != key {
		return Run{}, errors.New("unexpected controller continuation")
	}
	c.store.continues++
	c.store.run.State = domain.StateExecuting
	return c.store.run, nil
}

type dispatchDriver struct {
	mu      sync.Mutex
	calls   []ProductionDriveCommand
	err     error
	started chan struct{}
	allow   chan struct{}
}

func (d *dispatchDriver) Drive(ctx context.Context, command ProductionDriveCommand) (ProductionDriveResult, error) {
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
		return ProductionDriveResult{}, d.err
	}
	return ProductionDriveResult{Run: RunResult{RunID: command.RunID}, Action: ProductionStop}, nil
}

type dispatchStore struct {
	RunStore
	mu             sync.Mutex
	now            time.Time
	lease          LinearTodoAdmissionLease
	releasedLease  LinearTodoAdmissionLease
	held           bool
	run            Run
	journal        LinearTodoAdmissionJournal
	journalFound   bool
	reserveCalls   int
	adoptCalls     int
	continues      int
	side           SideEffectRecord
	attention      []OperatorAttentionEvent
	leaseLost      bool
	renewCalls     int
	failRenewAt    int
	renewed        chan int
	postProofDrift bool
	reserveBlocked chan struct{}
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

func (s *dispatchStore) RenewLinearTodoAdmissionLease(_ context.Context, lease LinearTodoAdmissionLease, ttl time.Duration, now time.Time) (LinearTodoAdmissionLease, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.renewCalls++
	if s.failRenewAt == s.renewCalls || !s.held || s.leaseLost || lease.Namespace != s.lease.Namespace || lease.OwnerNonce != s.lease.OwnerNonce || lease.Version != s.lease.Version {
		return LinearTodoAdmissionLease{}, false, nil
	}
	s.lease.Version++
	s.lease.RenewedAt, s.lease.ExpiresAt = now.UTC(), now.UTC().Add(ttl)
	if s.renewed != nil {
		select {
		case s.renewed <- s.renewCalls:
		default:
		}
	}
	return s.lease, true, nil
}

func (s *dispatchStore) ReleaseLinearTodoAdmissionLease(_ context.Context, lease LinearTodoAdmissionLease) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releasedLease = lease
	if !s.held || lease.OwnerNonce != s.lease.OwnerNonce || lease.Version != s.lease.Version {
		return false, nil
	}
	s.held = false
	return true, nil
}

func (s *dispatchStore) LinearTodoAdmissionLeaseHeld(_ context.Context, lease LinearTodoAdmissionLease, _ time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held && !s.leaseLost && lease.OwnerNonce == s.lease.OwnerNonce && lease.Version == s.lease.Version, nil
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
	if !s.held || s.run.ID != "" {
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
	s.run.State = to
	return nil
}

func (s *dispatchStore) AppendOperatorAttention(_ context.Context, event OperatorAttentionEvent) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attention = append(s.attention, event)
	return true, nil
}

func (s *dispatchStore) ListOperatorAttention(context.Context, int) ([]OperatorAttentionEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]OperatorAttentionEvent(nil), s.attention...), nil
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
	policy := LinearTodoDispatchPolicy{CandidateAuthority: dispatchAuthority(), StartAuthority: dispatchStartAuthority(), LeaseTTL: time.Minute, OwnerNonce: "dispatch-owner", Requester: Requester{ID: "operator", Kind: "github_login"}, AttentionProfile: OperatorAttentionProfile{ID: "automation", Name: "linear"}}
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
	return LinearTodoCandidate{IssueID: dispatchUUID(seed), Identifier: identifier, Priority: priority, State: dispatchAuthority().TodoState, Cycle: LinearCycle{ID: dispatchUUID(seed + "-cycle"), Number: 1, StartsAt: created, EndsAt: created.Add(24 * time.Hour), IsActive: true}, Labels: labels, RepositoryLabels: []LinearLabel{labels[1]}, BranchName: "ifan/" + stringsToBranch(identifier), SourceRevision: updated.Format(time.RFC3339Nano), SourceDigest: dispatchDigest(seed + "-source"), CreatedAt: created, UpdatedAt: updated}
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
	if len(reader.calls) != 4 || reader.calls[0] != low.Identifier || reader.calls[1] != high.Identifier || store.journal.Status != "started" || store.continues != 1 || store.renewCalls != 9 || store.releasedLease.Version != store.lease.Version || store.held {
		t.Fatalf("reader=%v journal=%+v continues=%d renews=%d released=%+v current=%+v held=%t", reader.calls, store.journal, store.continues, store.renewCalls, store.releasedLease, store.lease, store.held)
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
		{name: "driver", failAt: 8, scannerCalls: 1, reserveCalls: 1, starterCalls: 1},
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

func TestLinearTodoDispatcherPriorityTieAppendsAttentionWithoutMutation(t *testing.T) {
	first, second := dispatchCandidate("first", "IFAN-11", 1), dispatchCandidate("second", "IFAN-12", 1)
	dispatcher, store, scanner, _, starter, driver := newDispatchLab(t, first, second)

	result, err := dispatcher.Dispatch(context.Background())
	if err != nil || result.Outcome != LinearTodoDispatchAttention || scanner.calls != 1 || store.reserveCalls != 0 || len(starter.calls) != 0 || len(driver.calls) != 0 || len(store.attention) != 1 || store.attention[0].EventType != OperatorAttentionCandidatePriorityTie {
		t.Fatalf("result=%+v reserve=%d starter=%+v driver=%+v attention=%+v err=%v", result, store.reserveCalls, starter.calls, driver.calls, store.attention, err)
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

func TestLinearTodoDispatcherStopsForManualAndDriverConflict(t *testing.T) {
	for _, state := range []domain.State{domain.StateManualIntervention, domain.StateAwaitingHumanDecision, domain.StateAwaitingHumanApproval} {
		t.Run(string(state), func(t *testing.T) {
			candidate := dispatchCandidate("manual", "IFAN-14", 1)
			dispatcher, store, scanner, _, _, driver := newDispatchLab(t, candidate)
			store.run = authorizeDispatchRun(Run{ID: "run-manual", IssueID: candidate.Identifier, IdempotencyKey: "manual-key", Repository: "owner/repo", State: state})
			result, err := dispatcher.Dispatch(context.Background())
			if err != nil || result.Outcome != LinearTodoDispatchAttention || scanner.calls != 0 || len(driver.calls) != 0 || len(store.attention) != 1 {
				t.Fatalf("result=%+v scanner=%d driver=%+v attention=%+v err=%v", result, scanner.calls, driver.calls, store.attention, err)
			}
		})
	}
	t.Run("driver conflict", func(t *testing.T) {
		candidate := dispatchCandidate("conflict", "IFAN-15", 1)
		dispatcher, store, _, _, _, driver := newDispatchLab(t, candidate)
		store.run = authorizeDispatchRun(Run{ID: "run-conflict", IssueID: candidate.Identifier, IdempotencyKey: "conflict-key", Repository: "owner/repo", State: domain.StateExecuting})
		driver.err = serviceError(ErrorConflict, "driver authority changed", nil)
		result, err := dispatcher.Dispatch(context.Background())
		if err != nil || result.Outcome != LinearTodoDispatchAttention || len(driver.calls) != 1 || len(store.attention) != 1 || store.attention[0].ReasonCode != "admission_authority_conflict" {
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
	t.Run("invalid higher priority authority drift does not block valid best", func(t *testing.T) {
		invalid, best := dispatchCandidate("invalid-higher", "IFAN-24", 1), dispatchCandidate("best-lower", "IFAN-25", 2)
		dispatcher, store, _, reader, starter, driver := newDispatchLab(t, invalid, best)
		drifted := reader.sources[invalid.Identifier]
		drifted.BranchName = "ifan/drifted-authority"
		reader.sources[invalid.Identifier] = drifted
		result, err := dispatcher.Dispatch(context.Background())
		if err != nil || result.Outcome != LinearTodoDispatchDriven || store.run.IssueID != best.Identifier || store.reserveCalls != 1 || len(starter.calls) != 1 || starter.calls[0].IssueID != best.IssueID || len(driver.calls) != 1 || len(store.attention) != 0 {
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
	})
}

func TestLinearTodoDispatcherMutationAndPostStartConflictsStopWithoutAnotherCandidate(t *testing.T) {
	t.Run("mutation", func(t *testing.T) {
		first, second := dispatchCandidate("mutation-first", "IFAN-18", 1), dispatchCandidate("mutation-second", "IFAN-19", 2)
		dispatcher, store, _, _, starter, driver := newDispatchLab(t, first, second)
		starter.err = &LinearIssueStartMutationError{Class: "graphql"}
		result, err := dispatcher.Dispatch(context.Background())
		if err != nil || result.Outcome != LinearTodoDispatchAttention || store.reserveCalls != 1 || store.run.IssueID != first.Identifier || len(starter.calls) != 1 || len(driver.calls) != 0 || len(store.attention) != 1 || store.journal.Status != "manual_intervention" {
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
}

func authorizeDispatchRun(run Run) Run {
	repository, _ := json.Marshal(LocalRepository{CanonicalRepository: run.Repository, ProfileID: "profile-owner-repo", AllowedOperatorLogins: []string{"operator"}})
	run.RepositoryConfigJSON = string(repository)
	run.ProfileID = "profile-owner-repo"
	return run
}
