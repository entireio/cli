package codex

import (
	"context"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

func TestParseHookEvent_SessionStart(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	input := `{
		"session_id": "550e8400-e29b-41d4-a716-446655440000",
		"transcript_path": "/Users/test/.codex/rollouts/01/01/rollout-20260324-550e8400.jsonl",
		"cwd": "/tmp/testrepo",
		"hook_event_name": "SessionStart",
		"model": "gpt-4.1",
		"permission_mode": "default",
		"source": "startup"
	}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader(input))
	require.NoError(t, err)
	require.NotNil(t, event)
	require.Equal(t, agent.SessionStart, event.Type)
	require.Equal(t, "550e8400-e29b-41d4-a716-446655440000", event.SessionID)
	require.Equal(t, "/Users/test/.codex/rollouts/01/01/rollout-20260324-550e8400.jsonl", event.SessionRef)
	require.Equal(t, "gpt-4.1", event.Model)
}

func TestParseHookEvent_SessionStartNullTranscript(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	input := `{
		"session_id": "test-uuid",
		"transcript_path": null,
		"cwd": "/tmp/testrepo",
		"hook_event_name": "SessionStart",
		"model": "gpt-4.1",
		"permission_mode": "default",
		"source": "startup"
	}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader(input))
	require.NoError(t, err)
	require.NotNil(t, event)
	require.Equal(t, agent.SessionStart, event.Type)
	require.Equal(t, "test-uuid", event.SessionID)
	require.Empty(t, event.SessionRef)
}

func TestParseHookEvent_UserPromptSubmit(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	input := `{
		"session_id": "test-uuid",
		"turn_id": "turn-123",
		"transcript_path": "/tmp/rollout.jsonl",
		"cwd": "/tmp/testrepo",
		"hook_event_name": "UserPromptSubmit",
		"model": "gpt-4.1",
		"permission_mode": "default",
		"prompt": "Create a hello.txt file"
	}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameUserPromptSubmit, strings.NewReader(input))
	require.NoError(t, err)
	require.NotNil(t, event)
	require.Equal(t, agent.TurnStart, event.Type)
	require.Equal(t, "test-uuid", event.SessionID)
	require.Equal(t, "/tmp/rollout.jsonl", event.SessionRef)
	require.Equal(t, "Create a hello.txt file", event.Prompt)
	require.Equal(t, "gpt-4.1", event.Model)
}

func TestParseHookEvent_Stop(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	input := `{
		"session_id": "test-uuid",
		"turn_id": "turn-123",
		"transcript_path": "/tmp/rollout.jsonl",
		"cwd": "/tmp/testrepo",
		"hook_event_name": "Stop",
		"model": "gpt-4.1",
		"permission_mode": "default",
		"stop_hook_active": true,
		"last_assistant_message": "Done creating file."
	}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameStop, strings.NewReader(input))
	require.NoError(t, err)
	require.NotNil(t, event)
	require.Equal(t, agent.TurnEnd, event.Type)
	require.Equal(t, "test-uuid", event.SessionID)
	require.Equal(t, "/tmp/rollout.jsonl", event.SessionRef)
	require.Equal(t, "gpt-4.1", event.Model)
}

func TestParseHookEvent_PreToolUse_ReturnsNil(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	// PreToolUse is a pass-through — should return nil event
	event, err := ag.ParseHookEvent(context.Background(), HookNamePreToolUse, strings.NewReader("{}"))
	require.NoError(t, err)
	require.Nil(t, event)
}

func TestParseHookEvent_PostToolUse_ApplyPatch(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	// Match the wire shape from codex-rs/hooks/src/schema.rs PostToolUseCommandInput.
	// tool_input.command carries the patch envelope as a single string.
	input := `{
		"session_id": "550e8400-e29b-41d4-a716-446655440000",
		"turn_id": "turn-1",
		"transcript_path": "/tmp/rollout.jsonl",
		"cwd": "/tmp/testrepo",
		"hook_event_name": "PostToolUse",
		"model": "gpt-5",
		"permission_mode": "default",
		"tool_name": "apply_patch",
		"tool_use_id": "call-abc",
		"tool_input": {"command": "*** Begin Patch\n*** Add File: a.txt\n+hi\n*** Update File: b.txt\n@@\n-old\n+new\n*** Delete File: c.txt\n*** End Patch\n"},
		"tool_response": "Success."
	}`

	event, err := ag.ParseHookEvent(context.Background(), HookNamePostToolUse, strings.NewReader(input))
	require.NoError(t, err)
	require.NotNil(t, event)
	require.Equal(t, agent.ToolUse, event.Type)
	require.Equal(t, "550e8400-e29b-41d4-a716-446655440000", event.SessionID)
	require.Equal(t, "/tmp/rollout.jsonl", event.SessionRef)
	require.Equal(t, "/tmp/testrepo", event.CWD)
	require.Equal(t, "call-abc", event.ToolUseID)
	require.Equal(t, []string{"a.txt"}, event.NewFiles)
	require.Equal(t, []string{"b.txt"}, event.ModifiedFiles)
	require.Equal(t, []string{"c.txt"}, event.DeletedFiles)
}

func TestParseHookEvent_PostToolUse_AcceptsClaudeAliases(t *testing.T) {
	t.Parallel()
	// Codex registers Write and Edit as matcher aliases for apply_patch
	// (codex-rs/core/src/tools/hook_names.rs). Hook stdin still carries one of
	// those aliases as tool_name when a Claude-style hook config matches by
	// alias, so the parser must accept all three.
	for _, name := range []string{"apply_patch", "Write", "Edit"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ag := &CodexAgent{}
			input := `{
				"session_id": "s",
				"cwd": "/tmp/r",
				"tool_name": "` + name + `",
				"tool_use_id": "id",
				"tool_input": {"command": "*** Begin Patch\n*** Add File: x.txt\n+x\n*** End Patch\n"}
			}`
			event, err := ag.ParseHookEvent(context.Background(), HookNamePostToolUse, strings.NewReader(input))
			require.NoError(t, err)
			require.NotNil(t, event)
			require.Equal(t, []string{"x.txt"}, event.NewFiles)
		})
	}
}

func TestParseHookEvent_PostToolUse_NonMutatingTool_ReturnsNil(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	// Shell calls fire PostToolUse too, but we can't extract files from them
	// without parsing the shell command. Skip them entirely so we don't churn
	// session state on every command.
	input := `{
		"session_id": "s",
		"cwd": "/tmp/r",
		"tool_name": "shell",
		"tool_use_id": "id",
		"tool_input": {"command": ["echo", "hi"]},
		"tool_response": "hi\n"
	}`
	event, err := ag.ParseHookEvent(context.Background(), HookNamePostToolUse, strings.NewReader(input))
	require.NoError(t, err)
	require.Nil(t, event)
}

func TestParseHookEvent_PostToolUse_EmptyPatch_ReturnsNil(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	// A patch envelope with no Add/Update/Delete lines (e.g. malformed input
	// that still parses as JSON) should be a no-op rather than an error.
	input := `{
		"session_id": "s",
		"cwd": "/tmp/r",
		"tool_name": "apply_patch",
		"tool_use_id": "id",
		"tool_input": {"command": "*** Begin Patch\n*** End Patch\n"}
	}`
	event, err := ag.ParseHookEvent(context.Background(), HookNamePostToolUse, strings.NewReader(input))
	require.NoError(t, err)
	require.Nil(t, event)
}

func TestParseHookEvent_PostToolUse_MissingToolInput_ReturnsNil(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	// Defensive: if Codex ever fires PostToolUse for apply_patch with a
	// non-string tool_input shape, we should drop the event rather than fail
	// the hook (which would block the agent's tool call).
	input := `{
		"session_id": "s",
		"cwd": "/tmp/r",
		"tool_name": "apply_patch",
		"tool_use_id": "id",
		"tool_input": null
	}`
	event, err := ag.ParseHookEvent(context.Background(), HookNamePostToolUse, strings.NewReader(input))
	require.NoError(t, err)
	require.Nil(t, event)
}

func TestParseHookEvent_UnknownHook_ReturnsNil(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	event, err := ag.ParseHookEvent(context.Background(), "unknown-hook", strings.NewReader("{}"))
	require.NoError(t, err)
	require.Nil(t, event)
}

func TestParseHookEvent_EmptyInput_ReturnsError(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	_, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader(""))
	require.Error(t, err)
}

func TestParseHookEvent_MalformedJSON_ReturnsError(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	_, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader("{invalid json"))
	require.Error(t, err)
}
