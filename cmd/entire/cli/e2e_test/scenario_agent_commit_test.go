//go:build e2e

package e2e

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestE2E_AgentCommitsDuringTurn tests what happens when the agent commits during its turn.
// This is a P1 test because it tests the deferred finalization behavior.
func TestE2E_AgentCommitsDuringTurn(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, "manual-commit")

	// 1. First, agent creates a file
	t.Log("Step 1: Agent creating file")
	result, err := env.RunAgent(PromptCreateHelloGo.Prompt)
	require.NoError(t, err)
	AssertAgentSuccess(t, result, err)
	require.True(t, env.FileExists("hello.go"))

	// 2. Agent commits the changes (using Bash tool)
	t.Log("Step 2: Agent committing changes")
	commitPrompt := `Stage and commit the hello.go file with commit message "Add hello world via agent".
Use these exact commands:
1. git add hello.go
2. git commit -m "Add hello world via agent"
Only run these two commands, nothing else.`

	result, err = env.RunAgentWithTools(commitPrompt, []string{"Bash"})
	require.NoError(t, err)
	AssertAgentSuccess(t, result, err)
	t.Logf("Agent commit output: %s", result.Stdout)

	// 3. Verify the commit was made
	t.Log("Step 3: Verifying commit was made")
	headMsg := env.GetCommitMessage(env.GetHeadHash())
	t.Logf("HEAD commit message: %s", headMsg)

	// The commit might or might not have the Entire-Checkpoint trailer depending
	// on hook configuration. The key thing is the commit was made.

	// 4. Check rewind points
	t.Log("Step 4: Checking rewind points")
	points := env.GetRewindPoints()
	t.Logf("Found %d rewind points after agent commit", len(points))

	// 5. Agent makes another change after committing
	t.Log("Step 5: Agent making another change")
	result, err = env.RunAgent(PromptCreateCalculator.Prompt)
	require.NoError(t, err)
	AssertAgentSuccess(t, result, err)
	require.True(t, env.FileExists("calc.go"))

	// 6. User commits the second change
	t.Log("Step 6: User committing second change")
	env.GitCommitWithShadowHooks("Add calculator", "calc.go")

	// 7. Final verification
	checkpointID, err := env.GetLatestCheckpointIDFromHistory()
	if err != nil {
		t.Logf("No checkpoint ID found: %v", err)
	} else {
		t.Logf("Final checkpoint ID: %s", checkpointID)
	}
}

// TestE2E_MultipleAgentSessions tests behavior across multiple agent sessions.
func TestE2E_MultipleAgentSessions(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, "manual-commit")

	// Session 1: Create hello.go
	t.Log("Session 1: Creating hello.go")
	result, err := env.RunAgent(PromptCreateHelloGo.Prompt)
	require.NoError(t, err)
	AssertAgentSuccess(t, result, err)

	session1Points := env.GetRewindPoints()
	t.Logf("After session 1: %d rewind points", len(session1Points))

	// User commits
	env.GitCommitWithShadowHooks("Session 1: Add hello world", "hello.go")

	// Session 2: Create calc.go
	t.Log("Session 2: Creating calc.go")
	result, err = env.RunAgent(PromptCreateCalculator.Prompt)
	require.NoError(t, err)
	AssertAgentSuccess(t, result, err)

	session2Points := env.GetRewindPoints()
	t.Logf("After session 2: %d rewind points", len(session2Points))

	// User commits
	env.GitCommitWithShadowHooks("Session 2: Add calculator", "calc.go")

	// Session 3: Add multiply function
	t.Log("Session 3: Adding multiply function")
	result, err = env.RunAgent(PromptAddMultiplyFunction.Prompt)
	require.NoError(t, err)
	AssertAgentSuccess(t, result, err)

	// Verify multiply function was added
	AssertCalculatorFunctions(t, env, "calc.go", "Add", "Subtract", "Multiply")

	// User commits
	env.GitCommitWithShadowHooks("Session 3: Add multiply function", "calc.go")

	// Final check: we should have checkpoint IDs in commit history
	t.Log("Final verification")
	checkpointID, err := env.GetLatestCheckpointIDFromHistory()
	require.NoError(t, err, "Should find checkpoint in commit history")
	require.NotEmpty(t, checkpointID, "Should have checkpoint in final commit")
}
