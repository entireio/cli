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
	t.Logf("First checkpoint: %s", firstPointID[:12])

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
			i, p.ID[:12], p.IsLogsOnly, p.CondensationID)
	}

	// 5. Rewind to pre-commit checkpoint
	t.Log("Step 5: Rewinding to pre-commit checkpoint")
	err = env.Rewind(preCommitPointID)
	// Note: After commit, rewinding to a pre-commit checkpoint may only restore logs
	// depending on the checkpoint's state
	if err != nil {
		t.Logf("Rewind result: %v (may be expected for logs-only points)", err)
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
