package cli

import (
	"errors"
	"strings"
	"testing"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
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

func testRecapTUIScrollModel() recapTUIModel {
	vp := viewport.New(viewport.WithWidth(80), viewport.WithHeight(3))
	vp.SetContent(strings.Join([]string{
		"line 1",
		"line 2",
		"line 3",
		"line 4",
		"line 5",
		"line 6",
	}, "\n"))

	m := testRecapTUIModel()
	m.viewport = vp
	m.ready = true
	m.width = 80
	m.height = 4
	return m
}

func TestRecapTUIModel_IgnoresStaleFetchResults(t *testing.T) {
	t.Parallel()

	m := testRecapTUIModel()
	m.requestID = 2
	m.loading = true
	m.resp = nil

	staleResp := &recap.MeRecapResponse{Summary: recap.Summary{
		Me: recap.SummaryTotals{Sessions: 99},
	}}
	m, cmd := updateRecapTUIModel(t, m, recapDataMsg{requestID: 1, resp: staleResp})
	if cmd != nil {
		t.Fatal("stale data should not return a command")
	}
	if m.resp != nil {
		t.Fatalf("stale data should not replace current response: %#v", m.resp)
	}
	if !m.loading {
		t.Fatal("stale data should leave current request loading")
	}

	m, _ = updateRecapTUIModel(t, m, recapErrMsg{requestID: 1, err: errors.New("old failure")})
	if m.loadErr != nil {
		t.Fatalf("stale error should be ignored: %v", m.loadErr)
	}

	freshResp := &recap.MeRecapResponse{Summary: recap.Summary{
		Me: recap.SummaryTotals{Sessions: 1},
	}}
	m, _ = updateRecapTUIModel(t, m, recapDataMsg{requestID: 2, resp: freshResp})
	if m.resp != freshResp {
		t.Fatal("current data should update the response")
	}
	if m.loading {
		t.Fatal("current data should clear loading")
	}
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

func TestRecapTUIModel_ScrollKeys(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		key  tea.KeyPressMsg
	}{
		{name: "arrow down", key: tea.KeyPressMsg{Code: tea.KeyDown}},
		{name: "vim down", key: recapRuneKey('j')},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m, _ := updateRecapTUIModel(t, testRecapTUIScrollModel(), tt.key)
			if m.viewport.YOffset() != 1 {
				t.Fatalf("YOffset = %d, want 1", m.viewport.YOffset())
			}
		})
	}
}

func TestRecapTUIModel_TopBottomKeys(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		key     tea.KeyPressMsg
		wantTop bool
	}{
		{name: "home", key: tea.KeyPressMsg{Code: tea.KeyHome}, wantTop: true},
		{name: "vim top", key: recapRuneKey('g'), wantTop: true},
		{name: "end", key: tea.KeyPressMsg{Code: tea.KeyEnd}},
		{name: "vim bottom", key: recapRuneKey('G')},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := testRecapTUIScrollModel()
			if tt.wantTop {
				m.viewport.GotoBottom()
			}
			m, _ = updateRecapTUIModel(t, m, tt.key)

			if tt.wantTop && !m.viewport.AtTop() {
				t.Fatalf("viewport should be at top, YOffset = %d", m.viewport.YOffset())
			}
			if !tt.wantTop && !m.viewport.AtBottom() {
				t.Fatalf("viewport should be at bottom, YOffset = %d", m.viewport.YOffset())
			}
		})
	}
}

func TestRecapTUIModel_FooterFitsWidth(t *testing.T) {
	t.Parallel()

	m := testRecapTUIModel()
	m.width = 80
	footer := m.renderFooter()
	if got := lipgloss.Width(footer); got > m.width {
		t.Fatalf("wide footer width = %d, want <= %d: %q", got, m.width, footer)
	}
	for _, want := range []string{"t range", "v view", "a agent", "r refresh", "↑/↓ scroll", "q quit"} {
		if !strings.Contains(footer, want) {
			t.Fatalf("wide footer missing %q: %q", want, footer)
		}
	}

	m.width = 18
	footer = m.renderFooter()
	if got := lipgloss.Width(footer); got > m.width {
		t.Fatalf("narrow footer width = %d, want <= %d: %q", got, m.width, footer)
	}
	if !strings.Contains(footer, "q quit") {
		t.Fatalf("narrow footer should keep quit help: %q", footer)
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
