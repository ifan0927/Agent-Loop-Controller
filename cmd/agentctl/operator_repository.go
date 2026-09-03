package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

type operatorRepositoriesRequest struct {
	cursor   string
	previous []string
}

type operatorRepositoriesResultMsg struct {
	generation int64
	request    operatorRepositoriesRequest
	page       application.RoutineRepositoryPage
	err        operatorSafeError
}

type operatorRepositoriesState struct {
	page               *application.RoutineRepositoryPage
	request            operatorRepositoriesRequest
	pending            *operatorRepositoriesRequest
	initialError       *operatorSafeError
	staleError         *operatorSafeError
	refreshing         bool
	generation         int64
	index              int
	selectedRepository string
}

type operatorRepositoryDetailResultMsg struct {
	generation int64
	repository string
	detail     application.RoutineRepositoryDetail
	err        operatorSafeError
}

type operatorRepositoryOperationResultMsg struct {
	generation int64
	repository string
	requestID  string
	result     application.RepositoryMutationResult
	err        operatorSafeError
}

type operatorRepositoryOperationStage string

const (
	operatorRepositoryOperationIdle       operatorRepositoryOperationStage = "idle"
	operatorRepositoryOperationConfirming operatorRepositoryOperationStage = "confirming"
	operatorRepositoryOperationPending    operatorRepositoryOperationStage = "pending"
	operatorRepositoryOperationRetryable  operatorRepositoryOperationStage = "retryable"
	operatorRepositoryOperationFailed     operatorRepositoryOperationStage = "failed"
	operatorRepositoryOperationSucceeded  operatorRepositoryOperationStage = "succeeded"
)

type operatorRepositoryDetailState struct {
	detail              *application.RoutineRepositoryDetail
	repository          string
	returnRoute         operatorRoute
	initialError        *operatorSafeError
	staleError          *operatorSafeError
	refreshing          bool
	generation          int64
	dimensionIndex      int
	operationStage      operatorRepositoryOperationStage
	operationGeneration int64
	requestID           string
	operationError      *operatorSafeError
	receipt             *application.OperationReceipt
}

func newOperatorRequestID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return ""
	}
	return "operator-enable-" + hex.EncodeToString(value[:])
}

func (m operatorModel) selectedRepository() string {
	if m.repositories.page == nil || len(m.repositories.page.Repositories) == 0 {
		return ""
	}
	return m.repositories.page.Repositories[clamp(m.repositories.index, 0, len(m.repositories.page.Repositories)-1)].Repository
}

func (m *operatorModel) restoreRepositorySelection(repository string) {
	if m.repositories.page == nil {
		return
	}
	m.repositories.index = clamp(m.repositories.index, 0, max(len(m.repositories.page.Repositories)-1, 0))
	for index, item := range m.repositories.page.Repositories {
		if repository != "" && item.Repository == repository {
			m.repositories.index = index
			break
		}
	}
	m.repositories.selectedRepository = m.selectedRepository()
}

func (m *operatorModel) moveRepositorySelection(delta int) {
	if m.repositories.page == nil || len(m.repositories.page.Repositories) == 0 {
		return
	}
	m.repositories.index = clamp(m.repositories.index+delta, 0, len(m.repositories.page.Repositories)-1)
	m.repositories.selectedRepository = m.selectedRepository()
}

func (m operatorModel) repositoryActionOffered() bool {
	if m.repositoryDetail.detail == nil {
		return false
	}
	for _, action := range m.repositoryDetail.detail.LegalNextActions {
		if action == application.RoutineRepositoryActionEnable {
			return true
		}
	}
	return false
}

func (m operatorModel) repositoryEnableCanStart() bool {
	if !m.repositoryActionOffered() {
		return false
	}
	return m.repositoryDetail.operationStage == operatorRepositoryOperationIdle || m.repositoryDetail.operationStage == operatorRepositoryOperationFailed
}

func (m operatorModel) renderRepositoriesScreen() string {
	footer := m.renderHelp()
	if m.repositories.page == nil {
		if m.repositories.initialError != nil {
			return boundedLines(m.width, m.height, []string{"Agent Loop Controller / Repositories", "", "Repositories unavailable", m.repositories.initialError.String(), "", "r retry · 1 Overview · q quit"})
		}
		return boundedLines(m.width, m.height, []string{"Agent Loop Controller / Repositories", "", "Loading authorized repositories…", "", "1 Overview · q quit"})
	}
	page := m.repositories.page
	header := m.renderRouteHeader("Repositories", page.Metadata.ObservedAt, m.repositories.refreshing, m.repositories.staleError)
	pageNumber := len(m.repositories.request.previous) + 1
	summary := fmt.Sprintf("Page %d · %d of %d displayed", pageNumber, len(page.Repositories), page.Collection.Total)
	bodyHeight := max(m.height-lipgloss.Height(header)-lipgloss.Height(footer)-1, 1)
	lines := []string{summary}
	if len(page.Repositories) == 0 {
		lines = append(lines, "", "No authorized repositories")
	} else {
		available := max(bodyHeight-1, 1)
		selected := clamp(m.repositories.index, 0, len(page.Repositories)-1)
		start := clamp(selected-available+1, 0, max(len(page.Repositories)-available, 0))
		end := min(start+available, len(page.Repositories))
		for index := start; index < end; index++ {
			repository := page.Repositories[index]
			availability := "unavailable"
			if repository.Available {
				availability = "available"
			}
			detail := fmt.Sprintf("%s · %s · %s · %s", presentationLabel(string(repository.LifecycleIntent)), availability, presentationLabel(string(repository.Acceptance.Conclusion)), formatObservationAge(page.Metadata.ObservedAt, repository.LastObservedAt))
			lines = append(lines, m.renderRow(operatorRow{id: repository.Repository, name: repository.Repository, status: presentationLabel(string(repository.Readiness)), detail: detail, tone: repositoryRowTone(repository.Available, string(repository.Readiness))}, m.width, index == selected))
		}
	}
	body := lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(operatorBorderColor).Width(m.width).Height(bodyHeight).Render(strings.Join(lines, "\n"))
	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

func (m operatorModel) renderRepositoryDetailScreen() string {
	footer := m.renderHelp()
	state := m.repositoryDetail
	if state.detail == nil {
		if state.initialError != nil {
			return boundedLines(m.width, m.height, []string{"Agent Loop Controller / Repository detail", "", "Repository detail unavailable", state.initialError.String(), "", "r retry · esc back · q quit"})
		}
		return boundedLines(m.width, m.height, []string{"Agent Loop Controller / Repository detail", "", "Loading repository " + state.repository + "…", "", "esc back · q quit"})
	}
	if state.operationStage == operatorRepositoryOperationConfirming {
		return m.renderRepositoryConfirmation()
	}
	detail := state.detail
	header := m.renderRouteHeader("Repository detail", detail.Metadata.ObservedAt, state.refreshing, state.staleError)
	repository := detail.Repository
	availability := "unavailable"
	if repository.Available {
		availability = "available"
	}
	lines := []string{
		repository.Repository,
		fmt.Sprintf("Conclusion %s · %s", presentationLabel(string(repository.Acceptance.Conclusion)), reasonPresentation(repository.Acceptance.ReasonCode)),
		"Next direction " + presentationLabel(string(repository.Acceptance.NextDirection)),
		fmt.Sprintf("Lifecycle %s · readiness %s (%s)", presentationLabel(string(repository.LifecycleIntent)), presentationLabel(string(repository.Readiness)), reasonPresentation(repository.ReadinessReasonCode)),
		fmt.Sprintf("Availability %s (%s)", availability, reasonPresentation(repository.AvailabilityReasonCode)),
		fmt.Sprintf("Configuration %s (%s)", presentationLabel(string(repository.ConfigurationConvergence)), reasonPresentation(repository.ConfigurationReasonCode)),
	}
	if repository.ActiveRunID != "" {
		lines = append(lines, "Active run "+repository.ActiveRunID)
	} else {
		lines = append(lines, "Active run none")
	}
	if repository.Onboarding != nil {
		lines = append(lines, fmt.Sprintf("Onboarding %s · %s · %d steps · %s", repository.Onboarding.OnboardingID, presentationLabel(string(repository.Onboarding.Status)), repository.Onboarding.CompletedStepCount, reasonPresentation(repository.Onboarding.ReasonCode)))
	} else {
		lines = append(lines, "Current onboarding none")
	}
	lines = append(lines, "Last observation "+repository.LastObservedAt.Local().Format(time.RFC3339), "Readiness dimensions")
	var tail []string
	if state.receipt != nil {
		tail = append(tail, fmt.Sprintf("Enable result %s/%s · lifecycle %s v%d", presentationLabel(string(state.receipt.Phase)), presentationLabel(string(state.receipt.Outcome)), presentationLabel(state.receipt.ResultingState), state.receipt.ResultingVersion))
	}
	if state.operationError != nil {
		prefix := "Enable failed"
		if state.operationStage == operatorRepositoryOperationRetryable {
			prefix = "Enable result uncertain; Enter retries the same request"
		}
		tail = append(tail, prefix+" · "+state.operationError.String())
	} else if m.repositoryActionOffered() && state.operationStage == operatorRepositoryOperationIdle {
		tail = append(tail, "Action available: e enable repository")
	}
	dimensionSlots := max(m.height-lipgloss.Height(header)-lipgloss.Height(footer)-len(lines)-len(tail)-2, 1)
	selected := clamp(state.dimensionIndex, 0, max(len(detail.Dimensions)-1, 0))
	start := clamp(selected-dimensionSlots+1, 0, max(len(detail.Dimensions)-dimensionSlots, 0))
	end := min(start+dimensionSlots, len(detail.Dimensions))
	for index := start; index < end; index++ {
		dimension := detail.Dimensions[index]
		prefix := "  "
		if index == selected {
			prefix = "> "
		}
		line := fmt.Sprintf("%s%s · %s · %s · %s", prefix, presentationLabel(string(dimension.Dimension)), presentationLabel(string(dimension.Status)), reasonPresentation(dimension.ReasonCode), dimension.ObservedAt.Local().Format(time.RFC3339))
		lines = append(lines, line)
	}
	lines = append(lines, tail...)
	for index := range lines {
		lines[index] = ansi.Truncate(lines[index], m.width, "…")
	}
	return lipgloss.JoinVertical(lipgloss.Left, header, strings.Join(lines, "\n"), footer)
}

func (m operatorModel) renderRepositoryConfirmation() string {
	repository := m.repositoryDetail.repository
	lines := []string{
		"Agent Loop Controller / Confirm repository enablement",
		"",
		"Repository " + repository,
		"",
		"Enabling permits future admission for this repository.",
		"It does not enable global automatic admission.",
		"It does not start, cancel, or modify a run.",
		"It does not start, stop, or restart the worker.",
		"Stale authority may produce a safe conflict without changing lifecycle.",
		"",
		"Enter confirm · Esc cancel · q quit",
	}
	return boundedLines(m.width, m.height, lines)
}
