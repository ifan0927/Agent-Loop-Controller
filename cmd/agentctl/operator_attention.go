package main

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

func (m operatorModel) renderAttentionScreen() string {
	footer := m.renderHelp()
	if m.attention.page == nil {
		if m.attention.initialError != nil {
			return boundedLines(m.width, m.height, []string{"Agent Loop Controller / Attention", "", "Attention unavailable", m.attention.initialError.String(), "", "r retry · 1 Overview · 2 Runs · q quit"})
		}
		return boundedLines(m.width, m.height, []string{"Agent Loop Controller / Attention", "", "Loading complete authorized Attention inbox…", "", "1 Overview · 2 Runs · q quit"})
	}
	header := m.renderRouteHeader("Attention", m.attention.page.Metadata.ObservedAt, m.attention.refreshing, m.attention.staleError)
	bodyHeight := max(m.height-lipgloss.Height(header)-lipgloss.Height(footer), 2)
	var body string
	if m.width >= operatorWideWidth {
		listWidth := m.width * 11 / 20
		body = lipgloss.JoinHorizontal(lipgloss.Top,
			m.renderAttentionList(listWidth, bodyHeight),
			m.renderAttentionSummary(m.width-listWidth-1, bodyHeight),
		)
	} else {
		listHeight := min(max(bodyHeight*2/5, 7), bodyHeight-5)
		body = lipgloss.JoinVertical(lipgloss.Left,
			m.renderAttentionList(m.width, listHeight),
			m.renderAttentionSummary(m.width, bodyHeight-listHeight),
		)
	}
	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

func (m operatorModel) renderAttentionList(width, height int) string {
	page := m.attention.page
	innerWidth, innerHeight := max(width-2, 1), max(height-2, 1)
	pageNumber := len(m.attention.request.previous) + 1
	title := fmt.Sprintf("Active Attention · page %d · %d of %d displayed", pageNumber, len(page.Items), page.Collection.Total)
	lines := []string{ansi.Truncate(title, innerWidth, "…")}
	if len(page.Items) == 0 {
		lines = append(lines, "No active Attention items")
	} else {
		reserve := 0
		if len(page.RecentlyHandled) != 0 {
			reserve = min(len(page.RecentlyHandled)+1, 3)
		}
		available := max(innerHeight-1-reserve, 1)
		selected := clamp(m.attention.index, 0, len(page.Items)-1)
		start := clamp(selected-available+1, 0, max(len(page.Items)-available, 0))
		end := min(start+available, len(page.Items))
		for index := start; index < end; index++ {
			item := page.Items[index]
			identity := item.TargetID
			if item.LinearIdentifier != "" {
				identity = item.LinearIdentifier + " / " + item.TargetID
			}
			offer := "no action"
			if len(item.Offers) != 0 {
				offer = "CLI action"
				for _, current := range item.Offers {
					if current.Action == "decide" && current.InputKind == "decision" {
						offer = "decision ready"
						break
					}
				}
			}
			identity = presentationLabel(string(item.Scope)) + " " + identity
			row := operatorRow{
				id: item.EventID, name: identity,
				status: presentationLabel(item.Severity) + "/" + presentationLabel(string(item.AttentionState)),
				detail: item.EventType + " · " + reasonPresentation(item.ReasonCode) + " · " + offer + " · " + formatObservationAge(page.Metadata.ObservedAt, item.OccurredAt),
				tone:   severityTone(item.Severity),
			}
			lines = append(lines, m.renderRow(row, innerWidth, m.attention.focus == operatorAttentionListFocus && index == selected))
		}
	}
	if len(page.RecentlyHandled) != 0 && len(lines) < innerHeight {
		lines = append(lines, operatorAccentStyle.Render("Recently handled"))
		for _, handled := range page.RecentlyHandled {
			if len(lines) >= innerHeight {
				break
			}
			lines = append(lines, ansi.Truncate(formatHandledAttention(handled, page.Metadata.ObservedAt), innerWidth, "…"))
		}
	}
	style := lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(operatorBorderColor).Width(width).Height(height)
	if m.attention.focus == operatorAttentionListFocus && len(page.Items) != 0 {
		style = style.BorderForeground(operatorFocusColor)
	}
	return style.Render(strings.Join(lines, "\n"))
}

func (m operatorModel) renderAttentionSummary(width, height int) string {
	innerWidth, innerHeight := max(width-2, 1), max(height-2, 1)
	lines := []string{"Selected item"}
	item, ok := m.selectedAttentionItem()
	if !ok {
		lines = append(lines, "No active Attention item selected", "No Controller action required")
	} else {
		lines = append(lines,
			"Event "+item.EventID,
			"Family "+item.EventType,
			fmt.Sprintf("Scope %s · target %s", presentationLabel(string(item.Scope)), item.TargetID),
		)
		if item.RunID != "" {
			lines = append(lines, "Run "+item.RunID)
		}
		if item.LinearIdentifier != "" {
			lines = append(lines, "Linear "+item.LinearIdentifier)
		}
		if item.Repository != "" {
			lines = append(lines, "Repository "+item.Repository)
		}
		lines = append(lines,
			fmt.Sprintf("Controller %s · Attention %s", presentationLabel(item.ControllerState), presentationLabel(string(item.AttentionState))),
			fmt.Sprintf("Severity %s · reason %s", presentationLabel(item.Severity), reasonPresentation(item.ReasonCode)),
			"Occurred "+item.OccurredAt.Local().Format(time.RFC3339),
			"Observed "+item.ObservedAt.Local().Format(time.RFC3339),
		)
		if len(item.Offers) == 0 {
			lines = append(lines, "No Controller action offered")
			if item.EventType == "cleanup_residue_attention" {
				lines = append(lines, "Inspect the run for deleted and retained cleanup results.")
			}
		} else {
			lines = append(lines, "Primary action")
			for _, offer := range item.Offers {
				if offer.Action == "decide" && offer.InputKind == "decision" {
					lines = append(lines,
						operatorWarningStyle.Bold(true).Render("→ Enter handle human decision"),
						"Read the bounded request, move to options, then use ↑/↓ and Enter.",
					)
				} else {
					lines = append(lines,
						fmt.Sprintf("- %s · %s", operatorActionPresentation(string(offer.Action)), reasonPresentation(offer.Reason)),
						operatorMutedStyle.Render("  CLI only in this TUI release · Enter inspects the run"),
					)
				}
			}
		}
		if item.Navigation == "run_detail" && len(item.Offers) == 0 {
			lines = append(lines, "Enter inspect run")
		}
	}
	if len(m.attention.page.RecentlyHandled) != 0 {
		handled := m.attention.page.RecentlyHandled[0]
		lines = append(lines, "", operatorAccentStyle.Render("Recently handled"),
			formatHandledAttention(handled, m.attention.page.Metadata.ObservedAt),
			"Action record "+handled.Action.ActionID,
		)
	}
	available := max(innerHeight, 1)
	maxOffset := max(len(lines)-available, 0)
	offset := clamp(m.attention.summaryOffset, 0, maxOffset)
	end := min(offset+available, len(lines))
	visible := append([]string(nil), lines[offset:end]...)
	for index := range visible {
		visible[index] = ansi.Truncate(visible[index], innerWidth, "…")
	}
	style := lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(operatorBorderColor).Width(width).Height(height)
	if m.attention.focus == operatorAttentionSummaryFocus {
		style = style.BorderForeground(operatorFocusColor)
	}
	return style.Render(strings.Join(visible, "\n"))
}

func formatHandledAttention(item application.RoutineHandledAttentionItem, observedAt time.Time) string {
	identity := item.RunID
	if item.LinearIdentifier != "" {
		identity = item.LinearIdentifier
	}
	return fmt.Sprintf("✓ %s · %s %s → %s · %s", identity, operatorActionPresentation(string(item.Action.Action)), presentationLabel(item.Action.ResultStatus), presentationLabel(item.Action.ResultingState), formatObservationAge(observedAt, item.Action.ObservedAt))
}
