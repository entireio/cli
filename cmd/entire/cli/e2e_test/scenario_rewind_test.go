//go:build e2e

package e2e

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestE2E_RewindToCheckpoint tests rewinding to a previous checkpoint.
func TestE2E_RewindToCheckpoint(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, "manual-commit")

	// 1. Agent creates first file
	t.Log("Step 1: Creating first file")
	result, err := env.RunAgent(PromptCreateHelloGo.Prompt)
	require.NoError(t, err)
	AssertAgentSuccess(t, result, err)
	require.True(t, env.FileExists("hello.go"))

	// Get first checkpoint
	points1 := env.GetRewindPoints()
	require.GreaterOrEqual(t, len(points1), 1)
	firstPointID := points1[0].ID
	t.Logf("First checkpoint: %s", safeIDPrefix(firstPointID))

	// Save original content
	originalContent := env.ReadFile("hello.go")

	// 2. Agent modifies the file
	t.Log("Step 2: Modifying file")
	result, err = env.RunAgent(PromptModifyHelloGo.Prompt)
	require.NoError(t, err)
	AssertAgentSuccess(t, result, err)

	// Verify content changed
	modifiedContent := env.ReadFile("hello.go")
	assert.NotEqual(t, originalContent, modifiedContent, "Content should have changed")
	assert.Contains(t, modifiedContent, "E2E Test", "Should contain new message")

	// Get second checkpoint
	points2 := env.GetRewindPoints()
	require.GreaterOrEqual(t, len(points2), 2, "Should have at least 2 checkpoints")
	t.Logf("Now have %d checkpoints", len(points2))

	// 3. Rewind to first checkpoint
	t.Log("Step 3: Rewinding to first checkpoint")
	err = env.Rewind(firstPointID)
	require.NoError(t, err)

	// 4. Verify content was restored
	t.Log("Step 4: Verifying content restored")
	restoredContent := env.ReadFile("hello.go")
	assert.Equal(t, originalContent, restoredContent, "Content should be restored to original")
	assert.NotContains(t, restoredContent, "E2E Test", "Should not contain modified message")
}

// TestE2E_RewindAfterCommit tests that pre-commit checkpoint IDs become invalid after commit.
//
// Expected behavior:
// 1. Before commit: checkpoints are on shadow branch with shadow branch commit IDs
// 2. After commit: shadow branch is condensed and deleted
// 3. New logs-only checkpoints appear with DIFFERENT IDs (user commit hashes)
// 4. Attempting to rewind using the old shadow branch ID fails with "not found"
func TestE2E_RewindAfterCommit(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, "manual-commit")

	// 1. Agent creates file
	t.Log("Step 1: Creating file")
	result, err := env.RunAgent(PromptCreateHelloGo.Prompt)
	require.NoError(t, err)
	AssertAgentSuccess(t, result, err)

	// Get checkpoint before commit - this is a shadow branch commit ID
	pointsBefore := env.GetRewindPoints()
	require.GreaterOrEqual(t, len(pointsBefore), 1, "Should have checkpoint before commit")
	preCommitPointID := pointsBefore[0].ID
	t.Logf("Pre-commit checkpoint ID (shadow branch): %s", safeIDPrefix(preCommitPointID))

	// Verify this point is NOT logs-only (it's on shadow branch)
	assert.False(t, pointsBefore[0].IsLogsOnly, "Pre-commit checkpoint should NOT be logs-only")

	// 2. User commits - this condenses and deletes the shadow branch
	t.Log("Step 2: Committing (triggers condensation)")
	env.GitCommitWithShadowHooks("Add hello world", "hello.go")

	// 3. Verify checkpoint was created
	checkpointID, err := env.GetLatestCheckpointIDFromHistory()
	require.NoError(t, err, "Should find checkpoint in commit history")
	t.Logf("Committed checkpoint ID: %s", checkpointID)

	// 4. Get rewind points after commit
	t.Log("Step 3: Getting rewind points after commit")
	pointsAfter := env.GetRewindPoints()
	t.Logf("Found %d rewind points after commit", len(pointsAfter))

	// Find the logs-only point (should be from the user commit)
	var logsOnlyPoint *RewindPoint
	for i, p := range pointsAfter {
		t.Logf("  Point %d: ID=%s, IsLogsOnly=%v, CondensationID=%s",
			i, safeIDPrefix(p.ID), p.IsLogsOnly, p.CondensationID)
		if p.IsLogsOnly {
			pointCopy := p
			logsOnlyPoint = &pointCopy
		}
	}
	require.NotNil(t, logsOnlyPoint, "Should have a logs-only point after commit")

	// The logs-only point should have a DIFFERENT ID than the pre-commit shadow branch ID
	assert.NotEqual(t, preCommitPointID, logsOnlyPoint.ID,
		"Logs-only point ID should differ from shadow branch ID")

	// 5. Attempt to rewind to the OLD shadow branch ID - should fail
	t.Log("Step 4: Attempting rewind to old shadow branch ID (should fail)")
	err = env.Rewind(preCommitPointID)

	// The old shadow branch was deleted, so the ID is no longer valid
	assert.Error(t, err, "Rewind to deleted shadow branch ID should fail")
	assert.Contains(t, err.Error(), "not found",
		"Error should indicate the rewind point was not found")
	t.Logf("Rewind correctly failed: %v", err)
}

// TestE2E_RewindMultipleFiles tests rewinding changes across multiple files.
func TestE2E_RewindMultipleFiles(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, "manual-commit")

	// 1. Agent creates multiple files
	t.Log("Step 1: Creating first file")
	result, err := env.RunAgent(PromptCreateHelloGo.Prompt)
	require.NoError(t, err)
	AssertAgentSuccess(t, result, err)

	// Get checkpoint after first file
	points1 := env.GetRewindPoints()
	require.GreaterOrEqual(t, len(points1), 1)
	afterFirstFile := points1[0].ID

	t.Log("Step 2: Creating second file")
	result, err = env.RunAgent(PromptCreateCalculator.Prompt)
	require.NoError(t, err)
	AssertAgentSuccess(t, result, err)

	// Verify both files exist
	require.True(t, env.FileExists("hello.go"))
	require.True(t, env.FileExists("calc.go"))

	// 3. Rewind to after first file (before second)
	t.Log("Step 3: Rewinding to after first file")
	err = env.Rewind(afterFirstFile)
	require.NoError(t, err)

	// 4. Verify only first file exists
	assert.True(t, env.FileExists("hello.go"), "hello.go should still exist")
	assert.False(t, env.FileExists("calc.go"), "calc.go should be removed by rewind")
}
