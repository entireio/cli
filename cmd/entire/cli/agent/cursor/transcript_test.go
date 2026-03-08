package cursor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Compile-time interface check.
var _ agent.TranscriptAnalyzer = (*CursorAgent)(nil)

// --- GetTranscriptPosition ---

func TestCursorAgent_GetTranscriptPosition(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}

	tmpDir := t.TempDir()
	path := writeSampleTranscript(t, tmpDir)

	pos, err := ag.GetTranscriptPosition(path)
	if err != nil {
		t.Fatalf("GetTranscriptPosition() error = %v", err)
	}
	if pos != 4 {
		t.Errorf("GetTranscriptPosition() = %d, want 4", pos)
	}
}

func TestCursorAgent_GetTranscriptPosition_EmptyPath(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}

	pos, err := ag.GetTranscriptPosition("")
	if err != nil {
		t.Fatalf("GetTranscriptPosition() error = %v", err)
	}
	if pos != 0 {
		t.Errorf("GetTranscriptPosition() = %d, want 0", pos)
	}
}

func TestCursorAgent_GetTranscriptPosition_NonexistentFile(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}

	pos, err := ag.GetTranscriptPosition("/nonexistent/path/transcript.jsonl")
	if err != nil {
		t.Fatalf("GetTranscriptPosition() error = %v", err)
	}
	if pos != 0 {
		t.Errorf("GetTranscriptPosition() = %d, want 0", pos)
	}
}

// --- ExtractPrompts ---

func TestCursorAgent_ExtractPrompts(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}

	tmpDir := t.TempDir()
	path := writeSampleTranscript(t, tmpDir)

	prompts, err := ag.ExtractPrompts(path, 0)
	if err != nil {
		t.Fatalf("ExtractPrompts() error = %v", err)
	}
	if len(prompts) != 2 {
		t.Fatalf("ExtractPrompts() returned %d prompts, want 2", len(prompts))
	}
	// Verify <user_query> tags are stripped
	if prompts[0] != "hello" {
		t.Errorf("prompts[0] = %q, want %q", prompts[0], "hello")
	}
	if prompts[1] != "add 'one' to a file and commit" {
		t.Errorf("prompts[1] = %q, want %q", prompts[1], "add 'one' to a file and commit")
	}
}

func TestCursorAgent_ExtractPrompts_WithOffset(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}

	tmpDir := t.TempDir()
	path := writeSampleTranscript(t, tmpDir)

	// Offset 2 skips the first user+assistant pair, leaving 1 user prompt
	prompts, err := ag.ExtractPrompts(path, 2)
	if err != nil {
		t.Fatalf("ExtractPrompts() error = %v", err)
	}
	if len(prompts) != 1 {
		t.Fatalf("ExtractPrompts() returned %d prompts, want 1", len(prompts))
	}
	if prompts[0] != "add 'one' to a file and commit" {
		t.Errorf("prompts[0] = %q, want %q", prompts[0], "add 'one' to a file and commit")
	}
}

func TestCursorAgent_ExtractPrompts_EmptyFile(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}

	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "empty.jsonl")
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatalf("failed to write empty file: %v", err)
	}

	prompts, err := ag.ExtractPrompts(path, 0)
	if err != nil {
		t.Fatalf("ExtractPrompts() error = %v", err)
	}
	if len(prompts) != 0 {
		t.Errorf("ExtractPrompts() returned %d prompts, want 0", len(prompts))
	}
}

// --- ExtractSummary ---

func TestCursorAgent_ExtractSummary(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}

	tmpDir := t.TempDir()
	path := writeSampleTranscript(t, tmpDir)

	summary, err := ag.ExtractSummary(path)
	if err != nil {
		t.Fatalf("ExtractSummary() error = %v", err)
	}
	if summary != "Created one.txt with one and committed." {
		t.Errorf("ExtractSummary() = %q, want %q", summary, "Created one.txt with one and committed.")
	}
}

func TestCursorAgent_ExtractSummary_EmptyFile(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}

	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "empty.jsonl")
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatalf("failed to write empty file: %v", err)
	}

	summary, err := ag.ExtractSummary(path)
	if err != nil {
		t.Fatalf("ExtractSummary() error = %v", err)
	}
	if summary != "" {
		t.Errorf("ExtractSummary() = %q, want empty string", summary)
	}
}

// --- ExtractModifiedFilesFromOffset ---

func TestCursorAgent_ExtractModifiedFilesFromOffset(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}

	tmpDir := t.TempDir()
	path := writeSampleTranscript(t, tmpDir)

	files, pos, err := ag.ExtractModifiedFilesFromOffset(path, 0)
	if err != nil {
		t.Fatalf("ExtractModifiedFilesFromOffset() error = %v, want nil", err)
	}
	if files != nil {
		t.Errorf("ExtractModifiedFilesFromOffset() files = %v, want nil", files)
	}
	if pos != 0 {
		t.Errorf("ExtractModifiedFilesFromOffset() pos = %d, want 0", pos)
	}
}

func TestCursorAgent_ExtractModifiedFilesFromOffset_NonexistentFile(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}

	files, pos, err := ag.ExtractModifiedFilesFromOffset("/nonexistent/path.jsonl", 0)
	if err != nil {
		t.Fatalf("ExtractModifiedFilesFromOffset() error = %v, want nil", err)
	}
	if files != nil {
		t.Errorf("ExtractModifiedFilesFromOffset() files = %v, want nil", files)
	}
	if pos != 0 {
		t.Errorf("ExtractModifiedFilesFromOffset() pos = %d, want 0", pos)
	}
}

func TestCursorAgent_ExtractModifiedFilesFromOffset_EmptyPath(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}

	files, pos, err := ag.ExtractModifiedFilesFromOffset("", 0)
	if err != nil {
		t.Fatalf("ExtractModifiedFilesFromOffset() error = %v", err)
	}
	if files != nil {
		t.Errorf("ExtractModifiedFilesFromOffset() files = %v, want nil", files)
	}
	if pos != 0 {
		t.Errorf("ExtractModifiedFilesFromOffset() pos = %d, want 0", pos)
	}
}
