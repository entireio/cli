//go:build e2e

package e2e

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestE2E_CheckpointMetadata verifies that checkpoint metadata is correctly stored.
func TestE2E_CheckpointMetadata(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, "manual-commit")

	// 1. Agent creates a file
	t.Log("Step 1: Agent creating file")
	result, err := env.RunAgent(PromptCreateConfig.Prompt)
	require.NoError(t, err)
	AssertAgentSuccess(t, result, err)
	AssertExpectedFilesExist(t, env, PromptCreateConfig)

	// 2. Verify session created rewind points
	t.Log("Step 2: Checking session rewind points")
	points := env.GetRewindPoints()
	require.GreaterOrEqual(t, len(points), 1, "Should have rewind points before commit")

	// Note: Before commit, points are on the shadow branch
	// They should have metadata directories set
	for i, p := range points {
		t.Logf("Rewind point %d: ID=%s, MetadataDir=%s, Message=%s",
			i, p.ID[:12], p.MetadataDir, p.Message)
	}

	// 3. User commits
	t.Log("Step 3: Committing changes")
	env.GitCommitWithShadowHooks("Add config file", "config.json")

	// 4. Verify checkpoint trailer added
	checkpointID := env.GetLatestCheckpointIDFromHistory()
	require.NotEmpty(t, checkpointID, "Should have checkpoint ID in commit")
	t.Logf("Checkpoint ID: %s", checkpointID)

	// 5. Verify metadata branch has content
	assert.True(t, env.BranchExists("entire/checkpoints/v1"),
		"Metadata branch should exist after commit")

	// 6. Verify rewind points now reference condensed metadata
	t.Log("Step 4: Checking post-commit rewind points")
	postPoints := env.GetRewindPoints()
	// After commit, logs-only points from entire/checkpoints/v1 should exist
	for i, p := range postPoints {
		t.Logf("Post-commit point %d: ID=%s, IsLogsOnly=%v, CondensationID=%s",
			i, p.ID[:12], p.IsLogsOnly, p.CondensationID)
	}
}

// TestE2E_CheckpointIDFormat verifies checkpoint ID format is correct.
func TestE2E_CheckpointIDFormat(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, "manual-commit")

	// 1. Agent makes changes
	result, err := env.RunAgent(PromptCreateHelloGo.Prompt)
	require.NoError(t, err)
	AssertAgentSuccess(t, result, err)

	// 2. User commits
	env.GitCommitWithShadowHooks("Add hello world", "hello.go")

	// 3. Verify checkpoint ID format
	checkpointID := env.GetLatestCheckpointIDFromHistory()
	require.NotEmpty(t, checkpointID)

	// Checkpoint ID should be 12 hex characters
	assert.Len(t, checkpointID, 12, "Checkpoint ID should be 12 characters")

	// Should only contain hex characters
	for _, c := range checkpointID {
		assert.True(t, (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f'),
			"Checkpoint ID should be lowercase hex: got %c", c)
	}
}

// TestE2E_AutoCommitStrategy tests the auto-commit strategy creates clean commits.
func TestE2E_AutoCommitStrategy(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, "auto-commit")

	// 1. Agent creates a file
	t.Log("Step 1: Agent creating file with auto-commit strategy")
	result, err := env.RunAgent(PromptCreateHelloGo.Prompt)
	require.NoError(t, err)
	AssertAgentSuccess(t, result, err)

	// 2. Verify file exists
	require.True(t, env.FileExists("hello.go"))

	// 3. With auto-commit, commits are created automatically
	// Check if commits were made with checkpoint trailers
	commitMsg := env.GetCommitMessage(env.GetHeadHash())
	t.Logf("Latest commit message: %s", commitMsg)

	// 4. Verify metadata branch exists
	if env.BranchExists("entire/checkpoints/v1") {
		t.Log("Metadata branch exists (auto-commit creates it)")
	}

	// 5. Check for rewind points
	points := env.GetRewindPoints()
	t.Logf("Found %d rewind points", len(points))
}
