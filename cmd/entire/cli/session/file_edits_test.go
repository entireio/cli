package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestAppendAndReadFileEdits(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	store := &StateStore{stateDir: stateDir}
	sessionID := "2026-03-02-test-session"

	edit1 := agent.FileEdit{
		FilePath:     "cmd/main.go",
		Action:       agent.FileEditActionWrite,
		ToolName:     "Write",
		LinesAdded:   10,
		LinesRemoved: 0,
		Timestamp:    time.Now(),
	}

	edit2 := agent.FileEdit{
		FilePath:     "cmd/utils.go",
		Action:       agent.FileEditActionEdit,
		ToolName:     "Edit",
		LinesAdded:   3,
		LinesRemoved: 1,
		Timestamp:    time.Now(),
	}

	// Append two edits
	if err := store.AppendFileEdit(sessionID, edit1); err != nil {
		t.Fatalf("AppendFileEdit(edit1) error = %v", err)
	}
	if err := store.AppendFileEdit(sessionID, edit2); err != nil {
		t.Fatalf("AppendFileEdit(edit2) error = %v", err)
	}

	// Read them back
	edits, err := store.ReadFileEdits(sessionID)
	if err != nil {
		t.Fatalf("ReadFileEdits() error = %v", err)
	}
	if len(edits) != 2 {
		t.Fatalf("ReadFileEdits() got %d edits, want 2", len(edits))
	}
	if edits[0].FilePath != "cmd/main.go" {
		t.Errorf("edits[0].FilePath = %q, want %q", edits[0].FilePath, "cmd/main.go")
	}
	if edits[0].Action != agent.FileEditActionWrite {
		t.Errorf("edits[0].Action = %q, want %q", edits[0].Action, agent.FileEditActionWrite)
	}
	if edits[1].FilePath != "cmd/utils.go" {
		t.Errorf("edits[1].FilePath = %q, want %q", edits[1].FilePath, "cmd/utils.go")
	}
}

func TestReadFileEdits_NonexistentFile(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	store := &StateStore{stateDir: stateDir}

	edits, err := store.ReadFileEdits("2026-03-02-nonexistent")
	if err != nil {
		t.Fatalf("ReadFileEdits() error = %v", err)
	}
	if edits != nil {
		t.Errorf("ReadFileEdits() = %v, want nil", edits)
	}
}

func TestClearFileEdits(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	store := &StateStore{stateDir: stateDir}
	sessionID := "2026-03-02-clear-test"

	edit := agent.FileEdit{
		FilePath:  "test.go",
		Action:    agent.FileEditActionWrite,
		ToolName:  "Write",
		Timestamp: time.Now(),
	}
	if err := store.AppendFileEdit(sessionID, edit); err != nil {
		t.Fatalf("AppendFileEdit() error = %v", err)
	}

	// Verify file exists
	path := filepath.Join(stateDir, sessionID+"-edits.jsonl")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("edit log file should exist after AppendFileEdit")
	}

	// Clear
	if err := store.ClearFileEdits(sessionID); err != nil {
		t.Fatalf("ClearFileEdits() error = %v", err)
	}

	// Verify file is gone
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("edit log file should not exist after ClearFileEdits")
	}

	// ReadFileEdits should return nil for cleared file
	edits, err := store.ReadFileEdits(sessionID)
	if err != nil {
		t.Fatalf("ReadFileEdits() after clear error = %v", err)
	}
	if edits != nil {
		t.Errorf("ReadFileEdits() after clear = %v, want nil", edits)
	}
}

func TestClearFileEdits_NonexistentFile(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	store := &StateStore{stateDir: stateDir}

	// Should not error when file doesn't exist
	if err := store.ClearFileEdits("2026-03-02-nonexistent"); err != nil {
		t.Errorf("ClearFileEdits() error = %v, want nil", err)
	}
}

func TestAppendFileEdit_InvalidSessionID(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	store := &StateStore{stateDir: stateDir}

	edit := agent.FileEdit{FilePath: "test.go"}
	if err := store.AppendFileEdit("../../bad-id", edit); err == nil {
		t.Error("AppendFileEdit() with invalid session ID should error")
	}
}

func TestReadFileEdits_InvalidSessionID(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	store := &StateStore{stateDir: stateDir}

	if _, err := store.ReadFileEdits("../../bad-id"); err == nil {
		t.Error("ReadFileEdits() with invalid session ID should error")
	}
}

func TestAppendFileEdit_MalformedLines(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	store := &StateStore{stateDir: stateDir}
	sessionID := "2026-03-02-malformed"

	// Write valid edit
	edit := agent.FileEdit{
		FilePath: "good.go",
		Action:   agent.FileEditActionWrite,
		ToolName: "Write",
	}
	if err := store.AppendFileEdit(sessionID, edit); err != nil {
		t.Fatalf("AppendFileEdit() error = %v", err)
	}

	// Inject malformed line directly
	path := filepath.Join(stateDir, sessionID+"-edits.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := f.WriteString("not valid json\n"); err != nil {
		t.Fatalf("write malformed line: %v", err)
	}
	f.Close()

	// Append another valid edit
	edit2 := agent.FileEdit{
		FilePath: "good2.go",
		Action:   agent.FileEditActionEdit,
		ToolName: "Edit",
	}
	if err := store.AppendFileEdit(sessionID, edit2); err != nil {
		t.Fatalf("AppendFileEdit() error = %v", err)
	}

	// Should skip malformed lines
	edits, err := store.ReadFileEdits(sessionID)
	if err != nil {
		t.Fatalf("ReadFileEdits() error = %v", err)
	}
	if len(edits) != 2 {
		t.Errorf("ReadFileEdits() got %d edits, want 2 (skipping malformed)", len(edits))
	}
}
