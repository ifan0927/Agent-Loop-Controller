package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	sqlitestore "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type recordingOverviewSource struct {
	observedAt time.Time
	projection application.RoutineOverviewProjection
	err        error
}

func (s *recordingOverviewSource) Get(_ context.Context, _ application.Requester, observedAt time.Time) (application.RoutineOverviewProjection, error) {
	s.observedAt = observedAt
	return s.projection, s.err
}

type recordingRepositorySource struct {
	observedAt time.Time
	limit      int
	cursor     string
	page       application.RoutineRepositoryPage
	err        error
}

func (s *recordingRepositorySource) List(_ context.Context, _ application.Requester, limit int, cursor string, observedAt time.Time) (application.RoutineRepositoryPage, error) {
	s.observedAt, s.limit, s.cursor = observedAt, limit, cursor
	return s.page, s.err
}

type inertOperatorLoader struct{}

func (inertOperatorLoader) LoadOverview(context.Context, time.Time) (operatorOverviewBatch, error) {
	return operatorOverviewBatch{}, errors.New("not used")
}
func (inertOperatorLoader) LoadRuns(context.Context, application.RunLifecycleFilter, string, string, time.Time) (application.RoutineRunPage, error) {
	return application.RoutineRunPage{}, errors.New("not used")
}
func (inertOperatorLoader) LoadAttention(context.Context, string, time.Time) (application.RoutineAttentionPage, error) {
	return application.RoutineAttentionPage{}, errors.New("not used")
}
func (inertOperatorLoader) LoadRunDetail(context.Context, string, time.Time) (application.RoutineRunDetail, error) {
	return application.RoutineRunDetail{}, errors.New("not used")
}

func TestProductionOperatorOverviewLoaderUsesOneObservedTimeAndBoundedRepositoryPage(t *testing.T) {
	observedAt := time.Date(2026, 9, 1, 2, 3, 4, 0, time.FixedZone("fixture", 8*60*60))
	overview := &recordingOverviewSource{projection: application.RoutineOverviewProjection{Readiness: application.AggregateReady}}
	repositories := &recordingRepositorySource{page: application.RoutineRepositoryPage{Collection: application.RoutineCollectionMetadata{Total: 101, Truncated: true}}}
	loader := productionOperatorLoader{overview: overview, repositories: repositories}

	batch, err := loader.LoadOverview(context.Background(), observedAt)
	if err != nil {
		t.Fatal(err)
	}
	want := observedAt.UTC()
	if batch.ObservedAt != want || overview.observedAt != want || repositories.observedAt != want {
		t.Fatalf("batch=%s overview=%s repositories=%s", batch.ObservedAt, overview.observedAt, repositories.observedAt)
	}
	if repositories.limit != application.RoutineQueryMaximumLimit || repositories.cursor != "" || batch.Repositories.Collection.Total != 101 {
		t.Fatalf("limit=%d cursor=%q batch=%+v", repositories.limit, repositories.cursor, batch.Repositories.Collection)
	}

	repositories.err = errors.New("repository projection failed")
	if partial, err := loader.LoadOverview(context.Background(), observedAt); err == nil || partial.Overview.Readiness != "" {
		t.Fatalf("partial=%+v err=%v", partial, err)
	}
}

func TestOperatorModelRefreshFailureAndGenerationSafety(t *testing.T) {
	model := newOperatorModel(context.Background(), inertOperatorLoader{})
	defer model.cancel()
	model.refreshInterval = time.Hour

	updated, _ := model.Update(operatorRefreshResultMsg{generation: 1, err: operatorSafeError{Category: application.ErrorUnavailable, Message: "initial unavailable"}})
	model = updated.(operatorModel)
	if model.initialError == nil || model.batch != nil || model.refreshing {
		t.Fatalf("initial failure model=%+v", model)
	}
	updated, retry := model.Update(keyMessage('r'))
	model = updated.(operatorModel)
	if retry == nil || model.generation != 2 || !model.refreshing || model.initialError != nil {
		t.Fatalf("retry generation=%d refreshing=%t error=%v command=%v", model.generation, model.refreshing, model.initialError, retry)
	}
	updated, duplicate := model.Update(keyMessage('r'))
	model = updated.(operatorModel)
	if duplicate != nil || model.generation != 2 {
		t.Fatalf("duplicate refresh generation=%d command=%v", model.generation, duplicate)
	}

	first := operatorFixtureBatch()
	updated, ticker := model.Update(operatorRefreshResultMsg{generation: 2, batch: first})
	model = updated.(operatorModel)
	if ticker == nil || model.batch == nil || model.initialError != nil || !model.tickerStarted {
		t.Fatalf("first success model=%+v command=%v", model, ticker)
	}
	model.panels[operatorRepositoriesPanel] = operatorPanelState{index: 1}
	updated, _ = model.Update(keyMessage('r'))
	model = updated.(operatorModel)
	if model.generation != 3 || !model.refreshing {
		t.Fatalf("manual refresh model=%+v", model)
	}

	obsolete := operatorFixtureBatch()
	obsolete.Repositories.Repositories = nil
	updated, _ = model.Update(operatorRefreshResultMsg{generation: 2, batch: obsolete})
	model = updated.(operatorModel)
	if len(model.batch.Repositories.Repositories) != 2 || !model.refreshing {
		t.Fatal("obsolete generation replaced the current batch")
	}
	updated, _ = model.Update(operatorRefreshResultMsg{generation: 3, err: operatorSafeError{Category: application.ErrorUnavailable, Message: "refresh unavailable"}})
	model = updated.(operatorModel)
	if model.staleError == nil || model.batch == nil || model.refreshing || len(model.batch.Repositories.Repositories) != 2 {
		t.Fatalf("stale refresh model=%+v", model)
	}

	updated, _ = model.Update(keyMessage('r'))
	model = updated.(operatorModel)
	reordered := operatorFixtureBatch()
	reordered.Repositories.Repositories[0], reordered.Repositories.Repositories[1] = reordered.Repositories.Repositories[1], reordered.Repositories.Repositories[0]
	updated, _ = model.Update(operatorRefreshResultMsg{generation: 4, batch: reordered})
	model = updated.(operatorModel)
	if model.staleError != nil || model.panels[operatorRepositoriesPanel].index != 0 || model.panelRows(operatorRepositoriesPanel)[0].id != "owner/two" {
		t.Fatalf("selection was not preserved by identity: state=%+v rows=%+v", model.panels[operatorRepositoriesPanel], model.panelRows(operatorRepositoriesPanel))
	}
}

func TestOperatorLayoutsAreBoundedAndPrioritized(t *testing.T) {
	for _, test := range []struct {
		name          string
		width, height int
		wantOrder     []string
	}{
		{name: "wide", width: 92, height: 24, wantOrder: []string{"Repositories", "Attention"}},
		{name: "vertical", width: 80, height: 24, wantOrder: []string{"Attention", "Repositories", "Runs"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := loadedOperatorModel(test.width, test.height)
			output := model.render()
			if width, height := lipgloss.Width(output), lipgloss.Height(output); width > test.width || height > test.height {
				t.Fatalf("rendered=%dx%d terminal=%dx%d\n%s", width, height, test.width, test.height, ansi.Strip(output))
			}
			plain := ansi.Strip(output)
			if !strings.Contains(plain, "Agent Loop Controller / Overview") || !strings.Contains(plain, "Observed ") || !strings.Contains(plain, "● READY  ·  Controller is ready") {
				t.Fatalf("overview hierarchy is missing:\n%s", plain)
			}
			position := -1
			for _, marker := range test.wantOrder {
				next := operatorPanelPosition(plain, marker)
				if next < 0 || next <= position {
					t.Fatalf("marker %q order failed in:\n%s", marker, plain)
				}
				position = next
			}
			if !strings.Contains(plain, "2 of 4 displayed") || !strings.Contains(plain, "2 of 3 displayed") || !strings.Contains(plain, "1 of 2 displayed") {
				t.Fatalf("collection truncation is not explicit:\n%s", plain)
			}
			if strings.Count(plain, "> ") != 1 {
				t.Fatalf("only the focused selection should be highlighted:\n%s", plain)
			}
		})
	}
}

func TestOperatorTooSmallAndEmptyStates(t *testing.T) {
	model := loadedOperatorModel(79, 24)
	tooSmall := ansi.Strip(model.render())
	if !strings.Contains(tooSmall, "Terminal too small") || !strings.Contains(tooSmall, "79x24") || !strings.Contains(tooSmall, "3 Attention") {
		t.Fatalf("too-small output:\n%s", tooSmall)
	}
	model.width, model.height = 80, 23
	if output := ansi.Strip(model.render()); !strings.Contains(output, "Terminal too small") || !strings.Contains(output, "80x23") {
		t.Fatalf("short output:\n%s", output)
	}

	empty := operatorFixtureBatch()
	empty.Overview.Runs = application.RoutineRunCounts{}
	empty.Overview.Actionable, empty.Overview.Attention = nil, nil
	empty.Overview.ActionableTotal = 0
	empty.Repositories = application.RoutineRepositoryPage{}
	model.width, model.height, model.batch = 80, 24, &empty
	model.normalizeFocus()
	output := ansi.Strip(model.render())
	for _, phrase := range []string{"No registered repositories", "No active or recently ended runs", "No operator attention"} {
		if !strings.Contains(output, phrase) {
			t.Fatalf("missing %q in:\n%s", phrase, output)
		}
	}
	if model.focus != "" || len(model.selectablePanels()) != 0 {
		t.Fatalf("empty focus=%q panels=%v", model.focus, model.selectablePanels())
	}
}

func TestOperatorFocusOrderSelectionAndEnterNoop(t *testing.T) {
	model := loadedOperatorModel(80, 24)
	if model.focus != operatorAttentionPanel {
		t.Fatalf("initial vertical focus=%q", model.focus)
	}
	updated, _ := model.Update(keySpecial(tea.KeyTab, 0))
	model = updated.(operatorModel)
	if model.focus != operatorRepositoriesPanel {
		t.Fatalf("tab focus=%q", model.focus)
	}
	updated, _ = model.Update(keyMessage('j'))
	model = updated.(operatorModel)
	if model.panels[operatorRepositoriesPanel].index != 1 {
		t.Fatalf("repository selection=%d", model.panels[operatorRepositoriesPanel].index)
	}
	before := model
	updated, command := model.Update(keySpecial(tea.KeyEnter, 0))
	after := updated.(operatorModel)
	if command != nil || after.focus != before.focus || after.panels[operatorRepositoriesPanel] != before.panels[operatorRepositoriesPanel] || strings.Contains(strings.ToLower(after.renderHelp()), "enter") {
		t.Fatal("Enter changed or was advertised by the Overview")
	}
	updated, _ = model.Update(keySpecial(tea.KeyTab, tea.ModShift))
	model = updated.(operatorModel)
	if model.focus != operatorAttentionPanel {
		t.Fatalf("shift-tab focus=%q", model.focus)
	}
	updated, _ = model.Update(keyMessage('?'))
	model = updated.(operatorModel)
	if !model.help.ShowAll {
		t.Fatal("help did not expand")
	}
	if output := model.render(); lipgloss.Width(output) > model.width || lipgloss.Height(output) > model.height || strings.Contains(strings.ToLower(ansi.Strip(output)), "enter") {
		t.Fatalf("expanded help is not bounded or advertises Enter:\n%s", ansi.Strip(output))
	}
}

func TestOperatorHealthUsesProjectionAggregateVerbatim(t *testing.T) {
	for _, readiness := range []application.AggregateReadiness{
		application.AggregateReady, application.AggregateDegraded, application.AggregateAttentionRequired,
		application.AggregateRestartRequired, application.AggregateStale, application.AggregateOffline,
		application.AggregateUnknown, application.AggregateConflict,
	} {
		model := loadedOperatorModel(92, 24)
		model.batch.Overview.Readiness = readiness
		output := ansi.Strip(model.renderHealth())
		if !strings.Contains(output, " "+presentationLabel(string(readiness))+"  ·") || !strings.Contains(output, healthSummary(string(readiness))) {
			t.Fatalf("readiness=%q output=%q", readiness, output)
		}
	}
	if got := presentationLabel("future_controller_state"); got != "future_controller_state" {
		t.Fatalf("unknown code hidden as %q", got)
	}
}

func TestOperatorSelectedRowUsesOneUninterruptedHighlight(t *testing.T) {
	model := loadedOperatorModel(92, 24)
	row := operatorRow{id: "run-failed", name: "IFAN-139 / run-failed · owner/repository", status: "FAILED", detail: "recent · 1d ago", tone: "danger"}
	selected := model.renderRow(row, 80, true)
	if strings.Contains(selected, operatorDangerStyle.Render("● FAILED")) {
		t.Fatalf("selected row contains a nested status reset that interrupts its background: %q", selected)
	}
	if plain := ansi.Strip(selected); !strings.HasPrefix(plain, "> ") || !strings.Contains(plain, "● FAILED") {
		t.Fatalf("selected row lost its marker or status: %q", plain)
	}
	unselected := model.renderRow(row, 80, false)
	if !strings.Contains(unselected, operatorDangerStyle.Render("● FAILED")) {
		t.Fatal("unselected row lost its status color")
	}
}

func TestOperatorRoutesRunsFiltersPaginationAndReturnState(t *testing.T) {
	model := loadedOperatorModel(92, 24)
	model.focus = operatorRunsPanel
	model.panels[operatorRunsPanel] = operatorPanelState{index: 1}
	selected := model.selectedOverviewRunID()
	updated, detailCommand := model.Update(keySpecial(tea.KeyEnter, 0))
	model = updated.(operatorModel)
	if detailCommand == nil || model.route != operatorRunDetailRoute || model.detail.runID != selected || model.detail.returnRoute != operatorOverviewRoute {
		t.Fatalf("detail route=%q state=%+v", model.route, model.detail)
	}
	detail := operatorFixtureRunDetail(selected)
	updated, _ = model.Update(operatorRunDetailResultMsg{generation: model.detail.generation, runID: selected, detail: detail})
	model = updated.(operatorModel)
	updated, _ = model.Update(keySpecial(tea.KeyEscape, 0))
	model = updated.(operatorModel)
	if model.route != operatorOverviewRoute || model.panels[operatorRunsPanel].index != 1 {
		t.Fatalf("overview return route=%q selection=%d", model.route, model.panels[operatorRunsPanel].index)
	}

	updated, loadRuns := model.Update(keyMessage('2'))
	model = updated.(operatorModel)
	if loadRuns == nil || model.route != operatorRunsRoute || !model.runs.refreshing || model.runs.pending.lifecycle != application.RunLifecycleActive {
		t.Fatalf("runs route=%q state=%+v", model.route, model.runs)
	}
	page := application.RoutineRunPage{Metadata: application.RoutineProjectionMetadata{ObservedAt: time.Now().UTC()}, Lifecycle: application.RunLifecycleActive, Collection: application.RoutineCollectionMetadata{Total: 2, Truncated: true, NextCursor: "next"}, Runs: []application.RoutineRunSummary{{RunID: "run-2", Repository: "owner/repo", State: domain.StateExecuting}, {RunID: "run-1", Repository: "owner/repo", State: domain.StateVerifying}}}
	updated, _ = model.Update(operatorRunsResultMsg{generation: model.runs.generation, request: *model.runs.pending, page: page})
	model = updated.(operatorModel)
	updated, nextPage := model.Update(keyMessage('n'))
	model = updated.(operatorModel)
	if nextPage == nil || model.runs.pending.cursor != "next" || len(model.runs.pending.previous) != 1 {
		t.Fatalf("next pending=%+v", model.runs.pending)
	}
	second := page
	second.Collection = application.RoutineCollectionMetadata{Total: 2}
	second.Runs = []application.RoutineRunSummary{{RunID: "run-0", Repository: "owner/repo", State: domain.StateExecuting}}
	updated, _ = model.Update(operatorRunsResultMsg{generation: model.runs.generation, request: *model.runs.pending, page: second})
	model = updated.(operatorModel)
	updated, previousPage := model.Update(keyMessage('p'))
	model = updated.(operatorModel)
	if previousPage == nil || model.runs.pending.cursor != "" || len(model.runs.pending.previous) != 0 {
		t.Fatalf("previous pending=%+v", model.runs.pending)
	}
	updated, _ = model.Update(operatorRunsResultMsg{generation: model.runs.generation, request: *model.runs.pending, page: page})
	model = updated.(operatorModel)
	updated, filterCommand := model.Update(keyMessage('f'))
	model = updated.(operatorModel)
	if filterCommand == nil || model.runs.pending.lifecycle != application.RunLifecycleEnded || model.runs.pending.cursor != "" || len(model.runs.pending.previous) != 0 {
		t.Fatalf("filter pending=%+v", model.runs.pending)
	}
	obsolete := page
	obsolete.Runs = nil
	updated, _ = model.Update(operatorRunsResultMsg{generation: model.runs.generation - 1, request: operatorRunsRequest{lifecycle: application.RunLifecycleActive}, page: obsolete})
	model = updated.(operatorModel)
	if model.runs.page == nil || len(model.runs.page.Runs) != 2 {
		t.Fatal("obsolete Runs generation replaced the complete page")
	}
}

func TestOperatorAttentionRoutePagingNavigationRefreshAndRendering(t *testing.T) {
	model := loadedOperatorModel(80, 24)
	updated, load := model.Update(keyMessage('3'))
	model = updated.(operatorModel)
	if load == nil || model.route != operatorAttentionRoute || !model.attention.refreshing {
		t.Fatalf("attention route=%q state=%+v", model.route, model.attention)
	}
	now := time.Date(2026, 9, 2, 5, 0, 0, 0, time.UTC)
	page := application.RoutineAttentionPage{
		Metadata:   application.RoutineProjectionMetadata{SchemaVersion: application.RoutineAttentionSchemaVersion, ObservedAt: now},
		Collection: application.RoutineCollectionMetadata{Total: 3, Truncated: true, NextCursor: "next-attention"},
		Scope:      application.ScopeController,
		Items: []application.RoutineAttentionItem{
			{EventID: "event-controller", EventType: application.OperatorAttentionCandidateScan, Scope: application.ScopeController, TargetID: "local-controller", ControllerState: "scan", AttentionState: application.RoutineAttentionActive, Severity: "warning", ReasonCode: "truncated", OccurredAt: now.Add(-time.Hour), ObservedAt: now.Add(-time.Minute), Offers: []application.RoutineAttentionOfferSummary{}, Navigation: application.RoutineAttentionNavigationNone},
			{EventID: "event-run", EventType: application.OperatorAttentionHumanDecision, Scope: application.ScopeRun, TargetID: "run-attention", RunID: "run-attention", LinearIdentifier: "IFAN-177", Repository: "owner/repo", ControllerState: string(domain.StateAwaitingHumanDecision), AttentionState: application.RoutineAttentionActive, Severity: "warning", ReasonCode: "human_decision_required", OccurredAt: now.Add(-30 * time.Minute), ObservedAt: now, Offers: []application.RoutineAttentionOfferSummary{{OfferID: "opaque", Action: application.OperationDecide, Reason: "human_decision_required", Confirmation: application.LegalActionConfirmationInput, InputKind: application.LegalActionInputDecision, Consequence: application.LegalActionConsequenceResumeExecution}}, Navigation: application.RoutineAttentionNavigationRunDetail},
		},
	}
	updated, _ = model.Update(operatorAttentionResultMsg{generation: model.attention.generation, request: *model.attention.pending, page: page})
	model = updated.(operatorModel)
	plain := ansi.Strip(model.render())
	for _, phrase := range []string{"Agent Loop Controller / Attention", "Inbox · page 1 · 2 of 3 displayed", "No Controller action offered", "candidate_scan_incomplete", "WARNING/ACTIVE"} {
		if !strings.Contains(plain, phrase) {
			t.Fatalf("compact Attention missing %q:\n%s", phrase, plain)
		}
	}
	if commandModel, command := model.Update(keySpecial(tea.KeyEnter, 0)); command != nil || commandModel.(operatorModel).route != operatorAttentionRoute {
		t.Fatal("non-run Attention item exposed Enter navigation")
	}
	updated, _ = model.Update(keyMessage('j'))
	model = updated.(operatorModel)
	if model.attention.index != 1 || !strings.Contains(strings.ToLower(model.renderHelp()), "enter open run") {
		t.Fatalf("run selection/help state=%+v help=%q", model.attention, model.renderHelp())
	}
	updated, detailCommand := model.Update(keySpecial(tea.KeyEnter, 0))
	model = updated.(operatorModel)
	if detailCommand == nil || model.route != operatorRunDetailRoute || model.detail.runID != "run-attention" || model.detail.returnRoute != operatorAttentionRoute {
		t.Fatalf("detail state=%+v", model.detail)
	}
	updated, _ = model.Update(keySpecial(tea.KeyEscape, 0))
	model = updated.(operatorModel)
	if model.route != operatorAttentionRoute || model.attention.index != 1 || model.attention.selectedEventID != "event-run" {
		t.Fatalf("Attention return state=%+v", model.attention)
	}
	updated, next := model.Update(keyMessage('n'))
	model = updated.(operatorModel)
	if next == nil || model.attention.pending.cursor != "next-attention" || len(model.attention.pending.previous) != 1 {
		t.Fatalf("next request=%+v", model.attention.pending)
	}
	second := page
	second.Collection = application.RoutineCollectionMetadata{Total: 3}
	second.Items = []application.RoutineAttentionItem{{EventID: "event-last", EventType: application.OperatorAttentionSchedulerLease, Scope: application.ScopeController, TargetID: "local-controller", ControllerState: "scheduler", AttentionState: application.RoutineAttentionUnknown, Severity: "info", ReasonCode: "lease_lost", OccurredAt: now, ObservedAt: now, Offers: []application.RoutineAttentionOfferSummary{}, Navigation: application.RoutineAttentionNavigationNone}}
	updated, _ = model.Update(operatorAttentionResultMsg{generation: model.attention.generation, request: *model.attention.pending, page: second})
	model = updated.(operatorModel)
	updated, previous := model.Update(keyMessage('p'))
	model = updated.(operatorModel)
	if previous == nil || model.attention.pending.cursor != "" || len(model.attention.pending.previous) != 0 {
		t.Fatalf("previous request=%+v", model.attention.pending)
	}
	updated, _ = model.Update(operatorAttentionResultMsg{generation: model.attention.generation - 1, request: operatorAttentionRequest{}, page: application.RoutineAttentionPage{}})
	model = updated.(operatorModel)
	if model.attention.page == nil || model.attention.page.Items[0].EventID != "event-last" {
		t.Fatal("late Attention result replaced the current page")
	}
	updated, _ = model.Update(operatorAttentionResultMsg{generation: model.attention.generation, request: *model.attention.pending, err: operatorSafeError{Category: application.ErrorUnavailable, Message: "refresh unavailable"}})
	model = updated.(operatorModel)
	if model.attention.staleError == nil || model.attention.page == nil {
		t.Fatalf("stale Attention state=%+v", model.attention)
	}
	model.width = 92
	if output := model.render(); lipgloss.Width(output) > 92 || lipgloss.Height(output) > 24 {
		t.Fatalf("wide Attention exceeded bounds:\n%s", ansi.Strip(output))
	}
}

func TestOperatorAttentionInitialFailureAndDuplicateRefreshAreSafe(t *testing.T) {
	model := loadedOperatorModel(80, 24)
	updated, firstLoad := model.Update(keyMessage('3'))
	model = updated.(operatorModel)
	if firstLoad == nil || model.attention.pending == nil {
		t.Fatalf("initial Attention request=%+v command=%v", model.attention, firstLoad)
	}
	request := *model.attention.pending
	updated, duplicate := model.Update(keyMessage('r'))
	model = updated.(operatorModel)
	if duplicate != nil || model.attention.generation != 1 {
		t.Fatalf("duplicate refresh generation=%d command=%v", model.attention.generation, duplicate)
	}
	updated, _ = model.Update(operatorAttentionResultMsg{generation: 1, request: request, err: operatorSafeError{Category: application.ErrorUnavailable, Message: "initial unavailable"}})
	model = updated.(operatorModel)
	if model.attention.page != nil || model.attention.initialError == nil || model.attention.refreshing {
		t.Fatalf("initial failure state=%+v", model.attention)
	}
	updated, retry := model.Update(keyMessage('r'))
	model = updated.(operatorModel)
	if retry == nil || model.attention.generation != 2 || model.attention.initialError != nil || !model.attention.refreshing {
		t.Fatalf("retry state=%+v command=%v", model.attention, retry)
	}
	updated, duplicate = model.Update(keyMessage('r'))
	model = updated.(operatorModel)
	if duplicate != nil || model.attention.generation != 2 {
		t.Fatalf("duplicate retry generation=%d command=%v", model.attention.generation, duplicate)
	}
}

func TestOperatorExactRepositoryFilterAndSharedRunDetailRendering(t *testing.T) {
	model := loadedOperatorModel(80, 24)
	model.route = operatorRunsRoute
	page := application.RoutineRunPage{Metadata: application.RoutineProjectionMetadata{ObservedAt: time.Now().UTC()}, Lifecycle: application.RunLifecycleActive, Runs: []application.RoutineRunSummary{{RunID: "run-detail", Repository: "owner/repo", State: domain.StateAwaitingHumanApproval}}}
	model.runs.page, model.runs.request = &page, operatorRunsRequest{lifecycle: application.RunLifecycleActive}
	updated, _ := model.Update(keyMessage('/'))
	model = updated.(operatorModel)
	for _, value := range "owner/repo" {
		updated, _ = model.Update(keyMessage(value))
		model = updated.(operatorModel)
	}
	updated, applyFilter := model.Update(keySpecial(tea.KeyEnter, 0))
	model = updated.(operatorModel)
	if applyFilter == nil || model.runs.pending.repository != "owner/repo" || model.runs.pending.cursor != "" {
		t.Fatalf("repository pending=%+v", model.runs.pending)
	}
	filtered := page
	filtered.Repository = "owner/repo"
	updated, _ = model.Update(operatorRunsResultMsg{generation: model.runs.generation, request: *model.runs.pending, page: filtered})
	model = updated.(operatorModel)
	updated, detailCommand := model.Update(keySpecial(tea.KeyEnter, 0))
	model = updated.(operatorModel)
	if detailCommand == nil || model.route != operatorRunDetailRoute || model.detail.returnRoute != operatorRunsRoute {
		t.Fatalf("detail route=%q state=%+v", model.route, model.detail)
	}
	detail := operatorFixtureRunDetail("run-detail")
	updated, _ = model.Update(operatorRunDetailResultMsg{generation: model.detail.generation, runID: "run-detail", detail: detail})
	model = updated.(operatorModel)
	plain := ansi.Strip(model.render())
	for _, phrase := range []string{"Run detail", "Wait ABNORMAL WAIT", "ATTENTION", "Delivery gates", "VERIFICATION", "NOT APPLICABLE", "CONFLICT"} {
		if !strings.Contains(plain, phrase) {
			t.Fatalf("missing %q in:\n%s", phrase, plain)
		}
	}
	if strings.Contains(strings.ToLower(plain), "legal action") || strings.Contains(strings.ToLower(plain), "decision option") {
		t.Fatalf("read-only detail exposed mutation affordances:\n%s", plain)
	}
	if lipgloss.Width(model.render()) > model.width || lipgloss.Height(model.render()) > model.height {
		t.Fatalf("detail exceeded 80x24:\n%s", plain)
	}
}

func operatorFixtureRunDetail(runID string) application.RoutineRunDetail {
	now := time.Now().UTC()
	statuses := []application.DeliveryGateStatus{application.GatePassed, application.GateRunning, application.GateFailed, application.GateUnknown, application.GateConflict, application.GateNotApplicable, application.GateBlocked, application.GatePending, application.GatePassed, application.GatePassed, application.GatePassed}
	names := []application.DeliveryGateName{application.GateVerification, application.GateIndependentReview, application.GateBranchPublication, application.GatePullRequest, application.GateRequiredChecks, application.GateReviewConversations, application.GateHumanApproval, application.GateMerge, application.GateLinearCompletion, application.GateSourceCheckout, application.GateCleanup}
	gates := make([]application.RoutineDeliveryGate, 0, len(names))
	for index, name := range names {
		observed := now.Add(-time.Duration(index) * time.Minute)
		gates = append(gates, application.RoutineDeliveryGate{Name: name, Status: statuses[index], ReasonCode: "fixture_reason", BoundHead: "candidate-head", ObservedAt: &observed, EvidenceCount: index + 1, EvidenceTruncated: index == 3})
	}
	return application.RoutineRunDetail{
		Metadata: application.RoutineProjectionMetadata{ObservedAt: now},
		Run:      application.RoutineRunSummary{RunID: runID, LinearIdentifier: "IFAN-175", Repository: "owner/repo", State: domain.StateAwaitingHumanApproval, CandidateHead: "candidate-head", UpdatedAt: now.Add(-time.Minute), Attention: true},
		Phase:    application.RoutinePhaseApproval, Wait: application.RoutineWaitHumanApproval, WaitAssessment: application.RoutineAssessmentAbnormalWait,
		LatestTransition: &application.RoutineTransition{From: domain.StateReconcilingReviews, To: domain.StateAwaitingHumanApproval, ReasonCode: "ready_for_human_approval", BoundHead: "candidate-head", ObservedAt: now.Add(-time.Minute)},
		PullRequest:      &application.RoutinePullRequestSummary{Number: 175, State: "open", HeadSHA: "candidate-head"},
		Attention:        []application.RoutineAttentionSummary{{Severity: "warning", State: application.RoutineAttentionActive, ReasonCode: "human_approval_required", ObservedAt: now.Add(-time.Minute)}},
		Gates:            gates,
	}
}

func TestSafeOperatorErrorOnlyExposesServiceErrors(t *testing.T) {
	service := safeOperatorError(&application.ServiceError{Category: application.ErrorConflict, Message: "safe conflict"})
	if service.String() != "conflict: safe conflict" {
		t.Fatalf("service error=%q", service.String())
	}
	unsafe := safeOperatorError(errors.New("/private/path secret-token"))
	if unsafe.Category != application.ErrorInternal || unsafe.Message != "operator view is unavailable" {
		t.Fatalf("unsafe error=%+v", unsafe)
	}
}

func TestOperatorCommandHasNoRequesterFlags(t *testing.T) {
	called := false
	original := runOperatorProgram
	runOperatorProgram = func(tea.Model) error { called = true; return nil }
	t.Cleanup(func() { runOperatorProgram = original })
	if err := operatorCommand([]string{"--requester", "intruder"}); err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("requester flag error=%v", err)
	}
	if called {
		t.Fatal("operator program ran after unsupported requester flag")
	}
	if err := operatorCommand([]string{"unexpected"}); err == nil || !strings.Contains(err.Error(), "usage: agentctl operator") {
		t.Fatalf("positional argument error=%v", err)
	}
}

func TestComposeOperatorReadsBoundAuthorityWithoutMutation(t *testing.T) {
	root := resolvedTempDir(t)
	configPath, databasePath := writeCurrentManagedDraftConfig(t, root)
	loaded, err := loadManagedConfiguration(configPath)
	if err != nil {
		t.Fatal(err)
	}
	bindings := loaded.Registry.Bindings()
	if len(bindings) == 0 {
		t.Fatal("fixture registry has no repository")
	}
	binding := bindings[0]
	repository := application.LocalRepository{CanonicalRepository: binding.CanonicalRepository, AllowedOperatorLogins: []string{"ifan0927"}, TrustedOperatorActors: []application.TrustedActorIdentity{{DatabaseID: 33, NodeID: "MDQ6VXNlcjMz", Login: "ifan0927", Type: "User"}}}
	repositoryRaw, err := json.Marshal(repository)
	if err != nil {
		t.Fatal(err)
	}
	writable, err := sqlitestore.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	run := application.Run{ID: "operator-read-run", IssueID: "IFAN-175", IdempotencyKey: "operator-read-key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: binding.CanonicalRepository, RepositoryConfigJSON: string(repositoryRaw), BaseBranch: binding.BaseBranch, WorkingBranch: "ifan/ifan-175", State: domain.StateReceived}
	authority := testExistingNewAdmissionGate(t, writable).Decision.Authority
	if _, created, createErr := writable.CreateRun(context.Background(), application.CreateRunInput{Run: run, ConfigurationAuthority: authority}); createErr != nil || !created {
		_ = writable.Close()
		t.Fatalf("create fixture run created=%t err=%v", created, createErr)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}
	beforeConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeDatabase, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}

	composition, err := composeOperator(context.Background(), configPath)
	if err != nil {
		t.Fatal(err)
	}
	observedAt := time.Date(2026, 9, 1, 4, 5, 6, 0, time.UTC)
	batch, loadErr := composition.loader.LoadOverview(context.Background(), observedAt)
	runs, runsErr := composition.loader.LoadRuns(context.Background(), application.RunLifecycleActive, "", "", observedAt)
	attention, attentionErr := composition.loader.LoadAttention(context.Background(), "", observedAt)
	detail, detailErr := composition.loader.LoadRunDetail(context.Background(), run.ID, observedAt)
	closeErr := composition.Close()
	if loadErr != nil || runsErr != nil || attentionErr != nil || detailErr != nil || closeErr != nil {
		t.Fatalf("overview=%v runs=%v attention=%v detail=%v close=%v", loadErr, runsErr, attentionErr, detailErr, closeErr)
	}
	if batch.ObservedAt != observedAt || batch.Overview.Metadata.ObservedAt != observedAt || batch.Repositories.Metadata.ObservedAt != observedAt {
		t.Fatalf("observed times batch=%s overview=%s repositories=%s", batch.ObservedAt, batch.Overview.Metadata.ObservedAt, batch.Repositories.Metadata.ObservedAt)
	}
	if batch.Overview.Settings.DesiredDigest != loaded.Digest || batch.Repositories.Collection.Total < len(batch.Repositories.Repositories) {
		t.Fatalf("overview digest=%q repositories=%d", batch.Overview.Settings.DesiredDigest, batch.Repositories.Collection.Total)
	}
	if runs.Collection.Total != 1 || len(runs.Runs) != 1 || runs.Runs[0].RunID != run.ID || detail.Run.RunID != run.ID || len(detail.Gates) != 11 {
		t.Fatalf("runs=%+v detail=%+v", runs, detail)
	}
	if attention.Collection.Total != 0 || len(attention.Items) != 0 || attention.Metadata.SchemaVersion != application.RoutineAttentionSchemaVersion {
		t.Fatalf("attention=%+v", attention)
	}
	afterConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	afterDatabase, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(afterConfig) != sha256.Sum256(beforeConfig) || sha256.Sum256(afterDatabase) != sha256.Sum256(beforeDatabase) {
		t.Fatal("operator composition changed configuration authority evidence")
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Lstat(databasePath + suffix); !os.IsNotExist(err) {
			t.Fatalf("operator composition left SQLite auxiliary file %q: %v", suffix, err)
		}
	}
}

func loadedOperatorModel(width, height int) operatorModel {
	model := newOperatorModel(context.Background(), inertOperatorLoader{})
	batch := operatorFixtureBatch()
	model.width, model.height, model.batch, model.refreshing = width, height, &batch, false
	model.normalizeFocus()
	return model
}

func operatorFixtureBatch() operatorOverviewBatch {
	observedAt := time.Date(2026, 9, 1, 2, 0, 0, 0, time.UTC)
	heartbeatAge := int64(12)
	return operatorOverviewBatch{
		ObservedAt: observedAt,
		Overview: application.RoutineOverviewProjection{
			Readiness:        application.AggregateReady,
			Worker:           application.RuntimeObservation{Liveness: application.RuntimeLivenessFresh, Activity: application.RuntimeActivityRunning, HeartbeatAgeSeconds: &heartbeatAge},
			Capacity:         application.CapacityProjection{EffectiveCapacity: 2, InUse: 1},
			AdmissionEnabled: true,
			Repositories:     application.RoutineRepositoryCounts{Total: 4, Ready: 1, Unavailable: 1},
			Runs: application.RoutineRunCounts{
				Active: 2, Recent: 1, ActiveTruncated: true,
				ActiveRuns: []application.RoutineRunSummary{{RunID: "run-active", LinearIdentifier: "IFAN-1", Repository: "owner/one", State: domain.StateExecuting, Attention: true, UpdatedAt: observedAt.Add(-time.Minute)}},
				RecentRuns: []application.RoutineRunSummary{{RunID: "run-recent", LinearIdentifier: "IFAN-2", Repository: "owner/two", State: domain.StateCompleted, UpdatedAt: observedAt.Add(-time.Hour)}},
			},
			ActionableTotal:     2,
			ActionableTruncated: true,
			Actionable:          []application.RoutineActionableItem{{ItemID: "action-1", Scope: application.ScopeRun, TargetID: "run-active", Severity: "high", ReasonCode: "human_decision_required", ObservedAt: observedAt.Add(-2 * time.Minute)}},
			Attention:           []application.RoutineAttentionSummary{{EventID: "evidence-1", Scope: application.ScopeRun, TargetID: "run-active", Severity: "high", ReasonCode: "human_decision_required", State: application.RoutineAttentionActive, ObservedAt: observedAt.Add(-time.Minute)}},
		},
		Repositories: application.RoutineRepositoryPage{
			Collection: application.RoutineCollectionMetadata{Total: 4, Truncated: true},
			Repositories: []application.RoutineRepositorySummary{
				{Repository: "owner/one", LifecycleIntent: application.RepositoryEnabled, Readiness: domain.RepositoryReady, Available: true, ActiveRunID: "run-active", ConfigurationConvergence: domain.RepositoryReady, LastObservedAt: observedAt.Add(-time.Minute)},
				{Repository: "owner/two", LifecycleIntent: application.RepositoryDisabled, Readiness: domain.RepositoryNotReady, Available: false, ConfigurationConvergence: domain.RepositoryUnknown, LastObservedAt: observedAt.Add(-time.Hour)},
			},
		},
	}
}

func keyMessage(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Code: code, Text: string(code)})
}

func keySpecial(code rune, modifier tea.KeyMod) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Code: code, Mod: modifier})
}

func operatorPanelPosition(output, title string) int {
	offset := 0
	for _, line := range strings.SplitAfter(output, "\n") {
		if index := strings.Index(line, title); index >= 0 && strings.Contains(line, "displayed") {
			return offset + index
		}
		offset += len(line)
	}
	return -1
}
