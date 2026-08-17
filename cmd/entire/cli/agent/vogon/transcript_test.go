package vogon

import (
	"os"
	"path/filepath"
	"testing"
)

// TestExtractModifiedFilesFromOffset_OnlyToolUseEntries pins the analyzer to its
// contract: file paths come from tool-use entries only. Reading `files` off any
// line would misattribute paths the moment the transcript format grows another
// entry type carrying them.
func TestExtractModifiedFilesFromOffset_OnlyToolUseEntries(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "session.jsonl")
	lines := `{"type":"user","message":"write two files"}
{"type":"tool_use","message":"wrote a.go","files":["a.go"]}
{"type":"summary","message":"touched b.go","files":["b.go"]}
{"type":"assistant","message":"done"}
`
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	files, pos, err := (&Agent{}).ExtractModifiedFilesFromOffset(path, 0)
	if err != nil {
		t.Fatalf("ExtractModifiedFilesFromOffset: %v", err)
	}
	if pos != 4 {
		t.Errorf("position = %d, want 4", pos)
	}
	if len(files) != 1 || files[0] != "a.go" {
		t.Errorf("files = %v, want [a.go] — a non-tool_use entry must not contribute paths", files)
	}
}

// TestExtractModifiedFilesFromOffset_HonoursOffsetAndDedupes covers the two other
// properties the framework relies on: entries before the offset are excluded (turn
// scoping), and a file written twice is reported once.
func TestExtractModifiedFilesFromOffset_HonoursOffsetAndDedupes(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "session.jsonl")
	lines := `{"type":"tool_use","message":"wrote old.go","files":["old.go"]}
{"type":"tool_use","message":"wrote new.go","files":["new.go"]}
{"type":"tool_use","message":"wrote new.go again","files":["new.go"]}
`
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	files, _, err := (&Agent{}).ExtractModifiedFilesFromOffset(path, 1)
	if err != nil {
		t.Fatalf("ExtractModifiedFilesFromOffset: %v", err)
	}
	if len(files) != 1 || files[0] != "new.go" {
		t.Errorf("files = %v, want [new.go] (old.go is before the offset, new.go deduped)", files)
	}
}

// TestGetTranscriptPosition_MissingFile documents that an absent transcript is not
// an error — the framework asks for a position before the agent has written one.
func TestGetTranscriptPosition_MissingFile(t *testing.T) {
	t.Parallel()

	pos, err := (&Agent{}).GetTranscriptPosition(filepath.Join(t.TempDir(), "absent.jsonl"))
	if err != nil {
		t.Fatalf("GetTranscriptPosition on a missing file: %v", err)
	}
	if pos != 0 {
		t.Errorf("position = %d, want 0", pos)
	}
}
