package review

import (
	"bytes"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

func finishAndDismissTUI(t *testing.T, sink *TUISink, summary reviewtypes.RunSummary) {
	t.Helper()

	done := make(chan struct{})
	go func() {
		sink.RunFinished(summary)
		close(done)
	}()

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	timeout := time.After(10 * time.Second)
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			// 'q' is an explicit post-finish exit key. (Any-key-quits was
			// removed so the user can still Ctrl+O into completed output.)
			sink.program.Send(tea.KeyPressMsg(tea.Key{Code: 'q', Text: "q"}))
		case <-timeout:
			t.Fatal("RunFinished() did not return within 10 seconds")
		}
	}
}

// TestTUISink_StartIsIdempotent verifies that calling Start multiple times
// does not panic or spawn extra goroutines.
func TestTUISink_StartIsIdempotent(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	sink := NewTUISink([]string{"agent-a"}, func() {}, &buf, bytes.NewReader(nil))

	// Start twice — the second call must be a no-op (no panic, no deadlock).
	sink.Start()
	sink.Start()

	// Clean up: send RunFinished so the program exits, then Wait.
	finishAndDismissTUI(t, sink, reviewtypes.RunSummary{})

	// Wait with a timeout to avoid hanging the test suite on failure.
	done := make(chan struct{})
	go func() {
		sink.Wait()
		close(done)
	}()

	select {
	case <-done:
		// OK
	case <-time.After(5 * time.Second):
		t.Fatal("Wait() did not return within 5 seconds after RunFinished")
	}
}

// TestTUISink_WaitBeforeStart_IsNoOp verifies that calling Wait before Start
// returns immediately without blocking.
func TestTUISink_WaitBeforeStart_IsNoOp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	sink := NewTUISink([]string{"agent-a"}, func() {}, &buf, bytes.NewReader(nil))

	done := make(chan struct{})
	go func() {
		sink.Wait() // should return immediately
		close(done)
	}()

	select {
	case <-done:
		// OK
	case <-time.After(time.Second):
		t.Fatal("Wait() before Start() did not return immediately")
	}
}

// TestTUISink_AgentEvent_BeforeStart_IsNoOp verifies that AgentEvent before
// Start does not panic.
func TestTUISink_AgentEvent_BeforeStart_IsNoOp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	sink := NewTUISink([]string{"agent-a"}, func() {}, &buf, bytes.NewReader(nil))

	// Must not panic.
	sink.AgentEvent("agent-a", reviewtypes.Started{})
	sink.AgentEvent("agent-a", reviewtypes.AssistantText{Text: "hello"})
}

// TestTUISink_RunFinished_EventuallyUnblocks verifies that RunFinished unblocks
// once the finished TUI receives an explicit exit key (q) like a user would press.
func TestTUISink_RunFinished_EventuallyUnblocks(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	sink := NewTUISink([]string{"agent-a"}, func() {}, &buf, bytes.NewReader(nil))
	sink.Start()

	// Send some events before finishing.
	sink.AgentEvent("agent-a", reviewtypes.Started{})
	sink.AgentEvent("agent-a", reviewtypes.AssistantText{Text: "reviewing…"})
	sink.AgentEvent("agent-a", reviewtypes.Finished{Success: true})

	finishAndDismissTUI(t, sink, reviewtypes.RunSummary{
		AgentRuns: []reviewtypes.AgentRun{
			{Name: "agent-a", Status: reviewtypes.AgentStatusSucceeded},
		},
	})
}

// TestTUISink_RunFinished_AfterSecondCall_IsNoOp verifies that calling
// RunFinished a second time does not block or panic.
func TestTUISink_RunFinished_AfterSecondCall_IsNoOp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	sink := NewTUISink([]string{"agent-a"}, func() {}, &buf, bytes.NewReader(nil))
	sink.Start()

	// First RunFinished should unblock the program.
	finishAndDismissTUI(t, sink, reviewtypes.RunSummary{})

	// Second call should return immediately (no-op after finished=true).
	secondDone := make(chan struct{})
	go func() {
		sink.RunFinished(reviewtypes.RunSummary{})
		close(secondDone)
	}()

	select {
	case <-secondDone:
		// OK
	case <-time.After(time.Second):
		t.Fatal("second RunFinished call blocked unexpectedly")
	}
}

// TestTUISink_ImplementsSink verifies the compile-time interface constraint
// is reflected at test time too.
func TestTUISink_ImplementsSink(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var _ reviewtypes.Sink = NewTUISink(nil, func() {}, &buf, bytes.NewReader(nil))
}

// fakeFDWriter implements fdWriter with a controllable Fd, letting us drive
// terminalMeasurer through both branches (non-fdWriter → nil; fdWriter → a
// measurer that returns (0,0,false) for a non-terminal fd).
type fakeFDWriter struct {
	fd uintptr
}

func (f *fakeFDWriter) Write(p []byte) (int, error) { return len(p), nil }
func (f *fakeFDWriter) Fd() uintptr                 { return f.fd }

// TestTerminalMeasurer_NonFDWriter verifies that a writer without an Fd()
// method yields a nil measurer, which is the signal NewTUISink uses to skip
// the early measurement and rely on the first tea.WindowSizeMsg.
func TestTerminalMeasurer_NonFDWriter(t *testing.T) {
	t.Parallel()
	if got := terminalMeasurer(&bytes.Buffer{}); got != nil {
		t.Errorf("terminalMeasurer for non-fdWriter = non-nil, want nil")
	}
}

// TestTerminalMeasurer_FDWriter_InvalidFD verifies the happy-path shape:
// when the output is an fdWriter, terminalMeasurer returns a non-nil
// function. Calling it with a non-terminal fd surfaces ok=false (the
// fallback contract that NewTUISink relies on to not over-set termWidth).
func TestTerminalMeasurer_FDWriter_InvalidFD(t *testing.T) {
	t.Parallel()
	// fd=999999 is almost certainly not a real open descriptor on the test
	// process, so term.GetSize returns an error → measurer reports ok=false.
	measurer := terminalMeasurer(&fakeFDWriter{fd: 999999})
	if measurer == nil {
		t.Fatal("terminalMeasurer for fdWriter returned nil")
	}
	width, height, ok := measurer()
	if ok {
		t.Errorf("invalid fd should yield ok=false, got width=%d height=%d", width, height)
	}
	if width != 0 || height != 0 {
		t.Errorf("invalid fd should yield zero dims, got width=%d height=%d", width, height)
	}
}
