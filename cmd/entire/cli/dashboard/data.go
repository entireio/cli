package dashboard

import (
	"context"
	"errors"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// Message types for async data loading.
type sessionsMsg struct {
	sessions []strategy.Session
	err      error
}

type checkpointsMsg struct {
	points []strategy.RewindPoint
	err    error
}

type activeSessionsMsg struct {
	states []*session.State
	err    error
}

type settingsDataMsg struct {
	settings *settings.EntireSettings
	agents   []agent.AgentName
	err      error
}

type rewindRequestMsg struct {
	pointID string
}

// Commands that load data asynchronously.

//nolint:ireturn // required by bubbletea Cmd signature
func loadSessionsCmd() tea.Msg {
	sessions, err := strategy.ListSessions()
	return sessionsMsg{sessions: sessions, err: err}
}

//nolint:ireturn // required by bubbletea Cmd signature
func loadCheckpointsCmd() tea.Msg {
	strat := getConfiguredStrategy()
	if strat == nil {
		return checkpointsMsg{err: errors.New("no strategy configured")}
	}
	points, err := strat.GetRewindPoints(100)
	return checkpointsMsg{points: points, err: err}
}

//nolint:ireturn // required by bubbletea Cmd signature
func loadActiveSessionsCmd() tea.Msg {
	store, err := session.NewStateStore()
	if err != nil {
		return activeSessionsMsg{err: err}
	}
	states, err := store.List(context.Background())
	return activeSessionsMsg{states: states, err: err}
}

//nolint:ireturn // required by bubbletea Cmd signature
func loadSettingsCmd() tea.Msg {
	s, err := settings.Load()
	if err != nil {
		return settingsDataMsg{err: err}
	}
	agents := agent.List()
	return settingsDataMsg{settings: s, agents: agents}
}

// getConfiguredStrategy loads settings and returns the configured strategy.
func getConfiguredStrategy() strategy.Strategy {
	s, err := settings.Load()
	if err != nil {
		return strategy.Default()
	}
	strat, err := strategy.Get(s.Strategy)
	if err != nil {
		return strategy.Default()
	}
	return strat
}

// timeAgo formats a time as a human-readable relative duration.
func timeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		return fmt.Sprintf("%dm ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		return fmt.Sprintf("%dh ago", h)
	default:
		days := int(d.Hours() / 24)
		return fmt.Sprintf("%dd ago", days)
	}
}

// truncate shortens a string with an ellipsis if it exceeds maxLen.
func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	if maxLen < 3 {
		return string(runes[:maxLen])
	}
	return string(runes[:maxLen-3]) + "..."
}
