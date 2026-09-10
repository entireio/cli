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
//
// The shorter-than-a-BOM case additionally pins the one-byte gate in
// skipUTF8BOM: such a payload returns only because the BOM check peeks one byte
// before it peeks three. An unconditional Peek(3) blocks here.
//
// The claim is scoped to VALID payloads. A payload that is only a BOM does now
// block until the writer closes, because stripping it leaves the decoder
// nothing to read; see skipUTF8BOM.
func TestReadHookInputRawLimited_ReturnsBeforeEOF(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		payload string
	}{
		{"complete value", `{"session_file":"/t.jsonl"}`},
		{"shorter than a BOM", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pr, pw := io.Pipe()
			go func() {
				if _, err := pw.Write([]byte(tc.payload)); err != nil {
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
		})
	}
}

// TestReadAndParseHookInput_SkipsUTF8BOM covers the payload shape Cursor
// delivers on Windows: PowerShell 5.1 writes a BOM ahead of the JSON and a
// trailing CRLF after it, and Go's decoder rejects the BOM as
// `invalid character 'ï' looking for beginning of value`.
//
// Both BOM counts are measured, not hypothetical. The hook command's own
// `$OutputEncoding = [System.Text.Encoding]::UTF8` adds one at any console
// codepage; a UTF-8 console codepage adds a second, independently. So the
// stacked shape is what a cp65001 box actually sends.
func TestReadAndParseHookInput_SkipsUTF8BOM(t *testing.T) {
	t.Parallel()

	const bom = "\xef\xbb\xbf"
	const payload = `{"session_id":"abc","transcript_path":"/t.jsonl"}`

	for _, tc := range []struct {
		name  string
		input string
	}{
		{"one BOM", bom + payload + "\r\n"},
		{"stacked BOMs", bom + bom + payload + "\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ReadAndParseHookInput[hookInput](strings.NewReader(tc.input))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.SessionID != "abc" {
				t.Fatalf("session_id = %q, want %q", got.SessionID, "abc")
			}
			if got.TranscriptPath != "/t.jsonl" {
				t.Fatalf("transcript_path = %q, want %q", got.TranscriptPath, "/t.jsonl")
			}
		})
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
