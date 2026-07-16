package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	storeadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
	"github.com/ifan0927/Agent-Loop-Controller/internal/fixtureevidence"
)

func TestOfflineParkedDecisionSurvivesRestartAndAutomaticallyReturnsToDriver(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dbPath := filepath.Join(t.TempDir(), "controller.db")
	repository := offlineAdmissionRepository(t)
	candidate := offlineAdmissionCandidate()
	process := &offlineAdmissionCodex{}

	store, err := storeadapter.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	firstDriver := newOfflineAdmissionDriver()
	firstDriver.release()
	first := parkingDispatcher(t, store, repository, candidate, process, firstDriver, "parking-owner-one")
	firstResult, err := runAdmissionWorker(ctx, true, time.Minute, first.Dispatch, waitAdmissionWorker)
	if err != nil || firstResult.Stopped != "once" || firstResult.Status != workerStatusParked {
		store.Close()
		t.Fatalf("first=%+v err=%v", firstResult, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = storeadapter.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	restartedDriver := newOfflineAdmissionDriver()
	restarted := parkingDispatcher(t, store, repository, candidate, process, restartedDriver, "parking-owner-two")
	parked, err := runAdmissionWorker(ctx, true, time.Minute, restarted.Dispatch, waitAdmissionWorker)
	if err != nil || parked.LastOutcome != application.LinearTodoDispatchAttention || parked.Status != workerStatusParked || len(restartedDriver.commands()) != 0 {
		store.Close()
		t.Fatalf("parked=%+v driver=%+v err=%v", parked, restartedDriver.commands(), err)
	}
	attention, err := store.ListOperatorAttention(ctx, application.OperatorAttentionQueryInput{Limit: 10})
	if err != nil || len(attention) != 1 || attention[0].EventType != application.OperatorAttentionHumanDecision || len(attention[0].AllowedActions) != 1 || attention[0].AllowedActions[0] != application.OperatorAttentionActionDecide {
		store.Close()
		t.Fatalf("attention=%+v err=%v", attention, err)
	}
	eventKey := attention[0].EventKey
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = storeadapter.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	finalDriver := newOfflineAdmissionDriver()
	finalDriver.release()
	finalDispatcher := parkingDispatcher(t, store, repository, candidate, process, finalDriver, "parking-owner-three")
	replayed, err := runAdmissionWorker(ctx, true, time.Minute, finalDispatcher.Dispatch, waitAdmissionWorker)
	if err != nil || replayed.LastOutcome != application.LinearTodoDispatchAttention || replayed.Status != workerStatusParked {
		t.Fatalf("replayed=%+v err=%v", replayed, err)
	}
	attention, err = store.ListOperatorAttention(ctx, application.OperatorAttentionQueryInput{Limit: 10})
	if err != nil || len(attention) != 1 || attention[0].EventKey != eventKey {
		t.Fatalf("replayed attention=%+v err=%v", attention, err)
	}
	lease, acquired, err := store.AcquireLinearTodoAdmissionLease(ctx, "parking-proof", time.Minute, time.Now().UTC())
	if err != nil || !acquired {
		t.Fatalf("parked lease=%+v acquired=%t err=%v", lease, acquired, err)
	}
	_, _ = store.ReleaseLinearTodoAdmissionLease(ctx, lease)

	runs, err := store.ListNonterminalRuns(ctx)
	if err != nil || len(runs) != 1 || runs[0].State != domain.StateAwaitingHumanDecision {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
	if err := store.Transition(ctx, runs[0].ID, domain.StateAwaitingHumanDecision, domain.StateExecuting, "accepted authorized operator decision", "decision-action", ""); err != nil {
		t.Fatal(err)
	}
	updated, err := store.GetRun(ctx, runs[0].ID)
	if err != nil || updated.State != domain.StateExecuting {
		t.Fatalf("continued=%+v err=%v", updated, err)
	}
	resumed, err := runAdmissionWorker(ctx, true, time.Minute, finalDispatcher.Dispatch, waitAdmissionWorker)
	if err != nil || resumed.LastOutcome != application.LinearTodoDispatchDriven || len(finalDriver.commands()) != 1 || finalDriver.commands()[0].RunID != updated.ID {
		t.Fatalf("resumed=%+v driver=%+v err=%v", resumed, finalDriver.commands(), err)
	}
	fixtureevidence.Emit(t, fixtureevidence.Evidence{Scenario: "notification_provenance_safety", RunIDs: []string{updated.ID}, IssueIdentifiers: []string{updated.IssueID}, EventActionKeys: []string{eventKey}, StateSequence: []string{"awaiting_human_decision", "restarted", "executing"}, LeaseEvidence: []string{"parked_lease_released"}, RetryAbandonOutcomes: []string{"notification_deduplicated"}, FinalWorkerState: "stopped"})
}

func parkingDispatcher(t *testing.T, store *storeadapter.Store, repository application.LocalRepository, candidate application.LinearTodoCandidate, process *offlineAdmissionCodex, driver application.LinearTodoDispatchDriver, owner string) *application.LinearTodoDispatcher {
	t.Helper()
	reader := newOfflineAdmissionReader(offlineAdmissionSource(candidate))
	scanner := &offlineAdmissionScanner{scan: application.LinearTodoCandidateScan{Candidates: []application.LinearTodoCandidate{candidate}, Digest: offlineAdmissionDigest("parking-scan"), ObservedAt: candidate.UpdatedAt}}
	controller := application.NewLocalController(store, &offlineAdmissionWorktrees{}, process, offlineAdmissionVerifier{}, offlineAdmissionGit{}, "fixture-codex", repository.WorktreeRoot)
	dispatcher, err := newOfflineAdmissionDispatcher(scanner, reader, &offlineAdmissionStarter{reader: reader}, store, controller, driver, repository, owner)
	if err != nil {
		t.Fatal(err)
	}
	return dispatcher
}
