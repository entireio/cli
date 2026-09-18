//go:build integration

package integration

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/session"
)

// TestSessionStore_IdleLiveSessionSurvivesAnotherWorktreesCommitHook covers
// the shared-store deletion that re-initialises live sessions. A session whose
// last turn changed no files has no shadow branch and no last checkpoint — the
// normal shape of a read-only turn, and of any session right after a linked
// commit. Every process that lists the shared store used to treat that shape
// as an orphan and delete it, so a commit hook in ANOTHER worktree wiped the
// session between two of its turns; the next turn-start rebuilt it from zero
// and a commit in the gap found no session at all. An idle session whose
// agent is still alive must survive other worktrees' hooks.
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
