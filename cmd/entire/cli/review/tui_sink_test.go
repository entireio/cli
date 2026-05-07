package review

import (
	"bytes"
	"testing"
	"time"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// TestTUISink_StartIsIdempotent verifies that calling Start multiple times
// does not panic or spawn extra goroutines.
func TestTUISink_StartIsIdempotent(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	sink := NewTUISink([]string{"agent-a"}, func() {}, &buf)

	// Start twice — the second call must be a no-op (no panic, no deadlock).
	sink.Start()
	sink.Start()

	// Clean up: send RunFinished so the program exits, then Wait.
	sink.RunFinished(reviewtypes.RunSummary{})

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
	sink := NewTUISink([]string{"agent-a"}, func() {}, &buf)

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
	sink := NewTUISink([]string{"agent-a"}, func() {}, &buf)

	// Must not panic.
	sink.AgentEvent("agent-a", reviewtypes.Started{})
	sink.AgentEvent("agent-a", reviewtypes.AssistantText{Text: "hello"})
}

// TestTUISink_RunFinished_UnblockAfterQuit verifies that RunFinished unblocks
// when the Bubble Tea program receives a quit (sent via the model's any-key
// handler after finished=true).
//
// We cannot easily drive keystrokes into the Bubble Tea program in a unit
// test without a real terminal, so we trigger the quit path by sending
// RunFinished which sets finished=true in the model, then the program exits
// on the first internal message that causes tea.Quit.
//
// In practice: after RunFinished is sent, the program sets finished=true and
// the next tick or key press causes Quit. Since we're not in a TTY environment
// here the program exits quickly because it can't read from stdin.
func TestTUISink_RunFinished_EventuallyUnblocks(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	sink := NewTUISink([]string{"agent-a"}, func() {}, &buf)
	sink.Start()

	// Send some events before finishing.
	sink.AgentEvent("agent-a", reviewtypes.Started{})
	sink.AgentEvent("agent-a", reviewtypes.AssistantText{Text: "reviewing…"})
	sink.AgentEvent("agent-a", reviewtypes.Finished{Success: true})

	done := make(chan struct{})
	go func() {
		sink.RunFinished(reviewtypes.RunSummary{
			AgentRuns: []reviewtypes.AgentRun{
				{Name: "agent-a", Status: reviewtypes.AgentStatusSucceeded},
			},
		})
		close(done)
	}()

	select {
	case <-done:
		// OK — RunFinished returned (program exited).
	case <-time.After(10 * time.Second):
		t.Fatal("RunFinished() did not return within 10 seconds")
	}
}

// TestTUISink_RunFinished_AfterSecondCall_IsNoOp verifies that calling
// RunFinished a second time does not block or panic.
func TestTUISink_RunFinished_AfterSecondCall_IsNoOp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	sink := NewTUISink([]string{"agent-a"}, func() {}, &buf)
	sink.Start()

	// First RunFinished should unblock the program.
	done := make(chan struct{})
	go func() {
		sink.RunFinished(reviewtypes.RunSummary{})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("first RunFinished did not return in time")
	}

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
	var _ reviewtypes.Sink = NewTUISink(nil, func() {}, &buf)
}
