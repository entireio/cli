package hookcompat

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReadEnvelope_PluginCamelCasePayload(t *testing.T) {
	t.Parallel()

	env, err := ReadEnvelope(strings.NewReader(`{
		"sessionId": "plugin-session",
		"transcriptPath": "/tmp/session.jsonl",
		"hookEventName": "SessionStart",
		"prompt": "hello",
		"model": "gpt-5",
		"cwd": "/tmp/repo",
		"timestamp": "2026-02-09T10:30:00.000Z"
	}`))

	require.NoError(t, err)
	require.Equal(t, "plugin-session", env.SessionID)
	require.Equal(t, "/tmp/session.jsonl", env.TranscriptPath)
	require.Equal(t, "SessionStart", env.HookEventName)
	require.Equal(t, "hello", env.Prompt)
	require.Equal(t, "gpt-5", env.Model)
	require.Equal(t, "/tmp/repo", env.CWD)
	require.Equal(t, time.Date(2026, 2, 9, 10, 30, 0, 0, time.UTC), env.Timestamp)
}

func TestReadEnvelope_NativeSnakeCasePayloadWithEpochMillis(t *testing.T) {
	t.Parallel()

	env, err := ReadEnvelope(strings.NewReader(`{
		"session_id": "native-session",
		"transcript_path": "/tmp/session.jsonl",
		"hook_event_name": "UserPromptSubmit",
		"timestamp": 1770633000000
	}`))

	require.NoError(t, err)
	require.Equal(t, "native-session", env.SessionID)
	require.Equal(t, "/tmp/session.jsonl", env.TranscriptPath)
	require.Equal(t, "UserPromptSubmit", env.HookEventName)
	require.Equal(t, time.UnixMilli(1770633000000), env.Timestamp)
}

func TestHookEventMatches(t *testing.T) {
	t.Parallel()

	allowed := map[string][]string{
		"SessionStart": {"session-start"},
	}

	require.True(t, HookEventMatches("", "stop", allowed))
	require.True(t, HookEventMatches("SessionStart", "session-start", allowed))
	require.False(t, HookEventMatches("SessionStart", "stop", allowed))
	require.False(t, HookEventMatches("Unknown", "session-start", allowed))
}
