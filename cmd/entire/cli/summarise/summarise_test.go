package summarise

import (
	"encoding/json"
	"testing"
)

func TestBuildCondensedTranscript_UserPrompts(t *testing.T) {
	transcript := []TranscriptLine{
		{
			Type: "user",
			UUID: "user-1",
			Message: mustMarshal(t, userMessage{
				Content: "Hello, please help me with this task",
			}),
		},
	}

	entries := BuildCondensedTranscript(transcript)

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	if entries[0].Type != EntryTypeUser {
		t.Errorf("expected type %s, got %s", EntryTypeUser, entries[0].Type)
	}

	if entries[0].Content != "Hello, please help me with this task" {
		t.Errorf("unexpected content: %s", entries[0].Content)
	}
}

func TestBuildCondensedTranscript_AssistantResponses(t *testing.T) {
	transcript := []TranscriptLine{
		{
			Type: "assistant",
			UUID: "assistant-1",
			Message: mustMarshal(t, assistantMessage{
				Content: []contentBlock{
					{Type: "text", Text: "I'll help you with that."},
				},
			}),
		},
	}

	entries := BuildCondensedTranscript(transcript)

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	if entries[0].Type != EntryTypeAssistant {
		t.Errorf("expected type %s, got %s", EntryTypeAssistant, entries[0].Type)
	}

	if entries[0].Content != "I'll help you with that." {
		t.Errorf("unexpected content: %s", entries[0].Content)
	}
}

func TestBuildCondensedTranscript_ToolCalls(t *testing.T) {
	transcript := []TranscriptLine{
		{
			Type: "assistant",
			UUID: "assistant-1",
			Message: mustMarshal(t, assistantMessage{
				Content: []contentBlock{
					{
						Type: "tool_use",
						Name: "Read",
						Input: mustMarshal(t, toolInput{
							FilePath: "/path/to/file.go",
						}),
					},
				},
			}),
		},
	}

	entries := BuildCondensedTranscript(transcript)

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	if entries[0].Type != EntryTypeTool {
		t.Errorf("expected type %s, got %s", EntryTypeTool, entries[0].Type)
	}

	if entries[0].ToolName != "Read" {
		t.Errorf("expected tool name Read, got %s", entries[0].ToolName)
	}

	if entries[0].ToolDetail != "/path/to/file.go" {
		t.Errorf("expected tool detail /path/to/file.go, got %s", entries[0].ToolDetail)
	}
}

func TestBuildCondensedTranscript_ToolCallWithCommand(t *testing.T) {
	transcript := []TranscriptLine{
		{
			Type: "assistant",
			UUID: "assistant-1",
			Message: mustMarshal(t, assistantMessage{
				Content: []contentBlock{
					{
						Type: "tool_use",
						Name: "Bash",
						Input: mustMarshal(t, toolInput{
							Command: "go test ./...",
						}),
					},
				},
			}),
		},
	}

	entries := BuildCondensedTranscript(transcript)

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	if entries[0].ToolDetail != "go test ./..." {
		t.Errorf("expected tool detail 'go test ./...', got %s", entries[0].ToolDetail)
	}
}

func TestBuildCondensedTranscript_StripIDEContextTags(t *testing.T) {
	transcript := []TranscriptLine{
		{
			Type: "user",
			UUID: "user-1",
			Message: mustMarshal(t, userMessage{
				Content: "<ide_opened_file>some file content</ide_opened_file>Please review this code",
			}),
		},
	}

	entries := BuildCondensedTranscript(transcript)

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	if entries[0].Content != "Please review this code" {
		t.Errorf("expected IDE tags to be stripped, got: %s", entries[0].Content)
	}
}

func TestBuildCondensedTranscript_StripSystemTags(t *testing.T) {
	transcript := []TranscriptLine{
		{
			Type: "user",
			UUID: "user-1",
			Message: mustMarshal(t, userMessage{
				Content: "<system-reminder>internal instructions</system-reminder>User question here",
			}),
		},
	}

	entries := BuildCondensedTranscript(transcript)

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	if entries[0].Content != "User question here" {
		t.Errorf("expected system tags to be stripped, got: %s", entries[0].Content)
	}
}

func TestBuildCondensedTranscript_MixedContent(t *testing.T) {
	transcript := []TranscriptLine{
		{
			Type: "user",
			UUID: "user-1",
			Message: mustMarshal(t, userMessage{
				Content: "Create a new file",
			}),
		},
		{
			Type: "assistant",
			UUID: "assistant-1",
			Message: mustMarshal(t, assistantMessage{
				Content: []contentBlock{
					{Type: "text", Text: "I'll create that file for you."},
					{
						Type: "tool_use",
						Name: "Write",
						Input: mustMarshal(t, toolInput{
							FilePath: "/path/to/new.go",
						}),
					},
				},
			}),
		},
	}

	entries := BuildCondensedTranscript(transcript)

	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	if entries[0].Type != EntryTypeUser {
		t.Errorf("entry 0: expected type %s, got %s", EntryTypeUser, entries[0].Type)
	}

	if entries[1].Type != EntryTypeAssistant {
		t.Errorf("entry 1: expected type %s, got %s", EntryTypeAssistant, entries[1].Type)
	}

	if entries[2].Type != EntryTypeTool {
		t.Errorf("entry 2: expected type %s, got %s", EntryTypeTool, entries[2].Type)
	}
}

func TestBuildCondensedTranscript_EmptyTranscript(t *testing.T) {
	transcript := []TranscriptLine{}

	entries := BuildCondensedTranscript(transcript)

	if len(entries) != 0 {
		t.Errorf("expected 0 entries for empty transcript, got %d", len(entries))
	}
}

func TestBuildCondensedTranscript_UserArrayContent(t *testing.T) {
	// Test user message with array content (text blocks)
	transcript := []TranscriptLine{
		{
			Type: "user",
			UUID: "user-1",
			Message: mustMarshal(t, map[string]interface{}{
				"content": []interface{}{
					map[string]interface{}{
						"type": "text",
						"text": "First part",
					},
					map[string]interface{}{
						"type": "text",
						"text": "Second part",
					},
				},
			}),
		},
	}

	entries := BuildCondensedTranscript(transcript)

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	expected := "First part\n\nSecond part"
	if entries[0].Content != expected {
		t.Errorf("expected %q, got %q", expected, entries[0].Content)
	}
}

func TestBuildCondensedTranscript_SkipsEmptyContent(t *testing.T) {
	transcript := []TranscriptLine{
		{
			Type: "user",
			UUID: "user-1",
			Message: mustMarshal(t, userMessage{
				Content: "<ide_opened_file>only tags</ide_opened_file>",
			}),
		},
		{
			Type: "assistant",
			UUID: "assistant-1",
			Message: mustMarshal(t, assistantMessage{
				Content: []contentBlock{
					{Type: "text", Text: ""}, // Empty text
				},
			}),
		},
	}

	entries := BuildCondensedTranscript(transcript)

	if len(entries) != 0 {
		t.Errorf("expected 0 entries for empty content, got %d", len(entries))
	}
}

func TestFormatCondensedTranscript_BasicFormat(t *testing.T) {
	input := Input{
		Transcript: []Entry{
			{Type: EntryTypeUser, Content: "Hello"},
			{Type: EntryTypeAssistant, Content: "Hi there"},
			{Type: EntryTypeTool, ToolName: "Read", ToolDetail: "/file.go"},
		},
	}

	result := FormatCondensedTranscript(input)

	expected := `[User] Hello

[Assistant] Hi there

[Tool] Read: /file.go
`
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestFormatCondensedTranscript_WithFiles(t *testing.T) {
	input := Input{
		Transcript: []Entry{
			{Type: EntryTypeUser, Content: "Create files"},
		},
		FilesTouched: []string{"file1.go", "file2.go"},
	}

	result := FormatCondensedTranscript(input)

	expected := `[User] Create files

[Files Modified]
- file1.go
- file2.go
`
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestFormatCondensedTranscript_ToolWithoutDetail(t *testing.T) {
	input := Input{
		Transcript: []Entry{
			{Type: EntryTypeTool, ToolName: "TaskList"},
		},
	}

	result := FormatCondensedTranscript(input)

	expected := "[Tool] TaskList\n"
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestFormatCondensedTranscript_EmptyInput(t *testing.T) {
	input := Input{}

	result := FormatCondensedTranscript(input)

	if result != "" {
		t.Errorf("expected empty string for empty input, got: %s", result)
	}
}

// mustMarshal is a test helper that marshals v to JSON, failing the test on error.
func mustMarshal(t *testing.T, v interface{}) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}
	return data
}
