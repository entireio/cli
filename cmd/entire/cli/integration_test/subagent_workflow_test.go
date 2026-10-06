//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/stretchr/testify/require"
)

// workflowRunID is the run directory Claude Code names after the Workflow run
// (shape from a real Claude Code 2.1.291 run).
const workflowRunID = "wf_e5264e60-494"

type workflowAgentFixture struct {
	id    string
	file  string
	usage map[string]int
}

// TestClaudeWorkflowAgents_GetTaskRecordsAndSubagentTokens is the #2685
// regression, driven through the real hook binary in Claude Code 2.1.291's
// hook order. A Workflow launches its agents in the background without an
// Agent call each: the Workflow's PostToolUse names a run, not agents. Each
// agent fires SubagentStart (agent_type "workflow-subagent") and, after the
// parent's own Stop, SubagentStop with its transcript under
// subagents/workflows/<runId>/. The commit that follows must carry one task
// record per agent — transcript, files and token usage — and the session's
// subagent_tokens must include all three agents, without folding them into
// the parent's own usage.
func TestClaudeWorkflowAgents_GetTaskRecordsAndSubagentTokens(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	session := env.NewSession()

	agents := []workflowAgentFixture{
		{id: "ae3d7b8f2930c8787", file: "a.txt", usage: map[string]int{"input_tokens": 2, "cache_creation_input_tokens": 24379, "cache_read_input_tokens": 0, "output_tokens": 169}},
		{id: "ac82c55f48a03882b", file: "b.txt", usage: map[string]int{"input_tokens": 3, "cache_creation_input_tokens": 2674, "cache_read_input_tokens": 24379, "output_tokens": 145}},
		{id: "a6d78754c07df829a", file: "c.txt", usage: map[string]int{"input_tokens": 5, "cache_creation_input_tokens": 100, "cache_read_input_tokens": 200, "output_tokens": 7}},
	}

	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}
	writeWorkflowParentTranscript(t, session.TranscriptPath)

	// Launch: one SubagentStart per workflow agent. A direct Agent launch also
	// fires SubagentStart; it must not add a record (PostToolUse[Agent] owns it).
	for _, a := range agents {
		require.NoError(t, env.SimulateSubagentStart(SubagentStartInput{
			SessionID: session.ID, TranscriptPath: session.TranscriptPath,
			AgentID: a.id, AgentType: "workflow-subagent",
		}))
	}
	require.NoError(t, env.SimulateSubagentStart(SubagentStartInput{
		SessionID: session.ID, TranscriptPath: session.TranscriptPath,
		AgentID: "a0000000000direct", AgentType: "general-purpose",
	}))

	state, err := env.GetSessionState(session.ID)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.Len(t, state.TaskRecords, len(agents), "one live record per workflow agent, none for the direct launch")
	for _, a := range agents {
		require.True(t, hasLiveTaskRecord(state, a.id), "expected a live record keyed by workflow agent %s", a.id)
	}

	// The agents work; the parent's turn ends before any of them stops.
	runDir := filepath.Join(paths.SubagentsDir(filepath.Dir(session.TranscriptPath), session.ID),
		paths.SubagentWorkflowsDirName, workflowRunID)
	require.NoError(t, os.MkdirAll(runDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(runDir, "journal.jsonl"), []byte(`{"type":"launched"}`+"\n"), 0o600))
	transcripts := make(map[string]string, len(agents))
	for i, a := range agents {
		transcripts[a.id] = writeWorkflowAgentTranscript(t, runDir, i, a)
		env.WriteFile(a.file, fmt.Sprintf("line from agent %d\n", i+1))
	}
	require.NoError(t, env.SimulateStop(session.ID, session.TranscriptPath))

	for _, a := range agents {
		require.NoError(t, env.SimulateSubagentStop(SubagentStopInput{
			SessionID: session.ID, TranscriptPath: session.TranscriptPath,
			AgentID: a.id, AgentType: "workflow-subagent", AgentTranscriptPath: transcripts[a.id],
		}))
	}

	state, err = env.GetSessionState(session.ID)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.Empty(t, state.LiveTaskRecords(), "every workflow agent's SubagentStop must complete its record")

	files := make([]string, 0, len(agents))
	for _, a := range agents {
		files = append(files, a.file)
	}
	env.GitCommitWithShadowHooksAsAgent("Append workflow lines", files...)
	checkpointID := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, checkpointID, "expected a condensed checkpoint after committing the workflow agents' work")

	wantSubagent := map[string]int{}
	for _, a := range agents {
		assertWorkflowTaskRecord(t, env, checkpointID, a)
		for k, v := range a.usage {
			wantSubagent[k] += v
		}
	}

	raw, ok := env.ReadFileFromBranch(paths.MetadataBranchName, SessionMetadataPath(checkpointID))
	require.True(t, ok, "session metadata.json missing from checkpoint")
	var metadata struct {
		TokenUsage struct {
			InputTokens    int `json:"input_tokens"`
			OutputTokens   int `json:"output_tokens"`
			SubagentTokens *struct {
				InputTokens         int `json:"input_tokens"`
				CacheCreationTokens int `json:"cache_creation_tokens"`
				CacheReadTokens     int `json:"cache_read_tokens"`
				OutputTokens        int `json:"output_tokens"`
				APICallCount        int `json:"api_call_count"`
			} `json:"subagent_tokens"`
		} `json:"token_usage"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &metadata))
	require.Equal(t, 300, metadata.TokenUsage.InputTokens, "parent usage must not absorb the agents': %s", raw)
	require.Equal(t, 30, metadata.TokenUsage.OutputTokens, "parent usage must not absorb the agents': %s", raw)
	sub := metadata.TokenUsage.SubagentTokens
	require.NotNil(t, sub, "session subagent_tokens must include the workflow agents: %s", raw)
	require.Equal(t, wantSubagent["input_tokens"], sub.InputTokens, raw)
	require.Equal(t, wantSubagent["cache_creation_input_tokens"], sub.CacheCreationTokens, raw)
	require.Equal(t, wantSubagent["cache_read_input_tokens"], sub.CacheReadTokens, raw)
	require.Equal(t, wantSubagent["output_tokens"], sub.OutputTokens, raw)
	require.Equal(t, len(agents), sub.APICallCount, raw)
}

// TestClaudeWorkflowAgent_CommitWhileRunning_MaterializesRunTranscript covers
// a commit landing while a Workflow agent is still running: its record is live
// (SubagentStart seen, no SubagentStop yet), so no transcript path was ever
// declared for it. Condensation must find the transcript-so-far through the
// layout fallback, in subagents/workflows/<runId>/, and store it under the
// checkpoint's tasks/<agent_id>/.
func TestClaudeWorkflowAgent_CommitWhileRunning_MaterializesRunTranscript(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	session := env.NewSession()
	a := workflowAgentFixture{id: "ae3d7b8f2930c8787", file: "a.txt", usage: map[string]int{"input_tokens": 2, "output_tokens": 169}}

	// Real UserPromptSubmit payloads carry transcript_path; the layout
	// fallback resolves relative to it.
	writeWorkflowParentTranscript(t, session.TranscriptPath)
	require.NoError(t, env.SimulateUserPromptSubmitWithTranscriptPath(session.ID, session.TranscriptPath))
	require.NoError(t, env.SimulateSubagentStart(SubagentStartInput{
		SessionID: session.ID, TranscriptPath: session.TranscriptPath,
		AgentID: a.id, AgentType: "workflow-subagent",
	}))

	runDir := filepath.Join(paths.SubagentsDir(filepath.Dir(session.TranscriptPath), session.ID),
		paths.SubagentWorkflowsDirName, workflowRunID)
	writeWorkflowAgentTranscript(t, runDir, 0, a)
	env.WriteFile(a.file, "line from agent 1\n")
	require.NoError(t, env.SimulateStop(session.ID, session.TranscriptPath))

	state, err := env.GetSessionState(session.ID)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.True(t, hasLiveTaskRecord(state, a.id), "the agent is still running at commit time")

	env.GitCommitWithShadowHooksAsAgent("Append while the workflow runs", a.file)
	checkpointID := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, checkpointID)

	transcript, ok := env.ReadFileFromBranch(paths.MetadataBranchName,
		CheckpointTaskFilePath(checkpointID, a.id, paths.AgentTranscriptFileName(a.id)))
	require.True(t, ok, "the running workflow agent's transcript was not materialized from its run directory")
	require.Contains(t, transcript, a.file)
}

// writeWorkflowParentTranscript writes the parent's transcript: the Workflow
// call and its async_launched result, which names the run ("Run ID: …", as
// Claude Code 2.1.291 writes it) but no agent IDs, with usage of its own on two
// API calls.
func writeWorkflowParentTranscript(t *testing.T, path string) {
	t.Helper()
	result := "Workflow launched in background. Task ID: w73qhtvvh\nSummary: Three agents each append a line to their own file\n" +
		"Run ID: " + workflowRunID + "\nTranscript dir: <session>/subagents/workflows/" + workflowRunID + "\n"
	writeJSONLines(t, path,
		map[string]any{"type": "user", "uuid": "u1", "message": map[string]any{"role": "user", "content": "Run a workflow with three agents"}},
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{
			"id": "msg_parent_1", "role": "assistant",
			"content": []any{map[string]any{"type": "tool_use", "id": "toolu_workflow1", "name": "Workflow", "input": map[string]any{"name": "append-lines"}}},
			"usage":   map[string]int{"input_tokens": 100, "output_tokens": 10},
		}},
		map[string]any{"type": "user", "uuid": "u2", "message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_workflow1", "content": result},
		}}},
		map[string]any{"type": "assistant", "uuid": "a2", "message": map[string]any{
			"id": "msg_parent_2", "role": "assistant",
			"content": []any{map[string]any{"type": "text", "text": "The workflow is running in the background."}},
			"usage":   map[string]int{"input_tokens": 200, "output_tokens": 20},
		}},
	)
}

// writeWorkflowAgentTranscript writes agent a's transcript and meta file in
// the run directory and returns the transcript path. The transcript writes the
// agent's file and carries one API call's usage.
func writeWorkflowAgentTranscript(t *testing.T, runDir string, i int, a workflowAgentFixture) string {
	t.Helper()
	path := filepath.Join(runDir, paths.AgentTranscriptFileName(a.id))
	writeJSONLines(t, path,
		map[string]any{"type": "user", "isSidechain": true, "agentId": a.id, "message": map[string]any{"role": "user", "content": "Append a line to " + a.file}},
		map[string]any{"type": "assistant", "isSidechain": true, "agentId": a.id, "message": map[string]any{
			"id": fmt.Sprintf("msg_agent_%d", i), "role": "assistant",
			"content": []any{map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu_agent_%d", i), "name": "Write",
				"input": map[string]any{"file_path": a.file, "content": fmt.Sprintf("line from agent %d\n", i+1)}}},
			"usage": a.usage,
		}},
	)
	meta := fmt.Sprintf(`{"agentType":"workflow-subagent","description":"agent %d","workflowPhase":"Append","spawnDepth":1}`, i+1)
	require.NoError(t, os.WriteFile(filepath.Join(runDir, "agent-"+a.id+".meta.json"), []byte(meta), 0o600))
	return path
}

func writeJSONLines(t *testing.T, path string, lines ...map[string]any) {
	t.Helper()
	var b strings.Builder
	for _, line := range lines {
		data, err := json.Marshal(line)
		require.NoError(t, err)
		b.Write(data)
		b.WriteByte('\n')
	}
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(b.String()), 0o600))
}

// assertWorkflowTaskRecord checks tasks/<agentID>/ in the checkpoint: a
// completed task.json with the agent's file and token usage, beside the
// agent's materialized transcript.
func assertWorkflowTaskRecord(t *testing.T, env *TestEnv, checkpointID string, a workflowAgentFixture) {
	t.Helper()
	raw, ok := env.ReadFileFromBranch(paths.MetadataBranchName, CheckpointTaskFilePath(checkpointID, a.id, "task.json"))
	require.True(t, ok, "task.json for workflow agent %s not materialized", a.id)
	var task struct {
		AgentID      string   `json:"agent_id"`
		SubagentType string   `json:"subagent_type"`
		CompletedAt  string   `json:"completed_at"`
		Files        []string `json:"files"`
		TokenUsage   *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"token_usage"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &task))
	require.Equal(t, a.id, task.AgentID, raw)
	require.Equal(t, "workflow-subagent", task.SubagentType, raw)
	require.NotEmpty(t, task.CompletedAt, raw)
	require.Equal(t, []string{a.file}, task.Files, raw)
	require.NotNil(t, task.TokenUsage, "task token usage missing: %s", raw)
	require.Equal(t, a.usage["input_tokens"], task.TokenUsage.InputTokens, raw)
	require.Equal(t, a.usage["output_tokens"], task.TokenUsage.OutputTokens, raw)

	transcript, ok := env.ReadFileFromBranch(paths.MetadataBranchName,
		CheckpointTaskFilePath(checkpointID, a.id, paths.AgentTranscriptFileName(a.id)))
	require.True(t, ok, "transcript for workflow agent %s not materialized", a.id)
	require.Contains(t, transcript, a.file)
}
