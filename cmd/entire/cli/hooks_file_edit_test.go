package cli

import (
	"strings"
	"testing"
)

func TestParseFileEditHookInput_Write(t *testing.T) {
	t.Parallel()
	input := `{
		"session_id": "test-session",
		"tool_name": "Write",
		"tool_use_id": "toolu_123",
		"tool_input": {"file_path": "/tmp/test.go", "content": "line1\nline2\nline3\n"}
	}`
	parsed, err := parseFileEditHookInput(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parseFileEditHookInput() error = %v", err)
	}
	if parsed.ToolName != "Write" {
		t.Errorf("ToolName = %q, want %q", parsed.ToolName, "Write")
	}
	if parsed.FilePath != "/tmp/test.go" {
		t.Errorf("FilePath = %q, want %q", parsed.FilePath, "/tmp/test.go")
	}
	if parsed.LinesAdded != 3 {
		t.Errorf("LinesAdded = %d, want 3", parsed.LinesAdded)
	}
	if parsed.LinesRemoved != 0 {
		t.Errorf("LinesRemoved = %d, want 0", parsed.LinesRemoved)
	}
}

func TestParseFileEditHookInput_Edit(t *testing.T) {
	t.Parallel()
	input := `{
		"session_id": "test-session",
		"tool_name": "Edit",
		"tool_use_id": "toolu_456",
		"tool_input": {"file_path": "/tmp/test.go", "old_string": "old\n", "new_string": "new1\nnew2\n"}
	}`
	parsed, err := parseFileEditHookInput(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parseFileEditHookInput() error = %v", err)
	}
	if parsed.ToolName != "Edit" {
		t.Errorf("ToolName = %q, want %q", parsed.ToolName, "Edit")
	}
	if parsed.FilePath != "/tmp/test.go" {
		t.Errorf("FilePath = %q, want %q", parsed.FilePath, "/tmp/test.go")
	}
	if parsed.LinesAdded != 2 {
		t.Errorf("LinesAdded = %d, want 2", parsed.LinesAdded)
	}
	if parsed.LinesRemoved != 1 {
		t.Errorf("LinesRemoved = %d, want 1", parsed.LinesRemoved)
	}
}

func TestParseFileEditHookInput_EmptyInput(t *testing.T) {
	t.Parallel()
	_, err := parseFileEditHookInput(strings.NewReader(""))
	if err == nil {
		t.Error("expected error for empty input")
	}
}

func TestParseFileEditHookInput_UnsupportedTool(t *testing.T) {
	t.Parallel()
	input := `{
		"session_id": "test-session",
		"tool_name": "Bash",
		"tool_use_id": "toolu_789",
		"tool_input": {}
	}`
	_, err := parseFileEditHookInput(strings.NewReader(input))
	if err == nil {
		t.Error("expected error for unsupported tool")
	}
}

func TestParseFileEditHookInput_WriteEmptyContent(t *testing.T) {
	t.Parallel()
	input := `{
		"session_id": "test-session",
		"tool_name": "Write",
		"tool_use_id": "toolu_empty",
		"tool_input": {"file_path": "/tmp/empty.go", "content": ""}
	}`
	parsed, err := parseFileEditHookInput(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parseFileEditHookInput() error = %v", err)
	}
	if parsed.LinesAdded != 0 {
		t.Errorf("LinesAdded = %d, want 0 for empty content", parsed.LinesAdded)
	}
}

func TestParseFileEditHookInput_InvalidJSON(t *testing.T) {
	t.Parallel()
	_, err := parseFileEditHookInput(strings.NewReader("not json"))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}
