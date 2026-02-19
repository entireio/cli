package opencode

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestParseHookEvent_SessionStart(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	input := `{"session_id": "sess-abc123", "transcript_path": "/tmp/project/.opencode/sessions/entire/sess-abc123.json"}`

	event, err := ag.ParseHookEvent(HookNameSessionStart, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}
	if event.Type != agent.SessionStart {
		t.Errorf("expected SessionStart, got %v", event.Type)
	}
	if event.SessionID != "sess-abc123" {
		t.Errorf("expected session_id 'sess-abc123', got %q", event.SessionID)
	}
	if event.SessionRef != "/tmp/project/.opencode/sessions/entire/sess-abc123.json" {
		t.Errorf("unexpected session ref: %q", event.SessionRef)
	}
}

func TestParseHookEvent_TurnStart(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	input := `{"session_id": "sess-1", "transcript_path": "/tmp/t.json", "prompt": "Fix the bug in login.ts"}`

	event, err := ag.ParseHookEvent(HookNameTurnStart, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}
	if event.Type != agent.TurnStart {
		t.Errorf("expected TurnStart, got %v", event.Type)
	}
	if event.Prompt != "Fix the bug in login.ts" {
		t.Errorf("expected prompt 'Fix the bug in login.ts', got %q", event.Prompt)
	}
	if event.SessionID != "sess-1" {
		t.Errorf("expected session_id 'sess-1', got %q", event.SessionID)
	}
}

func TestParseHookEvent_TurnEnd(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	input := `{"session_id": "sess-2", "transcript_path": "/tmp/t.json"}`

	event, err := ag.ParseHookEvent(HookNameTurnEnd, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}
	if event.Type != agent.TurnEnd {
		t.Errorf("expected TurnEnd, got %v", event.Type)
	}
	if event.SessionID != "sess-2" {
		t.Errorf("expected session_id 'sess-2', got %q", event.SessionID)
	}
}

func TestParseHookEvent_Compaction(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	input := `{"session_id": "sess-3"}`

	event, err := ag.ParseHookEvent(HookNameCompaction, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}
	if event.Type != agent.Compaction {
		t.Errorf("expected Compaction, got %v", event.Type)
	}
	if event.SessionID != "sess-3" {
		t.Errorf("expected session_id 'sess-3', got %q", event.SessionID)
	}
}

func TestParseHookEvent_SessionEnd(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	input := `{"session_id": "sess-4", "transcript_path": "/tmp/t.json"}`

	event, err := ag.ParseHookEvent(HookNameSessionEnd, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}
	if event.Type != agent.SessionEnd {
		t.Errorf("expected SessionEnd, got %v", event.Type)
	}
}

func TestParseHookEvent_UnknownHook(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	event, err := ag.ParseHookEvent("unknown-hook", strings.NewReader(`{}`))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event != nil {
		t.Errorf("expected nil event for unknown hook, got %+v", event)
	}
}

func TestParseHookEvent_EmptyInput(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	_, err := ag.ParseHookEvent(HookNameSessionStart, strings.NewReader(""))

	if err == nil {
		t.Fatal("expected error for empty input")
	}
	if !strings.Contains(err.Error(), "empty hook input") {
		t.Errorf("expected 'empty hook input' error, got: %v", err)
	}
}

func TestParseHookEvent_MalformedJSON(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	_, err := ag.ParseHookEvent(HookNameSessionStart, strings.NewReader("not json"))

	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestHookNames(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	names := ag.HookNames()

	expected := []string{
		HookNameSessionStart,
		HookNameSessionEnd,
		HookNameTurnStart,
		HookNameTurnEnd,
		HookNameCompaction,
	}

	if len(names) != len(expected) {
		t.Fatalf("expected %d hook names, got %d", len(expected), len(names))
	}

	nameSet := make(map[string]bool)
	for _, n := range names {
		nameSet[n] = true
	}
	for _, e := range expected {
		if !nameSet[e] {
			t.Errorf("missing expected hook name: %s", e)
		}
	}
}
