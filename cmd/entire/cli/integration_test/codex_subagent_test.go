//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
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
	env := NewFeatureBranchEnv(t)

	const (
		sessionID  = "test-codex-subagent"
		agentID    = "child-thread-9"
		editedFile = "docs/red.md"
	)
	complete := true

	require.NoError(t, env.WriteSessionState(sessionID, &session.State{
		SessionID:                 sessionID,
		AgentType:                 agent.AgentTypeCodex,
		BaseCommit:                env.GetHeadHash(),
		SubagentInventoryComplete: &complete,
	}))

	rolloutDir := filepath.Join(env.RepoDir, ".entire", "tmp", "codex-rollouts")
	require.NoError(t, os.MkdirAll(rolloutDir, 0o750))
	parentRollout := filepath.Join(rolloutDir, "rollout-"+sessionID+".jsonl")
	require.NoError(t, os.WriteFile(parentRollout, []byte(`{"type":"session_meta","payload":{"id":"`+sessionID+`","thread_source":"user"}}`+"\n"), 0o600))
	subagentRollout := filepath.Join(rolloutDir, "rollout-"+agentID+".jsonl")
	require.NoError(t, os.WriteFile(subagentRollout, []byte(
		`{"type":"session_meta","payload":{"id":"`+agentID+`"}}`+"\n"+
			`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1"}}`+"\n"+
			`{"type":"response_item","payload":{"type":"custom_tool_call","status":"completed","name":"apply_patch","input":"*** Begin Patch\n*** Add File: `+editedFile+`\n+red\n*** End Patch"}}`+"\n"+
			`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":10,"cached_input_tokens":2,"output_tokens":3}}}}`+"\n"+
			`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-1"}}`+"\n"), 0o600))
	probe, err := (&codex.CodexAgent{RolloutRoots: []string{}}).ExtractWithSubagentInventory(nil, 0, []agent.SubagentReference{{AgentID: agentID, DeclaredTranscriptPath: subagentRollout}})
	require.NoError(t, err)
	require.Equal(t, []string{"turn-1"}, probe.Children[0].TerminalTurnIDs)

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

	// Codex sends no tool_use_id, so agent_id is the correlation key and therefore
	// keys the task record.
	state, err := env.GetSessionState(sessionID)
	require.NoError(t, err)
	rec := state.FindTaskRecord(agentID)
	require.NotNil(t, rec, "expected a task record keyed by agent_id")
	require.True(t, rec.CompletedAt.IsZero(), "provisional subagent-stop must not complete the record")

	// Root Stop observes terminal evidence in the same verified child rollout.
	hook("stop", map[string]any{"hook_event_name": "Stop", "last_assistant_message": "done"})
	state, err = env.GetSessionState(sessionID)
	require.NoError(t, err)
	require.Len(t, state.SubagentInventory, 1)
	require.Equal(t, subagentRollout, state.SubagentInventory[0].DeclaredTranscriptPath)
	require.Equal(t, []string{"turn-1"}, state.SubagentInventory[0].FinalizedTurnIDs)
	rec = state.FindTaskRecord(agentID)
	require.NotNil(t, rec)
	require.False(t, rec.CompletedAt.IsZero(), "terminal child rollout must reconcile the record")
	require.True(t, containsFile(rec.Files, editedFile), "the record must carry the child edit, got %v", rec.Files)

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
