package strategy

import (
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
)

// TestLoadSessionState_PackageLevel tests the package-level LoadSessionState function.
func TestLoadSessionState_PackageLevel(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	// Create and save a session state using the package-level function
	state := &SessionState{
		SessionID:                "test-session-pkg-123",
		BaseCommit:               "abc123def456",
		StartedAt:                time.Now(),
		CheckpointCount:          3,
		CondensedTranscriptLines: 150,
	}

	// Save using package-level function
	err = SaveSessionState(state)
	if err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	// Load using package-level function
	loaded, err := LoadSessionState("test-session-pkg-123")
	if err != nil {
		t.Fatalf("LoadSessionState() error = %v", err)
	}
	if loaded == nil {
		t.Fatal("LoadSessionState() returned nil")
	}

	// Validate fields (loaded is guaranteed non-nil after the check above)
	verifySessionState(t, loaded, state)
}

// verifySessionState compares loaded session state against expected values.
func verifySessionState(t *testing.T, loaded, expected *SessionState) {
	t.Helper()
	if loaded.SessionID != expected.SessionID {
		t.Errorf("SessionID = %q, want %q", loaded.SessionID, expected.SessionID)
	}
	if loaded.BaseCommit != expected.BaseCommit {
		t.Errorf("BaseCommit = %q, want %q", loaded.BaseCommit, expected.BaseCommit)
	}
	if loaded.CheckpointCount != expected.CheckpointCount {
		t.Errorf("CheckpointCount = %d, want %d", loaded.CheckpointCount, expected.CheckpointCount)
	}
	if loaded.CondensedTranscriptLines != expected.CondensedTranscriptLines {
		t.Errorf("CondensedTranscriptLines = %d, want %d", loaded.CondensedTranscriptLines, expected.CondensedTranscriptLines)
	}
}

// TestLoadSessionState_WithEndedAt tests that EndedAt serializes/deserializes correctly.
func TestLoadSessionState_WithEndedAt(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	// Test with EndedAt set
	endedAt := time.Now().Add(-time.Hour) // 1 hour ago
	state := &SessionState{
		SessionID:       "test-session-ended",
		BaseCommit:      "abc123def456",
		StartedAt:       time.Now().Add(-2 * time.Hour),
		EndedAt:         &endedAt,
		CheckpointCount: 5,
	}

	err = SaveSessionState(state)
	if err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	loaded, err := LoadSessionState("test-session-ended")
	if err != nil {
		t.Fatalf("LoadSessionState() error = %v", err)
	}
	if loaded == nil {
		t.Fatal("LoadSessionState() returned nil")
	}

	// Verify EndedAt was preserved
	if loaded.EndedAt == nil {
		t.Fatal("EndedAt was nil after load, expected non-nil")
	}
	if !loaded.EndedAt.Equal(endedAt) {
		t.Errorf("EndedAt = %v, want %v", *loaded.EndedAt, endedAt)
	}

	// Test with EndedAt nil (active session)
	stateActive := &SessionState{
		SessionID:       "test-session-active",
		BaseCommit:      "xyz789",
		StartedAt:       time.Now(),
		EndedAt:         nil,
		CheckpointCount: 1,
	}

	err = SaveSessionState(stateActive)
	if err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	loadedActive, err := LoadSessionState("test-session-active")
	if err != nil {
		t.Fatalf("LoadSessionState() error = %v", err)
	}
	if loadedActive == nil {
		t.Fatal("LoadSessionState() returned nil")
	}

	// Verify EndedAt remains nil
	if loadedActive.EndedAt != nil {
		t.Errorf("EndedAt = %v, want nil for active session", *loadedActive.EndedAt)
	}
}

// TestLoadSessionState_WithLastInteractionAt tests that LastInteractionAt serializes/deserializes correctly.
func TestLoadSessionState_WithLastInteractionAt(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	// Test with LastInteractionAt set
	lastInteraction := time.Now().Add(-5 * time.Minute)
	state := &SessionState{
		SessionID:         "test-session-interaction",
		BaseCommit:        "abc123def456",
		StartedAt:         time.Now().Add(-2 * time.Hour),
		LastInteractionAt: &lastInteraction,
		CheckpointCount:   3,
	}

	err = SaveSessionState(state)
	if err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	loaded, err := LoadSessionState("test-session-interaction")
	if err != nil {
		t.Fatalf("LoadSessionState() error = %v", err)
	}
	if loaded == nil {
		t.Fatal("LoadSessionState() returned nil")
	}

	// Verify LastInteractionAt was preserved
	if loaded.LastInteractionAt == nil {
		t.Fatal("LastInteractionAt was nil after load, expected non-nil")
	}
	if !loaded.LastInteractionAt.Equal(lastInteraction) {
		t.Errorf("LastInteractionAt = %v, want %v", *loaded.LastInteractionAt, lastInteraction)
	}

	// Test with LastInteractionAt nil (old session without this field)
	stateOld := &SessionState{
		SessionID:         "test-session-no-interaction",
		BaseCommit:        "xyz789",
		StartedAt:         time.Now(),
		LastInteractionAt: nil,
		CheckpointCount:   1,
	}

	err = SaveSessionState(stateOld)
	if err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	loadedOld, err := LoadSessionState("test-session-no-interaction")
	if err != nil {
		t.Fatalf("LoadSessionState() error = %v", err)
	}
	if loadedOld == nil {
		t.Fatal("LoadSessionState() returned nil")
	}

	// Verify LastInteractionAt remains nil
	if loadedOld.LastInteractionAt != nil {
		t.Errorf("LastInteractionAt = %v, want nil for old session", *loadedOld.LastInteractionAt)
	}
}

// TestLoadSessionState_PackageLevel_NonExistent tests loading a non-existent session.
func TestLoadSessionState_PackageLevel_NonExistent(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	loaded, err := LoadSessionState("nonexistent-session")
	if err != nil {
		t.Errorf("LoadSessionState() error = %v, want nil for nonexistent session", err)
	}
	if loaded != nil {
		t.Error("LoadSessionState() returned non-nil for nonexistent session")
	}
}

// TestManualCommitStrategy_SessionState_UsesPackageFunctions tests that ManualCommitStrategy
// methods delegate to the package-level functions.
func TestManualCommitStrategy_SessionState_UsesPackageFunctions(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	// Save using package-level function
	state := &SessionState{
		SessionID:       "cross-usage-test",
		BaseCommit:      "xyz789",
		StartedAt:       time.Now(),
		CheckpointCount: 2,
	}
	if err := SaveSessionState(state); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	// Load using ManualCommitStrategy method - should find the same state
	s := &ManualCommitStrategy{}
	loaded, err := s.loadSessionState("cross-usage-test")
	if err != nil {
		t.Fatalf("ManualCommitStrategy.loadSessionState() error = %v", err)
	}
	if loaded == nil {
		t.Fatal("ManualCommitStrategy.loadSessionState() returned nil")
	}

	// Verify via helper (loaded guaranteed non-nil after Fatal above)

	if loaded.SessionID != state.SessionID {
		t.Errorf("SessionID = %q, want %q", loaded.SessionID, state.SessionID)
	}

	// Save using ManualCommitStrategy method
	state2 := &SessionState{
		SessionID:       "cross-usage-test-2",
		BaseCommit:      "abc123",
		StartedAt:       time.Now(),
		CheckpointCount: 1,
	}
	if err := s.saveSessionState(state2); err != nil {
		t.Fatalf("ManualCommitStrategy.saveSessionState() error = %v", err)
	}

	// Load using package-level function - should find the state
	loaded2, err := LoadSessionState("cross-usage-test-2")
	if err != nil {
		t.Fatalf("LoadSessionState() error = %v", err)
	}
	if loaded2 == nil {
		t.Fatal("LoadSessionState() returned nil")
	}

	// Verify via direct comparison (loaded2 guaranteed non-nil after Fatal above)

	if loaded2.SessionID != state2.SessionID {
		t.Errorf("SessionID = %q, want %q", loaded2.SessionID, state2.SessionID)
	}
}
