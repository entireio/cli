package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func updateExpertsTUI(t *testing.T, m expertsTUIModel, msg tea.Msg) expertsTUIModel {
	t.Helper()
	next, _ := m.Update(msg)
	tm, ok := next.(expertsTUIModel)
	if !ok {
		t.Fatalf("Update returned %T, want expertsTUIModel", next)
	}
	return tm
}

func newSizedExpertsTUI(t *testing.T, resp expertsResponse) expertsTUIModel {
	t.Helper()
	// Color off keeps assertions on raw text (no ANSI escapes to match around).
	// A tall window keeps the whole detail pane visible so content assertions
	// aren't tripped by viewport scrolling.
	m := newExpertsTUIModel(resp, false)
	return updateExpertsTUI(t, m, tea.WindowSizeMsg{Width: 120, Height: 44})
}

func decodeExpertsFixture(t *testing.T) expertsResponse {
	t.Helper()
	var resp expertsResponse
	if err := json.Unmarshal([]byte(expertsSuccessBody()), &resp); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return resp
}

func expertsTUIView(t *testing.T, m expertsTUIModel) string {
	t.Helper()
	return m.View().Content
}

func TestExpertsTUIRendersAgentCenteredEvidence(t *testing.T) {
	m := newSizedExpertsTUI(t, decodeExpertsFixture(t))
	text := expertsTUIView(t, m)

	for _, want := range []string{
		"Agent provenance", "acme/widget", "Codex",
		"EVIDENCE", "SKILLS", "go-cli", "TOOLS", "shell",
		"SESSIONS", "feat: experts provenance",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("TUI view missing %q:\n%s", want, text)
		}
	}

	// The privacy boundary that the plain/JSON renderers enforce must hold in
	// the TUI too: no raw human-identity fields leak into the view.
	for _, forbidden := range []string{"peyton", "first_commit_author_username"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("TUI view should not expose %q:\n%s", forbidden, text)
		}
	}
}

func TestExpertsTUIEnterTogglesSessionEvidence(t *testing.T) {
	m := newSizedExpertsTUI(t, decodeExpertsFixture(t))

	if got := expertsTUIView(t, m); strings.Contains(got, "cp-1") {
		t.Fatalf("checkpoint ids should be hidden before expanding:\n%s", got)
	}

	m = updateExpertsTUI(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.expanded {
		t.Fatal("enter should set expanded=true")
	}
	if got := expertsTUIView(t, m); !strings.Contains(got, "cp-1") || !strings.Contains(got, "cp-2") {
		t.Fatalf("expanded view should reveal checkpoint ids:\n%s", got)
	}

	m = updateExpertsTUI(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.expanded {
		t.Fatal("second enter should collapse evidence")
	}
	if got := expertsTUIView(t, m); strings.Contains(got, "cp-1") {
		t.Fatalf("collapsed view should hide checkpoint ids again:\n%s", got)
	}
}

func TestExpertsTUINavigationClampsCursor(t *testing.T) {
	resp := expertsResponse{
		RepoFullName: "acme/widget",
		Branch:       "main",
		Scopes:       []string{"cmd/"},
		Profiles: []expertsProfile{
			{AgentID: "codex", AgentLabel: "Codex", SessionCount: 1, CheckpointCount: 2, StepCount: 9},
			{AgentID: "claude", AgentLabel: "Claude", SessionCount: 1, CheckpointCount: 1, StepCount: 3},
		},
	}
	m := newSizedExpertsTUI(t, resp)
	if m.cursor != 0 {
		t.Fatalf("initial cursor = %d, want 0", m.cursor)
	}

	// Down moves to the second profile and the selection caret follows.
	m = updateExpertsTUI(t, m, tea.KeyPressMsg{Code: 'j', Text: "j"})
	if m.cursor != 1 {
		t.Fatalf("after down cursor = %d, want 1", m.cursor)
	}
	if got := expertsTUIView(t, m); !strings.Contains(got, "▸ Claude") || strings.Contains(got, "▸ Codex") {
		t.Fatalf("selection caret should be on Claude:\n%s", got)
	}

	// Down past the end clamps.
	m = updateExpertsTUI(t, m, tea.KeyPressMsg{Code: 'j', Text: "j"})
	if m.cursor != 1 {
		t.Fatalf("down past end cursor = %d, want 1", m.cursor)
	}

	// Up returns to the first, and up past the start clamps.
	m = updateExpertsTUI(t, m, tea.KeyPressMsg{Code: 'k', Text: "k"})
	m = updateExpertsTUI(t, m, tea.KeyPressMsg{Code: 'k', Text: "k"})
	if m.cursor != 0 {
		t.Fatalf("up past start cursor = %d, want 0", m.cursor)
	}
}

func TestListScrollStartKeepsSelectionVisible(t *testing.T) {
	const profileLines = 2
	tests := []struct {
		name   string
		cursor int
		height int
		total  int
		want   int
	}{
		{name: "fits without scroll", cursor: 0, height: 10, total: 6, want: 0},
		{name: "first item", cursor: 0, height: 4, total: 10, want: 0},
		{name: "middle item scrolls down", cursor: 2, height: 4, total: 10, want: 2},
		{name: "last item scrolls to end", cursor: 4, height: 4, total: 10, want: 6},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := listScrollStart(tc.cursor, profileLines, tc.height, tc.total); got != tc.want {
				t.Fatalf("listScrollStart(%d, %d, %d) = %d, want %d", tc.cursor, tc.height, tc.total, got, tc.want)
			}
		})
	}
}

func TestExpertsTUIListScrollsSelectedAgentIntoView(t *testing.T) {
	profiles := make([]expertsProfile, 5)
	for i := range profiles {
		profiles[i] = expertsProfile{
			AgentID:    fmt.Sprintf("agent-%d", i),
			AgentLabel: fmt.Sprintf("Agent %d", i),
		}
	}
	resp := expertsResponse{RepoFullName: "acme/widget", Branch: "main", Scopes: []string{"cmd/"}, Profiles: profiles}
	m := newExpertsTUIModel(resp, false)
	// Short window: body height leaves room for only two agent rows in the list pane.
	m = updateExpertsTUI(t, m, tea.WindowSizeMsg{Width: 80, Height: 12})

	for i := range profiles {
		m.cursor = i
		got := m.renderList(m.listPaneWidth(), m.bodyHeight())
		if !strings.Contains(got, "▸ Agent "+strconv.Itoa(i)) {
			t.Fatalf("cursor=%d: selected agent not visible in list pane:\n%s", i, got)
		}
	}
}

func TestExpertsTUITabCyclesSections(t *testing.T) {
	m := newSizedExpertsTUI(t, decodeExpertsFixture(t))
	if len(m.sectionOffsets) < 2 {
		t.Fatalf("expected multiple section offsets, got %d", len(m.sectionOffsets))
	}
	if m.sectionIdx != 0 {
		t.Fatalf("initial sectionIdx = %d, want 0", m.sectionIdx)
	}

	m = updateExpertsTUI(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
	if m.sectionIdx != 1 {
		t.Fatalf("after tab sectionIdx = %d, want 1", m.sectionIdx)
	}

	// Cycling forward through every section wraps back to the start.
	for range m.sectionOffsets {
		m = updateExpertsTUI(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
	}
	if m.sectionIdx != 1 {
		t.Fatalf("after wrapping sectionIdx = %d, want 1", m.sectionIdx)
	}
}

func TestExpertsTUIQuitKeyEmitsQuit(t *testing.T) {
	m := newSizedExpertsTUI(t, decodeExpertsFixture(t))
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if cmd == nil {
		t.Fatal("quit key should return a command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("quit command should emit tea.QuitMsg, got %T", cmd())
	}
}

func TestExpertsTUIViewEmptyBeforeWindowSize(t *testing.T) {
	m := newExpertsTUIModel(decodeExpertsFixture(t), false)
	if got := expertsTUIView(t, m); got != "" {
		t.Fatalf("view before WindowSizeMsg should be empty, got %q", got)
	}
}

func TestExpertsSessionURL(t *testing.T) {
	t.Setenv("ENTIRE_WEB_BASE_URL", "https://entire.io")
	if got, want := expertsSessionURL("entireio/cli", "abc123"), "https://entire.io/gh/entireio/cli/session/abc123"; got != want {
		t.Fatalf("url = %q, want %q", got, want)
	}
	if got := expertsSessionURL("entireio/cli", ""); got != "" {
		t.Fatalf("empty session id should yield empty url, got %q", got)
	}
	if got := expertsSessionURL("noslash", "abc"); got != "" {
		t.Fatalf("invalid repo should yield empty url, got %q", got)
	}

	// A trailing slash on the base is normalized away.
	t.Setenv("ENTIRE_WEB_BASE_URL", "https://entire.io/")
	if got, want := expertsSessionURL("o/r", "s"), "https://entire.io/gh/o/r/session/s"; got != want {
		t.Fatalf("trailing-slash url = %q, want %q", got, want)
	}
}

func TestExpertsWebBaseFallsBackToEntireForLocalAPI(t *testing.T) {
	t.Setenv("ENTIRE_WEB_BASE_URL", "")

	// A local data API must NOT leak into session links — they should point at
	// the real entire.io web app.
	t.Setenv("ENTIRE_API_BASE_URL", "http://127.0.0.1:8787")
	if got, want := expertsSessionURL("entireio/cli", "abc"), "https://entire.io/gh/entireio/cli/session/abc"; got != want {
		t.Fatalf("local API url = %q, want %q", got, want)
	}

	// A real entire.io API origin is used as-is (frontend shares the host).
	t.Setenv("ENTIRE_API_BASE_URL", "https://entire.io")
	if got, want := expertsSessionURL("entireio/cli", "abc"), "https://entire.io/gh/entireio/cli/session/abc"; got != want {
		t.Fatalf("prod API url = %q, want %q", got, want)
	}
}

func TestExpertsTUIOpenKeyTargetsPrimarySession(t *testing.T) {
	t.Setenv("ENTIRE_WEB_BASE_URL", "https://entire.io")
	m := newSizedExpertsTUI(t, decodeExpertsFixture(t))

	if got, want := m.primarySessionURL(), "https://entire.io/gh/acme/widget/session/sess-a"; got != want {
		t.Fatalf("primary session url = %q, want %q", got, want)
	}

	_, cmd := m.Update(tea.KeyPressMsg{Code: 'o', Text: "o"})
	if cmd == nil {
		t.Fatal("'o' should return a command to open the session")
	}
	// Under test openBrowser is a no-op (no real browser is spawned); the
	// command must still run cleanly and emit no message.
	if msg := cmd(); msg != nil {
		t.Fatalf("open command should emit no message, got %T", msg)
	}
}

func TestRenderExpertsLinksSessionsWhenStyled(t *testing.T) {
	t.Setenv("ENTIRE_WEB_BASE_URL", "https://entire.io")
	resp := decodeExpertsFixture(t)

	var buf bytes.Buffer
	renderExpertsWithStyles(&buf, resp, expertsStylesForColor(true))
	out := buf.String()

	if !strings.Contains(out, "\x1b]8;;") {
		t.Fatalf("styled output should contain an OSC 8 hyperlink:\n%q", out)
	}
	if !strings.Contains(out, "https://entire.io/gh/acme/widget/session/sess-a") {
		t.Fatalf("styled output should link to the session url:\n%q", out)
	}

	// Plain (no color) output must stay link-free and unchanged for scripts.
	var plain bytes.Buffer
	renderExpertsWithStyles(&plain, resp, expertsStylesForColor(false))
	if strings.Contains(plain.String(), "\x1b]8;;") {
		t.Fatalf("plain output should not contain hyperlinks:\n%q", plain.String())
	}
}

func TestExpertsTUIExpandShowsSessionLink(t *testing.T) {
	t.Setenv("ENTIRE_WEB_BASE_URL", "https://entire.io")
	m := newExpertsTUIModel(decodeExpertsFixture(t), false)
	m = updateExpertsTUI(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	m = updateExpertsTUI(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	want := "https://entire.io/gh/acme/widget/session/sess-a"
	if got := expertsTUIView(t, m); !strings.Contains(got, want) {
		t.Fatalf("expanded view should show session link %q:\n%s", want, got)
	}
}
