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

// A deleted ID is stripped however its trailer is spaced, and the report
// matches what was written: a mention outside the trailer block is neither
// removed nor reported.
func TestStripDeletedCheckpointTrailers_NonCanonicalSpelling(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	recordDeleted(t, id.MustCheckpointID("abc123def456"))

	tests := []struct {
		name         string
		msg          string
		wantStripped bool
		wantContent  string
	}{
		{name: "no space", msg: "msg\n\nEntire-Checkpoint:abc123def456\n", wantStripped: true, wantContent: "msg\n"},
		{name: "tab", msg: "msg\n\nEntire-Checkpoint:\tabc123def456\n", wantStripped: true, wantContent: "msg\n"},
		{name: "cherry-pick -x note", msg: "msg\n\nbody\n\nEntire-Checkpoint: abc123def456\n(cherry picked from commit 0123456789abcdef0123456789abcdef01234567)\n", wantStripped: true, wantContent: "msg\n\nbody\n\n(cherry picked from commit 0123456789abcdef0123456789abcdef01234567)\n"},
		{name: "skip-ci marker", msg: "msg\n\nEntire-Checkpoint: abc123def456\n[skip ci]\n", wantStripped: true, wantContent: "msg\n\n[skip ci]\n"},
		{name: "body mention", msg: "msg\n\nEntire-Checkpoint: abc123def456 is gone, so\nthis prose stays.\n", wantContent: "msg\n\nEntire-Checkpoint: abc123def456 is gone, so\nthis prose stays.\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			commitMsgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
			require.NoError(t, os.WriteFile(commitMsgFile, []byte(tt.msg), 0o644))

			assert.Equal(t, tt.wantStripped, stripDeletedCheckpointTrailers(context.Background(), commitMsgFile))
			content, err := os.ReadFile(commitMsgFile)
			require.NoError(t, err)
			assert.Equal(t, tt.wantContent, string(content))
		})
	}
}

// git commit --amend of a `cherry-pick -x` commit: the deleted trailer sits
// above git's note and must still go, or the amend keeps the dead ID.
func TestPrepareCommitMsg_AmendStripsDeletedTrailerAboveCherryPickNote(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	recordDeleted(t, id.MustCheckpointID("abc123def456"))

	s := &ManualCommitStrategy{}
	commitMsgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(commitMsgFile, []byte("subject\n\nbody\n\nEntire-Checkpoint: abc123def456\n(cherry picked from commit 0123456789abcdef0123456789abcdef01234567)\n"), 0o644))

	require.NoError(t, s.PrepareCommitMsg(context.Background(), commitMsgFile, "commit"))

	content, err := os.ReadFile(commitMsgFile)
	require.NoError(t, err)
	assert.NotContains(t, string(content), "abc123def456")
	assert.Contains(t, string(content), "(cherry picked from commit")
}

// An amend whose message still mentions a deleted ID in prose (not a whole
// trailer line, so it is not stripped) must not treat that ID as the commit's
// live trailer: the session's live checkpoint is restored as usual.
func TestPrepareCommitMsg_AmendIgnoresDeletedIDWhenPreserving(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	dead := id.MustCheckpointID("abc123def456")
	live := id.MustCheckpointID("bcd234ef5678")
	recordDeleted(t, dead)

	s := &ManualCommitStrategy{}
	require.NoError(t, s.InitializeSession(context.Background(), "sess-amend-live", agent.AgentTypeClaudeCode, "", "", ""))
	require.NoError(t, MutateSessionState(context.Background(), "sess-amend-live", func(state *SessionState) error {
		state.LastCheckpointID = live
		return nil
	}))

	commitMsgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(commitMsgFile, []byte("subject\n\nEntire-Checkpoint: abc123def456 was dropped on purpose.\n"), 0o644))

	require.NoError(t, s.PrepareCommitMsg(context.Background(), commitMsgFile, "commit"))

	content, err := os.ReadFile(commitMsgFile)
	require.NoError(t, err)
	assert.Contains(t, trailers.ParseAllCheckpoints(string(content)), live, "the live checkpoint is restored:\n%s", content)
}
