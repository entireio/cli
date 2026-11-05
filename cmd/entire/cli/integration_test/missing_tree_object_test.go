//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
	"github.com/stretchr/testify/require"
)

// TestMissingSubtreeObject_HooksDoNotBlockCommit covers a repository whose
// HEAD names a subtree object that is absent from the object store, as in a
// damaged clone. go-git reports an unreadable subtree as an error rather than
// a shorter walk, so every hook on the turn and commit path must still exit
// zero: a failing prepare-commit-msg would abort the user's commit.
func TestMissingSubtreeObject_HooksDoNotBlockCommit(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.WriteFile("src/a.go", "package src\n\nfunc A() {}\n")
	env.WriteFile("src/nested/b.go", "package nested\n\nfunc B() {}\n")
	env.GitAdd("src/a.go", "src/nested/b.go")
	env.GitCommit("Add src")
	srcTree := strings.TrimSpace(testutil.RunGit(t, env.RepoDir, "rev-parse", "HEAD:src"))
	deleteLooseObject := func() {
		t.Helper()
		require.NoError(t, os.Remove(filepath.Join(env.RepoDir, ".git", "objects", srcTree[:2], srcTree[2:])),
			"src tree %s must be a loose object to delete", srcTree)
		check := execx.NonInteractive(t.Context(), "git", "cat-file", "-e", srcTree)
		check.Dir = env.RepoDir
		check.Env = gitenv.Isolated()
		require.Error(t, check.Run(), "src tree %s must be absent", srcTree)
	}
	deleteLooseObject()

	readOnly := env.NewSession()
	require.NoError(t, env.SimulateUserPromptSubmit(readOnly.ID))
	readOnly.TranscriptBuilder.AddUserMessage("Summarize src")
	readOnly.TranscriptBuilder.AddAssistantMessage("src defines A and nested.B.")
	require.NoError(t, readOnly.TranscriptBuilder.WriteToFile(readOnly.TranscriptPath))
	require.NoError(t, env.SimulateStop(readOnly.ID, readOnly.TranscriptPath), "stop hook on a read-only turn")
	roState, err := env.GetSessionState(readOnly.ID)
	require.NoError(t, err)
	require.Empty(t, roState.FilesTouched, "an unreadable tree must not make a read-only turn touch files")

	coding := env.NewSession()
	require.NoError(t, env.SimulateUserPromptSubmitWithTranscriptPath(coding.ID, coding.TranscriptPath))
	env.WriteFile("NOTES.md", "notes\n")
	coding.CreateTranscript("Add notes", []FileChange{{Path: "NOTES.md", Content: "notes\n"}})
	require.NoError(t, env.SimulateStop(coding.ID, coding.TranscriptPath), "stop hook on a coding turn")

	testutil.RunGit(t, env.RepoDir, "add", "NOTES.md")
	msgFile := env.commitMsgFile()
	require.NoError(t, os.WriteFile(msgFile, []byte("Add notes"), 0o600))
	prep := env.prepareCommitMsgCmd(false, msgFile, "message")
	prep.Env = append(prep.Env, gitNoEditorEnv)
	requireHookSucceeds(t, prep, "prepare-commit-msg")

	// git rebuilds the src tree from the index while committing; delete it
	// again so post-commit sees the same gap in both HEAD and its parent.
	testutil.RunGit(t, env.RepoDir, "commit", "--no-verify", "--no-gpg-sign", "-F", msgFile)
	deleteLooseObject()
	post := execx.NonInteractive(t.Context(), getTestBinary(), "hooks", "git", "post-commit")
	post.Dir = env.RepoDir
	post.Env = env.gitHookEnv()
	requireHookSucceeds(t, post, "post-commit")

	committed := testutil.RunGit(t, env.RepoDir, "show", "--name-only", "--format=", "HEAD")
	require.Equal(t, "NOTES.md", strings.TrimSpace(committed))
	require.NotEmpty(t, env.GetCheckpointIDFromCommitMessage(env.GetHeadHash()), "commit must keep its checkpoint trailer")
}

func requireHookSucceeds(t *testing.T, cmd *exec.Cmd, name string) {
	t.Helper()
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s hook must not fail: %s", name, out)
}
