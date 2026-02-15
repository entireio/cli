package strategy

import (
	"os"
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
		SessionID:                 "test-session-pkg-123",
		BaseCommit:                "abc123def456",
		StartedAt:                 time.Now(),
		StepCount:                 3,
		CheckpointTranscriptStart: 150,
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
	if loaded.StepCount != expected.StepCount {
		t.Errorf("StepCount = %d, want %d", loaded.StepCount, expected.StepCount)
	}
	if loaded.CheckpointTranscriptStart != expected.CheckpointTranscriptStart {
		t.Errorf("CheckpointTranscriptStart = %d, want %d", loaded.CheckpointTranscriptStart, expected.CheckpointTranscriptStart)
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
		SessionID:  "test-session-ended",
		BaseCommit: "abc123def456",
		StartedAt:  time.Now().Add(-2 * time.Hour),
		EndedAt:    &endedAt,
		StepCount:  5,
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
		SessionID:  "test-session-active",
		BaseCommit: "xyz789",
		StartedAt:  time.Now(),
		EndedAt:    nil,
		StepCount:  1,
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

// TestLoadSessionState_WithLastInteractionTime tests that LastInteractionTime serializes/deserializes correctly.
func TestLoadSessionState_WithLastInteractionTime(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	// Test with LastInteractionTime set
	lastInteraction := time.Now().Add(-5 * time.Minute)
	state := &SessionState{
		SessionID:           "test-session-interaction",
		BaseCommit:          "abc123def456",
		StartedAt:           time.Now().Add(-2 * time.Hour),
		LastInteractionTime: &lastInteraction,
		StepCount:           3,
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

	// Verify LastInteractionTime was preserved
	if loaded.LastInteractionTime == nil {
		t.Fatal("LastInteractionTime was nil after load, expected non-nil")
	}
	if !loaded.LastInteractionTime.Equal(lastInteraction) {
		t.Errorf("LastInteractionTime = %v, want %v", *loaded.LastInteractionTime, lastInteraction)
	}

	// Test with LastInteractionTime nil (old session without this field)
	stateOld := &SessionState{
		SessionID:           "test-session-no-interaction",
		BaseCommit:          "xyz789",
		StartedAt:           time.Now(),
		LastInteractionTime: nil,
		StepCount:           1,
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

	// Verify LastInteractionTime remains nil
	if loadedOld.LastInteractionTime != nil {
		t.Errorf("LastInteractionTime = %v, want nil for old session", *loadedOld.LastInteractionTime)
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
		SessionID:  "cross-usage-test",
		BaseCommit: "xyz789",
		StartedAt:  time.Now(),
		StepCount:  2,
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
		SessionID:  "cross-usage-test-2",
		BaseCommit: "abc123",
		StartedAt:  time.Now(),
		StepCount:  1,
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

// TestFindMostRecentSession_FiltersByWorktree tests that FindMostRecentSession
// returns sessions from the current worktree, not from other worktrees.
func TestFindMostRecentSession_FiltersByWorktree(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	// Get the resolved worktree path (git resolves symlinks, e.g. /var → /private/var on macOS)
	resolvedDir, err := GetWorktreePath()
	if err != nil {
		t.Fatalf("GetWorktreePath() error = %v", err)
	}

	older := time.Now().Add(-1 * time.Hour)
	newer := time.Now()

	// Session from a different worktree (more recent)
	otherWorktree := &SessionState{
		SessionID:           "other-worktree-session",
		BaseCommit:          "abc1234",
		WorktreePath:        "/some/other/worktree",
		StartedAt:           newer,
		LastInteractionTime: &newer,
		Phase:               "idle",
	}

	// Session from current worktree (older)
	currentWorktree := &SessionState{
		SessionID:           "current-worktree-session",
		BaseCommit:          "xyz7890",
		WorktreePath:        resolvedDir, // matches current worktree
		StartedAt:           older,
		LastInteractionTime: &older,
		Phase:               "idle",
	}

	if err := SaveSessionState(otherWorktree); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}
	if err := SaveSessionState(currentWorktree); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	// FindMostRecentSession should return the current worktree's session,
	// not the other worktree's session (even though it's more recent).
	result := FindMostRecentSession()
	if result != "current-worktree-session" {
		t.Errorf("FindMostRecentSession() = %q, want %q (should prefer current worktree)",
			result, "current-worktree-session")
	}
}

// TestFindMostRecentSession_FallsBackWhenNoWorktreeMatch tests that
// FindMostRecentSession falls back to all sessions when none match the current worktree.
func TestFindMostRecentSession_FallsBackWhenNoWorktreeMatch(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	newer := time.Now()

	// Session from a different worktree only (no sessions for current worktree)
	otherWorktree := &SessionState{
		SessionID:           "only-session",
		BaseCommit:          "abc1234",
		WorktreePath:        "/some/other/worktree",
		StartedAt:           newer,
		LastInteractionTime: &newer,
		Phase:               "idle",
	}

	if err := SaveSessionState(otherWorktree); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	// Should fall back to the only available session since none match current worktree
	result := FindMostRecentSession()
	if result != "only-session" {
		t.Errorf("FindMostRecentSession() = %q, want %q (should fall back when no worktree match)",
			result, "only-session")
	}

	// Cleanup
	if err := os.Remove(dir + "/.git/entire-sessions/only-session.json"); err != nil && !os.IsNotExist(err) {
		t.Logf("cleanup warning: %v", err)
	}
}

// TestFindMostRecentSession_ConcurrentSameWorktree verifies that with multiple
// concurrent sessions in the same worktree, FindMostRecentSession returns the
// session with the most recent LastInteractionTime. This is the deterministic
// behavior that replaces the previous nondeterministic os.ReadDir order.
func TestFindMostRecentSession_ConcurrentSameWorktree(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	resolvedDir, err := GetWorktreePath()
	if err != nil {
		t.Fatalf("GetWorktreePath() error = %v", err)
	}

	older := time.Now().Add(-10 * time.Minute)
	newer := time.Now()

	// Session A: older interaction
	sessionA := &SessionState{
		SessionID:           "concurrent-session-a",
		BaseCommit:          "abc1234",
		WorktreePath:        resolvedDir,
		StartedAt:           older,
		LastInteractionTime: &older,
		Phase:               "active",
	}

	// Session B: more recent interaction
	sessionB := &SessionState{
		SessionID:           "concurrent-session-b",
		BaseCommit:          "abc1234",
		WorktreePath:        resolvedDir,
		StartedAt:           newer,
		LastInteractionTime: &newer,
		Phase:               "active",
	}

	if err := SaveSessionState(sessionA); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}
	if err := SaveSessionState(sessionB); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	// FindMostRecentSession should deterministically return session B
	// (most recently interacted), not depend on os.ReadDir ordering.
	result := FindMostRecentSession()
	if result != "concurrent-session-b" {
		t.Errorf("FindMostRecentSession() = %q, want %q (should select most recently interacted session)",
			result, "concurrent-session-b")
	}
}
