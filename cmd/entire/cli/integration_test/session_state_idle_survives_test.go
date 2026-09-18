//go:build integration

package integration

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/session"
)

// A session whose last turn changed no files has no shadow branch and no last
// checkpoint. Every store listing used to delete that shape as an orphan, so a
// commit hook in another worktree wiped a live session between turns. An idle
// session whose agent is alive must survive.
func TestSessionStore_IdleLiveSessionSurvivesAnotherWorktreesCommitHook(t *testing.T) {
	t.Parallel()
	parent := NewRepoWithCommit(t)
	quiet := worktreeEnv(t, parent, "quiet")

	sess := quiet.NewSession()
	prompt := "Look around, change nothing"
	require.NoError(t, quiet.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, prompt, sess.TranscriptPath))
	sess.CreateTranscript(prompt, nil)
	require.NoError(t, quiet.SimulateStop(sess.ID, sess.TranscriptPath))

	state, err := parent.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.Equal(t, session.PhaseIdle, state.Phase)
	require.True(t, state.LastCheckpointID.IsEmpty(), "precondition: nothing condensed yet")
	require.NotNil(t, state.Owner, "precondition: the hook recorded a live owner")

	// A human commits in the parent; its prepare-commit-msg lists the shared store.
	parent.WriteFile("notes.txt", "unrelated\n")
	parent.GitCommitWithShadowHooks("Unrelated commit in the parent", "notes.txt")

	state, err = parent.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.NotNil(t, state, "an idle session whose agent is still running must not be deleted by another worktree's commit hook")
}
