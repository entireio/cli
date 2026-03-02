package session

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestAppendAndReadFileEdits(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewStateStoreWithDir(dir)

	sessionID := "2026-03-02-test-session"
	edit1 := agent.FileEdit{
		FilePath:     "cmd/main.go",
		Action:       agent.FileEditActionEdit,
		ToolName:     "Edit",
		LinesAdded:   5,
		LinesRemoved: 2,
		Timestamp:    time.Now(),
	}
	edit2 := agent.FileEdit{
		FilePath:   "cmd/server.go",
		Action:     agent.FileEditActionWrite,
		ToolName:   "Write",
		LinesAdded: 100,
		Timestamp:  time.Now(),
	}

	if err := store.AppendFileEdit(sessionID, edit1); err != nil {
		t.Fatalf("AppendFileEdit(1): %v", err)
	}
	if err := store.AppendFileEdit(sessionID, edit2); err != nil {
		t.Fatalf("AppendFileEdit(2): %v", err)
	}

	edits, err := store.ReadFileEdits(sessionID)
	if err != nil {
		t.Fatalf("ReadFileEdits: %v", err)
	}
	if len(edits) != 2 {
		t.Fatalf("ReadFileEdits returned %d edits, want 2", len(edits))
	}
	if edits[0].FilePath != "cmd/main.go" {
		t.Errorf("edits[0].FilePath = %q, want %q", edits[0].FilePath, "cmd/main.go")
	}
	if edits[0].Action != agent.FileEditActionEdit {
		t.Errorf("edits[0].Action = %q, want %q", edits[0].Action, agent.FileEditActionEdit)
	}
	if edits[0].LinesAdded != 5 {
		t.Errorf("edits[0].LinesAdded = %d, want 5", edits[0].LinesAdded)
	}
	if edits[0].LinesRemoved != 2 {
		t.Errorf("edits[0].LinesRemoved = %d, want 2", edits[0].LinesRemoved)
	}
	if edits[1].FilePath != "cmd/server.go" {
		t.Errorf("edits[1].FilePath = %q, want %q", edits[1].FilePath, "cmd/server.go")
	}
	if edits[1].Action != agent.FileEditActionWrite {
		t.Errorf("edits[1].Action = %q, want %q", edits[1].Action, agent.FileEditActionWrite)
	}
	if edits[1].LinesAdded != 100 {
		t.Errorf("edits[1].LinesAdded = %d, want 100", edits[1].LinesAdded)
	}
}

func TestClearFileEdits(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewStateStoreWithDir(dir)

	sessionID := "2026-03-02-clear-test"
	edit := agent.FileEdit{
		FilePath:  "file.go",
		Action:    agent.FileEditActionEdit,
		ToolName:  "Edit",
		Timestamp: time.Now(),
	}

	if err := store.AppendFileEdit(sessionID, edit); err != nil {
		t.Fatalf("AppendFileEdit: %v", err)
	}
	if err := store.ClearFileEdits(sessionID); err != nil {
		t.Fatalf("ClearFileEdits: %v", err)
	}

	edits, err := store.ReadFileEdits(sessionID)
	if err != nil {
		t.Fatalf("ReadFileEdits after clear: %v", err)
	}
	if len(edits) != 0 {
		t.Errorf("ReadFileEdits after clear returned %d edits, want 0", len(edits))
	}
}

func TestReadFileEdits_NonExistent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewStateStoreWithDir(dir)

	edits, err := store.ReadFileEdits("2026-03-02-nonexistent")
	if err != nil {
		t.Fatalf("ReadFileEdits for nonexistent: %v", err)
	}
	if len(edits) != 0 {
		t.Errorf("ReadFileEdits for nonexistent returned %d edits, want 0", len(edits))
	}
}

func TestFileEditsPath(t *testing.T) {
	t.Parallel()
	store := NewStateStoreWithDir("/tmp/test")
	path := store.fileEditsPath("2026-03-02-my-session")
	expected := filepath.Join("/tmp/test", "2026-03-02-my-session-edits.jsonl")
	if path != expected {
		t.Errorf("fileEditsPath = %q, want %q", path, expected)
	}
}

func TestAppendFileEdit_InvalidSessionID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewStateStoreWithDir(dir)

	err := store.AppendFileEdit("", agent.FileEdit{})
	if err == nil {
		t.Fatal("expected error for empty session ID, got nil")
	}

	err = store.AppendFileEdit("../../etc/passwd", agent.FileEdit{})
	if err == nil {
		t.Fatal("expected error for path traversal session ID, got nil")
	}
}

func TestReadFileEdits_InvalidSessionID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewStateStoreWithDir(dir)

	_, err := store.ReadFileEdits("")
	if err == nil {
		t.Fatal("expected error for empty session ID, got nil")
	}
}

func TestClearFileEdits_InvalidSessionID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewStateStoreWithDir(dir)

	err := store.ClearFileEdits("")
	if err == nil {
		t.Fatal("expected error for empty session ID, got nil")
	}
}

func TestClearFileEdits_NonExistent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewStateStoreWithDir(dir)

	// Clearing a non-existent file should not error
	err := store.ClearFileEdits("2026-03-02-no-such-session")
	if err != nil {
		t.Fatalf("ClearFileEdits on non-existent: %v", err)
	}
}
