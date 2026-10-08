package cli

import (
	"bytes"
	"testing"
)

// TestStartSpinner_NonTTYFallback locks in startSpinner's non-terminal
// contract now that it delegates to startUpdatableSpinner: no animation, and
// stop(true)/stop(false) behave exactly as they did before the refactor.
func TestStartSpinner_NonTTYFallback(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		success bool
		want    string
	}{
		{name: "success prints completion line", success: true, want: "✓ doing work\n"},
		{name: "failure prints nothing", success: false, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			stop := startSpinner(&buf, "doing work")
			stop(tt.success)
			if got := buf.String(); got != tt.want {
				t.Errorf("stop(%v) = %q, want %q", tt.success, got, tt.want)
			}
		})
	}
}

// TestStartUpdatableSpinner_NonTTYUpdateBeforeAnyDraw proves update is safe to
// call before anything has been drawn (a non-terminal writer never draws an
// in-flight frame at all, so every call here is "before the first draw") and
// that stop renders whichever message was set last.
func TestStartUpdatableSpinner_NonTTYUpdateBeforeAnyDraw(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	update, stop := startUpdatableSpinner(&buf, "starting")
	update("session 1/2 · turn 1/2")
	update("session 2/2 · turn 2/2")
	stop(true)
	if got, want := buf.String(), "✓ session 2/2 · turn 2/2\n"; got != want {
		t.Errorf("stop(true) after updates = %q, want %q", got, want)
	}
}

// TestStartUpdatableSpinner_NonTTYStopFalseIgnoresUpdates proves a failed run
// leaves no trace, regardless of how many updates preceded it.
func TestStartUpdatableSpinner_NonTTYStopFalseIgnoresUpdates(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	update, stop := startUpdatableSpinner(&buf, "starting")
	update("mid-flight")
	stop(false)
	if got := buf.String(); got != "" {
		t.Errorf("stop(false) = %q, want empty (no dangling output)", got)
	}
}

// TestStartUpdatableSpinner_NonTTYStopDoesNotPanicOnRepeatCalls documents the
// non-terminal stop closure's existing idempotency: it never closes a
// channel (that only happens on the terminal path), so calling it again is
// safe — it just re-prints the completion line. This matches startSpinner's
// pre-existing non-TTY behavior; the terminal path remains single-call only.
func TestStartUpdatableSpinner_NonTTYStopDoesNotPanicOnRepeatCalls(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	_, stop := startUpdatableSpinner(&buf, "starting")
	stop(true)
	stop(true)
	if got, want := buf.String(), "✓ starting\n✓ starting\n"; got != want {
		t.Errorf("double stop(true) = %q, want %q", got, want)
	}
}
