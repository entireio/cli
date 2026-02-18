package dashboard

import (
	"fmt"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// updateModel calls m.Update(msg) and asserts the result is a Model.
func updateModel(t *testing.T, m Model, msg tea.Msg) (Model, tea.Cmd) {
	t.Helper()
	result, cmd := m.Update(msg)
	model, ok := result.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want dashboard.Model", result)
	}
	return model, cmd
}

// keyMsg builds a tea.KeyMsg for common key strings.
func keyMsg(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg(tea.Key{Type: tea.KeyEnter})
	case "esc":
		return tea.KeyMsg(tea.Key{Type: tea.KeyEsc})
	case "tab":
		return tea.KeyMsg(tea.Key{Type: tea.KeyTab})
	case "shift+tab":
		return tea.KeyMsg(tea.Key{Type: tea.KeyShiftTab})
	case "backspace":
		return tea.KeyMsg(tea.Key{Type: tea.KeyBackspace})
	case "up":
		return tea.KeyMsg(tea.Key{Type: tea.KeyUp})
	case "down":
		return tea.KeyMsg(tea.Key{Type: tea.KeyDown})
	case "ctrl+c":
		return tea.KeyMsg(tea.Key{Type: tea.KeyCtrlC})
	default:
		// Single character rune keys (j, k, q, ?, /, r, y, n, etc.)
		return tea.KeyMsg(tea.Key{Type: tea.KeyRunes, Runes: []rune(s)})
	}
}

// testRewindPoints generates n test RewindPoint entries.
func testRewindPoints(n int) []strategy.RewindPoint {
	points := make([]strategy.RewindPoint, n)
	for i := range n {
		points[i] = strategy.RewindPoint{
			ID:            fmt.Sprintf("abc123def%03d", i),
			Message:       fmt.Sprintf("checkpoint %d", i),
			Date:          time.Now().Add(-time.Duration(i) * time.Hour),
			CheckpointID:  id.CheckpointID(fmt.Sprintf("cp%010d", i)),
			Agent:         agent.AgentTypeClaudeCode,
			SessionID:     fmt.Sprintf("session-%d", i),
			SessionPrompt: fmt.Sprintf("prompt %d", i),
		}
	}
	return points
}

// testSessions generates n test Session entries.
func testSessions(n int) []strategy.Session {
	sessions := make([]strategy.Session, n)
	for i := range n {
		sessions[i] = strategy.Session{
			ID:          fmt.Sprintf("2026-01-01-session-%d", i),
			Description: fmt.Sprintf("test session %d", i),
			Strategy:    "manual-commit",
			StartTime:   time.Now().Add(-time.Duration(i) * time.Hour),
		}
	}
	return sessions
}

// testSessionStates generates n test session.State entries.
// If includeEnded is true, the last entry will have EndedAt set.
func testSessionStates(n int, includeEnded bool) []*session.State {
	states := make([]*session.State, n)
	for i := range n {
		s := &session.State{
			SessionID:  fmt.Sprintf("sess-%d", i),
			BaseCommit: fmt.Sprintf("abc123%d", i),
			StartedAt:  time.Now().Add(-time.Duration(i) * time.Hour),
			Phase:      session.PhaseActive,
			AgentType:  agent.AgentTypeClaudeCode,
		}
		if includeEnded && i == n-1 {
			ended := time.Now()
			s.EndedAt = &ended
			s.Phase = session.PhaseEnded
		}
		states[i] = s
	}
	return states
}
