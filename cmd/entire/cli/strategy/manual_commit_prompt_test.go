package strategy

import (
	"errors"
	"io"
	"testing"
	"time"
)

// A prompt nobody answers must not hold the commit hook: the read gives up
// and reports the timeout, so askConfirmTTY falls back to its default.
func TestReadPromptAnswer_GivesUpWhenUnanswered(t *testing.T) {
	t.Parallel()
	r, w := io.Pipe()
	t.Cleanup(func() { _ = w.Close() })

	start := time.Now()
	_, err := readPromptAnswer(r, 50*time.Millisecond)
	if !errors.Is(err, errPromptTimedOut) {
		t.Fatalf("err = %v, want errPromptTimedOut", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("gave up after %s, want about the timeout", elapsed)
	}
}

func TestReadPromptAnswer_ReturnsAnswer(t *testing.T) {
	t.Parallel()
	r, w := io.Pipe()
	t.Cleanup(func() { _ = w.Close() })
	go func() {
		if _, err := io.WriteString(w, "a\n"); err != nil {
			t.Errorf("write answer: %v", err)
		}
	}()

	line, err := readPromptAnswer(r, time.Minute)
	if err != nil {
		t.Fatalf("readPromptAnswer: %v", err)
	}
	if line != "a\n" {
		t.Fatalf("line = %q, want %q", line, "a\n")
	}
}
