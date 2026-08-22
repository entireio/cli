package factoryaidroid

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	git "github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/require"
)

func TestParseHookEvent_SessionStart(t *testing.T) {
	t.Parallel()

	ag := &FactoryAIDroidAgent{}
	input := `{"session_id": "test-session", "transcript_path": "/tmp/transcript.jsonl"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.SessionStart {
		t.Errorf("expected SessionStart, got %v", event.Type)
	}
	if event.SessionID != "test-session" {
		t.Errorf("expected session_id 'test-session', got %q", event.SessionID)
	}
	if event.SessionRef != "/tmp/transcript.jsonl" {
		t.Errorf("expected transcript_path '/tmp/transcript.jsonl', got %q", event.SessionRef)
	}
}

func TestParseHookEvent_TurnStart(t *testing.T) {
	t.Parallel()

	ag := &FactoryAIDroidAgent{}
	input := `{"session_id": "sess-1", "transcript_path": "/tmp/t.jsonl", "prompt": "Fix the bug"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameUserPromptSubmit, strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Type != agent.TurnStart {
		t.Errorf("expected TurnStart, got %v", event.Type)
	}
	if event.Prompt != "Fix the bug" {
		t.Errorf("expected prompt 'Fix the bug', got %q", event.Prompt)
	}
	require.Equal(t, NativeHookUserPromptSubmit, event.NativeHook)
}

func TestFactoryAIDroidAgent_ContextInjector(t *testing.T) {
	t.Parallel()

	ag := &FactoryAIDroidAgent{}
	require.Equal(t, agent.TurnStart, ag.InjectionEvent())

	out, err := ag.RenderContextInjection(agent.ContextInjection{
		Text: "cross-repo evidence", NativeHook: NativeHookUserPromptSubmit,
	})
	require.NoError(t, err)
	require.JSONEq(t, `{
		"hookSpecificOutput": {
			"hookEventName": "UserPromptSubmit",
			"additionalContext": "cross-repo evidence"
		}
	}`, string(out))
}

// TestFactoryAIDroidAgent_ContextInjectionNamesTheNativeHook is the exec-mode
// transport: `droid exec` never fires UserPromptSubmit, so entire installs the
// user-prompt-submit hook under native SessionStart as well. Both produce
// TurnStart, but Factory discards a response whose hookEventName names the hook
// that did not fire — so the whole injection was silently dropped in exec mode
// until the native name travelled with the event.
func TestFactoryAIDroidAgent_ContextInjectionNamesTheNativeHook(t *testing.T) {
	t.Parallel()

	ag := &FactoryAIDroidAgent{}
	// The payload Factory sends to a SessionStart-registered hook: no prompt key,
	// and its own `source`.
	input := `{"session_id": "exec-sess", "transcript_path": "/tmp/exec.jsonl", "source": "startup"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameUserPromptSubmit, strings.NewReader(input))
	require.NoError(t, err)
	require.Equal(t, agent.TurnStart, event.Type, "exec mode must still open a turn")
	require.Equal(t, NativeHookSessionStart, event.NativeHook)

	out, err := ag.RenderContextInjection(agent.ContextInjection{
		Text: "cross-repo evidence", NativeHook: event.NativeHook,
	})
	require.NoError(t, err)
	require.JSONEq(t, `{
		"hookSpecificOutput": {
			"hookEventName": "SessionStart",
			"additionalContext": "cross-repo evidence"
		}
	}`, string(out))
}

// TestFactoryAIDroidAgent_ContextInjectionDefaultsToUserPromptSubmit keeps the
// interactive path working for callers that carry no native hook name.
func TestFactoryAIDroidAgent_ContextInjectionDefaultsToUserPromptSubmit(t *testing.T) {
	t.Parallel()

	ag := &FactoryAIDroidAgent{}

	out, err := ag.RenderContextInjection(agent.ContextInjection{Text: "cross-repo evidence"})
	require.NoError(t, err)
	require.Contains(t, string(out), `"hookEventName":"UserPromptSubmit"`)

	empty, err := ag.RenderContextInjection(agent.ContextInjection{Text: "   "})
	require.NoError(t, err)
	require.Empty(t, empty, "blank text must write nothing to stdout")
}

func TestParseHookEvent_TurnStart_IncludesModel(t *testing.T) {
	t.Parallel()

	ag := &FactoryAIDroidAgent{}
	input := `{"session_id": "sess-m", "transcript_path": "/tmp/t.jsonl", "prompt": "hi", "model": "gpt-4o"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameUserPromptSubmit, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Model != "gpt-4o" {
		t.Errorf("expected model 'gpt-4o', got %q", event.Model)
	}
}

func TestParseHookEvent_TurnStart_EmptyModel(t *testing.T) {
	t.Parallel()

	ag := &FactoryAIDroidAgent{}
	input := `{"session_id": "sess-nm", "transcript_path": "/tmp/t.jsonl", "prompt": "hi"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameUserPromptSubmit, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Model != "" {
		t.Errorf("expected empty model, got %q", event.Model)
	}
}

// TestParseHookEvent_TurnStart_SessionStartFormat verifies that parseTurnStart
// handles SessionStart-format stdin (no "prompt" field). This happens when
// user-prompt-submit is installed on the SessionStart event type to ensure
// TurnStart fires in droid exec mode.
func TestParseHookEvent_TurnStart_SessionStartFormat(t *testing.T) {
	t.Parallel()

	ag := &FactoryAIDroidAgent{}
	// SessionStart-format stdin: only session_id and transcript_path, no prompt
	input := `{"session_id": "exec-sess", "transcript_path": "/tmp/exec.jsonl"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameUserPromptSubmit, strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Type != agent.TurnStart {
		t.Errorf("expected TurnStart, got %v", event.Type)
	}
	if event.SessionID != "exec-sess" {
		t.Errorf("expected session_id 'exec-sess', got %q", event.SessionID)
	}
	if event.SessionRef != "/tmp/exec.jsonl" {
		t.Errorf("expected transcript_path '/tmp/exec.jsonl', got %q", event.SessionRef)
	}
	if event.Prompt != "" {
		t.Errorf("expected empty prompt, got %q", event.Prompt)
	}
	require.Equal(t, NativeHookSessionStart, event.NativeHook,
		"a payload with no prompt key came from native SessionStart, whatever else it carries")
}

func TestParseHookEvent_TurnEnd(t *testing.T) {
	t.Parallel()

	ag := &FactoryAIDroidAgent{}
	input := `{"session_id": "sess-2", "transcript_path": "/tmp/t.jsonl"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameStop, strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Type != agent.TurnEnd {
		t.Errorf("expected TurnEnd, got %v", event.Type)
	}
}

func TestParseHookEvent_SessionEnd(t *testing.T) {
	t.Parallel()

	ag := &FactoryAIDroidAgent{}
	input := `{"session_id": "sess-3", "transcript_path": "/tmp/t.jsonl"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameSessionEnd, strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Type != agent.SessionEnd {
		t.Errorf("expected SessionEnd, got %v", event.Type)
	}
}

func TestParseHookEvent_SubagentStart(t *testing.T) {
	t.Parallel()

	ag := &FactoryAIDroidAgent{}
	input := `{"session_id": "sess-4", "transcript_path": "/tmp/t.jsonl", "tool_use_id": "tu-123", "tool_input": {"prompt": "do something"}}`

	event, err := ag.ParseHookEvent(context.Background(), HookNamePreToolUse, strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Type != agent.SubagentStart {
		t.Errorf("expected SubagentStart, got %v", event.Type)
	}
	if event.ToolUseID != "tu-123" {
		t.Errorf("expected tool_use_id 'tu-123', got %q", event.ToolUseID)
	}
}

func TestParseHookEvent_SubagentEnd(t *testing.T) {
	t.Parallel()

	ag := &FactoryAIDroidAgent{}
	input := `{"session_id": "sess-5", "transcript_path": "/tmp/t.jsonl", "tool_use_id": "tu-456", "tool_input": {}, "tool_response": {"agentId": "agent-789"}}`

	event, err := ag.ParseHookEvent(context.Background(), HookNamePostToolUse, strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Type != agent.SubagentEnd {
		t.Errorf("expected SubagentEnd, got %v", event.Type)
	}
	if event.SubagentID != "agent-789" {
		t.Errorf("expected SubagentID 'agent-789', got %q", event.SubagentID)
	}
}

func TestParseHookEvent_SubagentStart_MissingToolUseID(t *testing.T) {
	repoDir := t.TempDir()
	if _, err := git.PlainInit(repoDir, false); err != nil {
		t.Fatalf("git init: %v", err)
	}
	t.Chdir(repoDir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	ag := &FactoryAIDroidAgent{}
	input := `{"session_id": "sess-4", "transcript_path": "/tmp/t.jsonl", "tool_name": "Task", "tool_input": {"prompt": "do something"}}`

	event, err := ag.ParseHookEvent(context.Background(), HookNamePreToolUse, strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.ToolUseID == "" {
		t.Fatal("expected fallback tool_use_id, got empty string")
	}
}

func TestParseHookEvent_SubagentEnd_StringToolResponse(t *testing.T) {
	t.Parallel()

	ag := &FactoryAIDroidAgent{}
	input := `{"session_id": "sess-5", "transcript_path": "/tmp/t.jsonl", "tool_name": "Task", "tool_input": {}, "tool_response": "agentId: agent-789"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNamePostToolUse, strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Type != agent.SubagentEnd {
		t.Errorf("expected SubagentEnd, got %v", event.Type)
	}
	if event.SubagentID != "agent-789" {
		t.Errorf("expected SubagentID 'agent-789', got %q", event.SubagentID)
	}
	if event.ToolUseID == "" {
		t.Fatal("expected fallback tool_use_id, got empty string")
	}
}

func TestParseHookEvent_MissingToolUseID_RepeatedInputsStayUniqueAndCorrelate(t *testing.T) {
	repoDir := t.TempDir()
	if _, err := git.PlainInit(repoDir, false); err != nil {
		t.Fatalf("git init: %v", err)
	}
	t.Chdir(repoDir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	ag := &FactoryAIDroidAgent{}
	input := `{"session_id": "sess-repeat", "transcript_path": "/tmp/t.jsonl", "tool_name": "Task", "tool_input": {"prompt": "do something"}}`

	startOne, err := ag.ParseHookEvent(context.Background(), HookNamePreToolUse, strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error on first start: %v", err)
	}
	startTwo, err := ag.ParseHookEvent(context.Background(), HookNamePreToolUse, strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error on second start: %v", err)
	}
	if startOne.ToolUseID == startTwo.ToolUseID {
		t.Fatalf("expected unique fallback tool_use_id values, got %q", startOne.ToolUseID)
	}

	endTwo, err := ag.ParseHookEvent(context.Background(), HookNamePostToolUse, strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error on second end: %v", err)
	}
	if endTwo.ToolUseID != startTwo.ToolUseID {
		t.Fatalf("expected most recent fallback tool_use_id %q, got %q", startTwo.ToolUseID, endTwo.ToolUseID)
	}

	endOne, err := ag.ParseHookEvent(context.Background(), HookNamePostToolUse, strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error on first end: %v", err)
	}
	if endOne.ToolUseID != startOne.ToolUseID {
		t.Fatalf("expected earlier fallback tool_use_id %q, got %q", startOne.ToolUseID, endOne.ToolUseID)
	}

	matches, err := filepath.Glob(filepath.Join(repoDir, paths.EntireTmpDir, fallbackToolUseStatePrefix+"*.json"))
	if err != nil {
		t.Fatalf("glob fallback state files: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected fallback state cleanup, found %v", matches)
	}
}

func TestParseHookEvent_Compaction(t *testing.T) {
	t.Parallel()

	ag := &FactoryAIDroidAgent{}
	input := `{"session_id": "sess-6", "transcript_path": "/tmp/t.jsonl"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNamePreCompact, strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Type != agent.Compaction {
		t.Errorf("expected Compaction, got %v", event.Type)
	}
}

func TestParseHookEvent_PassThroughHooks(t *testing.T) {
	t.Parallel()

	passThroughHooks := []string{
		HookNameSubagentStop,
		HookNameNotification,
	}

	for _, hookName := range passThroughHooks {
		t.Run(hookName, func(t *testing.T) {
			t.Parallel()
			ag := &FactoryAIDroidAgent{}
			event, err := ag.ParseHookEvent(context.Background(), hookName, strings.NewReader(`{"session_id":"s"}`))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if event != nil {
				t.Errorf("expected nil event for %s, got %+v", hookName, event)
			}
		})
	}
}

func TestParseHookEvent_UnknownHook(t *testing.T) {
	t.Parallel()

	ag := &FactoryAIDroidAgent{}
	event, err := ag.ParseHookEvent(context.Background(), "unknown-hook", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event != nil {
		t.Errorf("expected nil event for unknown hook, got %+v", event)
	}
}

func TestParseHookEvent_EmptyInput(t *testing.T) {
	t.Parallel()

	ag := &FactoryAIDroidAgent{}
	_, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader(""))
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestParseHookEvent_MalformedJSON(t *testing.T) {
	t.Parallel()

	ag := &FactoryAIDroidAgent{}
	_, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader("not json"))
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}
