//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/stretchr/testify/require"
)

// TestCodexSubagent_StoresDeclaredSubagentTranscript drives Codex's subagent hooks
// end to end — subagent-start → the subagent edits a file → subagent-stop — and
// asserts the task checkpoint stores the subagent's own rollout.
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

	require.NoError(t, env.WriteSessionState(sessionID, &session.State{
		SessionID:  sessionID,
		AgentType:  agent.AgentTypeCodex,
		BaseCommit: env.GetHeadHash(),
	}))

	rolloutDir := filepath.Join(env.RepoDir, ".entire", "tmp", "codex-rollouts")
	require.NoError(t, os.MkdirAll(rolloutDir, 0o750))
	parentRollout := filepath.Join(rolloutDir, "rollout-"+sessionID+".jsonl")
	require.NoError(t, os.WriteFile(parentRollout, []byte(`{"type":"session_meta","payload":{"id":"`+sessionID+`"}}`+"\n"), 0o600))
	subagentRollout := filepath.Join(rolloutDir, "rollout-"+agentID+".jsonl")
	require.NoError(t, os.WriteFile(subagentRollout,
		[]byte(`{"type":"response_item","payload":{"content":"wrote `+editedFile+`"}}`+"\n"), 0o600))

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
	// names the task directory.
	wantPath := paths.EntireMetadataDir + "/" + sessionID +
		"/tasks/" + agentID + "/" + paths.AgentTranscriptFileName(agentID)
	content, ok := env.ReadFileFromBranch(env.GetShadowBranchName(), wantPath)
	require.True(t, ok,
		"subagent transcript not stored at %s — the declared agent_transcript_path was not honoured", wantPath)
	require.Contains(t, content, editedFile, "stored transcript is not the subagent's rollout")
}
