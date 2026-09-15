//go:build integration

package integration

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/stretchr/testify/require"
)

// TestOpenCodeSubagentTaskRecord drives the OpenCode subagent contract through
// the real hook binary: the parent turn starts, two concurrent children and a
// read-only child are announced and completed, and the commit condenses the
// parent with each child's work as an independent task record rather than as
// a session of its own.
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
	// Exact values, not just presence: CreateOpenCodeTranscript's single
	// assistant message writes input=150, output=80, cache.read=5,
	// cache.write=15 (see hooks.go).
	require.NotNil(t, rec.TokenUsage, "child tokens are exact and must be recorded")
	require.Equal(t, 150, rec.TokenUsage.InputTokens)
	require.Equal(t, 80, rec.TokenUsage.OutputTokens)
	require.Equal(t, 5, rec.TokenUsage.CacheReadTokens)
	require.Equal(t, 15, rec.TokenUsage.CacheCreationTokens)
	require.Equal(t, 1, rec.TokenUsage.APICallCount)

	// Suppression of the child's own lifecycle hooks is plugin-side
	// (childSessions in entire_plugin.ts; see hooks_test.go and the e2e
	// single-session assertion). This only checks the Go handlers write no
	// child state.
	childState, err := env.GetSessionState(child.ID)
	require.NoError(t, err)
	require.Nil(t, childState, "the Go side creates no child session state from subagent-start/subagent-stop alone")

	// A stop whose child export cannot be fetched still completes the record,
	// marked transcript-unavailable. No copy to .entire/tmp precedes this
	// call, so the mock export fails.
	const orphanToolUseID = "call_orphan_2"
	require.NoError(t, env.SimulateOpenCodeSubagentStop(parent.ID, orphanToolUseID, "opencode-session-missing", "explore", "Look around"))
	state, err = env.GetSessionState(parent.ID)
	require.NoError(t, err)
	orphan := state.FindTaskRecord(orphanToolUseID)
	require.NotNil(t, orphan)
	require.False(t, orphan.CompletedAt.IsZero())
	require.True(t, orphan.TranscriptUnavailable)
	require.Empty(t, orphan.Files)

	// Two children started before either stops: records must stay independent
	// by ToolUseID/AgentID.
	child2 := env.NewOpenCodeSession()
	const toolUseID2 = "call_blue_2"
	child3 := env.NewOpenCodeSession()
	const toolUseID3 = "call_look_3"

	require.NoError(t, env.SimulateOpenCodeSubagentStart(parent.ID, toolUseID2, child2.ID, "general", "Create docs/blue.md"))
	require.NoError(t, env.SimulateOpenCodeSubagentStart(parent.ID, toolUseID3, child3.ID, "explore", "Look around some more"))

	env.WriteFile("docs/blue.md", "Blue is a cool colour.\n")
	child2Transcript := child2.CreateOpenCodeTranscript("Create docs/blue.md", []FileChange{
		{Path: "docs/blue.md", Content: "Blue is a cool colour.\n"},
	})
	env.CopyTranscriptToEntireTmp(child2.ID, child2Transcript)
	require.NoError(t, env.SimulateOpenCodeSubagentStop(parent.ID, toolUseID2, child2.ID, "general", "Create docs/blue.md"))

	// A read-only child: its export carries no `write` tool parts at all
	// (nil FileChanges), so it must complete with no files but a real,
	// available transcript.
	child3Transcript := child3.CreateOpenCodeTranscript("Look around some more", nil)
	env.CopyTranscriptToEntireTmp(child3.ID, child3Transcript)
	require.NoError(t, env.SimulateOpenCodeSubagentStop(parent.ID, toolUseID3, child3.ID, "explore", "Look around some more"))

	state, err = env.GetSessionState(parent.ID)
	require.NoError(t, err)

	rec2 := state.FindTaskRecord(toolUseID2)
	require.NotNil(t, rec2)
	require.False(t, rec2.CompletedAt.IsZero())
	require.Equal(t, []string{"docs/red.md"}, rec.Files)
	require.Equal(t, []string{"docs/blue.md"}, rec2.Files)
	require.NotEqual(t, rec.AgentID, rec2.AgentID, "concurrent children must be tracked under distinct agent IDs")

	rec3 := state.FindTaskRecord(toolUseID3)
	require.NotNil(t, rec3)
	require.False(t, rec3.CompletedAt.IsZero())
	require.Empty(t, rec3.Files, "a read-only child modifies nothing")
	require.False(t, rec3.TranscriptUnavailable)
	require.NotEmpty(t, rec3.DeclaredTranscriptPath)
	require.NotNil(t, rec3.TokenUsage)

	// Parent turn ends with both children's files present, then the user
	// commits (TTY shape, as in opencode_hooks_test.go).
	parent.CreateOpenCodeTranscript("use a subagent to create docs/red.md", nil)
	require.NoError(t, env.SimulateOpenCodeTurnEnd(parent.ID, parent.TranscriptPath))
	env.GitCommitWithShadowHooks("Add red.md via subagent", "docs/red.md", "docs/blue.md")

	checkpointID := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, checkpointID)

	for _, tid := range []string{toolUseID, toolUseID2, toolUseID3} {
		_, ok := env.ReadFileFromBranch(paths.MetadataBranchName, CheckpointTaskFilePath(checkpointID, tid, "task.json"))
		require.True(t, ok, "task.json must be materialized under the parent checkpoint's tasks/ subtree for %s", tid)
	}

	stored, ok := env.ReadFileFromBranch(paths.MetadataBranchName, CheckpointTaskFilePath(checkpointID, toolUseID, paths.AgentTranscriptFileName(child.ID)))
	require.True(t, ok, "the first child's export must be materialized as the task transcript")
	require.Contains(t, stored, "docs/red.md")

	stored2, ok := env.ReadFileFromBranch(paths.MetadataBranchName, CheckpointTaskFilePath(checkpointID, toolUseID2, paths.AgentTranscriptFileName(child2.ID)))
	require.True(t, ok, "the second concurrent child's export must be materialized as its own task transcript")
	require.Contains(t, stored2, "docs/blue.md")
}
