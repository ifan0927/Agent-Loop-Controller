package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
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

func (inertOperatorLoader) Load(context.Context, time.Time) (operatorOverviewBatch, error) {
	return operatorOverviewBatch{}, errors.New("not used")
}

func TestProductionOperatorOverviewLoaderUsesOneObservedTimeAndBoundedRepositoryPage(t *testing.T) {
	observedAt := time.Date(2026, 9, 1, 2, 3, 4, 0, time.FixedZone("fixture", 8*60*60))
	overview := &recordingOverviewSource{projection: application.RoutineOverviewProjection{Readiness: application.AggregateReady}}
	repositories := &recordingRepositorySource{page: application.RoutineRepositoryPage{Collection: application.RoutineCollectionMetadata{Total: 101, Truncated: true}}}
	loader := productionOperatorOverviewLoader{overview: overview, repositories: repositories}

	batch, err := loader.Load(context.Background(), observedAt)
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
	if partial, err := loader.Load(context.Background(), observedAt); err == nil || partial.Overview.Readiness != "" {
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
	if !strings.Contains(tooSmall, "Terminal too small") || !strings.Contains(tooSmall, "79x24") || strings.Contains(tooSmall, "Attention") {
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
	for _, phrase := range []string{"No registered repositories", "No active or recent runs", "No operator attention"} {
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

func TestSafeOperatorErrorOnlyExposesServiceErrors(t *testing.T) {
	service := safeOperatorError(&application.ServiceError{Category: application.ErrorConflict, Message: "safe conflict"})
	if service.String() != "conflict: safe conflict" {
		t.Fatalf("service error=%q", service.String())
	}
	unsafe := safeOperatorError(errors.New("/private/path secret-token"))
	if unsafe.Category != application.ErrorInternal || unsafe.Message != "operator overview is unavailable" {
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

func TestComposeOperatorOverviewReadsBoundAuthorityWithoutMutation(t *testing.T) {
	root := resolvedTempDir(t)
	configPath, databasePath := writeCurrentManagedDraftConfig(t, root)
	loaded, err := loadManagedConfiguration(configPath)
	if err != nil {
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

	composition, err := composeOperatorOverview(context.Background(), configPath)
	if err != nil {
		t.Fatal(err)
	}
	observedAt := time.Date(2026, 9, 1, 4, 5, 6, 0, time.UTC)
	batch, loadErr := composition.loader.Load(context.Background(), observedAt)
	closeErr := composition.Close()
	if loadErr != nil || closeErr != nil {
		t.Fatalf("load=%v close=%v", loadErr, closeErr)
	}
	if batch.ObservedAt != observedAt || batch.Overview.Metadata.ObservedAt != observedAt || batch.Repositories.Metadata.ObservedAt != observedAt {
		t.Fatalf("observed times batch=%s overview=%s repositories=%s", batch.ObservedAt, batch.Overview.Metadata.ObservedAt, batch.Repositories.Metadata.ObservedAt)
	}
	if batch.Overview.Settings.DesiredDigest != loaded.Digest || batch.Repositories.Collection.Total < len(batch.Repositories.Repositories) {
		t.Fatalf("overview digest=%q repositories=%d", batch.Overview.Settings.DesiredDigest, batch.Repositories.Collection.Total)
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
