package strategy

import (
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/session"

	"github.com/go-git/go-git/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRecordPendingPushRemote_ActiveCommittedSession verifies that
// recordPendingPushRemote sets PendingPushRemote on ACTIVE_COMMITTED sessions
// that have a PendingCheckpointID.
func TestRecordPendingPushRemote_ActiveCommittedSession(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-push-active-committed"

	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseActiveCommitted
	state.PendingCheckpointID = "a1b2c3d4e5f6"
	require.NoError(t, s.saveSessionState(state))

	s.recordPendingPushRemote("origin")

	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	assert.Equal(t, "origin", state.PendingPushRemote,
		"PendingPushRemote should be set for ACTIVE_COMMITTED session with PendingCheckpointID")
}

// TestRecordPendingPushRemote_IdleSession_Skipped verifies that
// recordPendingPushRemote does not set PendingPushRemote on IDLE sessions.
func TestRecordPendingPushRemote_IdleSession_Skipped(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-push-idle-skipped"

	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseIdle
	state.PendingCheckpointID = "a1b2c3d4e5f6"
	require.NoError(t, s.saveSessionState(state))

	s.recordPendingPushRemote("origin")

	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	assert.Empty(t, state.PendingPushRemote,
		"PendingPushRemote should not be set for IDLE session")
}

// TestRecordPendingPushRemote_ActiveCommitted_NoPendingID_Skipped verifies that
// recordPendingPushRemote does not set PendingPushRemote on ACTIVE_COMMITTED
// sessions that have no PendingCheckpointID.
func TestRecordPendingPushRemote_ActiveCommitted_NoPendingID_Skipped(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-push-ac-no-pending-id"

	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseActiveCommitted
	state.PendingCheckpointID = ""
	require.NoError(t, s.saveSessionState(state))

	s.recordPendingPushRemote("origin")

	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	assert.Empty(t, state.PendingPushRemote,
		"PendingPushRemote should not be set when PendingCheckpointID is empty")
}

// TestRecordPendingPushRemote_NoSessions_Noop verifies that
// recordPendingPushRemote is a no-op when there are no sessions.
func TestRecordPendingPushRemote_NoSessions_Noop(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	// Should not panic or error with no sessions
	s.recordPendingPushRemote("origin")
}

// TestHandleTurnEnd_PushAfterCondensation verifies the full flow:
// PostCommit defers condensation (ACTIVE → ACTIVE_COMMITTED),
// recordPendingPushRemote records the remote, and HandleTurnEnd
// condenses and clears PendingPushRemote.
func TestHandleTurnEnd_PushAfterCondensation(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-turnend-push"

	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	// Simulate agent mid-turn
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseActive
	require.NoError(t, s.saveSessionState(state))

	// Agent commits → PostCommit transitions ACTIVE → ACTIVE_COMMITTED
	commitWithCheckpointTrailer(t, repo, dir, "d4e5f6a1b2c3")
	err = s.PostCommit()
	require.NoError(t, err)

	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	require.Equal(t, session.PhaseActiveCommitted, state.Phase)
	require.NotEmpty(t, state.PendingCheckpointID)

	// Agent pushes → recordPendingPushRemote records remote
	s.recordPendingPushRemote("origin")

	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	assert.Equal(t, "origin", state.PendingPushRemote)

	// Turn ends → HandleTurnEnd condenses and clears PendingPushRemote
	result := session.Transition(state.Phase, session.EventTurnEnd, session.TransitionContext{})
	remaining := session.ApplyCommonActions(state, result)

	err = s.HandleTurnEnd(state, remaining)
	require.NoError(t, err)

	// PendingPushRemote should be cleared after turn-end
	assert.Empty(t, state.PendingPushRemote,
		"PendingPushRemote should be cleared after turn-end push")

	// Condensation should have succeeded
	assert.Equal(t, 0, state.StepCount,
		"StepCount should be reset after condensation")
}

// TestHandleTurnEnd_PushClearedWhenNoNewContent verifies that PendingPushRemote
// is cleared even when handleTurnEndCondense returns early due to no new
// transcript content (the !hasNew path at the early return).
func TestHandleTurnEnd_PushClearedWhenNoNewContent(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-turnend-push-no-content"

	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	// Simulate agent mid-turn
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseActive
	require.NoError(t, s.saveSessionState(state))

	// Agent commits → PostCommit transitions ACTIVE → ACTIVE_COMMITTED
	commitWithCheckpointTrailer(t, repo, dir, "d4e5f6a1b2c3")
	err = s.PostCommit()
	require.NoError(t, err)

	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	require.Equal(t, session.PhaseActiveCommitted, state.Phase)

	// Record pending push remote
	s.recordPendingPushRemote("origin")
	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	require.Equal(t, "origin", state.PendingPushRemote)

	// Set CheckpointTranscriptStart = 2 so sessionHasNewContent returns false
	// (transcript has exactly 2 lines from setupSessionWithCheckpoint)
	state.CheckpointTranscriptStart = 2
	require.NoError(t, s.saveSessionState(state))

	// Turn ends → HandleTurnEnd should still clear PendingPushRemote
	result := session.Transition(state.Phase, session.EventTurnEnd, session.TransitionContext{})
	remaining := session.ApplyCommonActions(state, result)

	err = s.HandleTurnEnd(state, remaining)
	require.NoError(t, err)

	assert.Empty(t, state.PendingPushRemote,
		"PendingPushRemote should be cleared even when no new content to condense")
}

// TestInitializeSession_ClearsPendingPushRemote verifies that PendingPushRemote
// is cleared on new prompt start (handles Ctrl-C recovery case).
func TestInitializeSession_ClearsPendingPushRemote(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}
	sessionID := "test-init-clears-push-remote"

	// First call creates the session
	err := s.InitializeSession(sessionID, "Claude Code", "", "first prompt")
	require.NoError(t, err)

	// Simulate stale PendingPushRemote (e.g., from a Ctrl-C before turn-end)
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	state.PendingPushRemote = "origin"
	state.Phase = session.PhaseIdle
	now := time.Now()
	state.LastInteractionTime = &now
	require.NoError(t, s.saveSessionState(state))

	// Second call should clear PendingPushRemote
	err = s.InitializeSession(sessionID, "Claude Code", "", "second prompt")
	require.NoError(t, err)

	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	assert.Empty(t, state.PendingPushRemote,
		"PendingPushRemote should be cleared on new prompt start")
}
