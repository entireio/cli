//go:build integration

package integration

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/stretchr/testify/require"
)

// forkedSkillAgentID is the agent a `context: fork` skill ran in (shape from a
// real Claude Code 2.1.291 run).
const forkedSkillAgentID = "a80ff32f89f7dadc4"

// TestClaudeForkedSkillAgent_GetsTaskRecordAndSubagentTokens drives a skill
// with `context: fork` through the real hook binary in Claude Code 2.1.291's
// hook order. The skill runs in an agent of its own, which Claude Code names
// only in the Skill call's result; its SubagentStart carries an ordinary agent
// type. The Skill's PostToolUse must record the launch, so the agent's
// SubagentStop completes it, and a repeated stop (the agent woken again by a
// background child) must not add a second record. The commit carries the
// record keyed by the Skill call, and the session's subagent_tokens include
// the agent's usage.
func TestClaudeForkedSkillAgent_GetsTaskRecordAndSubagentTokens(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	session := env.NewSession()

	writeForkedSkillParentTranscript(t, session.TranscriptPath)
	require.NoError(t, env.SimulateUserPromptSubmitWithTranscriptPath(session.ID, session.TranscriptPath))
	require.NoError(t, simulateSkillPostToolUse(env, session.ID, session.TranscriptPath, true))

	state, err := env.GetSessionState(session.ID)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.True(t, hasLiveTaskRecord(state, "toolu_skill"), "the forked skill's launch must record a live task")

	agentTranscript := writeForkedSkillAgentTranscript(t, session)
	env.WriteFile("c.txt", "from the forked skill\n")
	require.NoError(t, env.SimulateStop(session.ID, session.TranscriptPath))
	for range 2 {
		require.NoError(t, env.SimulateSubagentStop(SubagentStopInput{
			SessionID: session.ID, TranscriptPath: session.TranscriptPath,
			AgentID: forkedSkillAgentID, AgentType: "general-purpose", AgentTranscriptPath: agentTranscript,
		}))
	}

	state, err = env.GetSessionState(session.ID)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.Len(t, state.TaskRecords, 1, "a repeated stop must not add a record")
	require.Empty(t, state.LiveTaskRecords(), "the agent's SubagentStop must complete its record")

	env.GitCommitWithShadowHooksAsAgent("Write from forked skill", "c.txt")
	checkpointID := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, checkpointID)

	task := readForkedSkillTask(t, env, checkpointID)
	require.Equal(t, forkedSkillAgentID, task.AgentID)
	require.Equal(t, "/fanout-fork", task.TaskDescription)
	require.Equal(t, []string{"c.txt"}, task.Files)
	transcript, ok := env.ReadFileFromBranch(paths.MetadataBranchName,
		CheckpointTaskFilePath(checkpointID, "toolu_skill", paths.AgentTranscriptFileName(forkedSkillAgentID)))
	require.True(t, ok, "the forked skill agent's transcript was not materialized")
	require.Contains(t, transcript, "c.txt")

	raw, ok := env.ReadFileFromBranch(paths.MetadataBranchName, SessionMetadataPath(checkpointID))
	require.True(t, ok, "session metadata.json missing from checkpoint")
	var metadata struct {
		TokenUsage struct {
			InputTokens    int `json:"input_tokens"`
			SubagentTokens *struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"subagent_tokens"`
		} `json:"token_usage"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &metadata))
	require.Equal(t, 100, metadata.TokenUsage.InputTokens, "parent usage must not absorb the agent's: %s", raw)
	require.NotNil(t, metadata.TokenUsage.SubagentTokens, "session subagent_tokens must include the forked agent: %s", raw)
	require.Equal(t, 7, metadata.TokenUsage.SubagentTokens.InputTokens, raw)
	require.Equal(t, 9, metadata.TokenUsage.SubagentTokens.OutputTokens, raw)
}

// TestClaudeForkedSkillAgent_FinishedAtLaunch_TakesFilesFromItsTranscript: a
// Skill result with background:false reports a fork that already finished, so
// the PostToolUse completes its record. No PreToolUse ran for the Skill call to
// record a worktree baseline, so the record must not take the worktree's other
// uncommitted changes as the agent's.
func TestClaudeForkedSkillAgent_FinishedAtLaunch_TakesFilesFromItsTranscript(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	session := env.NewSession()

	writeForkedSkillParentTranscript(t, session.TranscriptPath)
	require.NoError(t, env.SimulateUserPromptSubmitWithTranscriptPath(session.ID, session.TranscriptPath))
	env.WriteFile("unrelated.txt", "not the agent's\n")
	writeForkedSkillAgentTranscript(t, session)
	env.WriteFile("c.txt", "from the forked skill\n")
	require.NoError(t, simulateSkillPostToolUse(env, session.ID, session.TranscriptPath, false))

	state, err := env.GetSessionState(session.ID)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.Len(t, state.TaskRecords, 1)
	require.Empty(t, state.LiveTaskRecords(), "a finished fork completes at its PostToolUse")

	require.NoError(t, env.SimulateStop(session.ID, session.TranscriptPath))
	env.GitCommitWithShadowHooksAsAgent("Write from forked skill", "c.txt")
	checkpointID := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, checkpointID)
	require.Equal(t, []string{"c.txt"}, readForkedSkillTask(t, env, checkpointID).Files)
}

func simulateSkillPostToolUse(env *TestEnv, sessionID, transcriptPath string, background bool) error {
	env.T.Helper()
	return NewHookRunner(env.RepoDir, env.ClaudeProjectDir, env.T).runHookWithInput("post-task", map[string]any{
		"session_id":      sessionID,
		"transcript_path": transcriptPath,
		"hook_event_name": "PostToolUse",
		"tool_name":       "Skill",
		"tool_use_id":     "toolu_skill",
		"tool_input":      map[string]any{"skill": "fanout-fork"},
		"tool_response": map[string]any{
			"success": true, "commandName": "fanout-fork", "status": "forked",
			"background": background, "agentId": forkedSkillAgentID,
		},
	})
}

// writeForkedSkillParentTranscript writes the parent's transcript: the Skill
// call and its forked result, which names the agent only in toolUseResult.
func writeForkedSkillParentTranscript(t *testing.T, path string) {
	t.Helper()
	writeJSONLines(t, path,
		map[string]any{"type": "user", "uuid": "u1", "message": map[string]any{"role": "user", "content": "/fanout-fork"}},
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{
			"id": "msg_parent_1", "role": "assistant",
			"content": []any{map[string]any{"type": "tool_use", "id": "toolu_skill", "name": "Skill", "input": map[string]any{"skill": "fanout-fork"}}},
			"usage":   map[string]int{"input_tokens": 100, "output_tokens": 10},
		}},
		map[string]any{
			"type": "user", "uuid": "u2",
			"message": map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_skill", "content": `Skill "fanout-fork" launched (forked execution, running in the background).`},
			}},
			"toolUseResult": map[string]any{"success": true, "commandName": "fanout-fork", "status": "forked", "background": true, "agentId": forkedSkillAgentID},
		},
	)
}

// writeForkedSkillAgentTranscript writes the forked agent's transcript where
// Claude Code keeps an Agent call's, and returns its path.
func writeForkedSkillAgentTranscript(t *testing.T, session *Session) string {
	t.Helper()
	path := filepath.Join(paths.SubagentsDir(filepath.Dir(session.TranscriptPath), session.ID),
		paths.AgentTranscriptFileName(forkedSkillAgentID))
	writeJSONLines(t, path,
		map[string]any{"type": "user", "isSidechain": true, "agentId": forkedSkillAgentID, "message": map[string]any{"role": "user", "content": "Run the fanout-fork skill"}},
		map[string]any{"type": "assistant", "isSidechain": true, "agentId": forkedSkillAgentID, "message": map[string]any{
			"id": "msg_fork_1", "role": "assistant",
			"content": []any{map[string]any{"type": "tool_use", "id": "toolu_fork_write", "name": "Write",
				"input": map[string]any{"file_path": "c.txt", "content": "from the forked skill\n"}}},
			"usage": map[string]int{"input_tokens": 7, "output_tokens": 9},
		}},
	)
	return path
}

type forkedSkillTask struct {
	AgentID         string   `json:"agent_id"`
	TaskDescription string   `json:"task_description"`
	Files           []string `json:"files"`
}

func readForkedSkillTask(t *testing.T, env *TestEnv, checkpointID string) forkedSkillTask {
	t.Helper()
	raw, ok := env.ReadFileFromBranch(paths.MetadataBranchName, CheckpointTaskFilePath(checkpointID, "toolu_skill", "task.json"))
	require.True(t, ok, "task.json for the forked skill agent not materialized")
	var task forkedSkillTask
	require.NoError(t, json.Unmarshal([]byte(raw), &task))
	return task
}
