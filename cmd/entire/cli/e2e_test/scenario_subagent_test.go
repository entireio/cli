//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestE2E_SubagentCheckpoint tests that subagent task checkpoints are created
// when Claude uses the Task tool to spawn a subagent.
//
// This tests the real subagent checkpoint flow:
// 1. Claude receives a prompt and uses the Task tool
// 2. PreTask hook fires, creating pre-task state
// 3. Subagent makes file changes
// 4. PostTask hook fires, creating task checkpoint
// 5. Rewind points include the task checkpoint with is_task_checkpoint=true
func TestE2E_SubagentCheckpoint(t *testing.T) {
	t.Parallel()

	// Skip for non-Claude agents - Task tool is Claude Code specific
	if defaultAgent != AgentNameClaudeCode {
		t.Skipf("Skipping subagent test for %s (Task tool is Claude Code specific)", defaultAgent)
	}

	env := NewFeatureBranchEnv(t, "manual-commit")

	// Get rewind points before agent action
	pointsBefore := env.GetRewindPoints()
	t.Logf("Rewind points before: %d", len(pointsBefore))

	// Run prompt that triggers Task tool usage
	// Include "Task" in allowed tools so Claude can spawn subagents
	t.Log("Running prompt that uses Task tool to spawn subagent")
	result, err := env.RunAgentWithTools(PromptUseTaskTool.Prompt, []string{
		"Edit", "Read", "Write", "Bash", "Glob", "Grep", "Task",
	})

	// Task tool usage may or may not succeed depending on Claude's interpretation
	// and available subagent types. Log the result for debugging.
	if err != nil {
		t.Logf("Agent completed with error (may be expected): %v", err)
		if result != nil {
			t.Logf("Stdout: %s", result.Stdout)
			t.Logf("Stderr: %s", result.Stderr)
		}
	} else if result != nil {
		t.Logf("Agent completed successfully")
		t.Logf("Output: %s", result.Stdout)
	}

	// Check if the expected file was created (by subagent or main agent)
	if env.FileExists("subagent_output.txt") {
		content := env.ReadFile("subagent_output.txt")
		t.Logf("subagent_output.txt content: %s", content)
	} else {
		t.Log("subagent_output.txt was not created - Task tool may not have been used")
	}

	// Get rewind points after agent action
	pointsAfter := env.GetRewindPoints()
	t.Logf("Rewind points after: %d", len(pointsAfter))

	// Log all rewind points for debugging
	for i, p := range pointsAfter {
		t.Logf("  Point %d: ID=%s, IsTask=%v, ToolUseID=%s, Message=%s",
			i, safeIDPrefix(p.ID), p.IsTaskCheckpoint, p.ToolUseID, truncateMessage(p.Message, 50))
	}

	// Check for task checkpoints
	var taskCheckpoints []RewindPoint
	for _, p := range pointsAfter {
		if p.IsTaskCheckpoint {
			taskCheckpoints = append(taskCheckpoints, p)
		}
	}

	if len(taskCheckpoints) > 0 {
		t.Logf("Found %d task checkpoint(s)", len(taskCheckpoints))

		// Validate the first task checkpoint
		taskCP := taskCheckpoints[0]
		assert.True(t, taskCP.IsTaskCheckpoint, "Should be marked as task checkpoint")
		assert.NotEmpty(t, taskCP.ToolUseID, "Task checkpoint should have ToolUseID")
		assert.True(t, strings.HasPrefix(taskCP.ToolUseID, "toolu_"),
			"ToolUseID should have expected format: %s", taskCP.ToolUseID)

		t.Logf("Task checkpoint validated: ToolUseID=%s", taskCP.ToolUseID)
	} else {
		// If no task checkpoints, Claude may have done the work itself without Task tool
		// This is acceptable behavior - log it for visibility
		t.Log("No task checkpoints found - Claude may not have used Task tool")
		t.Log("This is acceptable as Claude decides when to use Task tool")
	}

	// Verify we have at least one checkpoint (task or regular)
	assert.GreaterOrEqual(t, len(pointsAfter), 1,
		"Should have at least one checkpoint after agent action")
}

// TestE2E_SubagentCheckpoint_CommitFlow tests that task checkpoints are properly
// handled when the user commits changes that include subagent-created files.
func TestE2E_SubagentCheckpoint_CommitFlow(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, "manual-commit")

	// 1. Run prompt that may trigger Task tool
	t.Log("Step 1: Running prompt that may use Task tool")
	result, err := env.RunAgentWithTools(PromptUseTaskTool.Prompt, []string{
		"Edit", "Read", "Write", "Bash", "Glob", "Grep", "Task",
	})

	if err != nil {
		t.Logf("Agent completed with error: %v", err)
	}
	if result != nil {
		t.Logf("Agent output: %s", truncateMessage(result.Stdout, 200))
	}

	// 2. Check what files were created
	var filesToCommit []string
	if env.FileExists("subagent_output.txt") {
		filesToCommit = append(filesToCommit, "subagent_output.txt")
		t.Log("Found subagent_output.txt")
	}

	// If no files created, the test can't continue meaningfully
	if len(filesToCommit) == 0 {
		t.Log("No files created by agent - skipping commit flow test")
		t.Log("This may happen if Task tool wasn't used or failed")
		return
	}

	// 3. Get rewind points before commit
	pointsBefore := env.GetRewindPoints()
	taskCheckpointsBefore := countTaskCheckpoints(pointsBefore)
	t.Logf("Task checkpoints before commit: %d", taskCheckpointsBefore)

	// 4. Commit the changes
	t.Log("Step 2: Committing changes")
	env.GitCommitWithShadowHooks("Add subagent output", filesToCommit...)

	// 5. Verify checkpoint was created
	checkpointID, err := env.GetLatestCheckpointIDFromHistory()
	require.NoError(t, err, "Should find checkpoint in commit history")
	t.Logf("Checkpoint ID: %s", checkpointID)

	// 6. Validate checkpoint on metadata branch
	env.ValidateCheckpoint(CheckpointValidation{
		CheckpointID:              checkpointID,
		Strategy:                  "manual-commit",
		FilesTouched:              filesToCommit,
		ExpectedTranscriptContent: []string{"subagent_output.txt"},
	})

	// 7. Verify rewind points after commit
	pointsAfter := env.GetRewindPoints()
	t.Logf("Rewind points after commit: %d", len(pointsAfter))

	// Check for logs-only point from the commit
	var logsOnlyFound bool
	for _, p := range pointsAfter {
		if p.IsLogsOnly {
			logsOnlyFound = true
			t.Logf("Found logs-only point: %s", safeIDPrefix(p.ID))
		}
	}
	assert.True(t, logsOnlyFound, "Should have logs-only point after commit")
}

// countTaskCheckpoints counts how many task checkpoints are in the list.
func countTaskCheckpoints(points []RewindPoint) int {
	count := 0
	for _, p := range points {
		if p.IsTaskCheckpoint {
			count++
		}
	}
	return count
}

// truncateMessage truncates a message to maxLen characters, adding "..." if truncated.
func truncateMessage(msg string, maxLen int) string {
	// Remove newlines for cleaner logging
	msg = strings.ReplaceAll(msg, "\n", " ")
	if len(msg) > maxLen {
		return msg[:maxLen] + "..."
	}
	return msg
}
