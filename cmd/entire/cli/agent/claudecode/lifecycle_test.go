package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

const testFinalAssistantMessage = "Done."

func TestCaptureTranscript_ReturnsOwnedValidatedBytes(t *testing.T) {
	t.Parallel()

	transcriptPath := filepath.Join(t.TempDir(), "transcript.jsonl")
	want := []byte("{\"type\":\"user\",\"message\":{\"content\":\"fix it\"}}\n" +
		"{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"Done.\"}]}}\n")
	require.NoError(t, os.WriteFile(transcriptPath, want, 0o600))

	finalResponse := testFinalAssistantMessage
	snapshot, err := (&ClaudeCodeAgent{}).CaptureTranscript(context.Background(), agent.TranscriptCaptureRequest{
		SessionRef:    transcriptPath,
		StartPosition: 1,
		FinalResponse: &finalResponse,
	})
	require.NoError(t, err)
	require.Equal(t, want, snapshot.Data)
	require.Equal(t, 2, snapshot.Position)

	require.NoError(t, os.WriteFile(transcriptPath, []byte("replaced after capture\n"), 0o600))
	require.True(t, bytes.Equal(want, snapshot.Data), "source mutation changed the owned snapshot")
}

func TestParseHookEvent_SessionStart(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	input := `{"session_id": "test-session-123", "transcript_path": "/tmp/transcript.jsonl"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.SessionStart {
		t.Errorf("expected event type %v, got %v", agent.SessionStart, event.Type)
	}
	if event.SessionID != "test-session-123" {
		t.Errorf("expected session_id 'test-session-123', got %q", event.SessionID)
	}
	if event.SessionRef != "/tmp/transcript.jsonl" {
		t.Errorf("expected session_ref '/tmp/transcript.jsonl', got %q", event.SessionRef)
	}
	if event.Timestamp.IsZero() {
		t.Error("expected non-zero timestamp")
	}
}

func TestParseHookEvent_SessionStart_IncludesModel(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	input := `{"session_id": "model-session", "transcript_path": "/tmp/t.jsonl", "model": "claude-sonnet-4-20250514"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.SessionStart {
		t.Errorf("expected SessionStart, got %v", event.Type)
	}
	if event.Model != "claude-sonnet-4-20250514" {
		t.Errorf("expected model 'claude-sonnet-4-20250514', got %q", event.Model)
	}
}

func TestParseHookEvent_SessionStart_EmptyModel(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	input := `{"session_id": "no-model-session", "transcript_path": "/tmp/t.jsonl"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Model != "" {
		t.Errorf("expected empty model, got %q", event.Model)
	}
}

func TestParseHookEvent_TurnStart(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	input := `{"session_id": "sess-456", "transcript_path": "/tmp/t.jsonl", "prompt": "Hello world"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameUserPromptSubmit, strings.NewReader(input))

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
	if event.Prompt != "Hello world" {
		t.Errorf("expected prompt 'Hello world', got %q", event.Prompt)
	}
}

// The VS Code extension prepends an <ide_opened_file> context block to the
// prompt; it must be stripped so the session/checkpoint title and prompt show
// only what the user typed.
func TestParseHookEvent_TurnStart_StripsIDEContextTags(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	input := `{"session_id":"s1","transcript_path":"/tmp/t.jsonl","prompt":"<ide_opened_file>The user opened /a/b.md in the IDE.</ide_opened_file>\n\nrewrite these docs as one plan"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameUserPromptSubmit, strings.NewReader(input))
	require.NoError(t, err)
	require.NotNil(t, event)
	if event.Prompt != "rewrite these docs as one plan" {
		t.Errorf("IDE context tag not stripped; prompt = %q", event.Prompt)
	}
}

func TestParseHookEvent_TurnEnd(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	input := `{"session_id": "sess-789", "transcript_path": "/tmp/stop.jsonl"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameStop, strings.NewReader(input))

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

func TestParseHookEvent_TurnEnd_PreservesStopHookActive(t *testing.T) {
	t.Parallel()

	input := `{"session_id":"session","transcript_path":"/tmp/stop.jsonl","stop_hook_active":true}`
	event, err := (&ClaudeCodeAgent{}).ParseHookEvent(context.Background(), HookNameStop, strings.NewReader(input))
	require.NoError(t, err)
	require.True(t, event.StopHookActive)
}

func TestParseHookEvent_TurnEnd_PreservesFinalResponseFieldState(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		include     bool
		value       any
		wantPresent bool
		wantValue   *string
		wantError   bool
	}{
		"missing": {wantPresent: false},
		"null": {
			include:     true,
			wantPresent: true,
		},
		"empty": {
			include:     true,
			value:       "",
			wantPresent: true,
			wantValue:   ptrTo(""),
		},
		"non-empty": {
			include:     true,
			value:       testFinalAssistantMessage,
			wantPresent: true,
			wantValue:   ptrTo(testFinalAssistantMessage),
		},
		"wrong type": {
			include:   true,
			value:     42,
			wantError: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			input := map[string]any{
				"session_id":      "session",
				"transcript_path": filepath.Join(t.TempDir(), "transcript.jsonl"),
			}
			if tt.include {
				input["last_assistant_message"] = tt.value
			}
			payload, err := json.Marshal(input)
			require.NoError(t, err)

			event, err := (&ClaudeCodeAgent{}).ParseHookEvent(context.Background(), HookNameStop, strings.NewReader(string(payload)))
			if tt.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantPresent, event.FinalResponsePresent)
			require.Equal(t, tt.wantValue, event.FinalResponse)
		})
	}
}

func ptrTo(value string) *string {
	return &value
}

func TestParseHookEvent_TurnEnd_IncludesModel(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	input := `{"session_id": "sess-stop-model", "transcript_path": "/tmp/stop.jsonl", "model": "claude-opus-4-6"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameStop, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Model != "claude-opus-4-6" {
		t.Errorf("expected model 'claude-opus-4-6', got %q", event.Model)
	}
}

func TestParseHookEvent_SessionEnd(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	input := `{"session_id": "ending-session", "transcript_path": "/tmp/end.jsonl"}`

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

func TestParseHookEvent_SessionEnd_IncludesModel(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	input := `{"session_id": "end-model", "transcript_path": "/tmp/end.jsonl", "model": "claude-sonnet-4-20250514"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameSessionEnd, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Model != "claude-sonnet-4-20250514" {
		t.Errorf("expected model 'claude-sonnet-4-20250514', got %q", event.Model)
	}
}

func TestParseHookEvent_SubagentStart(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	toolInput := json.RawMessage(`{"description": "test task", "prompt": "do something"}`)
	inputData := map[string]any{
		"session_id":      "main-session",
		"transcript_path": "/tmp/main.jsonl",
		"tool_use_id":     "toolu_abc123",
		"tool_input":      toolInput,
	}
	inputBytes, marshalErr := json.Marshal(inputData)
	if marshalErr != nil {
		t.Fatalf("failed to marshal test input: %v", marshalErr)
	}

	event, err := ag.ParseHookEvent(context.Background(), HookNamePreTask, strings.NewReader(string(inputBytes)))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.SubagentStart {
		t.Errorf("expected event type %v, got %v", agent.SubagentStart, event.Type)
	}
	if event.SessionID != "main-session" {
		t.Errorf("expected session_id 'main-session', got %q", event.SessionID)
	}
	if event.ToolUseID != "toolu_abc123" {
		t.Errorf("expected tool_use_id 'toolu_abc123', got %q", event.ToolUseID)
	}
	if event.ToolInput == nil {
		t.Error("expected tool_input to be set")
	}
}

func TestParseHookEvent_SubagentEnd(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	inputData := map[string]any{
		"session_id":      "main-session",
		"transcript_path": "/tmp/main.jsonl",
		"tool_use_id":     "toolu_xyz789",
		"tool_input":      json.RawMessage(`{"prompt": "task done"}`),
		"tool_response": map[string]string{
			"agentId": "agent-subagent-001",
		},
	}
	inputBytes, marshalErr := json.Marshal(inputData)
	if marshalErr != nil {
		t.Fatalf("failed to marshal test input: %v", marshalErr)
	}

	event, err := ag.ParseHookEvent(context.Background(), HookNamePostTask, strings.NewReader(string(inputBytes)))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.SubagentEnd {
		t.Errorf("expected event type %v, got %v", agent.SubagentEnd, event.Type)
	}
	if event.ToolUseID != "toolu_xyz789" {
		t.Errorf("expected tool_use_id 'toolu_xyz789', got %q", event.ToolUseID)
	}
	if event.SubagentID != "agent-subagent-001" {
		t.Errorf("expected subagent_id 'agent-subagent-001', got %q", event.SubagentID)
	}
	// PostToolUse fires at the background launch stub, seconds after launch,
	// not at true completion — Final must stay false so downstream lifecycle
	// code can key its capture-now-vs-defer decision on this flag alone.
	if event.Final {
		t.Error("expected Final to be false for post-task (launch-time) event")
	}
}

func TestParseHookEvent_SubagentEnd_NoAgentID(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	inputData := map[string]any{
		"session_id":      "main-session",
		"transcript_path": "/tmp/main.jsonl",
		"tool_use_id":     "toolu_no_agent",
		"tool_input":      json.RawMessage(`{}`),
		"tool_response":   map[string]string{},
	}
	inputBytes, marshalErr := json.Marshal(inputData)
	if marshalErr != nil {
		t.Fatalf("failed to marshal test input: %v", marshalErr)
	}

	event, err := ag.ParseHookEvent(context.Background(), HookNamePostTask, strings.NewReader(string(inputBytes)))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.SubagentID != "" {
		t.Errorf("expected empty subagent_id, got %q", event.SubagentID)
	}
}

// TestParseHookEvent_SubagentStop covers the true-completion signal for
// background subagents. Background subagents return a launch stub
// immediately, so the launch-time post-task (PostToolUse) hook fires seconds
// after launch, before any work happens — the resulting SubagentEnd event is
// never captured beyond the stub. SubagentStop fires at real completion, even
// after the parent's turn ended, and must translate into the same
// agent.SubagentEnd event, marked Final so lifecycle code can tell the two
// apart and prefer the payload's own transcript path over resolution.
func TestParseHookEvent_SubagentStop(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	input := `{"session_id":"parent-sess","transcript_path":"/tmp/parent.jsonl","hook_event_name":"SubagentStop","agent_id":"a123","agent_transcript_path":"/tmp/parent/subagents/agent-a123.jsonl","tool_use_id":"toolu_01X","cwd":"/repo"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameSubagentStop, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.SubagentEnd {
		t.Errorf("expected event type %v, got %v", agent.SubagentEnd, event.Type)
	}
	if event.SessionID != "parent-sess" {
		t.Errorf("expected session_id 'parent-sess', got %q", event.SessionID)
	}
	if event.SessionRef != "/tmp/parent.jsonl" {
		t.Errorf("expected session_ref '/tmp/parent.jsonl', got %q", event.SessionRef)
	}
	if event.SubagentID != "a123" {
		t.Errorf("expected subagent_id 'a123', got %q", event.SubagentID)
	}
	if event.ToolUseID != "toolu_01X" {
		t.Errorf("expected tool_use_id 'toolu_01X', got %q", event.ToolUseID)
	}
	if event.SubagentTranscriptPath != "/tmp/parent/subagents/agent-a123.jsonl" {
		t.Errorf("expected subagent_transcript '/tmp/parent/subagents/agent-a123.jsonl', got %q", event.SubagentTranscriptPath)
	}
	if !event.Final {
		t.Error("expected Final to be true for SubagentStop (true-completion) event")
	}

	// Defensive case: agent_transcript_path is SDK-documented for the Agent
	// SDK but unverified for Claude Code's settings-file hook payloads, so a
	// payload missing it must leave SubagentTranscriptPath empty rather than
	// error, falling back to ResolveAgentTranscriptPath downstream.
	t.Run("no transcript path", func(t *testing.T) {
		t.Parallel()

		input := `{"session_id":"parent-sess","transcript_path":"/tmp/parent.jsonl","hook_event_name":"SubagentStop","agent_id":"a123","tool_use_id":"toolu_01X","cwd":"/repo"}`

		event, err := ag.ParseHookEvent(context.Background(), HookNameSubagentStop, strings.NewReader(input))

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		require.NotNil(t, event, "expected event, got nil")
		if event.SubagentTranscriptPath != "" {
			t.Errorf("expected empty subagent_transcript, got %q", event.SubagentTranscriptPath)
		}
		if !event.Final {
			t.Error("expected Final to be true for SubagentStop event")
		}
	})
}

func TestParseHookEvent_PostTodo_ReturnsNil(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	input := `{"session_id": "todo-session", "transcript_path": "/tmp/todo.jsonl"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNamePostTodo, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event != nil {
		t.Errorf("expected nil event for post-todo, got %+v", event)
	}
}

func TestParseHookEvent_UnknownHook_ReturnsNil(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	input := `{"session_id": "unknown", "transcript_path": "/tmp/unknown.jsonl"}`

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

	ag := &ClaudeCodeAgent{}

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

	ag := &ClaudeCodeAgent{}
	input := `{"session_id": "test", "transcript_path": INVALID}`

	_, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader(input))

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
			hookName:      HookNameSessionStart,
			expectedType:  agent.SessionStart,
			inputTemplate: `{"session_id": "s1", "transcript_path": "/t"}`,
		},
		{
			hookName:      HookNameUserPromptSubmit,
			expectedType:  agent.TurnStart,
			inputTemplate: `{"session_id": "s2", "transcript_path": "/t", "prompt": "hi"}`,
		},
		{
			hookName:      HookNameStop,
			expectedType:  agent.TurnEnd,
			inputTemplate: `{"session_id": "s3", "transcript_path": "/t"}`,
		},
		{
			hookName:      HookNameSessionEnd,
			expectedType:  agent.SessionEnd,
			inputTemplate: `{"session_id": "s4", "transcript_path": "/t"}`,
		},
		{
			hookName:      HookNamePreTask,
			expectedType:  agent.SubagentStart,
			inputTemplate: `{"session_id": "s5", "transcript_path": "/t", "tool_use_id": "t1", "tool_input": {}}`,
		},
		{
			hookName:      HookNamePostTask,
			expectedType:  agent.SubagentEnd,
			inputTemplate: `{"session_id": "s6", "transcript_path": "/t", "tool_use_id": "t2", "tool_input": {}, "tool_response": {}}`,
		},
		{
			hookName:      HookNamePostTodo,
			expectNil:     true,
			inputTemplate: `{"session_id": "s7", "transcript_path": "/t"}`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.hookName, func(t *testing.T) {
			t.Parallel()

			ag := &ClaudeCodeAgent{}
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

	input := `{"session_id": "test-123", "transcript_path": "/path/to/transcript"}`

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

func TestClaudeCodeAgent_ContextInjector(t *testing.T) {
	t.Parallel()
	c := &ClaudeCodeAgent{}
	if got := c.InjectionEvent(); got != agent.TurnStart {
		t.Errorf("InjectionEvent = %v, want TurnStart", got)
	}
	out, err := c.RenderContextInjection(agent.ContextInjection{Text: "use entire trail"})
	if err != nil {
		t.Fatalf("RenderContextInjection: %v", err)
	}
	var parsed struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("invalid JSON: %v (%q)", err, string(out))
	}
	if parsed.HookSpecificOutput.HookEventName != "UserPromptSubmit" {
		t.Errorf("hookEventName = %q, want UserPromptSubmit", parsed.HookSpecificOutput.HookEventName)
	}
	if parsed.HookSpecificOutput.AdditionalContext != "use entire trail" {
		t.Errorf("additionalContext = %q", parsed.HookSpecificOutput.AdditionalContext)
	}
}
