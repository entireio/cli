//go:build e2e

package e2e

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestE2E_BasicWorkflow tests the fundamental workflow:
// Agent creates a file -> User commits -> Checkpoint is created
func TestE2E_BasicWorkflow(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, "manual-commit")

	// 1. Agent creates a file
	t.Log("Step 1: Running agent to create hello.go")
	result, err := env.RunAgent(PromptCreateHelloGo.Prompt)
	require.NoError(t, err, "Agent should succeed")
	AssertAgentSuccess(t, result, err)
	t.Logf("Agent completed in %v", result.Duration)

	// 2. Verify file was created with expected content
	t.Log("Step 2: Verifying file was created")
	require.True(t, env.FileExists("hello.go"), "hello.go should exist")
	AssertHelloWorldProgram(t, env, "hello.go")

	// 3. Verify rewind points exist (session should have created checkpoints)
	t.Log("Step 3: Checking for rewind points")
	points := env.GetRewindPoints()
	assert.GreaterOrEqual(t, len(points), 1, "Should have at least 1 rewind point")
	if len(points) > 0 {
		t.Logf("Found %d rewind point(s), first: %s", len(points), points[0].Message)
	}

	// 4. User commits the changes with hooks
	t.Log("Step 4: Committing changes with hooks")
	env.GitCommitWithShadowHooks("Add hello world program", "hello.go")

	// 5. Verify checkpoint was created (trailer in commit)
	t.Log("Step 5: Verifying checkpoint")
	checkpointID := env.GetLatestCheckpointIDFromHistory()
	assert.NotEmpty(t, checkpointID, "Commit should have Entire-Checkpoint trailer")
	t.Logf("Checkpoint ID: %s", checkpointID)

	// 6. Verify metadata branch exists
	t.Log("Step 6: Checking metadata branch")
	assert.True(t, env.BranchExists("entire/checkpoints/v1"),
		"entire/checkpoints/v1 branch should exist")
}

// TestE2E_MultipleChanges tests multiple agent changes before commit.
func TestE2E_MultipleChanges(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, "manual-commit")

	// 1. First agent action: create hello.go
	t.Log("Step 1: Creating first file")
	result, err := env.RunAgent(PromptCreateHelloGo.Prompt)
	require.NoError(t, err)
	AssertAgentSuccess(t, result, err)
	require.True(t, env.FileExists("hello.go"))

	// 2. Second agent action: create calc.go
	t.Log("Step 2: Creating second file")
	result, err = env.RunAgent(PromptCreateCalculator.Prompt)
	require.NoError(t, err)
	AssertAgentSuccess(t, result, err)
	require.True(t, env.FileExists("calc.go"))

	// 3. Verify multiple rewind points exist
	t.Log("Step 3: Checking rewind points")
	points := env.GetRewindPoints()
	assert.GreaterOrEqual(t, len(points), 2, "Should have at least 2 rewind points")

	// 4. Commit both files
	t.Log("Step 4: Committing all changes")
	env.GitCommitWithShadowHooks("Add hello world and calculator", "hello.go", "calc.go")

	// 5. Verify checkpoint
	checkpointID := env.GetLatestCheckpointIDFromHistory()
	assert.NotEmpty(t, checkpointID)
}
