package investigate

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// TestTUIProgressSink_CtxCancelUnblocksWait pins the early-return contract:
// if the loop returns before RunFinished is called (validation error, early
// programmer-bug return, or a context cancellation that races RunFinished),
// the ctx watcher must push tea.Quit so Wait() unblocks. Without the
// watcher, Wait() would block forever — defers in executeLoopAndCapture
// run Wait BEFORE cancelTUI on the return path.
func TestTUIProgressSink_CtxCancelUnblocksWait(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	sink := newTUIProgressSink(
		"test prompt", "abcdef012345",
		[]string{"agent-a", "agent-b"}, 2, 2,
		cancel, &buf,
	)
	sink.Start(ctx)

	// Cancel before RunFinished — simulates an early loop return.
	cancel()

	done := make(chan struct{})
	go func() {
		sink.Wait()
		close(done)
	}()
	select {
	case <-done:
		// OK: Wait() returned because the ctx watcher pushed tea.Quit.
	case <-time.After(5 * time.Second):
		t.Fatal("Wait() did not return within 5 seconds after ctx cancel")
	}
}

// TestTUIProgressSink_NilCtxStillWorks pins that passing a nil ctx to
// Start does not panic and does not skip program lifecycle. RunFinished
// remains the dismissal path in this mode.
func TestTUIProgressSink_NilCtxStillWorks(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	sink := newTUIProgressSink(
		"test prompt", "abcdef012345",
		[]string{"agent-a"}, 1, 1,
		func() {}, &buf,
	)
	// Should not panic.
	//nolint:staticcheck // intentionally exercises the nil-ctx branch
	sink.Start(nil)

	// Drive the program to completion via RunFinished, then ensure Wait
	// returns. RunFinished calls Wait internally; back it with a timeout.
	done := make(chan struct{})
	go func() {
		sink.RunFinished(OutcomeQuorum)
		close(done)
	}()
	// RunFinished blocks until any key dismisses the post-run TUI; mimic
	// the keypress dismissal loop used elsewhere.
	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-tick.C:
			sink.program.Quit() // skip the keypress dance; Quit is idempotent
		case <-deadline:
			t.Fatal("RunFinished did not return within 10 seconds")
		}
	}
}
