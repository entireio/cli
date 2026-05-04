package cli

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/entireio/cli/cmd/entire/cli/recap"
)

func testRecapTUIModel() recapTUIModel {
	return recapTUIModel{
		rangeKey: recap.RangeDay,
		view:     recap.ViewBoth,
		agent:    recap.AgentAll,
		resp: &recap.MeRecapResponse{
			Agents: map[string]recap.AgentEntry{
				recapTestAgentCodex: {
					AgentID:    recapTestAgentCodex,
					AgentLabel: "Codex",
				},
				"claude": {
					AgentID:    "claude",
					AgentLabel: "Claude Code",
				},
			},
		},
	}
}

func updateRecapTUIModel(t *testing.T, m recapTUIModel, msg tea.Msg) (recapTUIModel, tea.Cmd) {
	t.Helper()

	updated, cmd := m.Update(msg)
	result, ok := updated.(recapTUIModel)
	if !ok {
		t.Fatalf("Update returned %T, want recapTUIModel", updated)
	}
	return result, cmd
}

func recapRuneKey(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Text: string(r)}
}

func TestRecapTUIModel_TogglesRange(t *testing.T) {
	t.Parallel()

	m, cmd := updateRecapTUIModel(t, testRecapTUIModel(), recapRuneKey('t'))
	if m.rangeKey != recap.RangeWeek {
		t.Fatalf("range = %q, want %q", m.rangeKey, recap.RangeWeek)
	}
	if !m.loading {
		t.Fatal("range toggle should mark model loading")
	}
	if cmd == nil {
		t.Fatal("range toggle should refetch recap data")
	}
}

func TestRecapTUIModel_TogglesView(t *testing.T) {
	t.Parallel()

	m, cmd := updateRecapTUIModel(t, testRecapTUIModel(), recapRuneKey('v'))
	if m.view != recap.ViewYou {
		t.Fatalf("view = %q, want %q", m.view, recap.ViewYou)
	}
	if cmd != nil {
		t.Fatal("view toggle should reuse fetched data")
	}
}

func TestRecapTUIModel_CyclesAgent(t *testing.T) {
	t.Parallel()

	m, cmd := updateRecapTUIModel(t, testRecapTUIModel(), recapRuneKey('a'))
	if m.agent != "claude" {
		t.Fatalf("agent = %q, want claude", m.agent)
	}
	if cmd != nil {
		t.Fatal("agent toggle should reuse fetched data")
	}

	m, _ = updateRecapTUIModel(t, m, recapRuneKey('a'))
	if m.agent != recapTestAgentCodex {
		t.Fatalf("agent = %q, want %s", m.agent, recapTestAgentCodex)
	}

	m, _ = updateRecapTUIModel(t, m, recapRuneKey('a'))
	if m.agent != recap.AgentAll {
		t.Fatalf("agent = %q, want all", m.agent)
	}
}

func TestRecapTUIModel_QuitKeys(t *testing.T) {
	t.Parallel()

	for _, key := range []tea.KeyPressMsg{
		recapRuneKey('q'),
		{Code: 'c', Mod: tea.ModCtrl},
		{Code: tea.KeyEscape},
	} {
		_, cmd := updateRecapTUIModel(t, testRecapTUIModel(), key)
		if cmd == nil {
			t.Fatalf("key %v: expected quit command, got nil", key)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatalf("key %v: expected QuitMsg", key)
		}
	}
}
