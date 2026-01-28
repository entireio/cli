package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestSession_IsSubSession(t *testing.T) {
	tests := []struct {
		name     string
		session  Session
		expected bool
	}{
		{
			name: "top-level session with empty ParentID",
			session: Session{
				ID:       "session-123",
				ParentID: "",
			},
			expected: false,
		},
		{
			name: "sub-session with ParentID set",
			session: Session{
				ID:        "session-456",
				ParentID:  "session-123",
				ToolUseID: "toolu_abc",
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.session.IsSubSession()
			if result != tt.expected {
				t.Errorf("IsSubSession() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestStateStore_RemoveAll(t *testing.T) {
	// Create a temp directory for the state store
	tmpDir := t.TempDir()
	stateDir := filepath.Join(tmpDir, "entire-sessions")

	store := NewStateStoreWithDir(stateDir)
	ctx := context.Background()

	// Create some session states
	states := []*State{
		{
			SessionID:  "session-1",
			BaseCommit: "abc123",
			StartedAt:  time.Now(),
		},
		{
			SessionID:  "session-2",
			BaseCommit: "def456",
			StartedAt:  time.Now(),
		},
		{
			SessionID:  "session-3",
			BaseCommit: "ghi789",
			StartedAt:  time.Now(),
		},
	}

	for _, state := range states {
		if err := store.Save(ctx, state); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	// Verify states were saved
	savedStates, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(savedStates) != len(states) {
		t.Fatalf("List() returned %d states, want %d", len(savedStates), len(states))
	}

	// Verify directory exists
	if _, err := os.Stat(stateDir); os.IsNotExist(err) {
		t.Fatal("state directory should exist before RemoveAll()")
	}

	// Remove all
	if err := store.RemoveAll(); err != nil {
		t.Fatalf("RemoveAll() error = %v", err)
	}

	// Verify directory is removed
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Error("state directory should not exist after RemoveAll()")
	}

	// List should return empty (directory doesn't exist)
	afterStates, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() after RemoveAll() error = %v", err)
	}
	if len(afterStates) != 0 {
		t.Errorf("List() after RemoveAll() returned %d states, want 0", len(afterStates))
	}
}

func TestStateStore_RemoveAll_EmptyDirectory(t *testing.T) {
	// Create a temp directory for the state store
	tmpDir := t.TempDir()
	stateDir := filepath.Join(tmpDir, "entire-sessions")

	// Create the directory but don't add any files
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatalf("failed to create state dir: %v", err)
	}

	store := NewStateStoreWithDir(stateDir)

	// Remove all on empty directory should succeed
	if err := store.RemoveAll(); err != nil {
		t.Fatalf("RemoveAll() on empty directory error = %v", err)
	}

	// Directory should be removed
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Error("state directory should not exist after RemoveAll()")
	}
}

func TestStateStore_RemoveAll_NonExistentDirectory(t *testing.T) {
	// Create a temp directory for the state store
	tmpDir := t.TempDir()
	stateDir := filepath.Join(tmpDir, "nonexistent-sessions")

	store := NewStateStoreWithDir(stateDir)

	// RemoveAll on non-existent directory should succeed (no-op)
	if err := store.RemoveAll(); err != nil {
		t.Fatalf("RemoveAll() on non-existent directory error = %v", err)
	}
}

func TestFindLegacyEntireSessionID(t *testing.T) {
	// Create a temp git repo
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo
	cmd := exec.CommandContext(context.Background(), "git", "init")
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Create state directory with legacy-format session files
	stateDir := filepath.Join(tmpDir, ".git", sessionStateDirName)
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatalf("failed to create state dir: %v", err)
	}

	t.Run("finds legacy session", func(t *testing.T) {
		agentID := "abc123-def456"
		legacySessionID := "2026-01-20-" + agentID

		// Create a legacy-format state file
		stateFile := filepath.Join(stateDir, legacySessionID+".json")
		if err := os.WriteFile(stateFile, []byte(`{"session_id":"`+legacySessionID+`"}`), 0o600); err != nil {
			t.Fatalf("failed to write state file: %v", err)
		}
		defer os.Remove(stateFile)

		found := FindLegacyEntireSessionID(agentID)
		if found != legacySessionID {
			t.Errorf("FindLegacyEntireSessionID(%q) = %q, want %q", agentID, found, legacySessionID)
		}
	})

	t.Run("returns empty for non-existent session", func(t *testing.T) {
		found := FindLegacyEntireSessionID("nonexistent-session-id")
		if found != "" {
			t.Errorf("FindLegacyEntireSessionID(nonexistent) = %q, want empty string", found)
		}
	})

	t.Run("returns empty for new-format session", func(t *testing.T) {
		// Create a new-format state file (no date prefix)
		newSessionID := "new-format-session-id"
		stateFile := filepath.Join(stateDir, newSessionID+".json")
		if err := os.WriteFile(stateFile, []byte(`{"session_id":"`+newSessionID+`"}`), 0o600); err != nil {
			t.Fatalf("failed to write state file: %v", err)
		}
		defer os.Remove(stateFile)

		// Should not find it as "legacy" since it doesn't have date prefix
		found := FindLegacyEntireSessionID(newSessionID)
		if found != "" {
			t.Errorf("FindLegacyEntireSessionID(new-format) = %q, want empty string", found)
		}
	})

	t.Run("returns empty for empty agent ID", func(t *testing.T) {
		found := FindLegacyEntireSessionID("")
		if found != "" {
			t.Errorf("FindLegacyEntireSessionID('') = %q, want empty string", found)
		}
	})

	t.Run("returns empty for path traversal attempts", func(t *testing.T) {
		// Should reject IDs with path traversal sequences
		maliciousIDs := []string{
			"../../../etc/passwd",
			"session/../../../etc",
			"session/id",
			"session.json/../..",
		}
		for _, id := range maliciousIDs {
			found := FindLegacyEntireSessionID(id)
			if found != "" {
				t.Errorf("FindLegacyEntireSessionID(%q) = %q, want empty string (should be rejected)", id, found)
			}
		}
	})

	t.Run("ignores tmp files", func(t *testing.T) {
		agentID := "tmp-test-id"
		legacySessionID := "2026-01-21-" + agentID

		// Create a .tmp file (should be ignored)
		tmpFile := filepath.Join(stateDir, legacySessionID+".json.tmp")
		if err := os.WriteFile(tmpFile, []byte(`{"session_id":"`+legacySessionID+`"}`), 0o600); err != nil {
			t.Fatalf("failed to write tmp file: %v", err)
		}
		defer os.Remove(tmpFile)

		found := FindLegacyEntireSessionID(agentID)
		if found != "" {
			t.Errorf("FindLegacyEntireSessionID should ignore .tmp files, got %q", found)
		}
	})
}
