package kiro

import (
	"context"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestParseHookEvent_SessionStart(t *testing.T) {
	t.Parallel()

	ag := &KiroAgent{}
	input := `{"session_id": "test-session-123", "hook_event_name": "agentSpawn", "cwd": "/tmp/repo"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameAgentSpawn, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}
	if event.Type != agent.SessionStart {
		t.Errorf("expected event type %v, got %v", agent.SessionStart, event.Type)
	}
	if event.SessionID != "test-session-123" {
		t.Errorf("expected session_id 'test-session-123', got %q", event.SessionID)
	}
	if event.Timestamp.IsZero() {
		t.Error("expected non-zero timestamp")
	}
}

func TestParseHookEvent_TurnStart(t *testing.T) {
	t.Parallel()

	ag := &KiroAgent{}
	input := `{"session_id": "sess-456", "hook_event_name": "userPromptSubmit", "cwd": "/tmp/repo", "prompt": "Hello world"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameUserPromptSubmit, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}
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

func TestParseHookEvent_TurnEnd(t *testing.T) {
	t.Parallel()

	ag := &KiroAgent{}
	input := `{"session_id": "sess-789", "hook_event_name": "stop", "cwd": "/tmp/repo"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameStop, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}
	if event.Type != agent.TurnEnd {
		t.Errorf("expected event type %v, got %v", agent.TurnEnd, event.Type)
	}
	if event.SessionID != "sess-789" {
		t.Errorf("expected session_id 'sess-789', got %q", event.SessionID)
	}
}

func TestParseHookEvent_PreToolUse(t *testing.T) {
	t.Parallel()

	ag := &KiroAgent{}
	input := `{"session_id": "sess-tool", "hook_event_name": "preToolUse", "cwd": "/tmp/repo", "tool_name": "write_file", "tool_use_id": "tu_123"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNamePreToolUse, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}
	if event.Type != agent.SubagentStart {
		t.Errorf("expected event type %v, got %v", agent.SubagentStart, event.Type)
	}
	if event.SessionID != "sess-tool" {
		t.Errorf("expected session_id 'sess-tool', got %q", event.SessionID)
	}
	if event.ToolUseID != "tu_123" {
		t.Errorf("expected tool_use_id 'tu_123', got %q", event.ToolUseID)
	}
}

func TestParseHookEvent_PreToolUse_NoToolName_ReturnsNil(t *testing.T) {
	t.Parallel()

	ag := &KiroAgent{}
	input := `{"session_id": "sess-empty", "hook_event_name": "preToolUse", "cwd": "/tmp/repo"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNamePreToolUse, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event != nil {
		t.Errorf("expected nil event for preToolUse with no tool_name, got %+v", event)
	}
}

func TestParseHookEvent_PostToolUse(t *testing.T) {
	t.Parallel()

	ag := &KiroAgent{}
	input := `{"session_id": "sess-post", "hook_event_name": "postToolUse", "cwd": "/tmp/repo", "tool_name": "write_file", "tool_use_id": "tu_456"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNamePostToolUse, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}
	if event.Type != agent.SubagentEnd {
		t.Errorf("expected event type %v, got %v", agent.SubagentEnd, event.Type)
	}
	if event.ToolUseID != "tu_456" {
		t.Errorf("expected tool_use_id 'tu_456', got %q", event.ToolUseID)
	}
}

func TestParseHookEvent_PostToolUse_NoToolName_ReturnsNil(t *testing.T) {
	t.Parallel()

	ag := &KiroAgent{}
	input := `{"session_id": "sess-empty", "hook_event_name": "postToolUse", "cwd": "/tmp/repo"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNamePostToolUse, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event != nil {
		t.Errorf("expected nil event for postToolUse with no tool_name, got %+v", event)
	}
}

func TestParseHookEvent_UnknownHook_ReturnsNil(t *testing.T) {
	t.Parallel()

	ag := &KiroAgent{}
	input := `{"session_id": "unknown"}`

	event, err := ag.ParseHookEvent(context.Background(), "unknown-hook-name", strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event != nil {
		t.Errorf("expected nil event for unknown hook, got %+v", event)
	}
}

func TestParseHookEvent_EmptyInput_ReturnsError(t *testing.T) {
	t.Parallel()

	ag := &KiroAgent{}

	_, err := ag.ParseHookEvent(context.Background(), HookNameAgentSpawn, strings.NewReader(""))

	if err == nil {
		t.Fatal("expected error for empty input, got nil")
	}
	if !strings.Contains(err.Error(), "empty hook input") {
		t.Errorf("expected 'empty hook input' error, got: %v", err)
	}
}

func TestParseHookEvent_MalformedJSON(t *testing.T) {
	t.Parallel()

	ag := &KiroAgent{}
	input := `{"session_id": "test", "cwd": INVALID}`

	_, err := ag.ParseHookEvent(context.Background(), HookNameAgentSpawn, strings.NewReader(input))

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
			hookName:      HookNameAgentSpawn,
			expectedType:  agent.SessionStart,
			inputTemplate: `{"session_id": "s1", "cwd": "/tmp"}`,
		},
		{
			hookName:      HookNameUserPromptSubmit,
			expectedType:  agent.TurnStart,
			inputTemplate: `{"session_id": "s2", "cwd": "/tmp", "prompt": "hi"}`,
		},
		{
			hookName:      HookNameStop,
			expectedType:  agent.TurnEnd,
			inputTemplate: `{"session_id": "s3", "cwd": "/tmp"}`,
		},
		{
			hookName:      HookNamePreToolUse,
			expectedType:  agent.SubagentStart,
			inputTemplate: `{"session_id": "s4", "cwd": "/tmp", "tool_name": "write", "tool_use_id": "t1"}`,
		},
		{
			hookName:      HookNamePostToolUse,
			expectedType:  agent.SubagentEnd,
			inputTemplate: `{"session_id": "s5", "cwd": "/tmp", "tool_name": "write", "tool_use_id": "t2"}`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.hookName, func(t *testing.T) {
			t.Parallel()

			ag := &KiroAgent{}
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

			if event == nil {
				t.Fatal("expected event, got nil")
			}
			if event.Type != tc.expectedType {
				t.Errorf("expected event type %v, got %v", tc.expectedType, event.Type)
			}
		})
	}
}
