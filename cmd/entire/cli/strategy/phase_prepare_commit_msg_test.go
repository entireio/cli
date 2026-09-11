package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
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
	err := s.InitializeSession(context.Background(), sessionID, agent.AgentTypeClaudeCode, "", "", "")
	require.NoError(t, err)

	// Write a commit message file that already has the trailer
	commitMsgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	existingMsg := "Original commit message\n\nEntire-Checkpoint: abc123def456\n"
	require.NoError(t, os.WriteFile(commitMsgFile, []byte(existingMsg), 0o644))

	// Call PrepareCommitMsg with source="commit" (amend)
	err = s.PrepareCommitMsg(context.Background(), commitMsgFile, "commit")
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
	err := s.InitializeSession(context.Background(), sessionID, agent.AgentTypeClaudeCode, "", "", "")
	require.NoError(t, err)

	// Simulate state after condensation: LastCheckpointID is set
	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	state.LastCheckpointID = id.CheckpointID("abc123def456")
	err = s.saveSessionState(context.Background(), state)
	require.NoError(t, err)

	// Write a commit message file with NO trailer (user did --amend -m "new message")
	commitMsgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	newMsg := "New amended message\n"
	require.NoError(t, os.WriteFile(commitMsgFile, []byte(newMsg), 0o644))

	// Call PrepareCommitMsg with source="commit" (amend)
	err = s.PrepareCommitMsg(context.Background(), commitMsgFile, "commit")
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
	err := s.InitializeSession(context.Background(), sessionID, agent.AgentTypeClaudeCode, "", "", "")
	require.NoError(t, err)

	// Verify LastCheckpointID is empty (default)
	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	assert.Empty(t, state.LastCheckpointID, "LastCheckpointID should be empty by default")

	// Write a commit message file with NO trailer
	commitMsgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	newMsg := "Amended without any session context\n"
	require.NoError(t, os.WriteFile(commitMsgFile, []byte(newMsg), 0o644))

	// Call PrepareCommitMsg with source="commit" (amend)
	err = s.PrepareCommitMsg(context.Background(), commitMsgFile, "commit")
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

// TestPrepareCommitMsg_StaleTaskRecordOnly_NoTrailerButStillCondensable pins the
// split between "condensable" and "may claim a trailer". An IDLE session whose
// only content is a task record past idleWithTaskContent's 24h bound used to be
// stamped by the slow path (sessionHasNewContent says yes on HasTaskContent with
// no bound) and then refused by PostCommit's overlap check, leaving
// Entire-Checkpoint pointing at a checkpoint nothing ever wrote. The trailer is
// now bounded, but sessionHasNewContent deliberately is not: the record stays
// recognized content so the session's transcript-so-far still materializes at
// its eventual condensation rather than being stranded.
func TestPrepareCommitMsg_StaleTaskRecordOnly_NoTrailerButStillCondensable(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	ctx := context.Background()
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	worktreePath, err := paths.WorktreeRoot(ctx)
	require.NoError(t, err)
	worktreeID, err := paths.GetWorktreeID(worktreePath)
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)

	// Sole content: a record started 30h ago. No transcript, no files, no steps.
	state := &SessionState{
		SessionID:    "test-session-stale-record-only",
		BaseCommit:   head.Hash().String(),
		WorktreePath: worktreePath,
		WorktreeID:   worktreeID,
		StartedAt:    time.Now().Add(-30 * time.Hour),
		Phase:        session.PhaseIdle,
		AgentType:    agent.AgentTypeClaudeCode,
		TaskRecords: []session.TaskRecord{
			{ToolUseID: "toolu_stale1", AgentID: "agent-stale1", StartedAt: time.Now().Add(-30 * time.Hour)},
		},
	}
	require.NoError(t, s.saveSessionState(ctx, state))

	commitMsgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(commitMsgFile, []byte("unrelated human commit\n"), 0o644))
	require.NoError(t, s.PrepareCommitMsg(ctx, commitMsgFile, ""))

	content, err := os.ReadFile(commitMsgFile)
	require.NoError(t, err)
	_, found := trailers.ParseCheckpoint(string(content))
	assert.False(t, found,
		"a session whose only content is a stale task record must not claim a trailer")

	hasNew, err := s.sessionHasNewContent(ctx, repo, state, contentCheckOpts{})
	require.NoError(t, err)
	assert.True(t, hasNew,
		"the stale record must remain condensable content so it is not stranded unmaterialized")
}
