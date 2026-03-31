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
}

func TestParseHookEvent_PreToolUse_ReturnsNil(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	// PreToolUse is a pass-through — should return nil event
	event, err := ag.ParseHookEvent(context.Background(), HookNamePreToolUse, strings.NewReader("{}"))
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
