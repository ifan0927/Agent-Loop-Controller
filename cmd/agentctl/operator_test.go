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
	reader     application.ControllerReadAuthority
	limit      int
	cursor     string
	page       application.RoutineRepositoryPage
	err        error
}

func (s *recordingRepositorySource) ListController(_ context.Context, reader application.ControllerReadAuthority, limit int, cursor string, observedAt time.Time) (application.RoutineRepositoryPage, error) {
	s.observedAt, s.reader, s.limit, s.cursor = observedAt, reader, limit, cursor
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
func (inertOperatorLoader) LoadRepositories(context.Context, string, time.Time) (application.RoutineRepositoryPage, error) {
	return application.RoutineRepositoryPage{}, errors.New("not used")
}
func (inertOperatorLoader) LoadRepositoryDetail(context.Context, string, time.Time) (application.RoutineRepositoryDetail, error) {
	return application.RoutineRepositoryDetail{}, errors.New("not used")
}
func (inertOperatorLoader) EnableRepository(context.Context, string, string) (application.RepositoryMutationResult, error) {
	return application.RepositoryMutationResult{}, errors.New("not used")
}

func TestProductionOperatorOverviewLoaderUsesOneObservedTimeAndBoundedRepositoryPage(t *testing.T) {
	observedAt := time.Date(2026, 9, 1, 2, 3, 4, 0, time.FixedZone("fixture", 8*60*60))
	operator := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "U_7", ActorType: "User"}
	authorizer, _ := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: operator})
	configured, _ := authorizer.ResolveConfiguredRequester(application.Requester{ID: operator.Login, Kind: "github_login", DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, ActorType: operator.ActorType})
	reader, _ := authorizer.ControllerReadCollectionAuthority(configured)
	overview := &recordingOverviewSource{projection: application.RoutineOverviewProjection{Readiness: application.AggregateReady}}
	repositories := &recordingRepositorySource{page: application.RoutineRepositoryPage{Collection: application.RoutineCollectionMetadata{Total: 101, Truncated: true}}}
	loader := productionOperatorLoader{overview: overview, repositories: repositories, reader: reader}

	batch, err := loader.LoadOverview(context.Background(), observedAt)
	if err != nil {
		t.Fatal(err)
	}
	want := observedAt.UTC()
	if batch.ObservedAt != want || overview.observedAt != want || repositories.observedAt != want {
		t.Fatalf("batch=%s overview=%s repositories=%s", batch.ObservedAt, overview.observedAt, repositories.observedAt)
	}
	if repositories.reader.Digest() != reader.Digest() || repositories.limit != application.RoutineQueryMaximumLimit || repositories.cursor != "" || batch.Repositories.Collection.Total != 101 {
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

func TestOperatorFocusOrderSelectionAndRepositoryDetailNavigation(t *testing.T) {
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
	updated, command := model.Update(keySpecial(tea.KeyEnter, 0))
	model = updated.(operatorModel)
	if command == nil || model.route != operatorRepositoryDetailRoute || model.repositoryDetail.repository != "owner/two" || model.repositoryDetail.returnRoute != operatorOverviewRoute {
		t.Fatalf("repository detail state=%+v route=%q", model.repositoryDetail, model.route)
	}
	updated, _ = model.Update(keySpecial(tea.KeyEscape, 0))
	model = updated.(operatorModel)
	if model.route != operatorOverviewRoute || model.panels[operatorRepositoriesPanel].index != 1 {
		t.Fatalf("overview return state route=%q selection=%d", model.route, model.panels[operatorRepositoriesPanel].index)
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
	updated, refreshLaterPage := model.Update(keyMessage('r'))
	model = updated.(operatorModel)
	if refreshLaterPage == nil || model.runs.pending.cursor != "next" {
		t.Fatalf("later-page refresh lost cursor: %+v", model.runs.pending)
	}
	grownSecond := second
	grownSecond.Collection.Total = 3
	updated, _ = model.Update(operatorRunsResultMsg{generation: model.runs.generation, request: *model.runs.pending, page: grownSecond})
	model = updated.(operatorModel)
	if model.runs.page == nil || model.runs.page.Collection.Total != 3 || model.runs.staleError != nil {
		t.Fatalf("later-page growth became stale: %+v", model.runs)
	}
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
	updated, refreshLaterPage := model.Update(keyMessage('r'))
	model = updated.(operatorModel)
	if refreshLaterPage == nil || model.attention.pending.cursor != "next-attention" {
		t.Fatalf("later Attention refresh lost cursor: %+v", model.attention.pending)
	}
	grownSecond := second
	grownSecond.Collection.Total = 4
	updated, _ = model.Update(operatorAttentionResultMsg{generation: model.attention.generation, request: *model.attention.pending, page: grownSecond})
	model = updated.(operatorModel)
	if model.attention.page == nil || model.attention.page.Collection.Total != 4 || model.attention.staleError != nil {
		t.Fatalf("later Attention growth became stale: %+v", model.attention)
	}
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

func TestOperatorRepositoriesPagingDetailConfirmationReplayAndSuccess(t *testing.T) {
	model := loadedOperatorModel(80, 24)
	model.newRequestID = func() string { return "operator-enable-stable" }
	updated, load := model.Update(keyMessage('4'))
	model = updated.(operatorModel)
	if load == nil || model.route != operatorRepositoriesRoute || !model.repositories.refreshing {
		t.Fatalf("repositories route=%q state=%+v", model.route, model.repositories)
	}
	now := time.Date(2026, 9, 3, 2, 0, 0, 0, time.UTC)
	page := application.RoutineRepositoryPage{
		Metadata:   application.RoutineProjectionMetadata{ObservedAt: now},
		Collection: application.RoutineCollectionMetadata{Total: 26, Truncated: true, NextCursor: "repositories-next"},
		Repositories: []application.RoutineRepositorySummary{
			{Repository: "owner/one", LifecycleIntent: application.RepositoryEnabled, Readiness: domain.RepositoryReady, Available: true, Acceptance: application.RoutineRepositoryAcceptance{Conclusion: application.RoutineRepositoryAcceptingNewWork}},
			{Repository: "owner/two", LifecycleIntent: application.RepositoryDisabled, Readiness: domain.RepositoryReady, AvailabilityReasonCode: "repository_disabled", Acceptance: application.RoutineRepositoryAcceptance{Conclusion: application.RoutineRepositoryReadyDisabled, ReasonCode: "repository_disabled", NextDirection: application.RoutineRepositoryDirectionEnable}},
		},
	}
	updated, _ = model.Update(operatorRepositoriesResultMsg{generation: model.repositories.generation, request: *model.repositories.pending, page: page})
	model = updated.(operatorModel)
	updated, _ = model.Update(keyMessage('j'))
	model = updated.(operatorModel)
	updated, detailLoad := model.Update(keySpecial(tea.KeyEnter, 0))
	model = updated.(operatorModel)
	if detailLoad == nil || model.route != operatorRepositoryDetailRoute || model.repositoryDetail.repository != "owner/two" {
		t.Fatalf("repository detail=%+v route=%q", model.repositoryDetail, model.route)
	}
	detail := operatorFixtureRepositoryDetail("owner/two", application.RepositoryDisabled, false)
	updated, _ = model.Update(operatorRepositoryDetailResultMsg{generation: model.repositoryDetail.generation, repository: "owner/two", detail: detail})
	model = updated.(operatorModel)
	plain := ansi.Strip(model.render())
	for _, phrase := range []string{"Repository detail", "READY DISABLED", "repository disabled", "Readiness dimensions", "PROFILE CONFIGURATION", "VERIFIER POLICY", "Action available", "╭", "┌"} {
		if !strings.Contains(plain, phrase) {
			t.Fatalf("detail missing %q:\n%s", phrase, plain)
		}
	}
	if lipgloss.Width(model.render()) > 80 || lipgloss.Height(model.render()) > 24 {
		t.Fatalf("repository detail exceeded 80x24: %dx%d", lipgloss.Width(model.render()), lipgloss.Height(model.render()))
	}
	updated, _ = model.Update(keyMessage('e'))
	model = updated.(operatorModel)
	if model.repositoryDetail.operationStage != operatorRepositoryOperationConfirming || !strings.Contains(model.render(), "does not enable global automatic admission") {
		t.Fatalf("confirmation state=%+v\n%s", model.repositoryDetail, model.render())
	}
	updated, submit := model.Update(keySpecial(tea.KeyEnter, 0))
	model = updated.(operatorModel)
	if submit == nil || model.repositoryDetail.operationStage != operatorRepositoryOperationPending || model.repositoryDetail.requestID != "operator-enable-stable" {
		t.Fatalf("pending state=%+v", model.repositoryDetail)
	}
	generation := model.repositoryDetail.operationGeneration
	updated, duplicate := model.Update(keySpecial(tea.KeyEnter, 0))
	model = updated.(operatorModel)
	if duplicate != nil || model.repositoryDetail.operationGeneration != generation || model.repositoryDetail.requestID != "operator-enable-stable" {
		t.Fatalf("duplicate changed attempt=%+v", model.repositoryDetail)
	}
	updated, _ = model.Update(operatorRepositoryOperationResultMsg{generation: generation, repository: "owner/two", requestID: "operator-enable-stable", err: operatorSafeError{Category: application.ErrorUnavailable, Message: "result unavailable"}})
	model = updated.(operatorModel)
	if model.repositoryDetail.operationStage != operatorRepositoryOperationRetryable || !strings.Contains(ansi.Strip(model.render()), "Enable uncertain") {
		t.Fatalf("retryable state=%+v", model.repositoryDetail)
	}
	updated, retry := model.Update(keySpecial(tea.KeyEnter, 0))
	model = updated.(operatorModel)
	if retry == nil || model.repositoryDetail.requestID != "operator-enable-stable" || model.repositoryDetail.operationGeneration != generation+1 {
		t.Fatalf("retry state=%+v", model.repositoryDetail)
	}
	receipt := application.OperationReceipt{OperationType: application.OperationEnableRepository, Phase: application.OperationPhaseObserved, Outcome: application.OperationOutcomeSucceeded, ResultingState: "enabled", ResultingVersion: 2}
	updated, refresh := model.Update(operatorRepositoryOperationResultMsg{generation: generation + 1, repository: "owner/two", requestID: "operator-enable-stable", result: application.RepositoryMutationResult{Receipt: receipt}})
	model = updated.(operatorModel)
	if refresh == nil || model.repositoryDetail.operationStage != operatorRepositoryOperationSucceeded || model.repositoryDetail.receipt == nil {
		t.Fatalf("success state=%+v", model.repositoryDetail)
	}
	updated, _ = model.Update(operatorRepositoryDetailResultMsg{generation: model.repositoryDetail.generation, repository: "owner/two", err: operatorSafeError{Category: application.ErrorUnavailable, Message: "post-success refresh unavailable"}})
	model = updated.(operatorModel)
	if model.repositoryDetail.staleError == nil || model.repositoryDetail.receipt == nil || !strings.Contains(ansi.Strip(model.render()), "Enable OBSERVED/SUCCEEDED") {
		t.Fatalf("post-success stale state=%+v", model.repositoryDetail)
	}
	if help := model.renderHelp(); strings.Contains(help, "e enable") {
		t.Fatalf("post-success stale help advertises a blocked action: %s", help)
	}
	updated, refresh = model.Update(keyMessage('r'))
	model = updated.(operatorModel)
	if refresh == nil || !model.repositoryDetail.refreshing {
		t.Fatalf("post-success retry state=%+v", model.repositoryDetail)
	}
	enabled := operatorFixtureRepositoryDetail("owner/two", application.RepositoryEnabled, true)
	updated, _ = model.Update(operatorRepositoryDetailResultMsg{generation: model.repositoryDetail.generation, repository: "owner/two", detail: enabled})
	model = updated.(operatorModel)
	if output := ansi.Strip(model.render()); !strings.Contains(output, "Enable OBSERVED/SUCCEEDED · ENABLED v2") || strings.Contains(output, "Action available") {
		t.Fatalf("settled detail:\n%s", output)
	}
	updated, _ = model.Update(keySpecial(tea.KeyEscape, 0))
	model = updated.(operatorModel)
	if model.route != operatorRepositoriesRoute || model.repositories.index != 1 {
		t.Fatalf("repositories return state=%+v route=%q", model.repositories, model.route)
	}
	updated, next := model.Update(keyMessage('n'))
	model = updated.(operatorModel)
	if next == nil || model.repositories.pending.cursor != "repositories-next" || len(model.repositories.pending.previous) != 1 {
		t.Fatalf("next repositories request=%+v", model.repositories.pending)
	}
	second := page
	second.Collection = application.RoutineCollectionMetadata{Total: 26}
	second.Repositories = []application.RoutineRepositorySummary{{Repository: "owner/last", LifecycleIntent: application.RepositoryEnabled, Readiness: domain.RepositoryReady, Available: true, Acceptance: application.RoutineRepositoryAcceptance{Conclusion: application.RoutineRepositoryAcceptingNewWork}}}
	updated, _ = model.Update(operatorRepositoriesResultMsg{generation: model.repositories.generation, request: *model.repositories.pending, page: second})
	model = updated.(operatorModel)
	updated, previous := model.Update(keyMessage('p'))
	model = updated.(operatorModel)
	if previous == nil || model.repositories.pending.cursor != "" || len(model.repositories.pending.previous) != 0 {
		t.Fatalf("previous repositories request=%+v", model.repositories.pending)
	}
}

func TestOperatorRepositoryConfirmationBlocksRefreshAndConflictNeverClaimsSuccess(t *testing.T) {
	model := loadedOperatorModel(80, 24)
	model.route = operatorRepositoryDetailRoute
	detail := operatorFixtureRepositoryDetail("owner/repo", application.RepositoryDisabled, false)
	model.repositoryDetail = operatorRepositoryDetailState{repository: "owner/repo", detail: &detail, operationStage: operatorRepositoryOperationConfirming}
	updated, tick := model.Update(operatorRefreshTickMsg{})
	model = updated.(operatorModel)
	if tick == nil || model.repositoryDetail.refreshing {
		t.Fatalf("confirmation refresh state=%+v", model.repositoryDetail)
	}
	model.newRequestID = func() string { return "operator-enable-conflict" }
	updated, _ = model.Update(keySpecial(tea.KeyEnter, 0))
	model = updated.(operatorModel)
	updated, _ = model.Update(operatorRepositoryOperationResultMsg{generation: model.repositoryDetail.operationGeneration, repository: "owner/repo", requestID: "operator-enable-conflict", err: operatorSafeError{Category: application.ErrorConflict, Message: "repository authority changed"}})
	model = updated.(operatorModel)
	output := strings.ToLower(ansi.Strip(model.render()))
	if model.repositoryDetail.operationStage != operatorRepositoryOperationFailed || strings.Contains(output, "enable result observed/succeeded") || !strings.Contains(output, "repository authority changed") {
		t.Fatalf("conflict state=%+v\n%s", model.repositoryDetail, output)
	}
}

func TestOperatorRepositoryLateRouteAndConfirmationResultsAreFenced(t *testing.T) {
	model := loadedOperatorModel(80, 24)
	updated, _ := model.Update(keyMessage('4'))
	model = updated.(operatorModel)
	lateGeneration := model.repositories.generation
	updated, _ = model.Update(keyMessage('1'))
	model = updated.(operatorModel)
	latePage := application.RoutineRepositoryPage{Repositories: []application.RoutineRepositorySummary{{Repository: "owner/late"}}}
	updated, _ = model.Update(operatorRepositoriesResultMsg{generation: lateGeneration, request: operatorRepositoriesRequest{}, page: latePage})
	model = updated.(operatorModel)
	if model.repositories.page != nil || model.repositories.refreshing {
		t.Fatalf("late collection replaced newer route: %+v", model.repositories)
	}

	detail := operatorFixtureRepositoryDetail("owner/repo", application.RepositoryDisabled, false)
	model.route = operatorRepositoryDetailRoute
	model.repositoryDetail = operatorRepositoryDetailState{repository: "owner/repo", detail: &detail, refreshing: true, generation: 7, operationStage: operatorRepositoryOperationIdle}
	updated, _ = model.Update(keyMessage('e'))
	model = updated.(operatorModel)
	lateDetail := operatorFixtureRepositoryDetail("owner/repo", application.RepositoryEnabled, true)
	updated, _ = model.Update(operatorRepositoryDetailResultMsg{generation: 7, repository: "owner/repo", detail: lateDetail})
	model = updated.(operatorModel)
	if model.repositoryDetail.operationStage != operatorRepositoryOperationConfirming || model.repositoryDetail.detail.Repository.LifecycleIntent != application.RepositoryDisabled {
		t.Fatalf("late detail replaced confirmation: %+v", model.repositoryDetail)
	}

	model.repositoryDetail.operationStage = operatorRepositoryOperationPending
	model.repositoryDetail.operationGeneration = 4
	model.repositoryDetail.requestID = "current-request"
	updated, _ = model.Update(operatorRepositoryOperationResultMsg{generation: 3, repository: "owner/repo", requestID: "old-request", result: application.RepositoryMutationResult{Receipt: application.OperationReceipt{Outcome: application.OperationOutcomeSucceeded}}})
	model = updated.(operatorModel)
	if model.repositoryDetail.operationStage != operatorRepositoryOperationPending || model.repositoryDetail.receipt != nil {
		t.Fatalf("late mutation replaced current operation: %+v", model.repositoryDetail)
	}
}

func operatorFixtureRepositoryDetail(repository string, intent application.RepositoryLifecycleIntent, available bool) application.RoutineRepositoryDetail {
	now := time.Date(2026, 9, 3, 2, 0, 0, 0, time.UTC)
	conclusion := application.RoutineRepositoryReadyDisabled
	direction := application.RoutineRepositoryDirectionEnable
	actions := []application.RoutineRepositoryAction{application.RoutineRepositoryActionEnable}
	reason := "repository_disabled"
	if intent == application.RepositoryEnabled {
		conclusion, direction, actions, reason = application.RoutineRepositoryAcceptingNewWork, application.RoutineRepositoryDirectionNone, []application.RoutineRepositoryAction{}, "available"
	}
	dimensions := make([]application.RoutineRepositoryDimension, 0, len(domain.RepositoryReadinessDimensions))
	for _, dimension := range domain.RepositoryReadinessDimensions {
		dimensions = append(dimensions, application.RoutineRepositoryDimension{Dimension: dimension, Status: domain.RepositoryReady, ReasonCode: "ready", ObservedAt: now})
	}
	return application.RoutineRepositoryDetail{
		Metadata:   application.RoutineProjectionMetadata{ObservedAt: now},
		Repository: application.RoutineRepositorySummary{Repository: repository, LifecycleIntent: intent, Readiness: domain.RepositoryReady, ReadinessReasonCode: "ready", Available: available, AvailabilityReasonCode: reason, ConfigurationConvergence: domain.RepositoryReady, ConfigurationReasonCode: "ready", LastObservedAt: now, Acceptance: application.RoutineRepositoryAcceptance{Conclusion: conclusion, ReasonCode: reason, NextDirection: direction}},
		Dimensions: dimensions, LegalNextActions: actions,
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

func TestProductionOperatorLoaderEnablesReadyDisabledRepositoryThroughReceiptService(t *testing.T) {
	root := resolvedTempDir(t)
	configPath, databasePath := writeCurrentManagedDraftConfig(t, root)
	loaded, err := loadManagedConfiguration(configPath)
	if err != nil {
		t.Fatal(err)
	}
	bindings := loaded.Registry.Bindings()
	if len(bindings) != 1 {
		t.Fatalf("repository bindings=%d", len(bindings))
	}
	profile, found, err := loaded.Registry.RepositoryProfile(context.Background(), bindings[0].CanonicalRepository)
	if err != nil || !found {
		t.Fatalf("profile found=%t err=%v", found, err)
	}
	store, err := sqlitestore.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	_ = testExistingNewAdmissionGate(t, store)
	now := time.Date(2026, 9, 3, 3, 0, 0, 0, time.UTC)
	if err := store.AdoptRepositoryLifecycleBaseline(context.Background(), application.RepositoryBaselineInput{Profiles: []application.RepositoryProfileAuthority{profile}, AdoptedAt: now}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	authority, err := store.RepositoryOperationAuthority(context.Background(), profile.Authority.Repository)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	requesterIdentity := profile.Authority.TrustedOperators[0]
	recheckReceipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationRecheckRepository, Scope: application.ScopeRepository, TargetID: profile.Authority.Repository, Requester: requesterIdentity, RequestDigest: strings.Repeat("1", 64), ExpectedAuthorityDigest: strings.Repeat("2", 64), OperationAnchorDigest: application.ConfigurationEvidenceDigest("operator-loader-recheck", profile.Authority.Repository), TargetBindingDigest: profile.Authority.BindingDigest, AcceptedAt: now.Add(time.Second)})
	if _, _, err := store.BeginOperationReceipt(context.Background(), recheckReceipt); err != nil {
		store.Close()
		t.Fatal(err)
	}
	start := application.RepositoryRecheckStart{AttemptID: "repository-recheck-" + recheckReceipt.OperationID, OperationID: recheckReceipt.OperationID, Expected: authority, Profile: profile.Profile, StartedAt: now.Add(2 * time.Second)}
	if _, created, err := store.BeginRepositoryRecheck(context.Background(), start); err != nil || !created {
		store.Close()
		t.Fatalf("recheck created=%t err=%v", created, err)
	}
	results := make([]domain.RepositoryDimensionResult, 0, len(domain.RepositoryReadinessDimensions))
	for _, dimension := range domain.RepositoryReadinessDimensions {
		result := domain.RepositoryDimensionResult{Dimension: dimension, Status: domain.RepositoryReady, ReasonCode: "ready", EvidenceDigest: application.ConfigurationEvidenceDigest("operator-loader-ready", string(dimension)), ObservedAt: now.Add(3 * time.Second)}
		results = append(results, result)
		if err := store.SaveRepositoryRecheckObservation(context.Background(), start.AttemptID, result); err != nil {
			store.Close()
			t.Fatal(err)
		}
	}
	if _, _, err := store.PublishRepositoryRecheck(context.Background(), application.RepositoryRecheckPublication{AttemptID: start.AttemptID, OperationID: recheckReceipt.OperationID, Expected: authority, Profile: profile.Profile, Results: results, PublishedAt: now.Add(4 * time.Second)}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	authorizer, _ := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: loaded.Controller.Operator})
	repositoryService, _ := application.NewRepositoryService(store, authorizer, loaded.Registry, application.RepositoryObservers{})
	requester := application.Requester{ID: requesterIdentity.Login, Kind: "github_login", DatabaseID: requesterIdentity.DatabaseID, NodeID: requesterIdentity.NodeID, ActorType: requesterIdentity.ActorType}
	if result, err := repositoryService.Disable(context.Background(), application.RepositoryMutationCommand{Requester: requester, Repository: profile.Authority.Repository, RequestID: "operator-loader-disable"}); err != nil || result.Repository.Lifecycle.Intent != application.RepositoryDisabled {
		store.Close()
		t.Fatalf("disable result=%+v err=%v", result, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	composition, err := composeOperator(context.Background(), configPath)
	if err != nil {
		t.Fatal(err)
	}
	defer composition.Close()
	observedAt := now.Add(10 * time.Second)
	before, err := composition.loader.LoadOverview(context.Background(), observedAt)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := composition.loader.LoadRepositoryDetail(context.Background(), profile.Authority.Repository, observedAt)
	if err != nil || len(detail.LegalNextActions) != 1 || detail.LegalNextActions[0] != application.RoutineRepositoryActionEnable || detail.Repository.Acceptance.Conclusion != application.RoutineRepositoryReadyDisabled {
		t.Fatalf("ready-disabled detail=%+v err=%v", detail, err)
	}
	rotatedRoot := resolvedTempDir(t)
	rotatedConfigPath, _ := writeCurrentManagedDraftConfig(t, rotatedRoot)
	rotatedPayload, err := os.ReadFile(rotatedConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var rotatedConfig map[string]any
	if err := json.Unmarshal(rotatedPayload, &rotatedConfig); err != nil {
		t.Fatal(err)
	}
	rotatedConfig["controller"].(map[string]any)["operator"] = map[string]any{"database_id": 44, "node_id": "MDQ6VXNlcjQ0", "login": "future", "type": "User"}
	for _, rawRepository := range rotatedConfig["repositories"].([]any) {
		policy := rawRepository.(map[string]any)["operator_identity_policy"].(map[string]any)
		policy["allowed_logins"] = []any{"ifan0927", "future"}
		policy["trusted_actors"] = []any{
			map[string]any{"database_id": 33, "node_id": "MDQ6VXNlcjMz", "login": "ifan0927", "type": "User"},
			map[string]any{"database_id": 44, "node_id": "MDQ6VXNlcjQ0", "login": "future", "type": "User"},
		}
	}
	rotatedPayload, err = json.Marshal(rotatedConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rotatedConfigPath, rotatedPayload, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadManagedConfiguration(rotatedConfigPath); err != nil {
		t.Fatalf("initialize rotated authority: %v", err)
	}
	production := composition.loader.(*productionOperatorLoader)
	production.configPath = rotatedConfigPath
	if _, err := composition.loader.EnableRepository(context.Background(), profile.Authority.Repository, "operator-loader-revoked"); err == nil {
		t.Fatal("long-lived operator retained revoked mutation authority")
	}
	production.configPath = configPath
	unchanged, err := composition.loader.LoadRepositoryDetail(context.Background(), profile.Authority.Repository, observedAt)
	if err != nil || unchanged.Repository.LifecycleIntent != application.RepositoryDisabled {
		t.Fatalf("revoked operator changed lifecycle detail=%+v err=%v", unchanged, err)
	}
	staleAuthority, err := composition.store.RepositoryOperationAuthority(context.Background(), profile.Authority.Repository)
	if err != nil {
		t.Fatal(err)
	}
	staleRecheckReceipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationRecheckRepository, Scope: application.ScopeRepository, TargetID: profile.Authority.Repository, Requester: requesterIdentity, RequestDigest: strings.Repeat("3", 64), ExpectedAuthorityDigest: strings.Repeat("4", 64), OperationAnchorDigest: application.ConfigurationEvidenceDigest("operator-loader-stale-recheck", profile.Authority.Repository), TargetBindingDigest: profile.Authority.BindingDigest, AcceptedAt: observedAt.Add(time.Second)})
	if _, _, err := composition.store.BeginOperationReceipt(context.Background(), staleRecheckReceipt); err != nil {
		t.Fatal(err)
	}
	staleRecheck := application.RepositoryRecheckStart{AttemptID: "repository-recheck-" + staleRecheckReceipt.OperationID, OperationID: staleRecheckReceipt.OperationID, Expected: staleAuthority, Profile: profile.Profile, StartedAt: observedAt.Add(2 * time.Second)}
	if _, created, err := composition.store.BeginRepositoryRecheck(context.Background(), staleRecheck); err != nil || !created {
		t.Fatalf("stale recheck created=%t err=%v", created, err)
	}
	if _, staleErr := composition.loader.EnableRepository(context.Background(), profile.Authority.Repository, "operator-loader-stale-enable"); staleErr == nil {
		t.Fatal("stale advertised enablement succeeded")
	} else {
		var safe *application.ServiceError
		if !errors.As(staleErr, &safe) || safe.Category != application.ErrorConflict {
			t.Fatalf("stale enable error=%v", staleErr)
		}
	}
	stillDisabled, err := composition.loader.LoadRepositoryDetail(context.Background(), profile.Authority.Repository, observedAt.Add(3*time.Second))
	if err != nil || stillDisabled.Repository.LifecycleIntent != application.RepositoryDisabled || stillDisabled.Repository.Acceptance.Conclusion != application.RoutineRepositoryUnavailable {
		t.Fatalf("stale enable changed lifecycle detail=%+v err=%v", stillDisabled, err)
	}
	if err := composition.store.SettleRepositoryRecheckFailure(context.Background(), application.RepositoryRecheckFailure{AttemptID: staleRecheck.AttemptID, OperationID: staleRecheck.OperationID, Outcome: application.OperationOutcomeFailed, ReasonCode: "fixture_recheck_cancelled", SettledAt: observedAt.Add(4 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	refreshedAuthority, err := composition.store.RepositoryOperationAuthority(context.Background(), profile.Authority.Repository)
	if err != nil {
		t.Fatal(err)
	}
	refreshReceipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationRecheckRepository, Scope: application.ScopeRepository, TargetID: profile.Authority.Repository, Requester: requesterIdentity, RequestDigest: strings.Repeat("5", 64), ExpectedAuthorityDigest: strings.Repeat("6", 64), OperationAnchorDigest: application.ConfigurationEvidenceDigest("operator-loader-refresh-recheck", profile.Authority.Repository), TargetBindingDigest: profile.Authority.BindingDigest, AcceptedAt: observedAt.Add(5 * time.Second)})
	if _, _, err := composition.store.BeginOperationReceipt(context.Background(), refreshReceipt); err != nil {
		t.Fatal(err)
	}
	refreshRecheck := application.RepositoryRecheckStart{AttemptID: "repository-recheck-" + refreshReceipt.OperationID, OperationID: refreshReceipt.OperationID, Expected: refreshedAuthority, Profile: profile.Profile, StartedAt: observedAt.Add(6 * time.Second)}
	if _, created, err := composition.store.BeginRepositoryRecheck(context.Background(), refreshRecheck); err != nil || !created {
		t.Fatalf("refresh recheck created=%t err=%v", created, err)
	}
	refreshResults := make([]domain.RepositoryDimensionResult, 0, len(domain.RepositoryReadinessDimensions))
	for _, dimension := range domain.RepositoryReadinessDimensions {
		ready := domain.RepositoryDimensionResult{Dimension: dimension, Status: domain.RepositoryReady, ReasonCode: "ready", EvidenceDigest: application.ConfigurationEvidenceDigest("operator-loader-refreshed-ready", string(dimension)), ObservedAt: observedAt.Add(7 * time.Second)}
		refreshResults = append(refreshResults, ready)
		if err := composition.store.SaveRepositoryRecheckObservation(context.Background(), refreshRecheck.AttemptID, ready); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := composition.store.PublishRepositoryRecheck(context.Background(), application.RepositoryRecheckPublication{AttemptID: refreshRecheck.AttemptID, OperationID: refreshReceipt.OperationID, Expected: refreshedAuthority, Profile: profile.Profile, Results: refreshResults, PublishedAt: observedAt.Add(8 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	result, err := composition.loader.EnableRepository(context.Background(), profile.Authority.Repository, "operator-loader-enable")
	if err != nil || result.Receipt.Phase != application.OperationPhaseObserved || result.Receipt.Outcome != application.OperationOutcomeSucceeded || result.Receipt.OperationType != application.OperationEnableRepository || result.Receipt.EvidenceDigest == "" {
		t.Fatalf("enable result=%+v err=%v", result, err)
	}
	activityService, err := application.NewActivityQueryService(composition.store, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	activity, err := activityService.List(context.Background(), application.ActivityListQuery{Requester: requester, Filter: application.ActivityFilter{Category: application.ActivityRepository, Scope: application.ScopeRepository, TargetID: profile.Authority.Repository}}, observedAt)
	if err != nil || len(activity.Events) == 0 {
		t.Fatalf("repository activity=%+v err=%v", activity, err)
	}
	foundEnableActivity := false
	for _, event := range activity.Events {
		if event.EventKind == application.ActivityRepositoryLifecycleChange && event.ResultingState == string(application.RepositoryEnabled) && len(event.OperationIDs) == 1 && event.OperationIDs[0] == result.Receipt.OperationID {
			foundEnableActivity = true
		}
	}
	if !foundEnableActivity {
		t.Fatalf("enable activity missing: %+v", activity.Events)
	}
	replayed, err := composition.loader.EnableRepository(context.Background(), profile.Authority.Repository, "operator-loader-enable")
	if err != nil || replayed.Receipt.OperationID != result.Receipt.OperationID {
		t.Fatalf("replay result=%+v err=%v", replayed, err)
	}
	settled, err := composition.loader.LoadRepositoryDetail(context.Background(), profile.Authority.Repository, observedAt.Add(time.Second))
	if err != nil || settled.Repository.LifecycleIntent != application.RepositoryEnabled || !settled.Repository.Available || settled.Repository.Acceptance.Conclusion != application.RoutineRepositoryAcceptingNewWork || len(settled.LegalNextActions) != 0 {
		t.Fatalf("enabled detail=%+v err=%v", settled, err)
	}
	after, err := composition.loader.LoadOverview(context.Background(), observedAt.Add(time.Second))
	if err != nil || before.Overview.Worker.Liveness != after.Overview.Worker.Liveness || before.Overview.Worker.Activity != after.Overview.Worker.Activity || before.Overview.Worker.Reason != after.Overview.Worker.Reason {
		t.Fatalf("worker changed before=%+v after=%+v err=%v", before.Overview.Worker, after.Overview.Worker, err)
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
