package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

func recordDeleted(t *testing.T, cid id.CheckpointID) {
	t.Helper()
	gitdir.ClearCache()
	dir, err := gitdir.CommonDir(context.Background())
	require.NoError(t, err)
	require.NoError(t, checkpoint.NewDeletedCheckpoints(dir).Record(cid))
}

// After `entire checkpoint delete`, amending the commit that carried the
// deleted checkpoint must not keep its trailer: post-commit would condense into
// the dead ID and re-create (and re-queue) the checkpoint.
func TestPrepareCommitMsg_AmendStripsDeletedCheckpointTrailer(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	recordDeleted(t, id.MustCheckpointID("abc123def456"))

	s := &ManualCommitStrategy{}
	require.NoError(t, s.InitializeSession(context.Background(), "sess-amend-deleted", agent.AgentTypeClaudeCode, "", "", ""))

	commitMsgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(commitMsgFile, []byte("Original commit message\n\nEntire-Checkpoint: abc123def456\n"), 0o644))

	require.NoError(t, s.PrepareCommitMsg(context.Background(), commitMsgFile, "commit"))

	content, err := os.ReadFile(commitMsgFile)
	require.NoError(t, err)
	_, found := trailers.ParseCheckpoint(string(content))
	assert.False(t, found, "a deleted checkpoint's trailer must be removed on amend:\n%s", content)
	assert.Contains(t, string(content), "Original commit message")
}

// A trailer for a checkpoint that was not deleted is still preserved.
func TestPrepareCommitMsg_AmendKeepsTrailerNotOnDeletedList(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	recordDeleted(t, id.MustCheckpointID("ffffffffffff"))

	s := &ManualCommitStrategy{}
	commitMsgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(commitMsgFile, []byte("msg\n\nEntire-Checkpoint: abc123def456\n"), 0o644))

	require.NoError(t, s.PrepareCommitMsg(context.Background(), commitMsgFile, "commit"))

	content, err := os.ReadFile(commitMsgFile)
	require.NoError(t, err)
	cpID, found := trailers.ParseCheckpoint(string(content))
	require.True(t, found)
	assert.Equal(t, "abc123def456", cpID.String())
}

// A message supplied with -F that carries a deleted trailer is cleaned too.
func TestPrepareCommitMsg_MessageSourceStripsDeletedCheckpointTrailer(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	recordDeleted(t, id.MustCheckpointID("abc123def456"))

	s := &ManualCommitStrategy{}
	commitMsgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(commitMsgFile, []byte("msg\n\nEntire-Checkpoint: abc123def456\n"), 0o644))

	require.NoError(t, s.PrepareCommitMsg(context.Background(), commitMsgFile, commitSourceMessage))

	content, err := os.ReadFile(commitMsgFile)
	require.NoError(t, err)
	_, found := trailers.ParseCheckpoint(string(content))
	assert.False(t, found, "a deleted checkpoint's trailer must be removed:\n%s", content)
}

// Squash and redo inheritance re-add trailers from other commits; a deleted
// checkpoint's ID must not come back that way.
func TestWithoutDeletedCheckpoints(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	deleted := id.MustCheckpointID("abc123def456")
	kept := id.MustCheckpointID("ffffffffffff")

	assert.Equal(t, []id.CheckpointID{deleted, kept}, withoutDeletedCheckpoints(context.Background(), []id.CheckpointID{deleted, kept}),
		"nothing filtered while the list is empty")
	recordDeleted(t, deleted)
	assert.Equal(t, []id.CheckpointID{kept}, withoutDeletedCheckpoints(context.Background(), []id.CheckpointID{deleted, kept}))
}
