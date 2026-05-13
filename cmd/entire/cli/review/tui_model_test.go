package review

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// newTestModel returns a reviewTUIModel wired to the provided cancel stub.
func newTestModel(agents []string, cancel func()) reviewTUIModel {
	return newReviewTUIModel(agents, cancel)
}

func testKey(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code}
}

func testCtrlKey(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code, Mod: tea.ModCtrl}
}

// mustModel extracts a reviewTUIModel from a tea.Model, failing the test if
// the assertion fails.
func mustModel(t *testing.T, m tea.Model) reviewTUIModel {
	t.Helper()
	tm, ok := m.(reviewTUIModel)
	if !ok {
		t.Fatalf("expected reviewTUIModel, got %T", m)
	}
	return tm
}

func TestTUIModel_AgentEvent_StampsRunStartOnFirstEvent(t *testing.T) {
	t.Parallel()
	m := newTestModel([]string{"agent-a", "agent-b"}, func() {})

	before := time.Now()
	updated, _ := m.Update(agentEventMsg{agent: "agent-a", ev: reviewtypes.Started{}})
	after := time.Now()

	m2 := mustModel(t, updated)
	row := m2.rows[m2.rowIdx["agent-a"]]
	if row.runStart.IsZero() {
		t.Fatal("runStart should be stamped after first event; got zero")
	}
	if row.runStart.Before(before) || row.runStart.After(after) {
		t.Errorf("runStart %v not in expected range [%v, %v]", row.runStart, before, after)
	}
	// Second event should NOT update runStart.
	firstStart := row.runStart
	updated2, _ := m2.Update(agentEventMsg{agent: "agent-a", ev: reviewtypes.Started{}})
	m3 := mustModel(t, updated2)
	if m3.rows[m3.rowIdx["agent-a"]].runStart != firstStart {
		t.Error("runStart should not be overwritten on subsequent events")
	}
}

func TestTUIModel_AgentEvent_TokensOverwrite(t *testing.T) {
	t.Parallel()
	m := newTestModel([]string{"agent-a"}, func() {})

	updated1, _ := m.Update(agentEventMsg{agent: "agent-a", ev: reviewtypes.Tokens{In: 100, Out: 50}})
	m1 := mustModel(t, updated1)
	updated2, _ := m1.Update(agentEventMsg{agent: "agent-a", ev: reviewtypes.Tokens{In: 200, Out: 80}})
	m2 := mustModel(t, updated2)

	row := m2.rows[0]
	if row.tokens.In != 200 || row.tokens.Out != 80 {
		t.Errorf("expected tokens {200 80} after overwrite, got {%d %d}", row.tokens.In, row.tokens.Out)
	}
}

func TestTUIModel_AgentEvent_FinishedSuccess(t *testing.T) {
	t.Parallel()
	m := newTestModel([]string{"agent-a"}, func() {})

	updated, _ := m.Update(agentEventMsg{agent: "agent-a", ev: reviewtypes.Finished{Success: true}})
	row := mustModel(t, updated).rows[0]
	if row.status != reviewtypes.AgentStatusSucceeded {
		t.Errorf("expected Succeeded, got %v", row.status)
	}
	if row.runEnd.IsZero() {
		t.Error("runEnd should be stamped on Finished")
	}
}

func TestTUIModel_AgentEvent_FinishedFailure(t *testing.T) {
	t.Parallel()
	m := newTestModel([]string{"agent-a"}, func() {})

	updated, _ := m.Update(agentEventMsg{agent: "agent-a", ev: reviewtypes.Finished{Success: false}})
	row := mustModel(t, updated).rows[0]
	if row.status != reviewtypes.AgentStatusFailed {
		t.Errorf("expected Failed, got %v", row.status)
	}
}

func TestTUIModel_AgentEvent_RunError(t *testing.T) {
	t.Parallel()
	m := newTestModel([]string{"agent-a"}, func() {})

	theErr := errors.New("agent crashed")
	updated, _ := m.Update(agentEventMsg{agent: "agent-a", ev: reviewtypes.RunError{Err: theErr}})
	row := mustModel(t, updated).rows[0]
	if row.status != reviewtypes.AgentStatusFailed {
		t.Errorf("expected Failed on RunError, got %v", row.status)
	}
	if row.err == nil {
		t.Error("row.err should be set after RunError")
	}
}

func TestTUIModel_DashboardShowsErrorPreviewForFailedAgent(t *testing.T) {
	t.Parallel()
	m := newTestModel([]string{"codex"}, func() {})
	m.termWidth = 200

	theErr := errors.New("auth: invalid API key - check ANTHROPIC_API_KEY")
	updated, _ := m.Update(agentEventMsg{agent: "codex", ev: reviewtypes.RunError{Err: theErr}})
	m = mustModel(t, updated)

	out := m.dashboardView()
	if !strings.Contains(out, "auth: invalid API key") {
		t.Errorf("expected error text in dashboard preview when agent failed, got:\n%s", out)
	}
}

func TestTUIModel_DashboardErrorPreviewStripsProcessErrorWrapper(t *testing.T) {
	t.Parallel()
	m := newTestModel([]string{"codex"}, func() {})
	m.termWidth = 200

	pe := &reviewtypes.ProcessError{
		AgentName: "codex",
		Err:       errors.New("exit status 1"),
		Stderr:    "Error: rate limit exceeded (RPS quota)\nRetry after: 47s",
	}
	updated, _ := m.Update(agentEventMsg{agent: "codex", ev: reviewtypes.RunError{Err: pe}})
	m = mustModel(t, updated)

	out := m.dashboardView()
	if !strings.Contains(out, "Error: rate limit exceeded") {
		t.Errorf("preview must show first stderr line, got:\n%s", out)
	}
	for _, noise := range []string{
		"error: codex:",
		"exit status 1:",
		"stderr:",
	} {
		if strings.Contains(out, noise) {
			t.Errorf("preview must not contain wrapper text %q, got:\n%s", noise, out)
		}
	}
}

func TestTUIModel_DashboardErrorPreviewFallsBackToErrStringForNonProcessError(t *testing.T) {
	t.Parallel()
	m := newTestModel([]string{"codex"}, func() {})
	m.termWidth = 200

	updated, _ := m.Update(agentEventMsg{agent: "codex", ev: reviewtypes.RunError{Err: errors.New("torn stdout stream")}})
	m = mustModel(t, updated)

	out := m.dashboardView()
	if !strings.Contains(out, "torn stdout stream") {
		t.Errorf("generic error should render verbatim in preview, got:\n%s", out)
	}
}

func TestTUIModel_DashboardErrorPreviewYieldsToAssistantTextBeforeFailure(t *testing.T) {
	t.Parallel()
	m := newTestModel([]string{"codex"}, func() {})
	m.termWidth = 200

	updated, _ := m.Update(agentEventMsg{agent: "codex", ev: reviewtypes.AssistantText{Text: "Found a real issue worth fixing"}})
	m = mustModel(t, updated)
	updated, _ = m.Update(agentEventMsg{agent: "codex", ev: reviewtypes.Finished{Success: true}})
	m = mustModel(t, updated)

	out := m.dashboardView()
	if !strings.Contains(out, "Found a real issue worth fixing") {
		t.Errorf("happy-path preview must still show assistant text, got:\n%s", out)
	}
}

func TestTUIModel_KeyCtrlC_NotDetailMode_CancelsAndMarksCancelling(t *testing.T) {
	t.Parallel()
	var called atomic.Bool
	cancel := func() { called.Store(true) }

	m := newTestModel([]string{"agent-a"}, cancel)
	updated, cmd := m.Update(testCtrlKey('c'))
	if !called.Load() {
		t.Error("expected cancel to be called on Ctrl+C outside detail mode")
	}
	m2 := mustModel(t, updated)
	if !m2.cancelling {
		t.Error("expected m.cancelling=true after first Ctrl+C")
	}
	// First Ctrl+C must NOT quit — the TUI stays up so agents can drain
	// visibly. Only a second Ctrl+C (or natural finish) dismisses.
	if cmd != nil {
		if msg := cmd(); msg != nil {
			if _, ok := msg.(tea.QuitMsg); ok {
				t.Error("first Ctrl+C must NOT send a quit command; it should wait for agents to drain")
			}
		}
	}
}

func TestTUIModel_KeyCtrlC_DetailMode_Ignored(t *testing.T) {
	t.Parallel()
	var called atomic.Bool
	cancel := func() { called.Store(true) }

	m := newTestModel([]string{"agent-a"}, cancel)
	m.detailMode = true

	_, cmd := m.Update(testCtrlKey('c'))
	if called.Load() {
		t.Error("cancel must NOT be called when Ctrl+C is pressed in detail mode")
	}
	if cmd != nil {
		// Command should be nil (no quit).
		if msg := cmd(); msg != nil {
			if _, ok := msg.(tea.QuitMsg); ok {
				t.Error("Ctrl+C in detail mode must not send a quit command")
			}
		}
	}
}

func TestTUIModel_CancelOnce_DuplicateKeyCtrlC(t *testing.T) {
	t.Parallel()
	var count atomic.Int32
	cancel := func() { count.Add(1) }

	m := newTestModel([]string{"agent-a"}, cancel)
	_, _ = m.Update(testCtrlKey('c'))
	_, _ = m.Update(testCtrlKey('c'))
	if count.Load() != 1 {
		t.Errorf("cancel should fire exactly once even on duplicate Ctrl+C; called %d time(s)", count.Load())
	}
}

func TestTUIModel_KeyCtrlO_EntersDrillIn(t *testing.T) {
	t.Parallel()
	m := newTestModel([]string{"agent-a", "agent-b"}, func() {})

	updated, cmd := m.Update(testCtrlKey('o'))
	m2 := mustModel(t, updated)
	if !m2.detailMode {
		t.Error("expected detailMode=true after Ctrl+O")
	}
	if cmd != nil {
		t.Error("Ctrl+O should not return an alt-screen command in Bubble Tea v2")
	}
	if !m2.View().AltScreen {
		t.Error("expected View().AltScreen=true in detail mode")
	}
	if got := m2.View().MouseMode; got != tea.MouseModeCellMotion {
		t.Errorf("expected View().MouseMode=MouseModeCellMotion in detail mode (so viewport gets wheel events); got %v", got)
	}
}

func TestTUIModel_KeyEsc_ExitsDrillIn(t *testing.T) {
	t.Parallel()
	m := newTestModel([]string{"agent-a"}, func() {})
	m.detailMode = true

	updated, cmd := m.Update(testKey(tea.KeyEscape))
	m2 := mustModel(t, updated)
	if m2.detailMode {
		t.Error("expected detailMode=false after Esc")
	}
	if cmd != nil {
		t.Error("Esc should not return an alt-screen command in Bubble Tea v2")
	}
	if !m2.View().AltScreen {
		t.Error("expected View().AltScreen=true outside detail mode")
	}
	if got := m2.View().MouseMode; got != tea.MouseModeNone {
		t.Errorf("expected View().MouseMode=MouseModeNone outside detail mode (preserve normal terminal selection on dashboard); got %v", got)
	}
}

func TestTUIModel_DashboardUsesAltScreen(t *testing.T) {
	t.Parallel()
	m := newTestModel([]string{"agent-a"}, func() {})

	if !m.View().AltScreen {
		t.Error("expected dashboard View().AltScreen=true")
	}
}

func TestTUIModel_FinishedStopsTickRedraws(t *testing.T) {
	t.Parallel()
	m := newTestModel([]string{"agent-a"}, func() {})
	updated, _ := m.Update(runFinishedMsg{summary: reviewtypes.RunSummary{}})
	m = mustModel(t, updated)

	_, tickCmd := m.Update(tickMsg(time.Now()))
	if tickCmd != nil {
		t.Fatal("finished dashboard should not schedule duration ticks")
	}
	_, spinnerCmd := m.Update(m.spinner.Tick())
	if spinnerCmd != nil {
		t.Fatal("finished dashboard should not schedule spinner ticks")
	}
}

func TestTUIModel_LeftRight_CycleDetailIdx(t *testing.T) {
	t.Parallel()
	agents := []string{"a", "b", "c"}
	m := newTestModel(agents, func() {})
	m.detailMode = true
	m.detailIdx = 0

	// Right: 0 → 1 → 2 → 0 (wrap).
	for _, wantIdx := range []int{1, 2, 0} {
		updated, _ := m.Update(testKey(tea.KeyRight))
		m = mustModel(t, updated)
		if m.detailIdx != wantIdx {
			t.Errorf("after Right: want detailIdx=%d, got %d", wantIdx, m.detailIdx)
		}
	}

	// Left: 0 → 2 (wrap).
	updated, _ := m.Update(testKey(tea.KeyLeft))
	m = mustModel(t, updated)
	if m.detailIdx != 2 {
		t.Errorf("after Left from 0: want detailIdx=2, got %d", m.detailIdx)
	}
}

// TestTUIModel_InitializesDetailViewport pins that the viewport widget on the
// model starts with non-zero default dimensions so that an immediate Ctrl+O
// before a WindowSizeMsg still renders without panicking.
func TestTUIModel_InitializesDetailViewport(t *testing.T) {
	t.Parallel()
	m := newTestModel([]string{"agent-a"}, func() {})
	if w := m.detail.Width(); w <= 0 {
		t.Errorf("expected detail viewport width > 0, got %d", w)
	}
	if h := m.detail.Height(); h <= 0 {
		t.Errorf("expected detail viewport height > 0, got %d", h)
	}
}

// TestTUIModel_DelegatesMouseWheelToViewport pins that mouse-wheel events in
// detail mode reach the viewport's MouseWheelMsg handler. The bug this guards
// against: setting View.MouseMode = MouseModeCellMotion on entry to detail
// mode caused the Program to receive mouse events, but the Update switch
// only routed tea.KeyPressMsg — mouse events fell through unhandled and the
// viewport never got a chance to scroll.
func TestTUIModel_DelegatesMouseWheelToViewport(t *testing.T) {
	t.Parallel()
	m := newTestModel([]string{"agent-a"}, func() {})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	m = mustModel(t, updated)
	long := strings.Repeat("paragraph of text ", 20)
	for range 5 {
		updated, _ = m.Update(agentEventMsg{agent: "agent-a", ev: reviewtypes.AssistantText{Text: long}})
		m = mustModel(t, updated)
	}
	updated, _ = m.Update(testCtrlKey('o'))
	m = mustModel(t, updated)
	m.detail.GotoTop()
	startOffset := m.detail.YOffset()

	updated, _ = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	m = mustModel(t, updated)
	if m.detail.YOffset() <= startOffset {
		t.Errorf("expected mouse-wheel-down to advance viewport YOffset beyond %d; got %d", startOffset, m.detail.YOffset())
	}
}

// TestTUIModel_DelegatesScrollKeysToViewport pins that pager keys (PgDn) in
// detail mode reach the viewport's internal keymap and advance YOffset rather
// than being swallowed by the model's switch.
func TestTUIModel_DelegatesScrollKeysToViewport(t *testing.T) {
	t.Parallel()
	m := newTestModel([]string{"agent-a"}, func() {})
	// Size the window so the viewport has a sensible height.
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	m = mustModel(t, updated)
	// Populate enough content to scroll. AssistantText with long text wraps
	// to many viewport lines.
	long := strings.Repeat("paragraph of text ", 20)
	for range 5 {
		updated, _ = m.Update(agentEventMsg{agent: "agent-a", ev: reviewtypes.AssistantText{Text: long}})
		m = mustModel(t, updated)
	}
	// Enter detail mode (auto-tails to bottom).
	updated, _ = m.Update(testCtrlKey('o'))
	m = mustModel(t, updated)
	// Jump to top so PgDn has somewhere to scroll into.
	m.detail.GotoTop()
	startOffset := m.detail.YOffset()

	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	m = mustModel(t, updated)
	if m.detail.YOffset() <= startOffset {
		t.Errorf("expected PgDn to advance viewport YOffset beyond %d; got %d", startOffset, m.detail.YOffset())
	}
}

func TestTUIModel_WindowSizeMsg_UpdatesDimensions(t *testing.T) {
	t.Parallel()
	m := newTestModel([]string{"agent-a"}, func() {})

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m2 := mustModel(t, updated)
	if m2.termWidth != 120 || m2.termHeight != 40 {
		t.Errorf("expected 120×40, got %d×%d", m2.termWidth, m2.termHeight)
	}
}

func TestTUIModel_TickMsg_ReSchedulesTick(t *testing.T) {
	t.Parallel()
	m := newTestModel([]string{"agent-a"}, func() {})

	_, cmd := m.Update(tickMsg(time.Now()))
	if cmd == nil {
		t.Error("tickMsg should return a non-nil command (re-tick)")
	}
}

func TestTUIModel_RunFinishedMsg_MarksFinished(t *testing.T) {
	t.Parallel()
	m := newTestModel([]string{"agent-a"}, func() {})

	updated, _ := m.Update(runFinishedMsg{summary: reviewtypes.RunSummary{}})
	m2 := mustModel(t, updated)
	if !m2.finished {
		t.Error("model should be finished after runFinishedMsg")
	}
}

// TestTUIModel_PostFinishCtrlOEntersDetailMode pins that Ctrl+O still enters
// drill-in after both agents finish so the user can inspect completed output.
// Previously any-key-quits behavior swallowed Ctrl+O on the post-finish frame.
func TestTUIModel_PostFinishCtrlOEntersDetailMode(t *testing.T) {
	t.Parallel()
	m := newTestModel([]string{"agent-a", "agent-b"}, func() {})
	updated, _ := m.Update(runFinishedMsg{summary: reviewtypes.RunSummary{}})
	m = mustModel(t, updated)
	if !m.finished {
		t.Fatal("setup: expected model finished after runFinishedMsg")
	}

	updated, cmd := m.Update(testCtrlKey('o'))
	m2 := mustModel(t, updated)
	if !m2.detailMode {
		t.Error("expected Ctrl+O post-finish to enter detail mode")
	}
	// Ctrl+O must NOT quit post-finish.
	if cmd != nil {
		if msg := cmd(); msg != nil {
			if _, ok := msg.(tea.QuitMsg); ok {
				t.Error("Ctrl+O post-finish must not produce a quit command")
			}
		}
	}
}

// TestTUIModel_PostFinishQuitsOnExplicitKeys pins that q, Esc, Enter, and Ctrl+C
// each produce tea.QuitMsg when finished and in dashboard mode.
func TestTUIModel_PostFinishQuitsOnExplicitKeys(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		key  tea.KeyPressMsg
	}{
		{"q", testKey('q')},
		{"Esc", testKey(tea.KeyEscape)},
		{"KeyEsc", testKey(tea.KeyEsc)},
		{"Enter", testKey(tea.KeyEnter)},
		{"Ctrl+C", testCtrlKey('c')},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newTestModel([]string{"agent-a"}, func() {})
			updated, _ := m.Update(runFinishedMsg{summary: reviewtypes.RunSummary{}})
			m = mustModel(t, updated)

			_, cmd := m.Update(tc.key)
			if cmd == nil {
				t.Fatalf("expected a command for explicit-exit key %q post-finish", tc.name)
			}
			msg := cmd()
			if _, ok := msg.(tea.QuitMsg); !ok {
				t.Errorf("expected tea.QuitMsg for key %q post-finish, got %T", tc.name, msg)
			}
		})
	}
}

// TestTUIModel_PostFinishIgnoresRandomKeys pins that non-exit keys (e.g. 'x',
// arrow keys) do NOT quit when finished on the dashboard. They fall through to
// normal handling instead of the old any-key-quits shortcut.
func TestTUIModel_PostFinishIgnoresRandomKeys(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		key  tea.KeyPressMsg
	}{
		{"x", testKey('x')},
		{"Right", testKey(tea.KeyRight)},
		{"Left", testKey(tea.KeyLeft)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newTestModel([]string{"agent-a", "agent-b"}, func() {})
			updated, _ := m.Update(runFinishedMsg{summary: reviewtypes.RunSummary{}})
			m = mustModel(t, updated)

			_, cmd := m.Update(tc.key)
			if cmd != nil {
				if msg := cmd(); msg != nil {
					if _, ok := msg.(tea.QuitMsg); ok {
						t.Errorf("key %q must not quit post-finish on dashboard", tc.name)
					}
				}
			}
		})
	}
}

// TestTUIModel_PostFinishFooterUsesExplicitExitKeys pins the dashboard footer
// switches from the old "Press any key to exit." prompt to an explicit-keys
// hint that mentions Ctrl+O and the named exit keys.
func TestTUIModel_PostFinishFooterUsesExplicitExitKeys(t *testing.T) {
	t.Parallel()
	m := newTestModel([]string{"agent-a"}, func() {})
	m.termWidth = 120
	updated, _ := m.Update(runFinishedMsg{summary: reviewtypes.RunSummary{}})
	m = mustModel(t, updated)

	out := m.dashboardView()
	if strings.Contains(out, "Press any key to exit.") {
		t.Errorf("post-finish footer must not show legacy 'Press any key to exit.' line:\n%s", out)
	}
	if !strings.Contains(out, "Ctrl+O") {
		t.Errorf("post-finish footer should mention Ctrl+O for drill-in:\n%s", out)
	}
	if !strings.Contains(out, "q/Esc/Enter") {
		t.Errorf("post-finish footer should list q/Esc/Enter as exit keys:\n%s", out)
	}
}

// TestTUIModel_CtrlCMarksAgentsCancelling pins that the first Ctrl+C while
// agents are still running sets m.cancelling and renderRow reflects an
// in-flight cancellation indicator (distinct from the terminal Cancelled
// state).
func TestTUIModel_CtrlCMarksAgentsCancelling(t *testing.T) {
	t.Parallel()
	m := newTestModel([]string{"agent-a"}, func() {})
	// Stamp runStart so the row is in the running branch of renderRow.
	updated, _ := m.Update(agentEventMsg{agent: "agent-a", ev: reviewtypes.Started{}})
	m = mustModel(t, updated)
	m.termWidth = 120

	// Before Ctrl+C the running row should say "running".
	if status := m.renderRow(m.rows[0]); !strings.Contains(status, "running") {
		t.Fatalf("setup: expected running indicator pre-Ctrl+C; got %q", status)
	}

	updated, _ = m.Update(testCtrlKey('c'))
	m = mustModel(t, updated)
	if !m.cancelling {
		t.Fatal("expected m.cancelling=true after first Ctrl+C")
	}

	got := m.renderRow(m.rows[0])
	if strings.Contains(got, "running") {
		t.Errorf("renderRow should drop the 'running' indicator once cancelling; got %q", got)
	}
	if !strings.Contains(got, "cancel") {
		t.Errorf("renderRow should show a cancelling indicator; got %q", got)
	}
}

// TestTUIModel_FooterDuringCancellation pins that the dashboard footer changes
// while a cancel is in flight (cancelling && !finished) to signal the user
// that draining is in progress and a second Ctrl+C will force quit.
func TestTUIModel_FooterDuringCancellation(t *testing.T) {
	t.Parallel()
	m := newTestModel([]string{"agent-a"}, func() {})
	m.termWidth = 120
	updated, _ := m.Update(testCtrlKey('c'))
	m = mustModel(t, updated)

	out := m.dashboardView()
	if !strings.Contains(out, "Cancelling agents") {
		t.Errorf("expected footer to announce cancellation in progress; got:\n%s", out)
	}
	if !strings.Contains(out, "Ctrl+C again") {
		t.Errorf("expected footer to mention force-quit hint; got:\n%s", out)
	}
}

// TestTUIModel_SecondCtrlCForceQuits pins that once cancelling is in flight,
// a second Ctrl+C emits tea.QuitMsg immediately rather than waiting for
// agents to drain.
func TestTUIModel_SecondCtrlCForceQuits(t *testing.T) {
	t.Parallel()
	var count atomic.Int32
	cancel := func() { count.Add(1) }

	m := newTestModel([]string{"agent-a"}, cancel)
	updated, _ := m.Update(testCtrlKey('c'))
	m = mustModel(t, updated)
	if !m.cancelling {
		t.Fatal("setup: expected cancelling=true after first Ctrl+C")
	}

	_, cmd := m.Update(testCtrlKey('c'))
	if cmd == nil {
		t.Fatal("expected a command from second Ctrl+C")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg from second Ctrl+C, got %T", msg)
	}
	// Shared CancelFunc still fires at most once thanks to cancelOnce.
	if got := count.Load(); got != 1 {
		t.Errorf("CancelFunc should fire exactly once across both Ctrl+Cs; got %d", got)
	}
}

// TestTUIModel_SecondCtrlCForceQuits_FromDetailMode locks the force-quit
// escape hatch from inside the drill-in view. When cancelling is already
// in flight, the dashboard footer promises "Ctrl+C again: force quit" —
// a user who drilled into a hanging agent's buffer to diagnose the hang
// needs that promise to hold from drill-in too. Without the cancelling-
// before-detailMode precedence in handleKey, Ctrl+C in this state was
// silently swallowed.
func TestTUIModel_SecondCtrlCForceQuits_FromDetailMode(t *testing.T) {
	t.Parallel()
	var count atomic.Int32
	cancel := func() { count.Add(1) }

	m := newTestModel([]string{"agent-a"}, cancel)
	// First Ctrl+C on dashboard initiates cancellation.
	updated, _ := m.Update(testCtrlKey('c'))
	m = mustModel(t, updated)
	if !m.cancelling {
		t.Fatal("setup: expected cancelling=true after first Ctrl+C")
	}
	// Drill in to inspect the hanging agent.
	updated, _ = m.Update(testCtrlKey('o'))
	m = mustModel(t, updated)
	if !m.detailMode {
		t.Fatal("setup: expected detailMode=true after Ctrl+O")
	}

	// Second Ctrl+C while cancelling AND in drill-in must force-quit.
	_, cmd := m.Update(testCtrlKey('c'))
	if cmd == nil {
		t.Fatal("expected a command from second Ctrl+C in detail mode while cancelling")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg from second Ctrl+C in detail mode while cancelling, got %T", msg)
	}
	if got := count.Load(); got != 1 {
		t.Errorf("CancelFunc should fire exactly once; got %d", got)
	}
}

// TestTUIModel_AutoFollow_DetailMode pins that when the viewport is sitting
// at the bottom and a new event arrives, the model snaps back to bottom so
// the user keeps seeing the tail. The viewport's AtBottom() drives this.
func TestTUIModel_AutoFollow_DetailMode(t *testing.T) {
	t.Parallel()
	m := newTestModel([]string{"agent-a"}, func() {})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 8})
	m = mustModel(t, updated)
	updated, _ = m.Update(testCtrlKey('o'))
	m = mustModel(t, updated)

	// Send events that produce content taller than the viewport so a tail
	// exists below the visible window.
	long := strings.Repeat("review finding ", 10)
	for range 6 {
		updated, _ = m.Update(agentEventMsg{agent: "agent-a", ev: reviewtypes.AssistantText{Text: long}})
		m = mustModel(t, updated)
	}
	if !m.detail.AtBottom() {
		t.Errorf("expected viewport to track bottom after each event (auto-follow); YOffset=%d, total=%d",
			m.detail.YOffset(), m.detail.TotalLineCount())
	}
}

// TestTUIModel_AutoFollow_ResizePreservesBottomWhenTailing pins that a user who
// is tailing the detail viewport remains at the bottom after a resize changes
// wrapping and increases the viewport's maximum scroll offset.
func TestTUIModel_AutoFollow_ResizePreservesBottomWhenTailing(t *testing.T) {
	t.Parallel()
	m := newTestModel([]string{"agent-a"}, func() {})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 12})
	m = mustModel(t, updated)
	updated, _ = m.Update(testCtrlKey('o'))
	m = mustModel(t, updated)

	long := strings.Repeat("resize-sensitive review finding ", 8)
	for range 8 {
		updated, _ = m.Update(agentEventMsg{agent: "agent-a", ev: reviewtypes.AssistantText{Text: long}})
		m = mustModel(t, updated)
	}
	m.detail.GotoBottom()
	if !m.detail.AtBottom() {
		t.Fatal("setup: expected viewport to be at bottom before resize")
	}

	updated, _ = m.Update(tea.WindowSizeMsg{Width: 30, Height: 8})
	m = mustModel(t, updated)
	if !m.detail.AtBottom() {
		t.Errorf("expected viewport to stay at bottom after resize; YOffset=%d, total=%d",
			m.detail.YOffset(), m.detail.TotalLineCount())
	}
}

func TestTUIModel_View_DashboardMode(t *testing.T) {
	t.Parallel()
	m := newTestModel([]string{"agent-a", "agent-b"}, func() {})

	v := m.View()
	if v.Content == "" {
		t.Error("View() should not return empty string")
	}
	// Should contain agent names.
	for _, name := range []string{"agent-a", "agent-b"} {
		if !strings.Contains(v.Content, name) {
			t.Errorf("View() missing agent %q", name)
		}
	}
}

func TestTUIModel_DashboardLinesFitTerminalWidth(t *testing.T) {
	t.Parallel()

	for _, width := range []int{1, 2, 10, 20, 24, 30, 40, 53, 54, 55, 60, 80, 120} {
		t.Run(fmt.Sprintf("running width %d", width), func(t *testing.T) {
			t.Parallel()
			m := runningDashboardModel(t, width)
			assertDashboardFitsWidth(t, m)
		})
		t.Run(fmt.Sprintf("finished width %d", width), func(t *testing.T) {
			t.Parallel()
			m := runningDashboardModel(t, width)
			for _, name := range []string{"claude-code-with-a-long-name", "codex"} {
				updated, _ := m.Update(agentEventMsg{agent: name, ev: reviewtypes.Finished{Success: true}})
				m = mustModel(t, updated)
			}
			updated, _ := m.Update(runFinishedMsg{summary: reviewtypes.RunSummary{
				AgentRuns: []reviewtypes.AgentRun{
					{Name: "claude-code-with-a-long-name", Status: reviewtypes.AgentStatusSucceeded},
					{Name: "codex", Status: reviewtypes.AgentStatusSucceeded},
				},
			}})
			m = mustModel(t, updated)
			assertDashboardFitsWidth(t, m)
		})
	}
}

func TestTUIModel_DashboardPreviewStripsControlSequences(t *testing.T) {
	t.Parallel()
	m := newReviewTUIModel([]string{"codex"}, nil)
	m.termWidth = 80

	updated, _ := m.Update(agentEventMsg{
		agent: "codex",
		ev:    reviewtypes.AssistantText{Text: "hello\x1b[?25lworld\x1b[?25h"},
	})
	m = mustModel(t, updated)
	out := m.dashboardView()

	if strings.Contains(out, "\x1b[?25") {
		t.Fatalf("dashboard preview leaked control sequence:\n%q", out)
	}
	if !strings.Contains(out, "helloworld") {
		t.Fatalf("dashboard preview missing stripped text:\n%s", out)
	}
}

func TestTUIModel_WindowResizeKeepsDashboardWithinNewWidth(t *testing.T) {
	t.Parallel()
	m := runningDashboardModel(t, 120)

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 30, Height: 24})
	m = mustModel(t, updated)

	assertDashboardFitsWidth(t, m)
}

func TestTUIModel_DashboardUsesCachedTerminalWidthBeforeResizeMsg(t *testing.T) {
	t.Parallel()
	m := runningDashboardModel(t, 30)

	assertDashboardFitsWidthAt(t, m, 30)
}

func runningDashboardModel(t *testing.T, width int) reviewTUIModel {
	t.Helper()
	m := newReviewTUIModel([]string{"claude-code-with-a-long-name", "codex"}, nil)
	m.termWidth = width

	longPreview := strings.Repeat("review finding with enough text to overflow ", 4)
	for _, name := range []string{"claude-code-with-a-long-name", "codex"} {
		updated, _ := m.Update(agentEventMsg{agent: name, ev: reviewtypes.Started{}})
		m = mustModel(t, updated)
		updated, _ = m.Update(agentEventMsg{agent: name, ev: reviewtypes.Tokens{In: 1_234_567, Out: 987_654}})
		m = mustModel(t, updated)
		updated, _ = m.Update(agentEventMsg{agent: name, ev: reviewtypes.AssistantText{Text: longPreview}})
		m = mustModel(t, updated)
	}
	return m
}

func assertDashboardFitsWidth(t *testing.T, m reviewTUIModel) {
	t.Helper()
	assertDashboardFitsWidthAt(t, m, m.termWidth)
}

func assertDashboardFitsWidthAt(t *testing.T, m reviewTUIModel, width int) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSuffix(m.dashboardView(), "\n"), "\n") {
		if got := ansi.StringWidth(line); got > width {
			t.Fatalf("dashboard line width = %d, want <= %d:\n%s", got, width, line)
		}
	}
}

// TestTUIModel_AutoFollow_PreservesUserScroll pins the contract that new
// agent events must NOT yank the user back to the tail when they have
// scrolled up to inspect older events. Auto-follow only re-engages when
// the viewport is already at the bottom.
func TestTUIModel_AutoFollow_PreservesUserScroll(t *testing.T) {
	t.Parallel()
	m := newReviewTUIModel([]string{"agent-a"}, nil)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	m = mustModel(t, updated)
	updated, _ = m.Update(testCtrlKey('o'))
	m = mustModel(t, updated)

	// Build up enough wrapped content to overflow the viewport several times.
	long := strings.Repeat("line of review text ", 10)
	for range 20 {
		updated, _ = m.Update(agentEventMsg{agent: "agent-a", ev: reviewtypes.AssistantText{Text: long}})
		m = mustModel(t, updated)
	}

	// Scroll up away from the bottom.
	m.detail.GotoTop()
	startOffset := m.detail.YOffset()
	if m.detail.AtBottom() {
		t.Fatal("test setup: viewport unexpectedly at bottom after GotoTop")
	}

	// Send another event — the user is NOT at the bottom, so YOffset must not move.
	updated, _ = m.Update(agentEventMsg{agent: "agent-a", ev: reviewtypes.AssistantText{Text: long}})
	m = mustModel(t, updated)
	if m.detail.YOffset() != startOffset {
		t.Errorf("expected viewport YOffset to stay at %d (user scrolled up); got %d (auto-follow yanked back)",
			startOffset, m.detail.YOffset())
	}

	// Now jump to bottom and send another event — should auto-follow.
	m.detail.GotoBottom()
	updated, _ = m.Update(agentEventMsg{agent: "agent-a", ev: reviewtypes.AssistantText{Text: long}})
	m = mustModel(t, updated)
	if !m.detail.AtBottom() {
		t.Errorf("expected auto-follow to track bottom after event; YOffset=%d, total=%d",
			m.detail.YOffset(), m.detail.TotalLineCount())
	}
}

// TestTUIModel_RunFinishedMsg_SyncsStatusFromSummary pins the contract
// that the final dashboard frame reflects the orchestrator's classified
// status, not the in-stream event status alone. Without this sync, an
// agent the orchestrator classified Cancelled (Ctrl+C, no Finished
// emitted) would still appear "running" in the final frame.
func TestTUIModel_RunFinishedMsg_SyncsStatusFromSummary(t *testing.T) {
	t.Parallel()
	m := newReviewTUIModel([]string{"agent-a", "agent-b"}, nil)

	// Stamp runStart for both via a Started event each.
	for _, name := range []string{"agent-a", "agent-b"} {
		updated, _ := m.Update(agentEventMsg{agent: name, ev: reviewtypes.Started{}})
		m = mustModel(t, updated)
	}

	// Both rows should still be Unknown (no Finished/RunError seen).
	if m.rows[0].status != reviewtypes.AgentStatusUnknown {
		t.Fatalf("expected agent-a Unknown pre-finish, got %v", m.rows[0].status)
	}

	// Send runFinishedMsg with summary that classifies a=Cancelled, b=Failed.
	summary := reviewtypes.RunSummary{
		AgentRuns: []reviewtypes.AgentRun{
			{Name: "agent-a", Status: reviewtypes.AgentStatusCancelled},
			{Name: "agent-b", Status: reviewtypes.AgentStatusFailed},
		},
	}
	updated, _ := m.Update(runFinishedMsg{summary: summary})
	m = mustModel(t, updated)

	if m.rows[0].status != reviewtypes.AgentStatusCancelled {
		t.Errorf("expected agent-a synced to Cancelled, got %v", m.rows[0].status)
	}
	if m.rows[1].status != reviewtypes.AgentStatusFailed {
		t.Errorf("expected agent-b synced to Failed, got %v", m.rows[1].status)
	}
}

func TestTUIModel_RunFinishedMsg_SyncsTokensFromSummary(t *testing.T) {
	t.Parallel()
	m := newReviewTUIModel([]string{"agent-a"}, nil)

	updated, _ := m.Update(runFinishedMsg{summary: reviewtypes.RunSummary{
		AgentRuns: []reviewtypes.AgentRun{
			{
				Name:   "agent-a",
				Status: reviewtypes.AgentStatusSucceeded,
				Tokens: reviewtypes.Tokens{In: 1200, Out: 345},
			},
		},
	}})
	m = mustModel(t, updated)

	if got := m.rows[0].tokens; got.In != 1200 || got.Out != 345 {
		t.Fatalf("tokens = {%d %d}, want {1200 345}", got.In, got.Out)
	}
}

func TestTUIModel_RunFinishedMsg_SyncsErrorFromSummary(t *testing.T) {
	t.Parallel()
	m := newReviewTUIModel([]string{"codex"}, nil)
	m.termWidth = 200

	updated, _ := m.Update(runFinishedMsg{summary: reviewtypes.RunSummary{
		AgentRuns: []reviewtypes.AgentRun{
			{
				Name:   "codex",
				Status: reviewtypes.AgentStatusFailed,
				Err:    errors.New("binary not found"),
			},
		},
	}})
	m = mustModel(t, updated)

	out := m.dashboardView()
	if !strings.Contains(out, "binary not found") {
		t.Fatalf("expected summary error in dashboard preview, got:\n%s", out)
	}
}

// TestTUIModel_RunErrorSticky_FinishedDoesNotFlipToSucceeded pins the
// CU3-fix-loop contract that RunError implies Failed and is sticky against
// a subsequent Finished{Success: true}. Mirrors classifyStatus from CU4
// which honors sawRunError → Failed.
func TestTUIModel_RunErrorSticky_FinishedDoesNotFlipToSucceeded(t *testing.T) {
	t.Parallel()
	m := newReviewTUIModel([]string{"agent-a"}, nil)

	// RunError → Finished{Success: true}. Status should stay Failed.
	updated, _ := m.Update(agentEventMsg{agent: "agent-a", ev: reviewtypes.RunError{Err: errors.New("torn")}})
	m = mustModel(t, updated)
	if m.rows[0].status != reviewtypes.AgentStatusFailed {
		t.Fatalf("expected Failed after RunError, got %v", m.rows[0].status)
	}
	updated, _ = m.Update(agentEventMsg{agent: "agent-a", ev: reviewtypes.Finished{Success: true}})
	m = mustModel(t, updated)
	if m.rows[0].status != reviewtypes.AgentStatusFailed {
		t.Errorf("expected Failed to stick (RunError is sticky), got %v", m.rows[0].status)
	}
}

// TestTUIModel_DelegatesNonWheelMouseEventsToViewport pins that the Update
// case-arm routing mouse events to the viewport covers Click, Release, and
// Motion in addition to Wheel. The viewport's selection support (click-drag
// highlight in terminals that emit cell-motion events) needs the full event
// stream, not just scroll. A future refactor that narrowed the case-arm to
// wheel-only would silently break selection while still passing
// TestTUIModel_DelegatesMouseWheelToViewport.
//
// Coverage limitation: the viewport's internal selection state is not
// exposed via a public getter, so this test asserts the weaker invariant
// that the events do not panic and that detailMode survives. That catches
// the regression where the case-arm is removed entirely (and the events
// fall through Update's catch-all to return m, nil); it does not catch
// finer-grained changes to selection semantics inside the viewport.
func TestTUIModel_DelegatesNonWheelMouseEventsToViewport(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		msg  tea.Msg
	}{
		{"click", tea.MouseClickMsg{}},
		{"release", tea.MouseReleaseMsg{}},
		{"motion", tea.MouseMotionMsg{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newTestModel([]string{"agent-a"}, func() {})
			updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
			m = mustModel(t, updated)
			updated, _ = m.Update(testCtrlKey('o'))
			m = mustModel(t, updated)
			if !m.detailMode {
				t.Fatalf("setup: expected detailMode=true after Ctrl+O")
			}

			updated, _ = m.Update(tc.msg)
			m = mustModel(t, updated)
			if !m.detailMode {
				t.Errorf("detailMode should remain true after mouse %s event", tc.name)
			}
		})
	}
}

// TestTUIModel_CancellingIndicatorOnlyAffectsRunningRows pins that the
// "cancelling" status indicator in renderRow only replaces "running" — rows
// already in a terminal status keep their own indicator. Without this gate
// the post-Ctrl+C frame would briefly show every row as "cancelling" before
// the dashboard transitioned to its post-finish state, including agents
// that had already succeeded.
func TestTUIModel_CancellingIndicatorOnlyAffectsRunningRows(t *testing.T) {
	t.Parallel()
	m := newTestModel([]string{"agent-a", "agent-b"}, func() {})
	m.termWidth = 120
	// agent-a has already succeeded; agent-b is still running.
	updated, _ := m.Update(agentEventMsg{agent: "agent-a", ev: reviewtypes.Started{}})
	m = mustModel(t, updated)
	updated, _ = m.Update(agentEventMsg{agent: "agent-a", ev: reviewtypes.Finished{Success: true}})
	m = mustModel(t, updated)
	updated, _ = m.Update(agentEventMsg{agent: "agent-b", ev: reviewtypes.Started{}})
	m = mustModel(t, updated)

	// Flip cancelling without going through Ctrl+C so the
	// allAgentsTerminal() short-circuit doesn't fire.
	m.cancelling = true

	gotA := m.renderRow(m.rows[0])
	if !strings.Contains(gotA, "done") {
		t.Errorf("succeeded row should retain ✓ done indicator while cancelling; got %q", gotA)
	}
	if strings.Contains(gotA, "cancelling") {
		t.Errorf("succeeded row must not show 'cancelling' indicator; got %q", gotA)
	}

	gotB := m.renderRow(m.rows[1])
	if !strings.Contains(gotB, "cancelling") {
		t.Errorf("running row should show cancelling indicator; got %q", gotB)
	}
}

// TestTUIModel_CtrlCAfterAllRowsTerminalQuitsImmediately pins that Ctrl+C
// during the race window between an agent's terminal event (Finished or
// RunError) and the orchestrator's runFinishedMsg short-circuits to tea.Quit
// instead of flashing the "Cancelling agents..." indicator. The pre-fix
// behavior fired CancelFunc and set m.cancelling=true even though every
// agent had already finished — confusing the user for the brief window
// before the dashboard transitioned to its post-finish state.
func TestTUIModel_CtrlCAfterAllRowsTerminalQuitsImmediately(t *testing.T) {
	t.Parallel()
	var called atomic.Bool
	cancel := func() { called.Store(true) }
	m := newTestModel([]string{"agent-a", "agent-b"}, cancel)

	// Drive both rows to a terminal status via events. runFinishedMsg is NOT
	// sent — that's the race window we're testing.
	updated, _ := m.Update(agentEventMsg{agent: "agent-a", ev: reviewtypes.Finished{Success: true}})
	m = mustModel(t, updated)
	updated, _ = m.Update(agentEventMsg{agent: "agent-b", ev: reviewtypes.Finished{Success: true}})
	m = mustModel(t, updated)
	if m.finished {
		t.Fatal("setup: m.finished should be false (runFinishedMsg not sent)")
	}

	updated, cmd := m.Update(testCtrlKey('c'))
	m = mustModel(t, updated)
	if m.cancelling {
		t.Error("Ctrl+C with all rows already terminal must NOT set cancelling=true")
	}
	if called.Load() {
		t.Error("CancelFunc must not fire when there's nothing left to cancel")
	}
	if cmd == nil {
		t.Fatal("expected a quit command when Ctrl+C arrives with all rows terminal")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg, got %T", cmd())
	}
}

// TestTUIModel_CtrlCInDetailModeAfterAllRowsTerminalQuits pins that Ctrl+C
// pressed inside drill-in during the narrow race window where every agent
// has emitted a terminal event but the orchestrator's runFinishedMsg has
// not arrived yet quits the TUI instead of being swallowed. Without the
// detail-mode gate on allAgentsTerminal(), the user reading a completed
// agent's buffer would have to press Esc first to return to the dashboard
// before Ctrl+C took effect — needlessly two-step when there is nothing
// left to cancel. Companion to
// [TestTUIModel_CtrlCAfterAllRowsTerminalQuitsImmediately] for the
// dashboard-mode case.
func TestTUIModel_CtrlCInDetailModeAfterAllRowsTerminalQuits(t *testing.T) {
	t.Parallel()
	var called atomic.Bool
	cancel := func() { called.Store(true) }
	m := newTestModel([]string{"agent-a", "agent-b"}, cancel)

	updated, _ := m.Update(agentEventMsg{agent: "agent-a", ev: reviewtypes.Finished{Success: true}})
	m = mustModel(t, updated)
	updated, _ = m.Update(agentEventMsg{agent: "agent-b", ev: reviewtypes.Finished{Success: true}})
	m = mustModel(t, updated)
	updated, _ = m.Update(testCtrlKey('o'))
	m = mustModel(t, updated)
	if !m.detailMode {
		t.Fatal("setup: expected detailMode=true after Ctrl+O")
	}
	if m.finished {
		t.Fatal("setup: m.finished should be false (runFinishedMsg not sent)")
	}

	updated, cmd := m.Update(testCtrlKey('c'))
	m = mustModel(t, updated)
	if m.cancelling {
		t.Error("Ctrl+C in detail mode with all rows terminal must NOT set cancelling=true")
	}
	if called.Load() {
		t.Error("CancelFunc must not fire when there is nothing left to cancel")
	}
	if cmd == nil {
		t.Fatal("expected a quit command when Ctrl+C arrives in detail mode with all rows terminal")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg, got %T", cmd())
	}
}

// TestTUIModel_PostFinishInDetailMode_ExitKeysQuit pins that q/Enter/Ctrl+C
// pressed while drilled in AFTER the run finished dismiss the TUI directly
// instead of being swallowed by the viewport (which has no quit binding).
// Pre-fix the user had to Esc out of detail mode first, then press an exit
// key — a two-step dismissal with no on-screen hint that q/Enter were inert.
func TestTUIModel_PostFinishInDetailMode_ExitKeysQuit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		key  tea.KeyPressMsg
	}{
		{"q", testKey('q')},
		{"Enter", testKey(tea.KeyEnter)},
		{"Ctrl+C", testCtrlKey('c')},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newTestModel([]string{"agent-a"}, func() {})
			updated, _ := m.Update(runFinishedMsg{summary: reviewtypes.RunSummary{}})
			m = mustModel(t, updated)
			updated, _ = m.Update(testCtrlKey('o'))
			m = mustModel(t, updated)
			if !m.detailMode {
				t.Fatalf("setup: expected detailMode=true after Ctrl+O")
			}

			_, cmd := m.Update(tc.key)
			if cmd == nil {
				t.Fatalf("expected a command for key %q post-finish in detail mode", tc.name)
			}
			if _, ok := cmd().(tea.QuitMsg); !ok {
				t.Errorf("expected tea.QuitMsg for key %q post-finish in detail mode, got %T", tc.name, cmd())
			}
		})
	}
}

// TestTUIModel_PostFinishInDetailMode_EscReturnsToDashboard pins that Esc
// preserves its "back to dashboard" meaning even after the run finishes,
// rather than quitting outright. The user can then dismiss from the
// dashboard via q/Esc/Enter/Ctrl+C. Regression guard for the post-finish
// detail-mode dismissal change.
func TestTUIModel_PostFinishInDetailMode_EscReturnsToDashboard(t *testing.T) {
	t.Parallel()
	m := newTestModel([]string{"agent-a"}, func() {})
	updated, _ := m.Update(runFinishedMsg{summary: reviewtypes.RunSummary{}})
	m = mustModel(t, updated)
	updated, _ = m.Update(testCtrlKey('o'))
	m = mustModel(t, updated)
	if !m.detailMode {
		t.Fatalf("setup: expected detailMode=true after Ctrl+O")
	}

	updated, cmd := m.Update(testKey(tea.KeyEscape))
	m = mustModel(t, updated)
	if m.detailMode {
		t.Error("Esc post-finish in detail mode must return to dashboard, not quit")
	}
	if cmd != nil {
		if msg := cmd(); msg != nil {
			if _, ok := msg.(tea.QuitMsg); ok {
				t.Error("Esc post-finish in detail mode must NOT quit; it returns to dashboard")
			}
		}
	}
}
