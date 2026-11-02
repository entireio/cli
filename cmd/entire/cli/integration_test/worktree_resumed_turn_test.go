//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// A session started in the main worktree and resumed in a linked one: the
// resumed turn must not mark the file it left uncommitted at home as deleted
// (it isn't in the linked tree), its prompt must join the home's stored copy,
// and committing that file at home must still link the session.
func TestResumedInLinkedWorktree_HomeCommitStillLinks(t *testing.T) {
	t.Parallel()
	parent := NewRepoWithCommit(t)
	linked := worktreeEnv(t, parent, "resumed")
	sess := parent.NewSession()

	// Turn 1 at home creates an untracked file.
	first := "Create the home file"
	require.NoError(t, parent.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, first, sess.TranscriptPath))
	parent.WriteFile("home.go", "package home\n")
	sess.CreateTranscript(first, []FileChange{{Path: filepath.Join(parent.RepoDir, "home.go"), Content: "package home\n"}})
	require.NoError(t, parent.SimulateStop(sess.ID, sess.TranscriptPath))

	// Turn 2 runs in the linked worktree and changes nothing there.
	second := "Look at the linked branch"
	require.NoError(t, linked.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, second, sess.TranscriptPath))
	sess.CreateTranscript(second, nil)
	require.NoError(t, linked.SimulateStop(sess.ID, sess.TranscriptPath))

	state, err := parent.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.NotNil(t, state)
	assert.Contains(t, state.FilesTouched, "home.go")
	assert.NotEmpty(t, state.TouchedFileHashes["home.go"], "the home's untracked file was recorded as deleted from the linked tree")

	metadataDir := filepath.FromSlash(paths.SessionMetadataDirFromSessionID(sess.ID))
	prompts, err := os.ReadFile(filepath.Join(parent.RepoDir, metadataDir, paths.PromptFileName))
	require.NoError(t, err)
	assert.Contains(t, string(prompts), first)
	assert.Contains(t, string(prompts), second, "the linked turn's prompt should join the home's stored copy")
	_, err = os.Stat(filepath.Join(linked.RepoDir, metadataDir))
	assert.True(t, os.IsNotExist(err), "a second stored copy appeared in the linked worktree")

	// Committing the file at home links the session.
	parent.GitCommitWithHooks("Add home file", "home.go")
	cpID := parent.GetCheckpointIDFromCommitMessage(parent.GetHeadHash())
	require.NotEmpty(t, cpID, "the home commit has no Entire-Checkpoint trailer")
	parent.validatePromptContent(cpID, []string{first, second})
}
