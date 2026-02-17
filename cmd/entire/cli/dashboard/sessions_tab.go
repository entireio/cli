package dashboard

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

//nolint:recvcheck // bubbletea pattern: pointer receivers for mutating setters, value receivers for update/view
type sessionsModel struct {
	sessions   []strategy.Session
	filtered   []strategy.Session
	err        error
	cursor     int
	scrollPos  int
	width      int
	height     int
	showDetail bool
	filtering  bool
	filter     string
}

func newSessionsModel() sessionsModel {
	return sessionsModel{}
}

func (m *sessionsModel) setSize(width, height int) {
	m.width = width
	m.height = height
}

func (m *sessionsModel) setSessions(sessions []strategy.Session) {
	m.sessions = sessions
	m.applyFilter()
}

func (m *sessionsModel) applyFilter() {
	if m.filter == "" {
		m.filtered = m.sessions
		return
	}
	lower := strings.ToLower(m.filter)
	var filtered []strategy.Session
	for _, s := range m.sessions {
		if strings.Contains(strings.ToLower(s.ID), lower) ||
			strings.Contains(strings.ToLower(s.Description), lower) ||
			strings.Contains(strings.ToLower(s.Strategy), lower) {
			filtered = append(filtered, s)
		}
	}
	m.filtered = filtered
	if m.cursor >= len(m.filtered) {
		m.cursor = max(0, len(m.filtered)-1)
	}
}

func (m sessionsModel) update(msg tea.Msg) (sessionsModel, tea.Cmd) { //nolint:unparam // tea.Cmd needed for consistent tab interface
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}

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
	}

	return m, nil
}

func (m sessionsModel) view(width, height int) string {
	if m.err != nil {
		return errorStyle.Render(fmt.Sprintf("Error loading sessions: %v", m.err))
	}

	if m.showDetail && m.cursor < len(m.filtered) {
		return m.renderDetail(width, height)
	}

	return m.renderList(width, height)
}

func (m sessionsModel) renderList(_ int, height int) string {
	var lines []string

	// Header
	headerText := fmt.Sprintf("Sessions (%d)", len(m.filtered))
	if m.filter != "" {
		headerText += "  filter: " + m.filter
	}
	if m.filtering {
		headerText += dimStyle.Render("  type to filter, Enter to confirm, Esc to cancel")
	}
	lines = append(lines, "  "+titleStyle.Render(headerText))
	lines = append(lines, "")

	if len(m.filtered) == 0 {
		lines = append(lines, "  "+dimStyle.Render("No sessions found"))
		return strings.Join(lines, "\n")
	}

	for i, s := range m.filtered {
		shortID := s.ID
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}

		desc := s.Description
		if desc == "" || desc == strategy.NoDescription {
			desc = dimStyle.Render("(no description)")
		} else {
			desc = truncate(desc, 50)
		}

		age := timeAgo(s.StartTime)
		cpCount := len(s.Checkpoints)
		stratName := s.Strategy
		if stratName == "" {
			stratName = "-"
		}

		line := fmt.Sprintf("  %-12s  %s  %s  %s  %d cp",
			shortID, desc, dimStyle.Render(stratName), dimStyle.Render(age), cpCount)

		if i == m.cursor {
			line = selectedItemStyle.Render(line)
		}

		lines = append(lines, line)
	}

	// Scroll if needed
	visibleStart := 0
	headerLines := 2
	maxVisible := height - headerLines
	if maxVisible < 1 {
		maxVisible = 1
	}

	if m.cursor-visibleStart >= maxVisible {
		visibleStart = m.cursor - maxVisible + 1
	}

	result := lines[:headerLines] // always show header
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

func (m sessionsModel) renderDetail(_ int, height int) string {
	s := m.filtered[m.cursor]

	var lines []string
	lines = append(lines, titleStyle.Render("Session Detail"))
	lines = append(lines, "")
	lines = append(lines, fmt.Sprintf("  %s  %s", labelStyle.Render("ID:"), valueStyle.Render(s.ID)))
	lines = append(lines, fmt.Sprintf("  %s  %s", labelStyle.Render("Description:"), valueStyle.Render(s.Description)))
	lines = append(lines, fmt.Sprintf("  %s  %s", labelStyle.Render("Strategy:"), valueStyle.Render(s.Strategy)))
	lines = append(lines, fmt.Sprintf("  %s  %s", labelStyle.Render("Started:"), valueStyle.Render(s.StartTime.Format("2006-01-02 15:04:05"))))
	lines = append(lines, "")

	lines = append(lines, fmt.Sprintf("  %s (%d)", labelStyle.Render("Checkpoints:"), len(s.Checkpoints)))
	lines = append(lines, "")

	for _, cp := range s.Checkpoints {
		cpType := cpTypeSession
		if cp.IsTaskCheckpoint {
			cpType = cpTypeTask
		}
		cpID := cp.CheckpointID.String()
		if cpID == "" {
			cpID = "(none)"
		}

		lines = append(lines, fmt.Sprintf("    %s  %s  %s  %s",
			dimStyle.Render(cpID),
			dimStyle.Render("["+cpType+"]"),
			cp.Message,
			dimStyle.Render(timeAgo(cp.Timestamp)),
		))
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
