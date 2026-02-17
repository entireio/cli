package dashboard

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/entireio/cli/cmd/entire/cli/session"
)

//nolint:recvcheck // bubbletea pattern: pointer receivers for mutating setters, value receivers for update/view
type activeModel struct {
	states     []*session.State
	err        error
	cursor     int
	scrollPos  int
	width      int
	height     int
	showDetail bool
}

func newActiveModel() activeModel {
	return activeModel{}
}

func (m *activeModel) setSize(width, height int) {
	m.width = width
	m.height = height
}

func (m *activeModel) setSessions(states []*session.State) {
	// Filter to active sessions only (no EndedAt)
	var active []*session.State
	for _, s := range states {
		if s.EndedAt == nil {
			active = append(active, s)
		}
	}
	m.states = active
}

func (m activeModel) update(msg tea.Msg) (activeModel, tea.Cmd) { //nolint:unparam // tea.Cmd needed for consistent tab interface
	if keyMsg, ok := msg.(tea.KeyMsg); ok {
		switch keyMsg.String() {
		case keyJ, keyDown:
			if m.showDetail {
				m.scrollPos++
			} else if m.cursor < len(m.states)-1 {
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
			if len(m.states) > 0 {
				m.showDetail = !m.showDetail
				m.scrollPos = 0
			}
		case keyEsc:
			m.showDetail = false
			m.scrollPos = 0
		}
	}
	return m, nil
}

func (m activeModel) view(width, height int) string {
	if m.err != nil {
		return errorStyle.Render(fmt.Sprintf("Error loading active sessions: %v", m.err))
	}
	if len(m.states) == 0 {
		return dimStyle.Render("  No active sessions")
	}

	if m.showDetail && m.cursor < len(m.states) {
		return m.renderDetail(width, height)
	}

	return m.renderList(width, height)
}

func (m activeModel) renderList(_ int, height int) string {
	var lines []string

	lines = append(lines, fmt.Sprintf("  %s (%d)",
		titleStyle.Render("Active Sessions"),
		len(m.states)))
	lines = append(lines, "")

	// Table header
	header := fmt.Sprintf("  %-9s  %-18s  %-14s  %-12s  %-12s  %s",
		labelStyle.Render("ID"),
		labelStyle.Render("Phase"),
		labelStyle.Render("Agent"),
		labelStyle.Render("Started"),
		labelStyle.Render("Last Active"),
		labelStyle.Render("Prompt"),
	)
	lines = append(lines, header)
	lines = append(lines, "  "+dimStyle.Render(strings.Repeat("─", 80)))

	for i, s := range m.states {
		shortID := s.SessionID
		if len(shortID) > 7 {
			shortID = shortID[:7]
		}

		phase := renderPhase(s.Phase)

		agentLabel := string(s.AgentType)
		if agentLabel == "" {
			agentLabel = "(unknown)"
		}
		agentLabel = truncate(agentLabel, 12)

		started := timeAgo(s.StartedAt)

		lastActive := "-"
		if s.LastInteractionTime != nil {
			lastActive = timeAgo(*s.LastInteractionTime)
		}

		prompt := truncate(s.FirstPrompt, 30)
		if prompt == "" {
			prompt = dimStyle.Render("-")
		}

		line := fmt.Sprintf("  %-9s  %-18s  %-14s  %-12s  %-12s  %s",
			shortID, phase, agentLabel, started, lastActive, prompt)

		if i == m.cursor {
			line = selectedItemStyle.Render(line)
		}

		lines = append(lines, line)
	}

	// Scroll data lines, keeping header pinned
	headerLines := 4 // title + blank + header + separator
	maxVisible := height - headerLines
	if maxVisible < 1 {
		maxVisible = 1
	}

	visibleStart := 0
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

func (m activeModel) renderDetail(_ int, height int) string {
	s := m.states[m.cursor]

	var lines []string
	lines = append(lines, titleStyle.Render("Session Detail"))
	lines = append(lines, "")
	lines = append(lines, fmt.Sprintf("  %s  %s", labelStyle.Render("Session ID:"), valueStyle.Render(s.SessionID)))
	lines = append(lines, fmt.Sprintf("  %s  %s", labelStyle.Render("Phase:"), renderPhase(s.Phase)))
	lines = append(lines, fmt.Sprintf("  %s  %s", labelStyle.Render("Agent:"), valueStyle.Render(string(s.AgentType))))
	lines = append(lines, fmt.Sprintf("  %s  %s", labelStyle.Render("Started:"), valueStyle.Render(s.StartedAt.Format("2006-01-02 15:04:05"))))

	if s.LastInteractionTime != nil {
		lines = append(lines, fmt.Sprintf("  %s  %s", labelStyle.Render("Last Active:"), valueStyle.Render(s.LastInteractionTime.Format("2006-01-02 15:04:05"))))
	}

	lines = append(lines, fmt.Sprintf("  %s  %s", labelStyle.Render("Base Commit:"), valueStyle.Render(truncate(s.BaseCommit, 12))))
	lines = append(lines, fmt.Sprintf("  %s  %d", labelStyle.Render("Step Count:"), s.StepCount))

	if s.WorktreePath != "" {
		lines = append(lines, fmt.Sprintf("  %s  %s", labelStyle.Render("Worktree:"), valueStyle.Render(s.WorktreePath)))
	}

	if s.FirstPrompt != "" {
		lines = append(lines, "")
		lines = append(lines, labelStyle.Render("  First Prompt:"))
		lines = append(lines, "  "+valueStyle.Render(s.FirstPrompt))
	}

	if s.TokenUsage != nil {
		lines = append(lines, "")
		lines = append(lines, labelStyle.Render("  Token Usage:"))
		lines = append(lines, fmt.Sprintf("    Input:  %d", s.TokenUsage.InputTokens))
		lines = append(lines, fmt.Sprintf("    Output: %d", s.TokenUsage.OutputTokens))
	}

	if len(s.FilesTouched) > 0 {
		lines = append(lines, "")
		lines = append(lines, fmt.Sprintf("  %s (%d files)", labelStyle.Render("Files Touched:"), len(s.FilesTouched)))
		for _, f := range s.FilesTouched {
			lines = append(lines, "    "+dimStyle.Render(f))
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

func renderPhase(p session.Phase) string {
	switch p {
	case session.PhaseActive:
		return activePhaseStyle.Render("ACTIVE")
	case session.PhaseIdle:
		return idlePhaseStyle.Render("IDLE")
	case session.PhaseEnded:
		return endedPhaseStyle.Render("ENDED")
	default:
		return idlePhaseStyle.Render("IDLE")
	}
}
