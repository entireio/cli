package dashboard

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

//nolint:recvcheck // bubbletea pattern: pointer receivers for mutating setters, value receivers for update/view
type settingsModel struct {
	data      *settings.EntireSettings
	agents    []agent.AgentName
	err       error
	scrollPos int
	height    int
	lines     []string
}

func newSettingsModel() settingsModel {
	return settingsModel{}
}

func (m *settingsModel) setSize(_ int, height int) {
	m.height = height
}

func (m *settingsModel) setData(msg settingsDataMsg) {
	m.data = msg.settings
	m.agents = msg.agents
	m.err = msg.err
	m.buildLines()
}

func (m *settingsModel) buildLines() {
	if m.data == nil {
		return
	}

	var lines []string
	s := m.data

	lines = append(lines, titleStyle.Render("Configuration"))
	lines = append(lines, "")

	// Enabled/Disabled
	enabledStr := activePhaseStyle.Render("Enabled")
	if !s.Enabled {
		enabledStr = endedPhaseStyle.Render("Disabled")
	}
	lines = append(lines, fmt.Sprintf("  %s  %s", labelStyle.Render("Status:"), enabledStr))

	// Strategy
	lines = append(lines, fmt.Sprintf("  %s  %s", labelStyle.Render("Strategy:"), valueStyle.Render(s.Strategy)))

	// Log level
	logLevel := s.LogLevel
	if logLevel == "" {
		logLevel = "info (default)"
	}
	lines = append(lines, fmt.Sprintf("  %s  %s", labelStyle.Render("Log Level:"), valueStyle.Render(logLevel)))

	// Telemetry
	telemetryStr := dimStyle.Render("not configured")
	if s.Telemetry != nil {
		if *s.Telemetry {
			telemetryStr = activePhaseStyle.Render("opted in")
		} else {
			telemetryStr = endedPhaseStyle.Render("opted out")
		}
	}
	lines = append(lines, fmt.Sprintf("  %s  %s", labelStyle.Render("Telemetry:"), telemetryStr))

	// Summarize
	summarize := "disabled"
	if s.IsSummarizeEnabled() {
		summarize = "enabled"
	}
	lines = append(lines, fmt.Sprintf("  %s  %s", labelStyle.Render("Summarize:"), valueStyle.Render(summarize)))

	// Push sessions
	pushSessions := "enabled"
	if s.IsPushSessionsDisabled() {
		pushSessions = "disabled"
	}
	lines = append(lines, fmt.Sprintf("  %s  %s", labelStyle.Render("Push Sessions:"), valueStyle.Render(pushSessions)))

	lines = append(lines, "")
	lines = append(lines, titleStyle.Render("Installed Agents"))
	lines = append(lines, "")

	if len(m.agents) == 0 {
		lines = append(lines, "  "+dimStyle.Render("No agents registered"))
	} else {
		for _, name := range m.agents {
			ag, err := agent.Get(name)
			if err != nil {
				lines = append(lines, fmt.Sprintf("  %s %s", dimStyle.Render("-"), valueStyle.Render(string(name))))
				continue
			}

			hooksStr := dimStyle.Render("no hooks")
			if hs, ok := ag.(agent.HookSupport); ok {
				if hs.AreHooksInstalled() {
					hooksStr = activePhaseStyle.Render("hooks installed")
				} else {
					hooksStr = idlePhaseStyle.Render("hooks not installed")
				}
			}

			lines = append(lines, fmt.Sprintf("  %s  %s  %s",
				labelStyle.Render(string(ag.Type())),
				dimStyle.Render("("+string(name)+")"),
				hooksStr,
			))
		}
	}

	// Strategy options
	if len(s.StrategyOptions) > 0 {
		lines = append(lines, "")
		lines = append(lines, titleStyle.Render("Strategy Options"))
		lines = append(lines, "")
		for k, v := range s.StrategyOptions {
			lines = append(lines, fmt.Sprintf("  %s  %v", labelStyle.Render(k+":"), v))
		}
	}

	m.lines = lines
}

func (m settingsModel) update(msg tea.Msg) (settingsModel, tea.Cmd) { //nolint:unparam // tea.Cmd needed for consistent tab interface
	if keyMsg, ok := msg.(tea.KeyMsg); ok {
		switch keyMsg.String() {
		case keyJ, keyDown:
			if m.scrollPos < len(m.lines)-m.height {
				m.scrollPos++
			}
		case keyK, keyUp:
			if m.scrollPos > 0 {
				m.scrollPos--
			}
		}
	}
	return m, nil
}

func (m settingsModel) view(_ int, height int) string {
	if m.err != nil {
		return errorStyle.Render(fmt.Sprintf("Error loading settings: %v", m.err))
	}
	if m.data == nil {
		return dimStyle.Render("  No settings data available")
	}

	// Apply scroll
	start := m.scrollPos
	end := start + height
	if end > len(m.lines) {
		end = len(m.lines)
	}
	if start > len(m.lines) {
		start = len(m.lines)
	}

	visible := m.lines[start:end]
	return strings.Join(visible, "\n")
}
