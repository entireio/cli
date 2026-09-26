//go:build integration

package integration

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Hooks keep running where the agent was launched, but the payload's cwd
// follows the agent into a worktree. The session must follow it at the hook,
// with no process ancestry involved, so a later non-agent commit there links.
func TestCommitLinking_HookWorkingDirectoryReHomesSession(t *testing.T) {
	t.Parallel()
	parent := NewRepoWithCommit(t)
	other := worktreeEnv(t, parent, "other")
	require.NoError(t, other.SimulateUserPromptSubmit("other-agent-session"))
	disownSession(t, parent, "other-agent-session")
	feature := worktreeEnv(t, parent, "feature")

	sess := parent.NewSession()
	hooks := NewHookRunner(parent.RepoDir, parent.ClaudeProjectDir, t)
	payload := func(prompt, cwd string) map[string]string {
		return map[string]string{"session_id": sess.ID, "transcript_path": sess.TranscriptPath, "prompt": prompt, "cwd": cwd}
	}
	// Turn 1 in the parent; turn 2 after the agent entered the worktree.
	require.NoError(t, hooks.runHookWithInput("user-prompt-submit", payload("start", parent.RepoDir)))
	require.NoError(t, hooks.runHookWithInput("user-prompt-submit", payload("continue in the worktree", feature.RepoDir)))

	state, err := parent.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.Equal(t, feature.RepoDir, state.WorktreePath, "the session must follow the agent's reported working directory")

	disownSession(t, parent, sess.ID)
	feature.WriteFile("feature.txt", "work\n")
	sess.CreateTranscript("continue in the worktree", []FileChange{{Path: "feature.txt", Content: "work\n"}})
	feature.GitCommitWithShadowHooksAsAgent("Commit in the worktree", "feature.txt")
	require.Contains(t, headMessage(t, feature.RepoDir), "Entire-Checkpoint:", "exact match after re-homing, no ancestry needed")
}
