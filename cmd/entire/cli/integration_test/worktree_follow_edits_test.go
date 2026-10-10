//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// An agent launched in the main checkout fires every hook there, but may edit
// a linked worktree by absolute path. The session moves to the worktree its
// turn edited, so the files are recorded there and a later commit of them in
// that worktree links; a turn back at home moves it back.
func TestTurnEditingALinkedWorktree_MovesTheSessionThere(t *testing.T) {
	t.Parallel()
	parent := NewRepoWithCommit(t)
	linked := worktreeEnv(t, parent, "edits")
	sess := parent.NewSession()

	turn := func(prompt, dir, file string) {
		t.Helper()
		require.NoError(t, parent.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, prompt, sess.TranscriptPath))
		path := filepath.Join(dir, file)
		writeFileAt(t, path, "agent content of "+file+"\n")
		sess.CreateTranscript(prompt, []FileChange{{Path: path, Content: "x"}})
		require.NoError(t, parent.SimulateStop(sess.ID, sess.TranscriptPath))
	}

	turn("Edit the linked worktree", linked.RepoDir, "wt.txt")
	state, err := parent.GetSessionState(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, resolved(t, linked.RepoDir), resolved(t, state.WorktreePath), "the session should move to the worktree its turn edited")
	assert.Contains(t, state.FilesTouched, "wt.txt")
	assert.NotEmpty(t, state.TouchedFileHashes["wt.txt"])

	turn("Edit the main checkout", parent.RepoDir, "home.txt")
	state, err = parent.GetSessionState(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, resolved(t, linked.RepoDir), resolved(t, state.WorktreePath), "with wt.txt still uncommitted the session must stay")

	// Once its worktree work is committed there, the next home turn moves it back.
	linked.GitCommitWithHooks("Commit the agent's file", "wt.txt")
	turn("Edit the main checkout again", parent.RepoDir, "home2.txt")
	state, err = parent.GetSessionState(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, resolved(t, parent.RepoDir), resolved(t, state.WorktreePath), "a turn back at home should move the session back")
	assert.Contains(t, state.FilesTouched, "home2.txt")
}

// A turn recorded in the linked worktree also records what git status shows
// there: a tracked file the agent deleted without the transcript naming it.
func TestTurnInALinkedWorktree_RecordsItsDeletions(t *testing.T) {
	t.Parallel()
	parent := NewRepoWithCommit(t)
	linked := worktreeEnv(t, parent, "deletes")
	writeFileAt(t, filepath.Join(linked.RepoDir, "old.txt"), "tracked\n")
	testutil.RunGit(t, linked.RepoDir, "add", "old.txt")
	testutil.RunGit(t, linked.RepoDir, "commit", "-q", "--no-verify", "-m", "add old.txt")
	sess := parent.NewSession()

	require.NoError(t, parent.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, "replace old", sess.TranscriptPath))
	path := filepath.Join(linked.RepoDir, "new.txt")
	writeFileAt(t, path, "new\n")
	require.NoError(t, os.Remove(filepath.Join(linked.RepoDir, "old.txt")))
	sess.CreateTranscript("replace old", []FileChange{{Path: path, Content: "new"}})
	require.NoError(t, parent.SimulateStop(sess.ID, sess.TranscriptPath))

	state, err := parent.GetSessionState(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, resolved(t, linked.RepoDir), resolved(t, state.WorktreePath))
	assert.Contains(t, state.FilesTouched, "new.txt")
	assert.Contains(t, state.FilesTouched, "old.txt", "the deletion in the turn's worktree was not recorded")
}

// A turn that edits both worktrees leaves the session where it is.
func TestTurnEditingTwoWorktrees_LeavesTheSessionHome(t *testing.T) {
	t.Parallel()
	parent := NewRepoWithCommit(t)
	linked := worktreeEnv(t, parent, "mixed")
	sess := parent.NewSession()

	require.NoError(t, parent.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, "both", sess.TranscriptPath))
	home := filepath.Join(parent.RepoDir, "m.txt")
	other := filepath.Join(linked.RepoDir, "w.txt")
	writeFileAt(t, home, "m\n")
	writeFileAt(t, other, "w\n")
	sess.CreateTranscript("both", []FileChange{{Path: home, Content: "m"}, {Path: other, Content: "w"}})
	require.NoError(t, parent.SimulateStop(sess.ID, sess.TranscriptPath))

	state, err := parent.GetSessionState(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, resolved(t, parent.RepoDir), resolved(t, state.WorktreePath))
	assert.Contains(t, state.FilesTouched, "m.txt")
}

// A subagent's edits in another worktree don't move the parent session.
func TestSubagentEditsInAnotherWorktree_DontMoveTheSession(t *testing.T) {
	t.Parallel()
	parent := NewRepoWithCommit(t)
	linked := worktreeEnv(t, parent, "subagent")
	sess := parent.NewSession()

	require.NoError(t, parent.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, "delegate", sess.TranscriptPath))
	other := filepath.Join(linked.RepoDir, "sub.txt")
	writeFileAt(t, other, "s\n")
	sess.CreateSubagentTranscript("agent1", []FileChange{{Path: other, Content: "s"}})
	// The main transcript launches the subagent, so turn end sees its edits.
	sess.TranscriptBuilder.AddUserMessage("delegate")
	taskID := sess.TranscriptBuilder.AddTaskToolUse("toolu_task1", "edit in the other worktree")
	sess.TranscriptBuilder.AddTaskToolResult(taskID, "agent1")
	require.NoError(t, sess.TranscriptBuilder.WriteToFile(sess.TranscriptPath))
	require.NoError(t, parent.SimulateStop(sess.ID, sess.TranscriptPath))

	state, err := parent.GetSessionState(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, resolved(t, parent.RepoDir), resolved(t, state.WorktreePath))
}

func writeFileAt(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func resolved(t *testing.T, path string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)
	return r
}
