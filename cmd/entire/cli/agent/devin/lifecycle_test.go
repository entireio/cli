package devin

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Payloads below are verbatim captures from devin 3000.2.17 (see AGENT.md).

func TestParseHookEvent_SessionStart(t *testing.T) {
	transcriptDir := t.TempDir()
	t.Setenv("ENTIRE_TEST_DEVIN_TRANSCRIPT_DIR", transcriptDir)

	d := &DevinAgent{}
	payload := `{"hook_event_name":"SessionStart","source":"startup","session_id":"snowy-efraasia"}`
	event, err := d.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader(payload))
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if event.Type != agent.SessionStart {
		t.Errorf("Type = %v, want SessionStart", event.Type)
	}
	if event.SessionID != liveSessionID {
		t.Errorf("SessionID = %q, want %q", event.SessionID, liveSessionID)
	}
	want := filepath.Join(transcriptDir, liveSessionID+".json")
	if event.SessionRef != want {
		t.Errorf("SessionRef = %q, want %q", event.SessionRef, want)
	}
}

func TestParseHookEvent_UserPromptSubmit(t *testing.T) {
	transcriptDir := t.TempDir()
	t.Setenv("ENTIRE_TEST_DEVIN_TRANSCRIPT_DIR", transcriptDir)

	d := &DevinAgent{}
	payload := `{"hook_event_name":"UserPromptSubmit","prompt":"Create a file named hello.txt","session_id":"snowy-efraasia","prompt_id":"54697fcd-fbea-4b60-b718-f3abcf9375fc"}`
	event, err := d.ParseHookEvent(context.Background(), HookNameUserPromptSubmit, strings.NewReader(payload))
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if event.Type != agent.TurnStart {
		t.Errorf("Type = %v, want TurnStart", event.Type)
	}
	if event.Prompt != "Create a file named hello.txt" {
		t.Errorf("Prompt = %q", event.Prompt)
	}
	if event.SessionID != liveSessionID {
		t.Errorf("SessionID = %q", event.SessionID)
	}
	if event.SessionRef == "" {
		t.Error("SessionRef is empty, want derived transcript path")
	}
}

func TestParseHookEvent_Stop(t *testing.T) {
	transcriptDir := t.TempDir()
	t.Setenv("ENTIRE_TEST_DEVIN_TRANSCRIPT_DIR", transcriptDir)

	d := &DevinAgent{}
	payload := `{"hook_event_name":"Stop","stop_hook_active":false,"session_id":"snowy-efraasia","prompt_id":"54697fcd-fbea-4b60-b718-f3abcf9375fc"}`
	event, err := d.ParseHookEvent(context.Background(), HookNameStop, strings.NewReader(payload))
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if event.Type != agent.TurnEnd {
		t.Errorf("Type = %v, want TurnEnd", event.Type)
	}
	if event.SessionRef != filepath.Join(transcriptDir, liveSessionID+".json") {
		t.Errorf("SessionRef = %q", event.SessionRef)
	}
}

func TestParseHookEvent_SessionEnd(t *testing.T) {
	t.Setenv("ENTIRE_TEST_DEVIN_TRANSCRIPT_DIR", t.TempDir())

	d := &DevinAgent{}
	payload := `{"hook_event_name":"SessionEnd","reason":"session_complete","session_id":"snowy-efraasia","prompt_id":"54697fcd-fbea-4b60-b718-f3abcf9375fc"}`
	event, err := d.ParseHookEvent(context.Background(), HookNameSessionEnd, strings.NewReader(payload))
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if event.Type != agent.SessionEnd {
		t.Errorf("Type = %v, want SessionEnd", event.Type)
	}
}

func TestParseHookEvent_PostToolUse_FileModification(t *testing.T) {
	t.Setenv("ENTIRE_TEST_DEVIN_TRANSCRIPT_DIR", t.TempDir())

	d := &DevinAgent{}
	payload := `{"hook_event_name":"PostToolUse","tool_name":"write","tool_input":{"file_path":"/repo/hello.txt","content":"hook test ok"},"tool_use_id":"write_0","tool_response":{"success":true,"output":"File created","error":null},"session_id":"snowy-efraasia","prompt_id":"54697fcd-fbea-4b60-b718-f3abcf9375fc"}`
	event, err := d.ParseHookEvent(context.Background(), HookNamePostToolUse, strings.NewReader(payload))
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if event == nil {
		t.Fatal("event is nil, want ToolUse event")
	}
	if event.Type != agent.ToolUse {
		t.Errorf("Type = %v, want ToolUse", event.Type)
	}
	if len(event.ModifiedFiles) != 1 || event.ModifiedFiles[0] != "/repo/hello.txt" {
		t.Errorf("ModifiedFiles = %v", event.ModifiedFiles)
	}
	if event.ToolUseID != "write_0" {
		t.Errorf("ToolUseID = %q", event.ToolUseID)
	}
}

func TestParseHookEvent_PostToolUse_PassThrough(t *testing.T) {
	t.Setenv("ENTIRE_TEST_DEVIN_TRANSCRIPT_DIR", t.TempDir())
	d := &DevinAgent{}

	cases := []struct {
		name    string
		payload string
	}{
		{"non-file tool", `{"hook_event_name":"PostToolUse","tool_name":"read","tool_input":{"file_path":"/repo/a.txt"},"tool_use_id":"read_0","tool_response":{"success":true},"session_id":"s"}`},
		{"failed tool", `{"hook_event_name":"PostToolUse","tool_name":"write","tool_input":{"file_path":"/repo/a.txt"},"tool_use_id":"write_0","tool_response":{"success":false},"session_id":"s"}`},
		{"missing file_path", `{"hook_event_name":"PostToolUse","tool_name":"write","tool_input":{},"tool_use_id":"write_0","tool_response":{"success":true},"session_id":"s"}`},
		// Devin has been observed to spawn secondary PostToolUse matcher
		// groups without piping the payload — must not error (best-effort).
		{"empty stdin", ``},
		{"malformed payload", `{not json`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			event, err := d.ParseHookEvent(context.Background(), HookNamePostToolUse, strings.NewReader(tc.payload))
			if err != nil {
				t.Fatalf("ParseHookEvent: %v", err)
			}
			if event != nil {
				t.Errorf("event = %+v, want nil (pass-through)", event)
			}
		})
	}
}

func TestParseHookEvent_UnknownHook(t *testing.T) {
	t.Parallel()
	d := &DevinAgent{}
	event, err := d.ParseHookEvent(context.Background(), "unknown-hook", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if event != nil {
		t.Errorf("event = %+v, want nil for unknown hook", event)
	}
}

func TestParseHookEvent_EmptyInput(t *testing.T) {
	t.Parallel()
	d := &DevinAgent{}
	if _, err := d.ParseHookEvent(context.Background(), HookNameStop, strings.NewReader("")); err == nil {
		t.Error("expected error for empty input")
	}
}

func TestParseHookEvent_MalformedJSON(t *testing.T) {
	t.Parallel()
	d := &DevinAgent{}
	if _, err := d.ParseHookEvent(context.Background(), HookNameStop, strings.NewReader("{not json")); err == nil {
		t.Error("expected error for malformed JSON")
	}
}

func TestHookNames(t *testing.T) {
	t.Parallel()
	d := &DevinAgent{}
	names := d.HookNames()
	want := []string{
		HookNameSessionStart,
		HookNameSessionEnd,
		HookNameStop,
		HookNameUserPromptSubmit,
		HookNamePostToolUse,
	}
	if len(names) != len(want) {
		t.Fatalf("HookNames() = %v, want %v", names, want)
	}
	for i, name := range want {
		if names[i] != name {
			t.Errorf("HookNames()[%d] = %q, want %q", i, names[i], name)
		}
	}
}
