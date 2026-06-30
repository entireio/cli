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

	// The explicit parent->child link (dipree's PR #1570 ask): the parent session
	// state must carry a UI-consumable subagent list so the checkpoint metadata can
	// render the parent->children tree. For Codex the agent_id doubles as the
	// ToolUseID (there is no per-task tool_use_id).
	require.Len(t, state.Subagents, 1, "exactly one subagent link recorded on the parent")
	link := state.Subagents[0]
	assert.Equal(t, agentID, link.AgentID, "the child subagent id is the explicit parent->child link")
	assert.Equal(t, agentID, link.ToolUseID, "Codex keys the link by agent_id")
	assert.Equal(t, "worker", link.SubagentType, "subagent type captured")
	assert.GreaterOrEqual(t, link.FileCount, 1, "file count reflects the subagent's change")
}

// TestCodexSubagent_RecordsLinkForEveryChild verifies the complete parent->child
// roster: a subagent that changed NO files is still recorded on the parent
// (it just produces no task checkpoint), alongside a file-changing one. This
// guards the "render the complete tree" requirement — the link is recorded
// before the no-file-change early-return in handleLifecycleSubagentEnd.
func TestCodexSubagent_RecordsLinkForEveryChild(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)

	parentSessionID := "codex-parent-roster"
	editorID := "019e1111-1111-7222-8333-444455556666"
	readerID := "019e2222-1111-7222-8333-444455556666"

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

	rolloutDir := filepath.Join(env.RepoDir, ".codex-test")
	require.NoError(t, os.MkdirAll(rolloutDir, 0o750))
	applyPatch := func(file string) string {
		return "{\"type\":\"response_item\",\"payload\":{\"type\":\"custom_tool_call\",\"name\":\"apply_patch\"," +
			"\"input\":\"*** Begin Patch\\n*** Add File: " + file + "\\n+x\\n*** End Patch\"}}"
	}
	subMeta := func(id string) string {
		return "{\"type\":\"session_meta\",\"payload\":{\"id\":\"" + id + "\",\"timestamp\":\"2026-06-29T12:00:05.000Z\",\"thread_source\":\"subagent\"}}"
	}
	parentRollout := filepath.Join(rolloutDir, "parent.jsonl")
	require.NoError(t, os.WriteFile(parentRollout, []byte(
		"{\"type\":\"session_meta\",\"payload\":{\"id\":\""+parentSessionID+"\",\"timestamp\":\"2026-06-29T12:00:00.000Z\",\"thread_source\":\"user\"}}\n"), 0o600))

	// Editor child changes a file -> task checkpoint + roster entry.
	editorRollout := filepath.Join(rolloutDir, "editor.jsonl")
	require.NoError(t, os.WriteFile(editorRollout, []byte(subMeta(editorID)+"\n"+applyPatch("edited.go")+"\n"), 0o600))
	// Reader child changes nothing -> no task checkpoint, but still rostered.
	readerRollout := filepath.Join(rolloutDir, "reader.jsonl")
	require.NoError(t, os.WriteFile(readerRollout, []byte(subMeta(readerID)+"\n"), 0o600))

	runner := NewCodexHookRunner(env.RepoDir, t)
	require.NoError(t, runner.SimulateCodexSubagentStart(parentSessionID, parentRollout, editorID, "editor"))
	require.NoError(t, runner.SimulateCodexSubagentStop(parentSessionID, parentRollout, editorID, "editor", editorRollout))
	require.NoError(t, runner.SimulateCodexSubagentStart(parentSessionID, parentRollout, readerID, "reader"))
	require.NoError(t, runner.SimulateCodexSubagentStop(parentSessionID, parentRollout, readerID, "reader", readerRollout))

	state, err := env.GetSessionState(parentSessionID)
	require.NoError(t, err)
	require.NotNil(t, state)

	require.Len(t, state.Subagents, 2, "both subagents recorded, including the no-file-change one")
	var sawEditor, sawReader bool
	for i := range state.Subagents {
		l := state.Subagents[i]
		switch l.AgentID {
		case editorID:
			sawEditor = true
			assert.Equal(t, "editor", l.SubagentType)
			assert.GreaterOrEqual(t, l.FileCount, 1, "editor recorded its file change")
		case readerID:
			sawReader = true
			assert.Equal(t, "reader", l.SubagentType)
			assert.Equal(t, 0, l.FileCount, "reader changed nothing but is still rostered")
		}
	}
	assert.True(t, sawEditor, "file-changing subagent recorded")
	assert.True(t, sawReader, "no-file-change subagent is STILL recorded (complete tree)")
}
