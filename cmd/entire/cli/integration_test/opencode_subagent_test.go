//go:build integration

package integration

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/stretchr/testify/require"
)

// TestOpenCodeSubagentTaskRecord drives the OpenCode subagent contract through
// the real hook binary: the parent turn starts, a child session is announced
// and completed, and the commit condenses the parent with the child's work as
// a task record — never as a session of its own.
func TestOpenCodeSubagentTaskRecord(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.InitEntireWithAgent(agent.AgentNameOpenCode)

	parent := env.NewOpenCodeSession()
	child := env.NewOpenCodeSession() // stands in for the task tool's child session
	const toolUseID = "call_red_1"

	require.NoError(t, env.SimulateOpenCodeSessionStart(parent.ID, parent.TranscriptPath))
	require.NoError(t, env.SimulateOpenCodeTurnStart(parent.ID, parent.TranscriptPath, "use a subagent to create docs/red.md"))

	require.NoError(t, env.SimulateOpenCodeSubagentStart(parent.ID, toolUseID, child.ID, "general", "Create docs/red.md"))
	state, err := env.GetSessionState(parent.ID)
	require.NoError(t, err)
	live := state.FindTaskRecord(toolUseID)
	require.NotNil(t, live, "subagent-start must leave an in-flight record on the parent")
	require.True(t, live.CompletedAt.IsZero())
	require.Equal(t, child.ID, live.AgentID)

	// The child does the work: file on disk plus its own export naming the write.
	env.WriteFile("docs/red.md", "Red is a warm colour.\n")
	childTranscript := child.CreateOpenCodeTranscript("Create docs/red.md", []FileChange{
		{Path: "docs/red.md", Content: "Red is a warm colour.\n"},
	})
	env.CopyTranscriptToEntireTmp(child.ID, childTranscript)
	require.NoError(t, env.SimulateOpenCodeSubagentStop(parent.ID, toolUseID, child.ID, "general", "Create docs/red.md"))

	state, err = env.GetSessionState(parent.ID)
	require.NoError(t, err)
	rec := state.FindTaskRecord(toolUseID)
	require.NotNil(t, rec)
	require.False(t, rec.CompletedAt.IsZero(), "subagent-stop must complete the record")
	require.Equal(t, []string{"docs/red.md"}, rec.Files, "files must come from the child's own export")
	require.Contains(t, state.FilesTouched, "docs/red.md")
	require.NotEmpty(t, rec.DeclaredTranscriptPath)
	require.False(t, rec.TranscriptUnavailable)
	require.NotNil(t, rec.TokenUsage, "child tokens are exact and must be recorded")

	// No child session state may exist: the child is a task, not a session.
	// GetSessionState returns (nil, nil) for a missing state file.
	childState, err := env.GetSessionState(child.ID)
	require.NoError(t, err)
	require.Nil(t, childState, "child session must not have its own Entire session state")

	// A stop whose child export cannot be fetched still completes the record,
	// marked transcript-unavailable (spec acceptance criterion). No copy to
	// .entire/tmp precedes this call, so the mock export fails.
	const orphanToolUseID = "call_orphan_2"
	require.NoError(t, env.SimulateOpenCodeSubagentStop(parent.ID, orphanToolUseID, "opencode-session-missing", "explore", "Look around"))
	state, err = env.GetSessionState(parent.ID)
	require.NoError(t, err)
	orphan := state.FindTaskRecord(orphanToolUseID)
	require.NotNil(t, orphan)
	require.False(t, orphan.CompletedAt.IsZero())
	require.True(t, orphan.TranscriptUnavailable)
	require.Empty(t, orphan.Files)

	// Parent turn ends with the child's file present, then the user commits.
	// GitCommitWithShadowHooks (TTY shape) is deliberate and matches
	// opencode_hooks_test.go; codex_subagent_test.go's ...AsAgent variant is
	// the agent-commit shape and is not what this test is about.
	parent.CreateOpenCodeTranscript("use a subagent to create docs/red.md", nil)
	require.NoError(t, env.SimulateOpenCodeTurnEnd(parent.ID, parent.TranscriptPath))
	env.GitCommitWithShadowHooks("Add red.md via subagent", "docs/red.md")

	checkpointID := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, checkpointID)
	_, ok := env.ReadFileFromBranch(paths.MetadataBranchName, CheckpointTaskFilePath(checkpointID, toolUseID, "task.json"))
	require.True(t, ok, "task.json must be materialized under the parent checkpoint's tasks/ subtree")
	stored, ok := env.ReadFileFromBranch(paths.MetadataBranchName, CheckpointTaskFilePath(checkpointID, toolUseID, paths.AgentTranscriptFileName(child.ID)))
	require.True(t, ok, "the child's export must be materialized as the task transcript")
	require.Contains(t, stored, "docs/red.md")
}
