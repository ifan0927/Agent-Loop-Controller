package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

const (
	operatorMinimumWidth    = 80
	operatorWideWidth       = 92
	operatorMinimumHeight   = 24
	operatorRefreshInterval = 10 * time.Second
)

type operatorPanel string

const (
	operatorRepositoriesPanel operatorPanel = "repositories"
	operatorRunsPanel         operatorPanel = "runs"
	operatorAttentionPanel    operatorPanel = "attention"
)

type operatorRoute string

const (
	operatorOverviewRoute         operatorRoute = "overview"
	operatorRunsRoute             operatorRoute = "runs"
	operatorAttentionRoute        operatorRoute = "attention"
	operatorRepositoriesRoute     operatorRoute = "repositories"
	operatorRunDetailRoute        operatorRoute = "run_detail"
	operatorRepositoryDetailRoute operatorRoute = "repository_detail"
)

type operatorPanelState struct {
	index int
}

type operatorSafeError struct {
	Category application.ErrorCategory
	Message  string
}

func (e operatorSafeError) String() string {
	if e.Category == "" {
		return e.Message
	}
	return string(e.Category) + ": " + e.Message
}

type operatorRefreshResultMsg struct {
	generation int64
	batch      operatorOverviewBatch
	err        operatorSafeError
}

type operatorRunsRequest struct {
	lifecycle  application.RunLifecycleFilter
	repository string
	cursor     string
	previous   []string
}

type operatorRunsResultMsg struct {
	generation int64
	request    operatorRunsRequest
	page       application.RoutineRunPage
	err        operatorSafeError
}

type operatorRunDetailResultMsg struct {
	generation int64
	runID      string
	detail     application.RoutineRunDetail
	err        operatorSafeError
}

type operatorAttentionRequest struct {
	cursor   string
	previous []string
}

type operatorAttentionResultMsg struct {
	generation int64
	request    operatorAttentionRequest
	page       application.RoutineAttentionPage
	err        operatorSafeError
}

type operatorAttentionFocus string

const (
	operatorAttentionListFocus    operatorAttentionFocus = "list"
	operatorAttentionSummaryFocus operatorAttentionFocus = "summary"
)

type operatorAttentionState struct {
	page            *application.RoutineAttentionPage
	request         operatorAttentionRequest
	pending         *operatorAttentionRequest
	initialError    *operatorSafeError
	staleError      *operatorSafeError
	refreshing      bool
	generation      int64
	index           int
	selectedEventID string
	focus           operatorAttentionFocus
	summaryOffset   int
}

type operatorRunsState struct {
	page              *application.RoutineRunPage
	request           operatorRunsRequest
	pending           *operatorRunsRequest
	initialError      *operatorSafeError
	staleError        *operatorSafeError
	refreshing        bool
	generation        int64
	index             int
	selectedRunID     string
	repositoryEditing bool
	repositoryInput   string
}

type operatorRunDetailState struct {
	detail       *application.RoutineRunDetail
	runID        string
	returnRoute  operatorRoute
	initialError *operatorSafeError
	staleError   *operatorSafeError
	refreshing   bool
	generation   int64
	gateIndex    int
}

type operatorRefreshTickMsg struct{}

type operatorKeyMap struct {
	up, down, next, previous, refresh, help, quit                                                              key.Binding
	overview, runs, attention, repositories, open, back, lifecycle, repository, enable, nextPage, previousPage key.Binding
}

func (k operatorKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.overview, k.runs, k.attention, k.repositories, k.next, k.previous, k.up, k.down, k.refresh, k.help, k.quit}
}

func (k operatorKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{{k.overview, k.runs, k.attention, k.repositories}, {k.next, k.previous}, {k.up, k.down}, {k.open, k.back, k.enable}, {k.lifecycle, k.repository}, {k.nextPage, k.previousPage}, {k.refresh, k.help, k.quit}}
}

var operatorKeys = operatorKeyMap{
	up:           key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
	down:         key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
	next:         key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "next panel")),
	previous:     key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("shift+tab", "previous panel")),
	refresh:      key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
	help:         key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
	quit:         key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	overview:     key.NewBinding(key.WithKeys("1"), key.WithHelp("1", "overview")),
	runs:         key.NewBinding(key.WithKeys("2"), key.WithHelp("2", "runs")),
	attention:    key.NewBinding(key.WithKeys("3"), key.WithHelp("3", "attention")),
	repositories: key.NewBinding(key.WithKeys("4"), key.WithHelp("4", "repositories")),
	open:         key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open run")),
	back:         key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
	lifecycle:    key.NewBinding(key.WithKeys("f"), key.WithHelp("f", "lifecycle filter")),
	repository:   key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "repository filter")),
	enable:       key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "enable repository")),
	nextPage:     key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "next page")),
	previousPage: key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "previous page")),
}

type operatorModel struct {
	ctx              context.Context
	cancel           context.CancelFunc
	loader           operatorLoader
	now              func() time.Time
	newRequestID     func() string
	refreshInterval  time.Duration
	width            int
	height           int
	batch            *operatorOverviewBatch
	initialError     *operatorSafeError
	staleError       *operatorSafeError
	refreshing       bool
	generation       int64
	tickerStarted    bool
	help             help.Model
	focus            operatorPanel
	panels           map[operatorPanel]operatorPanelState
	route            operatorRoute
	runs             operatorRunsState
	attention        operatorAttentionState
	detail           operatorRunDetailState
	repositories     operatorRepositoriesState
	repositoryDetail operatorRepositoryDetailState
}

func newOperatorModel(parent context.Context, loader operatorLoader) operatorModel {
	ctx, cancel := context.WithCancel(parent)
	helpModel := help.New()
	return operatorModel{
		ctx:             ctx,
		cancel:          cancel,
		loader:          loader,
		now:             func() time.Time { return time.Now().UTC() },
		newRequestID:    newOperatorRequestID,
		refreshInterval: operatorRefreshInterval,
		refreshing:      true,
		generation:      1,
		help:            helpModel,
		focus:           "",
		panels: map[operatorPanel]operatorPanelState{
			operatorRepositoriesPanel: {},
			operatorRunsPanel:         {},
			operatorAttentionPanel:    {},
		},
		route:            operatorOverviewRoute,
		runs:             operatorRunsState{request: operatorRunsRequest{lifecycle: application.RunLifecycleActive}},
		attention:        operatorAttentionState{focus: operatorAttentionListFocus},
		repositoryDetail: operatorRepositoryDetailState{operationStage: operatorRepositoryOperationIdle},
	}
}

func (m operatorModel) Init() tea.Cmd {
	return m.overviewLoadCommand(m.generation)
}

func (m operatorModel) overviewLoadCommand(generation int64) tea.Cmd {
	ctx, loader, observedAt := m.ctx, m.loader, m.now().UTC()
	return func() tea.Msg {
		batch, err := loader.LoadOverview(ctx, observedAt)
		if err != nil {
			return operatorRefreshResultMsg{generation: generation, err: safeOperatorError(err)}
		}
		return operatorRefreshResultMsg{generation: generation, batch: batch}
	}
}

func (m operatorModel) runsLoadCommand(generation int64, request operatorRunsRequest) tea.Cmd {
	ctx, loader, observedAt := m.ctx, m.loader, m.now().UTC()
	request.previous = append([]string(nil), request.previous...)
	return func() tea.Msg {
		page, err := loader.LoadRuns(ctx, request.lifecycle, request.repository, request.cursor, observedAt)
		if err != nil {
			return operatorRunsResultMsg{generation: generation, request: request, err: safeOperatorError(err)}
		}
		return operatorRunsResultMsg{generation: generation, request: request, page: page}
	}
}

func (m operatorModel) detailLoadCommand(generation int64, runID string) tea.Cmd {
	ctx, loader, observedAt := m.ctx, m.loader, m.now().UTC()
	return func() tea.Msg {
		detail, err := loader.LoadRunDetail(ctx, runID, observedAt)
		if err != nil {
			return operatorRunDetailResultMsg{generation: generation, runID: runID, err: safeOperatorError(err)}
		}
		return operatorRunDetailResultMsg{generation: generation, runID: runID, detail: detail}
	}
}

func (m operatorModel) attentionLoadCommand(generation int64, request operatorAttentionRequest) tea.Cmd {
	ctx, loader, observedAt := m.ctx, m.loader, m.now().UTC()
	request.previous = append([]string(nil), request.previous...)
	return func() tea.Msg {
		page, err := loader.LoadAttention(ctx, request.cursor, observedAt)
		if err != nil {
			return operatorAttentionResultMsg{generation: generation, request: request, err: safeOperatorError(err)}
		}
		return operatorAttentionResultMsg{generation: generation, request: request, page: page}
	}
}

func (m operatorModel) repositoriesLoadCommand(generation int64, request operatorRepositoriesRequest) tea.Cmd {
	ctx, loader, observedAt := m.ctx, m.loader, m.now().UTC()
	request.previous = append([]string(nil), request.previous...)
	return func() tea.Msg {
		page, err := loader.LoadRepositories(ctx, request.cursor, observedAt)
		if err != nil {
			return operatorRepositoriesResultMsg{generation: generation, request: request, err: safeOperatorError(err)}
		}
		return operatorRepositoriesResultMsg{generation: generation, request: request, page: page}
	}
}

func (m operatorModel) repositoryDetailLoadCommand(generation int64, repository string) tea.Cmd {
	ctx, loader, observedAt := m.ctx, m.loader, m.now().UTC()
	return func() tea.Msg {
		detail, err := loader.LoadRepositoryDetail(ctx, repository, observedAt)
		if err != nil {
			return operatorRepositoryDetailResultMsg{generation: generation, repository: repository, err: safeOperatorError(err)}
		}
		return operatorRepositoryDetailResultMsg{generation: generation, repository: repository, detail: detail}
	}
}

func (m operatorModel) repositoryEnableCommand(generation int64, repository, requestID string) tea.Cmd {
	ctx, loader := m.ctx, m.loader
	return func() tea.Msg {
		result, err := loader.EnableRepository(ctx, repository, requestID)
		if err != nil {
			return operatorRepositoryOperationResultMsg{generation: generation, repository: repository, requestID: requestID, err: safeOperatorError(err)}
		}
		return operatorRepositoryOperationResultMsg{generation: generation, repository: repository, requestID: requestID, result: result}
	}
}

func (m operatorModel) tickCommand() tea.Cmd {
	interval := m.refreshInterval
	return tea.Tick(interval, func(time.Time) tea.Msg { return operatorRefreshTickMsg{} })
}

func (m operatorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if msg.Width < operatorMinimumWidth || msg.Height < operatorMinimumHeight {
			m.runs.repositoryEditing = false
		}
		m.help.SetWidth(max(msg.Width-2, 1))
		return m, nil
	case operatorRefreshResultMsg:
		if msg.generation != m.generation {
			return m, nil
		}
		m.refreshing = false
		if msg.err.Message != "" {
			if m.batch == nil {
				m.initialError = &msg.err
			} else {
				m.staleError = &msg.err
			}
			return m, nil
		}
		previous := m.selectedIdentities()
		batch := msg.batch
		m.batch = &batch
		m.initialError, m.staleError = nil, nil
		m.restoreSelections(previous)
		m.normalizeFocus()
		if !m.tickerStarted {
			m.tickerStarted = true
			return m, m.tickCommand()
		}
		return m, nil
	case operatorRunsResultMsg:
		if msg.generation != m.runs.generation {
			return m, nil
		}
		m.runs.refreshing, m.runs.pending = false, nil
		if msg.err.Message != "" {
			if m.runs.page == nil {
				m.runs.request = msg.request
				m.runs.initialError = &msg.err
			} else {
				m.runs.staleError = &msg.err
			}
			return m, nil
		}
		selected := m.selectedRunsID()
		page := msg.page
		m.runs.page, m.runs.request = &page, msg.request
		m.runs.initialError, m.runs.staleError = nil, nil
		m.restoreRunsSelection(selected)
		if !m.tickerStarted {
			m.tickerStarted = true
			return m, m.tickCommand()
		}
		return m, nil
	case operatorRunDetailResultMsg:
		if msg.generation != m.detail.generation || msg.runID != m.detail.runID {
			return m, nil
		}
		m.detail.refreshing = false
		if msg.err.Message != "" {
			if m.detail.detail == nil {
				m.detail.initialError = &msg.err
			} else {
				m.detail.staleError = &msg.err
			}
			return m, nil
		}
		detail := msg.detail
		m.detail.detail = &detail
		m.detail.initialError, m.detail.staleError = nil, nil
		m.detail.gateIndex = clamp(m.detail.gateIndex, 0, max(len(detail.Gates)-1, 0))
		if !m.tickerStarted {
			m.tickerStarted = true
			return m, m.tickCommand()
		}
		return m, nil
	case operatorAttentionResultMsg:
		if msg.generation != m.attention.generation {
			return m, nil
		}
		m.attention.refreshing, m.attention.pending = false, nil
		if msg.err.Message != "" {
			if m.attention.page == nil {
				m.attention.request = msg.request
				m.attention.initialError = &msg.err
			} else {
				m.attention.staleError = &msg.err
			}
			return m, nil
		}
		selected := m.selectedAttentionEventID()
		page := msg.page
		m.attention.page, m.attention.request = &page, msg.request
		m.attention.initialError, m.attention.staleError = nil, nil
		m.restoreAttentionSelection(selected)
		if !m.tickerStarted {
			m.tickerStarted = true
			return m, m.tickCommand()
		}
		return m, nil
	case operatorRepositoriesResultMsg:
		if m.route != operatorRepositoriesRoute || msg.generation != m.repositories.generation {
			return m, nil
		}
		m.repositories.refreshing, m.repositories.pending = false, nil
		if msg.err.Message != "" {
			if m.repositories.page == nil {
				m.repositories.request = msg.request
				m.repositories.initialError = &msg.err
			} else {
				m.repositories.staleError = &msg.err
			}
			return m, nil
		}
		selected := m.selectedRepository()
		page := msg.page
		m.repositories.page, m.repositories.request = &page, msg.request
		m.repositories.initialError, m.repositories.staleError = nil, nil
		m.restoreRepositorySelection(selected)
		if !m.tickerStarted {
			m.tickerStarted = true
			return m, m.tickCommand()
		}
		return m, nil
	case operatorRepositoryDetailResultMsg:
		if m.route != operatorRepositoryDetailRoute || msg.generation != m.repositoryDetail.generation || msg.repository != m.repositoryDetail.repository {
			return m, nil
		}
		m.repositoryDetail.refreshing = false
		if msg.err.Message != "" {
			if m.repositoryDetail.detail == nil {
				m.repositoryDetail.initialError = &msg.err
			} else {
				m.repositoryDetail.staleError = &msg.err
			}
			return m, nil
		}
		detail := msg.detail
		m.repositoryDetail.detail = &detail
		m.repositoryDetail.initialError, m.repositoryDetail.staleError = nil, nil
		m.repositoryDetail.dimensionIndex = clamp(m.repositoryDetail.dimensionIndex, 0, max(len(detail.Dimensions)-1, 0))
		if !m.tickerStarted {
			m.tickerStarted = true
			return m, m.tickCommand()
		}
		return m, nil
	case operatorRepositoryOperationResultMsg:
		state := &m.repositoryDetail
		if m.route != operatorRepositoryDetailRoute || state.operationStage != operatorRepositoryOperationPending || msg.generation != state.operationGeneration || msg.repository != state.repository || msg.requestID != state.requestID {
			return m, nil
		}
		if msg.err.Message != "" {
			state.operationError = &msg.err
			if msg.err.Category == application.ErrorUnavailable || msg.err.Category == application.ErrorInternal {
				state.operationStage = operatorRepositoryOperationRetryable
			} else {
				state.operationStage = operatorRepositoryOperationFailed
			}
			return m, nil
		}
		receipt := msg.result.Receipt
		state.receipt, state.operationError = &receipt, nil
		switch receipt.Outcome {
		case application.OperationOutcomeSucceeded:
			state.operationStage = operatorRepositoryOperationSucceeded
			return m, m.startRepositoryDetailRefresh()
		case application.OperationOutcomePending, application.OperationOutcomeAmbiguous:
			state.operationStage = operatorRepositoryOperationRetryable
			state.operationError = &operatorSafeError{Category: application.ErrorUnavailable, Message: "repository enablement outcome is uncertain"}
		default:
			state.operationStage = operatorRepositoryOperationFailed
			state.operationError = &operatorSafeError{Category: application.ErrorConflict, Message: "repository enablement did not succeed"}
		}
		return m, nil
	case operatorRefreshTickMsg:
		next := m.tickCommand()
		switch m.route {
		case operatorOverviewRoute:
			if m.batch != nil && !m.refreshing {
				return m, tea.Batch(next, m.startOverviewRefresh())
			}
		case operatorRunsRoute:
			if m.runs.page != nil && !m.runs.refreshing {
				return m, tea.Batch(next, m.startRunsRefresh(m.runs.request))
			}
		case operatorAttentionRoute:
			if m.attention.page != nil && !m.attention.refreshing {
				return m, tea.Batch(next, m.startAttentionRefresh(m.attention.request))
			}
		case operatorRunDetailRoute:
			if m.detail.detail != nil && !m.detail.refreshing {
				return m, tea.Batch(next, m.startDetailRefresh())
			}
		case operatorRepositoriesRoute:
			if m.repositories.page != nil && !m.repositories.refreshing {
				return m, tea.Batch(next, m.startRepositoriesRefresh(m.repositories.request))
			}
		case operatorRepositoryDetailRoute:
			if m.repositoryDetail.detail != nil && !m.repositoryDetail.refreshing && m.repositoryDetail.operationStage != operatorRepositoryOperationConfirming && m.repositoryDetail.operationStage != operatorRepositoryOperationPending {
				return m, tea.Batch(next, m.startRepositoryDetailRefresh())
			}
		}
		return m, next
	case tea.KeyPressMsg:
		if msg.Code == 'c' && msg.Mod&tea.ModCtrl != 0 {
			m.cancel()
			return m, tea.Quit
		}
		if m.route == operatorRunsRoute && m.runs.repositoryEditing {
			return m.updateRepositoryEditor(msg)
		}
		switch {
		case key.Matches(msg, operatorKeys.quit):
			m.cancel()
			return m, tea.Quit
		case m.route == operatorRepositoryDetailRoute && m.repositoryDetail.operationStage == operatorRepositoryOperationPending:
			return m, nil
		case m.route == operatorRepositoryDetailRoute && m.repositoryDetail.operationStage == operatorRepositoryOperationConfirming && key.Matches(msg, operatorKeys.open):
			requestID := m.newRequestID()
			if requestID == "" {
				m.repositoryDetail.operationStage = operatorRepositoryOperationFailed
				m.repositoryDetail.operationError = &operatorSafeError{Category: application.ErrorInternal, Message: "repository enablement request could not be created"}
				return m, nil
			}
			m.repositoryDetail.requestID = requestID
			m.repositoryDetail.operationGeneration++
			m.repositoryDetail.operationStage = operatorRepositoryOperationPending
			m.repositoryDetail.operationError, m.repositoryDetail.receipt = nil, nil
			return m, m.repositoryEnableCommand(m.repositoryDetail.operationGeneration, m.repositoryDetail.repository, requestID)
		case m.route == operatorRepositoryDetailRoute && m.repositoryDetail.operationStage == operatorRepositoryOperationConfirming && key.Matches(msg, operatorKeys.back):
			m.repositoryDetail.operationStage = operatorRepositoryOperationIdle
			return m, nil
		case m.route == operatorRepositoryDetailRoute && m.repositoryDetail.operationStage == operatorRepositoryOperationConfirming:
			return m, nil
		case m.route == operatorRepositoryDetailRoute && m.repositoryDetail.operationStage == operatorRepositoryOperationRetryable && key.Matches(msg, operatorKeys.open):
			m.repositoryDetail.operationGeneration++
			m.repositoryDetail.operationStage = operatorRepositoryOperationPending
			m.repositoryDetail.operationError = nil
			return m, m.repositoryEnableCommand(m.repositoryDetail.operationGeneration, m.repositoryDetail.repository, m.repositoryDetail.requestID)
		case key.Matches(msg, operatorKeys.overview):
			m.invalidateRepositoryReadsForRouteChange(operatorOverviewRoute)
			m.route = operatorOverviewRoute
			if !m.refreshing {
				return m, m.startOverviewRefresh()
			}
			return m, nil
		case key.Matches(msg, operatorKeys.runs):
			m.invalidateRepositoryReadsForRouteChange(operatorRunsRoute)
			m.route = operatorRunsRoute
			if !m.runs.refreshing {
				return m, m.startRunsRefresh(m.runs.request)
			}
			return m, nil
		case key.Matches(msg, operatorKeys.attention):
			m.invalidateRepositoryReadsForRouteChange(operatorAttentionRoute)
			m.route = operatorAttentionRoute
			if !m.attention.refreshing {
				return m, m.startAttentionRefresh(m.attention.request)
			}
			return m, nil
		case key.Matches(msg, operatorKeys.repositories):
			m.invalidateRepositoryReadsForRouteChange(operatorRepositoriesRoute)
			m.route = operatorRepositoriesRoute
			if !m.repositories.refreshing {
				return m, m.startRepositoriesRefresh(m.repositories.request)
			}
			return m, nil
		case m.route == operatorRunDetailRoute && key.Matches(msg, operatorKeys.back):
			m.route = m.detail.returnRoute
			return m, nil
		case m.route == operatorRepositoryDetailRoute && key.Matches(msg, operatorKeys.back):
			returnRoute := m.repositoryDetail.returnRoute
			m.invalidateRepositoryReadsForRouteChange(returnRoute)
			m.route = returnRoute
			return m, nil
		case key.Matches(msg, operatorKeys.refresh):
			switch m.route {
			case operatorOverviewRoute:
				if !m.refreshing {
					return m, m.startOverviewRefresh()
				}
			case operatorRunsRoute:
				if !m.runs.refreshing {
					return m, m.startRunsRefresh(m.runs.request)
				}
			case operatorAttentionRoute:
				if !m.attention.refreshing {
					return m, m.startAttentionRefresh(m.attention.request)
				}
			case operatorRunDetailRoute:
				if !m.detail.refreshing {
					return m, m.startDetailRefresh()
				}
			case operatorRepositoriesRoute:
				if !m.repositories.refreshing {
					return m, m.startRepositoriesRefresh(m.repositories.request)
				}
			case operatorRepositoryDetailRoute:
				if !m.repositoryDetail.refreshing && m.repositoryDetail.operationStage != operatorRepositoryOperationConfirming {
					return m, m.startRepositoryDetailRefresh()
				}
			}
			return m, nil
		case key.Matches(msg, operatorKeys.help):
			m.help.ShowAll = !m.help.ShowAll
			return m, nil
		case m.route == operatorOverviewRoute && m.batch != nil && key.Matches(msg, operatorKeys.next):
			m.moveFocus(1)
			return m, nil
		case m.route == operatorOverviewRoute && m.batch != nil && key.Matches(msg, operatorKeys.previous):
			m.moveFocus(-1)
			return m, nil
		case m.route == operatorOverviewRoute && m.batch != nil && key.Matches(msg, operatorKeys.up):
			m.moveSelection(-1)
			return m, nil
		case m.route == operatorOverviewRoute && m.batch != nil && key.Matches(msg, operatorKeys.down):
			m.moveSelection(1)
			return m, nil
		case m.route == operatorOverviewRoute && m.batch != nil && key.Matches(msg, operatorKeys.open):
			if m.focus == operatorRunsPanel {
				return m.openRunDetail(m.selectedOverviewRunID(), operatorOverviewRoute)
			}
			if m.focus == operatorRepositoriesPanel {
				rows := m.panelRows(operatorRepositoriesPanel)
				index := m.panels[operatorRepositoriesPanel].index
				if index >= 0 && index < len(rows) {
					return m.openRepositoryDetail(rows[index].id, operatorOverviewRoute)
				}
			}
			return m, nil
		case m.route == operatorRunsRoute && m.runs.page != nil && key.Matches(msg, operatorKeys.up):
			m.moveRunsSelection(-1)
			return m, nil
		case m.route == operatorRunsRoute && m.runs.page != nil && key.Matches(msg, operatorKeys.down):
			m.moveRunsSelection(1)
			return m, nil
		case m.route == operatorRunsRoute && m.runs.page != nil && key.Matches(msg, operatorKeys.open):
			return m.openRunDetail(m.selectedRunsID(), operatorRunsRoute)
		case m.route == operatorRunsRoute && key.Matches(msg, operatorKeys.lifecycle):
			request := m.runs.request
			request.lifecycle = nextRunLifecycle(request.lifecycle)
			request.cursor, request.previous = "", nil
			return m, m.startRunsRefresh(request)
		case m.route == operatorRunsRoute && key.Matches(msg, operatorKeys.repository):
			m.runs.repositoryEditing = true
			m.runs.repositoryInput = m.runs.request.repository
			return m, nil
		case m.route == operatorRunsRoute && m.runs.page != nil && key.Matches(msg, operatorKeys.nextPage):
			if m.runs.page.Collection.NextCursor != "" {
				request := m.runs.request
				request.previous = append(append([]string(nil), request.previous...), request.cursor)
				request.cursor = m.runs.page.Collection.NextCursor
				return m, m.startRunsRefresh(request)
			}
			return m, nil
		case m.route == operatorRunsRoute && m.runs.page != nil && key.Matches(msg, operatorKeys.previousPage):
			if len(m.runs.request.previous) != 0 {
				request := m.runs.request
				request.cursor = request.previous[len(request.previous)-1]
				request.previous = append([]string(nil), request.previous[:len(request.previous)-1]...)
				return m, m.startRunsRefresh(request)
			}
			return m, nil
		case m.route == operatorAttentionRoute && m.attention.page != nil && key.Matches(msg, operatorKeys.next):
			m.toggleAttentionFocus(1)
			return m, nil
		case m.route == operatorAttentionRoute && m.attention.page != nil && key.Matches(msg, operatorKeys.previous):
			m.toggleAttentionFocus(-1)
			return m, nil
		case m.route == operatorAttentionRoute && m.attention.page != nil && key.Matches(msg, operatorKeys.up):
			m.moveAttention(-1)
			return m, nil
		case m.route == operatorAttentionRoute && m.attention.page != nil && key.Matches(msg, operatorKeys.down):
			m.moveAttention(1)
			return m, nil
		case m.route == operatorAttentionRoute && m.attention.page != nil && key.Matches(msg, operatorKeys.open):
			item, ok := m.selectedAttentionItem()
			if ok && item.Navigation == application.RoutineAttentionNavigationRunDetail {
				return m.openRunDetail(item.RunID, operatorAttentionRoute)
			}
			return m, nil
		case m.route == operatorAttentionRoute && m.attention.page != nil && key.Matches(msg, operatorKeys.nextPage):
			if m.attention.page.Collection.NextCursor != "" {
				request := m.attention.request
				request.previous = append(append([]string(nil), request.previous...), request.cursor)
				request.cursor = m.attention.page.Collection.NextCursor
				return m, m.startAttentionRefresh(request)
			}
			return m, nil
		case m.route == operatorAttentionRoute && m.attention.page != nil && key.Matches(msg, operatorKeys.previousPage):
			if len(m.attention.request.previous) != 0 {
				request := m.attention.request
				request.cursor = request.previous[len(request.previous)-1]
				request.previous = append([]string(nil), request.previous[:len(request.previous)-1]...)
				return m, m.startAttentionRefresh(request)
			}
			return m, nil
		case m.route == operatorRepositoriesRoute && m.repositories.page != nil && key.Matches(msg, operatorKeys.up):
			m.moveRepositorySelection(-1)
			return m, nil
		case m.route == operatorRepositoriesRoute && m.repositories.page != nil && key.Matches(msg, operatorKeys.down):
			m.moveRepositorySelection(1)
			return m, nil
		case m.route == operatorRepositoriesRoute && m.repositories.page != nil && key.Matches(msg, operatorKeys.open):
			return m.openRepositoryDetail(m.selectedRepository(), operatorRepositoriesRoute)
		case m.route == operatorRepositoriesRoute && m.repositories.page != nil && key.Matches(msg, operatorKeys.nextPage):
			if m.repositories.page.Collection.NextCursor != "" {
				request := m.repositories.request
				request.previous = append(append([]string(nil), request.previous...), request.cursor)
				request.cursor = m.repositories.page.Collection.NextCursor
				return m, m.startRepositoriesRefresh(request)
			}
			return m, nil
		case m.route == operatorRepositoriesRoute && m.repositories.page != nil && key.Matches(msg, operatorKeys.previousPage):
			if len(m.repositories.request.previous) != 0 {
				request := m.repositories.request
				request.cursor = request.previous[len(request.previous)-1]
				request.previous = append([]string(nil), request.previous[:len(request.previous)-1]...)
				return m, m.startRepositoriesRefresh(request)
			}
			return m, nil
		case m.route == operatorRepositoryDetailRoute && m.repositoryDetail.detail != nil && key.Matches(msg, operatorKeys.enable):
			if m.repositoryEnableCanStart() {
				m.repositoryDetail.generation++
				m.repositoryDetail.refreshing = false
				m.repositoryDetail.operationStage = operatorRepositoryOperationConfirming
				m.repositoryDetail.operationError, m.repositoryDetail.receipt, m.repositoryDetail.requestID = nil, nil, ""
			}
			return m, nil
		case m.route == operatorRepositoryDetailRoute && m.repositoryDetail.detail != nil && key.Matches(msg, operatorKeys.up):
			m.repositoryDetail.dimensionIndex = clamp(m.repositoryDetail.dimensionIndex-1, 0, max(len(m.repositoryDetail.detail.Dimensions)-1, 0))
			return m, nil
		case m.route == operatorRepositoryDetailRoute && m.repositoryDetail.detail != nil && key.Matches(msg, operatorKeys.down):
			m.repositoryDetail.dimensionIndex = clamp(m.repositoryDetail.dimensionIndex+1, 0, max(len(m.repositoryDetail.detail.Dimensions)-1, 0))
			return m, nil
		case m.route == operatorRunDetailRoute && m.detail.detail != nil && key.Matches(msg, operatorKeys.up):
			m.detail.gateIndex = clamp(m.detail.gateIndex-1, 0, max(len(m.detail.detail.Gates)-1, 0))
			return m, nil
		case m.route == operatorRunDetailRoute && m.detail.detail != nil && key.Matches(msg, operatorKeys.down):
			m.detail.gateIndex = clamp(m.detail.gateIndex+1, 0, max(len(m.detail.detail.Gates)-1, 0))
			return m, nil
		}
	}
	return m, nil
}

func (m *operatorModel) startOverviewRefresh() tea.Cmd {
	m.initialError = nil
	m.generation++
	m.refreshing = true
	return m.overviewLoadCommand(m.generation)
}

func (m *operatorModel) invalidateRepositoryReadsForRouteChange(target operatorRoute) {
	if m.route == target {
		return
	}
	if m.route == operatorRepositoriesRoute {
		m.repositories.generation++
		m.repositories.refreshing = false
		m.repositories.pending = nil
	}
	if m.route == operatorRepositoryDetailRoute {
		m.repositoryDetail.generation++
		m.repositoryDetail.refreshing = false
	}
}

func (m *operatorModel) startRunsRefresh(request operatorRunsRequest) tea.Cmd {
	if m.runs.refreshing {
		return nil
	}
	m.runs.initialError = nil
	m.runs.generation++
	m.runs.refreshing = true
	copy := request
	copy.previous = append([]string(nil), request.previous...)
	m.runs.pending = &copy
	return m.runsLoadCommand(m.runs.generation, copy)
}

func (m *operatorModel) startAttentionRefresh(request operatorAttentionRequest) tea.Cmd {
	if m.attention.refreshing {
		return nil
	}
	m.attention.initialError = nil
	m.attention.generation++
	m.attention.refreshing = true
	copy := request
	copy.previous = append([]string(nil), request.previous...)
	m.attention.pending = &copy
	return m.attentionLoadCommand(m.attention.generation, copy)
}

func (m *operatorModel) startDetailRefresh() tea.Cmd {
	if m.detail.refreshing || m.detail.runID == "" {
		return nil
	}
	m.detail.initialError = nil
	m.detail.generation++
	m.detail.refreshing = true
	return m.detailLoadCommand(m.detail.generation, m.detail.runID)
}

func (m *operatorModel) startRepositoriesRefresh(request operatorRepositoriesRequest) tea.Cmd {
	if m.repositories.refreshing {
		return nil
	}
	m.repositories.initialError = nil
	m.repositories.generation++
	m.repositories.refreshing = true
	copy := request
	copy.previous = append([]string(nil), request.previous...)
	m.repositories.pending = &copy
	return m.repositoriesLoadCommand(m.repositories.generation, copy)
}

func (m *operatorModel) startRepositoryDetailRefresh() tea.Cmd {
	if m.repositoryDetail.refreshing || m.repositoryDetail.repository == "" {
		return nil
	}
	m.repositoryDetail.initialError = nil
	m.repositoryDetail.generation++
	m.repositoryDetail.refreshing = true
	return m.repositoryDetailLoadCommand(m.repositoryDetail.generation, m.repositoryDetail.repository)
}

func (m operatorModel) openRunDetail(runID string, returnRoute operatorRoute) (tea.Model, tea.Cmd) {
	if runID == "" {
		return m, nil
	}
	if m.detail.runID != runID {
		m.detail = operatorRunDetailState{runID: runID, returnRoute: returnRoute}
	} else {
		m.detail.returnRoute = returnRoute
	}
	m.route = operatorRunDetailRoute
	return m, m.startDetailRefresh()
}

func (m operatorModel) openRepositoryDetail(repository string, returnRoute operatorRoute) (tea.Model, tea.Cmd) {
	if repository == "" {
		return m, nil
	}
	m.invalidateRepositoryReadsForRouteChange(operatorRepositoryDetailRoute)
	if m.repositoryDetail.repository != repository {
		m.repositoryDetail = operatorRepositoryDetailState{repository: repository, returnRoute: returnRoute, operationStage: operatorRepositoryOperationIdle}
	} else {
		m.repositoryDetail.returnRoute = returnRoute
	}
	m.route = operatorRepositoryDetailRoute
	return m, m.startRepositoryDetailRefresh()
}

func (m operatorModel) updateRepositoryEditor(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.Code == tea.KeyEscape {
		m.runs.repositoryEditing = false
		return m, nil
	}
	if msg.Code == tea.KeyEnter {
		value := strings.TrimSpace(m.runs.repositoryInput)
		if value != "" && !validOperatorRepositoryFilter(value) {
			return m, nil
		}
		m.runs.repositoryEditing = false
		request := m.runs.request
		request.repository, request.cursor, request.previous = value, "", nil
		return m, m.startRunsRefresh(request)
	}
	if msg.Code == tea.KeyBackspace {
		runes := []rune(m.runs.repositoryInput)
		if len(runes) != 0 {
			m.runs.repositoryInput = string(runes[:len(runes)-1])
		}
		return m, nil
	}
	if msg.Text != "" && len([]rune(m.runs.repositoryInput+msg.Text)) <= 128 {
		m.runs.repositoryInput += msg.Text
	}
	return m, nil
}

func validOperatorRepositoryFilter(value string) bool {
	parts := strings.Split(value, "/")
	if len(parts) != 2 || len(parts[0]) == 0 || len(parts[1]) == 0 || len(value) > 128 || strings.ToLower(value) != value {
		return false
	}
	for index, part := range parts {
		for _, char := range part {
			valid := char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-' || index == 1 && (char == '_' || char == '.')
			if !valid {
				return false
			}
		}
	}
	return true
}

func nextRunLifecycle(current application.RunLifecycleFilter) application.RunLifecycleFilter {
	switch current {
	case application.RunLifecycleActive:
		return application.RunLifecycleEnded
	case application.RunLifecycleEnded:
		return application.RunLifecycleAll
	default:
		return application.RunLifecycleActive
	}
}

func (m operatorModel) selectedOverviewRunID() string {
	rows := m.panelRows(operatorRunsPanel)
	if len(rows) == 0 {
		return ""
	}
	return rows[clamp(m.panels[operatorRunsPanel].index, 0, len(rows)-1)].id
}

func (m operatorModel) selectedRunsID() string {
	if m.runs.page == nil || len(m.runs.page.Runs) == 0 {
		return ""
	}
	return m.runs.page.Runs[clamp(m.runs.index, 0, len(m.runs.page.Runs)-1)].RunID
}

func (m *operatorModel) restoreRunsSelection(runID string) {
	m.runs.index = clamp(m.runs.index, 0, max(len(m.runs.page.Runs)-1, 0))
	if runID != "" {
		for index, run := range m.runs.page.Runs {
			if run.RunID == runID {
				m.runs.index = index
				break
			}
		}
	}
	m.runs.selectedRunID = m.selectedRunsID()
}

func (m *operatorModel) moveRunsSelection(delta int) {
	if m.runs.page == nil || len(m.runs.page.Runs) == 0 {
		return
	}
	m.runs.index = clamp(m.runs.index+delta, 0, len(m.runs.page.Runs)-1)
	m.runs.selectedRunID = m.selectedRunsID()
}

func (m operatorModel) selectedAttentionEventID() string {
	item, ok := m.selectedAttentionItem()
	if !ok {
		return ""
	}
	return item.EventID
}

func (m operatorModel) selectedAttentionItem() (application.RoutineAttentionItem, bool) {
	if m.attention.page == nil || len(m.attention.page.Items) == 0 {
		return application.RoutineAttentionItem{}, false
	}
	return m.attention.page.Items[clamp(m.attention.index, 0, len(m.attention.page.Items)-1)], true
}

func (m *operatorModel) restoreAttentionSelection(eventID string) {
	if m.attention.page == nil {
		return
	}
	m.attention.index = clamp(m.attention.index, 0, max(len(m.attention.page.Items)-1, 0))
	for index, item := range m.attention.page.Items {
		if eventID != "" && item.EventID == eventID {
			m.attention.index = index
			break
		}
	}
	m.attention.selectedEventID = m.selectedAttentionEventID()
	m.attention.summaryOffset = 0
	if len(m.attention.page.Items) == 0 {
		m.attention.focus = operatorAttentionListFocus
	}
}

func (m *operatorModel) toggleAttentionFocus(_ int) {
	if m.attention.focus == operatorAttentionListFocus {
		m.attention.focus = operatorAttentionSummaryFocus
	} else {
		m.attention.focus = operatorAttentionListFocus
	}
}

func (m *operatorModel) moveAttention(delta int) {
	if m.attention.focus == operatorAttentionSummaryFocus {
		m.attention.summaryOffset = max(m.attention.summaryOffset+delta, 0)
		return
	}
	if m.attention.page == nil || len(m.attention.page.Items) == 0 {
		return
	}
	m.attention.index = clamp(m.attention.index+delta, 0, len(m.attention.page.Items)-1)
	m.attention.selectedEventID = m.selectedAttentionEventID()
	m.attention.summaryOffset = 0
}

func safeOperatorError(err error) operatorSafeError {
	var safe *application.ServiceError
	if errors.As(err, &safe) {
		return operatorSafeError{Category: safe.Category, Message: safe.Message}
	}
	return operatorSafeError{Category: application.ErrorInternal, Message: "operator view is unavailable"}
}

func (m operatorModel) selectablePanels() []operatorPanel {
	if m.batch == nil {
		return nil
	}
	var order []operatorPanel
	if m.width >= operatorWideWidth {
		order = []operatorPanel{operatorRepositoriesPanel, operatorRunsPanel, operatorAttentionPanel}
	} else {
		order = []operatorPanel{operatorAttentionPanel, operatorRepositoriesPanel, operatorRunsPanel}
	}
	result := order[:0]
	for _, panel := range order {
		if m.panelLength(panel) > 0 {
			result = append(result, panel)
		}
	}
	return result
}

func (m *operatorModel) normalizeFocus() {
	panels := m.selectablePanels()
	if len(panels) == 0 {
		m.focus = ""
		return
	}
	for _, panel := range panels {
		if panel == m.focus {
			return
		}
	}
	m.focus = panels[0]
}

func (m *operatorModel) moveFocus(delta int) {
	panels := m.selectablePanels()
	if len(panels) == 0 {
		m.focus = ""
		return
	}
	current := 0
	for i, panel := range panels {
		if panel == m.focus {
			current = i
			break
		}
	}
	next := (current + delta) % len(panels)
	if next < 0 {
		next += len(panels)
	}
	m.focus = panels[next]
}

func (m *operatorModel) moveSelection(delta int) {
	length := m.panelLength(m.focus)
	if length == 0 {
		return
	}
	state := m.panels[m.focus]
	state.index = clamp(state.index+delta, 0, length-1)
	m.panels[m.focus] = state
}

func (m operatorModel) panelLength(panel operatorPanel) int {
	if m.batch == nil {
		return 0
	}
	switch panel {
	case operatorRepositoriesPanel:
		return len(m.batch.Repositories.Repositories)
	case operatorRunsPanel:
		return len(m.batch.Overview.Runs.ActiveRuns) + len(m.batch.Overview.Runs.RecentRuns)
	case operatorAttentionPanel:
		return len(m.batch.Overview.Actionable)
	default:
		return 0
	}
}

func (m operatorModel) selectedIdentities() map[operatorPanel]string {
	result := map[operatorPanel]string{}
	if m.batch == nil {
		return result
	}
	for _, panel := range []operatorPanel{operatorRepositoriesPanel, operatorRunsPanel, operatorAttentionPanel} {
		rows := m.panelRows(panel)
		index := m.panels[panel].index
		if index >= 0 && index < len(rows) {
			result[panel] = rows[index].id
		}
	}
	return result
}

func (m *operatorModel) restoreSelections(previous map[operatorPanel]string) {
	for _, panel := range []operatorPanel{operatorRepositoriesPanel, operatorRunsPanel, operatorAttentionPanel} {
		rows := m.panelRows(panel)
		state := m.panels[panel]
		matched := false
		for i, row := range rows {
			if previous[panel] != "" && row.id == previous[panel] {
				state.index, matched = i, true
				break
			}
		}
		if !matched {
			state.index = clamp(state.index, 0, max(len(rows)-1, 0))
		}
		m.panels[panel] = state
	}
}

type operatorRow struct {
	id     string
	name   string
	status string
	detail string
	tone   string
}

func (m operatorModel) panelRows(panel operatorPanel) []operatorRow {
	if m.batch == nil {
		return nil
	}
	observedAt := m.batch.ObservedAt
	switch panel {
	case operatorRepositoriesPanel:
		rows := make([]operatorRow, 0, len(m.batch.Repositories.Repositories))
		for _, repository := range m.batch.Repositories.Repositories {
			onboarding := "none"
			if repository.Onboarding != nil {
				onboarding = presentationLabel(string(repository.Onboarding.Status))
			}
			availability := "unavailable"
			if repository.Available {
				availability = "available"
			}
			detail := fmt.Sprintf("%s · %s · config %s · onboarding %s · %s", presentationLabel(string(repository.LifecycleIntent)), availability, presentationLabel(string(repository.ConfigurationConvergence)), onboarding, formatObservationAge(observedAt, repository.LastObservedAt))
			if repository.ActiveRunID != "" {
				detail += " · active run " + repository.ActiveRunID
			}
			rows = append(rows, operatorRow{
				id: repository.Repository, name: repository.Repository,
				status: presentationLabel(string(repository.Readiness)), detail: detail,
				tone: repositoryRowTone(repository.Available, string(repository.Readiness)),
			})
		}
		return rows
	case operatorRunsPanel:
		rows := make([]operatorRow, 0, len(m.batch.Overview.Runs.ActiveRuns)+len(m.batch.Overview.Runs.RecentRuns))
		appendRuns := func(kind string, runs []application.RoutineRunSummary) {
			for _, run := range runs {
				attention := ""
				if run.Attention {
					attention = " · attention evidence"
				}
				identifier := run.LinearIdentifier
				if identifier == "" {
					identifier = run.RunID
				} else {
					identifier += " / " + run.RunID
				}
				rows = append(rows, operatorRow{
					id: run.RunID, name: identifier + " · " + run.Repository,
					status: presentationLabel(string(run.State)),
					detail: kind + attention + " · " + formatObservationAge(observedAt, run.UpdatedAt),
					tone:   runRowTone(string(run.State), run.Attention),
				})
			}
		}
		appendRuns("active", m.batch.Overview.Runs.ActiveRuns)
		appendRuns("recently ended", m.batch.Overview.Runs.RecentRuns)
		return rows
	case operatorAttentionPanel:
		rows := make([]operatorRow, 0, len(m.batch.Overview.Actionable))
		for _, item := range m.batch.Overview.Actionable {
			support := matchingAttentionEvidence(item, m.batch.Overview.Attention)
			detail := fmt.Sprintf("%s · %s", presentationLabel(string(item.Scope)), formatObservationAge(observedAt, item.ObservedAt))
			if support != "" {
				detail += " · evidence " + support
			}
			rows = append(rows, operatorRow{
				id: item.ItemID, name: m.attentionTargetLabel(item.TargetID) + " · " + reasonPresentation(item.ReasonCode),
				status: presentationLabel(item.Severity), detail: detail, tone: severityTone(item.Severity),
			})
		}
		return rows
	default:
		return nil
	}
}

func repositoryRowTone(available bool, readiness string) string {
	if !available || readiness == "not_ready" || readiness == "unavailable" {
		return "danger"
	}
	if readiness == "ready" {
		return "good"
	}
	return "warning"
}

func runRowTone(state string, attention bool) string {
	if attention {
		return "warning"
	}
	switch state {
	case "completed":
		return "good"
	case "failed", "cancelled", "rejected":
		return "danger"
	default:
		return "muted"
	}
}

func severityTone(severity string) string {
	switch strings.ToLower(severity) {
	case "critical", "high", "error":
		return "danger"
	case "medium", "warning":
		return "warning"
	default:
		return "muted"
	}
}

func (m operatorModel) attentionTargetLabel(targetID string) string {
	for _, run := range append(append([]application.RoutineRunSummary{}, m.batch.Overview.Runs.ActiveRuns...), m.batch.Overview.Runs.RecentRuns...) {
		if run.RunID == targetID && run.LinearIdentifier != "" {
			return run.LinearIdentifier
		}
	}
	return targetID
}

var reasonPresentations = map[string]string{
	"admission_authority_conflict":  "admission authority conflict",
	"cleanup_residue":               "cleanup residue",
	"human_decision_required":       "human decision required",
	"incomplete_authority":          "incomplete authority",
	"lease_lost":                    "lease lost",
	"legacy_ci_topology_drift":      "legacy CI topology drift",
	"manual_intervention":           "manual intervention",
	"manual_state":                  "manual state",
	"terminal_failure":              "terminal failure",
	"top_priority_tie":              "top priority tie",
	"repository_disabled":           "repository disabled",
	"repository_busy":               "repository busy",
	"readiness_recheck_in_progress": "readiness recheck in progress",
	"available":                     "available",
}

func reasonPresentation(reason string) string {
	if label, ok := reasonPresentations[reason]; ok {
		return label
	}
	return reason
}

func matchingAttentionEvidence(item application.RoutineActionableItem, evidence []application.RoutineAttentionSummary) string {
	for _, attention := range evidence {
		if attention.Scope == item.Scope && attention.TargetID == item.TargetID && attention.ReasonCode == item.ReasonCode {
			return presentationLabel(string(attention.State))
		}
	}
	return ""
}

func (m operatorModel) View() tea.View {
	content := m.render()
	view := tea.NewView(content)
	view.AltScreen = true
	view.MouseMode = tea.MouseModeNone
	view.WindowTitle = "Agent Loop Controller Operator"
	return view
}

func (m operatorModel) render() string {
	if m.width < operatorMinimumWidth || m.height < operatorMinimumHeight {
		navigation := "1 Overview  ·  2 Runs  ·  3 Attention  ·  4 Repositories"
		if m.route == operatorRunDetailRoute || m.route == operatorRepositoryDetailRoute {
			navigation += "  ·  Esc back"
		}
		return lipgloss.NewStyle().Padding(1, 2).Render(fmt.Sprintf("Terminal too small\n\nCurrent: %dx%d\nRequired: 80x24 minimum\n\n%s  ·  q / Ctrl-C quit", m.width, m.height, navigation))
	}
	switch m.route {
	case operatorRunsRoute:
		return m.renderRunsScreen()
	case operatorAttentionRoute:
		return m.renderAttentionScreen()
	case operatorRepositoriesRoute:
		return m.renderRepositoriesScreen()
	case operatorRunDetailRoute:
		return m.renderRunDetailScreen()
	case operatorRepositoryDetailRoute:
		return m.renderRepositoryDetailScreen()
	default:
		return m.renderOverviewScreen()
	}
}

func (m operatorModel) renderOverviewScreen() string {
	if m.batch == nil {
		if m.initialError != nil {
			return lipgloss.NewStyle().Padding(1, 2).Render("Operator Overview unavailable\n\n" + m.initialError.String() + "\n\nr retry · 2 Runs · 3 Attention · 4 Repositories · q / Ctrl-C quit")
		}
		return lipgloss.NewStyle().Padding(1, 2).Render("Agent Loop Controller\n\nLoading operator overview…\n\n2 Runs · 3 Attention · 4 Repositories · q / Ctrl-C quit")
	}
	header := lipgloss.JoinVertical(lipgloss.Left, m.renderHeader(), m.renderHealth())
	footer := m.renderHelp()
	bodyHeight := max(m.height-lipgloss.Height(header)-lipgloss.Height(footer), 3)
	var body string
	if m.width >= operatorWideWidth {
		body = m.renderWide(bodyHeight)
	} else {
		body = m.renderVertical(bodyHeight)
	}
	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

func (m operatorModel) renderRunsScreen() string {
	footer := m.renderHelp()
	if m.runs.page == nil {
		if m.runs.initialError != nil {
			return boundedLines(m.width, m.height, []string{"Agent Loop Controller / Runs", "", "Runs unavailable", m.runs.initialError.String(), "", "r retry · 1 overview · q quit"})
		}
		return boundedLines(m.width, m.height, []string{"Agent Loop Controller / Runs", "", "Loading authorized runs…", "", "1 overview · q quit"})
	}
	header := m.renderRouteHeader("Runs", m.runs.page.Metadata.ObservedAt, m.runs.refreshing, m.runs.staleError)
	repository := m.runs.request.repository
	if repository == "" {
		repository = "All repositories"
	}
	pageNumber := len(m.runs.request.previous) + 1
	filter := fmt.Sprintf("Lifecycle %s  ·  Repository %s  ·  Page %d  ·  %d of %d displayed",
		presentationLabel(string(m.runs.request.lifecycle)), repository, pageNumber, len(m.runs.page.Runs), m.runs.page.Collection.Total)
	if m.runs.repositoryEditing {
		filter = "Repository filter (owner/repository, blank for All): " + m.runs.repositoryInput + "█"
	}
	bodyHeight := max(m.height-lipgloss.Height(header)-lipgloss.Height(footer)-2, 1)
	lines := []string{truncateRunes(filter, m.width)}
	if len(m.runs.page.Runs) == 0 {
		lines = append(lines, "", fmt.Sprintf("No runs for lifecycle %s and repository %s", presentationLabel(string(m.runs.request.lifecycle)), repository))
	} else {
		available := max(bodyHeight-1, 1)
		selected := clamp(m.runs.index, 0, len(m.runs.page.Runs)-1)
		start := clamp(selected-available+1, 0, max(len(m.runs.page.Runs)-available, 0))
		end := min(start+available, len(m.runs.page.Runs))
		for index := start; index < end; index++ {
			run := m.runs.page.Runs[index]
			identifier := run.LinearIdentifier
			if identifier == "" {
				identifier = run.RunID
			} else {
				identifier += " / " + run.RunID
			}
			detail := run.Repository + " · " + formatObservationAge(m.runs.page.Metadata.ObservedAt, run.UpdatedAt)
			if run.Attention {
				detail += " · attention"
			}
			if run.CandidateHead != "" && m.width >= operatorWideWidth {
				detail += " · head " + run.CandidateHead
			}
			lines = append(lines, m.renderRow(operatorRow{id: run.RunID, name: identifier, status: presentationLabel(string(run.State)), detail: detail, tone: runRowTone(string(run.State), run.Attention)}, m.width, index == selected))
		}
	}
	body := lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(operatorBorderColor).Width(m.width).Height(bodyHeight).Render(strings.Join(lines, "\n"))
	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

func (m operatorModel) renderRunDetailScreen() string {
	footer := m.renderHelp()
	if m.detail.detail == nil {
		if m.detail.initialError != nil {
			return boundedLines(m.width, m.height, []string{"Agent Loop Controller / Run detail", "", "Run detail unavailable", m.detail.initialError.String(), "", "r retry · esc back · q quit"})
		}
		return boundedLines(m.width, m.height, []string{"Agent Loop Controller / Run detail", "", "Loading run " + m.detail.runID + "…", "", "esc back · q quit"})
	}
	detail := m.detail.detail
	header := m.renderRouteHeader("Run detail", detail.Metadata.ObservedAt, m.detail.refreshing, m.detail.staleError)
	identifier := detail.Run.LinearIdentifier
	if identifier == "" {
		identifier = detail.Run.RunID
	} else {
		identifier += " / " + detail.Run.RunID
	}
	head := detail.Run.CandidateHead
	if head == "" {
		head = "not established"
	}
	lines := []string{
		fmt.Sprintf("%s · %s", detail.Run.Repository, identifier),
		fmt.Sprintf("State %s · Phase %s", presentationLabel(string(detail.Run.State)), presentationLabel(string(detail.Phase))),
		"Candidate exact head " + head,
		fmt.Sprintf("Wait %s · Kind %s · updated %s", presentationLabel(string(detail.WaitAssessment)), presentationLabel(string(detail.Wait)), formatObservationAge(detail.Metadata.ObservedAt, detail.Run.UpdatedAt)),
	}
	if len(detail.Attention) != 0 {
		attention := detail.Attention[0]
		lines = append(lines, fmt.Sprintf("ATTENTION %s · %s · %s · %s", presentationLabel(attention.Severity), presentationLabel(string(attention.State)), attention.ReasonCode, formatObservationAge(detail.Metadata.ObservedAt, attention.ObservedAt)))
	} else {
		lines = append(lines, "Attention none")
	}
	if detail.LatestTransition != nil {
		transition := detail.LatestTransition
		lines = append(lines, fmt.Sprintf("Progress %s → %s · %s · %s", presentationLabel(string(transition.From)), presentationLabel(string(transition.To)), transition.ReasonCode, formatObservationAge(detail.Metadata.ObservedAt, transition.ObservedAt)))
		if transition.BoundHead != "" {
			lines = append(lines, "Transition bound head "+transition.BoundHead)
		}
	} else {
		lines = append(lines, "Progress no meaningful transition recorded")
	}
	if detail.PullRequest != nil {
		pr := detail.PullRequest
		lines = append(lines, fmt.Sprintf("Pull request #%d · %s · head %s · merged %t", pr.Number, presentationLabel(pr.State), pr.HeadSHA, pr.Merged))
	} else {
		lines = append(lines, "Pull request not established")
	}
	lines = append(lines, "Delivery gates (Controller order)")
	gateSlots := max(m.height-lipgloss.Height(header)-lipgloss.Height(footer)-len(lines)-2, 1)
	selected := clamp(m.detail.gateIndex, 0, max(len(detail.Gates)-1, 0))
	start := clamp(selected-gateSlots+1, 0, max(len(detail.Gates)-gateSlots, 0))
	end := min(start+gateSlots, len(detail.Gates))
	for index := start; index < end; index++ {
		gate := detail.Gates[index]
		marker, tone := gateMarker(gate.Status)
		status := marker + " " + presentationLabel(string(gate.Status))
		if index != selected {
			status = tone.Render(status)
		}
		line := fmt.Sprintf("  %-28s %s", presentationLabel(string(gate.Name)), status)
		if index == selected {
			line = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("28")).Width(m.width).Render("> " + line[2:])
		}
		lines = append(lines, line)
	}
	if len(detail.Gates) != 0 {
		gate := detail.Gates[selected]
		evidence := fmt.Sprintf("Selected reason %s · evidence %d", gate.ReasonCode, gate.EvidenceCount)
		if gate.EvidenceTruncated {
			evidence += " (truncated)"
		}
		bound := gate.BoundHead
		if bound == "" {
			bound = "not supplied"
		}
		observation := "not supplied"
		if gate.ObservedAt != nil {
			observation = gate.ObservedAt.UTC().Format(time.RFC3339)
		}
		lines = append(lines, evidence, "Selected head "+bound+" · observed "+observation)
	}
	for index := range lines {
		lines[index] = ansi.Truncate(lines[index], m.width, "…")
	}
	return lipgloss.JoinVertical(lipgloss.Left, header, strings.Join(lines, "\n"), footer)
}

func (m operatorModel) renderRouteHeader(title string, observedAt time.Time, refreshing bool, stale *operatorSafeError) string {
	left := operatorHeadingStyle.Render("Agent Loop Controller / " + title)
	right := operatorMutedStyle.Render("Observed " + observedAt.Local().Format("15:04:05") + " · auto refresh " + m.refreshInterval.String())
	if stale != nil {
		right = operatorDangerStyle.Render("STALE · " + stale.String())
	} else if refreshing {
		right = operatorWarningStyle.Render("Refreshing…")
	}
	right = ansi.Truncate(right, max(m.width-lipgloss.Width(left)-2, 1), "…")
	gap := max(m.width-lipgloss.Width(left)-lipgloss.Width(right), 1)
	return left + strings.Repeat(" ", gap) + right
}

func boundedLines(width, height int, lines []string) string {
	for index := range lines {
		lines[index] = truncateRunes(lines[index], width)
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	return strings.Join(lines, "\n")
}

func gateMarker(status application.DeliveryGateStatus) (string, lipgloss.Style) {
	switch status {
	case application.GatePassed:
		return "✓", operatorAccentStyle
	case application.GatePending, application.GateRunning, application.GateBlocked:
		return "…", operatorWarningStyle
	case application.GateFailed, application.GateConflict:
		return "✕", operatorDangerStyle
	default:
		return "○", operatorMutedStyle
	}
}

var (
	operatorAccentStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	operatorWarningStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	operatorDangerStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	operatorMutedStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	operatorHeadingStyle = lipgloss.NewStyle().Bold(true)
	operatorBorderColor  = lipgloss.Color("240")
	operatorFocusColor   = lipgloss.Color("42")
)

func (m operatorModel) renderHeader() string {
	left := operatorHeadingStyle.Render("Agent Loop Controller / Overview")
	observation := "Observed " + m.batch.ObservedAt.Local().Format("15:04:05") + " · auto refresh " + m.refreshInterval.String()
	if m.staleError != nil {
		observation = operatorDangerStyle.Render("STALE · " + m.staleError.String())
	} else if m.refreshing {
		observation = operatorWarningStyle.Render("Refreshing…")
	} else {
		observation = operatorMutedStyle.Render(observation)
	}
	available := max(m.width-lipgloss.Width(left)-2, 0)
	if available == 0 {
		return truncateRunes(left, m.width)
	}
	observation = ansi.Truncate(observation, available, "…")
	gap := max(m.width-lipgloss.Width(left)-lipgloss.Width(observation), 1)
	return left + strings.Repeat(" ", gap) + observation
}

func (m operatorModel) renderHealth() string {
	overview := m.batch.Overview
	draining := ""
	if overview.Capacity.Draining {
		draining = " · draining"
	}
	admission := "disabled"
	if overview.AdmissionEnabled {
		admission = "enabled"
	}
	readiness := presentationLabel(string(overview.Readiness))
	marker, tone := healthMarker(string(overview.Readiness))
	status := tone.Bold(true).Render(marker + " " + readiness)
	health := fmt.Sprintf("%s  ·  %s\nWorker %s/%s  ·  heartbeat %s\nCapacity %d/%d in use%s  ·  Admission %s  ·  Repositories %d total, %d ready, %d unavailable",
		status,
		healthSummary(string(overview.Readiness)),
		presentationLabel(string(overview.Worker.Liveness)),
		presentationLabel(string(overview.Worker.Activity)),
		formatHeartbeatAge(overview.Worker),
		overview.Capacity.InUse,
		overview.Capacity.EffectiveCapacity,
		draining,
		admission,
		overview.Repositories.Total,
		overview.Repositories.Ready,
		overview.Repositories.Unavailable,
	)
	return lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(operatorBorderColor).Padding(0, 1).Width(m.width).Render(health)
}

func (m operatorModel) renderWide(height int) string {
	leftWidth := (m.width - 1) * 3 / 5
	rightWidth := m.width - leftWidth - 1
	leftHeights := m.allocatePanelHeights([]operatorPanel{operatorRepositoriesPanel, operatorRunsPanel}, height)
	left := lipgloss.JoinVertical(lipgloss.Left,
		m.renderPanel(operatorRepositoriesPanel, leftWidth, leftHeights[0]),
		m.renderPanel(operatorRunsPanel, leftWidth, leftHeights[1]),
	)
	rightHeight := min(m.panelDesiredHeight(operatorAttentionPanel), height)
	right := m.renderPanel(operatorAttentionPanel, rightWidth, rightHeight)
	return lipgloss.JoinHorizontal(lipgloss.Top, left, " ", right)
}

func (m operatorModel) renderVertical(height int) string {
	panels := []operatorPanel{operatorAttentionPanel, operatorRepositoriesPanel, operatorRunsPanel}
	heights := m.allocatePanelHeights(panels, height)
	return lipgloss.JoinVertical(lipgloss.Left,
		m.renderPanel(operatorAttentionPanel, m.width, heights[0]),
		m.renderPanel(operatorRepositoriesPanel, m.width, heights[1]),
		m.renderPanel(operatorRunsPanel, m.width, heights[2]),
	)
}

func (m operatorModel) panelDesiredHeight(panel operatorPanel) int {
	contentRows := len(m.panelRows(panel))
	if contentRows == 0 {
		contentRows = 1
	}
	return contentRows + 3
}

func (m operatorModel) allocatePanelHeights(panels []operatorPanel, available int) []int {
	heights := make([]int, len(panels))
	remaining := available
	for index := range panels {
		heights[index] = min(4, max(available-index*3, 1))
		remaining -= heights[index]
	}
	for remaining > 0 {
		advanced := false
		for index, panel := range panels {
			if remaining == 0 {
				break
			}
			if heights[index] < m.panelDesiredHeight(panel) {
				heights[index]++
				remaining--
				advanced = true
			}
		}
		if !advanced {
			break
		}
	}
	return heights
}

func (m operatorModel) renderPanel(panel operatorPanel, width, height int) string {
	rows := m.panelRows(panel)
	title, empty, supporting := m.panelTitle(panel), m.panelEmpty(panel), ""
	if panel == operatorAttentionPanel && len(rows) == 0 && len(m.batch.Overview.Attention) > 0 {
		supporting = fmt.Sprintf("Supporting evidence only: %d active", len(m.batch.Overview.Attention))
	}
	innerWidth, innerHeight := max(width-2, 1), max(height-2, 1)
	lines := []string{m.renderPanelHeader(title, m.panelSummary(panel), innerWidth)}
	available := max(innerHeight-1, 0)
	if len(rows) == 0 {
		if supporting != "" {
			lines = append(lines, truncateRunes(supporting, innerWidth))
		} else if available > 0 {
			lines = append(lines, truncateRunes(empty, innerWidth))
		}
	} else if available > 0 {
		selected := clamp(m.panels[panel].index, 0, len(rows)-1)
		start := clamp(selected-available+1, 0, max(len(rows)-available, 0))
		end := min(start+available, len(rows))
		for index := start; index < end; index++ {
			active := m.focus == panel && index == selected
			lines = append(lines, m.renderRow(rows[index], innerWidth, active))
		}
	}
	style := lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(operatorBorderColor).Width(width).Height(height)
	if m.focus == panel && len(rows) > 0 {
		style = style.BorderForeground(operatorFocusColor)
	}
	return style.Render(strings.Join(lines, "\n"))
}

func (m operatorModel) panelTitle(panel operatorPanel) string {
	switch panel {
	case operatorRepositoriesPanel:
		return "Repositories"
	case operatorRunsPanel:
		return "Runs"
	case operatorAttentionPanel:
		return "Attention"
	default:
		return ""
	}
}

func collectionSummary(shown, total int, truncated bool) string {
	if truncated || shown < total {
		return fmt.Sprintf("%d of %d displayed", shown, total)
	}
	return fmt.Sprintf("%d total", total)
}

func (m operatorModel) panelSummary(panel operatorPanel) string {
	switch panel {
	case operatorRepositoriesPanel:
		return collectionSummary(len(m.batch.Repositories.Repositories), m.batch.Repositories.Collection.Total, m.batch.Repositories.Collection.Truncated)
	case operatorRunsPanel:
		runs := m.batch.Overview.Runs
		shown, total := len(runs.ActiveRuns)+len(runs.RecentRuns), runs.Active+runs.Recent
		if runs.ActiveTruncated || runs.RecentTruncated || shown < total {
			return collectionSummary(shown, total, true)
		}
		return fmt.Sprintf("%d active · %d recently ended", runs.Active, runs.Recent)
	case operatorAttentionPanel:
		return collectionSummary(len(m.batch.Overview.Actionable), m.batch.Overview.ActionableTotal, m.batch.Overview.ActionableTruncated)
	default:
		return ""
	}
}

func (m operatorModel) renderPanelHeader(title, summary string, width int) string {
	title = truncateRunes(title, width)
	available := width - lipgloss.Width(title) - 1
	if available <= 0 {
		return operatorHeadingStyle.Render(title)
	}
	summary = truncateRunes(summary, available)
	gap := max(width-lipgloss.Width(title)-lipgloss.Width(summary), 1)
	return operatorHeadingStyle.Render(title) + strings.Repeat(" ", gap) + operatorMutedStyle.Render(summary)
}

func (m operatorModel) renderRow(row operatorRow, width int, active bool) string {
	prefix := "  "
	if active {
		prefix = "> "
	}
	marker, statusStyle := rowMarker(row.tone)
	status := marker + " " + row.status
	if !active {
		status = statusStyle.Render(status)
	}
	available := max(width-lipgloss.Width(prefix), 1)
	var content string
	if available >= 62 {
		nameWidth := min(30, max(18, available/3))
		name := truncateRunes(row.name, nameWidth)
		content = lipgloss.NewStyle().Width(nameWidth).Render(name) + "  " + status + "  " + row.detail
	} else {
		content = row.name + "  " + status + " · " + row.detail
	}
	line := prefix + ansi.Truncate(content, available, "…")
	if active {
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("28")).Width(width).Render(line)
	}
	return line
}

func rowMarker(tone string) (string, lipgloss.Style) {
	switch tone {
	case "good":
		return "●", operatorAccentStyle
	case "warning":
		return "▲", operatorWarningStyle
	case "danger":
		return "●", operatorDangerStyle
	default:
		return "○", operatorMutedStyle
	}
}

func healthMarker(readiness string) (string, lipgloss.Style) {
	switch readiness {
	case "ready":
		return "●", operatorAccentStyle
	case "degraded", "attention_required", "restart_required", "stale":
		return "▲", operatorWarningStyle
	case "offline", "conflict":
		return "●", operatorDangerStyle
	default:
		return "○", operatorMutedStyle
	}
}

func healthSummary(readiness string) string {
	switch readiness {
	case "ready":
		return "Controller is ready"
	case "degraded":
		return "Controller is degraded"
	case "attention_required":
		return "Operator attention is required"
	case "restart_required":
		return "Worker restart is required"
	case "stale":
		return "Controller evidence is stale"
	case "offline":
		return "Controller worker is offline"
	case "conflict":
		return "Controller authority conflicts"
	default:
		return "Controller health is unknown"
	}
}

func (m operatorModel) panelEmpty(panel operatorPanel) string {
	switch panel {
	case operatorRepositoriesPanel:
		return "No registered repositories"
	case operatorRunsPanel:
		return "No active or recently ended runs"
	case operatorAttentionPanel:
		return "No operator attention"
	default:
		return ""
	}
}

func (m operatorModel) renderHelp() string {
	var value string
	switch m.route {
	case operatorRunsRoute:
		if m.runs.repositoryEditing {
			value = "type exact owner/repository · enter apply · esc cancel"
		} else if m.help.ShowAll {
			value = "1 overview · 2 runs · 3 attention · 4 repositories · ↑/↓ select · enter open · f lifecycle · / repository · n/p page · r refresh · ? help · q quit"
		} else {
			value = "1/2/3/4 navigate · ↑/↓ select · enter open · f filter · / repository · n/p page · r refresh · ? · q"
		}
	case operatorAttentionRoute:
		open := ""
		if item, ok := m.selectedAttentionItem(); ok && item.Navigation == application.RoutineAttentionNavigationRunDetail {
			open = " · enter open run"
		}
		if m.help.ShowAll {
			value = "1 overview · 2 runs · 3 attention · 4 repositories · tab list/summary · ↑/↓ select or scroll" + open + " · n/p page · r refresh · ? help · q quit"
		} else {
			value = "1/2/3/4 navigate · tab region · ↑/↓ select/scroll" + open + " · n/p page · r refresh · ? · q"
		}
	case operatorRunDetailRoute:
		value = "1/2/3/4 navigate · esc back · ↑/↓ delivery gates · r refresh · ? help · q quit"
	case operatorRepositoriesRoute:
		value = "1/2/3/4 navigate · ↑/↓ select · enter open · n/p page · r refresh · ? · q"
	case operatorRepositoryDetailRoute:
		switch m.repositoryDetail.operationStage {
		case operatorRepositoryOperationConfirming:
			value = "enter confirm enable · esc cancel · q quit"
		case operatorRepositoryOperationPending:
			value = "enablement pending · q quit"
		case operatorRepositoryOperationRetryable:
			value = "enter retry same request · esc back · q quit"
		default:
			value = "1/2/3/4 navigate · esc back · ↑/↓ dimensions · r refresh · ? help · q quit"
			if m.repositoryEnableCanStart() {
				value = "1/2/3/4 navigate · esc back · ↑/↓ dimensions · e enable · r refresh · ? · q"
			}
		}
	default:
		if m.help.ShowAll {
			value = "1 overview · 2 runs · 3 attention · 4 repositories · tab/shift+tab panels · ↑/↓ or j/k select · r refresh · ? help · q quit"
		} else {
			value = "1/2/3/4 navigate · tab panels · ↑/↓ select · r refresh · ? · q"
		}
		if m.focus == operatorRunsPanel && m.batch != nil && len(m.panelRows(operatorRunsPanel)) != 0 {
			value = "1/2/3/4 navigate · tab panels · ↑/↓ select · enter open run · r refresh · ? · q"
		}
		if m.focus == operatorRepositoriesPanel && m.batch != nil && len(m.panelRows(operatorRepositoriesPanel)) != 0 {
			value = "1/2/3/4 navigate · tab panels · ↑/↓ select · enter open repository · r refresh · ? · q"
		}
	}
	return " " + ansi.Truncate(value, max(m.width-2, 1), "…") + " "
}

var presentationLabels = map[string]string{
	"ready": "READY", "degraded": "DEGRADED", "attention_required": "ATTENTION REQUIRED",
	"restart_required": "RESTART REQUIRED", "stale": "STALE", "offline": "OFFLINE",
	"unknown": "UNKNOWN", "conflict": "CONFLICT", "fresh": "FRESH",
	"running": "RUNNING", "driving": "DRIVING", "parked": "PARKED", "stopping": "STOPPING",
	"enabled": "ENABLED", "disabled": "DISABLED", "available": "AVAILABLE",
	"unavailable": "UNAVAILABLE", "active": "ACTIVE", "present": "PRESENT",
	"none": "NONE", "controller": "CONTROLLER", "repository": "REPOSITORY",
	"run": "RUN", "onboarding": "ONBOARDING", "critical": "CRITICAL",
	"high": "HIGH", "medium": "MEDIUM", "low": "LOW", "info": "INFO",
	"not_ready": "NOT READY", "not_applicable": "NOT APPLICABLE",
	"open": "OPEN", "closed": "CLOSED", "merged": "MERGED", "opened": "OPENED", "preflight_ready": "PREFLIGHT READY", "cancelled": "CANCELLED",
	"accepted": "ACCEPTED", "waiting_for_operator": "WAITING FOR OPERATOR",
	"ready_disabled": "READY DISABLED", "received": "RECEIVED", "admitting": "ADMITTING",
	"rejected": "REJECTED", "provisioning": "PROVISIONING", "executing": "EXECUTING",
	"awaiting_human_decision": "AWAITING HUMAN DECISION", "verifying": "VERIFYING",
	"fresh_review": "FRESH REVIEW", "approval_ready": "APPROVAL READY",
	"pushing_branch": "PUSHING BRANCH", "branch_pushed": "BRANCH PUSHED",
	"opening_pr": "OPENING PR", "repairing": "REPAIRING", "pr_open": "PR OPEN",
	"reconciling_reviews": "RECONCILING REVIEWS", "replying_review_feedback": "REPLYING REVIEW FEEDBACK",
	"awaiting_human_approval": "AWAITING HUMAN APPROVAL", "merging": "MERGING",
	"awaiting_github_mergeability": "AWAITING GITHUB MERGEABILITY",
	"awaiting_linear_completion":   "AWAITING LINEAR COMPLETION", "cleaning": "CLEANING",
	"completed": "COMPLETED", "failed": "FAILED", "manual_intervention": "MANUAL INTERVENTION",
	"all": "ALL", "ended": "ENDED", "progressing": "PROGRESSING", "normal_wait": "NORMAL WAIT",
	"abnormal_wait": "ABNORMAL WAIT", "human_decision": "HUMAN DECISION", "human_approval": "HUMAN APPROVAL",
	"external_checks": "EXTERNAL CHECKS", "mergeability": "MERGEABILITY", "linear_completion": "LINEAR COMPLETION",
	"terminal": "TERMINAL", "admission": "ADMISSION", "workspace": "WORKSPACE", "implementation": "IMPLEMENTATION",
	"verification": "VERIFICATION", "review": "INDEPENDENT REVIEW", "publication": "PUBLICATION", "pull_request": "PULL REQUEST",
	"approval": "APPROVAL", "merge": "MERGE", "cleanup": "CLEANUP", "independent_review": "INDEPENDENT REVIEW",
	"branch_publication": "BRANCH PUBLICATION", "required_checks": "REQUIRED CHECKS", "review_conversations": "REVIEW CONVERSATIONS",
	"source_checkout": "SOURCE CHECKOUT", "pending": "PENDING", "passed": "PASSED", "blocked": "BLOCKED",
	"run_detail": "RUN DETAIL", "warning": "WARNING", "error": "ERROR", "observed": "OBSERVED", "succeeded": "SUCCEEDED",
	"accepting_new_work": "ACCEPTING NEW WORK", "enable_repository": "ENABLE REPOSITORY",
	"resolve_readiness": "RESOLVE READINESS", "resolve_conflict": "RESOLVE CONFLICT",
	"refresh_authority": "REFRESH AUTHORITY", "inspect_unavailability": "INSPECT UNAVAILABILITY",
	"profile_configuration": "PROFILE CONFIGURATION", "configuration_convergence": "CONFIGURATION CONVERGENCE",
	"local_checkout": "LOCAL CHECKOUT", "base_branch": "BASE BRANCH", "github_repository": "GITHUB REPOSITORY",
	"github_app": "GITHUB APP", "linear_label": "LINEAR LABEL", "verifier_policy": "VERIFIER POLICY",
}

func presentationLabel(value string) string {
	if label, ok := presentationLabels[value]; ok {
		return label
	}
	if value == "" {
		return "UNKNOWN"
	}
	return value
}

func formatHeartbeatAge(observation application.RuntimeObservation) string {
	if observation.HeartbeatAgeSeconds == nil {
		return "age unknown"
	}
	return formatAge(time.Duration(*observation.HeartbeatAgeSeconds) * time.Second)
}

func formatObservationAge(observedAt, value time.Time) string {
	if value.IsZero() || observedAt.Before(value) {
		return "age unknown"
	}
	return formatAge(observedAt.Sub(value))
}

func formatAge(age time.Duration) string {
	if age < time.Minute {
		return fmt.Sprintf("%ds ago", max(int(age/time.Second), 0))
	}
	if age < time.Hour {
		return fmt.Sprintf("%dm ago", int(age/time.Minute))
	}
	if age < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(age/time.Hour))
	}
	return fmt.Sprintf("%dd ago", int(age/(24*time.Hour)))
}

func truncateRunes(value string, width int) string {
	if width < 1 {
		return ""
	}
	return ansi.Truncate(value, width, "…")
}

func clamp(value, minimum, maximum int) int {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

var runOperatorProgram = func(model tea.Model) error {
	_, err := tea.NewProgram(model).Run()
	return err
}

func operatorCommand(args []string) error {
	flags := flag.NewFlagSet("operator", flag.ContinueOnError)
	configPath := configPathFlag(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: agentctl operator [--config <controller.json>]")
	}
	composition, err := composeOperator(context.Background(), *configPath)
	if err != nil {
		return err
	}
	defer composition.Close()
	model := newOperatorModel(context.Background(), composition.loader)
	defer model.cancel()
	return runOperatorProgram(model)
}
