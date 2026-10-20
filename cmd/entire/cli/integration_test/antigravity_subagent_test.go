//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/stretchr/testify/require"
)

// agy runs a subagent as a conversation with its own hooks. The hook order and
// step shapes below are agy 1.4.2's, captured from a live invoke_subagent run:
// the child's transcript does not exist during its first hooks, and the parent
// is resumed by a new execution (invocationNum 0) once the child replies,
// before the child's message reaches the parent's transcript.
func TestAntigravity_SubagentWorkBelongsToTheParent(t *testing.T) {
	t.Parallel()
	env := newAntigravityEnv(t)
	extraEnv := []string{
		"ENTIRE_ANTIGRAVITY_STATUS_DIR=" + t.TempDir(),
		"ENTIRE_ANTIGRAVITY_CONFIG_DIR=" + t.TempDir(),
	}
	const (
		parentID = "4010e6d3-6d62-4c4b-9a0e-1f2b3c4d5e6f"
		childID  = "9fca78fd-a601-4996-a47c-6126c87772a4"
	)
	transcript := func(id string) string {
		return filepath.Join(antigravityBrainDir(env.RepoDir), id, ".system_generated", "logs", "transcript_full.jsonl")
	}
	hook := func(id, name string, fields map[string]any) {
		t.Helper()
		base := map[string]any{
			"conversationId":        id,
			"workspacePaths":        []string{env.RepoDir},
			"transcriptPath":        transcript(id),
			"artifactDirectoryPath": filepath.Join(antigravityBrainDir(env.RepoDir), id),
		}
		require.NoError(t, runAntigravityHookWithEnv(t, env.RepoDir, name, mergeMaps(base, fields), extraEnv), name)
	}
	planner := func(step, input, output int, extra string) string {
		return fmt.Sprintf(`{"step_index":%d,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE"%s,"input_tokens":%d,"cache_read_tokens":0,"output_tokens":%d}`,
			step, extra, input, output)
	}
	stop := map[string]any{"executionNum": 0, "terminationReason": "NO_TOOL_CALL", "error": ""}

	// The parent's prompt and its invoke_subagent call.
	appendLines(t, transcript(parentID),
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"<USER_REQUEST>\nDelegate the change to a subagent\n</USER_REQUEST>"}`)
	hook(parentID, "pre-invocation", map[string]any{"invocationNum": 0, "initialNumSteps": 1})
	appendLines(t, transcript(parentID),
		planner(1, 7071, 62, `,"tool_calls":[{"name":"invoke_subagent","args":{"Prompt":"Create child.txt","Role":"File Writer","TypeName":"research"}}]`),
		fmt.Sprintf(`{"step_index":2,"source":"MODEL","type":"GENERIC","status":"DONE","content":"Created the following subagent:\n{\n  \"conversationId\": \"%s\"\n}"}`, childID))
	hook(parentID, "pre-invocation", map[string]any{"invocationNum": 1, "initialNumSteps": 3})
	appendLines(t, transcript(parentID), planner(3, 7460, 116, ""))
	hook(parentID, "stop", mergeMaps(stop, map[string]any{"fullyIdle": false}))

	// The child: its first call runs before its transcript exists.
	hook(childID, "session-start", nil)
	hook(childID, "pre-invocation", map[string]any{"invocationNum": 0, "initialNumSteps": 0})
	appendLines(t, transcript(childID),
		fmt.Sprintf(`{"step_index":0,"source":"SYSTEM","type":"SYSTEM_MESSAGE","status":"DONE","content":"<SYSTEM_MESSAGE>\n[Message] timestamp=2026-10-20T00:12:09Z sender=%s priority=MESSAGE_PRIORITY_HIGH content=Create child.txt\n</SYSTEM_MESSAGE>"}`, parentID))
	env.WriteFile("child.txt", "from the subagent\n")
	hook(childID, "pre-tool-use", map[string]any{
		"toolCall": map[string]any{"name": "write_to_file", "args": map[string]any{"TargetFile": "child.txt"}},
		"stepIdx":  1,
	})
	appendLines(t, transcript(childID), planner(1, 5846, 119, `,"tool_calls":[{"name":"write_to_file","args":{"TargetFile":"child.txt"}}]`))

	child, err := env.GetSessionState(childID)
	require.NoError(t, err)
	require.NotNil(t, child)
	require.Equal(t, parentID, child.ParentSessionID, "the child's first tool call marks it")
	require.Empty(t, child.FilesTouched, "the child's changes are recorded on the parent")

	// A commit while the child still runs links the parent only, carrying the
	// child's work as its task.
	_, _ = commitCheckpointTokens(t, env, "", "Add child.txt", "child.txt")
	first := env.GetLatestCheckpointID()
	summaryRaw, ok := env.ReadFileFromBranch(paths.MetadataBranchName, CheckpointSummaryPath(first))
	require.True(t, ok)
	var summary checkpoint.CheckpointSummary
	require.NoError(t, json.Unmarshal([]byte(summaryRaw), &summary))
	require.Len(t, summary.Sessions, 1, "the child is never a checkpoint session of its own: %s", summaryRaw)
	_, ok = env.ReadFileFromBranch(paths.MetadataBranchName,
		CheckpointTaskFilePath(first, childID, paths.AgentTranscriptFileName(childID)))
	require.True(t, ok, "the parent's checkpoint carries the child's transcript so far")

	// The child replies and finishes.
	appendLines(t, transcript(childID), planner(3, 6224, 115, `,"tool_calls":[{"name":"send_message","args":{}}]`), planner(5, 6449, 20, ""))
	hook(childID, "stop", mergeMaps(stop, map[string]any{"fullyIdle": true}))

	// The parent resumes, before the child's message is in its transcript.
	hook(parentID, "pre-invocation", map[string]any{"invocationNum": 0, "initialNumSteps": 4})
	appendLines(t, transcript(parentID),
		fmt.Sprintf(`{"step_index":4,"source":"SYSTEM","type":"SYSTEM_MESSAGE","status":"DONE","content":"<SYSTEM_MESSAGE>\n[Message] timestamp=2026-10-20T00:12:15Z sender=%s content=done\n</SYSTEM_MESSAGE>"}`, childID),
		planner(5, 7694, 71, ""))
	env.WriteFile("parent.txt", "from the parent\n")
	hook(parentID, "pre-tool-use", map[string]any{
		"toolCall": map[string]any{"name": "write_to_file", "args": map[string]any{"TargetFile": "parent.txt"}},
		"stepIdx":  6,
	})
	hook(parentID, "stop", mergeMaps(stop, map[string]any{"fullyIdle": true}))

	// The parent's turn kept its start through the wake-up: all three of its
	// calls count, and none of the child's.
	require.Equal(t, &agent.TokenUsage{InputTokens: 7071 + 7460 + 7694, OutputTokens: 62 + 116 + 71, APICallCount: 3},
		sessionTokenTotal(t, env, parentID))
	parent, err := env.GetSessionState(parentID)
	require.NoError(t, err)
	rec := parent.FindTaskRecord(childID)
	require.NotNil(t, rec)
	require.False(t, rec.CompletedAt.IsZero())
	require.Equal(t, &agent.TokenUsage{InputTokens: 5846 + 6224 + 6449, OutputTokens: 119 + 115 + 20, APICallCount: 3}, rec.TokenUsage)
	child, err = env.GetSessionState(childID)
	require.NoError(t, err)
	if child != nil && child.TokenUsage != nil {
		require.Zero(t, child.TokenUsage.InputTokens, "the child's usage is on the parent's task, not its own session")
	}

	_, _ = commitCheckpointTokens(t, env, first, "Add parent.txt", "parent.txt")
	second := env.GetLatestCheckpointID()
	raw, ok := env.ReadFileFromBranch(paths.MetadataBranchName, CheckpointTaskFilePath(second, childID, "task.json"))
	require.True(t, ok, "the completed task is in the next checkpoint")
	var task checkpoint.TaskRecord
	require.NoError(t, json.Unmarshal([]byte(raw), &task))
	require.Equal(t, childID, task.AgentID)
	require.Equal(t, "research", task.SubagentType)
	require.Equal(t, "File Writer", task.TaskDescription)
	require.NotContains(t, task.Files, "child.txt", "the first commit already took child.txt: %s", raw)
	require.Equal(t, 5846+6224+6449, task.TokenUsage.InputTokens, "%s", raw)
}
