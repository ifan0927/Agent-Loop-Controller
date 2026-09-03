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
	innerWidth := max(m.width-2, 1)
	lines := []string{m.renderPanelHeader("Repository lifecycle", summary, innerWidth)}
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
			lines = append(lines, m.renderRow(operatorRow{id: repository.Repository, name: repository.Repository, status: presentationLabel(string(repository.Readiness)), detail: detail, tone: repositoryRowTone(repository.Available, string(repository.Readiness))}, innerWidth, index == selected))
		}
	}
	body := lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(operatorFocusColor).Width(m.width).Height(bodyHeight).Render(strings.Join(lines, "\n"))
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
	var overview string
	if m.width >= operatorWideWidth {
		leftWidth := (m.width - 1) * 3 / 5
		rightWidth := m.width - leftWidth - 1
		overview = lipgloss.JoinHorizontal(lipgloss.Top,
			m.renderRepositoryStatusCard(detail, leftWidth),
			" ",
			m.renderRepositoryContextCard(detail, rightWidth),
		)
	} else {
		overview = m.renderRepositoryCompactCard(detail, m.width)
	}
	dimensions := m.renderRepositoryDimensionsCard(detail, m.width)
	return lipgloss.JoinVertical(lipgloss.Left, header, overview, dimensions, footer)
}

func (m operatorModel) renderRepositoryConfirmation() string {
	detail := m.repositoryDetail.detail
	header := m.renderRouteHeader("Confirm enablement", detail.Metadata.ObservedAt, false, nil)
	title := operatorWarningStyle.Bold(true).Render("▲ CONFIRM REPOSITORY ENABLEMENT")
	lines := []string{
		renderStyledHeader(title, operatorMutedStyle.Render(m.repositoryDetail.repository), max(m.width-6, 1)),
		"",
		operatorHeadingStyle.Render("Effect"),
		"Permit future admission for this repository.",
		"",
		operatorHeadingStyle.Render("Unaffected"),
		"It does not enable global automatic admission.",
		"It does not start, cancel, or modify a run.",
		"It does not start, stop, or restart the worker.",
		"",
		operatorMutedStyle.Render("Stale authority safely conflicts without changing lifecycle."),
	}
	card := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("214")).Padding(1, 2).Width(m.width).Render(strings.Join(lines, "\n"))
	return lipgloss.JoinVertical(lipgloss.Left, header, card, m.renderHelp())
}

func (m operatorModel) renderRepositoryCompactCard(detail *application.RoutineRepositoryDetail, width int) string {
	repository := detail.Repository
	innerWidth := max(width-4, 1)
	lines := []string{
		m.renderRepositoryTitle(repository, innerWidth),
		fmt.Sprintf("Conclusion %s · %s", presentationLabel(string(repository.Acceptance.Conclusion)), reasonPresentation(repository.Acceptance.ReasonCode)),
		"Next direction " + presentationLabel(string(repository.Acceptance.NextDirection)),
		fmt.Sprintf("Lifecycle %s · Readiness %s (%s)", presentationLabel(string(repository.LifecycleIntent)), presentationLabel(string(repository.Readiness)), reasonPresentation(repository.ReadinessReasonCode)),
		fmt.Sprintf("Availability %s (%s)", repositoryAvailabilityLabel(repository.Available), reasonPresentation(repository.AvailabilityReasonCode)),
		fmt.Sprintf("Configuration %s (%s)", presentationLabel(string(repository.ConfigurationConvergence)), reasonPresentation(repository.ConfigurationReasonCode)),
		"Last observed " + repository.LastObservedAt.Local().Format(time.RFC3339),
		"Active run " + repositoryActiveRunLabel(repository) + " · Onboarding " + repositoryOnboardingLabel(repository),
		m.repositoryOperationPresentation(),
	}
	return repositoryCardStyle(width, repositoryAcceptanceTone(string(repository.Acceptance.Conclusion))).Render(strings.Join(truncateStyledLines(lines, innerWidth), "\n"))
}

func (m operatorModel) renderRepositoryStatusCard(detail *application.RoutineRepositoryDetail, width int) string {
	repository := detail.Repository
	innerWidth := max(width-4, 1)
	lines := []string{
		m.renderRepositoryTitle(repository, innerWidth),
		fmt.Sprintf("Conclusion %s · %s", presentationLabel(string(repository.Acceptance.Conclusion)), reasonPresentation(repository.Acceptance.ReasonCode)),
		"Next direction " + presentationLabel(string(repository.Acceptance.NextDirection)),
		fmt.Sprintf("Lifecycle %s · Readiness %s (%s)", presentationLabel(string(repository.LifecycleIntent)), presentationLabel(string(repository.Readiness)), reasonPresentation(repository.ReadinessReasonCode)),
		fmt.Sprintf("Availability %s (%s)", repositoryAvailabilityLabel(repository.Available), reasonPresentation(repository.AvailabilityReasonCode)),
		fmt.Sprintf("Configuration %s (%s)", presentationLabel(string(repository.ConfigurationConvergence)), reasonPresentation(repository.ConfigurationReasonCode)),
		"Last observed " + repository.LastObservedAt.Local().Format(time.RFC3339),
	}
	return repositoryCardStyle(width, repositoryAcceptanceTone(string(repository.Acceptance.Conclusion))).Render(strings.Join(truncateStyledLines(lines, innerWidth), "\n"))
}

func (m operatorModel) renderRepositoryContextCard(detail *application.RoutineRepositoryDetail, width int) string {
	repository := detail.Repository
	innerWidth := max(width-4, 1)
	lines := []string{
		m.renderPanelHeader("Operational context", "LOCAL", innerWidth),
		"Active run " + repositoryActiveRunLabel(repository),
		"Onboarding " + repositoryOnboardingLabel(repository),
		operatorMutedStyle.Render("Repository-local admission · run/worker unchanged"),
		m.repositoryOperationPresentation(),
	}
	return repositoryCardStyle(width, "muted").Render(strings.Join(truncateStyledLines(lines, innerWidth), "\n"))
}

func (m operatorModel) renderRepositoryTitle(repository application.RoutineRepositorySummary, width int) string {
	badge := repositoryStatusBadge(presentationLabel(string(repository.Acceptance.Conclusion)), repositoryAcceptanceTone(string(repository.Acceptance.Conclusion)))
	return renderStyledHeader(operatorHeadingStyle.Render(repository.Repository), badge, width)
}

func (m operatorModel) renderRepositoryDimensionsCard(detail *application.RoutineRepositoryDetail, width int) string {
	innerWidth := max(width-4, 1)
	ready := 0
	for _, dimension := range detail.Dimensions {
		if string(dimension.Status) == "ready" {
			ready++
		}
	}
	lines := []string{m.renderPanelHeader("Readiness dimensions", fmt.Sprintf("%d/%d ready", ready, len(detail.Dimensions)), innerWidth)}
	selected := clamp(m.repositoryDetail.dimensionIndex, 0, max(len(detail.Dimensions)-1, 0))
	for index, dimension := range detail.Dimensions {
		marker, tone := repositoryDimensionMarker(string(dimension.Status))
		nameWidth := min(31, max(21, innerWidth/3))
		name := lipgloss.NewStyle().Width(nameWidth).Render(presentationLabel(string(dimension.Dimension)))
		content := fmt.Sprintf("%s %s  %s · %s · %s", marker, name, presentationLabel(string(dimension.Status)), reasonPresentation(dimension.ReasonCode), dimension.ObservedAt.Local().Format("15:04:05"))
		content = ansi.Truncate(content, innerWidth-2, "…")
		line := "  " + tone.Render(content)
		if index == selected {
			line = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("28")).Width(innerWidth).Render("> " + content)
		}
		lines = append(lines, line)
	}
	return lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(operatorFocusColor).Padding(0, 1).Width(width).Render(strings.Join(lines, "\n"))
}

func (m operatorModel) repositoryOperationPresentation() string {
	state := m.repositoryDetail
	if state.operationError != nil {
		prefix := "Enable failed"
		style := operatorDangerStyle
		if state.operationStage == operatorRepositoryOperationRetryable {
			prefix = "Enable uncertain · Enter retries same request"
			style = operatorWarningStyle
		}
		return style.Bold(true).Render(prefix + " · " + state.operationError.String())
	}
	if state.receipt != nil {
		return operatorAccentStyle.Bold(true).Render(fmt.Sprintf("✓ Enable %s/%s · %s v%d", presentationLabel(string(state.receipt.Phase)), presentationLabel(string(state.receipt.Outcome)), presentationLabel(state.receipt.ResultingState), state.receipt.ResultingVersion))
	}
	if m.repositoryEnableCanStart() {
		return operatorWarningStyle.Bold(true).Render("→ Action available · e enable repository")
	}
	return operatorMutedStyle.Render("○ No Repository action offered")
}

func repositoryAvailabilityLabel(available bool) string {
	if available {
		return "AVAILABLE"
	}
	return "UNAVAILABLE"
}

func repositoryActiveRunLabel(repository application.RoutineRepositorySummary) string {
	if repository.ActiveRunID == "" {
		return "none"
	}
	return repository.ActiveRunID
}

func repositoryOnboardingLabel(repository application.RoutineRepositorySummary) string {
	if repository.Onboarding == nil {
		return "none"
	}
	return fmt.Sprintf("%s · %s · %d steps", repository.Onboarding.OnboardingID, presentationLabel(string(repository.Onboarding.Status)), repository.Onboarding.CompletedStepCount)
}

func repositoryAcceptanceTone(conclusion string) string {
	switch conclusion {
	case "accepting_new_work":
		return "good"
	case "ready_disabled", "not_ready", "unknown":
		return "warning"
	case "conflict", "unavailable":
		return "danger"
	default:
		return "muted"
	}
}

func repositoryCardStyle(width int, tone string) lipgloss.Style {
	color := operatorBorderColor
	switch tone {
	case "good":
		color = operatorFocusColor
	case "warning":
		color = lipgloss.Color("214")
	case "danger":
		color = lipgloss.Color("196")
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(color).Padding(0, 1).Width(width)
}

func repositoryStatusBadge(label, tone string) string {
	style := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("230")).Padding(0, 1)
	switch tone {
	case "good":
		style = style.Background(lipgloss.Color("28"))
	case "warning":
		style = style.Foreground(lipgloss.Color("16")).Background(lipgloss.Color("214"))
	case "danger":
		style = style.Background(lipgloss.Color("124"))
	default:
		style = style.Background(lipgloss.Color("238"))
	}
	return style.Render(label)
}

func repositoryDimensionMarker(status string) (string, lipgloss.Style) {
	switch status {
	case "ready":
		return "✓", operatorAccentStyle
	case "not_ready", "unknown":
		return "▲", operatorWarningStyle
	case "conflict":
		return "✕", operatorDangerStyle
	default:
		return "○", operatorMutedStyle
	}
}

func truncateStyledLines(lines []string, width int) []string {
	for index := range lines {
		lines[index] = ansi.Truncate(lines[index], width, "…")
	}
	return lines
}

func renderStyledHeader(left, right string, width int) string {
	left = ansi.Truncate(left, width, "…")
	available := width - lipgloss.Width(left) - 1
	if available <= 0 {
		return left
	}
	right = ansi.Truncate(right, available, "…")
	gap := max(width-lipgloss.Width(left)-lipgloss.Width(right), 1)
	return left + strings.Repeat(" ", gap) + right
}
