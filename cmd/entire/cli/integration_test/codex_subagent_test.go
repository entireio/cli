//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/stretchr/testify/require"
)

// TestCodexSubagent_StoresDeclaredSubagentTranscript drives Codex's subagent hooks
// end to end — subagent-start → the subagent edits a file → subagent-stop → commit —
// and asserts the declared rollout reaches the condensed checkpoint's tasks/ subtree
// via the task record the materializer follows (#2058).
//
// The rollout path here is deliberately flat, matching Codex's real layout and
// matching neither candidate the Claude Code fallback probes, so the assertion can
// only pass if the declared path is honoured (see Event.SubagentTranscriptPath).
func TestCodexSubagent_StoresDeclaredSubagentTranscript(t *testing.T) {
	t.Parallel()
	sc := newCodexSubagentScenario(t)
	env, hook := sc.env, sc.hook
	const (
		sessionID  = codexScenarioSessionID
		agentID    = codexScenarioAgentID
		editedFile = codexScenarioEditedFile
	)
	subagentRollout := sc.subagentRollout

	// Codex sends no tool_use_id, so agent_id is the correlation key and therefore
	// keys the task record.
	state, err := env.GetSessionState(sessionID)
	require.NoError(t, err)
	rec := state.FindTaskRecord(agentID)
	require.NotNil(t, rec, "expected a task record keyed by agent_id")
	require.True(t, rec.CompletedAt.IsZero(), "provisional subagent-stop must not complete the record")

	// The fixture includes the real fork shape: inherited parent history with
	// an open parent turn and no ordinal boundary. Root Stop observes terminal evidence in the same verified child rollout.
	hook("stop", map[string]any{"hook_event_name": "Stop", "last_assistant_message": "done"})
	state, err = env.GetSessionState(sessionID)
	require.NoError(t, err)
	require.Len(t, state.SubagentInventory, 1)
	require.Equal(t, subagentRollout, state.SubagentInventory[0].DeclaredTranscriptPath)
	require.Equal(t, []string{"turn-1"}, state.SubagentInventory[0].FinalizedTurnIDs)
	rec = state.FindTaskRecord(agentID)
	require.NotNil(t, rec)
	require.False(t, rec.CompletedAt.IsZero(), "terminal child rollout must reconcile the record")
	require.Equal(t, []string{editedFile}, rec.Files, "only hook-observed child turns contribute files")
	require.Nil(t, rec.TokenUsage, "unscoped fork counters are not exact child usage")
	require.False(t, *state.TokenUsage.SubagentTokensComplete)

	// Committing condenses the session, and the materializer must store the rollout
	// itself — the storage guarantee this test is named for.
	env.GitCommitWithShadowHooksAsAgent("Add red doc", editedFile)
	checkpointID := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, checkpointID, "expected a condensed checkpoint after committing the subagent's work")
	stored, ok := env.ReadFileFromBranch(paths.MetadataBranchName,
		CheckpointTaskFilePath(checkpointID, agentID, paths.AgentTranscriptFileName(agentID)))
	require.True(t, ok, "declared rollout not materialized under the checkpoint's tasks/ subtree")
	require.Contains(t, stored, editedFile, "materialized transcript is not the subagent's rollout")
}

const (
	codexScenarioSessionID  = "test-codex-subagent"
	codexScenarioAgentID    = "child-thread-9"
	codexScenarioEditedFile = "docs/red.md"
)

type codexSubagentScenario struct {
	env             *TestEnv
	hook            func(string, map[string]any)
	subagentRollout string
}

// newCodexSubagentScenario drives a Codex session through subagent-start, the
// subagent's edit, and its provisional subagent-stop. The child's rollout
// already shows its turn complete.
func newCodexSubagentScenario(t *testing.T) codexSubagentScenario {
	t.Helper()
	env := NewFeatureBranchEnv(t)

	const (
		sessionID  = codexScenarioSessionID
		agentID    = codexScenarioAgentID
		editedFile = codexScenarioEditedFile
	)
	complete := true

	require.NoError(t, env.WriteSessionState(sessionID, &session.State{
		SessionID:                 sessionID,
		AgentType:                 agent.AgentTypeCodex,
		BaseCommit:                env.GetHeadHash(),
		SubagentInventoryComplete: &complete,
	}))

	// Git hooks must resolve the same Codex sessions dir the Codex hooks do
	// (CodexHookRunner sets it), or rollout reads are refused there.
	env.ExtraEnv = append(env.ExtraEnv, "ENTIRE_TEST_CODEX_SESSION_DIR="+filepath.Join(env.RepoDir, ".entire", "tmp"))
	rolloutDir := filepath.Join(env.RepoDir, ".entire", "tmp", "codex-rollouts")
	require.NoError(t, os.MkdirAll(rolloutDir, 0o750))
	parentRollout := filepath.Join(rolloutDir, "rollout-"+sessionID+".jsonl")
	require.NoError(t, os.WriteFile(parentRollout, []byte(`{"type":"session_meta","payload":{"id":"`+sessionID+`","thread_source":"user"}}`+"\n"), 0o600))
	subagentRollout := filepath.Join(rolloutDir, "rollout-"+agentID+".jsonl")
	require.NoError(t, os.WriteFile(subagentRollout, []byte(
		`{"type":"session_meta","payload":{"id":"`+agentID+`","forked_from_id":"`+sessionID+`"}}`+"\n"+
			`{"type":"event_msg","payload":{"type":"task_started","turn_id":"inherited-parent-turn"}}`+"\n"+
			`{"type":"response_item","payload":{"type":"custom_tool_call","name":"apply_patch","input":"*** Begin Patch\n*** Add File: parent-only.txt\n+x\n*** End Patch"}}`+"\n"+
			`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1"}}`+"\n"+
			`{"type":"response_item","payload":{"type":"custom_tool_call","status":"completed","name":"apply_patch","input":"*** Begin Patch\n*** Add File: `+editedFile+`\n+red\n*** End Patch"}}`+"\n"+
			`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":10,"cached_input_tokens":2,"output_tokens":3}}}}`+"\n"+
			`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-1"}}`+"\n"), 0o600))
	hook := codexHooker(t, env.RepoDir, sessionID, parentRollout)
	hook("subagent-start", map[string]any{
		"hook_event_name": "SubagentStart",
		"agent_id":        agentID,
		"agent_type":      "reviewer",
		"turn_id":         "turn-1",
	})

	env.WriteFile(editedFile, "Red is a warm colour.\n")

	hook("subagent-stop", map[string]any{
		"hook_event_name":       "SubagentStop",
		"agent_id":              agentID,
		"agent_type":            "reviewer",
		"agent_transcript_path": subagentRollout,
		"stop_hook_active":      false,
		"turn_id":               "turn-1",
	})

	return codexSubagentScenario{env: env, hook: hook, subagentRollout: subagentRollout}
}

// TestCodexSubagent_CommitBeforeParentTurnEnds_CompletesTaskRecord pins that a
// child whose rollout already shows its turn complete is stored as completed,
// with its files, when the parent commits mid-turn. (This fixture's child is a
// fork whose counters are not exact child usage, so its token_usage stays
// unset by design; see TestCodexSubagent_StoresDeclaredSubagentTranscript.) Codex's
// subagent-stop is provisional and only the parent's turn end reconciled the
// rollout, so a parent that waited for the child and committed before its own
// turn ended stored the record as still in flight.
func TestCodexSubagent_CommitBeforeParentTurnEnds_CompletesTaskRecord(t *testing.T) {
	t.Parallel()
	sc := newCodexSubagentScenario(t)

	sc.env.GitCommitWithShadowHooksAsAgent("Add red doc", codexScenarioEditedFile)
	checkpointID := sc.env.TryGetLatestCheckpointID()
	require.NotEmpty(t, checkpointID, "expected a condensed checkpoint after committing the subagent's work")

	raw, ok := sc.env.ReadFileFromBranch(paths.MetadataBranchName,
		CheckpointTaskFilePath(checkpointID, codexScenarioAgentID, "task.json"))
	require.True(t, ok, "task.json not materialized under the checkpoint's tasks/ subtree")
	var task struct {
		CompletedAt string   `json:"completed_at"`
		Files       []string `json:"files"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &task))
	require.NotEmpty(t, task.CompletedAt, "a child whose rollout shows its turn complete must be stored as completed: %s", raw)
	require.Equal(t, []string{codexScenarioEditedFile}, task.Files, "stored task record must list the child's files: %s", raw)
}
