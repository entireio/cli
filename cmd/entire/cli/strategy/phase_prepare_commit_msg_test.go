package strategy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
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
// restore: when a user does `git commit --amend -m "new message"`, the Entire-Checkpoint
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
	state.LastCheckpointID = "abc123def456"
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

// TestPrepareCommitMsg_AmendNoTrailerNoLastID verifies that when amending with
// no existing trailer and no LastCheckpointID in session state, no trailer is added.
// This is the case where the session has never been condensed yet.
func TestPrepareCommitMsg_AmendNoTrailerNoLastID(t *testing.T) {
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
	assert.True(t, state.LastCheckpointID.IsEmpty(), "LastCheckpointID should be empty by default")

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
