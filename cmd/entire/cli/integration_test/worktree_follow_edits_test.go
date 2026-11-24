//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

// An agent launched in the main checkout fires every hook there, but edits a
// linked worktree by absolute path. Each edit is recorded against the
// worktree it is in, and a commit in a worktree links and clears only that
// worktree's edits:
//
//   - A: with work pending at home, the agent's edit in the linked worktree is
//     recorded for it, and committing it there links only that file;
//   - B: the home's pending file then links from the home;
//   - C: back at home, a turn tracks there as before.
func TestEditsAreRecordedAndLinkedPerWorktree(t *testing.T) {
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

	turn("Edit at home", parent.RepoDir, "home.go")
	turn("Edit the linked worktree", linked.RepoDir, "a.go")

	state, err := parent.GetSessionState(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"home.go"}, state.FilesTouched, "the home's record should hold only the home's file")
	work := worktreeWork(t, state, linked.RepoDir)
	require.NotNil(t, work, "the linked worktree's edit was not recorded for it")
	assert.Equal(t, []string{"a.go"}, work.FilesTouched)
	assert.NotEmpty(t, work.TouchedFileHashes["a.go"])

	// A: someone commits the agent's file in the linked worktree.
	linked.GitCommitWithHooks("Commit a.go", "a.go")
	cpA := linked.GetCheckpointIDFromCommitMessage(linked.GetHeadHash())
	require.NotEmpty(t, cpA, "the linked worktree's commit of the agent's file was not linked")
	assert.Equal(t, []string{"a.go"}, checkpointFiles(t, parent, cpA))
	state, err = parent.GetSessionState(sess.ID)
	require.NoError(t, err)
	assert.Nil(t, worktreeWork(t, state, linked.RepoDir), "the linked worktree's record should be cleared")
	assert.Equal(t, []string{"home.go"}, state.FilesTouched, "a commit elsewhere must leave the home's pending file")

	// B: the home's file links from the home.
	parent.GitCommitWithHooks("Commit home.go", "home.go")
	cpB := parent.GetCheckpointIDFromCommitMessage(parent.GetHeadHash())
	require.NotEmpty(t, cpB, "the home commit of the agent's file was not linked")
	assert.Equal(t, []string{"home.go"}, checkpointFiles(t, parent, cpB))
	state, err = parent.GetSessionState(sess.ID)
	require.NoError(t, err)
	assert.False(t, state.HasPendingFiles(), "nothing should be pending after both commits")

	// C: back at home.
	turn("Edit at home again", parent.RepoDir, "home2.go")
	state, err = parent.GetSessionState(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"home2.go"}, state.FilesTouched)
	assert.NotEmpty(t, state.TouchedFileHashes["home2.go"])
	assert.Nil(t, state.OtherWorktrees)
}

// One turn editing both worktrees records each file for its own worktree.
func TestTurnEditingTwoWorktrees_RecordsEach(t *testing.T) {
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
	assert.Equal(t, []string{"m.txt"}, state.FilesTouched)
	work := worktreeWork(t, state, linked.RepoDir)
	require.NotNil(t, work)
	assert.Equal(t, []string{"w.txt"}, work.FilesTouched)
	assert.Equal(t, 1, state.StepCount, "one turn is one step")
}

// A subagent's edits in another worktree are left to its task record.
func TestSubagentEditsInAnotherWorktree_AreNotRecordedForIt(t *testing.T) {
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
	assert.Nil(t, worktreeWork(t, state, linked.RepoDir))
}

// worktreeWork is state's record of edits in the worktree at dir, or nil.
func worktreeWork(t *testing.T, state *session.State, dir string) *session.WorktreeWork {
	t.Helper()
	worktreeID, err := paths.GetWorktreeID(dir)
	require.NoError(t, err)
	return state.OtherWorktrees[session.WorktreeKey(worktreeID)]
}

// checkpointFiles is the files_touched a checkpoint recorded.
func checkpointFiles(t *testing.T, env *TestEnv, checkpointID string) []string {
	t.Helper()
	data, found := env.ReadFileFromBranch(paths.MetadataBranchName, CheckpointSummaryPath(checkpointID))
	require.True(t, found, "checkpoint %s has no summary", checkpointID)
	var summary checkpoint.CheckpointSummary
	require.NoError(t, json.Unmarshal([]byte(data), &summary))
	return summary.FilesTouched
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

// A turn that removes an untracked file it created earlier at home while also
// editing another worktree records the removal, as a home-only turn does.
func TestTurnEditingElsewhere_StillRecordsHomeRemovals(t *testing.T) {
	t.Parallel()
	parent := NewRepoWithCommit(t)
	linked := worktreeEnv(t, parent, "removals")
	sess := parent.NewSession()

	require.NoError(t, parent.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, "create", sess.TranscriptPath))
	home := filepath.Join(parent.RepoDir, "scratch.txt")
	writeFileAt(t, home, "temp\n")
	sess.CreateTranscript("create", []FileChange{{Path: home, Content: "temp"}})
	require.NoError(t, parent.SimulateStop(sess.ID, sess.TranscriptPath))
	state, err := parent.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.NotEmpty(t, state.TouchedFileHashes["scratch.txt"], "fixture: the file is recorded with its hash")

	require.NoError(t, parent.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, "clean up and edit elsewhere", sess.TranscriptPath))
	require.NoError(t, os.Remove(home))
	other := filepath.Join(linked.RepoDir, "x.txt")
	writeFileAt(t, other, "x\n")
	sess.CreateTranscript("clean up and edit elsewhere", []FileChange{{Path: other, Content: "x"}})
	require.NoError(t, parent.SimulateStop(sess.ID, sess.TranscriptPath))

	state, err = parent.GetSessionState(sess.ID)
	require.NoError(t, err)
	hash, recorded := state.TouchedFileHashes["scratch.txt"]
	assert.True(t, recorded && hash == "", "the home removal was not recorded as a deletion: %q (recorded=%v)", hash, recorded)
}
