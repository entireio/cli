package copilotcli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

// testSessionID is the UUID used in captured Copilot CLI hook payloads.
const testSessionID = "b0ff98c0-8e01-4b73-bf92-9649b139931b"

func TestParseHookEvent_UserPromptSubmitted(t *testing.T) {
	t.Parallel()

	ag := &CopilotCLIAgent{}
	input := `{"timestamp":1771480081360,"cwd":"/path/to/repo","sessionId":"` + testSessionID + `","prompt":"hi"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameUserPromptSubmitted, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.TurnStart {
		t.Errorf("expected event type %v, got %v", agent.TurnStart, event.Type)
	}
	if event.SessionID != testSessionID {
		t.Errorf("expected session_id %q, got %q", testSessionID, event.SessionID)
	}
	if event.Prompt != "hi" {
		t.Errorf("expected prompt 'hi', got %q", event.Prompt)
	}
	if event.Timestamp.IsZero() {
		t.Error("expected non-zero timestamp")
	}
}

func TestParseHookEvent_UserPromptSubmitted_TranscriptRef(t *testing.T) {
	ag := &CopilotCLIAgent{}
	t.Setenv("ENTIRE_TEST_COPILOT_SESSION_DIR", "/test/sessions")

	input := `{"timestamp":1771480081360,"cwd":"/path/to/repo","sessionId":"test-sess-id","prompt":"hello"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameUserPromptSubmitted, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	expected := "/test/sessions/test-sess-id/events.jsonl"
	if event.SessionRef != expected {
		t.Errorf("expected SessionRef %q, got %q", expected, event.SessionRef)
	}
}

func TestParseHookEvent_UserPromptSubmitted_VSCodePayload(t *testing.T) {
	t.Parallel()

	ag := &CopilotCLIAgent{}
	input := `{"timestamp":"2026-02-09T10:30:00.000Z","cwd":"/path/to/repo","sessionId":"` + testSessionID + `","hookEventName":"UserPromptSubmit","transcript_path":"/tmp/transcript.json","prompt":"hi"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameUserPromptSubmitted, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.TurnStart {
		t.Errorf("expected event type %v, got %v", agent.TurnStart, event.Type)
	}
	if event.SessionID != testSessionID {
		t.Errorf("expected session_id %q, got %q", testSessionID, event.SessionID)
	}
	if event.Prompt != "hi" {
		t.Errorf("expected prompt 'hi', got %q", event.Prompt)
	}
	if event.SessionRef != "/tmp/transcript.json" {
		t.Errorf("expected transcript path in SessionRef, got %q", event.SessionRef)
	}
}

func TestParseHookEvent_SessionStart(t *testing.T) {
	t.Parallel()

	ag := &CopilotCLIAgent{}
	input := `{"timestamp":1771480081383,"cwd":"/path/to/repo","sessionId":"` + testSessionID + `","source":"new","initialPrompt":"hi"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.SessionStart {
		t.Errorf("expected event type %v, got %v", agent.SessionStart, event.Type)
	}
	if event.SessionID != testSessionID {
		t.Errorf("expected session_id %q, got %q", testSessionID, event.SessionID)
	}
	if event.Timestamp.IsZero() {
		t.Error("expected non-zero timestamp")
	}
}

func TestParseHookEvent_SessionStart_VSCodePayload(t *testing.T) {
	t.Parallel()

	ag := &CopilotCLIAgent{}
	input := `{"timestamp":"2026-02-09T10:30:00.000Z","cwd":"/path/to/repo","sessionId":"` + testSessionID + `","hookEventName":"SessionStart"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.SessionStart {
		t.Errorf("expected event type %v, got %v", agent.SessionStart, event.Type)
	}
	if event.SessionID != testSessionID {
		t.Errorf("expected session_id %q, got %q", testSessionID, event.SessionID)
	}
}

func TestParseHookEvent_AgentStop(t *testing.T) {
	t.Parallel()

	ag := &CopilotCLIAgent{}
	transcriptPath := "/home/user/.copilot/session-state/" + testSessionID + "/events.jsonl"
	input := `{"timestamp":1771480085412,"cwd":"/path/to/repo","sessionId":"` + testSessionID + `","transcriptPath":"` + transcriptPath + `","stopReason":"end_turn"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameAgentStop, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.TurnEnd {
		t.Errorf("expected event type %v, got %v", agent.TurnEnd, event.Type)
	}
	if event.SessionID != testSessionID {
		t.Errorf("expected session_id %q, got %q", testSessionID, event.SessionID)
	}
	if event.SessionRef != transcriptPath {
		t.Errorf("expected transcript path in SessionRef, got %q", event.SessionRef)
	}
}

func TestParseHookEvent_AgentStop_VSCodePayload(t *testing.T) {
	t.Parallel()

	ag := &CopilotCLIAgent{}
	transcriptPath := "/tmp/transcript.json"
	input := `{"timestamp":"2026-02-09T10:30:00.000Z","cwd":"/path/to/repo","sessionId":"` + testSessionID + `","hookEventName":"Stop","transcript_path":"` + transcriptPath + `","stopReason":"end_turn"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameAgentStop, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.TurnEnd {
		t.Errorf("expected event type %v, got %v", agent.TurnEnd, event.Type)
	}
	if event.SessionID != testSessionID {
		t.Errorf("expected session_id %q, got %q", testSessionID, event.SessionID)
	}
	if event.SessionRef != transcriptPath {
		t.Errorf("expected transcript path in SessionRef, got %q", event.SessionRef)
	}
}

func TestParseHookEvent_AgentStop_ExtractsModel(t *testing.T) {
	t.Parallel()

	// Create a temp transcript with tool.execution_complete containing model field
	// (Copilot CLI v0.0.421+ includes model per tool call, not via session.model_change)
	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "events.jsonl")
	transcriptContent := strings.Join([]string{
		`{"type":"session.start","data":{"sessionId":"model-sess"},"id":"1","timestamp":"2026-03-03T00:00:00Z","parentId":null}`,
		`{"type":"user.message","data":{"content":"hello"},"id":"2","timestamp":"2026-03-03T00:00:01Z","parentId":""}`,
		`{"type":"tool.execution_complete","data":{"toolCallId":"tc1","model":"claude-sonnet-4.6","toolTelemetry":{"properties":{},"metrics":{}}},"id":"3","timestamp":"2026-03-03T00:00:02Z","parentId":""}`,
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(transcriptContent), 0o644); err != nil {
		t.Fatalf("failed to write test transcript: %v", err)
	}

	ag := &CopilotCLIAgent{}
	input := `{"timestamp":1771480085412,"cwd":"/path/to/repo","sessionId":"model-sess","transcriptPath":"` + transcriptPath + `","stopReason":"end_turn"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameAgentStop, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Model != "claude-sonnet-4.6" {
		t.Errorf("expected model 'claude-sonnet-4.6', got %q", event.Model)
	}
}

func TestParseHookEvent_AgentStop_NoTranscript_EmptyModel(t *testing.T) {
	t.Parallel()

	ag := &CopilotCLIAgent{}
	// transcriptPath points to a nonexistent file — model should be empty, not error
	input := `{"timestamp":1771480085412,"cwd":"/path/to/repo","sessionId":"no-model-sess","transcriptPath":"/nonexistent/events.jsonl","stopReason":"end_turn"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameAgentStop, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Model != "" {
		t.Errorf("expected empty model for nonexistent transcript, got %q", event.Model)
	}
}

func TestParseHookEvent_SessionEnd(t *testing.T) {
	t.Parallel()

	ag := &CopilotCLIAgent{}
	input := `{"timestamp":1771480085425,"cwd":"/path/to/repo","sessionId":"` + testSessionID + `","reason":"complete"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameSessionEnd, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.SessionEnd {
		t.Errorf("expected event type %v, got %v", agent.SessionEnd, event.Type)
	}
	if event.SessionID != testSessionID {
		t.Errorf("expected session_id %q, got %q", testSessionID, event.SessionID)
	}
}

func TestParseHookEvent_SessionEnd_VSCodePayload(t *testing.T) {
	t.Parallel()

	ag := &CopilotCLIAgent{}
	// VS Code uses "Stop" for both agent-stop and session-end hooks.
	input := `{"timestamp":"2026-02-09T10:30:00.000Z","cwd":"/path/to/repo","sessionId":"` + testSessionID + `","hookEventName":"Stop","reason":"complete"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameSessionEnd, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.SessionEnd {
		t.Errorf("expected event type %v, got %v", agent.SessionEnd, event.Type)
	}
	if event.SessionID != testSessionID {
		t.Errorf("expected session_id %q, got %q", testSessionID, event.SessionID)
	}
}

func TestParseHookEvent_SessionEnd_VSCodeNonTerminalStop_ReturnsNil(t *testing.T) {
	t.Parallel()

	ag := &CopilotCLIAgent{}
	input := `{"timestamp":"2026-02-09T10:30:00.000Z","cwd":"/path/to/repo","sessionId":"` + testSessionID + `","hookEventName":"Stop","stopReason":"end_turn"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameSessionEnd, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event != nil {
		t.Errorf("expected nil event for non-terminal VS Code Stop payload, got %+v", event)
	}
}

func TestParseHookEvent_VSCodeMismatchedHookEventName_ReturnsNil(t *testing.T) {
	t.Parallel()

	ag := &CopilotCLIAgent{}
	// VS Code sends "SessionStart" but the CLI subcommand is "agent-stop" — mismatch.
	input := `{"timestamp":"2026-02-09T10:30:00.000Z","cwd":"/path/to/repo","sessionId":"` + testSessionID + `","hookEventName":"SessionStart","transcript_path":"/tmp/t.json"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameAgentStop, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event != nil {
		t.Errorf("expected nil event for mismatched hookEventName, got %+v", event)
	}
}

func TestParseHookEvent_VSCodeUnknownHookEventName_ReturnsNil(t *testing.T) {
	t.Parallel()

	ag := &CopilotCLIAgent{}
	input := `{"timestamp":"2026-02-09T10:30:00.000Z","cwd":"/path/to/repo","sessionId":"` + testSessionID + `","hookEventName":"FutureEvent"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event != nil {
		t.Errorf("expected nil event for unknown hookEventName, got %+v", event)
	}
}

func TestParseHookEvent_PassthroughHooks_VSCodePayload_ReturnNil(t *testing.T) {
	t.Parallel()

	ag := &CopilotCLIAgent{}
	passthroughHooks := []string{
		HookNamePreToolUse,
		HookNamePostToolUse,
		HookNameErrorOccurred,
	}

	for _, hookName := range passthroughHooks {
		t.Run(hookName, func(t *testing.T) {
			t.Parallel()
			input := `{"timestamp":"2026-02-09T10:30:00.000Z","cwd":"/path/to/repo","sessionId":"` + testSessionID + `","hookEventName":"PreToolUse"}`
			event, err := ag.ParseHookEvent(context.Background(), hookName, strings.NewReader(input))
			if err != nil {
				t.Fatalf("unexpected error for %s: %v", hookName, err)
			}
			if event != nil {
				t.Errorf("expected nil event for pass-through hook %s, got %+v", hookName, event)
			}
		})
	}
}

func TestParseHookEvent_SubagentStop(t *testing.T) {
	t.Parallel()

	ag := &CopilotCLIAgent{}
	input := `{"timestamp":1771480085412,"cwd":"/path/to/repo","sessionId":"` + testSessionID + `"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameSubagentStop, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.SubagentEnd {
		t.Errorf("expected event type %v, got %v", agent.SubagentEnd, event.Type)
	}
	if event.SessionID != testSessionID {
		t.Errorf("expected session_id %q, got %q", testSessionID, event.SessionID)
	}
}

func TestParseHookEvent_PassthroughHooks_ReturnNil(t *testing.T) {
	t.Parallel()

	ag := &CopilotCLIAgent{}
	passthroughHooks := []string{
		HookNamePreToolUse,
		HookNamePostToolUse,
		HookNameErrorOccurred,
	}

	for _, hookName := range passthroughHooks {
		t.Run(hookName, func(t *testing.T) {
			t.Parallel()
			// Pass-through hooks should return nil event without reading stdin
			event, err := ag.ParseHookEvent(context.Background(), hookName, strings.NewReader(""))
			if err != nil {
				t.Fatalf("unexpected error for %s: %v", hookName, err)
			}
			if event != nil {
				t.Errorf("expected nil event for pass-through hook %s, got %+v", hookName, event)
			}
		})
	}
}

func TestParseHookEvent_UnknownHook_ReturnsNil(t *testing.T) {
	t.Parallel()

	ag := &CopilotCLIAgent{}
	event, err := ag.ParseHookEvent(context.Background(), "unknown-hook-name", strings.NewReader(`{"sessionId":"s1"}`))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event != nil {
		t.Errorf("expected nil event for unknown hook, got %+v", event)
	}
}

func TestParseHookEvent_EmptyInput_ReturnsError(t *testing.T) {
	t.Parallel()

	ag := &CopilotCLIAgent{}
	_, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader(""))

	if err == nil {
		t.Fatal("expected error for empty input, got nil")
	}
	if !strings.Contains(err.Error(), "empty hook input") {
		t.Errorf("expected 'empty hook input' error, got: %v", err)
	}
}

func TestParseHookEvent_MalformedJSON(t *testing.T) {
	t.Parallel()

	ag := &CopilotCLIAgent{}
	_, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader(`{"sessionId": INVALID}`))

	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
	if !strings.Contains(err.Error(), "failed to parse hook input") {
		t.Errorf("expected 'failed to parse hook input' error, got: %v", err)
	}
}

func TestParseHookEvent_AllHookTypes(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		hookName      string
		expectedType  agent.EventType
		expectNil     bool
		inputTemplate string
	}{
		{
			hookName:      HookNameUserPromptSubmitted,
			expectedType:  agent.TurnStart,
			inputTemplate: `{"timestamp":1,"cwd":"/repo","sessionId":"s1","prompt":"hi"}`,
		},
		{
			hookName:      HookNameSessionStart,
			expectedType:  agent.SessionStart,
			inputTemplate: `{"timestamp":1,"cwd":"/repo","sessionId":"s2","source":"new","initialPrompt":"hi"}`,
		},
		{
			hookName:      HookNameAgentStop,
			expectedType:  agent.TurnEnd,
			inputTemplate: `{"timestamp":1,"cwd":"/repo","sessionId":"s3","transcriptPath":"/t","stopReason":"end_turn"}`,
		},
		{
			hookName:      HookNameSessionEnd,
			expectedType:  agent.SessionEnd,
			inputTemplate: `{"timestamp":1,"cwd":"/repo","sessionId":"s4","reason":"complete"}`,
		},
		{
			hookName:      HookNameSubagentStop,
			expectedType:  agent.SubagentEnd,
			inputTemplate: `{"timestamp":1,"cwd":"/repo","sessionId":"s5"}`,
		},
		{
			hookName:      HookNamePreToolUse,
			expectNil:     true,
			inputTemplate: `{}`,
		},
		{
			hookName:      HookNamePostToolUse,
			expectNil:     true,
			inputTemplate: `{}`,
		},
		{
			hookName:      HookNameErrorOccurred,
			expectNil:     true,
			inputTemplate: `{}`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.hookName, func(t *testing.T) {
			t.Parallel()

			ag := &CopilotCLIAgent{}
			event, err := ag.ParseHookEvent(context.Background(), tc.hookName, strings.NewReader(tc.inputTemplate))

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tc.expectNil {
				if event != nil {
					t.Errorf("expected nil event, got %+v", event)
				}
				return
			}

			require.NotNil(t, event, "expected event, got nil")
			if event.Type != tc.expectedType {
				t.Errorf("expected event type %v, got %v", tc.expectedType, event.Type)
			}
		})
	}
}

func TestHookNames_ReturnsAllHooks(t *testing.T) {
	t.Parallel()

	ag := &CopilotCLIAgent{}
	names := ag.HookNames()

	if len(names) != 8 {
		t.Errorf("HookNames() returned %d hooks, want 8", len(names))
	}

	expected := map[string]bool{
		HookNameUserPromptSubmitted: false,
		HookNameSessionStart:        false,
		HookNameAgentStop:           false,
		HookNameSessionEnd:          false,
		HookNameSubagentStop:        false,
		HookNamePreToolUse:          false,
		HookNamePostToolUse:         false,
		HookNameErrorOccurred:       false,
	}

	for _, name := range names {
		if _, ok := expected[name]; !ok {
			t.Errorf("unexpected hook name: %q", name)
		}
		expected[name] = true
	}

	for name, found := range expected {
		if !found {
			t.Errorf("missing hook name: %q", name)
		}
	}
}
