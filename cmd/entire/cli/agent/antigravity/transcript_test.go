package antigravity

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChunkAndReassemble_RoundTrip(t *testing.T) {
	t.Parallel()
	a := &AntigravityAgent{}
	original := []byte(`{"role":"user","content":"hi"}` + "\n" + `{"role":"assistant","content":"hello"}` + "\n")
	chunks, err := a.ChunkTranscript(context.Background(), original, 1024)
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.ReassembleTranscript(chunks)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, original) {
		t.Errorf("round-trip mismatch:\n  in:  %q\n  out: %q", original, out)
	}
}

// TestPrepareTranscript_AbsentFileCreatesPlaceholder verifies the
// TranscriptPreparer creates an empty file when agy hasn't flushed its
// transcript yet (the common case at Stop hook time). Without this, the
// framework's fileExists check in handleLifecycleTurnEnd would fail and our
// hook would exit non-zero, aborting agy's turn.
func TestPrepareTranscript_AbsentFileCreatesPlaceholder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Path with non-existent parent dirs to also exercise MkdirAll
	path := filepath.Join(dir, ".gemini", "antigravity-cli", "brain", "conv", "logs", "t.jsonl")
	a := &AntigravityAgent{}
	if err := a.PrepareTranscript(context.Background(), path); err != nil {
		t.Fatalf("PrepareTranscript: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("placeholder not created: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("placeholder size = %d, want 0 (empty)", info.Size())
	}
}

// TestPrepareTranscript_PresentFilePreserved verifies PrepareTranscript leaves
// an already-written transcript untouched. This is the case when agy's writer
// races ahead of the Stop hook.
func TestPrepareTranscript_PresentFilePreserved(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	original := []byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE"}` + "\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{}
	if err := a.PrepareTranscript(context.Background(), path); err != nil {
		t.Fatalf("PrepareTranscript: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("PrepareTranscript should not overwrite an existing transcript\n  before: %q\n  after:  %q", original, got)
	}
}

// TestPrepareTranscript_EmptyRefIsNoOp verifies an empty transcript path is
// a graceful no-op (defensive — the framework probably never passes empty,
// but agents have been bitten by empty refs in the past).
func TestPrepareTranscript_EmptyRefIsNoOp(t *testing.T) {
	t.Parallel()
	a := &AntigravityAgent{}
	if err := a.PrepareTranscript(context.Background(), ""); err != nil {
		t.Errorf("PrepareTranscript(\"\") should not error, got %v", err)
	}
}

func TestExtractPrompts_StripsUserRequestWrapper(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	lines := []string{
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"<USER_REQUEST>\nread a.txt and exit\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is: x.\n</ADDITIONAL_METADATA>"}`,
		`{"step_index":1,"source":"SYSTEM","type":"CONVERSATION_HISTORY","status":"DONE"}`,
		`{"step_index":2,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","content":"ok"}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{}
	prompts, err := a.ExtractPrompts(path, 0)
	if err != nil {
		t.Fatalf("ExtractPrompts: %v", err)
	}
	if len(prompts) != 1 {
		t.Fatalf("want 1 prompt, got %d: %#v", len(prompts), prompts)
	}
	if prompts[0] != "read a.txt and exit" {
		t.Errorf("want stripped request, got %q", prompts[0])
	}
}

func TestExtractPrompts_RespectsOffsetAndMissingFile(t *testing.T) {
	t.Parallel()
	a := &AntigravityAgent{}
	got, err := a.ExtractPrompts(filepath.Join(t.TempDir(), "nope.jsonl"), 0)
	if err != nil || got != nil {
		t.Fatalf("missing file: want (nil,nil), got (%#v,%v)", got, err)
	}
}

func TestExtractPrompts_RealFixture(t *testing.T) {
	t.Parallel()
	a := &AntigravityAgent{}
	prompts, err := a.ExtractPrompts("testdata/transcript_sample.jsonl", 0)
	if err != nil {
		t.Fatalf("ExtractPrompts: %v", err)
	}
	if len(prompts) != 1 || prompts[0] != "read a.txt and tell me what it says, then exit" {
		t.Fatalf("unexpected prompts: %#v", prompts)
	}
}

func TestExtractPrompts_SkipsLinesAtOrBelowOffset(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	lines := []string{
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"<USER_REQUEST>first</USER_REQUEST>"}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","content":"ok"}`,
		`{"step_index":2,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"<USER_REQUEST>second</USER_REQUEST>"}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{}

	// offset 0 → both prompts
	all, err := a.ExtractPrompts(path, 0)
	if err != nil {
		t.Fatalf("ExtractPrompts(0): %v", err)
	}
	if len(all) != 2 || all[0] != "first" || all[1] != "second" {
		t.Fatalf("offset 0: want [first second], got %#v", all)
	}

	// offset 1 → first non-blank line consumed, so only the second USER_INPUT remains
	rest, err := a.ExtractPrompts(path, 1)
	if err != nil {
		t.Fatalf("ExtractPrompts(1): %v", err)
	}
	if len(rest) != 1 || rest[0] != "second" {
		t.Fatalf("offset 1: want [second], got %#v", rest)
	}
}
