package review

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

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

func TestTUIModel_KeyCtrlC_NotDetailMode_CancelsAndQuits(t *testing.T) {
	t.Parallel()
	var called atomic.Bool
	cancel := func() { called.Store(true) }

	m := newTestModel([]string{"agent-a"}, cancel)
	_, cmd := m.Update(testCtrlKey('c'))
	if !called.Load() {
		t.Error("expected cancel to be called on Ctrl+C outside detail mode")
	}
	if cmd == nil {
		t.Error("expected a quit command to be returned")
	}
	// Verify it's the quit command by running it.
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg from Ctrl+C cmd, got %T", msg)
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
	if m2.View().AltScreen {
		t.Error("expected View().AltScreen=false outside detail mode")
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

func TestTUIModel_UpDown_Scroll(t *testing.T) {
	t.Parallel()
	m := newTestModel([]string{"agent-a"}, func() {})
	m.detailMode = true
	// Populate buffer so max scroll > 0.
	for range 5 {
		m.rows[0].buffer = append(m.rows[0].buffer, reviewtypes.Started{})
	}
	m.detailScroll = 4 // at max

	// Down when at max: clamp.
	updated, _ := m.Update(testKey(tea.KeyDown))
	m = mustModel(t, updated)
	if m.detailScroll != 4 {
		t.Errorf("scroll should stay at max on Down; got %d", m.detailScroll)
	}

	// Up: 4 → 3.
	updated, _ = m.Update(testKey(tea.KeyUp))
	m = mustModel(t, updated)
	if m.detailScroll != 3 {
		t.Errorf("expected scroll=3 after Up; got %d", m.detailScroll)
	}

	// Down again: 3 → 4.
	updated, _ = m.Update(testKey(tea.KeyDown))
	m = mustModel(t, updated)
	if m.detailScroll != 4 {
		t.Errorf("expected scroll=4 after Down; got %d", m.detailScroll)
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

func TestTUIModel_RunFinishedMsg_AnyKeyQuits(t *testing.T) {
	t.Parallel()
	m := newTestModel([]string{"agent-a"}, func() {})

	updated, _ := m.Update(runFinishedMsg{summary: reviewtypes.RunSummary{}})
	m2 := mustModel(t, updated)
	if !m2.finished {
		t.Error("model should be finished after runFinishedMsg")
	}

	// Any key should now quit.
	_, cmd := m2.Update(testKey(tea.KeyEnter))
	if cmd == nil {
		t.Error("expected quit command after finished + any key")
	}
	if msg := cmd(); msg == nil {
		t.Error("expected non-nil quit msg")
	}
}

func TestTUIModel_AutoFollow_DetailMode(t *testing.T) {
	t.Parallel()
	m := newTestModel([]string{"agent-a"}, func() {})
	m.detailMode = true
	m.detailIdx = 0
	m.detailScroll = 0

	// Send 5 events; model should auto-scroll to bottom each time.
	current := m
	for i := range 5 {
		updated, _ := current.Update(agentEventMsg{agent: "agent-a", ev: reviewtypes.Started{}})
		current = mustModel(t, updated)
		wantScroll := i // buffer has i+1 events; max scroll is i
		if current.detailScroll != wantScroll {
			t.Errorf("event %d: want detailScroll=%d, got %d", i, wantScroll, current.detailScroll)
		}
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

// TestTUIModel_AutoFollow_PreservesUserScroll pins the contract that new
// agent events should NOT yank the user back to the tail when they have
// scrolled up to inspect older events. Auto-follow only re-engages when
// the user is already at the bottom.
func TestTUIModel_AutoFollow_PreservesUserScroll(t *testing.T) {
	t.Parallel()
	m := newReviewTUIModel([]string{"agent-a"}, nil)
	m.termHeight = 10
	m.detailMode = true
	m.detailIdx = 0

	// Build up a buffer of 20 events.
	for range 20 {
		updated, _ := m.Update(agentEventMsg{agent: "agent-a", ev: reviewtypes.AssistantText{Text: "line"}})
		m = mustModel(t, updated)
	}

	// Snap to bottom, then scroll up by 5.
	m.detailScroll = m.maxDetailScroll() - 5
	scrollBeforeNewEvent := m.detailScroll

	// Send another event. The user is NOT at the bottom — scroll should not move.
	updated, _ := m.Update(agentEventMsg{agent: "agent-a", ev: reviewtypes.AssistantText{Text: "new line"}})
	m = mustModel(t, updated)
	if m.detailScroll != scrollBeforeNewEvent {
		t.Errorf("expected detailScroll to stay at %d (user scrolled up), got %d (auto-follow yanked back)",
			scrollBeforeNewEvent, m.detailScroll)
	}

	// Now scroll to bottom and send another event — should auto-follow.
	m.detailScroll = m.maxDetailScroll()
	updated, _ = m.Update(agentEventMsg{agent: "agent-a", ev: reviewtypes.AssistantText{Text: "another"}})
	m = mustModel(t, updated)
	if m.detailScroll != m.maxDetailScroll() {
		t.Errorf("expected auto-follow to track bottom (got detailScroll=%d, max=%d)",
			m.detailScroll, m.maxDetailScroll())
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
