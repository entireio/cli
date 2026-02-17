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
			i, safeIDPrefix(p.ID), p.MetadataDir, p.Message)
	}

	// 3. User commits
	t.Log("Step 3: Committing changes")
	env.GitCommitWithShadowHooks("Add config file", "config.json")

	// 4. Verify checkpoint trailer added
	checkpointID, err := env.GetLatestCheckpointIDFromHistory()
	require.NoError(t, err, "Should find checkpoint ID in commit history")
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
			i, safeIDPrefix(p.ID), p.IsLogsOnly, p.CondensationID)
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
	checkpointID, err := env.GetLatestCheckpointIDFromHistory()
	require.NoError(t, err, "Should find checkpoint ID in commit history")
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

	// Count commits before agent action
	commitsBefore := env.GetCommitCount()
	t.Logf("Commits before: %d", commitsBefore)

	// 1. Agent creates a file
	t.Log("Step 1: Agent creating file with auto-commit strategy")
	result, err := env.RunAgent(PromptCreateHelloGo.Prompt)
	require.NoError(t, err)
	AssertAgentSuccess(t, result, err)

	// 2. Verify file exists
	require.True(t, env.FileExists("hello.go"), "hello.go should exist")
	AssertHelloWorldProgram(t, env, "hello.go")

	// 3. With auto-commit, commits are created automatically
	commitsAfter := env.GetCommitCount()
	t.Logf("Commits after: %d", commitsAfter)
	assert.Greater(t, commitsAfter, commitsBefore, "Auto-commit should create at least one commit")

	// 4. Verify checkpoint trailer in commit history
	checkpointID, err := env.GetLatestCheckpointIDFromHistory()
	require.NoError(t, err, "Should find checkpoint ID in commit history")
	require.NotEmpty(t, checkpointID, "Commit should have Entire-Checkpoint trailer")
	t.Logf("Checkpoint ID: %s", checkpointID)

	// Verify checkpoint ID format (12 hex characters)
	assert.Len(t, checkpointID, 12, "Checkpoint ID should be 12 characters")

	// 5. Verify metadata branch exists
	assert.True(t, env.BranchExists("entire/checkpoints/v1"),
		"entire/checkpoints/v1 branch should exist")

	// 6. Check for rewind points
	points := env.GetRewindPoints()
	assert.GreaterOrEqual(t, len(points), 1, "Should have at least 1 rewind point")
	t.Logf("Found %d rewind points", len(points))

	// 7. Validate checkpoint has proper metadata on entire/checkpoints/v1
	env.ValidateCheckpoint(CheckpointValidation{
		CheckpointID:              checkpointID,
		Strategy:                  "auto-commit",
		FilesTouched:              []string{"hello.go"},
		ExpectedTranscriptContent: []string{"hello.go"},
	})
}
