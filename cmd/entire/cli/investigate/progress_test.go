package investigate

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

// TestTextProgressSink_TurnLines verifies that textProgressSink writes the
// two-line shape today's headless run produces:
//
//	Turn N · <agent>
//	  Stance: <stance>
func TestTextProgressSink_TurnLines(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	sink := newTextProgressSink(&buf)

	sink.TurnStarted("claude-code", 1, 1, 3)
	sink.TurnFinished("claude-code", 1, stanceApprove, 12*time.Second, false, nil, "")
	sink.TurnStarted("codex", 2, 1, 3)
	sink.TurnFinished("codex", 2, stanceRequestChanges, 8*time.Second, false, nil, "")
	sink.RunFinished(OutcomeQuorum)

	want := "Turn 1 · claude-code\n  Stance: approve\nTurn 2 · codex\n  Stance: request-changes\n"
	if got := buf.String(); got != want {
		t.Errorf("textProgressSink output mismatch\n got: %q\nwant: %q", got, want)
	}
}

// TestTextProgressSink_NilWriter verifies a nil writer is a silent no-op
// rather than a panic. Cheap defensive cover for an embedded sink.
func TestTextProgressSink_NilWriter(t *testing.T) {
	t.Parallel()

	sink := newTextProgressSink(nil)
	// Each method should be safe; no panic.
	sink.TurnStarted("a", 1, 1, 1)
	sink.TurnFinished("a", 1, stanceApprove, time.Second, false, nil, "")
	sink.RunFinished(OutcomeQuorum)
}

// Compile-time guard: nullProgressSink must satisfy ProgressSink so the
// loop can use it as the default when LoopDeps.Progress is nil.
var _ ProgressSink = nullProgressSink{}

// TestNullProgressSink_NoPanic verifies the default sink's methods are
// safe to call. A panic here would break any loop run that doesn't wire a
// progress sink.
func TestNullProgressSink_NoPanic(t *testing.T) {
	t.Parallel()

	s := nullProgressSink{}
	s.TurnStarted("a", 1, 1, 1)
	s.TurnFinished("a", 1, stanceReject, 0, true, errors.New("x"), "")
	s.RunFinished(OutcomeStalled)
}
