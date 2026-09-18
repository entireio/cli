//go:build integration

package integration

import (
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPostCommit_ResolvesSessionFromReservedTrailerWhenPathAndAncestryFail
// covers the dangling-trailer shape: prepare-commit-msg identified a session
// (here by process ancestry, in a worktree that is not the session's home and
// while another worktree also has a live session, so path matching is
// ambiguous), stamped its trailer, and then post-commit could no longer see
// the same evidence. Today post-commit logs "no active sessions despite
// trailer" and the commit names a checkpoint nobody wrote. The trailer's ID is
// reserved on the session when it is stamped, so post-commit resolves the
// session from the commit itself.
func TestPostCommit_ResolvesSessionFromReservedTrailerWhenPathAndAncestryFail(t *testing.T) {
	t.Parallel()
	parent := NewRepoWithCommit(t)

	other := worktreeEnv(t, parent, "other")
	require.NoError(t, other.SimulateUserPromptSubmit("other-agent-session"))
	disownSession(t, parent, "other-agent-session")

	home := worktreeEnv(t, parent, "home")
	sess := home.NewSession()
	prompt := "Draft the change"
	require.NoError(t, home.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, prompt, sess.TranscriptPath))
	sess.CreateTranscript(prompt, []FileChange{{Path: "draft.txt", Content: "draft\n"}})

	// The agent commits in the parent: prepare-commit-msg links it by ancestry.
	parent.WriteFile("draft.txt", "draft\n")
	parent.GitAdd("draft.txt")
	msgFile := parent.commitMsgFile()
	require.NoError(t, os.WriteFile(msgFile, []byte("Agent commit from the parent"), 0o600))
	if out, err := parent.prepareCommitMsgCmd(false, msgFile, "message").CombinedOutput(); err != nil {
		t.Fatalf("prepare-commit-msg: %v\n%s", err, out)
	}
	message, err := os.ReadFile(msgFile)
	require.NoError(t, err)
	require.Contains(t, string(message), "Entire-Checkpoint:", "prepare-commit-msg must have stamped the trailer")
	parent.GitCommit(string(message))
	cpID := parent.GetCheckpointIDFromCommitMessage(parent.GetHeadHash())
	require.NotEmpty(t, cpID)

	// Between the two hooks the evidence prepare used is gone: the committing
	// process no longer descends from the agent, and the path fallback is
	// ambiguous between two live worktrees.
	disownSession(t, parent, sess.ID)
	post := exec.CommandContext(t.Context(), getTestBinary(), "hooks", "git", "post-commit")
	post.Dir = parent.RepoDir
	post.Env = parent.gitHookEnv()
	if out, err := post.CombinedOutput(); err != nil {
		t.Fatalf("post-commit: %v\n%s", err, out)
	}

	state, err := parent.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.Equal(t, cpID, state.LastCheckpointID.String(),
		"post-commit must condense the session whose reserved checkpoint the trailer names")
}
