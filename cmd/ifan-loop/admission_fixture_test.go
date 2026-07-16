package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/codex"
	processadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/process"
	sqlitestore "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/verifier"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

// This fixture is deliberately offline. Two --once workers share the real
// SQLite store and run the production dispatcher path; only Linear transport
// and delivery are deterministic in-memory ports. The blocked driver keeps
// the winning lease alive while the losing worker must stop at attention.
func TestOfflineWorkerOnceConcurrencyHasOneSQLiteAdmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	store, err := sqlitestore.Open(t.TempDir() + "/controller.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	repository := offlineAdmissionRepository(t)
	candidate := offlineAdmissionCandidate()
	reader := newOfflineAdmissionReader(offlineAdmissionSource(candidate))
	scanner := &offlineAdmissionScanner{scan: application.LinearTodoCandidateScan{
		Candidates: []application.LinearTodoCandidate{candidate},
		Digest:     offlineAdmissionDigest("scan"),
		ObservedAt: candidate.UpdatedAt,
	}}
	starter := &offlineAdmissionStarter{reader: reader}
	driver := newOfflineAdmissionDriver()
	defer driver.release()
	worktrees := &offlineAdmissionWorktrees{}
	controller := application.NewLocalController(store, worktrees, &offlineAdmissionCodex{}, offlineAdmissionVerifier{}, offlineAdmissionGit{}, "fixture-codex", repository.WorktreeRoot)

	first, err := newOfflineAdmissionDispatcher(scanner, reader, starter, store, controller, driver, repository, "fixture-owner-one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := newOfflineAdmissionDispatcher(scanner, reader, starter, store, controller, driver, repository, "fixture-owner-two")
	if err != nil {
		t.Fatal(err)
	}

	ready := make(chan struct{}, 2)
	start := make(chan struct{})
	results := make(chan offlineAdmissionObserved, 2)
	for _, dispatcher := range []*application.LinearTodoDispatcher{first, second} {
		go func(dispatcher *application.LinearTodoDispatcher) {
			ready <- struct{}{}
			<-start
			result, runErr := runAdmissionWorker(ctx, true, time.Minute, dispatcher.Dispatch, waitAdmissionWorker)
			results <- offlineAdmissionObserved{result: result, err: runErr}
		}(dispatcher)
	}
	<-ready
	<-ready
	close(start)

	// The driver is reached only after the reservation, durable journal,
	// controller continuation, and Linear start mutation all succeeded.
	var loser offlineAdmissionObserved
	loserObserved := false
waitForDriver:
	for {
		select {
		case <-driver.started:
			break waitForDriver
		case result := <-results:
			if loserObserved || result.err != nil || result.result.LastOutcome != application.LinearTodoDispatchAttention || result.result.Stopped != "once" {
				attention, _ := store.ListOperatorAttention(context.Background(), application.OperatorAttentionQueryInput{Limit: 10})
				reasons := make([]string, 0, len(attention))
				for _, event := range attention {
					reasons = append(reasons, event.EventType+":"+event.ReasonCode)
				}
				t.Fatalf("offline admission worker ended before driver handoff: %+v attention=%v", result, reasons)
			}
			loser, loserObserved = result, true
		case <-ctx.Done():
			t.Fatal("offline admission winner did not reach driver handoff")
		}
	}

	// While the winner is blocked in Drive, the other worker has no route to a
	// scan, reservation, mutation, or second driver handoff.
	if !loserObserved {
		loser = receiveOfflineAdmissionWorker(t, ctx, results)
	}
	if loser.err != nil || loser.result.LastOutcome != application.LinearTodoDispatchAttention || loser.result.Stopped != "once" {
		t.Fatalf("losing worker=%+v", loser)
	}

	driver.release()
	winner := receiveOfflineAdmissionWorker(t, ctx, results)
	if winner.err != nil || winner.result.LastOutcome != application.LinearTodoDispatchDriven || winner.result.Stopped != "once" {
		t.Fatalf("winning worker=%+v", winner)
	}

	commands := driver.commands()
	mutations := starter.mutations()
	if scanner.calls() != 1 || len(mutations) != 1 || len(commands) != 1 {
		t.Fatalf("scan=%d mutations=%+v drive=%+v", scanner.calls(), mutations, commands)
	}
	if mutations[0].IssueID != candidate.IssueID || mutations[0].TargetStateID != offlineAdmissionInProgressState.ID || commands[0].RunID == "" {
		t.Fatalf("mutation=%+v drive=%+v", mutations[0], commands[0])
	}

	run, err := store.GetRun(ctx, commands[0].RunID)
	if err != nil || run.State != domain.StateAwaitingHumanDecision || run.IssueID != candidate.Identifier {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	inspection, err := store.Inspect(ctx, run.ID)
	continuations := 0
	if err == nil {
		for _, transition := range inspection.Timeline {
			if transition.From == domain.StateReceived && transition.To == domain.StateAdmitting {
				continuations++
			}
		}
	}
	if err != nil || continuations != 1 || worktrees.calls() != 1 {
		t.Fatalf("continuations=%d provisions=%d inspection=%+v err=%v", continuations, worktrees.calls(), inspection, err)
	}
	runs, err := store.ListNonterminalRuns(ctx)
	if err != nil || len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatalf("nonterminal runs=%+v err=%v", runs, err)
	}
	journal, found, err := store.GetLinearTodoAdmissionJournal(ctx, run.ID)
	if err != nil || !found || journal.RunID != run.ID || journal.IssueUUID != candidate.IssueID || journal.Status != "started" {
		t.Fatalf("journal=%+v found=%t err=%v", journal, found, err)
	}
	attention, err := store.ListOperatorAttention(ctx, application.OperatorAttentionQueryInput{Limit: 10})
	if err != nil || len(attention) != 1 || attention[0].ReasonCode != "lease_conflict" {
		t.Fatalf("attention=%+v err=%v", attention, err)
	}
}

type offlineAdmissionObserved struct {
	result admissionWorkerResult
	err    error
}

func receiveOfflineAdmissionWorker(t *testing.T, ctx context.Context, results <-chan offlineAdmissionObserved) offlineAdmissionObserved {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-ctx.Done():
		t.Fatal("offline admission workers did not finish")
		return offlineAdmissionObserved{}
	}
}

var (
	offlineAdmissionTodoState       = application.LinearState{ID: "123e4567-e89b-42d3-a456-426614174101", Name: "Todo", Type: "unstarted"}
	offlineAdmissionInProgressState = application.LinearState{ID: "123e4567-e89b-42d3-a456-426614174102", Name: "In Progress", Type: "started"}
)

type offlineAdmissionScanner struct {
	mu   sync.Mutex
	scan application.LinearTodoCandidateScan
	seen int
}

func (s *offlineAdmissionScanner) ListTodoCandidates(context.Context, application.LinearTodoCandidateAuthority) (application.LinearTodoCandidateScan, []application.LinearRequestObservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen++
	return s.scan, nil, nil
}

func (s *offlineAdmissionScanner) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen
}

type offlineAdmissionReader struct {
	mu      sync.Mutex
	source  application.LinearTaskSource
	started bool
}

func newOfflineAdmissionReader(source application.LinearTaskSource) *offlineAdmissionReader {
	return &offlineAdmissionReader{source: source}
}

func (r *offlineAdmissionReader) ReadIssue(_ context.Context, identifier string) (application.LinearTaskSource, []application.LinearRequestObservation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if identifier != r.source.Identifier {
		return application.LinearTaskSource{}, nil, errors.New("offline fixture received an unexpected issue")
	}
	source := r.source
	if r.started {
		source.State = offlineAdmissionInProgressState
		source.UpdatedAt = source.UpdatedAt.Add(time.Second)
		source.SourceRevision = source.UpdatedAt.UTC().Format(time.RFC3339Nano)
		source.ObservedAt = source.UpdatedAt
	}
	return source, nil, nil
}

func (r *offlineAdmissionReader) markStarted(issueID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if issueID != r.source.IssueID || r.started {
		return errors.New("offline fixture start mutation was not unique")
	}
	r.started = true
	return nil
}

type offlineAdmissionStarter struct {
	mu     sync.Mutex
	reader *offlineAdmissionReader
	calls  []application.LinearIssueStartMutation
}

func (s *offlineAdmissionStarter) MoveReservedIssueToStarted(_ context.Context, mutation application.LinearIssueStartMutation) (application.LinearIssueStartMutationResult, []application.LinearRequestObservation, error) {
	if mutation.TargetStateID != offlineAdmissionInProgressState.ID {
		return application.LinearIssueStartMutationResult{}, nil, errors.New("offline fixture received an unexpected start state")
	}
	if err := s.reader.markStarted(mutation.IssueID); err != nil {
		return application.LinearIssueStartMutationResult{}, nil, err
	}
	s.mu.Lock()
	s.calls = append(s.calls, mutation)
	s.mu.Unlock()
	return application.LinearIssueStartMutationResult{IssueID: mutation.IssueID, State: offlineAdmissionInProgressState}, nil, nil
}

func (s *offlineAdmissionStarter) mutations() []application.LinearIssueStartMutation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]application.LinearIssueStartMutation(nil), s.calls...)
}

type offlineAdmissionResolver struct{ repository application.LocalRepository }

func (r offlineAdmissionResolver) ResolveLinearAdmissionRepository(label string) (application.LocalRepository, bool) {
	return r.repository, label == "repo:test"
}

// These fakes are only the process, Git, verifier, and worktree ports that a
// real LocalController owns outside the application. The dispatcher therefore
// exercises its actual persisted continuation path rather than a test-only
// LocalRunController implementation.
type offlineAdmissionWorktrees struct {
	mu         sync.Mutex
	provisions int
}

func (w *offlineAdmissionWorktrees) Provision(_ context.Context, spec application.WorktreeSpec) (application.WorktreeRecord, error) {
	if err := os.Mkdir(spec.Path, 0o700); err != nil {
		return application.WorktreeRecord{}, err
	}
	w.mu.Lock()
	w.provisions++
	w.mu.Unlock()
	return application.WorktreeRecord{SourcePath: spec.SourcePath, OriginPath: spec.OriginPath, Path: spec.Path, Branch: spec.Branch, BaseBranch: spec.BaseBranch, BaseSHA: "offline-base", Nonce: spec.Nonce}, nil
}

func (*offlineAdmissionWorktrees) ValidateOwned(context.Context, application.WorktreeRecord) error {
	return nil
}

func (w *offlineAdmissionWorktrees) calls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.provisions
}

type offlineAdmissionCodex struct{}

func (*offlineAdmissionCodex) Preflight(context.Context, string, string) (codex.PreflightEvidence, error) {
	return codex.PreflightEvidence{Version: "offline-fixture"}, nil
}

func (*offlineAdmissionCodex) Implementation(_ context.Context, _ codex.CommandSpec, artifacts string) (codex.StructuredResult[domain.AgentOutcome], error) {
	outcome := domain.AgentOutcome{Status: domain.AgentNeedsHumanDecision, Summary: "offline continuation boundary", DecisionRequest: &domain.DecisionRequest{Question: "Choose the fixture boundary", Context: "The deterministic fixture stops before delivery.", Options: []domain.DecisionOption{{ID: "stop", Description: "Stop after continuation"}, {ID: "continue", Description: "Continue delivery"}}, Recommendation: "stop", BlockingReason: "Fixture boundary requires a human choice"}}
	if err := outcome.Validate(); err != nil {
		return codex.StructuredResult[domain.AgentOutcome]{}, err
	}
	data, err := json.Marshal(outcome)
	if err != nil {
		return codex.StructuredResult[domain.AgentOutcome]{}, err
	}
	stdout, stderr := filepath.Join(artifacts, "implementation.stdout.jsonl"), filepath.Join(artifacts, "implementation.stderr.txt")
	if err := os.WriteFile(filepath.Join(artifacts, "implementation-outcome.json"), data, 0o600); err != nil {
		return codex.StructuredResult[domain.AgentOutcome]{}, err
	}
	if err := os.WriteFile(stdout, []byte(`{"type":"thread.started","thread_id":"offline-session"}\n`), 0o600); err != nil {
		return codex.StructuredResult[domain.AgentOutcome]{}, err
	}
	if err := os.WriteFile(stderr, nil, 0o600); err != nil {
		return codex.StructuredResult[domain.AgentOutcome]{}, err
	}
	return codex.StructuredResult[domain.AgentOutcome]{SessionID: "offline-session", Outcome: outcome, Process: processadapter.Result{Outcome: processadapter.OutcomeExited, ExitCode: 0, StdoutPath: stdout, StderrPath: stderr}}, nil
}

func (*offlineAdmissionCodex) Resume(context.Context, codex.CommandSpec, string) (codex.StructuredResult[domain.AgentOutcome], error) {
	return codex.StructuredResult[domain.AgentOutcome]{}, errors.New("offline fixture must not resume")
}

func (*offlineAdmissionCodex) Review(context.Context, codex.CommandSpec, string) (codex.StructuredResult[domain.ReviewOutcome], error) {
	return codex.StructuredResult[domain.ReviewOutcome]{}, errors.New("offline fixture must not review")
}

type offlineAdmissionVerifier struct{}

func (offlineAdmissionVerifier) Run(context.Context, []string, string, string, string) (verifier.Evidence, error) {
	return verifier.Evidence{}, errors.New("offline fixture must not verify")
}

type offlineAdmissionGit struct{}

func (offlineAdmissionGit) Head(context.Context, string) (string, error) {
	return "", errors.New("offline fixture must not read Git")
}

func (offlineAdmissionGit) Branch(context.Context, string) (string, error) {
	return "", errors.New("offline fixture must not read Git")
}

func (offlineAdmissionGit) Status(context.Context, string) (string, error) {
	return "", errors.New("offline fixture must not read Git")
}

func (offlineAdmissionGit) ValidateRemoteBase(context.Context, string, string, string) error {
	return errors.New("offline fixture must not validate Git")
}

func (offlineAdmissionGit) CommitCandidate(context.Context, string, string) (string, error) {
	return "", errors.New("offline fixture must not commit")
}

func (offlineAdmissionGit) CommitMetadata(context.Context, string, string) (string, string, error) {
	return "", "", errors.New("offline fixture must not read Git metadata")
}

type offlineAdmissionDriver struct {
	mu        sync.Mutex
	calls     []application.ProductionDriveCommand
	started   chan struct{}
	allow     chan struct{}
	startOnce sync.Once
	allowOnce sync.Once
}

func newOfflineAdmissionDriver() *offlineAdmissionDriver {
	return &offlineAdmissionDriver{started: make(chan struct{}), allow: make(chan struct{})}
}

func (d *offlineAdmissionDriver) Drive(ctx context.Context, command application.ProductionDriveCommand) (application.ProductionDriveResult, error) {
	d.mu.Lock()
	d.calls = append(d.calls, command)
	d.mu.Unlock()
	d.startOnce.Do(func() { close(d.started) })
	select {
	case <-d.allow:
	case <-ctx.Done():
		return application.ProductionDriveResult{}, ctx.Err()
	}
	return application.ProductionDriveResult{Run: application.RunResult{RunID: command.RunID}, Action: application.ProductionStop}, nil
}

func (d *offlineAdmissionDriver) release() {
	d.allowOnce.Do(func() { close(d.allow) })
}

func (d *offlineAdmissionDriver) commands() []application.ProductionDriveCommand {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]application.ProductionDriveCommand(nil), d.calls...)
}

func newOfflineAdmissionDispatcher(scanner application.LinearTodoCandidateScanner, reader application.LinearIssueReader, starter application.LinearReservedIssueStarter, store *sqlitestore.Store, controller application.LocalRunController, driver application.LinearTodoDispatchDriver, repository application.LocalRepository, owner string) (*application.LinearTodoDispatcher, error) {
	return application.NewLinearTodoDispatcher(scanner, reader, offlineAdmissionResolver{repository: repository}, starter, store, controller, driver, application.LinearTodoDispatchPolicy{
		CandidateAuthority: application.LinearTodoCandidateAuthority{TeamID: "123e4567-e89b-42d3-a456-426614174100", TeamKey: "IFAN", TodoState: offlineAdmissionTodoState, InProgressState: offlineAdmissionInProgressState, MaxCandidates: 10, MaxPages: 1},
		StartAuthority:     application.LinearIssueStartAuthority{TeamID: "123e4567-e89b-42d3-a456-426614174100", TeamKey: "IFAN", TodoState: offlineAdmissionTodoState, InProgressState: offlineAdmissionInProgressState},
		LeaseTTL:           time.Minute,
		OwnerNonce:         owner,
		Requester:          application.Requester{ID: "operator", Kind: "github_login"},
		AttentionProfile:   application.OperatorAttentionProfile{ID: "offline", Name: "offline-fixture"},
	})
}

func offlineAdmissionRepository(t *testing.T) application.LocalRepository {
	t.Helper()
	return application.LocalRepository{CanonicalRepository: "owner/repo", OriginPath: offlineAdmissionTempDir(t), SourcePath: offlineAdmissionTempDir(t), BaseBranch: "main", RunRoot: offlineAdmissionTempDir(t), WorktreeRoot: offlineAdmissionTempDir(t), ProfileID: "profile-owner-repo", ProfileSnapshotVersion: 1, ProfileDigest: offlineAdmissionDigest("profile"), ProfileSnapshotJSON: `{}`, RegistryVersion: 1, RegistryDigest: offlineAdmissionDigest("registry"), RepositoryBindingDigest: offlineAdmissionDigest("binding"), VerifierIDs: []string{"go-test"}, AllowedOperatorLogins: []string{"operator"}}
}

func offlineAdmissionTempDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func offlineAdmissionCandidate() application.LinearTodoCandidate {
	created := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	updated := created.Add(time.Hour)
	labels := []application.LinearLabel{{ID: "123e4567-e89b-42d3-a456-426614174105", Name: "agent:codex"}, {ID: "123e4567-e89b-42d3-a456-426614174106", Name: "repo:test"}}
	return application.LinearTodoCandidate{IssueID: "123e4567-e89b-42d3-a456-426614174103", Identifier: "IFAN-41", Priority: 1, State: offlineAdmissionTodoState, Cycle: application.LinearCycle{ID: "123e4567-e89b-42d3-a456-426614174104", Number: 1, StartsAt: created, EndsAt: created.Add(24 * time.Hour), IsActive: true}, Labels: labels, RepositoryLabels: []application.LinearLabel{labels[1]}, BranchName: "ifan/ifan-41-admission-fixtures", SourceRevision: updated.Format(time.RFC3339Nano), SourceDigest: offlineAdmissionDigest("source"), CreatedAt: created, UpdatedAt: updated}
}

func offlineAdmissionSource(candidate application.LinearTodoCandidate) application.LinearTaskSource {
	return application.LinearTaskSource{Provider: "linear", IssueID: candidate.IssueID, Identifier: candidate.Identifier, URL: "https://linear.invalid/" + candidate.Identifier, Title: "Offline fixture", Description: "## Outcome\n\nDeliver one task.\n\n## Acceptance Criteria\n\n- Keep state durable.\n\n## Out of Scope\n\n- Extra work.", Team: application.LinearTeam{ID: "123e4567-e89b-42d3-a456-426614174100", Key: "IFAN", Name: "I-Fan"}, State: candidate.State, Labels: append([]application.LinearLabel(nil), candidate.Labels...), Cycle: candidate.Cycle, BranchName: candidate.BranchName, SourceRevision: candidate.SourceRevision, CreatedAt: candidate.CreatedAt, UpdatedAt: candidate.UpdatedAt, ObservedAt: candidate.UpdatedAt}
}

func offlineAdmissionDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum)
}
