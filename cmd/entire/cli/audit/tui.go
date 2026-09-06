package audit

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type activeTab int

const (
	tabOverview activeTab = iota
	tabIntents
	tabRisks
	tabHandoff
)

type tuiModel struct {
	result   *AuditResult
	active   activeTab
	width    int
	height   int
	quitting bool
}

func newTUIModel(res *AuditResult) tuiModel {
	return tuiModel{
		result: res,
		active: tabOverview,
	}
}

func (m tuiModel) Init() tea.Cmd {
	return nil
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			m.quitting = true
			return m, tea.Quit
		case "tab", "l", "right":
			m.active = (m.active + 1) % 4
		case "shift+tab", "h", "left":
			m.active = (m.active + 3) % 4
		case "1":
			m.active = tabOverview
		case "2":
			m.active = tabIntents
		case "3":
			m.active = tabRisks
		case "4":
			m.active = tabHandoff
		}
	}
	return m, nil
}

func (m tuiModel) View() tea.View {
	v := tea.View{}
	if m.quitting {
		v.SetContent("Audit Dashboard closed.\n")
		return v
	}

	var sb strings.Builder

	tabStyle := lipgloss.NewStyle().Padding(0, 2).Foreground(lipgloss.Color("246"))
	activeTabStyle := lipgloss.NewStyle().Padding(0, 2).Bold(true).Background(lipgloss.Color("63")).Foreground(lipgloss.Color("255"))

	tabs := []string{"[1] Overview", "[2] Intents", "[3] Risks", "[4] Handoff"}
	var renderedTabs []string
	for i, t := range tabs {
		if activeTab(i) == m.active {
			renderedTabs = append(renderedTabs, activeTabStyle.Render(t))
		} else {
			renderedTabs = append(renderedTabs, tabStyle.Render(t))
		}
	}

	sb.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("86")).Render("ENTIRE CHECKPOINT AUDIT DASHBOARD") + "\n")
	sb.WriteString(strings.Join(renderedTabs, " ") + "\n\n")

	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("63")).
		Padding(1, 2)

	var paneContent string
	switch m.active {
	case tabOverview:
		paneContent = renderOverviewTab(m.result)
	case tabIntents:
		paneContent = renderIntentsTab(m.result)
	case tabRisks:
		paneContent = renderRisksTab(m.result)
	case tabHandoff:
		paneContent = renderHandoffTab(m.result)
	}

	sb.WriteString(boxStyle.Render(paneContent) + "\n\n")
	sb.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("Use [Tab] / [1-4] to switch tabs • [q] to exit"))

	v.SetContent(sb.String())
	return v
}

func renderOverviewTab(res *AuditResult) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Branch:            %s\n", res.BranchName))
	sb.WriteString(fmt.Sprintf("Head Commit:       %s\n", res.HeadCommit))
	sb.WriteString(fmt.Sprintf("Checkpoints Count: %d committed sessions\n\n", res.CheckpointsCount))

	badgeColor := "42"
	if res.ReadinessScore < 80 {
		badgeColor = "214"
	}
	if res.ReadinessScore < 60 {
		badgeColor = "196"
	}

	scoreStr := lipgloss.NewStyle().Bold(true).Background(lipgloss.Color(badgeColor)).Foreground(lipgloss.Color("0")).Padding(0, 1).
		Render(fmt.Sprintf("READINESS SCORE: %d / 100 (%s)", res.ReadinessScore, res.ReadinessGrade))

	sb.WriteString(scoreStr + "\n\n")
	sb.WriteString("Graph Evidence Summary:\n")
	for _, ge := range res.GraphEvidence {
		sb.WriteString(fmt.Sprintf(" • %s\n", ge))
	}
	return sb.String()
}

func renderIntentsTab(res *AuditResult) string {
	var sb strings.Builder
	sb.WriteString("INTENT VERIFICATION MATRIX\n\n")
	for _, item := range res.Intents {
		sb.WriteString(fmt.Sprintf(" • [%s] %s: %s\n   Reason: %s\n\n",
			item.Status, item.ID, item.Prompt, item.Reasoning))
	}
	return sb.String()
}

func renderRisksTab(res *AuditResult) string {
	var sb strings.Builder
	sb.WriteString("IDENTIFIED RISKS & AUDIT WARNINGS\n\n")
	if len(res.Risks) == 0 {
		sb.WriteString("✔ No risks found.")
	} else {
		for _, r := range res.Risks {
			sb.WriteString(fmt.Sprintf(" • [%s] %s: %s\n   Location: %s\n   Details:  %s\n\n",
				r.Severity, r.ID, r.Title, r.Location, r.Description))
		}
	}
	return sb.String()
}

func renderHandoffTab(res *AuditResult) string {
	var sb strings.Builder
	sb.WriteString("DEVELOPER & AGENT HANDOFF PACKAGE\n\n")
	sb.WriteString(fmt.Sprintf("Goal: %s\n\n", res.Handoff.Goal))
	sb.WriteString("Milestones:\n")
	for _, m := range res.Handoff.CompletedMilestones {
		sb.WriteString(fmt.Sprintf(" - %s\n", m))
	}
	sb.WriteString("\nNext Steps:\n")
	for _, step := range res.Handoff.NextRecommendedSteps {
		sb.WriteString(fmt.Sprintf(" - %s\n", step))
	}
	return sb.String()
}

// RunTUI launches the interactive Bubbletea TUI application.
func RunTUI(res *AuditResult) error {
	p := tea.NewProgram(newTUIModel(res))
	_, err := p.Run()
	return err
}
