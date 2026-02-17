package dashboard

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

//nolint:recvcheck // bubbletea pattern: pointer receivers for mutating setters, value receivers for update/view
type checkpointsModel struct {
	points     []strategy.RewindPoint
	filtered   []strategy.RewindPoint
	err        error
	cursor     int
	scrollPos  int
	width      int
	height     int
	showDetail bool
	filtering  bool
	filter     string
	confirming bool // rewind confirmation dialog
}

func newCheckpointsModel() checkpointsModel {
	return checkpointsModel{}
}

func (m *checkpointsModel) setSize(width, height int) {
	m.width = width
	m.height = height
}

func (m *checkpointsModel) setCheckpoints(points []strategy.RewindPoint) {
	m.points = points
	m.applyFilter()
}

func (m *checkpointsModel) applyFilter() {
	if m.filter == "" {
		m.filtered = m.points
		return
	}
	lower := strings.ToLower(m.filter)
	var filtered []strategy.RewindPoint
	for _, p := range m.points {
		if strings.Contains(strings.ToLower(p.ID), lower) ||
			strings.Contains(strings.ToLower(p.Message), lower) ||
			strings.Contains(strings.ToLower(p.SessionPrompt), lower) ||
			strings.Contains(strings.ToLower(p.CheckpointID.String()), lower) {
			filtered = append(filtered, p)
		}
	}
	m.filtered = filtered
	if m.cursor >= len(m.filtered) {
		m.cursor = max(0, len(m.filtered)-1)
	}
}

func (m checkpointsModel) update(msg tea.Msg) (checkpointsModel, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}

	// Handle confirmation dialog
	if m.confirming {
		switch keyMsg.String() {
		case keyY, keyYUpper:
			m.confirming = false
			if m.cursor < len(m.filtered) {
				point := m.filtered[m.cursor]
				return m, func() tea.Msg {
					return rewindRequestMsg{pointID: point.ID}
				}
			}
		case keyN, keyNUpper, keyEsc:
			m.confirming = false
		}
		return m, nil
	}

	// Handle filter input
	if m.filtering {
		switch keyMsg.String() {
		case keyEsc:
			m.filtering = false
			m.filter = ""
			m.applyFilter()
		case keyEnter:
			m.filtering = false
		case keyBackspace:
			runes := []rune(m.filter)
			if len(runes) > 0 {
				m.filter = string(runes[:len(runes)-1])
				m.applyFilter()
			}
		default:
			r := []rune(keyMsg.String())
			if len(r) == 1 && r[0] >= 32 {
				m.filter += keyMsg.String()
				m.applyFilter()
			}
		}
		return m, nil
	}

	switch keyMsg.String() {
	case keyJ, keyDown:
		if m.showDetail {
			m.scrollPos++
		} else if m.cursor < len(m.filtered)-1 {
			m.cursor++
		}
	case keyK, keyUp:
		if m.showDetail {
			if m.scrollPos > 0 {
				m.scrollPos--
			}
		} else if m.cursor > 0 {
			m.cursor--
		}
	case keyEnter:
		if len(m.filtered) > 0 {
			m.showDetail = !m.showDetail
			m.scrollPos = 0
		}
	case keyEsc:
		if m.showDetail {
			m.showDetail = false
			m.scrollPos = 0
		} else if m.filter != "" {
			m.filter = ""
			m.applyFilter()
		}
	case keyFilter:
		m.filtering = true
	case keyR:
		if len(m.filtered) > 0 && !m.showDetail {
			m.confirming = true
		}
	}

	return m, nil
}

func (m checkpointsModel) view(width, height int) string {
	if m.err != nil {
		return errorStyle.Render(fmt.Sprintf("Error loading checkpoints: %v", m.err))
	}

	if m.confirming && m.cursor < len(m.filtered) {
		return m.renderConfirmation(width, height)
	}

	if m.showDetail && m.cursor < len(m.filtered) {
		return m.renderDetail(width, height)
	}

	return m.renderList(width, height)
}

func (m checkpointsModel) renderList(_ int, height int) string {
	var lines []string

	// Header
	headerText := fmt.Sprintf("Checkpoints (%d)", len(m.filtered))
	if m.filter != "" {
		headerText += "  filter: " + m.filter
	}
	if m.filtering {
		headerText += dimStyle.Render("  type to filter, Enter to confirm, Esc to cancel")
	}
	lines = append(lines, "  "+titleStyle.Render(headerText))
	lines = append(lines, "")

	if len(m.filtered) == 0 {
		lines = append(lines, "  "+dimStyle.Render("No checkpoints found"))
		return strings.Join(lines, "\n")
	}

	for i, p := range m.filtered {
		cpType := cpTypeSession
		if p.IsTaskCheckpoint {
			cpType = cpTypeTask
		}
		if p.IsLogsOnly {
			cpType = cpTypeCommitted
		}

		cpID := p.CheckpointID.String()
		if cpID == "" {
			cpID = truncate(p.ID, 12)
		}

		prompt := truncate(p.SessionPrompt, 40)
		if prompt == "" {
			prompt = truncate(p.Message, 40)
		}

		agentStr := string(p.Agent)
		if agentStr == "" {
			agentStr = "-"
		}

		line := fmt.Sprintf("  %-12s  %-10s  %-14s  %-10s  %s",
			cpID,
			dimStyle.Render("["+cpType+"]"),
			agentStr,
			dimStyle.Render(timeAgo(p.Date)),
			prompt,
		)

		if i == m.cursor {
			line = selectedItemStyle.Render(line)
		}

		lines = append(lines, line)
	}

	// Scroll
	visibleStart := 0
	headerLines := 2
	maxVisible := height - headerLines
	if maxVisible < 1 {
		maxVisible = 1
	}

	if m.cursor-visibleStart >= maxVisible {
		visibleStart = m.cursor - maxVisible + 1
	}

	result := lines[:headerLines]
	dataLines := lines[headerLines:]
	end := visibleStart + maxVisible
	if end > len(dataLines) {
		end = len(dataLines)
	}
	if visibleStart < len(dataLines) {
		result = append(result, dataLines[visibleStart:end]...)
	}

	return strings.Join(result, "\n")
}

func (m checkpointsModel) renderDetail(_ int, height int) string {
	p := m.filtered[m.cursor]

	var lines []string
	lines = append(lines, titleStyle.Render("Checkpoint Detail"))
	lines = append(lines, "")
	lines = append(lines, fmt.Sprintf("  %s  %s", labelStyle.Render("ID:"), valueStyle.Render(p.ID)))

	cpID := p.CheckpointID.String()
	if cpID != "" {
		lines = append(lines, fmt.Sprintf("  %s  %s", labelStyle.Render("Checkpoint ID:"), valueStyle.Render(cpID)))
	}

	lines = append(lines, fmt.Sprintf("  %s  %s", labelStyle.Render("Date:"), valueStyle.Render(p.Date.Format("2006-01-02 15:04:05"))))
	lines = append(lines, fmt.Sprintf("  %s  %s", labelStyle.Render("Message:"), valueStyle.Render(p.Message)))

	if string(p.Agent) != "" {
		lines = append(lines, fmt.Sprintf("  %s  %s", labelStyle.Render("Agent:"), valueStyle.Render(string(p.Agent))))
	}

	cpType := "Session checkpoint"
	if p.IsTaskCheckpoint {
		cpType = "Task checkpoint"
	}
	if p.IsLogsOnly {
		cpType = "Committed (logs only)"
	}
	lines = append(lines, fmt.Sprintf("  %s  %s", labelStyle.Render("Type:"), valueStyle.Render(cpType)))

	if p.SessionID != "" {
		lines = append(lines, fmt.Sprintf("  %s  %s", labelStyle.Render("Session:"), valueStyle.Render(p.SessionID)))
	}

	if p.SessionPrompt != "" {
		lines = append(lines, "")
		lines = append(lines, labelStyle.Render("  Prompt:"))
		lines = append(lines, "  "+valueStyle.Render(p.SessionPrompt))
	}

	if p.ToolUseID != "" {
		lines = append(lines, fmt.Sprintf("  %s  %s", labelStyle.Render("Tool Use ID:"), valueStyle.Render(p.ToolUseID)))
	}

	if p.SessionCount > 1 {
		lines = append(lines, fmt.Sprintf("  %s  %d", labelStyle.Render("Sessions:"), p.SessionCount))
		for i, sid := range p.SessionIDs {
			prompt := ""
			if i < len(p.SessionPrompts) {
				prompt = truncate(p.SessionPrompts[i], 40)
			}
			lines = append(lines, fmt.Sprintf("    %s  %s", dimStyle.Render(truncate(sid, 20)), prompt))
		}
	}

	lines = append(lines, "")
	lines = append(lines, dimStyle.Render("  Press Esc to go back"))

	// Apply scroll
	start := m.scrollPos
	end := start + height
	if end > len(lines) {
		end = len(lines)
	}
	if start >= len(lines) {
		start = max(0, len(lines)-1)
	}

	return strings.Join(lines[start:end], "\n")
}

func (m checkpointsModel) renderConfirmation(_ int, _ int) string {
	p := m.filtered[m.cursor]

	var lines []string
	lines = append(lines, "")
	lines = append(lines, confirmStyle.Render("  Rewind to this checkpoint?"))
	lines = append(lines, "")

	cpID := p.CheckpointID.String()
	if cpID == "" {
		cpID = truncate(p.ID, 20)
	}
	lines = append(lines, fmt.Sprintf("  %s  %s", labelStyle.Render("Checkpoint:"), valueStyle.Render(cpID)))
	lines = append(lines, fmt.Sprintf("  %s  %s", labelStyle.Render("Date:"), valueStyle.Render(p.Date.Format("2006-01-02 15:04:05"))))
	if p.SessionPrompt != "" {
		lines = append(lines, fmt.Sprintf("  %s  %s", labelStyle.Render("Prompt:"), valueStyle.Render(truncate(p.SessionPrompt, 50))))
	}

	if p.IsLogsOnly {
		lines = append(lines, "")
		lines = append(lines, "  "+idlePhaseStyle.Render("This is a logs-only checkpoint. Only session logs will be restored."))
	}

	lines = append(lines, "")
	lines = append(lines, "  "+confirmStyle.Render("Press y to confirm, n or Esc to cancel"))

	return strings.Join(lines, "\n")
}
