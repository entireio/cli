package geminicli

import (
	"testing"
)

func TestParseTranscript(t *testing.T) {
	t.Parallel()

	// GeminiMessage uses "type" field with values "user" or "gemini"
	data := []byte(`{
  "messages": [
    {"type": "user", "content": "hello"},
    {"type": "gemini", "content": "hi there"}
  ]
}`)

	transcript, err := ParseTranscript(data)
	if err != nil {
		t.Fatalf("ParseTranscript() error = %v", err)
	}

	if len(transcript.Messages) != 2 {
		t.Errorf("ParseTranscript() got %d messages, want 2", len(transcript.Messages))
	}

	if transcript.Messages[0].Type != "user" {
		t.Errorf("First message type = %q, want user", transcript.Messages[0].Type)
	}
	if transcript.Messages[1].Type != "gemini" {
		t.Errorf("Second message type = %q, want gemini", transcript.Messages[1].Type)
	}
}

func TestParseTranscript_Empty(t *testing.T) {
	t.Parallel()

	data := []byte(`{"messages": []}`)
	transcript, err := ParseTranscript(data)
	if err != nil {
		t.Fatalf("ParseTranscript() error = %v", err)
	}

	if len(transcript.Messages) != 0 {
		t.Errorf("ParseTranscript() got %d messages, want 0", len(transcript.Messages))
	}
}

func TestParseTranscript_Invalid(t *testing.T) {
	t.Parallel()

	data := []byte(`not valid json`)
	_, err := ParseTranscript(data)
	if err == nil {
		t.Error("ParseTranscript() should error on invalid JSON")
	}
}

func TestExtractModifiedFiles(t *testing.T) {
	t.Parallel()

	// Gemini transcript with tool calls in ToolCalls array
	data := []byte(`{
  "messages": [
    {"type": "user", "content": "create a file"},
    {"type": "gemini", "content": "", "toolCalls": [{"name": "write_file", "args": {"file_path": "foo.go"}}]},
    {"type": "gemini", "content": "", "toolCalls": [{"name": "edit_file", "args": {"file_path": "bar.go"}}]},
    {"type": "gemini", "content": "", "toolCalls": [{"name": "read_file", "args": {"file_path": "other.go"}}]},
    {"type": "gemini", "content": "", "toolCalls": [{"name": "write_file", "args": {"file_path": "foo.go"}}]}
  ]
}`)

	files, err := ExtractModifiedFiles(data)
	if err != nil {
		t.Fatalf("ExtractModifiedFiles() error = %v", err)
	}

	// Should have foo.go and bar.go (deduplicated, read_file not included)
	if len(files) != 2 {
		t.Errorf("ExtractModifiedFiles() got %d files, want 2", len(files))
	}

	hasFile := func(name string) bool {
		for _, f := range files {
			if f == name {
				return true
			}
		}
		return false
	}

	if !hasFile("foo.go") {
		t.Error("ExtractModifiedFiles() missing foo.go")
	}
	if !hasFile("bar.go") {
		t.Error("ExtractModifiedFiles() missing bar.go")
	}
}

func TestExtractModifiedFiles_AlternativeFieldNames(t *testing.T) {
	t.Parallel()

	// Test different field names for file path
	data := []byte(`{
  "messages": [
    {"type": "gemini", "content": "", "toolCalls": [{"name": "write_file", "args": {"path": "via_path.go"}}]},
    {"type": "gemini", "content": "", "toolCalls": [{"name": "save_file", "args": {"filename": "via_filename.go"}}]}
  ]
}`)

	files, err := ExtractModifiedFiles(data)
	if err != nil {
		t.Fatalf("ExtractModifiedFiles() error = %v", err)
	}

	if len(files) != 2 {
		t.Errorf("ExtractModifiedFiles() got %d files, want 2", len(files))
	}

	hasFile := func(name string) bool {
		for _, f := range files {
			if f == name {
				return true
			}
		}
		return false
	}

	if !hasFile("via_path.go") {
		t.Error("ExtractModifiedFiles() missing via_path.go")
	}
	if !hasFile("via_filename.go") {
		t.Error("ExtractModifiedFiles() missing via_filename.go")
	}
}

func TestExtractModifiedFiles_NoToolUses(t *testing.T) {
	t.Parallel()

	data := []byte(`{
  "messages": [
    {"type": "user", "content": "hello"},
    {"type": "gemini", "content": "just text response"}
  ]
}`)

	files, err := ExtractModifiedFiles(data)
	if err != nil {
		t.Fatalf("ExtractModifiedFiles() error = %v", err)
	}

	if len(files) != 0 {
		t.Errorf("ExtractModifiedFiles() got %d files, want 0", len(files))
	}
}

func TestExtractLastUserPrompt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data string
		want string
	}{
		{
			name: "string content",
			data: `{"messages": [
				{"type": "user", "content": "first"},
				{"type": "gemini", "content": "response"},
				{"type": "user", "content": "second"}
			]}`,
			want: "second",
		},
		{
			name: "only one user message",
			data: `{"messages": [{"type": "user", "content": "only message"}]}`,
			want: "only message",
		},
		{
			name: "no user messages",
			data: `{"messages": [{"type": "gemini", "content": "assistant only"}]}`,
			want: "",
		},
		{
			name: "empty messages",
			data: `{"messages": []}`,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ExtractLastUserPrompt([]byte(tt.data))
			if err != nil {
				t.Fatalf("ExtractLastUserPrompt() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("ExtractLastUserPrompt() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractModifiedFilesFromTranscript(t *testing.T) {
	t.Parallel()

	transcript := &GeminiTranscript{
		Messages: []GeminiMessage{
			{Type: "user", Content: "hello"},
			{Type: "gemini", Content: "", ToolCalls: []GeminiToolCall{
				{Name: "write_file", Args: map[string]interface{}{"file_path": "test.go"}},
			}},
		},
	}

	files := ExtractModifiedFilesFromTranscript(transcript)

	if len(files) != 1 {
		t.Errorf("got %d files, want 1", len(files))
	}
	if len(files) > 0 && files[0] != "test.go" {
		t.Errorf("got file %q, want test.go", files[0])
	}
}

func TestExtractLastUserPromptFromTranscript(t *testing.T) {
	t.Parallel()

	transcript := &GeminiTranscript{
		Messages: []GeminiMessage{
			{Type: "user", Content: "first prompt"},
			{Type: "gemini", Content: "response"},
			{Type: "user", Content: "last prompt"},
		},
	}

	got := ExtractLastUserPromptFromTranscript(transcript)

	if got != "last prompt" {
		t.Errorf("got %q, want 'last prompt'", got)
	}
}
