//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCodexPostToolUse_PopulatesFilesTouched verifies the end-to-end wiring
// of Codex's PostToolUse hook: the apply_patch envelope is parsed in
// codex.parsePostToolUse, dispatched as agent.ToolUse, and merged into
// state.FilesTouched by handleLifecycleToolUse.
//
// This is the regression guard for the original Codex bug — without per-tool
// file accounting, mid-turn commits triggered carry-forward erroneously and
// reset the checkpoint transcript offset on the next condense.
func TestCodexPostToolUse_PopulatesFilesTouched(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)

	sessionID := "test-codex-post-tool-use"
	statePath := filepath.Join(env.RepoDir, ".git", "entire-sessions", sessionID+".json")
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o755))

	// Pre-create state with AgentType=Codex. We skip UserPromptSubmit because
	// the hook handler we're exercising doesn't depend on phase or pending
	// attribution — just on the state file existing for RecordFilesTouched.
	initialState := map[string]any{
		"session_id":  sessionID,
		"agent_type":  "Codex",
		"base_commit": env.GetHeadHash(),
		"started_at":  time.Now().Format(time.RFC3339Nano),
		"step_count":  0,
	}
	initialBytes, err := json.Marshal(initialState)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(statePath, initialBytes, 0o600))

	patch := "*** Begin Patch\n" +
		"*** Add File: docs/added.md\n" +
		"+# added\n" +
		"*** Update File: src/changed.go\n" +
		"@@\n" +
		"-old\n" +
		"+new\n" +
		"*** Delete File: tmp/gone.txt\n" +
		"*** End Patch\n"

	runner := NewCodexHookRunner(env.RepoDir, t)
	require.NoError(t, runner.SimulateCodexPostToolUseApplyPatch(sessionID, env.RepoDir, patch))

	afterBytes, err := os.ReadFile(statePath)
	require.NoError(t, err)

	var after map[string]any
	require.NoError(t, json.Unmarshal(afterBytes, &after))

	rawFiles, ok := after["files_touched"].([]any)
	require.True(t, ok, "files_touched should be present and an array; got %T", after["files_touched"])

	got := make([]string, 0, len(rawFiles))
	for _, v := range rawFiles {
		s, _ := v.(string)
		got = append(got, s)
	}
	assert.ElementsMatch(t,
		[]string{"docs/added.md", "src/changed.go", "tmp/gone.txt"},
		got,
		"PostToolUse should merge Add/Update/Delete paths into FilesTouched")
}

// TestCodexPostToolUse_NonMutatingToolIsNoop verifies that PostToolUse hooks
// for non-apply_patch tools (e.g., shell) leave session state unchanged.
// Without this guard the hook would churn state on every shell command and
// risk overwriting genuine FilesTouched entries with empty data.
func TestCodexPostToolUse_NonMutatingToolIsNoop(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)

	sessionID := "test-codex-post-tool-use-noop"
	statePath := filepath.Join(env.RepoDir, ".git", "entire-sessions", sessionID+".json")
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o755))

	initialState := map[string]any{
		"session_id":    sessionID,
		"agent_type":    "Codex",
		"base_commit":   env.GetHeadHash(),
		"started_at":    time.Now().Format(time.RFC3339Nano),
		"step_count":    1,
		"files_touched": []string{"existing.go"},
	}
	initialBytes, err := json.Marshal(initialState)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(statePath, initialBytes, 0o600))

	// Shape mirrors codex-rs PostToolUseCommandInput for a shell call. The
	// parser must skip it without mutating state.
	input := map[string]any{
		"session_id":      sessionID,
		"turn_id":         "t1",
		"transcript_path": nil,
		"cwd":             env.RepoDir,
		"hook_event_name": "PostToolUse",
		"model":           "gpt-5",
		"permission_mode": "default",
		"tool_name":       "shell",
		"tool_use_id":     "call-shell",
		"tool_input":      map[string]any{"command": []string{"echo", "hi"}},
		"tool_response":   "hi\n",
	}
	inputJSON, err := json.Marshal(input)
	require.NoError(t, err)

	runner := NewCodexHookRunner(env.RepoDir, t)
	require.NoError(t, runner.runCodexHook("post-tool-use", inputJSON))

	afterBytes, err := os.ReadFile(statePath)
	require.NoError(t, err)

	var after map[string]any
	require.NoError(t, json.Unmarshal(afterBytes, &after))

	rawFiles, ok := after["files_touched"].([]any)
	require.True(t, ok)
	require.Len(t, rawFiles, 1)
	assert.Equal(t, "existing.go", rawFiles[0])
}
