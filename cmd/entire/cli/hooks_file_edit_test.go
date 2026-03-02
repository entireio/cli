package cli

import (
	"strings"
	"testing"
)

func TestParseFileEditHookInput_Write(t *testing.T) {
	t.Parallel()
	input := `{
		"session_id": "abc-123",
		"tool_name": "Write",
		"tool_use_id": "tu-1",
		"tool_input": {"file_path": "/repo/cmd/main.go", "content": "package main\n\nfunc main() {\n}\n"},
		"tool_response": ""
	}`
	parsed, err := parseFileEditHookInput(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parseFileEditHookInput: %v", err)
	}
	if parsed.SessionID != "abc-123" {
		t.Errorf("SessionID = %q, want %q", parsed.SessionID, "abc-123")
	}
	if parsed.ToolName != "Write" {
		t.Errorf("ToolName = %q, want %q", parsed.ToolName, "Write")
	}
	if parsed.FilePath != "/repo/cmd/main.go" {
		t.Errorf("FilePath = %q, want %q", parsed.FilePath, "/repo/cmd/main.go")
	}
	if parsed.LinesAdded != 4 {
		t.Errorf("LinesAdded = %d, want 4", parsed.LinesAdded)
	}
	if parsed.LinesRemoved != 0 {
		t.Errorf("LinesRemoved = %d, want 0", parsed.LinesRemoved)
	}
}

func TestParseFileEditHookInput_Edit(t *testing.T) {
	t.Parallel()
	input := `{
		"session_id": "abc-123",
		"tool_name": "Edit",
		"tool_use_id": "tu-2",
		"tool_input": {"file_path": "/repo/cmd/main.go", "old_string": "line1\nline2\n", "new_string": "line1\nline2\nline3\nline4\n"},
		"tool_response": ""
	}`
	parsed, err := parseFileEditHookInput(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parseFileEditHookInput: %v", err)
	}
	if parsed.ToolName != "Edit" {
		t.Errorf("ToolName = %q, want %q", parsed.ToolName, "Edit")
	}
	if parsed.FilePath != "/repo/cmd/main.go" {
		t.Errorf("FilePath = %q, want %q", parsed.FilePath, "/repo/cmd/main.go")
	}
	if parsed.LinesAdded != 4 {
		t.Errorf("LinesAdded = %d, want 4", parsed.LinesAdded)
	}
	if parsed.LinesRemoved != 2 {
		t.Errorf("LinesRemoved = %d, want 2", parsed.LinesRemoved)
	}
}

func TestParseFileEditHookInput_EmptyInput(t *testing.T) {
	t.Parallel()
	_, err := parseFileEditHookInput(strings.NewReader(""))
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestParseFileEditHookInput_UnsupportedTool(t *testing.T) {
	t.Parallel()
	input := `{
		"session_id": "abc-123",
		"tool_name": "Bash",
		"tool_use_id": "tu-3",
		"tool_input": {},
		"tool_response": ""
	}`
	_, err := parseFileEditHookInput(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for unsupported tool")
	}
}
