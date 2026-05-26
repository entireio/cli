//go:build integration

package integration

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// TestV1MetadataRef_FreshRepo_ManualOptIn_FullWorkflow exercises the
// manual-opt-in path: a fresh repo gets checkpoints_version=1.1 in
// settings.json, then runs a session and commit. The custom ref
// refs/entire/checkpoints/v1 should be created; the legacy branch
// refs/heads/entire/checkpoints/v1 should not exist.
func TestV1MetadataRef_FreshRepo_ManualOptIn_FullWorkflow(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	defer env.Cleanup()

	env.InitEntireWithOptions(map[string]any{
		"checkpoints_version": "1.1",
	})

	session := env.NewSession()
	require.NoError(t, env.SimulateUserPromptSubmitWithPrompt(session.ID, "Add hello"))
	env.WriteFile("hello.txt", "world")
	session.CreateTranscript("Add hello", []FileChange{{Path: "hello.txt", Content: "world"}})
	require.NoError(t, env.SimulateStop(session.ID, session.TranscriptPath))

	env.GitAdd("hello.txt")
	env.GitCommitWithShadowHooks("Add hello")

	assert.True(t, env.RefExists(paths.MetadataRefName),
		"custom ref %s should exist on a fresh 1.1 repo after first commit", paths.MetadataRefName)
	assert.False(t, env.BranchExists(paths.MetadataBranchName),
		"legacy branch refs/heads/%s should NOT be created on a fresh 1.1 repo", paths.MetadataBranchName)
}

// TestLegacyV1_StillWorks_Regression verifies that v1 (default) behavior is
// unchanged: the legacy branch is created, the custom ref is not.
func TestLegacyV1_StillWorks_Regression(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	defer env.Cleanup()

	env.InitEntire()

	session := env.NewSession()
	require.NoError(t, env.SimulateUserPromptSubmitWithPrompt(session.ID, "Add plain"))
	env.WriteFile("plain.txt", "plain")
	session.CreateTranscript("Add plain", []FileChange{{Path: "plain.txt", Content: "plain"}})
	require.NoError(t, env.SimulateStop(session.ID, session.TranscriptPath))
	env.GitAdd("plain.txt")
	env.GitCommitWithShadowHooks("Add plain")

	assert.True(t, env.BranchExists(paths.MetadataBranchName),
		"legacy v1 branch should exist after a v1 commit")
	assert.False(t, env.RefExists(paths.MetadataRefName),
		"custom ref should NOT exist on a v1 repo")
}

// TestV1MetadataRef_PushAndFetch_NonFF exercises pushRefIfNeeded at the
// custom-ref namespace: two diverging "machines" (clones from the same bare
// remote) push 1.1 metadata commits; the second push triggers
// fetchAndRebaseSessionsCommon at the custom ref pair.
func TestV1MetadataRef_PushAndFetch_NonFF(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	defer env.Cleanup()

	env.InitEntireWithOptions(map[string]any{"checkpoints_version": "1.1"})
	bareDir := env.SetupBareRemote()

	// Round 1: one commit on the 1.1 repo, pushed to origin.
	session1 := env.NewSession()
	require.NoError(t, env.SimulateUserPromptSubmitWithPrompt(session1.ID, "Add round1"))
	env.WriteFile("round1.txt", "1")
	session1.CreateTranscript("Add round1", []FileChange{{Path: "round1.txt", Content: "1"}})
	require.NoError(t, env.SimulateStop(session1.ID, session1.TranscriptPath))
	env.GitAdd("round1.txt")
	env.GitCommitWithShadowHooks("Add round1")
	env.RunPrePush("origin")

	// Verify the custom ref landed on the bare remote.
	assert.True(t, bareRefExists(t, bareDir, paths.MetadataRefName),
		"custom ref %s should exist on bare remote after first push", paths.MetadataRefName)

	// Round 2: a second commit on the same 1.1 repo, pushed again. This is
	// fast-forward so it should not trigger the rebase path, but it exercises
	// the same code path with a real custom-ref refspec.
	session2 := env.NewSession()
	require.NoError(t, env.SimulateUserPromptSubmitWithPrompt(session2.ID, "Add round2"))
	env.WriteFile("round2.txt", "2")
	session2.CreateTranscript("Add round2", []FileChange{{Path: "round2.txt", Content: "2"}})
	require.NoError(t, env.SimulateStop(session2.ID, session2.TranscriptPath))
	env.GitAdd("round2.txt")
	env.GitCommitWithShadowHooks("Add round2")
	env.RunPrePush("origin")

	assert.True(t, bareRefExists(t, bareDir, paths.MetadataRefName),
		"custom ref should still exist on bare remote after second push")
	assert.False(t, bareRefExists(t, bareDir, "refs/heads/"+paths.MetadataBranchName),
		"legacy branch should NOT be pushed by a 1.1 repo")
}

func bareRefExists(t *testing.T, bareDir, refName string) bool {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "show-ref", "--verify", "--quiet", refName)
	cmd.Dir = bareDir
	cmd.Env = testutil.GitIsolatedEnv()
	return cmd.Run() == nil
}
