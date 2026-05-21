package antigravity

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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
