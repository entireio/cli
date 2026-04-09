//go:build integration

package integration

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bareRefExists checks if a ref exists in a bare repo by running git ls-remote.
func bareRefExists(t *testing.T, bareDir, refName string) bool {
	t.Helper()
	cmd := exec.Command("git", "ls-remote", bareDir, refName)
	cmd.Env = testutil.GitIsolatedEnv()
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(output)) != ""
}

func TestV2Push_FullCycle(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.WriteFile(".gitignore", ".entire/\n")
	env.GitAdd("README.md")
	env.GitAdd(".gitignore")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/v2-push-test")

	// Initialize with both checkpoints_v2 and push_v2_refs enabled
	env.InitEntireWithOptions(map[string]any{
		"checkpoints_v2": true,
		"push_v2_refs":   true,
	})

	bareDir := env.SetupBareRemote()

	// Start session, create file, stop, commit
	session := env.NewSession()
	err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Add feature")
	require.NoError(t, err)

	env.WriteFile("feature.go", "package main\n\nfunc Feature() {}")
	session.CreateTranscript(
		"Add feature",
		[]FileChange{{Path: "feature.go", Content: "package main\n\nfunc Feature() {}"}},
	)
	err = env.SimulateStop(session.ID, session.TranscriptPath)
	require.NoError(t, err)

	env.GitAdd("feature.go")
	env.GitCommitWithShadowHooks("Add feature")

	// Run pre-push (which pushes v1 and v2 refs)
	env.RunPrePush("origin")

	// Verify v2 refs exist on remote
	assert.True(t, bareRefExists(t, bareDir, paths.V2MainRefName),
		"v2 /main ref should exist on remote after push")
	assert.True(t, bareRefExists(t, bareDir, paths.V2FullCurrentRefName),
		"v2 /full/current ref should exist on remote after push")

	// v1 should also be pushed (dual-write)
	assert.True(t, bareRefExists(t, bareDir, "refs/heads/"+paths.MetadataBranchName),
		"v1 metadata branch should exist on remote after push")
}

func TestV2Push_Disabled_NoV2Refs(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.WriteFile(".gitignore", ".entire/\n")
	env.GitAdd("README.md")
	env.GitAdd(".gitignore")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/v2-push-disabled")

	// Enable checkpoints_v2 but NOT push_v2_refs
	env.InitEntireWithOptions(map[string]any{
		"checkpoints_v2": true,
	})

	bareDir := env.SetupBareRemote()

	session := env.NewSession()
	err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Add feature")
	require.NoError(t, err)

	env.WriteFile("feature.go", "package main\n\nfunc Feature() {}")
	session.CreateTranscript(
		"Add feature",
		[]FileChange{{Path: "feature.go", Content: "package main\n\nfunc Feature() {}"}},
	)
	err = env.SimulateStop(session.ID, session.TranscriptPath)
	require.NoError(t, err)

	env.GitAdd("feature.go")
	env.GitCommitWithShadowHooks("Add feature")

	env.RunPrePush("origin")

	// v2 refs should NOT be pushed
	assert.False(t, bareRefExists(t, bareDir, paths.V2MainRefName),
		"v2 /main ref should NOT exist on remote when push_v2_refs is disabled")
	assert.False(t, bareRefExists(t, bareDir, paths.V2FullCurrentRefName),
		"v2 /full/current ref should NOT exist on remote when push_v2_refs is disabled")

	// v1 should still be pushed
	assert.True(t, bareRefExists(t, bareDir, "refs/heads/"+paths.MetadataBranchName),
		"v1 metadata branch should still exist on remote")
}
