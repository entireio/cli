package strategy

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestPushShouldReveal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		elapsed, thresh time.Duration
		tty, want       bool
	}{
		{"non-tty never reveals", 10 * time.Second, time.Second, false, false},
		{"tty below threshold stays hidden", 500 * time.Millisecond, time.Second, true, false},
		{"tty at threshold reveals", time.Second, time.Second, true, true},
		{"tty past threshold reveals", 3 * time.Second, time.Second, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := pushShouldReveal(tc.elapsed, tc.thresh, tc.tty); got != tc.want {
				t.Fatalf("pushShouldReveal(%v,%v,%v)=%v want %v", tc.elapsed, tc.thresh, tc.tty, got, tc.want)
			}
		})
	}
}

func TestPushReporter_NonTTY_WritesNothing(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	r := newPushReporter(context.Background(), &buf, false, time.Millisecond)
	r.phase("syncing 3 checkpoints")
	time.Sleep(10 * time.Millisecond)
	r.finish("pushed 3")
	if buf.Len() != 0 {
		t.Fatalf("non-tty reporter wrote %q, want nothing", buf.String())
	}
}

func TestPushReporter_TTY_RevealsThenClears(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	r := newPushReporter(context.Background(), &buf, true, time.Millisecond)
	r.phase("syncing 3 checkpoints")
	time.Sleep(15 * time.Millisecond) // let the reveal goroutine fire
	r.finish("pushed 3")
	out := buf.String()
	if !strings.Contains(out, "syncing 3 checkpoints") {
		t.Fatalf("expected revealed phase text, got %q", out)
	}
	if !strings.HasSuffix(out, "\r\033[K") {
		t.Fatalf("expected trailing clear sequence, got %q", out)
	}
}

func TestPushReporter_TTY_FastPush_StaysHidden(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	r := newPushReporter(context.Background(), &buf, true, time.Hour) // never reached
	r.phase("syncing 3 checkpoints")
	r.finish("pushed 3")
	if buf.Len() != 0 {
		t.Fatalf("fast push wrote %q, want nothing", buf.String())
	}
}
