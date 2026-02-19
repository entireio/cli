package pi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestParseHookEvent_SessionStart(t *testing.T) {
	t.Parallel()

	ag := &PiAgent{}
	input := `{"session_id":"pi-session-1","transcript_path":"/tmp/pi.jsonl"}`

	event, err := ag.ParseHookEvent(HookNameSessionStart, strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}
	if event.Type != agent.SessionStart {
		t.Fatalf("expected type %v, got %v", agent.SessionStart, event.Type)
	}
	if event.SessionID != "pi-session-1" {
		t.Fatalf("expected session_id pi-session-1, got %q", event.SessionID)
	}
}

func TestParseHookEvent_TurnStart(t *testing.T) {
	t.Parallel()

	ag := &PiAgent{}
	input := `{"session_id":"pi-session-2","transcript_path":"/tmp/pi.jsonl","prompt":"do work"}`

	event, err := ag.ParseHookEvent(HookNameUserPromptSubmit, strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}
	if event.Type != agent.TurnStart {
		t.Fatalf("expected type %v, got %v", agent.TurnStart, event.Type)
	}
	if event.Prompt != "do work" {
		t.Fatalf("expected prompt %q, got %q", "do work", event.Prompt)
	}
}

func TestParseHookEvent_TurnEnd_WithLeafMetadata(t *testing.T) {
	t.Parallel()

	ag := &PiAgent{}
	input := `{"session_id":"pi-session-3","transcript_path":"/tmp/pi.jsonl","leaf_id":"leaf-abc"}`

	event, err := ag.ParseHookEvent(HookNameStop, strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}
	if event.Type != agent.TurnEnd {
		t.Fatalf("expected type %v, got %v", agent.TurnEnd, event.Type)
	}
	if event.Metadata["leaf_id"] != "leaf-abc" {
		t.Fatalf("expected leaf_id metadata, got %+v", event.Metadata)
	}
}

func TestParseHookEvent_SessionEnd(t *testing.T) {
	t.Parallel()

	ag := &PiAgent{}
	input := `{"session_id":"pi-session-4","transcript_path":"/tmp/pi.jsonl"}`

	event, err := ag.ParseHookEvent(HookNameSessionEnd, strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}
	if event.Type != agent.SessionEnd {
		t.Fatalf("expected type %v, got %v", agent.SessionEnd, event.Type)
	}
}

func TestParseHookEvent_Compaction(t *testing.T) {
	t.Parallel()

	ag := &PiAgent{}
	input := `{"session_id":"pi-session-5","transcript_path":"/tmp/pi.jsonl"}`

	event, err := ag.ParseHookEvent(HookNameBeforeCompact, strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}
	if event.Type != agent.Compaction {
		t.Fatalf("expected type %v, got %v", agent.Compaction, event.Type)
	}
}

func TestParseHookEvent_PassThroughHooks_ReturnNil(t *testing.T) {
	t.Parallel()

	ag := &PiAgent{}
	input := `{"session_id":"pi-session-6","transcript_path":"/tmp/pi.jsonl"}`

	for _, hook := range []string{HookNameBeforeTool, HookNameAfterTool} {
		event, err := ag.ParseHookEvent(hook, strings.NewReader(input))
		if err != nil {
			t.Fatalf("unexpected error for hook %s: %v", hook, err)
		}
		if event != nil {
			t.Fatalf("expected nil event for hook %s, got %+v", hook, event)
		}
	}
}

func TestParseHookEvent_UnknownHook_ReturnsNil(t *testing.T) {
	t.Parallel()

	ag := &PiAgent{}
	input := `{"session_id":"pi-session-7","transcript_path":"/tmp/pi.jsonl"}`

	event, err := ag.ParseHookEvent("unknown-hook", strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event != nil {
		t.Fatalf("expected nil event, got %+v", event)
	}
}

func TestParseHookEvent_EmptyInput(t *testing.T) {
	t.Parallel()

	ag := &PiAgent{}
	_, err := ag.ParseHookEvent(HookNameSessionStart, strings.NewReader(""))
	if err == nil {
		t.Fatal("expected error for empty hook input")
	}
}

func TestParseHookEvent_MalformedJSON(t *testing.T) {
	t.Parallel()

	ag := &PiAgent{}
	_, err := ag.ParseHookEvent(HookNameSessionStart, strings.NewReader(`{"session_id":`))
	if err == nil {
		t.Fatal("expected error for malformed json")
	}
}

func TestExtractPromptsAndSummaryWithLeaf(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "pi.jsonl")
	data := `{"type":"message","id":"1","parentId":null,"message":{"role":"user","content":"root prompt"}}
{"type":"message","id":"2","parentId":"1","message":{"role":"assistant","content":[{"type":"text","text":"common response"}]}}
{"type":"message","id":"3","parentId":"2","message":{"role":"user","content":"left prompt"}}
{"type":"message","id":"4","parentId":"2","message":{"role":"user","content":"right prompt"}}
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	ag := &PiAgent{}

	leftPrompts, err := ag.ExtractPromptsWithLeaf(path, 0, "3")
	if err != nil {
		t.Fatalf("ExtractPromptsWithLeaf(left) error: %v", err)
	}
	if len(leftPrompts) != 2 || leftPrompts[1] != "left prompt" {
		t.Fatalf("unexpected left prompts: %#v", leftPrompts)
	}

	rightPrompts, err := ag.ExtractPromptsWithLeaf(path, 0, "4")
	if err != nil {
		t.Fatalf("ExtractPromptsWithLeaf(right) error: %v", err)
	}
	if len(rightPrompts) != 2 || rightPrompts[1] != "right prompt" {
		t.Fatalf("unexpected right prompts: %#v", rightPrompts)
	}

	summary, err := ag.ExtractSummaryWithLeaf(path, "3")
	if err != nil {
		t.Fatalf("ExtractSummaryWithLeaf error: %v", err)
	}
	if summary != "common response" {
		t.Fatalf("expected summary %q, got %q", "common response", summary)
	}
}

func TestCalculateTokenUsageWithLeaf(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "pi.jsonl")
	data := `{"type":"message","id":"1","parentId":null,"message":{"role":"user","content":"root prompt"}}
{"type":"message","id":"2","parentId":"1","message":{"role":"assistant","usage":{"input_tokens":5,"output_tokens":1}}}
{"type":"message","id":"3","parentId":"1","message":{"role":"assistant","usage":{"input_tokens":7,"output_tokens":2}}}
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	ag := &PiAgent{}

	leftUsage, err := ag.CalculateTokenUsageWithLeaf(path, 0, "2")
	if err != nil {
		t.Fatalf("CalculateTokenUsageWithLeaf(left) error: %v", err)
	}
	if leftUsage.APICallCount != 1 || leftUsage.InputTokens != 5 || leftUsage.OutputTokens != 1 {
		t.Fatalf("unexpected left usage: %+v", leftUsage)
	}

	rightUsage, err := ag.CalculateTokenUsageWithLeaf(path, 0, "3")
	if err != nil {
		t.Fatalf("CalculateTokenUsageWithLeaf(right) error: %v", err)
	}
	if rightUsage.APICallCount != 1 || rightUsage.InputTokens != 7 || rightUsage.OutputTokens != 2 {
		t.Fatalf("unexpected right usage: %+v", rightUsage)
	}
}
