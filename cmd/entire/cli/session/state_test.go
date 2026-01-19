package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"entire.io/cli/cmd/entire/cli/paths"
)

func TestStateStore_Save_CreatesReadme(t *testing.T) {
	tmpDir := t.TempDir()
	stateDir := filepath.Join(tmpDir, "entire-sessions")

	store := NewStateStoreWithDir(stateDir)

	state := &State{
		SessionID:  "test-session-123",
		BaseCommit: "abc123def456",
		StartedAt:  time.Now(),
	}

	err := store.Save(context.Background(), state)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// Verify README was created
	readmePath := filepath.Join(stateDir, paths.ReadmeFileName)
	content, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("README should exist at %s: %v", readmePath, err)
	}

	// Verify content matches expected
	if string(content) != paths.SessionStateDirReadme {
		t.Errorf("README content mismatch\ngot:\n%s\nwant:\n%s", string(content), paths.SessionStateDirReadme)
	}
}

func TestStateStore_Save_DoesNotOverwriteExistingReadme(t *testing.T) {
	tmpDir := t.TempDir()
	stateDir := filepath.Join(tmpDir, "entire-sessions")

	// Create directory with custom README
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("failed to create state dir: %v", err)
	}
	customContent := "# Custom README\n\nUser-modified content\n"
	readmePath := filepath.Join(stateDir, paths.ReadmeFileName)
	if err := os.WriteFile(readmePath, []byte(customContent), 0o644); err != nil {
		t.Fatalf("failed to write custom README: %v", err)
	}

	store := NewStateStoreWithDir(stateDir)

	state := &State{
		SessionID:  "test-session-456",
		BaseCommit: "def789abc012",
		StartedAt:  time.Now(),
	}

	err := store.Save(context.Background(), state)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// Verify README still has custom content
	content, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("failed to read README: %v", err)
	}

	if string(content) != customContent {
		t.Errorf("README was overwritten\ngot:\n%s\nwant:\n%s", string(content), customContent)
	}
}

func TestStateStore_SaveLoadClear(t *testing.T) {
	tmpDir := t.TempDir()
	stateDir := filepath.Join(tmpDir, "entire-sessions")

	store := NewStateStoreWithDir(stateDir)

	// Test Save
	state := &State{
		SessionID:       "test-session-789",
		BaseCommit:      "abc123",
		StartedAt:       time.Now().Truncate(time.Second),
		CheckpointCount: 5,
		FilesTouched:    []string{"file1.go", "file2.go"},
	}

	err := store.Save(context.Background(), state)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// Test Load
	loaded, err := store.Load(context.Background(), state.SessionID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded == nil {
		t.Fatal("Load() returned nil")
	}
	if loaded.SessionID != state.SessionID {
		t.Errorf("SessionID = %q, want %q", loaded.SessionID, state.SessionID)
	}
	if loaded.BaseCommit != state.BaseCommit {
		t.Errorf("BaseCommit = %q, want %q", loaded.BaseCommit, state.BaseCommit)
	}
	if loaded.CheckpointCount != state.CheckpointCount {
		t.Errorf("CheckpointCount = %d, want %d", loaded.CheckpointCount, state.CheckpointCount)
	}

	// Test Clear
	err = store.Clear(context.Background(), state.SessionID)
	if err != nil {
		t.Fatalf("Clear() error = %v", err)
	}

	// Verify cleared
	loaded, err = store.Load(context.Background(), state.SessionID)
	if err != nil {
		t.Fatalf("Load() after clear error = %v", err)
	}
	if loaded != nil {
		t.Error("Load() after clear should return nil")
	}
}
