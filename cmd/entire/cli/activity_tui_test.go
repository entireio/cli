package cli

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func testActivityTUIModel() activityModel {
	vp := viewport.New(viewport.WithWidth(80), viewport.WithHeight(3))
	vp.SetContent(strings.Join([]string{
		"line 1",
		"line 2",
		"line 3",
		"line 4",
		"line 5",
		"line 6",
	}, "\n"))

	return activityModel{
		viewport: vp,
		ready:    true,
		width:    80,
		height:   4,
	}
}

func updateActivityModel(t *testing.T, m activityModel, msg tea.Msg) (activityModel, tea.Cmd) {
	t.Helper()

	updated, cmd := m.Update(msg)
	result, ok := updated.(activityModel)
	if !ok {
		t.Fatalf("Update returned %T, want activityModel", updated)
	}
	return result, cmd
}

func activityRuneKey(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Text: string(r)}
}

func TestActivityModel_ScrollKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  tea.KeyPressMsg
	}{
		{name: "arrow down", key: tea.KeyPressMsg{Code: tea.KeyDown}},
		{name: "vim down", key: activityRuneKey('j')},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m, _ := updateActivityModel(t, testActivityTUIModel(), tt.key)
			if m.viewport.YOffset() != 1 {
				t.Fatalf("YOffset = %d, want 1", m.viewport.YOffset())
			}
		})
	}
}

func TestActivityModel_ScrollUpKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  tea.KeyPressMsg
	}{
		{name: "arrow up", key: tea.KeyPressMsg{Code: tea.KeyUp}},
		{name: "vim up", key: activityRuneKey('k')},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := testActivityTUIModel()
			m.viewport.SetYOffset(2)
			m, _ = updateActivityModel(t, m, tt.key)
			if m.viewport.YOffset() != 1 {
				t.Fatalf("YOffset = %d, want 1", m.viewport.YOffset())
			}
		})
	}
}

func TestActivityModel_TopBottomKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		key      tea.KeyPressMsg
		gotoTop  bool
		wantDesc string
	}{
		{name: "home", key: tea.KeyPressMsg{Code: tea.KeyHome}, gotoTop: true, wantDesc: "top"},
		{name: "vim top", key: activityRuneKey('g'), gotoTop: true, wantDesc: "top"},
		{name: "end", key: tea.KeyPressMsg{Code: tea.KeyEnd}, wantDesc: "bottom"},
		{name: "vim bottom", key: activityRuneKey('G'), wantDesc: "bottom"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := testActivityTUIModel()
			if tt.gotoTop {
				m.viewport.GotoBottom()
			}
			m, _ = updateActivityModel(t, m, tt.key)

			if tt.gotoTop && !m.viewport.AtTop() {
				t.Fatalf("viewport should be at %s, YOffset = %d", tt.wantDesc, m.viewport.YOffset())
			}
			if !tt.gotoTop && !m.viewport.AtBottom() {
				t.Fatalf("viewport should be at %s, YOffset = %d", tt.wantDesc, m.viewport.YOffset())
			}
		})
	}
}

func TestActivityModel_QuitKeys(t *testing.T) {
	t.Parallel()

	quitKeys := []tea.KeyPressMsg{
		activityRuneKey('q'),
		{Code: 'c', Mod: tea.ModCtrl},
		{Code: tea.KeyEscape},
	}

	for _, key := range quitKeys {
		_, cmd := updateActivityModel(t, testActivityTUIModel(), key)
		if cmd == nil {
			t.Fatalf("key %v: expected quit command, got nil", key)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatalf("key %v: expected QuitMsg", key)
		}
	}
}

func TestActivityModel_FooterDocumentsVisibleControlsOnly(t *testing.T) {
	t.Parallel()

	m := testActivityTUIModel()
	m.sty = newActivityStylesWithWidth(m.width, true)

	footer := m.renderFooter()
	for _, want := range []string{"↑/↓, j/k", " scroll", "home/end, g/G", " top/bottom", "q", " quit"} {
		if !strings.Contains(footer, want) {
			t.Fatalf("footer missing %q: %q", want, footer)
		}
	}
}

func TestActivityModel_FooterScrollPercentFitsWidth(t *testing.T) {
	t.Parallel()

	m := testActivityTUIModel()
	m.sty = newActivityStylesWithWidth(m.width, true)

	footer := m.renderFooter()
	if got := lipgloss.Width(footer); got != m.width {
		t.Fatalf("wide footer width = %d, want %d: %q", got, m.width, footer)
	}

	m.width = 50
	m.sty = newActivityStylesWithWidth(m.width, true)

	footer = m.renderFooter()
	if got := lipgloss.Width(footer); got != m.width {
		t.Fatalf("compact footer width = %d, want %d: %q", got, m.width, footer)
	}
	for _, want := range []string{"↑/↓", " scroll", "home/end", " top/bottom", "q", " quit"} {
		if !strings.Contains(footer, want) {
			t.Fatalf("compact footer missing %q: %q", want, footer)
		}
	}
	for _, hidden := range []string{"j/k", "g/G"} {
		if strings.Contains(footer, hidden) {
			t.Fatalf("compact footer should drop %q: %q", hidden, footer)
		}
	}

	m.width = 20
	m.sty = newActivityStylesWithWidth(m.width, true)

	footer = m.renderFooter()
	if got := lipgloss.Width(footer); got != m.width {
		t.Fatalf("narrow footer width = %d, want %d: %q", got, m.width, footer)
	}
}
