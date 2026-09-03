package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

const operatorNeutralDecisionInstructions = "No additional instructions."

type operatorDecisionStage string

const (
	operatorDecisionIdle       operatorDecisionStage = "idle"
	operatorDecisionSelecting  operatorDecisionStage = "selecting"
	operatorDecisionEditing    operatorDecisionStage = "editing"
	operatorDecisionReviewing  operatorDecisionStage = "reviewing"
	operatorDecisionConfirming operatorDecisionStage = "confirming"
	operatorDecisionPending    operatorDecisionStage = "pending"
	operatorDecisionRetryable  operatorDecisionStage = "retryable"
	operatorDecisionConflicted operatorDecisionStage = "conflicted"
	operatorDecisionFailed     operatorDecisionStage = "failed"
	operatorDecisionSucceeded  operatorDecisionStage = "succeeded"
)

type operatorDecisionState struct {
	stage               operatorDecisionStage
	optionIndex         int
	requestFocus        operatorDecisionRequestFocus
	requestOffset       int
	reviewOffset        int
	offerID             string
	payload             application.LegalDecisionInput
	editor              textarea.Model
	operationGeneration int64
	receipt             *application.OperationReceipt
	operationError      *operatorSafeError
}

type operatorDecisionRequestFocus string

const (
	operatorDecisionRequestContent operatorDecisionRequestFocus = "content"
	operatorDecisionRequestOptions operatorDecisionRequestFocus = "options"
)

type operatorDecisionResultMsg struct {
	generation int64
	runID      string
	offerID    string
	payload    application.LegalDecisionInput
	receipt    application.OperationReceipt
	err        operatorSafeError
}

func newOperatorDecisionState() operatorDecisionState {
	editor := textarea.New()
	editor.Placeholder = "Optional bounded clarification"
	editor.ShowLineNumbers = false
	editor.CharLimit = 1 << 20
	editor.SetWidth(72)
	editor.SetHeight(6)
	return operatorDecisionState{stage: operatorDecisionIdle, requestFocus: operatorDecisionRequestContent, editor: editor}
}

func (m *operatorModel) resizeDecisionEditor() {
	m.decision.editor.SetWidth(max(m.width-8, 20))
	m.decision.editor.SetHeight(clamp(m.height/4, 3, 7))
}

func (m operatorModel) currentDecisionOffer() (application.LegalActionOffer, bool) {
	if m.detail.detail == nil || m.detail.detail.Run.State != domain.StateAwaitingHumanDecision || m.detail.detail.Decision == nil {
		return application.LegalActionOffer{}, false
	}
	for _, offer := range m.detail.detail.Offers {
		if offer.Action == application.OperationDecide && offer.Scope == application.ScopeRun && offer.TargetID == m.detail.runID {
			return offer, true
		}
	}
	return application.LegalActionOffer{}, false
}

func (m operatorModel) decisionCanStart() bool {
	offer, offered := m.currentDecisionOffer()
	if !offered {
		return false
	}
	if m.decision.stage == operatorDecisionSucceeded {
		return offer.OfferID != m.decision.offerID
	}
	return m.decision.stage == operatorDecisionIdle || m.decision.stage == operatorDecisionFailed || m.decision.stage == operatorDecisionConflicted
}

func (m operatorModel) startDecisionFlow() (tea.Model, tea.Cmd) {
	offer, ok := m.currentDecisionOffer()
	if !ok {
		return m, nil
	}
	m.detail.generation++
	m.detail.refreshing = false
	nextGeneration := m.decision.operationGeneration + 1
	m.decision = newOperatorDecisionState()
	m.resizeDecisionEditor()
	m.decision.stage = operatorDecisionSelecting
	m.decision.offerID = offer.OfferID
	m.decision.operationGeneration = nextGeneration
	return m, nil
}

func (s operatorDecisionState) interceptsKeys() bool {
	switch s.stage {
	case operatorDecisionSelecting, operatorDecisionEditing, operatorDecisionReviewing, operatorDecisionConfirming,
		operatorDecisionPending, operatorDecisionRetryable, operatorDecisionConflicted, operatorDecisionFailed:
		return true
	default:
		return false
	}
}

func (s operatorDecisionState) blocksRefresh() bool {
	return s.interceptsKeys()
}

func (m operatorModel) updateDecisionFlow(msg tea.KeyPressMsg) (operatorModel, tea.Cmd, bool) {
	if m.decision.stage != operatorDecisionEditing && key.Matches(msg, operatorKeys.quit) {
		return m, nil, false
	}
	if m.decision.stage != operatorDecisionPending && (key.Matches(msg, operatorKeys.overview) || key.Matches(msg, operatorKeys.runs) || key.Matches(msg, operatorKeys.attention) || key.Matches(msg, operatorKeys.repositories)) {
		m.cancelDecisionFlow()
		return m, nil, false
	}
	if m.detail.detail == nil || m.detail.detail.Decision == nil {
		m.cancelDecisionFlow()
		return m, nil, true
	}
	request := m.detail.detail.Decision
	switch m.decision.stage {
	case operatorDecisionSelecting:
		switch {
		case key.Matches(msg, operatorKeys.back):
			m.cancelDecisionFlow()
		case key.Matches(msg, operatorKeys.next), key.Matches(msg, operatorKeys.previous):
			if m.decision.requestFocus == operatorDecisionRequestContent {
				m.decision.requestFocus = operatorDecisionRequestOptions
				m.ensureSelectedDecisionOptionVisible()
			} else {
				m.decision.requestFocus = operatorDecisionRequestContent
			}
		case key.Matches(msg, operatorKeys.up):
			if m.decision.requestFocus == operatorDecisionRequestContent {
				m.decision.requestOffset = clamp(m.decision.requestOffset-1, 0, m.maximumDecisionRequestOffset())
			} else {
				m.decision.optionIndex = clamp(m.decision.optionIndex-1, 0, len(request.Options)-1)
				m.ensureSelectedDecisionOptionVisible()
			}
		case key.Matches(msg, operatorKeys.down):
			if m.decision.requestFocus == operatorDecisionRequestContent {
				m.decision.requestOffset = clamp(m.decision.requestOffset+1, 0, m.maximumDecisionRequestOffset())
			} else {
				m.decision.optionIndex = clamp(m.decision.optionIndex+1, 0, len(request.Options)-1)
				m.ensureSelectedDecisionOptionVisible()
			}
		case key.Matches(msg, operatorKeys.open) && m.decision.requestFocus == operatorDecisionRequestContent:
			m.decision.requestFocus = operatorDecisionRequestOptions
			m.ensureSelectedDecisionOptionVisible()
		case key.Matches(msg, operatorKeys.open) && len(request.Options) != 0:
			m.decision.payload.ChoiceID = request.Options[m.decision.optionIndex].ID
			m.decision.stage = operatorDecisionEditing
			return m, m.decision.editor.Focus(), true
		}
		return m, nil, true
	case operatorDecisionEditing:
		if key.Matches(msg, operatorKeys.back) {
			m.decision.editor.Blur()
			m.decision.stage = operatorDecisionSelecting
			return m, nil, true
		}
		if msg.Code == 's' && msg.Mod&tea.ModCtrl != 0 {
			instructions := strings.TrimSpace(m.decision.editor.Value())
			if instructions == "" {
				instructions = operatorNeutralDecisionInstructions
			}
			m.decision.payload.Instructions = instructions
			m.decision.editor.Blur()
			m.decision.stage = operatorDecisionReviewing
			return m, nil, true
		}
		var command tea.Cmd
		m.decision.editor, command = m.decision.editor.Update(msg)
		return m, command, true
	case operatorDecisionReviewing:
		if key.Matches(msg, operatorKeys.up) {
			m.decision.reviewOffset = clamp(m.decision.reviewOffset-1, 0, m.maximumDecisionReviewOffset())
			return m, nil, true
		}
		if key.Matches(msg, operatorKeys.down) {
			m.decision.reviewOffset = clamp(m.decision.reviewOffset+1, 0, m.maximumDecisionReviewOffset())
			return m, nil, true
		}
		if key.Matches(msg, operatorKeys.back) {
			m.decision.stage = operatorDecisionEditing
			return m, m.decision.editor.Focus(), true
		}
		if key.Matches(msg, operatorKeys.open) {
			m.decision.stage = operatorDecisionConfirming
		}
		return m, nil, true
	case operatorDecisionConfirming:
		if key.Matches(msg, operatorKeys.up) {
			m.decision.reviewOffset = clamp(m.decision.reviewOffset-1, 0, m.maximumDecisionReviewOffset())
			return m, nil, true
		}
		if key.Matches(msg, operatorKeys.down) {
			m.decision.reviewOffset = clamp(m.decision.reviewOffset+1, 0, m.maximumDecisionReviewOffset())
			return m, nil, true
		}
		if key.Matches(msg, operatorKeys.back) {
			m.decision.stage = operatorDecisionReviewing
			return m, nil, true
		}
		if key.Matches(msg, operatorKeys.open) {
			m.decision.operationGeneration++
			m.decision.stage = operatorDecisionPending
			m.decision.operationError, m.decision.receipt = nil, nil
			return m, m.decisionSubmissionCommand(), true
		}
		return m, nil, true
	case operatorDecisionPending:
		return m, nil, true
	case operatorDecisionRetryable:
		if key.Matches(msg, operatorKeys.open) {
			m.decision.operationGeneration++
			m.decision.stage = operatorDecisionPending
			m.decision.operationError = nil
			return m, m.decisionSubmissionCommand(), true
		}
		if key.Matches(msg, operatorKeys.back) {
			m.cancelDecisionFlow()
		}
		return m, nil, true
	case operatorDecisionConflicted, operatorDecisionFailed:
		if key.Matches(msg, operatorKeys.back) {
			m.cancelDecisionFlow()
		}
		return m, nil, true
	default:
		return m, nil, false
	}
}

func (m *operatorModel) cancelDecisionFlow() {
	nextGeneration := m.decision.operationGeneration + 1
	m.decision = newOperatorDecisionState()
	m.resizeDecisionEditor()
	m.decision.operationGeneration = nextGeneration
}

func (m operatorModel) decisionSubmissionCommand() tea.Cmd {
	ctx, loader := m.ctx, m.loader
	generation, runID, offerID, payload := m.decision.operationGeneration, m.detail.runID, m.decision.offerID, m.decision.payload
	return func() tea.Msg {
		receipt, err := loader.AcceptDecision(ctx, offerID, payload)
		if err != nil {
			return operatorDecisionResultMsg{generation: generation, runID: runID, offerID: offerID, payload: payload, err: safeOperatorError(err)}
		}
		return operatorDecisionResultMsg{generation: generation, runID: runID, offerID: offerID, payload: payload, receipt: receipt}
	}
}

func (m operatorModel) applyDecisionResult(msg operatorDecisionResultMsg) (tea.Model, tea.Cmd) {
	state := &m.decision
	if m.route != operatorRunDetailRoute || state.stage != operatorDecisionPending || msg.generation != state.operationGeneration || msg.runID != m.detail.runID || msg.offerID != state.offerID || msg.payload != state.payload {
		return m, nil
	}
	if msg.err.Message != "" {
		state.operationError = &msg.err
		switch msg.err.Category {
		case application.ErrorUnavailable, application.ErrorInternal:
			state.stage = operatorDecisionRetryable
		case application.ErrorConflict:
			state.stage = operatorDecisionConflicted
		default:
			state.stage = operatorDecisionFailed
		}
		return m, nil
	}
	receipt := msg.receipt
	state.receipt, state.operationError = &receipt, nil
	switch receipt.Outcome {
	case application.OperationOutcomeSucceeded:
		state.stage = operatorDecisionSucceeded
		return m, m.startDetailRefresh()
	case application.OperationOutcomePending, application.OperationOutcomeAmbiguous:
		state.stage = operatorDecisionRetryable
		state.operationError = &operatorSafeError{Category: application.ErrorUnavailable, Message: "decision outcome is uncertain"}
	case application.OperationOutcomeConflict:
		state.stage = operatorDecisionConflicted
		state.operationError = &operatorSafeError{Category: application.ErrorConflict, Message: "decision did not retain current authority"}
	default:
		state.stage = operatorDecisionFailed
		state.operationError = &operatorSafeError{Category: application.ErrorInternal, Message: "decision did not succeed"}
	}
	return m, nil
}

func (m operatorModel) renderDecisionFlowScreen() string {
	detail := m.detail.detail
	header := m.renderRouteHeader("Human decision", detail.Metadata.ObservedAt, false, nil)
	request := detail.Decision
	if request == nil {
		return boundedLines(m.width, m.height, []string{header, "Decision request is no longer current.", "", "esc return"})
	}
	var lines []string
	switch m.decision.stage {
	case operatorDecisionSelecting:
		lines = m.renderDecisionRequest(request)
	case operatorDecisionEditing:
		lines = []string{
			operatorHeadingStyle.Render("Additional instructions (optional)"),
			operatorWarningStyle.Render("Bounded clarification only; this cannot change the task or grant new authority."),
			"",
			m.decision.editor.View(),
		}
	case operatorDecisionReviewing, operatorDecisionConfirming:
		lines = m.visibleDecisionReviewLines()
	case operatorDecisionPending:
		lines = []string{operatorWarningStyle.Bold(true).Render("Submitting decision…"), "", "The TUI is recording the authorized decision only.", "Worker execution is not started from this process."}
	case operatorDecisionRetryable, operatorDecisionConflicted, operatorDecisionFailed:
		title := "Decision failed"
		if m.decision.stage == operatorDecisionRetryable {
			title = "Decision outcome uncertain"
		} else if m.decision.stage == operatorDecisionConflicted {
			title = "Decision authority conflicted"
		}
		lines = []string{operatorDangerStyle.Bold(true).Render(title), ""}
		if m.decision.operationError != nil {
			lines = append(lines, m.decision.operationError.String())
		}
		if m.decision.receipt != nil {
			lines = append(lines, "Receipt "+decisionReceiptLabel(*m.decision.receipt))
		}
	}
	contentHeight := max(m.height-lipgloss.Height(header)-1, 1)
	body := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("214")).Padding(1, 2).Width(m.width).Height(contentHeight).Render(strings.Join(truncateStyledLines(lines, max(m.width-6, 1)), "\n"))
	return lipgloss.JoinVertical(lipgloss.Left, header, body, m.renderDecisionHelp())
}

func (m operatorModel) renderDecisionRequest(request *application.RoutineDecisionRequest) []string {
	lines, _ := m.decisionRequestLines(request)
	capacity := m.decisionBodyLineCapacity()
	start := clamp(m.decision.requestOffset, 0, max(len(lines)-capacity, 0))
	return lines[start:min(start+capacity, len(lines))]
}

func (m operatorModel) decisionRequestLines(request *application.RoutineDecisionRequest) ([]string, []int) {
	width := max(m.width-8, 20)
	lines := []string{
		operatorWarningStyle.Bold(true).Render("UNTRUSTED DECISION REQUEST"),
	}
	lines = append(lines, wrappedDecisionLines("Question: ", request.Question, width)...)
	lines = append(lines, wrappedDecisionLines("Context: ", request.Context, width)...)
	lines = append(lines, wrappedDecisionLines("Blocking reason: ", request.BlockingReason, width)...)
	lines = append(lines, wrappedDecisionLiteralLines("Recommendation option ID (untrusted): ", request.Recommendation, width)...)
	lines = append(lines, "", operatorHeadingStyle.Render("Persisted options"))
	optionStarts := make([]int, 0, len(request.Options))
	for index, option := range request.Options {
		optionStarts = append(optionStarts, len(lines))
		prefix := "  "
		if m.decision.requestFocus == operatorDecisionRequestOptions && index == m.decision.optionIndex {
			prefix = "> "
		}
		lines = append(lines, wrappedDecisionOptionLines(prefix, option.ID, option.Description, width)...)
	}
	return lines, optionStarts
}

func (m operatorModel) renderDecisionHelp() string {
	var value string
	switch m.decision.stage {
	case operatorDecisionSelecting:
		if m.decision.requestFocus == operatorDecisionRequestContent {
			value = "↑/↓ read request · tab or enter options · esc cancel · q quit"
		} else {
			value = "↑/↓ select persisted option · tab request · enter continue · esc cancel · q quit"
		}
	case operatorDecisionEditing:
		value = "type optional instructions · ctrl+s review · esc back · ctrl+c quit"
	case operatorDecisionReviewing:
		value = "↑/↓ review exact input · enter confirmation · esc edit · q quit"
	case operatorDecisionConfirming:
		value = "↑/↓ review exact input · enter confirm and submit · esc review · q quit"
	case operatorDecisionPending:
		value = "decision pending · q quit"
	case operatorDecisionRetryable:
		value = "enter retry exact payload · esc dismiss · q quit"
	default:
		value = "esc dismiss · q quit"
	}
	return " " + ansi.Truncate(value, max(m.width-2, 1), "…") + " "
}

func wrappedDecisionLines(prefix, value string, width int) []string {
	plain := prefix + operatorDecisionText(value)
	return strings.Split(ansi.Wrap(plain, width, ""), "\n")
}

func wrappedDecisionLiteralLines(prefix, value string, width int) []string {
	return strings.Split(ansi.Wrap(prefix+operatorDecisionLiteral(value), width, ""), "\n")
}

func wrappedDecisionOptionLines(prefix, id, description string, width int) []string {
	return strings.Split(ansi.Wrap(prefix+operatorDecisionLiteral(id)+" — "+operatorDecisionText(description), width, ""), "\n")
}

func (m operatorModel) decisionBodyLineCapacity() int {
	return max(m.height-7, 1)
}

func (m operatorModel) maximumDecisionRequestOffset() int {
	if m.detail.detail == nil || m.detail.detail.Decision == nil {
		return 0
	}
	lines, _ := m.decisionRequestLines(m.detail.detail.Decision)
	return max(len(lines)-m.decisionBodyLineCapacity(), 0)
}

func (m *operatorModel) ensureSelectedDecisionOptionVisible() {
	if m.detail.detail == nil || m.detail.detail.Decision == nil {
		return
	}
	lines, starts := m.decisionRequestLines(m.detail.detail.Decision)
	if len(starts) == 0 {
		return
	}
	selected := clamp(m.decision.optionIndex, 0, len(starts)-1)
	start, capacity := starts[selected], m.decisionBodyLineCapacity()
	if start < m.decision.requestOffset {
		m.decision.requestOffset = start
	} else if start >= m.decision.requestOffset+capacity {
		m.decision.requestOffset = start - capacity + 1
	}
	m.decision.requestOffset = clamp(m.decision.requestOffset, 0, max(len(lines)-capacity, 0))
}

func (m operatorModel) decisionReviewLines() []string {
	title := "Review exact decision"
	if m.decision.stage == operatorDecisionConfirming {
		title = "CONFIRM HUMAN DECISION"
	}
	width := max(m.width-8, 20)
	lines := []string{
		operatorWarningStyle.Bold(true).Render(title),
		"",
		"Selected option ID (exact JSON string):",
	}
	lines = append(lines, strings.Split(ansi.Wrap(operatorDecisionLiteral(m.decision.payload.ChoiceID), width, ""), "\n")...)
	lines = append(lines, "Effective instructions (exact JSON string):")
	lines = append(lines, strings.Split(ansi.Wrap(operatorDecisionLiteral(m.decision.payload.Instructions), width, ""), "\n")...)
	return append(lines, "", operatorMutedStyle.Render("Submission uses the current opaque Controller offer. The worker resumes separately."))
}

func (m operatorModel) visibleDecisionReviewLines() []string {
	lines, capacity := m.decisionReviewLines(), m.decisionBodyLineCapacity()
	start := clamp(m.decision.reviewOffset, 0, max(len(lines)-capacity, 0))
	return lines[start:min(start+capacity, len(lines))]
}

func (m operatorModel) maximumDecisionReviewOffset() int {
	return max(len(m.decisionReviewLines())-m.decisionBodyLineCapacity(), 0)
}

func operatorDecisionText(value string) string {
	value = ansi.Strip(value)
	value = strings.Map(func(char rune) rune {
		if unicode.IsControl(char) {
			return ' '
		}
		return char
	}, value)
	return strings.Join(strings.Fields(value), " ")
}

func operatorDecisionLiteral(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func decisionReceiptLabel(receipt application.OperationReceipt) string {
	return fmt.Sprintf("%s/%s · %s", presentationLabel(string(receipt.Phase)), presentationLabel(string(receipt.Outcome)), presentationLabel(receipt.ResultingState))
}

func (m operatorModel) decisionRunDetailLines() []string {
	if m.detail.detail == nil {
		return nil
	}
	detail := m.detail.detail
	var lines []string
	if detail.Decision != nil {
		lines = append(lines, operatorWarningStyle.Bold(true).Render("UNTRUSTED DECISION REQUEST · persisted options only"))
		lines = append(lines,
			"Question "+operatorDecisionText(detail.Decision.Question),
			"Context "+operatorDecisionText(detail.Decision.Context),
			"Blocking "+operatorDecisionText(detail.Decision.BlockingReason),
			"Recommendation option ID "+operatorDecisionLiteral(detail.Decision.Recommendation),
		)
		optionIDs := make([]string, 0, len(detail.Decision.Options))
		for _, option := range detail.Decision.Options {
			optionIDs = append(optionIDs, operatorDecisionLiteral(option.ID))
		}
		lines = append(lines, "Options "+strings.Join(optionIDs, " · "))
		if m.decisionCanStart() {
			lines = append(lines, operatorWarningStyle.Bold(true).Render("→ Action available · d decide"))
		}
	}
	if m.decision.receipt != nil {
		lines = append(lines, operatorAccentStyle.Bold(true).Render("✓ Decision "+decisionReceiptLabel(*m.decision.receipt)))
	}
	if detail.DecisionHandoff != nil {
		if detail.DecisionHandoff.Status == application.RoutineDecisionWorkerResumed {
			lines = append(lines, operatorAccentStyle.Render("Worker resume observed from durable attempt evidence"))
		} else {
			lines = append(lines, operatorWarningStyle.Render("Decision accepted · waiting for worker resume observation"))
		}
	}
	return lines
}
