//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/stretchr/testify/require"
)

// Cursor names a subagent's model only in subagentStart's subagent_model, and
// reports no subagent tokens to label with it, so the model is kept on the
// task record and stored in task.json.
func TestCursorSubagentModel_RecordedOnTask(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.InitEntireWithAgent(agent.AgentNameCursor)

	cursorProjectDir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(cursorProjectDir); err == nil {
		cursorProjectDir = resolved
	}

	const conversationID = "cursor-subagent-model-session"
	const subagentID = "sub_model_1"

	transcriptDir := filepath.Join(cursorProjectDir, conversationID)
	require.NoError(t, os.MkdirAll(transcriptDir, 0o755))
	transcriptPath := filepath.Join(transcriptDir, conversationID+".jsonl")
	require.NoError(t, os.WriteFile(transcriptPath,
		[]byte(`{"type":"user","text":"delegate a feature"}`+"\n"+
			`{"type":"assistant","text":"done"}`+"\n"), 0o600))
	subagentTranscript := filepath.Join(transcriptDir, subagentID+".jsonl")
	require.NoError(t, os.WriteFile(subagentTranscript,
		[]byte(`{"type":"user","text":"write the feature"}`+"\n"+
			`{"type":"assistant","text":"written"}`+"\n"), 0o600))

	runCursorHook(t, env, cursorProjectDir, "session-start", map[string]any{
		"conversation_id": conversationID,
		"transcript_path": transcriptPath,
		"model":           "cursor-default",
	})
	runCursorHook(t, env, cursorProjectDir, "before-submit-prompt", map[string]any{
		"conversation_id": conversationID,
		"transcript_path": transcriptPath,
		"prompt":          "delegate a feature",
	})
	runCursorHook(t, env, cursorProjectDir, "subagent-start", map[string]any{
		"conversation_id": conversationID,
		"transcript_path": transcriptPath,
		"subagent_id":     subagentID,
		"subagent_type":   "generalPurpose",
		"subagent_model":  "claude-4.5-haiku",
		"task":            "write the feature",
	})
	env.WriteFile("feature.go", "package main\n// feature\n")
	runCursorHook(t, env, cursorProjectDir, "subagent-stop", map[string]any{
		"conversation_id":       conversationID,
		"transcript_path":       transcriptPath,
		"subagent_id":           subagentID,
		"subagent_type":         "generalPurpose",
		"task":                  "write the feature",
		"modified_files":        []string{"feature.go"},
		"agent_transcript_path": subagentTranscript,
	})
	runCursorHook(t, env, cursorProjectDir, "stop", map[string]any{
		"conversation_id": conversationID,
		"transcript_path": transcriptPath,
		"model":           "cursor-default",
		"loop_count":      1,
	})
	env.GitCommitWithHooks("Add feature", "feature.go")

	checkpointID := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, checkpointID, "expected a condensed checkpoint after commit")
	raw, ok := env.ReadFileFromBranch(paths.MetadataBranchName, CheckpointTaskFilePath(checkpointID, subagentID, "task.json"))
	require.True(t, ok, "task.json not materialized for the Cursor subagent")
	var task checkpoint.TaskRecord
	require.NoError(t, json.Unmarshal([]byte(raw), &task))
	require.Equal(t, "claude-4.5-haiku", task.SubagentModel, "task.json must record the subagent's model: %s", raw)
	require.Nil(t, task.TokenUsage, "Cursor reports no subagent tokens; the model must not invent any: %s", raw)
}
