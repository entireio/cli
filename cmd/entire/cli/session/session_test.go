package session

import (
	"context"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
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

// TestGetOrCreateEntireSessionID tests the stable session ID generation logic.
func TestGetOrCreateEntireSessionID(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	agentUUID := "a6c3cac2-2f45-43aa-8c69-419f66a3b5e1"

	// First call - should create new session ID with today's date
	sessionID1 := GetOrCreateEntireSessionID(agentUUID)

	// Verify format: YYYY-MM-DD-<uuid>
	if len(sessionID1) < 11 {
		t.Fatalf("Session ID too short: %s", sessionID1)
	}
	expectedSuffix := "-" + agentUUID
	if sessionID1[len(sessionID1)-len(expectedSuffix):] != expectedSuffix {
		t.Errorf("Session ID should end with %s, got %s", expectedSuffix, sessionID1)
	}

	// Create a state store and save a state to simulate existing session
	store, err := NewStateStore()
	if err != nil {
		t.Fatalf("NewStateStore() error = %v", err)
	}

	state := &State{
		SessionID:       sessionID1,
		BaseCommit:      "test123",
		StartedAt:       time.Now(),
		CheckpointCount: 1,
	}
	if err := store.Save(context.Background(), state); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// Second call - should reuse existing session ID
	sessionID2 := GetOrCreateEntireSessionID(agentUUID)

	if sessionID2 != sessionID1 {
		t.Errorf("Expected to reuse session ID %s, got %s", sessionID1, sessionID2)
	}
}

// TestGetOrCreateEntireSessionID_MultipleStates tests cleanup of duplicate state files.
func TestGetOrCreateEntireSessionID_MultipleStates(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	agentUUID := "b7d4dbd3-3e56-54bb-a70a-52ae77d94c6f"

	// Simulate the bug: create state files from different days
	oldSessionID := "2026-01-22-" + agentUUID
	newSessionID := "2026-01-23-" + agentUUID

	store, err := NewStateStore()
	if err != nil {
		t.Fatalf("NewStateStore() error = %v", err)
	}

	oldState := &State{
		SessionID:       oldSessionID,
		BaseCommit:      "old123",
		StartedAt:       time.Now().Add(-24 * time.Hour),
		CheckpointCount: 2,
	}
	if err := store.Save(context.Background(), oldState); err != nil {
		t.Fatalf("Save(old) error = %v", err)
	}

	newState := &State{
		SessionID:       newSessionID,
		BaseCommit:      "new456",
		StartedAt:       time.Now(),
		CheckpointCount: 3,
	}
	if err := store.Save(context.Background(), newState); err != nil {
		t.Fatalf("Save(new) error = %v", err)
	}

	// Call GetOrCreateEntireSessionID - should pick the newest and cleanup old
	selectedID := GetOrCreateEntireSessionID(agentUUID)

	// Should pick the most recent (2026-01-23)
	if selectedID != newSessionID {
		t.Errorf("Expected most recent session ID %s, got %s", newSessionID, selectedID)
	}

	// Old state file should be cleaned up
	oldStateLoaded, err := store.Load(context.Background(), oldSessionID)
	if err != nil {
		t.Fatalf("Load(old) error = %v", err)
	}
	if oldStateLoaded != nil {
		t.Errorf("Old state file should have been cleaned up, but still exists")
	}

	// New state file should still exist
	newStateLoaded, err := store.Load(context.Background(), newSessionID)
	if err != nil {
		t.Fatalf("Load(new) error = %v", err)
	}
	if newStateLoaded == nil {
		t.Errorf("New state file should exist")
	}
}
