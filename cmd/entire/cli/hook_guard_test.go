package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// TestShouldSkipForwardedHook_TranscriptBelongsToOtherAgent verifies the
// cross-agent guard for hooks that arrive at the wrong agent. When Cursor IDE
// invokes Claude Code hooks (because .cursor/hooks.json is missing — see
// issue #1262), the hook payload's transcript_path is inside Cursor's session
// directory. The firing agent (claude-code) must skip dispatch so the session
// isn't claimed for the wrong agent.
func TestShouldSkipForwardedHook_TranscriptBelongsToOtherAgent(t *testing.T) {
	setupStopTestRepo(t)
	cursorDir := t.TempDir()
	t.Setenv("ENTIRE_TEST_CURSOR_PROJECT_DIR", cursorDir)

	cursorTranscript := filepath.Join(cursorDir, "abc-session", "abc-session.jsonl")

	claudeAgent, err := agent.Get(agent.AgentNameClaudeCode)
	require.NoError(t, err)

	event := &agent.Event{
		Type:       agent.SessionStart,
		SessionID:  "abc-session",
		SessionRef: cursorTranscript,
	}

	require.True(t,
		shouldSkipForwardedHook(context.Background(), claudeAgent, event),
		"claude-code must skip: transcript_path is in Cursor's session dir")
}

// TestShouldSkipForwardedHook_TranscriptBelongsToFiringAgent verifies the
// guard does not fire when the transcript path is in the firing agent's own
// session directory — that's the normal case (Cursor → cursor hook).
func TestShouldSkipForwardedHook_TranscriptBelongsToFiringAgent(t *testing.T) {
	setupStopTestRepo(t)
	cursorDir := t.TempDir()
	t.Setenv("ENTIRE_TEST_CURSOR_PROJECT_DIR", cursorDir)

	cursorTranscript := filepath.Join(cursorDir, "abc", "abc.jsonl")

	cursorAgent, err := agent.Get(agent.AgentNameCursor)
	require.NoError(t, err)

	event := &agent.Event{
		Type:       agent.SessionStart,
		SessionID:  "abc",
		SessionRef: cursorTranscript,
	}

	require.False(t,
		shouldSkipForwardedHook(context.Background(), cursorAgent, event),
		"cursor must not skip its own session")
}

// TestExecuteAgentHook_SkipsWhenTranscriptPathBelongsToOtherAgent reproduces
// issue #1262: only .claude/settings.json is installed, so Cursor IDE invokes
// `entire hooks claude-code session-start` with a Cursor-shaped payload. The
// transcript_path inside Cursor's session dir proves the session is Cursor's,
// so executeAgentHook must short-circuit before SessionStart runs. Otherwise
// StoreAgentTypeHint would claim the session for claude-code.
func TestExecuteAgentHook_SkipsWhenTranscriptPathBelongsToOtherAgent(t *testing.T) {
	setupStopTestRepo(t)

	cwd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(cwd, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(cwd, ".entire", "settings.json"),
		[]byte(`{"enabled":true}`),
		0o644,
	))

	cursorDir := t.TempDir()
	t.Setenv("ENTIRE_TEST_CURSOR_PROJECT_DIR", cursorDir)
	cursorTranscript := filepath.Join(cursorDir, "abc-session", "abc-session.jsonl")

	payload, err := json.Marshal(map[string]string{
		"session_id":      "abc-session",
		"transcript_path": cursorTranscript,
	})
	require.NoError(t, err)

	cmd := &cobra.Command{}
	cmd.SetIn(bytes.NewReader(payload))
	cmd.SetContext(context.Background())

	require.NoError(t, executeAgentHook(cmd, agent.AgentNameClaudeCode, "session-start", false))

	// State file must not exist — the guard skipped before SessionStart ran.
	statePath := filepath.Join(cwd, ".git", "entire-sessions", "abc-session.json")
	_, statErr := os.Stat(statePath)
	require.True(t, os.IsNotExist(statErr), "session state must not be created when the hook is forwarded from another agent (got: %v)", statErr)

	// Agent hint file must not exist either — it's the precursor to AgentType=ClaudeCode.
	hintPath := filepath.Join(cwd, ".git", "entire-sessions", "abc-session.agent")
	_, hintErr := os.Stat(hintPath)
	require.True(t, os.IsNotExist(hintErr), "agent hint must not be written when the hook is forwarded (got: %v)", hintErr)
}
