//go:build integration

package integration

import (
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

// The reservation made at stamp time must resolve the session in post-commit
// when neither the path nor the ancestry that prepare-commit-msg used remains.
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

	// Agent commit in the parent: linked by ancestry.
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

	// Between the hooks, ancestry is gone and the path fallback is ambiguous.
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
