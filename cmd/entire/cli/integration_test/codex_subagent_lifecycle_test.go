//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"encoding/json"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCodexSubagentEnd_SourcesFilesFromChildNotParent is the integration
// regression guard for handleLifecycleSubagentEnd's resolver: a Codex
// SubagentStop must attribute the subagent's modified files from the CHILD
// rollout (via the hook's agent_transcript_path / SubagentTranscriptResolver),
// never by scanning the PARENT rollout. A revert to scanning event.SessionRef at
// offset 0 would attribute the parent's apply_patch files to the subagent task
// checkpoint — this test fails in that case.
func TestCodexSubagentEnd_SourcesFilesFromChildNotParent(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)

	parentSessionID := "codex-parent-sess"
	agentID := "019e0000-1111-7222-8333-444455556666"

	// Pre-create parent session state as Codex (mirrors codex_post_tool_use_test).
	statePath := filepath.Join(env.RepoDir, ".git", "entire-sessions", parentSessionID+".json")
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o755))
	initialState := map[string]any{
		"session_id":  parentSessionID,
		"agent_type":  "Codex",
		"base_commit": env.GetHeadHash(),
		"started_at":  time.Now().Format(time.RFC3339Nano),
		"step_count":  0,
	}
	b, err := json.Marshal(initialState)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(statePath, b, 0o600))

	// Parent rollout edits parent-only.go; child rollout edits child-file.go.
	rolloutDir := filepath.Join(env.RepoDir, ".codex-test")
	require.NoError(t, os.MkdirAll(rolloutDir, 0o750))
	applyPatch := func(file string) string {
		return "{\"type\":\"response_item\",\"payload\":{\"type\":\"custom_tool_call\",\"name\":\"apply_patch\"," +
			"\"input\":\"*** Begin Patch\\n*** Add File: " + file + "\\n+x\\n*** End Patch\"}}"
	}
	parentRollout := filepath.Join(rolloutDir, "parent.jsonl")
	require.NoError(t, os.WriteFile(parentRollout, []byte(
		"{\"type\":\"session_meta\",\"payload\":{\"id\":\""+parentSessionID+"\",\"timestamp\":\"2026-06-29T12:00:00.000Z\",\"thread_source\":\"user\"}}\n"+
			applyPatch("parent-only.go")+"\n"), 0o600))
	childRollout := filepath.Join(rolloutDir, "child.jsonl")
	require.NoError(t, os.WriteFile(childRollout, []byte(
		"{\"type\":\"session_meta\",\"payload\":{\"id\":\""+agentID+"\",\"timestamp\":\"2026-06-29T12:00:05.000Z\",\"thread_source\":\"subagent\"}}\n"+
			applyPatch("child-file.go")+"\n"), 0o600))

	runner := NewCodexHookRunner(env.RepoDir, t)
	require.NoError(t, runner.SimulateCodexSubagentStart(parentSessionID, parentRollout, agentID, "worker"))
	require.NoError(t, runner.SimulateCodexSubagentStop(parentSessionID, parentRollout, agentID, "worker", childRollout))

	state, err := env.GetSessionState(parentSessionID)
	require.NoError(t, err)
	require.NotNil(t, state)

	assert.Contains(t, state.FilesTouched, "child-file.go",
		"subagent's modified file (from the child rollout) must be attributed")
	assert.NotContains(t, state.FilesTouched, "parent-only.go",
		"the parent rollout must NOT be scanned as the subagent transcript")
}
