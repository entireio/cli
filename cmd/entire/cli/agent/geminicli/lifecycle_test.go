package geminicli

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

func TestParseHookEvent_SessionStart(t *testing.T) {
	t.Parallel()

	ag := &GeminiCLIAgent{}
	input := `{"session_id": "gemini-session-123", "transcript_path": "/tmp/gemini.json"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.SessionStart {
		t.Errorf("expected event type %v, got %v", agent.SessionStart, event.Type)
	}
	if event.SessionID != "gemini-session-123" {
		t.Errorf("expected session_id 'gemini-session-123', got %q", event.SessionID)
	}
	if event.SessionRef != "/tmp/gemini.json" {
		t.Errorf("expected session_ref '/tmp/gemini.json', got %q", event.SessionRef)
	}
	if event.Timestamp.IsZero() {
		t.Error("expected non-zero timestamp")
	}
}

func TestParseHookEvent_TurnStart(t *testing.T) {
	t.Parallel()

	ag := &GeminiCLIAgent{}
	input := `{
		"session_id": "sess-456",
		"transcript_path": "/tmp/t.json",
		"cwd": "/home/user",
		"hook_event_name": "before-agent",
		"timestamp": "2024-01-15T10:00:00Z",
		"prompt": "Hello Gemini"
	}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameBeforeAgent, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.TurnStart {
		t.Errorf("expected event type %v, got %v", agent.TurnStart, event.Type)
	}
	if event.SessionID != "sess-456" {
		t.Errorf("expected session_id 'sess-456', got %q", event.SessionID)
	}
	if event.Prompt != "Hello Gemini" {
		t.Errorf("expected prompt 'Hello Gemini', got %q", event.Prompt)
	}
}

func TestParseHookEvent_TurnEnd(t *testing.T) {
	t.Parallel()

	ag := &GeminiCLIAgent{}
	input := `{
		"session_id": "sess-789",
		"transcript_path": "/tmp/after.json",
		"cwd": "/home/user",
		"hook_event_name": "after-agent",
		"timestamp": "2024-01-15T10:05:00Z"
	}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameAfterAgent, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.TurnEnd {
		t.Errorf("expected event type %v, got %v", agent.TurnEnd, event.Type)
	}
	if event.SessionID != "sess-789" {
		t.Errorf("expected session_id 'sess-789', got %q", event.SessionID)
	}
}

func TestParseHookEvent_SessionEnd(t *testing.T) {
	t.Parallel()

	ag := &GeminiCLIAgent{}
	input := `{
		"session_id": "ending-session",
		"transcript_path": "/tmp/end.json",
		"reason": "exit"
	}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameSessionEnd, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.SessionEnd {
		t.Errorf("expected event type %v, got %v", agent.SessionEnd, event.Type)
	}
	if event.SessionID != "ending-session" {
		t.Errorf("expected session_id 'ending-session', got %q", event.SessionID)
	}
}

func TestParseHookEvent_Compaction(t *testing.T) {
	t.Parallel()

	ag := &GeminiCLIAgent{}
	input := `{
		"session_id": "compress-session",
		"transcript_path": "/tmp/compress.json",
		"hook_event_name": "pre-compress"
	}`

	event, err := ag.ParseHookEvent(context.Background(), HookNamePreCompress, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.Compaction {
		t.Errorf("expected event type %v, got %v", agent.Compaction, event.Type)
	}
	if event.SessionID != "compress-session" {
		t.Errorf("expected session_id 'compress-session', got %q", event.SessionID)
	}
}

func TestParseHookEvent_BeforeModel_ReturnsModelUpdate(t *testing.T) {
	t.Parallel()

	ag := &GeminiCLIAgent{}
	input := `{
		"session_id": "model-sess",
		"transcript_path": "/tmp/t.json",
		"llm_request": {"model": "gemini-2.5-pro"}
	}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameBeforeModel, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.ModelUpdate {
		t.Errorf("expected ModelUpdate, got %v", event.Type)
	}
	if event.SessionID != "model-sess" {
		t.Errorf("expected session_id 'model-sess', got %q", event.SessionID)
	}
	if event.Model != "gemini-2.5-pro" {
		t.Errorf("expected model 'gemini-2.5-pro', got %q", event.Model)
	}
}

func TestParseHookEvent_BeforeModel_EmptyModel_ReturnsNil(t *testing.T) {
	t.Parallel()

	ag := &GeminiCLIAgent{}
	input := `{
		"session_id": "no-model-sess",
		"transcript_path": "/tmp/t.json",
		"llm_request": {"model": ""}
	}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameBeforeModel, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event != nil {
		t.Errorf("expected nil event for empty model, got %+v", event)
	}
}

func TestParseHookEvent_PassThroughHooks_ReturnNil(t *testing.T) {
	t.Parallel()

	passThroughHooks := []string{
		HookNameBeforeTool,
		HookNameAfterTool,
		HookNameAfterModel,
		HookNameBeforeToolSelection,
		HookNameNotification,
	}

	ag := &GeminiCLIAgent{}
	input := `{"session_id": "test", "transcript_path": "/t"}`

	for _, hookName := range passThroughHooks {
		t.Run(hookName, func(t *testing.T) {
			t.Parallel()

			event, err := ag.ParseHookEvent(context.Background(), hookName, strings.NewReader(input))

			if err != nil {
				t.Fatalf("unexpected error for %s: %v", hookName, err)
			}
			if event != nil {
				t.Errorf("expected nil event for %s, got %+v", hookName, event)
			}
		})
	}
}

func TestParseHookEvent_UnknownHook_ReturnsNil(t *testing.T) {
	t.Parallel()

	ag := &GeminiCLIAgent{}
	input := `{"session_id": "unknown", "transcript_path": "/tmp/unknown.json"}`

	event, err := ag.ParseHookEvent(context.Background(), "unknown-hook-name", strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event != nil {
		t.Errorf("expected nil event for unknown hook, got %+v", event)
	}
}

func TestParseHookEvent_EmptyInput(t *testing.T) {
	t.Parallel()

	ag := &GeminiCLIAgent{}

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

	ag := &GeminiCLIAgent{}
	input := `{"session_id": "test", "transcript_path": INVALID}`

	_, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader(input))

	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
	if !strings.Contains(err.Error(), "failed to parse hook input") {
		t.Errorf("expected 'failed to parse hook input' error, got: %v", err)
	}
}

func TestParseHookEvent_AllLifecycleHooks(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		hookName      string
		expectedType  agent.EventType
		expectNil     bool
		inputTemplate string
	}{
		{
			hookName:      HookNameSessionStart,
			expectedType:  agent.SessionStart,
			inputTemplate: `{"session_id": "s1", "transcript_path": "/t"}`,
		},
		{
			hookName:      HookNameBeforeAgent,
			expectedType:  agent.TurnStart,
			inputTemplate: `{"session_id": "s2", "transcript_path": "/t", "prompt": "hi"}`,
		},
		{
			hookName:      HookNameAfterAgent,
			expectedType:  agent.TurnEnd,
			inputTemplate: `{"session_id": "s3", "transcript_path": "/t"}`,
		},
		{
			hookName:      HookNameSessionEnd,
			expectedType:  agent.SessionEnd,
			inputTemplate: `{"session_id": "s4", "transcript_path": "/t"}`,
		},
		{
			hookName:      HookNamePreCompress,
			expectedType:  agent.Compaction,
			inputTemplate: `{"session_id": "s5", "transcript_path": "/t"}`,
		},
		{
			hookName:      HookNameBeforeTool,
			expectNil:     true,
			inputTemplate: `{"session_id": "s6", "transcript_path": "/t"}`,
		},
		{
			hookName:      HookNameAfterTool,
			expectNil:     true,
			inputTemplate: `{"session_id": "s7", "transcript_path": "/t"}`,
		},
		{
			hookName:      HookNameBeforeModel,
			expectedType:  agent.ModelUpdate,
			inputTemplate: `{"session_id": "s8", "transcript_path": "/t", "llm_request": {"model": "gemini-2.5-pro"}}`,
		},
		{
			hookName:      HookNameAfterModel,
			expectNil:     true,
			inputTemplate: `{"session_id": "s9", "transcript_path": "/t"}`,
		},
		{
			hookName:      HookNameBeforeToolSelection,
			expectNil:     true,
			inputTemplate: `{"session_id": "s10", "transcript_path": "/t"}`,
		},
		{
			hookName:      HookNameNotification,
			expectNil:     true,
			inputTemplate: `{"session_id": "s11", "transcript_path": "/t"}`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.hookName, func(t *testing.T) {
			t.Parallel()

			ag := &GeminiCLIAgent{}
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

func TestReadAndParse_ValidInput(t *testing.T) {
	t.Parallel()

	input := `{
		"session_id": "test-123",
		"transcript_path": "/path/to/transcript",
		"cwd": "/home/user",
		"hook_event_name": "session-start",
		"timestamp": "2024-01-15T10:00:00Z"
	}`

	result, err := agent.ReadAndParseHookInput[sessionInfoRaw](strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, result, "expected result, got nil")
	if result.SessionID != "test-123" {
		t.Errorf("expected session_id 'test-123', got %q", result.SessionID)
	}
	if result.TranscriptPath != "/path/to/transcript" {
		t.Errorf("expected transcript_path '/path/to/transcript', got %q", result.TranscriptPath)
	}
	if result.Cwd != "/home/user" {
		t.Errorf("expected cwd '/home/user', got %q", result.Cwd)
	}
}

func TestReadAndParse_EmptyInput(t *testing.T) {
	t.Parallel()

	_, err := agent.ReadAndParseHookInput[sessionInfoRaw](strings.NewReader(""))

	if err == nil {
		t.Fatal("expected error for empty input")
	}
	if !strings.Contains(err.Error(), "empty hook input") {
		t.Errorf("expected 'empty hook input' error, got: %v", err)
	}
}

func TestReadAndParse_InvalidJSON(t *testing.T) {
	t.Parallel()

	_, err := agent.ReadAndParseHookInput[sessionInfoRaw](strings.NewReader("not valid json"))

	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "failed to parse hook input") {
		t.Errorf("expected 'failed to parse hook input' error, got: %v", err)
	}
}

func TestReadAndParse_PartialJSON(t *testing.T) {
	t.Parallel()

	// JSON with only some fields - should still parse (missing fields are zero values)
	input := `{"session_id": "partial-only"}`

	result, err := agent.ReadAndParseHookInput[sessionInfoRaw](strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SessionID != "partial-only" {
		t.Errorf("expected session_id 'partial-only', got %q", result.SessionID)
	}
	if result.TranscriptPath != "" {
		t.Errorf("expected empty transcript_path, got %q", result.TranscriptPath)
	}
}

func TestReadAndParse_ExtraFields(t *testing.T) {
	t.Parallel()

	// JSON with extra fields - should ignore them
	input := `{"session_id": "test", "transcript_path": "/t", "extra_field": "ignored", "another": 123}`

	result, err := agent.ReadAndParseHookInput[sessionInfoRaw](strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SessionID != "test" {
		t.Errorf("expected session_id 'test', got %q", result.SessionID)
	}
}

func TestReadAndParse_AgentHookInput(t *testing.T) {
	t.Parallel()

	input := `{
		"session_id": "agent-session",
		"transcript_path": "/path/to/agent.json",
		"cwd": "/work",
		"hook_event_name": "before-agent",
		"timestamp": "2024-01-15T12:00:00Z",
		"prompt": "User's question here"
	}`

	result, err := agent.ReadAndParseHookInput[agentHookInputRaw](strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SessionID != "agent-session" {
		t.Errorf("expected session_id 'agent-session', got %q", result.SessionID)
	}
	if result.Prompt != "User's question here" {
		t.Errorf("expected prompt 'User's question here', got %q", result.Prompt)
	}
	if result.HookEventName != "before-agent" {
		t.Errorf("expected hook_event_name 'before-agent', got %q", result.HookEventName)
	}
}

// captureStdout swaps os.Stdout for a pipe, runs fn, and returns what was
// written. Sequential (no t.Parallel) because os.Stdout is process-global.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)

	original := os.Stdout
	os.Stdout = w

	done := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(r) //nolint:errcheck // best-effort drain
		done <- data
	}()

	fn()
	require.NoError(t, w.Close())
	os.Stdout = original
	got := <-done
	require.NoError(t, r.Close())
	return string(got)
}

// TestWriteHookResponse_PlainText_NoJSON verifies the response is emitted as
// plain text (not JSON). Gemini CLI v0.40.0 double-displays JSON systemMessage
// (once with the [hookName] tag, once without) — plain text takes only the
// non-tagged path so the user sees the banner once.
func TestWriteHookResponse_PlainText_NoJSON(t *testing.T) {
	ag := &GeminiCLIAgent{}
	out := captureStdout(t, func() {
		require.NoError(t, ag.WriteHookResponse("hello banner"))
	})

	require.Equal(t, "hello banner\n", out, "expected exact plain-text output (no JSON envelope)")
	require.False(t, strings.HasPrefix(strings.TrimSpace(out), "{"),
		"output must not start with '{' — gemini's JSON parser would route it through the duplicate-display path")
}

func TestWriteHookResponse_EmptyMessage_WritesNothing(t *testing.T) {
	ag := &GeminiCLIAgent{}
	out := captureStdout(t, func() {
		require.NoError(t, ag.WriteHookResponse(""))
	})
	require.Empty(t, out, "empty message should produce no output")
}
