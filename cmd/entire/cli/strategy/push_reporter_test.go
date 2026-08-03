package strategy

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a mutex-guarded io.Writer for tests that read a buffer
// concurrently with a goroutine writing to it (e.g. pushReporter's reveal
// goroutine). Reading a plain bytes.Buffer while such a goroutine runs is a
// data race; syncBuffer serializes access so polling is race-safe.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// waitForContains polls buf until it contains substr, rather than sleeping a
// fixed duration: under saturated -race CI load the reporter's background
// goroutine may not be scheduled within any fixed window. buf.String() is
// mutex-guarded, so polling concurrently with the goroutine's writes is
// race-safe.
func waitForContains(t *testing.T, buf *syncBuffer, substr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		out := buf.String()
		if strings.Contains(out, substr) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q, got %q", substr, out)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPushShouldReveal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		elapsed, thresh time.Duration
		styled, want    bool
	}{
		{"unstyled never reveals", 10 * time.Second, time.Second, false, false},
		{"styled below threshold stays hidden", 500 * time.Millisecond, time.Second, true, false},
		{"styled at threshold reveals", time.Second, time.Second, true, true},
		{"styled past threshold reveals", 3 * time.Second, time.Second, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := pushShouldReveal(tc.elapsed, tc.thresh, tc.styled); got != tc.want {
				t.Fatalf("pushShouldReveal(%v,%v,%v)=%v want %v", tc.elapsed, tc.thresh, tc.styled, got, tc.want)
			}
		})
	}
}

func TestPushReporter_NotStyled_WritesNothing(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	r := newPushReporter(context.Background(), &buf, false, time.Millisecond)
	r.phase("syncing 3 checkpoints")
	r.setDetail("writing 1/2 objects")
	time.Sleep(10 * time.Millisecond)
	r.finish("pushed 3 checkpoints")
	if buf.Len() != 0 {
		t.Fatalf("unstyled reporter wrote %q, want nothing", buf.String())
	}
}

// TestPushReporter_Styled_RevealsLiveDetailThenPersistentSummary exercises the
// full reveal -> live setDetail -> finish flow: the prefix appears once
// revealed, setDetail updates the in-place line with live transfer detail
// while the push is still "running", and finish prints a PERSISTENT summary
// line (trailing newline, no ellipsis) rather than clearing the line to
// nothing.
func TestPushReporter_Styled_RevealsLiveDetailThenPersistentSummary(t *testing.T) {
	t.Parallel()
	buf := &syncBuffer{}
	r := newPushReporter(context.Background(), buf, true, time.Millisecond)
	r.phase("syncing 3 checkpoints")
	waitForContains(t, buf, "syncing 3 checkpoints")

	r.setDetail("writing 1/2 objects")
	waitForContains(t, buf, "writing 1/2 objects")

	r.finish("pushed 3 checkpoints")
	out := buf.String()
	if !strings.Contains(out, "pushed 3 checkpoints") {
		t.Fatalf("expected persistent summary text, got %q", out)
	}
	if strings.Contains(out, "pushed 3 checkpoints…") {
		t.Fatalf("final summary must not carry the in-progress ellipsis, got %q", out)
	}
	if !strings.HasSuffix(out, "\033[K\n") {
		t.Fatalf("expected persistent final line to end with a newline, got %q", out)
	}
}

// TestPushReporter_Styled_EmptySummaryClearsWithoutGarbage covers the
// aborted/failed-push path (e.g. non-interactive SSH auth failure) where the
// caller calls finish("") and then prints its own error. A revealed line must
// be cleared, NOT replaced by a content-less "[entire]  (Ns)" persistent line.
func TestPushReporter_Styled_EmptySummaryClearsWithoutGarbage(t *testing.T) {
	t.Parallel()
	buf := &syncBuffer{}
	r := newPushReporter(context.Background(), buf, true, time.Millisecond)
	r.phase("syncing 3 checkpoints")
	waitForContains(t, buf, "syncing 3 checkpoints")

	r.finish("")
	out := buf.String()
	if strings.Contains(out, "[entire]  (") {
		t.Fatalf("empty-summary finish emitted a content-less persistent line: %q", out)
	}
	if !strings.HasSuffix(out, "\r\033[K") {
		t.Fatalf("expected empty-summary finish to clear the line, got %q", out)
	}
}

func TestPushReporter_Styled_FastPush_StaysHidden(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	r := newPushReporter(context.Background(), &buf, true, time.Hour) // never reached
	r.phase("syncing 3 checkpoints")
	r.setDetail("writing 1/2 objects")
	r.finish("pushed 3 checkpoints")
	if buf.Len() != 0 {
		t.Fatalf("fast push wrote %q, want nothing", buf.String())
	}
}

// TestGitProgressStreamer_ParsesChunkedProgressStream feeds a realistic
// \r-delimited git --progress byte stream in small chunks that deliberately
// split lines mid-way (simulating a real os/exec stderr pipe copy), and
// asserts onEvent fires with the expected phases/counts as soon as each
// segment completes.
func TestGitProgressStreamer_ParsesChunkedProgressStream(t *testing.T) {
	t.Parallel()

	full := "Enumerating objects: 47, done.\r" +
		"Counting objects:  10% (5/47)\r" +
		"Counting objects: 100% (47/47), done.\n" +
		"Compressing objects:  50% (19/38)\r" +
		"Compressing objects: 100% (38/38), done.\n" +
		"Writing objects:  85% (40/47), 120.00 KiB | 240.00 KiB/s\r" +
		"Writing objects: 100% (47/47), 156.23 KiB | 312.00 KiB/s, done.\n"

	var events []*gitProgressEvent
	streamer := &gitProgressStreamer{
		onEvent: func(e *gitProgressEvent) {
			events = append(events, e)
		},
	}

	const chunkSize = 7
	for i := 0; i < len(full); i += chunkSize {
		end := min(i+chunkSize, len(full))
		n, err := streamer.Write([]byte(full[i:end]))
		if err != nil {
			t.Fatalf("Write returned error: %v", err)
		}
		if n != end-i {
			t.Fatalf("Write returned n=%d, want %d", n, end-i)
		}
	}

	if len(events) == 0 {
		t.Fatal("expected at least one parsed event")
	}

	var sawWritingInProgress, sawWritingDone bool
	for _, e := range events {
		if e.Phase != gitProgressPhaseWriting {
			continue
		}
		switch {
		case e.Done:
			sawWritingDone = true
			if e.Current != 47 || e.Total != 47 {
				t.Fatalf("writing done event has wrong counts: %+v", e)
			}
		case e.Current == 40 && e.Total == 47:
			sawWritingInProgress = true
			if got := formatPushProgressDetail(e); got != "writing 40/47 objects" {
				t.Fatalf("formatPushProgressDetail = %q, want %q", got, "writing 40/47 objects")
			}
		}
	}
	if !sawWritingInProgress {
		t.Fatalf("expected an in-progress writing event (40/47), got %+v", events)
	}
	if !sawWritingDone {
		t.Fatalf("expected a writing-phase done event, got %+v", events)
	}
}

func TestGitProgressStreamer_BuffersLeftoverPartialLine(t *testing.T) {
	t.Parallel()

	var events []*gitProgressEvent
	streamer := &gitProgressStreamer{
		onEvent: func(e *gitProgressEvent) {
			events = append(events, e)
		},
	}

	// Split a single line across two Write calls with no terminator in the
	// first chunk — the streamer must buffer it rather than parse a partial
	// line or drop it.
	n1, err := streamer.Write([]byte("Compressing objects:  50% (19"))
	if err != nil || n1 != len("Compressing objects:  50% (19") {
		t.Fatalf("first Write: n=%d err=%v", n1, err)
	}
	if len(events) != 0 {
		t.Fatalf("expected no event before the line terminator, got %+v", events)
	}

	n2, err := streamer.Write([]byte("/38)\r"))
	if err != nil || n2 != len("/38)\r") {
		t.Fatalf("second Write: n=%d err=%v", n2, err)
	}
	if len(events) != 1 {
		t.Fatalf("expected exactly one event after the terminator arrived, got %+v", events)
	}
	if events[0].Phase != gitProgressPhaseCompressing || events[0].Current != 19 || events[0].Total != 38 {
		t.Fatalf("unexpected event: %+v", events[0])
	}
}
