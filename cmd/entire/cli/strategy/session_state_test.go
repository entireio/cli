package strategy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/require"
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
	err = SaveSessionState(context.Background(), state)
	if err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	// Load using package-level function
	loaded, err := LoadSessionState(context.Background(), "test-session-pkg-123")
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

	err = SaveSessionState(context.Background(), state)
	if err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	loaded, err := LoadSessionState(context.Background(), "test-session-ended")
	if err != nil {
		t.Fatalf("LoadSessionState() error = %v", err)
	}
	require.NotNil(t, loaded, "LoadSessionState() returned nil")

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

	err = SaveSessionState(context.Background(), stateActive)
	if err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	loadedActive, err := LoadSessionState(context.Background(), "test-session-active")
	if err != nil {
		t.Fatalf("LoadSessionState() error = %v", err)
	}
	require.NotNil(t, loadedActive, "LoadSessionState() returned nil")

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

	err = SaveSessionState(context.Background(), state)
	if err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	loaded, err := LoadSessionState(context.Background(), "test-session-interaction")
	if err != nil {
		t.Fatalf("LoadSessionState() error = %v", err)
	}
	require.NotNil(t, loaded, "LoadSessionState() returned nil")

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

	err = SaveSessionState(context.Background(), stateOld)
	if err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	loadedOld, err := LoadSessionState(context.Background(), "test-session-no-interaction")
	if err != nil {
		t.Fatalf("LoadSessionState() error = %v", err)
	}
	require.NotNil(t, loadedOld, "LoadSessionState() returned nil")

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

	loaded, err := LoadSessionState(context.Background(), "nonexistent-session")
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
	if err := SaveSessionState(context.Background(), state); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	// Load using ManualCommitStrategy method - should find the same state
	s := &ManualCommitStrategy{}
	loaded, err := s.loadSessionState(context.Background(), "cross-usage-test")
	if err != nil {
		t.Fatalf("ManualCommitStrategy.loadSessionState() error = %v", err)
	}
	require.NotNil(t, loaded, "ManualCommitStrategy.loadSessionState() returned nil")

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
	if err := s.saveSessionState(context.Background(), state2); err != nil {
		t.Fatalf("ManualCommitStrategy.saveSessionState() error = %v", err)
	}

	// Load using package-level function - should find the state
	loaded2, err := LoadSessionState(context.Background(), "cross-usage-test-2")
	if err != nil {
		t.Fatalf("LoadSessionState() error = %v", err)
	}
	require.NotNil(t, loaded2, "LoadSessionState() returned nil")

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
	resolvedDir, err := paths.WorktreeRoot(context.Background())
	if err != nil {
		t.Fatalf("paths.WorktreeRoot() error = %v", err)
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

	if err := SaveSessionState(context.Background(), otherWorktree); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}
	if err := SaveSessionState(context.Background(), currentWorktree); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	// FindMostRecentSession should return the current worktree's session,
	// not the other worktree's session (even though it's more recent).
	result := FindMostRecentSession(context.Background())
	if result != "current-worktree-session" {
		t.Errorf("FindMostRecentSession(context.Background()) = %q, want %q (should prefer current worktree)",
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

	if err := SaveSessionState(context.Background(), otherWorktree); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	// Should fall back to the only available session since none match current worktree
	result := FindMostRecentSession(context.Background())
	if result != "only-session" {
		t.Errorf("FindMostRecentSession(context.Background()) = %q, want %q (should fall back when no worktree match)",
			result, "only-session")
	}

	// Cleanup
	if err := os.Remove(dir + "/.git/entire-sessions/only-session.json"); err != nil && !os.IsNotExist(err) {
		t.Logf("cleanup warning: %v", err)
	}
}

// errorActionHandler returns an error from HandleCondense to test
// that TransitionAndLog propagates handler errors while still applying the phase transition.
type errorActionHandler struct {
	session.NoOpActionHandler
}

func (errorActionHandler) HandleCondense(_ *session.State) error {
	return errors.New("test condense error")
}

// TestTransitionAndLog_ReturnsHandlerError verifies that TransitionAndLog
// applies the phase transition even when the handler returns an error,
// and propagates that error to the caller.
func TestTransitionAndLog_ReturnsHandlerError(t *testing.T) {
	t.Parallel()

	state := &SessionState{
		SessionID: "test-error-handler",
		Phase:     session.PhaseIdle,
	}

	// IDLE + GitCommit → IDLE with ActionCondense.
	// The handler will fail on ActionCondense, but the phase should still be IDLE.
	err := TransitionAndLog(context.Background(), state, session.EventGitCommit, session.TransitionContext{}, &errorActionHandler{})

	if state.Phase != session.PhaseIdle {
		t.Errorf("Phase = %q, want %q (should transition despite handler error)", state.Phase, session.PhaseIdle)
	}
	if err == nil {
		t.Error("TransitionAndLog() should return handler error")
	}
}

// TestLoadSessionState_DeletesStaleSession tests that LoadSessionState returns (nil, nil)
// for a stale session and deletes the file from disk.
func TestLoadSessionState_DeletesStaleSession(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	// Create a stale session (ended >2wk ago)
	staleInteracted := time.Now().Add(-2 * 7 * 24 * time.Hour)
	state := &SessionState{
		SessionID:           "stale-load-test",
		BaseCommit:          "abc123def456",
		StartedAt:           time.Now().Add(-3 * 7 * 24 * time.Hour),
		LastInteractionTime: &staleInteracted,
		StepCount:           5,
	}

	err = SaveSessionState(context.Background(), state)
	if err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	// Verify file exists before load
	stateFile, err := sessionStateFile(context.Background(), "stale-load-test")
	if err != nil {
		t.Fatalf("sessionStateFile() error = %v", err)
	}
	if _, err := os.Stat(stateFile); err != nil {
		t.Fatalf("state file should exist before load: %v", err)
	}

	// Load should return (nil, nil) for stale session
	loaded, err := LoadSessionState(context.Background(), "stale-load-test")
	if err != nil {
		t.Errorf("LoadSessionState() error = %v, want nil for stale session", err)
	}
	if loaded != nil {
		t.Error("LoadSessionState() returned non-nil for stale session")
	}

	// File should be deleted from disk
	if _, err := os.Stat(stateFile); !os.IsNotExist(err) {
		t.Error("stale session file should be deleted after LoadSessionState()")
	}
}

// --- Model hint file tests ---

func TestStoreModelHint_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}
	t.Chdir(dir)

	ctx := context.Background()
	sessionID := "2026-01-01-hint-roundtrip"

	err = StoreModelHint(ctx, sessionID, "claude-sonnet-4-20250514")
	if err != nil {
		t.Fatalf("StoreModelHint() error = %v", err)
	}

	got := LoadModelHint(ctx, sessionID)
	if got != "claude-sonnet-4-20250514" {
		t.Errorf("LoadModelHint() = %q, want %q", got, "claude-sonnet-4-20250514")
	}
}

func TestStoreModelHint_EmptyModel_NoOp(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}
	t.Chdir(dir)

	ctx := context.Background()
	sessionID := "2026-01-01-hint-empty"

	err = StoreModelHint(ctx, sessionID, "")
	if err != nil {
		t.Fatalf("StoreModelHint() error = %v", err)
	}

	// No file should have been created
	stateDir, sdErr := getSessionStateDir(ctx)
	if sdErr != nil {
		t.Fatalf("getSessionStateDir() error = %v", sdErr)
	}
	hintPath := stateDir + "/" + sessionID + ".model"
	if _, statErr := os.Stat(hintPath); !os.IsNotExist(statErr) {
		t.Error("StoreModelHint with empty model should not create a file")
	}
}

func TestLoadModelHint_NoFile_ReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}
	t.Chdir(dir)

	got := LoadModelHint(context.Background(), "2026-01-01-nonexistent")
	if got != "" {
		t.Errorf("LoadModelHint() = %q, want empty string for missing file", got)
	}
}

func TestStoreModelHint_InvalidSessionID_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}
	t.Chdir(dir)

	err = StoreModelHint(context.Background(), "../../../etc/passwd", "model")
	if err == nil {
		t.Error("StoreModelHint() should return error for invalid session ID")
	}
}

func TestLoadModelHint_InvalidSessionID_ReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}
	t.Chdir(dir)

	got := LoadModelHint(context.Background(), "../../../etc/passwd")
	if got != "" {
		t.Errorf("LoadModelHint() = %q, want empty for invalid session ID", got)
	}
}

func TestLoadModelHint_TrimsWhitespace(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}
	t.Chdir(dir)

	ctx := context.Background()
	sessionID := "2026-01-01-hint-whitespace"

	// Write hint with trailing newline (simulating manual edit)
	stateDir, sdErr := getSessionStateDir(ctx)
	if sdErr != nil {
		t.Fatalf("getSessionStateDir() error = %v", sdErr)
	}
	if mkErr := os.MkdirAll(stateDir, 0o750); mkErr != nil {
		t.Fatalf("MkdirAll() error = %v", mkErr)
	}
	hintPath := stateDir + "/" + sessionID + ".model"
	if wErr := os.WriteFile(hintPath, []byte("claude-opus-4-6\n"), 0o600); wErr != nil {
		t.Fatalf("WriteFile() error = %v", wErr)
	}

	got := LoadModelHint(ctx, sessionID)
	if got != "claude-opus-4-6" {
		t.Errorf("LoadModelHint() = %q, want %q (should trim whitespace)", got, "claude-opus-4-6")
	}
}

// --- Agent type hint file tests ---

func TestStoreAgentTypeHint_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	t.Chdir(dir)

	ctx := context.Background()
	sessionID := "2026-01-01-agent-roundtrip"

	created, err := StoreAgentTypeHint(ctx, sessionID, agent.AgentTypeCursor)
	require.NoError(t, err)
	require.True(t, created, "first call must report it created the hint")

	got := LoadAgentTypeHint(ctx, sessionID)
	require.Equal(t, agent.AgentTypeCursor, got)
}

func TestStoreAgentTypeHint_FirstWriterWins(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	t.Chdir(dir)

	ctx := context.Background()
	sessionID := "2026-01-01-agent-firstwriter"

	// Cursor claims the session first.
	created, err := StoreAgentTypeHint(ctx, sessionID, agent.AgentTypeCursor)
	require.NoError(t, err)
	require.True(t, created)

	// Claude Code's hook fires next (concurrent forwarded-hook scenario).
	// Should be a no-op — does not overwrite the existing hint.
	created, err = StoreAgentTypeHint(ctx, sessionID, agent.AgentTypeClaudeCode)
	require.NoError(t, err)
	require.False(t, created, "second call must report it did not create the hint")

	got := LoadAgentTypeHint(ctx, sessionID)
	require.Equal(t, agent.AgentTypeCursor, got, "first writer's hint must persist")
}

func TestStoreAgentTypeHint_EmptyOrUnknown_NoOp(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	t.Chdir(dir)

	ctx := context.Background()

	for _, tc := range []struct {
		sid string
		at  types.AgentType
	}{
		{"2026-01-01-empty", ""},
		{"2026-01-01-unknown", agent.AgentTypeUnknown},
	} {
		created, hErr := StoreAgentTypeHint(ctx, tc.sid, tc.at)
		require.NoError(t, hErr)
		require.False(t, created, "empty/Unknown must report created=false")
	}

	stateDir, sdErr := getSessionStateDir(ctx)
	require.NoError(t, sdErr)

	for _, sid := range []string{"2026-01-01-empty", "2026-01-01-unknown"} {
		hintPath := filepath.Join(stateDir, sid+".agent")
		_, statErr := os.Stat(hintPath)
		require.True(t, os.IsNotExist(statErr), "no hint file should be created for empty/Unknown agent type")
	}
}

func TestLoadAgentTypeHint_NoFile_ReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	t.Chdir(dir)

	got := LoadAgentTypeHint(context.Background(), "2026-01-01-nonexistent")
	require.Empty(t, string(got))
}

func TestStoreAgentTypeHint_InvalidSessionID_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	t.Chdir(dir)

	_, err = StoreAgentTypeHint(context.Background(), "../../../etc/passwd", agent.AgentTypeCursor)
	require.Error(t, err)
}

func TestClaimSessionStartBanner_FirstWriterWins(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	t.Chdir(dir)

	ctx := context.Background()
	sessionID := "2026-01-01-banner-claim"

	claimed, err := ClaimSessionStartBanner(ctx, sessionID)
	require.NoError(t, err)
	require.True(t, claimed, "first call must win the banner claim")

	claimed, err = ClaimSessionStartBanner(ctx, sessionID)
	require.NoError(t, err)
	require.False(t, claimed, "subsequent calls must report the banner already claimed")
}

func TestClaimSessionStartBanner_InvalidSessionID_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	t.Chdir(dir)

	_, err = ClaimSessionStartBanner(context.Background(), "../../../etc/passwd")
	require.Error(t, err)
}

func TestClearSessionState_RemovesBannerMarker(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	t.Chdir(dir)

	ctx := context.Background()
	sessionID := "2026-01-01-clear-banner"

	_, err = ClaimSessionStartBanner(ctx, sessionID)
	require.NoError(t, err)
	require.NoError(t, ClearSessionState(ctx, sessionID))

	// After clear, the marker is gone — the next claim wins again.
	claimed, err := ClaimSessionStartBanner(ctx, sessionID)
	require.NoError(t, err)
	require.True(t, claimed, "ClearSessionState should remove the banner marker")
}

func TestClearSessionState_RemovesAgentHint(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	t.Chdir(dir)

	ctx := context.Background()
	sessionID := "2026-01-01-clear-agent-hint"

	_, err = StoreAgentTypeHint(ctx, sessionID, agent.AgentTypeCursor)
	require.NoError(t, err)
	require.NoError(t, ClearSessionState(ctx, sessionID))

	got := LoadAgentTypeHint(ctx, sessionID)
	require.Empty(t, string(got))
}

func TestClearSessionState_RemovesHintFile(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}
	t.Chdir(dir)

	ctx := context.Background()
	sessionID := "2026-01-01-clear-hint"

	// Create both state and hint files
	state := &SessionState{
		SessionID:  sessionID,
		BaseCommit: "abc123",
		StartedAt:  time.Now(),
	}
	if sErr := SaveSessionState(ctx, state); sErr != nil {
		t.Fatalf("SaveSessionState() error = %v", sErr)
	}
	if sErr := StoreModelHint(ctx, sessionID, "some-model"); sErr != nil {
		t.Fatalf("StoreModelHint() error = %v", sErr)
	}

	// Clear should remove both
	if cErr := ClearSessionState(ctx, sessionID); cErr != nil {
		t.Fatalf("ClearSessionState() error = %v", cErr)
	}

	stateDir, sdErr := getSessionStateDir(ctx)
	if sdErr != nil {
		t.Fatalf("getSessionStateDir() error = %v", sdErr)
	}
	matches, err := filepath.Glob(filepath.Join(stateDir, sessionID+".*"))
	if err != nil {
		t.Fatalf("filepath.Glob() error = %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("expected no files for session after clear, found: %v", matches)
	}
}

func TestClearSessionState_RemovesOrphanedHintFile(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}
	t.Chdir(dir)

	ctx := context.Background()
	sessionID := "2026-01-01-orphan-hint"

	// Only create hint file (no state file)
	if sErr := StoreModelHint(ctx, sessionID, "orphan-model"); sErr != nil {
		t.Fatalf("StoreModelHint() error = %v", sErr)
	}

	// Clear should succeed and remove the hint file
	if cErr := ClearSessionState(ctx, sessionID); cErr != nil {
		t.Fatalf("ClearSessionState() error = %v", cErr)
	}

	stateDir, sdErr := getSessionStateDir(ctx)
	if sdErr != nil {
		t.Fatalf("getSessionStateDir() error = %v", sdErr)
	}
	matches, err := filepath.Glob(filepath.Join(stateDir, sessionID+".*"))
	if err != nil {
		t.Fatalf("filepath.Glob() error = %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("expected no files for session after clear, found: %v", matches)
	}
}
