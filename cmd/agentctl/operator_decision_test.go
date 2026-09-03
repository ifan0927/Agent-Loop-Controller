package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/codex"
	processadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/process"
	sqlitestore "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestProductionOperatorDecisionJourneyStopsTUIBeforeWorkerResume(t *testing.T) {
	root := resolvedTempDir(t)
	configPath, databasePath := writeCurrentManagedDraftConfig(t, root)
	loaded, err := loadManagedConfiguration(configPath)
	if err != nil {
		t.Fatal(err)
	}
	bindings := loaded.Registry.Bindings()
	if len(bindings) != 1 {
		t.Fatalf("bindings=%d", len(bindings))
	}
	repository := localRepository(bindings[0])
	store, err := sqlitestore.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	authority := testExistingNewAdmissionGate(t, store).Decision.Authority
	now := time.Date(2026, 9, 3, 4, 0, 0, 0, time.UTC)
	task := domain.CodingTask{RunID: "operator-decision-run", IssueID: "IFAN-206", Title: "Decision fixture", Description: "Exercise the local operator decision boundary.", Repository: repository.CanonicalRepository, BaseBranch: repository.BaseBranch, WorkingBranch: "ifan/operator-decision", Goal: "Accept one bounded decision.", AcceptanceCriteria: []string{"Persist the selected option."}, VerifierIDs: append([]string(nil), repository.VerifierIDs...), Policy: domain.TaskPolicy{HumanApprovalRequired: true, MergeMethod: "squash", MaxRepairAttempts: 1}, SourceRevision: "fixture-v1", CreatedAt: now}
	normalized, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"number":206}`)
	rawDigest, taskDigest := sha256.Sum256(raw), sha256.Sum256(normalized)
	process := &decisionHandoffCodex{}
	controller := application.NewLocalController(store, &offlineAdmissionWorktrees{}, process, offlineAdmissionVerifier{}, offlineAdmissionGit{}, "fixture-codex", repository.WorktreeRoot)
	run, err := controller.Start(context.Background(), application.LocalStartInput{Task: task, RawIssueJSON: raw, RawIssueHash: hex.EncodeToString(rawDigest[:]), NormalizedJSON: normalized, TaskHash: hex.EncodeToString(taskDigest[:]), IdempotencyKey: "operator-decision-key", Repository: repository, RunRoot: repository.RunRoot, WorktreeRoot: repository.WorktreeRoot, ConfigurationAuthority: authority})
	if err != nil || run.State != domain.StateAwaitingHumanDecision {
		store.Close()
		t.Fatalf("run=%+v err=%v", run, err)
	}
	inspection, err := store.Inspect(context.Background(), run.ID)
	if err != nil || len(inspection.Timeline) == 0 || len(inspection.Attempts) != 1 {
		store.Close()
		t.Fatalf("inspection=%+v err=%v", inspection, err)
	}
	event, err := application.HumanDecisionAttentionEvent(run, inspection.Timeline[len(inspection.Timeline)-1])
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if _, err := store.AppendOperatorAttention(context.Background(), event); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	composition, err := composeOperator(context.Background(), configPath)
	if err != nil {
		t.Fatal(err)
	}
	defer composition.Close()
	model := newOperatorModel(context.Background(), composition.loader)
	model.width, model.height = 80, 24
	model.resizeDecisionEditor()
	updated, attentionLoad := model.Update(keyMessage('3'))
	model = updated.(operatorModel)
	if attentionLoad == nil || model.route != operatorAttentionRoute {
		t.Fatalf("TUI did not open Attention: route=%s", model.route)
	}
	updated, _ = model.Update(attentionLoad())
	model = updated.(operatorModel)
	if model.attention.page == nil || len(model.attention.page.Items) != 1 || model.attention.page.Items[0].RunID != run.ID || model.attention.page.Items[0].Navigation != application.RoutineAttentionNavigationRunDetail {
		t.Fatalf("TUI Attention did not project the decision run: %+v", model.attention.page)
	}
	updated, detailLoad := model.Update(keySpecial(tea.KeyEnter, 0))
	model = updated.(operatorModel)
	if detailLoad == nil || model.route != operatorRunDetailRoute || model.detail.runID != run.ID || model.detail.returnRoute != operatorAttentionRoute {
		t.Fatalf("TUI did not navigate from Attention to Run detail: route=%s detail=%+v", model.route, model.detail)
	}
	updated, _ = model.Update(detailLoad())
	model = updated.(operatorModel)
	if model.detail.detail == nil || model.detail.detail.Decision == nil || len(model.detail.detail.Offers) != 1 || model.detail.detail.Offers[0].Action != application.OperationDecide {
		t.Fatalf("detail=%+v", model.detail.detail)
	}
	detail := *model.detail.detail
	for _, inputKey := range []tea.KeyPressMsg{keyMessage('d'), keySpecial(tea.KeyEnter, 0), keySpecial(tea.KeyEnter, 0), keySpecial('s', tea.ModCtrl), keySpecial(tea.KeyEnter, 0)} {
		updated, _ = model.Update(inputKey)
		model = updated.(operatorModel)
	}
	updated, submit := model.Update(keySpecial(tea.KeyEnter, 0))
	model = updated.(operatorModel)
	if submit == nil || model.decision.stage != operatorDecisionPending {
		t.Fatalf("TUI did not submit the decision: state=%+v", model.decision)
	}
	updated, refresh := model.Update(submit())
	model = updated.(operatorModel)
	if refresh == nil || model.decision.receipt == nil {
		t.Fatalf("TUI did not retain the decision receipt: state=%+v", model.decision)
	}
	receipt := *model.decision.receipt
	input := model.decision.payload
	if receipt.OperationType != application.OperationDecide || receipt.Phase != application.OperationPhaseObserved || receipt.Outcome != application.OperationOutcomeSucceeded {
		t.Fatalf("receipt=%+v", receipt)
	}
	accepted, err := composition.store.Inspect(context.Background(), run.ID)
	if err != nil || accepted.Run.State != domain.StateExecuting || len(accepted.Attempts) != 1 {
		t.Fatalf("decision acceptance resumed worker: run=%+v attempts=%d err=%v", accepted.Run, len(accepted.Attempts), err)
	}
	if process.resumeCalls != 0 {
		t.Fatalf("TUI decision acceptance resumed Codex %d times", process.resumeCalls)
	}
	if journal, found, journalErr := composition.store.GetLinearTodoAdmissionJournal(context.Background(), run.ID); journalErr != nil || found {
		t.Fatalf("decision fixture unexpectedly had an admission journal: journal=%+v found=%t err=%v", journal, found, journalErr)
	}
	candidate := offlineAdmissionCandidate()
	reader := newOfflineAdmissionReader(offlineAdmissionSource(candidate))
	scanner := &offlineAdmissionScanner{scan: application.LinearTodoCandidateScan{Candidates: []application.LinearTodoCandidate{candidate}, Digest: offlineAdmissionDigest("decision-handoff-scan"), ObservedAt: candidate.UpdatedAt}}
	workerController := application.NewLocalController(composition.store, &offlineAdmissionWorktrees{}, process, offlineAdmissionVerifier{}, offlineAdmissionGit{}, "fixture-codex", repository.WorktreeRoot)
	workerDriver := &decisionHandoffWorkerDriver{controller: workerController, store: composition.store, owner: "decision-handoff-worker"}
	production := composition.loader.(*productionOperatorLoader)
	if err := composition.store.BindWorkerSupervisor("decision-handoff-worker"); err != nil {
		t.Fatal(err)
	}
	dispatcher, err := newOfflineAdmissionDispatcherWithRequester(scanner, reader, &offlineAdmissionStarter{reader: reader}, composition.store, workerController, workerDriver, offlineAdmissionResolver{repository: repository}, "decision-handoff-worker", production.requester)
	if err != nil {
		t.Fatal(err)
	}
	workerResult, err := runAdmissionWorker(context.Background(), true, time.Minute, dispatcher.Dispatch, waitAdmissionWorker)
	if err != nil || workerResult.LastOutcome != application.LinearTodoDispatchDriven || process.resumeCalls != 1 || workerDriver.calls != 1 {
		attention, _ := composition.store.ListOperatorAttention(context.Background(), application.OperatorAttentionQueryInput{Limit: 10})
		t.Fatalf("worker=%+v driver_calls=%d resume_calls=%d attention=%+v err=%v", workerResult, workerDriver.calls, process.resumeCalls, attention, err)
	}
	observed, err := composition.loader.LoadRunDetail(context.Background(), run.ID, now.Add(2*time.Minute))
	if err != nil || observed.DecisionHandoff == nil || observed.DecisionHandoff.Status != application.RoutineDecisionWorkerResumed {
		t.Fatalf("worker handoff detail=%+v err=%v", observed.DecisionHandoff, err)
	}
	model.detail.detail = &observed
	if output := ansi.Strip(model.render()); !strings.Contains(output, "Decision OBSERVED/SUCCEEDED") || !strings.Contains(output, "Worker resume observed from durable attempt evidence") {
		t.Fatalf("TUI did not retain the receipt and worker result:\n%s", output)
	}
	replayed, err := composition.loader.AcceptDecision(context.Background(), detail.Offers[0].OfferID, input)
	if err != nil || replayed.OperationID != receipt.OperationID {
		t.Fatalf("replay=%+v receipt=%+v err=%v", replayed, receipt, err)
	}
	production.requester.DatabaseID++
	if _, err := composition.loader.AcceptDecision(context.Background(), detail.Offers[0].OfferID, input); err == nil {
		t.Fatal("long-lived operator retained changed decision authority")
	} else {
		var safe *application.ServiceError
		if !errors.As(err, &safe) || safe.Category != application.ErrorConflict {
			t.Fatalf("changed authority error=%v", err)
		}
	}
}

type decisionHandoffCodex struct {
	offlineAdmissionCodex
	resumeCalls int
}

func (c *decisionHandoffCodex) Resume(_ context.Context, _ codex.CommandSpec, artifacts string) (codex.StructuredResult[domain.AgentOutcome], error) {
	c.resumeCalls++
	outcome := domain.AgentOutcome{Status: domain.AgentBlocked, Summary: "The worker resumed the persisted session exactly once."}
	data, err := json.Marshal(outcome)
	if err != nil {
		return codex.StructuredResult[domain.AgentOutcome]{}, err
	}
	stdout := filepath.Join(artifacts, "implementation.stdout.jsonl")
	stderr := filepath.Join(artifacts, "implementation.stderr.txt")
	if err := os.WriteFile(filepath.Join(artifacts, "implementation-outcome.json"), data, 0o600); err != nil {
		return codex.StructuredResult[domain.AgentOutcome]{}, err
	}
	if err := os.WriteFile(stdout, []byte(`{"type":"turn.completed"}`+"\n"), 0o600); err != nil {
		return codex.StructuredResult[domain.AgentOutcome]{}, err
	}
	if err := os.WriteFile(stderr, nil, 0o600); err != nil {
		return codex.StructuredResult[domain.AgentOutcome]{}, err
	}
	return codex.StructuredResult[domain.AgentOutcome]{SessionID: "offline-session", Outcome: outcome, Process: processadapter.Result{Outcome: processadapter.OutcomeExited, ExitCode: 0, StdoutPath: stdout, StderrPath: stderr}}, nil
}

type decisionHandoffWorkerDriver struct {
	controller application.LocalRunController
	store      *sqlitestore.Store
	owner      string
	calls      int
}

func (d *decisionHandoffWorkerDriver) Drive(ctx context.Context, command application.ProductionDriveCommand) (application.ProductionDriveResult, error) {
	d.calls++
	result, err := application.NewCommandService(d.controller, d.store).Continue(application.WithHeavyPermitOwner(ctx, d.owner), application.ContinueCommand{Requester: command.Requester, RunID: command.RunID, Repository: command.Repository, ExpectedState: domain.StateExecuting, IdempotencyKey: command.IdempotencyKey})
	return application.ProductionDriveResult{Run: result.Run, Action: application.ProductionStop}, err
}

func TestOperatorDecisionFlowNormalizesReviewsConfirmsRetriesAndObservesWorker(t *testing.T) {
	model := decisionOperatorModel()
	plain := ansi.Strip(model.render())
	for _, phrase := range []string{"UNTRUSTED HUMAN DECISION · 2 persisted options", "Question: Choose a release boundary?", "open Attention and press Enter", "d decide"} {
		if !strings.Contains(plain, phrase) {
			t.Fatalf("decision detail missing %q:\n%s", phrase, plain)
		}
	}
	updated, _ := model.Update(keySpecial(tea.KeyTab, 0))
	model = updated.(operatorModel)
	contextViews := ansi.Strip(model.render())
	for range 12 {
		updated, _ = model.Update(keyMessage('j'))
		model = updated.(operatorModel)
		contextViews += "\n" + ansi.Strip(model.render())
	}
	for _, phrase := range []string{"Context:", "Blocking reason:", "Recommendation option ID", "Option \"inclusive\"", "Option \"exclusive\""} {
		if !strings.Contains(contextViews, phrase) {
			t.Fatalf("scrollable decision context missing %q:\n%s", phrase, contextViews)
		}
	}
	if lipgloss.Width(model.render()) > 80 || lipgloss.Height(model.render()) > 24 {
		t.Fatalf("decision detail exceeded 80x24: %dx%d", lipgloss.Width(model.render()), lipgloss.Height(model.render()))
	}

	oldDetailGeneration := model.detail.generation
	updated, command := model.Update(keyMessage('d'))
	model = updated.(operatorModel)
	if command != nil || model.decision.stage != operatorDecisionSelecting || model.detail.generation == oldDetailGeneration {
		t.Fatalf("selection state=%+v detail_generation=%d", model.decision, model.detail.generation)
	}
	if lipgloss.Width(model.render()) > 80 || lipgloss.Height(model.render()) > 24 {
		t.Fatalf("decision selection exceeded 80x24: %dx%d", lipgloss.Width(model.render()), lipgloss.Height(model.render()))
	}
	late := decisionFixtureRunDetail()
	late.Run.State = domain.StateFailed
	updated, _ = model.Update(operatorRunDetailResultMsg{generation: oldDetailGeneration, runID: model.detail.runID, detail: late})
	model = updated.(operatorModel)
	if model.detail.detail.Run.State != domain.StateAwaitingHumanDecision {
		t.Fatal("late detail refresh replaced an active decision flow")
	}

	updated, _ = model.Update(keySpecial(tea.KeyEnter, 0))
	model = updated.(operatorModel)
	updated, _ = model.Update(keyMessage('j'))
	model = updated.(operatorModel)
	updated, command = model.Update(keySpecial(tea.KeyEnter, 0))
	model = updated.(operatorModel)
	if command == nil || model.decision.stage != operatorDecisionEditing || model.decision.payload.ChoiceID != "exclusive" {
		t.Fatalf("editor state=%+v command=%v", model.decision, command)
	}
	if lipgloss.Width(model.render()) > 80 || lipgloss.Height(model.render()) > 24 {
		t.Fatalf("decision editor exceeded 80x24: %dx%d", lipgloss.Width(model.render()), lipgloss.Height(model.render()))
	}
	model.decision.editor.SetValue("  \n \t ")
	updated, _ = model.Update(keySpecial('s', tea.ModCtrl))
	model = updated.(operatorModel)
	if model.decision.stage != operatorDecisionReviewing || model.decision.payload.Instructions != operatorNeutralDecisionInstructions {
		t.Fatalf("normalized payload=%+v", model.decision.payload)
	}
	if review := ansi.Strip(model.render()); !strings.Contains(review, operatorDecisionLiteral(operatorNeutralDecisionInstructions)) || !strings.Contains(review, "Selected option ID (exact JSON string):") || !strings.Contains(review, operatorDecisionLiteral("exclusive")) {
		t.Fatalf("review did not show exact effective input:\n%s", review)
	}
	updated, _ = model.Update(keySpecial(tea.KeyEnter, 0))
	model = updated.(operatorModel)
	if model.decision.stage != operatorDecisionConfirming || !strings.Contains(ansi.Strip(model.render()), "CONFIRM HUMAN DECISION") {
		t.Fatalf("confirmation state=%+v", model.decision)
	}
	updated, submit := model.Update(keySpecial(tea.KeyEnter, 0))
	model = updated.(operatorModel)
	if submit == nil || model.decision.stage != operatorDecisionPending {
		t.Fatalf("pending state=%+v command=%v", model.decision, submit)
	}
	generation := model.decision.operationGeneration
	updated, duplicate := model.Update(keySpecial(tea.KeyEnter, 0))
	model = updated.(operatorModel)
	if duplicate != nil || model.decision.operationGeneration != generation {
		t.Fatal("duplicate decision submission was not fenced")
	}
	payload, offerID := model.decision.payload, model.decision.offerID
	updated, _ = model.Update(operatorDecisionResultMsg{generation: generation, runID: model.detail.runID, offerID: offerID, payload: payload, err: operatorSafeError{Category: application.ErrorUnavailable, Message: "response unavailable"}})
	model = updated.(operatorModel)
	if model.decision.stage != operatorDecisionRetryable || !strings.Contains(ansi.Strip(model.render()), "Decision outcome uncertain") {
		t.Fatalf("retryable state=%+v", model.decision)
	}
	updated, retry := model.Update(keySpecial(tea.KeyEnter, 0))
	model = updated.(operatorModel)
	if retry == nil || model.decision.stage != operatorDecisionPending || model.decision.payload != payload || model.decision.offerID != offerID || model.decision.operationGeneration != generation+1 {
		t.Fatalf("retry changed exact request: %+v", model.decision)
	}
	updated, _ = model.Update(operatorDecisionResultMsg{generation: generation, runID: model.detail.runID, offerID: offerID, payload: payload, receipt: application.OperationReceipt{Outcome: application.OperationOutcomeSucceeded}})
	model = updated.(operatorModel)
	if model.decision.stage != operatorDecisionPending {
		t.Fatal("late mutation result replaced the current retry")
	}
	receipt := application.OperationReceipt{OperationType: application.OperationDecide, Phase: application.OperationPhaseObserved, Outcome: application.OperationOutcomeSucceeded, ResultingState: string(domain.StateExecuting)}
	updated, refresh := model.Update(operatorDecisionResultMsg{generation: generation + 1, runID: model.detail.runID, offerID: offerID, payload: payload, receipt: receipt})
	model = updated.(operatorModel)
	if refresh == nil || model.decision.stage != operatorDecisionSucceeded || model.decision.receipt == nil || !model.detail.refreshing {
		t.Fatalf("success state=%+v detail=%+v", model.decision, model.detail)
	}
	updated, _ = model.Update(operatorRunDetailResultMsg{generation: model.detail.generation, runID: model.detail.runID, err: operatorSafeError{Category: application.ErrorUnavailable, Message: "refresh unavailable"}})
	model = updated.(operatorModel)
	if model.detail.staleError == nil || model.decision.receipt == nil || !strings.Contains(ansi.Strip(model.render()), "Decision OBSERVED/SUCCEEDED") {
		t.Fatalf("post-success stale state decision=%+v detail=%+v", model.decision, model.detail)
	}
	updated, refresh = model.Update(keyMessage('r'))
	model = updated.(operatorModel)
	if refresh == nil {
		t.Fatal("post-success detail refresh was unavailable")
	}
	resumed := decisionFixtureRunDetail()
	resumed.Run.State = domain.StateVerifying
	resumed.Decision = nil
	observedAt := resumed.Metadata.ObservedAt.Add(-time.Second)
	resumed.DecisionHandoff = &application.RoutineDecisionHandoff{Status: application.RoutineDecisionWorkerResumed, AcceptedAt: observedAt.Add(-time.Second), ResumeObservedAt: &observedAt}
	updated, _ = model.Update(operatorRunDetailResultMsg{generation: model.detail.generation, runID: model.detail.runID, detail: resumed})
	model = updated.(operatorModel)
	if output := ansi.Strip(model.render()); !strings.Contains(output, "Worker resume observed from durable attempt evidence") || !strings.Contains(output, "Decision OBSERVED/SUCCEEDED") {
		t.Fatalf("resumed decision result not observable:\n%s", output)
	}

	next := decisionFixtureRunDetail()
	next.Offers[0].OfferID = "opaque-second-decision-offer"
	model.detail.detail = &next
	if !model.decisionCanStart() || !strings.Contains(ansi.Strip(model.render()), "d decide") {
		t.Fatal("a successful prior decision blocked a new offer for the same run")
	}
	updated, _ = model.Update(keyMessage('d'))
	model = updated.(operatorModel)
	if model.decision.stage != operatorDecisionSelecting || model.decision.offerID != next.Offers[0].OfferID || model.decision.receipt != nil {
		t.Fatalf("new same-run decision did not replace the settled flow: %+v", model.decision)
	}
}

func TestOperatorAttentionEnterOpensDecisionOptionsDirectly(t *testing.T) {
	model := loadedOperatorModel(80, 24)
	now := time.Date(2026, 9, 3, 5, 0, 0, 0, time.UTC)
	model.route = operatorAttentionRoute
	model.attention.page = &application.RoutineAttentionPage{
		Metadata:   application.RoutineProjectionMetadata{SchemaVersion: application.RoutineAttentionSchemaVersion, ObservedAt: now},
		Collection: application.RoutineCollectionMetadata{Total: 1},
		Items: []application.RoutineAttentionItem{{
			EventID: "attention-decision", EventType: application.OperatorAttentionHumanDecision,
			Scope: application.ScopeRun, TargetID: "run-decision", RunID: "run-decision",
			LinearIdentifier: "IFAN-206", Repository: "owner/repo",
			ControllerState: string(domain.StateAwaitingHumanDecision), AttentionState: application.RoutineAttentionActive,
			Severity: "warning", ReasonCode: "human_decision_required", OccurredAt: now.Add(-time.Minute), ObservedAt: now,
			Offers:     []application.RoutineAttentionOfferSummary{{OfferID: "opaque-decision-offer", Action: application.OperationDecide, InputKind: application.LegalActionInputDecision}},
			Navigation: application.RoutineAttentionNavigationRunDetail,
		}},
	}

	updated, load := model.Update(keySpecial(tea.KeyEnter, 0))
	model = updated.(operatorModel)
	if load == nil || model.route != operatorRunDetailRoute || !model.detail.startDecisionOnLoad {
		t.Fatalf("Attention did not start direct decision load: route=%q detail=%+v", model.route, model.detail)
	}
	detail := decisionFixtureRunDetail()
	updated, _ = model.Update(operatorRunDetailResultMsg{generation: model.detail.generation, runID: model.detail.runID, detail: detail})
	model = updated.(operatorModel)
	if model.decision.stage != operatorDecisionSelecting || model.detail.startDecisionOnLoad || model.decision.requestFocus != operatorDecisionRequestOptions {
		t.Fatalf("loaded Attention decision did not enter selection: %+v", model.decision)
	}
	if output := ansi.Strip(model.render()); !strings.Contains(output, "Attention / Human decision") || !strings.Contains(output, "Persisted options") {
		t.Fatalf("direct decision screen did not use the shared TUI flow:\n%s", output)
	}
	updated, _ = model.Update(keyMessage('j'))
	model = updated.(operatorModel)
	updated, command := model.Update(keySpecial(tea.KeyEnter, 0))
	model = updated.(operatorModel)
	if command == nil || model.decision.stage != operatorDecisionEditing || model.decision.payload.ChoiceID != "exclusive" {
		t.Fatalf("Attention option navigation failed: state=%+v command=%v", model.decision, command)
	}
}

func TestOperatorAttentionDecisionLoadIsInvalidatedWhenRouteChanges(t *testing.T) {
	model := loadedOperatorModel(80, 24)
	now := time.Date(2026, 9, 3, 5, 15, 0, 0, time.UTC)
	model.route = operatorAttentionRoute
	model.attention.page = &application.RoutineAttentionPage{Items: []application.RoutineAttentionItem{{
		EventID: "attention-decision", RunID: "run-decision", Navigation: application.RoutineAttentionNavigationRunDetail,
		Offers: []application.RoutineAttentionOfferSummary{{Action: application.OperationDecide, InputKind: application.LegalActionInputDecision}},
	}}, Metadata: application.RoutineProjectionMetadata{ObservedAt: now}}
	updated, command := model.Update(keySpecial(tea.KeyEnter, 0))
	model = updated.(operatorModel)
	if command == nil || !model.detail.startDecisionOnLoad || !model.detail.refreshing {
		t.Fatalf("decision load did not start: %+v", model.detail)
	}
	staleGeneration := model.detail.generation
	updated, _ = model.Update(keyMessage('1'))
	model = updated.(operatorModel)
	if model.route != operatorOverviewRoute || model.detail.startDecisionOnLoad || model.detail.refreshing || model.detail.generation == staleGeneration {
		t.Fatalf("route change did not invalidate direct decision load: route=%s detail=%+v", model.route, model.detail)
	}
	updated, _ = model.Update(operatorRunDetailResultMsg{generation: staleGeneration, runID: "run-decision", detail: decisionFixtureRunDetail()})
	model = updated.(operatorModel)
	if model.decision.stage != operatorDecisionIdle || model.detail.detail != nil {
		t.Fatalf("stale detail result created a background decision: decision=%+v detail=%+v", model.decision, model.detail)
	}
}

func TestOperatorDecisionDoesNotStartFromRefreshingOrStaleCachedDetail(t *testing.T) {
	model := decisionOperatorModel()
	opened, refresh := model.openRunDetail(model.detail.runID, operatorAttentionRoute)
	model = opened.(operatorModel)
	if refresh == nil || !model.detail.refreshing || model.decisionCanStart() {
		t.Fatalf("cached detail remained actionable during refresh: %+v", model.detail)
	}
	generation := model.detail.generation
	updated, command := model.Update(keyMessage('d'))
	model = updated.(operatorModel)
	if command != nil || model.decision.stage != operatorDecisionIdle || model.detail.generation != generation {
		t.Fatalf("refreshing cached detail started decision: decision=%+v detail=%+v", model.decision, model.detail)
	}
	updated, _ = model.Update(operatorRunDetailResultMsg{generation: generation, runID: model.detail.runID, err: operatorSafeError{Category: application.ErrorUnavailable, Message: "refresh unavailable"}})
	model = updated.(operatorModel)
	if model.detail.staleError == nil || model.decisionCanStart() {
		t.Fatalf("stale cached detail remained actionable: %+v", model.detail)
	}
	updated, command = model.Update(keyMessage('d'))
	model = updated.(operatorModel)
	if command != nil || model.decision.stage != operatorDecisionIdle {
		t.Fatalf("stale cached detail started decision: %+v", model.decision)
	}
}

func TestOperatorDecisionPinsTrustWarningWhileDirectOptionIsVisible(t *testing.T) {
	model := decisionOperatorModel()
	updated, _ := model.Update(keyMessage('d'))
	model = updated.(operatorModel)
	request := model.detail.detail.Decision
	request.Question = strings.Repeat("Long untrusted question ", 20)
	request.Context = strings.Repeat("Long untrusted context ", 20)
	request.BlockingReason = strings.Repeat("Long untrusted blocking reason ", 20)
	request.Options = append(request.Options, application.RoutineDecisionOption{ID: "final-option", Description: "Final persisted choice"})
	model.decision.requestFocus = operatorDecisionRequestOptions
	model.decision.optionIndex = len(request.Options) - 1
	model.ensureSelectedDecisionOptionVisible()
	output := ansi.Strip(model.render())
	if !strings.Contains(output, "UNTRUSTED DECISION REQUEST") || !strings.Contains(output, "final-option") {
		t.Fatalf("warning or selected option scrolled away:\n%s", output)
	}
	if lipgloss.Width(model.render()) > 80 || lipgloss.Height(model.render()) > 24 {
		t.Fatalf("long direct decision exceeded viewport: %dx%d", lipgloss.Width(model.render()), lipgloss.Height(model.render()))
	}
}

func TestOperatorDecisionReviewEscapesExactPayload(t *testing.T) {
	model := decisionOperatorModel()
	model.decision.stage = operatorDecisionReviewing
	model.decision.payload = application.LegalDecisionInput{ChoiceID: "choice  a\x1b[31m", Instructions: "line one\nline\t\x1b[2Jtwo"}
	review := ansi.Strip(strings.Join(model.decisionReviewLines(), "\n"))
	for _, exact := range []string{operatorDecisionLiteral(model.decision.payload.ChoiceID), operatorDecisionLiteral(model.decision.payload.Instructions)} {
		if !strings.Contains(review, exact) {
			t.Fatalf("review omitted exact escaped payload %q:\n%s", exact, review)
		}
	}
	serialized, err := json.Marshal(model.decision.payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"choice_id":` + operatorDecisionLiteral(model.decision.payload.ChoiceID), `"instructions":` + operatorDecisionLiteral(model.decision.payload.Instructions)} {
		if !strings.Contains(string(serialized), field) {
			t.Fatalf("serialized payload omitted reviewed field %q: payload=%s review=%s", field, serialized, review)
		}
	}
	if strings.ContainsRune(review, '\x1b') {
		t.Fatalf("review emitted an active terminal escape:\n%q", review)
	}
	request := decisionFixtureRunDetail().Decision
	request.Options = []application.RoutineDecisionOption{{ID: "choice  a\x1b[31m", Description: "First"}, {ID: "choice a", Description: "Second"}}
	selection := ansi.Strip(strings.Join(model.renderDecisionRequest(request), "\n"))
	for _, id := range []string{request.Options[0].ID, request.Options[1].ID} {
		if !strings.Contains(selection, operatorDecisionLiteral(id)) {
			t.Fatalf("selection did not distinguish exact option ID %q:\n%s", id, selection)
		}
	}
	if strings.ContainsRune(selection, '\x1b') {
		t.Fatalf("selection emitted an active terminal escape:\n%q", selection)
	}
}

func TestOperatorDecisionCancellationConflictAndRouteFencing(t *testing.T) {
	model := decisionOperatorModel()
	updated, _ := model.Update(keyMessage('d'))
	model = updated.(operatorModel)
	updated, command := model.Update(keySpecial(tea.KeyEscape, 0))
	model = updated.(operatorModel)
	if command != nil || model.decision.stage != operatorDecisionIdle || model.detail.detail.Run.State != domain.StateAwaitingHumanDecision {
		t.Fatalf("cancel mutated state=%+v", model.decision)
	}
	updated, _ = model.Update(keyMessage('d'))
	model = updated.(operatorModel)
	updated, route := model.Update(keyMessage('3'))
	model = updated.(operatorModel)
	if route == nil || model.route != operatorAttentionRoute || model.decision.stage != operatorDecisionIdle {
		t.Fatalf("route change retained decision state=%+v route=%q", model.decision, model.route)
	}

	model = decisionOperatorModel()
	updated, _ = model.Update(keyMessage('d'))
	model = updated.(operatorModel)
	updated, _ = model.Update(keySpecial(tea.KeyEnter, 0))
	model = updated.(operatorModel)
	updated, _ = model.Update(keySpecial(tea.KeyEnter, 0))
	model = updated.(operatorModel)
	model.decision.editor.SetValue("  Keep the public contract.  ")
	updated, _ = model.Update(keySpecial('s', tea.ModCtrl))
	model = updated.(operatorModel)
	if model.decision.payload.Instructions != "Keep the public contract." {
		t.Fatalf("nonblank instructions=%q", model.decision.payload.Instructions)
	}
	updated, _ = model.Update(keySpecial(tea.KeyEnter, 0))
	model = updated.(operatorModel)
	updated, _ = model.Update(keySpecial(tea.KeyEnter, 0))
	model = updated.(operatorModel)
	updated, _ = model.Update(operatorDecisionResultMsg{generation: model.decision.operationGeneration, runID: model.detail.runID, offerID: model.decision.offerID, payload: model.decision.payload, err: operatorSafeError{Category: application.ErrorConflict, Message: "decision authority changed"}})
	model = updated.(operatorModel)
	if model.decision.stage != operatorDecisionConflicted || strings.Contains(strings.ToLower(ansi.Strip(model.render())), "succeeded") {
		t.Fatalf("conflict claimed success: %+v\n%s", model.decision, ansi.Strip(model.render()))
	}
}

func decisionOperatorModel() operatorModel {
	model := newOperatorModel(context.Background(), inertOperatorLoader{})
	detail := decisionFixtureRunDetail()
	model.width, model.height, model.route = 80, 24, operatorRunDetailRoute
	model.detail = operatorRunDetailState{detail: &detail, runID: detail.Run.RunID, returnRoute: operatorAttentionRoute, generation: 3}
	model.refreshing = false
	model.resizeDecisionEditor()
	return model
}

func decisionFixtureRunDetail() application.RoutineRunDetail {
	now := time.Date(2026, 9, 3, 4, 0, 0, 0, time.UTC)
	offer := application.LegalActionOffer{OfferID: "opaque-decision-offer", Action: application.OperationDecide, Scope: application.ScopeRun, TargetID: "run-decision", Confirmation: application.LegalActionConfirmationInput, InputKind: application.LegalActionInputDecision, Consequence: application.LegalActionConsequenceResumeExecution}
	return application.RoutineRunDetail{
		Metadata: application.RoutineProjectionMetadata{ObservedAt: now},
		Run:      application.RoutineRunSummary{RunID: "run-decision", Repository: "owner/repo", State: domain.StateAwaitingHumanDecision, UpdatedAt: now.Add(-time.Minute)},
		Phase:    application.RoutinePhaseImplementation,
		Wait:     application.RoutineWaitHumanDecision,
		Decision: &application.RoutineDecisionRequest{Question: "Choose a release boundary?", Context: "The stored contract needs one fixed option.", BlockingReason: "Execution cannot continue without a choice.", Recommendation: "inclusive", ContentTrust: "untrusted", Options: []application.RoutineDecisionOption{{ID: "inclusive", Description: "Include both bounds."}, {ID: "exclusive", Description: "Exclude both bounds."}}},
		Offers:   []application.LegalActionOffer{offer},
	}
}
