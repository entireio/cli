package recap

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestNewTUIModel_InitialStateMatchesView(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 21, 15, 0, 0, 0, time.UTC)
	sessions := []RecapSession{
		{SessionID: "a", StartedAt: now.Add(-1 * time.Hour), LastInteraction: now, AgentsUsed: []string{"claude-code"}},
	}
	view := BuildView(sessions, BuildOpts{Range: RangeDay, Now: now})
	m := NewTUIModel(sessions, view, "")
	if m.view.Range != RangeDay {
		t.Errorf("initial Range = %q, want %q", m.view.Range, RangeDay)
	}
	if len(m.agents) != 1 || m.agents[0] != "claude-code" {
		t.Errorf("agents = %v, want [claude-code]", m.agents)
	}
}

func TestTUIModel_RangeKeysRebuildView(t *testing.T) {
	t.Parallel()
	now := time.Now()
	sessions := []RecapSession{{StartedAt: now, LastInteraction: now}}
	view := BuildView(sessions, BuildOpts{Range: RangeDay, Now: now})
	m := NewTUIModel(sessions, view, "")

	cases := []struct {
		key  string
		want RangeKey
	}{
		{"1", RangeDay},
		{"w", RangeWeek},
		{"m", RangeMonth},
		{"4", Range90d},
	}
	for _, c := range cases {
		nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(c.key)})
		tm, _ := nm.(TUIModel) //nolint:errcheck // type from Update is guaranteed TUIModel
		if tm.view.Range != c.want {
			t.Errorf("after %q: Range = %q, want %q", c.key, tm.view.Range, c.want)
		}
	}
}

func TestTUIModel_AgentKeyCyclesThroughAgents(t *testing.T) {
	t.Parallel()
	now := time.Now()
	sessions := []RecapSession{
		{StartedAt: now, LastInteraction: now, AgentsUsed: []string{"claude-code"}},
		{StartedAt: now, LastInteraction: now, AgentsUsed: []string{"codex"}},
	}
	view := BuildView(sessions, BuildOpts{Range: RangeDay, Now: now})
	m := NewTUIModel(sessions, view, "")
	// "" → first agent
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	tm, _ := nm.(TUIModel) //nolint:errcheck // type from Update is guaranteed TUIModel
	if tm.agentFilter == "" {
		t.Error("first 'a' press should select an agent, not stay empty")
	}
	// Again → next agent
	nm2, _ := tm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	tm2, _ := nm2.(TUIModel) //nolint:errcheck // type from Update is guaranteed TUIModel
	if tm2.agentFilter == tm.agentFilter {
		t.Errorf("second 'a' didn't cycle: still %q", tm2.agentFilter)
	}
}

func TestTUIModel_QuitKeysReturnTeaQuit(t *testing.T) {
	t.Parallel()
	m := NewTUIModel(nil, View{}, "")
	for _, key := range []string{"q", "esc", "ctrl+c"} {
		var msg tea.KeyMsg
		switch key {
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "ctrl+c":
			msg = tea.KeyMsg{Type: tea.KeyCtrlC}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
		}
		_, cmd := m.Update(msg)
		if cmd == nil {
			t.Errorf("key %q should produce a quit command", key)
		}
	}
}

func TestTUIModel_ViewIncludesHelpLine(t *testing.T) {
	t.Parallel()
	m := NewTUIModel(nil, View{Title: "Today"}, "")
	out := m.View()
	if !strings.Contains(out, "range") {
		t.Errorf("TUI view missing range hint in help:\n%s", out)
	}
	if !strings.Contains(out, "quit") {
		t.Errorf("TUI view missing quit hint:\n%s", out)
	}
}

func TestTUIModel_VKeyCyclesViewMode(t *testing.T) {
	t.Parallel()
	now := time.Now()
	sessions := []RecapSession{{StartedAt: now, LastInteraction: now}}
	view := BuildView(sessions, BuildOpts{Range: RangeDay, Now: now})
	m := NewTUIModel(sessions, view, "")
	// Start: ViewBoth → v → ViewMe
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'v'}})
	tm, _ := nm.(TUIModel) //nolint:errcheck // type from Update is guaranteed TUIModel
	if tm.mode != ViewMe {
		t.Errorf("after first v: mode = %q, want %q", tm.mode, ViewMe)
	}
	// ViewMe → v → ViewContributors
	nm2, _ := tm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'v'}})
	tm2, _ := nm2.(TUIModel) //nolint:errcheck // type from Update is guaranteed TUIModel
	if tm2.mode != ViewContributors {
		t.Errorf("after second v: mode = %q, want %q", tm2.mode, ViewContributors)
	}
	// ViewContributors → v → ViewBoth
	nm3, _ := tm2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'v'}})
	tm3, _ := nm3.(TUIModel) //nolint:errcheck // type from Update is guaranteed TUIModel
	if tm3.mode != ViewBoth {
		t.Errorf("after third v: mode = %q, want %q", tm3.mode, ViewBoth)
	}
}

func TestCycleAgent_EmptyListStaysEmpty(t *testing.T) {
	t.Parallel()
	if got := cycleAgent(nil, "anything"); got != "" {
		t.Errorf("empty agents → %q, want empty", got)
	}
}

func TestCycleAgent_WrapsBackToAll(t *testing.T) {
	t.Parallel()
	agents := []string{"a", "b"}
	if got := cycleAgent(agents, "b"); got != "" {
		t.Errorf("last → %q, want empty (wrap to all)", got)
	}
}
