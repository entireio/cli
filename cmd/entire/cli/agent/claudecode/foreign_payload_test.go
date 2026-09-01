package claudecode

import (
	"bytes"
	"context"
	"testing"
)

// grokSessionStartPayload is a verbatim Grok Build session_start payload,
// captured from a real Grok 1.0.5 run. Grok scans ~/.claude/settings.json for
// hooks by default and treats the user scope as always-trusted, so on a machine
// with both installed this is exactly what Entire's claude-code hooks receive.
const grokSessionStartPayload = `{
  "hookEventName": "session_start",
  "sessionId": "01a03a9f-0f3b-78d2-bad3-cf2ad4e0ff2e",
  "cwd": "/repo",
  "workspaceRoot": "/repo/",
  "timestamp": "2026-08-25T20:31:38.607470+00:00",
  "permissionMode": "bypassPermissions",
  "source": "new"
}`

// TestParseHookEvent_ForeignAgentPayloadIsIgnored is a cross-agent regression
// guard. Grok spells the field sessionId; Claude Code binds session_id. Without
// the guard this parsed into an event with an empty SessionID and no
// transcript, recording a Grok session under Claude Code's name.
func TestParseHookEvent_ForeignAgentPayloadIsIgnored(t *testing.T) {
	t.Parallel()

	c := &ClaudeCodeAgent{}
	for _, hook := range []string{
		HookNameSessionStart,
		HookNameUserPromptSubmit,
		HookNameStop,
		HookNameSessionEnd,
	} {
		t.Run(hook, func(t *testing.T) {
			t.Parallel()
			ev, err := c.ParseHookEvent(context.Background(), hook, bytes.NewReader([]byte(grokSessionStartPayload)))
			if err != nil {
				t.Fatalf("ParseHookEvent(%s): %v", hook, err)
			}
			if ev != nil {
				t.Errorf("got event %v for a foreign payload, want nil", ev.Type)
			}
		})
	}
}

// TestParseHookEvent_NativePayloadStillWorks pins that the guard only rejects
// payloads with no session_id, not Claude Code's own.
func TestParseHookEvent_NativePayloadStillWorks(t *testing.T) {
	t.Parallel()

	native := `{"session_id":"abc-123","transcript_path":"/tmp/t.jsonl","hook_event_name":"SessionStart"}`
	c := &ClaudeCodeAgent{}
	ev, err := c.ParseHookEvent(context.Background(), HookNameSessionStart, bytes.NewReader([]byte(native)))
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if ev == nil {
		t.Fatal("native Claude Code payload was dropped")
	}
	if ev.SessionID != "abc-123" {
		t.Errorf("SessionID = %q, want abc-123", ev.SessionID)
	}
}
