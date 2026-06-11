package investigate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
)

// newTestModel returns a fresh model with three agents and a no-op cancel so
// transition tests do not need to spawn a Bubble Tea program.
func newTestModel(t *testing.T) investigateTUIModel {
	t.Helper()
	_, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return newInvestigateTUIModel("a topic", "abcd1234efff",
		[]string{"claude-code", "codex", "gemini-cli"}, 3, 2, cancel)
}

func TestInvestigateTUIModel_TurnLifecycle(t *testing.T) {
	t.Parallel()
	m := newTestModel(t)

	// All rows start queued.
	for i, r := range m.rows {
		if r.status != rowStatusQueued {
			t.Errorf("rows[%d].status = %v, want queued", i, r.status)
		}
	}

	// Start a turn for claude-code → running.
	updated, _ := m.Update(turnStartedMsg{agent: "claude-code", turn: 1})
	m2, ok := updated.(investigateTUIModel)
	if !ok {
		t.Fatalf("Update returned wrong type: %T", updated)
	}
	if got := m2.rows[0].status; got != rowStatusRunning {
		t.Errorf("after TurnStarted: rows[0].status = %v, want running", got)
	}
	if m2.rows[0].currentStart.IsZero() {
		t.Errorf("after TurnStarted: rows[0].currentStart not stamped")
	}

	// Finish that turn with approve → queued, turn count incremented, stance recorded.
	updated2, _ := m2.Update(turnFinishedMsg{
		agent:    "claude-code",
		turn:     1,
		stance:   stanceApprove,
		duration: 5 * time.Second,
		failed:   false,
	})
	m3, ok := updated2.(investigateTUIModel)
	if !ok {
		t.Fatalf("Update returned wrong type: %T", updated2)
	}
	if got := m3.rows[0].status; got != rowStatusQueued {
		t.Errorf("after TurnFinished: rows[0].status = %v, want queued", got)
	}
	if got := m3.rows[0].turnsTaken; got != 1 {
		t.Errorf("after TurnFinished: rows[0].turnsTaken = %d, want 1", got)
	}
	if got := m3.rows[0].latestStance; got != stanceApprove {
		t.Errorf("after TurnFinished: rows[0].latestStance = %q, want approve", got)
	}
	if got := m3.rows[0].accumulated; got != 5*time.Second {
		t.Errorf("after TurnFinished: rows[0].accumulated = %v, want 5s", got)
	}
	if got := m3.approvals; got != 1 {
		t.Errorf("after TurnFinished: m.approvals = %d, want 1", got)
	}
}

func TestInvestigateTUIModel_FailedTurnExhaustsBudget(t *testing.T) {
	t.Parallel()
	m := newTestModel(t)
	// Three failures in a row for the same agent should flip status to failed
	// because maxTurns is 3.
	for i := 1; i <= 3; i++ {
		updated, _ := m.Update(turnFinishedMsg{
			agent:    "codex",
			turn:     i,
			stance:   stanceUnknown,
			duration: time.Second,
			failed:   true,
		})
		next, ok := updated.(investigateTUIModel)
		if !ok {
			t.Fatalf("Update returned wrong type: %T", updated)
		}
		m = next
	}
	if got := m.rows[1].status; got != rowStatusFailed {
		t.Errorf("after 3 failures: rows[1].status = %v, want failed", got)
	}
}

func TestInvestigateTUIModel_RunFinishedMarksDone(t *testing.T) {
	t.Parallel()
	m := newTestModel(t)
	// One successful turn for the first agent.
	updated, _ := m.Update(turnFinishedMsg{
		agent:    "claude-code",
		turn:     1,
		stance:   stanceApprove,
		duration: time.Second,
	})
	next, ok := updated.(investigateTUIModel)
	if !ok {
		t.Fatalf("Update returned wrong type after TurnFinished: %T", updated)
	}
	m = next

	updated, _ = m.Update(runFinishedMsg{outcome: OutcomeQuorum})
	next, ok = updated.(investigateTUIModel)
	if !ok {
		t.Fatalf("Update returned wrong type after RunFinished: %T", updated)
	}
	m = next

	if !m.finished {
		t.Errorf("after RunFinished: m.finished = false, want true")
	}
	if got := m.outcome; got != OutcomeQuorum {
		t.Errorf("after RunFinished: m.outcome = %v, want quorum", got)
	}
	// Only the agent that took a turn flips to Done. Agents that never ran
	// (quorum reached before their turn) stay Queued so the dashboard
	// doesn't claim work that didn't happen.
	if got := m.rows[0].status; got != rowStatusDone {
		t.Errorf("rows[0] (turn-taker).status = %v, want done", got)
	}
	for i := 1; i < len(m.rows); i++ {
		if got := m.rows[i].status; got != rowStatusQueued {
			t.Errorf("rows[%d] (un-run).status = %v, want queued", i, got)
		}
	}
}

func TestInvestigateTUIModel_View_ColumnHeaders(t *testing.T) {
	t.Parallel()
	m := newTestModel(t)
	view := m.dashboardView()
	for _, h := range []string{"AGENT", "STATUS", "DURATION", "TURN", "APPROVED"} {
		if !strings.Contains(view, h) {
			t.Errorf("dashboardView missing header %q\nfull view:\n%s", h, view)
		}
	}
	if !strings.Contains(view, "Ctrl+C: cancel") {
		t.Errorf("dashboardView missing cancel hint\nfull view:\n%s", view)
	}
}

func TestFormatStance(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{stanceApprove, "✓ approve"},
		{stanceRequestChanges, "✗ changes"},
		{stanceReject, "✗ reject"},
		{stanceUnknown, ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := formatStance(c.in); got != c.want {
			t.Errorf("formatStance(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   time.Duration
		want string
	}{
		{300 * time.Millisecond, "300ms"},
		{5*time.Second + 200*time.Millisecond, "5.2s"},
		{90 * time.Second, "1m30s"},
	}
	for _, c := range cases {
		if got := formatDuration(c.in); got != c.want {
			t.Errorf("formatDuration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestModel_HandleTurnStartedAppendsTimelineEntry(t *testing.T) {
	t.Parallel()
	m := newInvestigateTUIModel("topic", "run", []string{"claude-code"}, 3, 1, func() {})
	next, _ := m.Update(turnStartedMsg{agent: "claude-code", turn: 1})
	got, ok := next.(investigateTUIModel)
	require.True(t, ok)
	require.Len(t, got.rows[0].buffer, 1)
	require.Equal(t, "started", got.rows[0].buffer[0].kind)
	require.Equal(t, 1, got.rows[0].buffer[0].turn)
}

func TestModel_HandleTurnFinishedAppendsFinishedEntry(t *testing.T) {
	t.Parallel()
	m := newInvestigateTUIModel("topic", "run", []string{"claude-code"}, 3, 1, func() {})
	m1, _ := m.Update(turnStartedMsg{agent: "claude-code", turn: 1})
	m1Model, ok := m1.(investigateTUIModel)
	require.True(t, ok)
	m2, _ := m1Model.Update(turnFinishedMsg{
		agent:    "claude-code",
		turn:     1,
		stance:   stanceApprove,
		duration: 2 * time.Second,
		findings: "shared findings doc updated",
	})
	got, ok := m2.(investigateTUIModel)
	require.True(t, ok)
	require.Len(t, got.rows[0].buffer, 2)
	require.Equal(t, "finished", got.rows[0].buffer[1].kind)
	require.Equal(t, "shared findings doc updated", got.rows[0].buffer[1].findings)
}

func TestModel_HandleTurnFinishedFailedAppendsFailedEntry(t *testing.T) {
	t.Parallel()
	m := newInvestigateTUIModel("topic", "run", []string{"codex"}, 3, 1, func() {})
	m1, _ := m.Update(turnFinishedMsg{
		agent:    "codex",
		turn:     1,
		duration: 500 * time.Millisecond,
		failed:   true,
		err:      errors.New("spawner exited"),
	})
	got, ok := m1.(investigateTUIModel)
	require.True(t, ok)
	require.Len(t, got.rows[0].buffer, 1)
	require.Equal(t, "failed", got.rows[0].buffer[0].kind)
	require.Equal(t, "spawner exited", got.rows[0].buffer[0].errStr)
}

// updateModel sends msg to m via Update and returns the new
// investigateTUIModel, failing the test on a type assertion mismatch.
func updateModel(t *testing.T, m investigateTUIModel, msg tea.Msg) investigateTUIModel {
	t.Helper()
	next, _ := m.Update(msg)
	got, ok := next.(investigateTUIModel)
	require.True(t, ok, "Update returned wrong type: %T", next)
	return got
}

func TestModel_CtrlOEntersDetail(t *testing.T) {
	t.Parallel()
	m := newInvestigateTUIModel("topic", "run", []string{"claude-code", "codex"}, 3, 2, func() {})
	got := updateModel(t, m, tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	require.True(t, got.detailMode, "Ctrl+O must enter detail mode")
	require.Equal(t, 0, got.detailIdx)
}

func TestModel_EscReturnsFromDetail(t *testing.T) {
	t.Parallel()
	m := newInvestigateTUIModel("topic", "run", []string{"claude-code"}, 3, 1, func() {})
	inDetail := updateModel(t, m, tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	got := updateModel(t, inDetail, tea.KeyPressMsg{Code: tea.KeyEscape})
	require.False(t, got.detailMode)
}

func TestModel_LeftRightCyclesAgents(t *testing.T) {
	t.Parallel()
	m := newInvestigateTUIModel("topic", "run", []string{"a", "b", "c"}, 3, 1, func() {})
	inDetail := updateModel(t, m, tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	right := updateModel(t, inDetail, tea.KeyPressMsg{Code: tea.KeyRight})
	require.Equal(t, 1, right.detailIdx)
	right2 := updateModel(t, right, tea.KeyPressMsg{Code: tea.KeyRight})
	require.Equal(t, 2, right2.detailIdx)
	wrap := updateModel(t, right2, tea.KeyPressMsg{Code: tea.KeyRight})
	require.Equal(t, 0, wrap.detailIdx, "wraps around")
}

func TestModel_UpDownScrollsInDetail(t *testing.T) {
	t.Parallel()
	m := newInvestigateTUIModel("topic", "run", []string{"a"}, 3, 1, func() {})
	// Seed two entries so there's room to scroll up.
	m.rows[0].buffer = []timelineEntry{
		{turn: 1, kind: "started"},
		{turn: 1, kind: "finished"},
	}
	inDetail := updateModel(t, m, tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	// detailScroll starts at len-1 == 1 (most recent).
	require.Equal(t, 1, inDetail.detailScroll)
	up := updateModel(t, inDetail, tea.KeyPressMsg{Code: tea.KeyUp})
	require.Equal(t, 0, up.detailScroll)
	// Clamped at 0.
	up2 := updateModel(t, up, tea.KeyPressMsg{Code: tea.KeyUp})
	require.Equal(t, 0, up2.detailScroll)
	down := updateModel(t, up2, tea.KeyPressMsg{Code: tea.KeyDown})
	require.Equal(t, 1, down.detailScroll)
}

func TestModel_MouseWheelInDashboardIgnored(t *testing.T) {
	t.Parallel()
	m := newInvestigateTUIModel("topic", "run", []string{"a"}, 3, 1, func() {})
	m.rows[0].buffer = []timelineEntry{{turn: 1, kind: "started"}, {turn: 1, kind: "finished"}}
	next := updateModel(t, m, tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	require.Equal(t, 0, next.detailScroll)
}

func TestModel_MouseWheelInDetailScrolls(t *testing.T) {
	t.Parallel()
	m := newInvestigateTUIModel("topic", "run", []string{"a"}, 3, 1, func() {})
	m.rows[0].buffer = []timelineEntry{{turn: 1, kind: "started"}, {turn: 1, kind: "finished"}}
	inDetail := updateModel(t, m, tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	// detailScroll starts at 1 (most recent).
	up := updateModel(t, inDetail, tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	require.Equal(t, 0, up.detailScroll)
	down := updateModel(t, up, tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	require.Equal(t, 1, down.detailScroll)
}
