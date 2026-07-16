package agent

import (
	"io"
	"strings"
	"testing"
	"time"
)

type hookInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
}

// TestReadAndParseHookInput_ReturnsBeforeEOF proves the hook reader returns as
// soon as a complete JSON value has arrived, WITHOUT waiting for stdin to be
// closed. On Windows/Git Bash the agent keeps the pipe's write end open for the
// hook's lifetime; io.ReadAll blocked forever there (issue #1398). We simulate
// that by writing the payload to an io.Pipe and never closing the writer.
func TestReadAndParseHookInput_ReturnsBeforeEOF(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	// Write a complete payload, then hold the pipe open (never Close) — mimics an
	// agent that keeps stdin open after delivering the JSON.
	go func() {
		if _, err := pw.Write([]byte(`{"session_id":"s1","transcript_path":"/t.jsonl"}`)); err != nil {
			_ = pw.CloseWithError(err)
		}
		// Intentionally no pw.Close() on success: stdin stays open, so EOF never arrives.
	}()

	type result struct {
		val *hookInput
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := ReadAndParseHookInput[hookInput](pr)
		done <- result{v, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("unexpected error: %v", r.err)
		}
		if r.val == nil || r.val.SessionID != "s1" || r.val.TranscriptPath != "/t.jsonl" {
			t.Fatalf("unexpected value: %+v", r.val)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ReadAndParseHookInput blocked waiting for EOF — regression of #1398")
	}
}

// TestReadHookInputRawLimited_ReturnsBeforeEOF is the external-agent analogue of
// TestReadAndParseHookInput_ReturnsBeforeEOF: the size-bounded raw reader must
// also return on the first complete JSON value without waiting for stdin close
// (issue #1398).
func TestReadHookInputRawLimited_ReturnsBeforeEOF(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	go func() {
		if _, err := pw.Write([]byte(`{"session_file":"/t.jsonl"}`)); err != nil {
			_ = pw.CloseWithError(err)
		}
		// No Close(): the write end stays open, so EOF never arrives.
	}()

	done := make(chan error, 1)
	go func() {
		_, err := ReadHookInputRawLimited(pr, 10*1024*1024)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ReadHookInputRawLimited blocked waiting for EOF — regression of #1398")
	}
}

// TestReadHookInputRawLimited_RejectsOversized proves the byte ceiling turns an
// over-limit payload into an error rather than an unbounded read.
func TestReadHookInputRawLimited_RejectsOversized(t *testing.T) {
	t.Parallel()

	big := `{"k":"` + strings.Repeat("x", 512) + `"}`
	_, err := ReadHookInputRawLimited(strings.NewReader(big), 64)
	if err == nil {
		t.Fatal("expected error for payload exceeding the limit, got nil")
	}
}

func TestReadAndParseHookInput_EmptyInputEOF(t *testing.T) {
	t.Parallel()

	_, err := ReadAndParseHookInput[hookInput](strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "empty hook input") {
		t.Fatalf("want 'empty hook input' error, got: %v", err)
	}
}

func TestReadAndParseHookInput_MalformedJSON(t *testing.T) {
	t.Parallel()

	_, err := ReadAndParseHookInput[hookInput](strings.NewReader(`{"session_id": INVALID}`))
	if err == nil || !strings.Contains(err.Error(), "failed to parse hook input") {
		t.Fatalf("want 'failed to parse hook input' error, got: %v", err)
	}
}

func TestReadAndParseHookInput_ValidPayload(t *testing.T) {
	t.Parallel()

	got, err := ReadAndParseHookInput[hookInput](strings.NewReader(`{"session_id":"abc","transcript_path":"/x"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.SessionID != "abc" || got.TranscriptPath != "/x" {
		t.Fatalf("unexpected value: %+v", got)
	}
}
