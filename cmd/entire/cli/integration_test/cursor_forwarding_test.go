//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestDispatcher_ForwardedStopFromNonOwnerIsSkipped verifies the dispatcher
// skip end-to-end through the real `entire hooks claude-code stop` binary
// invocation. Cursor IDE forwards Stop to both .cursor/hooks.json and
// .claude/settings.json — when the SessionState records Cursor as the owner,
// the claude-code-side hook must no-op so we don't double-write checkpoints
// and metadata.
func TestDispatcher_ForwardedStopFromNonOwnerIsSkipped(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)

	sessionID := "test-cursor-forward-stop"
	statePath := filepath.Join(env.RepoDir, ".git", "entire-sessions", sessionID+".json")
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o755))

	// Pre-record state with AgentType=Cursor: the firing claude-code hook
	// should be recognized as a forwarded duplicate.
	initialState := map[string]any{
		"session_id":  sessionID,
		"agent_type":  "Cursor",
		"base_commit": env.GetHeadHash(),
		"started_at":  time.Now().Format(time.RFC3339Nano),
		"step_count":  0,
	}
	initialBytes, err := json.Marshal(initialState)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(statePath, initialBytes, 0o600))

	// Modify a file so the stop handler would normally have a checkpoint to write.
	env.WriteFile("changed.txt", "modification")

	session := env.NewSession()
	session.ID = sessionID
	session.CreateTranscript("change a file", []FileChange{
		{Path: "changed.txt", Content: "modification"},
	})

	require.NoError(t, env.SimulateStop(sessionID, session.TranscriptPath))

	// If the dispatcher had let the event through, SaveStep would have run and
	// step_count would have advanced. The skip leaves the state file untouched.
	afterBytes, err := os.ReadFile(statePath)
	require.NoError(t, err)

	var after map[string]any
	require.NoError(t, json.Unmarshal(afterBytes, &after))

	require.Equal(t, "Cursor", after["agent_type"], "AgentType must remain Cursor")
	require.EqualValues(t, 0, after["step_count"], "StepCount must not advance when the stop hook was skipped")
}

// TestDispatcher_ForwardedSessionEndFromNonOwnerIsSkipped is the SessionEnd
// variant. Cursor IDE also forwards sessionEnd to .claude/settings.json. When
// the owning agent is Cursor, the claude-code SessionEnd must not transition
// the session to ENDED, eager-condense, or alter step counts.
func TestDispatcher_ForwardedSessionEndFromNonOwnerIsSkipped(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)

	sessionID := "test-cursor-forward-sessionend"
	statePath := filepath.Join(env.RepoDir, ".git", "entire-sessions", sessionID+".json")
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o755))

	initialState := map[string]any{
		"session_id":  sessionID,
		"agent_type":  "Cursor",
		"base_commit": env.GetHeadHash(),
		"started_at":  time.Now().Format(time.RFC3339Nano),
		"step_count":  0,
	}
	initialBytes, err := json.Marshal(initialState)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(statePath, initialBytes, 0o600))

	require.NoError(t, env.SimulateSessionEnd(sessionID))

	afterBytes, err := os.ReadFile(statePath)
	require.NoError(t, err)

	var after map[string]any
	require.NoError(t, json.Unmarshal(afterBytes, &after))

	// markSessionEnded would have set ended_at and transitioned phase to "ended".
	require.Nil(t, after["ended_at"], "ended_at must remain nil when SessionEnd was skipped")
	if phase, ok := after["phase"]; ok {
		require.NotEqual(t, "ended", phase, "phase must not transition to ENDED when SessionEnd was skipped")
	}
}
