//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/stretchr/testify/require"
)

// TestHookOverwrite_LefthookRefreshKeepsCurrentCommitCovered pins the manager
// integration contract: once Entire has published its Lefthook-local scripts,
// a Lefthook refresh may replace the shared native wrappers and the very next
// commit still reaches Entire. Recovery must not wait for another agent prompt.
func TestHookOverwrite_LefthookRefreshKeepsCurrentCommitCovered(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	writeLefthookConfig(t, env.RepoDir)
	env.RunCLI("enable", "--absolute-git-hook-path")

	sess := env.NewSession()
	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(
		sess.ID, "Create files A and B", sess.TranscriptPath))

	env.WriteFile("fileA.go", pkgFuncA)
	env.WriteFile("fileB.go", pkgFuncB)
	sess.CreateTranscript("Create files A and B", []FileChange{
		{Path: "fileA.go", Content: pkgFuncA},
		{Path: "fileB.go", Content: pkgFuncB},
	})

	env.GitCommitWithShadowHooksAsAgent("Add file A", "fileA.go")
	require.NotEmpty(t, env.GetCheckpointIDFromCommitMessage(env.GetHeadHash()))

	// Simulate `lefthook install` refreshing every shared hook between commits.
	installSimulatedLefthookHooks(t, env.RepoDir)
	env.GitAdd("fileB.go")
	cmd := execx.NonInteractive(context.Background(), "git", "commit", "-m", "Add file B")
	cmd.Dir = env.RepoDir
	// The runner may disable repository hooks globally with LEFTHOOK=0. This
	// scenario specifically exercises an active Lefthook refresh, so declare
	// that precondition instead of inheriting the runner's ambient opt-out.
	cmd.Env = append(env.cliEnv(), "LEFTHOOK=1")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)

	require.NotEmpty(t, env.GetCheckpointIDFromCommitMessage(env.GetHeadHash()),
		"the commit immediately after Lefthook refresh must retain Entire behavior")
	for _, hook := range []string{"prepare-commit-msg", "commit-msg", "post-commit", "post-rewrite", "pre-push"} {
		data, readErr := os.ReadFile(filepath.Join(env.RepoDir, ".git", "hooks", hook))
		require.NoError(t, readErr)
		require.Contains(t, string(data), "lefthook generated wrapper")
	}
}
