package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
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

func TestPrepareTranscript_WaitsForDelayedTranscript(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ".gemini", "antigravity-cli", "brain", "conv", "logs", "t.jsonl")
	original := []byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE"}` + "\n")

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeErr := make(chan error, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		writeErr <- os.WriteFile(path, original, 0o600)
	}()

	a := &AntigravityAgent{}
	if err := a.PrepareTranscript(context.Background(), path); err != nil {
		t.Fatalf("PrepareTranscript: %v", err)
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("delayed write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("PrepareTranscript() should wait for delayed transcript, got %q want %q", got, original)
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

func TestExtractPrompts_UnwrapsAntigravityUserInput(t *testing.T) {
	t.Parallel()
	path := writeAntigravityTranscript(t, map[string]string{
		"source": "USER_EXPLICIT",
		"type":   "USER_INPUT",
		"content": "<USER_REQUEST>\n" +
			"Use the workspace at /tmp/repo.\n\n" +
			"Request:\n" +
			"create a file at docs/feature.md with feature module notes\n" +
			"</USER_REQUEST>\n" +
			"<ADDITIONAL_METADATA>\nignored metadata\n</ADDITIONAL_METADATA>",
	})

	prompts, err := (&AntigravityAgent{}).ExtractPrompts(path, 0)
	if err != nil {
		t.Fatalf("ExtractPrompts: %v", err)
	}
	want := []string{"create a file at docs/feature.md with feature module notes"}
	if !reflect.DeepEqual(prompts, want) {
		t.Fatalf("ExtractPrompts() = %#v, want %#v", prompts, want)
	}
}

func TestExtractPrompts_FromOffset(t *testing.T) {
	t.Parallel()
	path := writeAntigravityTranscript(t,
		map[string]string{
			"source":  "USER_EXPLICIT",
			"type":    "USER_INPUT",
			"content": "old prompt",
		},
		map[string]string{
			"source":  "MODEL",
			"type":    "PLANNER_RESPONSE",
			"content": "model output",
		},
		map[string]string{
			"source":  "USER_EXPLICIT",
			"type":    "USER_INPUT",
			"content": "<USER_REQUEST>\nRequest:\nnew prompt\n</USER_REQUEST>",
		},
	)

	prompts, err := (&AntigravityAgent{}).ExtractPrompts(path, 2)
	if err != nil {
		t.Fatalf("ExtractPrompts: %v", err)
	}
	want := []string{"new prompt"}
	if !reflect.DeepEqual(prompts, want) {
		t.Fatalf("ExtractPrompts() = %#v, want %#v", prompts, want)
	}
}

func TestExtractPrompts_IgnoresMalformedAndNonUserInput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript_full.jsonl")
	data := []byte("not json\n" +
		`{"source":"MODEL","type":"PLANNER_RESPONSE","content":"assistant text"}` + "\n" +
		`{"source":"USER_EXPLICIT","type":"USER_INPUT","content":"keep me"}` + "\n" +
		`{"source":"USER_EXPLICIT","type":"USER_INPUT","content":"   "}` + "\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	prompts, err := (&AntigravityAgent{}).ExtractPrompts(path, 0)
	if err != nil {
		t.Fatalf("ExtractPrompts: %v", err)
	}
	want := []string{"keep me"}
	if !reflect.DeepEqual(prompts, want) {
		t.Fatalf("ExtractPrompts() = %#v, want %#v", prompts, want)
	}
}

func TestExtractPrompts_MissingFileReturnsNoPrompts(t *testing.T) {
	t.Parallel()
	prompts, err := (&AntigravityAgent{}).ExtractPrompts(filepath.Join(t.TempDir(), "missing.jsonl"), 0)
	if err != nil {
		t.Fatalf("ExtractPrompts: %v", err)
	}
	if len(prompts) != 0 {
		t.Fatalf("ExtractPrompts() = %#v, want no prompts", prompts)
	}
}

func writeAntigravityTranscript(t *testing.T, entries ...map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript_full.jsonl")
	var data []byte
	for _, entry := range entries {
		line, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, line...)
		data = append(data, '\n')
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
