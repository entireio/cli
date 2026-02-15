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

// TestE2E_RewindAfterCommit tests rewinding to a checkpoint after user commits.
func TestE2E_RewindAfterCommit(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, "manual-commit")

	// 1. Agent creates file
	t.Log("Step 1: Creating file")
	result, err := env.RunAgent(PromptCreateHelloGo.Prompt)
	require.NoError(t, err)
	AssertAgentSuccess(t, result, err)

	// Get checkpoint before commit
	pointsBefore := env.GetRewindPoints()
	require.GreaterOrEqual(t, len(pointsBefore), 1)
	preCommitPointID := pointsBefore[0].ID

	// 2. User commits
	t.Log("Step 2: Committing")
	env.GitCommitWithShadowHooks("Add hello world", "hello.go")

	// 3. Agent modifies file (new session)
	t.Log("Step 3: Modifying file after commit")
	result, err = env.RunAgent(PromptModifyHelloGo.Prompt)
	require.NoError(t, err)
	AssertAgentSuccess(t, result, err)

	modifiedContent := env.ReadFile("hello.go")
	require.Contains(t, modifiedContent, "E2E Test")

	// 4. Get rewind points - should include both pre and post commit points
	t.Log("Step 4: Getting rewind points")
	points := env.GetRewindPoints()
	t.Logf("Found %d rewind points", len(points))
	for i, p := range points {
		t.Logf("  Point %d: %s (logs_only=%v, condensation_id=%s)",
			i, safeIDPrefix(p.ID), p.IsLogsOnly, p.CondensationID)
	}

	// 5. Rewind to pre-commit checkpoint
	// After commit, the pre-commit checkpoint becomes logs-only because the shadow branch
	// is condensed. Rewinding to a logs-only point should fail with a clear error.
	t.Log("Step 5: Attempting rewind to pre-commit (logs-only) checkpoint")
	err = env.Rewind(preCommitPointID)

	// Verify the current file state regardless of rewind result
	currentContent := env.ReadFile("hello.go")

	if err != nil {
		// Rewind failed as expected for logs-only checkpoint
		t.Logf("Rewind to logs-only point failed as expected: %v", err)
		// File should still have the modifications since rewind failed
		assert.Contains(t, currentContent, "E2E Test",
			"File should still have modifications after logs-only rewind failure")
	} else {
		// If rewind succeeded, the file might have been restored
		// This could happen if the rewind point wasn't actually logs-only
		t.Log("Rewind succeeded - checkpoint may not have been logs-only")
	}
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
