//go:build integration

package integration

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// An idle session with no shadow branch and no checkpoint is a live session
// between turns; another worktree's commit hook must not delete it.
func TestSessionStore_IdleSessionSurvivesAnotherWorktreesCommitHook(t *testing.T) {
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

	// Another worktree's hook lists the shared store.
	parent.WriteFile("notes.txt", "unrelated\n")
	parent.GitCommitWithShadowHooks("Unrelated commit in the parent", "notes.txt")

	state, err = parent.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.NotNil(t, state, "an idle session between turns must not be deleted by another worktree's commit hook")
}

// worktreeEnv adds a linked worktree, with Entire initialised, as a TestEnv.
func worktreeEnv(t *testing.T, parent *TestEnv, name string) *TestEnv {
	t.Helper()
	base := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(base); err == nil {
		base = resolved
	}
	dir := filepath.Join(base, name)
	testutil.RunGit(t, parent.RepoDir, "worktree", "add", "-b", "wt/"+name, dir)
	env := *parent
	env.RepoDir = dir
	env.InitEntire()
	return &env
}
