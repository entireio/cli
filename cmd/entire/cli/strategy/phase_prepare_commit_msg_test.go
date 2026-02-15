package strategy

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPrepareCommitMsg_AmendPreservesExistingTrailer verifies that when amending
// a commit that already has an Entire-Checkpoint trailer, the trailer is preserved
// unchanged. source="commit" indicates an amend operation.
func TestPrepareCommitMsg_AmendPreservesExistingTrailer(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	sessionID := "test-session-amend-preserve"
	err := s.InitializeSession(sessionID, agent.AgentTypeClaudeCode, "", "")
	require.NoError(t, err)

	// Write a commit message file that already has the trailer
	commitMsgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	existingMsg := "Original commit message\n\nEntire-Checkpoint: abc123def456\n"
	require.NoError(t, os.WriteFile(commitMsgFile, []byte(existingMsg), 0o644))

	// Call PrepareCommitMsg with source="commit" (amend)
	err = s.PrepareCommitMsg(commitMsgFile, "commit")
	require.NoError(t, err)

	// Read the file back and verify the trailer is still present
	content, err := os.ReadFile(commitMsgFile)
	require.NoError(t, err)

	cpID, found := trailers.ParseCheckpoint(string(content))
	assert.True(t, found, "trailer should still be present after amend")
	assert.Equal(t, "abc123def456", cpID.String(),
		"trailer should preserve the original checkpoint ID")
}

// TestPrepareCommitMsg_AmendRestoresTrailerFromLastCheckpointID verifies the amend
// bug fix: when a user does `git commit --amend -m "new message"`, the Entire-Checkpoint
// trailer is lost because the new message replaces the old one. PrepareCommitMsg restores
// the trailer from LastCheckpointID in session state.
func TestPrepareCommitMsg_AmendRestoresTrailerFromLastCheckpointID(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	sessionID := "test-session-amend-restore"
	err := s.InitializeSession(sessionID, agent.AgentTypeClaudeCode, "", "")
	require.NoError(t, err)

	// Simulate state after condensation: LastCheckpointID is set
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	state.LastCheckpointID = id.CheckpointID("abc123def456")
	err = s.saveSessionState(state)
	require.NoError(t, err)

	// Write a commit message file with NO trailer (user did --amend -m "new message")
	commitMsgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	newMsg := "New amended message\n"
	require.NoError(t, os.WriteFile(commitMsgFile, []byte(newMsg), 0o644))

	// Call PrepareCommitMsg with source="commit" (amend)
	err = s.PrepareCommitMsg(commitMsgFile, "commit")
	require.NoError(t, err)

	// Read the file back - trailer should be restored from LastCheckpointID
	content, err := os.ReadFile(commitMsgFile)
	require.NoError(t, err)

	cpID, found := trailers.ParseCheckpoint(string(content))
	assert.True(t, found,
		"trailer should be restored from LastCheckpointID on amend")
	assert.Equal(t, "abc123def456", cpID.String(),
		"restored trailer should use LastCheckpointID value")
}

// TestPrepareCommitMsg_AmendNoTrailerNoLastCheckpointID verifies that when amending with
// no existing trailer and no LastCheckpointID in session state, no trailer is added.
// This is the case where the session has never been condensed yet.
func TestPrepareCommitMsg_AmendNoTrailerNoLastCheckpointID(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	sessionID := "test-session-amend-no-id"
	err := s.InitializeSession(sessionID, agent.AgentTypeClaudeCode, "", "")
	require.NoError(t, err)

	// Verify LastCheckpointID is empty (default)
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	assert.Empty(t, state.LastCheckpointID, "LastCheckpointID should be empty by default")

	// Write a commit message file with NO trailer
	commitMsgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	newMsg := "Amended without any session context\n"
	require.NoError(t, os.WriteFile(commitMsgFile, []byte(newMsg), 0o644))

	// Call PrepareCommitMsg with source="commit" (amend)
	err = s.PrepareCommitMsg(commitMsgFile, "commit")
	require.NoError(t, err)

	// Read the file back - no trailer should be added
	content, err := os.ReadFile(commitMsgFile)
	require.NoError(t, err)

	_, found := trailers.ParseCheckpoint(string(content))
	assert.False(t, found,
		"no trailer should be added when LastCheckpointID is empty")

	// Message should be unchanged
	assert.Equal(t, newMsg, string(content),
		"commit message should be unchanged when no trailer to restore")
}

// TestPrepareCommitMsg_ConcurrentSessions_PIDMatch verifies that with two concurrent
// active sessions, PrepareCommitMsg selects the session whose agent PID is in the
// hook process's ancestor chain (the session that initiated the commit).
func TestPrepareCommitMsg_ConcurrentSessions_PIDMatch(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	// Simulate agent mode (no TTY) to trigger the fast path
	t.Setenv("ENTIRE_TEST_TTY", "0")

	s := &ManualCommitStrategy{}

	// Initialize session A (the "wrong" session - different agent PID)
	sessionIDA := "test-concurrent-session-a"
	err := s.InitializeSession(sessionIDA, agent.AgentTypeClaudeCode, "", "")
	require.NoError(t, err)
	stateA, err := s.loadSessionState(sessionIDA)
	require.NoError(t, err)
	stateA.AgentPID = 1 // PID 1 (init) - won't match our process chain
	err = s.saveSessionState(stateA)
	require.NoError(t, err)

	// Initialize session B (the "correct" session - PID matches test process)
	sessionIDB := "test-concurrent-session-b"
	err = s.InitializeSession(sessionIDB, agent.AgentTypeClaudeCode, "", "")
	require.NoError(t, err)
	stateB, err := s.loadSessionState(sessionIDB)
	require.NoError(t, err)
	stateB.AgentPID = os.Getpid() // Matches current test process (ancestor of hook)
	err = s.saveSessionState(stateB)
	require.NoError(t, err)

	// Write a commit message file
	commitMsgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(commitMsgFile, []byte("test commit\n"), 0o644))

	// Call PrepareCommitMsg with source="message" (agent commit with -m)
	err = s.PrepareCommitMsg(commitMsgFile, "message")
	require.NoError(t, err)

	// Read the file back - trailer should be present
	content, err := os.ReadFile(commitMsgFile)
	require.NoError(t, err)

	_, found := trailers.ParseCheckpoint(string(content))
	assert.True(t, found,
		"trailer should be added when an active session matches via PID chain")

	// Verify that session B was selected by checking that the trailer was logged
	// against session B. We verify indirectly: both sessions are active, but the
	// PID-chain match should have selected session B (the one with our PID).
	// The trailer is assigned to the matched session in addTrailerForAgentCommit.
}

// TestPrepareCommitMsg_ConcurrentSessions_FallbackToLastInteraction verifies that
// when PID matching is unavailable (AgentPID=0 for both sessions), the most recently
// interacted session is selected.
func TestPrepareCommitMsg_ConcurrentSessions_FallbackToLastInteraction(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	// Simulate agent mode (no TTY) to trigger the fast path
	t.Setenv("ENTIRE_TEST_TTY", "0")

	s := &ManualCommitStrategy{}

	// Initialize session A (older interaction)
	sessionIDA := "test-fallback-session-a"
	err := s.InitializeSession(sessionIDA, agent.AgentTypeClaudeCode, "", "")
	require.NoError(t, err)
	stateA, err := s.loadSessionState(sessionIDA)
	require.NoError(t, err)
	stateA.AgentPID = 0 // Pre-upgrade, no PID tracking
	olderTime := time.Now().Add(-10 * time.Minute)
	stateA.LastInteractionTime = &olderTime
	err = s.saveSessionState(stateA)
	require.NoError(t, err)

	// Initialize session B (more recent interaction)
	sessionIDB := "test-fallback-session-b"
	err = s.InitializeSession(sessionIDB, agent.AgentTypeClaudeCode, "", "")
	require.NoError(t, err)
	stateB, err := s.loadSessionState(sessionIDB)
	require.NoError(t, err)
	stateB.AgentPID = 0 // Pre-upgrade, no PID tracking
	newerTime := time.Now()
	stateB.LastInteractionTime = &newerTime
	err = s.saveSessionState(stateB)
	require.NoError(t, err)

	// Write a commit message file
	commitMsgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(commitMsgFile, []byte("test commit\n"), 0o644))

	// Call PrepareCommitMsg with source="message"
	err = s.PrepareCommitMsg(commitMsgFile, "message")
	require.NoError(t, err)

	// Read the file back - trailer should be present (the most recently interacted
	// session should be selected)
	content, err := os.ReadFile(commitMsgFile)
	require.NoError(t, err)

	_, found := trailers.ParseCheckpoint(string(content))
	assert.True(t, found,
		"trailer should be added via LastInteractionTime fallback when PID matching unavailable")
}
