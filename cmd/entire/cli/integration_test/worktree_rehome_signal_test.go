//go:build integration

package integration

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A hook that merely runs in the launch directory, with no payload cwd and no
// edits there, must not pull a re-homed session back; only a payload naming
// the tree or edits captured in it may move a session at a hook.
func TestCommitLinking_HookWithoutSignalDoesNotPullSessionBack(t *testing.T) {
	t.Parallel()
	parent := NewRepoWithCommit(t)
	feature := worktreeEnv(t, parent, "feature")
	sess := parent.NewSession()
	prompt := "Work in the worktree"
	require.NoError(t, parent.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, prompt, sess.TranscriptPath))
	feature.WriteFile("feature.txt", "work\n")
	sess.CreateTranscript(prompt, []FileChange{{Path: "feature.txt", Content: "work\n"}})
	feature.GitCommitWithShadowHooksAsAgent("Agent commit in the worktree", "feature.txt")
	state, err := parent.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.Equal(t, feature.RepoDir, state.WorktreePath, "precondition: the agent's commit re-homed the session")

	// The agent's hooks keep running in the parent with no cwd and no edits there.
	require.NoError(t, parent.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, "next turn", sess.TranscriptPath))
	require.NoError(t, parent.SimulateStop(sess.ID, sess.TranscriptPath))

	state, err = parent.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.Equal(t, feature.RepoDir, state.WorktreePath, "a signal-less hook in the launch directory must not move the session")
}

// Factory Droid's payload carries no working directory at all, so its hooks
// always run in the launch directory; the own-commit re-home must still hold.
func TestCommitLinking_AgentWithoutPayloadCwdKeepsCommitHome(t *testing.T) {
	t.Parallel()
	parent := NewRepoWithCommit(t)
	feature := worktreeEnv(t, parent, "feature")
	sess := parent.NewFactoryDroidSession()
	droid := NewFactoryDroidHookRunner(parent.RepoDir, t)
	prompt := map[string]string{"session_id": sess.ID, "transcript_path": sess.TranscriptPath, "prompt": "Work in the worktree"}
	require.NoError(t, droid.runDroidHookWithInput("user-prompt-submit", prompt))
	feature.WriteFile("feature.txt", "work\n")
	sess.CreateDroidTranscript("Work in the worktree", []FileChange{{Path: "feature.txt", Content: "work\n"}})
	feature.GitCommitWithShadowHooksAsAgent("Agent commit in the worktree", "feature.txt")
	state, err := parent.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.Equal(t, feature.RepoDir, state.WorktreePath, "precondition: the agent's commit re-homed the session")

	require.NoError(t, droid.runDroidHookWithInput("user-prompt-submit", prompt))
	require.NoError(t, droid.SimulateStop(sess.ID, sess.TranscriptPath))

	state, err = parent.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.Equal(t, feature.RepoDir, state.WorktreePath, "hooks without a working directory must not move the session")
}
