package geminicli

import (
	"os"
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

	// Test different field names for file path (path, filename)
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

func TestExtractModifiedFiles_ReplaceTool(t *testing.T) {
	t.Parallel()

	// Test the "replace" tool which is used by Gemini CLI for file edits
	data := []byte(`{
  "messages": [
    {"type": "user", "content": "make the output uppercase"},
    {"type": "gemini", "content": "", "toolCalls": [{"name": "read_file", "args": {"file_path": "random_letter.rb"}}]},
    {"type": "gemini", "content": "", "toolCalls": [{"name": "replace", "args": {"file_path": "/path/to/random_letter.rb", "old_string": "sample", "new_string": "sample.upcase"}}]},
    {"type": "gemini", "content": "Done!"}
  ]
}`)

	files, err := ExtractModifiedFiles(data)
	if err != nil {
		t.Fatalf("ExtractModifiedFiles() error = %v", err)
	}

	// Should have random_letter.rb (read_file not included)
	if len(files) != 1 {
		t.Errorf("ExtractModifiedFiles() got %d files, want 1", len(files))
	}

	if len(files) > 0 && files[0] != "/path/to/random_letter.rb" {
		t.Errorf("ExtractModifiedFiles() got file %q, want /path/to/random_letter.rb", files[0])
	}
}

func TestParseTranscript_ArrayContent(t *testing.T) {
	t.Parallel()

	// Real Gemini CLI format: user messages have array content, gemini messages have string content
	data := []byte(`{
  "messages": [
    {"type": "user", "content": [{"text": "hello world"}]},
    {"type": "gemini", "content": "hi there"},
    {"type": "user", "content": [{"text": "do something"}]},
    {"type": "gemini", "content": "sure thing"}
  ]
}`)

	transcript, err := ParseTranscript(data)
	if err != nil {
		t.Fatalf("ParseTranscript() error = %v", err)
	}

	if len(transcript.Messages) != 4 {
		t.Fatalf("ParseTranscript() got %d messages, want 4", len(transcript.Messages))
	}

	// User messages should have extracted text from array
	if transcript.Messages[0].Content != "hello world" {
		t.Errorf("Message 0 content = %q, want %q", transcript.Messages[0].Content, "hello world")
	}
	if transcript.Messages[2].Content != "do something" {
		t.Errorf("Message 2 content = %q, want %q", transcript.Messages[2].Content, "do something")
	}

	// Gemini messages should have string content as-is
	if transcript.Messages[1].Content != "hi there" {
		t.Errorf("Message 1 content = %q, want %q", transcript.Messages[1].Content, "hi there")
	}
	if transcript.Messages[3].Content != "sure thing" {
		t.Errorf("Message 3 content = %q, want %q", transcript.Messages[3].Content, "sure thing")
	}
}

func TestParseTranscript_ArrayContentMultipleParts(t *testing.T) {
	t.Parallel()

	// Array content with multiple text parts should be joined with newlines
	data := []byte(`{
  "messages": [
    {"type": "user", "content": [{"text": "part one"}, {"text": "part two"}]}
  ]
}`)

	transcript, err := ParseTranscript(data)
	if err != nil {
		t.Fatalf("ParseTranscript() error = %v", err)
	}

	if len(transcript.Messages) != 1 {
		t.Fatalf("ParseTranscript() got %d messages, want 1", len(transcript.Messages))
	}

	want := "part one\npart two"
	if transcript.Messages[0].Content != want {
		t.Errorf("Content = %q, want %q", transcript.Messages[0].Content, want)
	}
}

func TestParseTranscript_NullContent(t *testing.T) {
	t.Parallel()

	data := []byte(`{
  "messages": [
    {"type": "user", "content": null},
    {"type": "gemini", "content": "response"}
  ]
}`)

	transcript, err := ParseTranscript(data)
	if err != nil {
		t.Fatalf("ParseTranscript() error = %v", err)
	}

	if transcript.Messages[0].Content != "" {
		t.Errorf("Content = %q, want empty string", transcript.Messages[0].Content)
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
			name: "array content",
			data: `{"messages": [
				{"type": "user", "content": [{"text": "first prompt"}]},
				{"type": "gemini", "content": "response"},
				{"type": "user", "content": [{"text": "second prompt"}]}
			]}`,
			want: "second prompt",
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

func TestGetLastMessageID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		data    string
		want    string
		wantErr bool
	}{
		{
			name: "transcript with message IDs",
			data: `{"messages": [
				{"id": "msg-1", "type": "user", "content": "hello"},
				{"id": "msg-2", "type": "gemini", "content": "hi there"}
			]}`,
			want:    "msg-2",
			wantErr: false,
		},
		{
			name:    "empty transcript",
			data:    `{"messages": []}`,
			want:    "",
			wantErr: false,
		},
		{
			name: "message without ID (empty ID field)",
			data: `{"messages": [
				{"type": "user", "content": "hello"},
				{"type": "gemini", "content": "hi"}
			]}`,
			want:    "",
			wantErr: false,
		},
		{
			name: "mixed - some with IDs, some without",
			data: `{"messages": [
				{"id": "msg-1", "type": "user", "content": "hello"},
				{"type": "gemini", "content": "hi"}
			]}`,
			want:    "",
			wantErr: false,
		},
		{
			name:    "invalid JSON",
			data:    `not valid json`,
			want:    "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := GetLastMessageID([]byte(tt.data))
			if (err != nil) != tt.wantErr {
				t.Errorf("GetLastMessageID() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("GetLastMessageID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGetLastMessageIDFromTranscript(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		transcript *GeminiTranscript
		want       string
	}{
		{
			name: "transcript with message IDs",
			transcript: &GeminiTranscript{
				Messages: []GeminiMessage{
					{ID: "msg-1", Type: "user", Content: "hello"},
					{ID: "msg-2", Type: "gemini", Content: "hi there"},
				},
			},
			want: "msg-2",
		},
		{
			name: "empty transcript",
			transcript: &GeminiTranscript{
				Messages: []GeminiMessage{},
			},
			want: "",
		},
		{
			name: "message without ID",
			transcript: &GeminiTranscript{
				Messages: []GeminiMessage{
					{Type: "user", Content: "hello"},
				},
			},
			want: "",
		},
		{
			name:       "nil messages",
			transcript: &GeminiTranscript{},
			want:       "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := GetLastMessageIDFromTranscript(tt.transcript)
			if got != tt.want {
				t.Errorf("GetLastMessageIDFromTranscript() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGetLastMessageIDFromFile(t *testing.T) {
	t.Parallel()

	t.Run("empty path", func(t *testing.T) {
		t.Parallel()
		got, err := GetLastMessageIDFromFile("")
		if err != nil {
			t.Errorf("GetLastMessageIDFromFile() error = %v", err)
		}
		if got != "" {
			t.Errorf("GetLastMessageIDFromFile() = %q, want empty string", got)
		}
	})

	t.Run("non-existent file", func(t *testing.T) {
		t.Parallel()
		got, err := GetLastMessageIDFromFile("/nonexistent/path/transcript.json")
		if err != nil {
			t.Errorf("GetLastMessageIDFromFile() error = %v, want nil for non-existent file", err)
		}
		if got != "" {
			t.Errorf("GetLastMessageIDFromFile() = %q, want empty string", got)
		}
	})

	t.Run("empty file", func(t *testing.T) {
		t.Parallel()
		tmpFile := t.TempDir() + "/empty.json"
		if err := os.WriteFile(tmpFile, []byte(""), 0o644); err != nil {
			t.Fatalf("failed to create test file: %v", err)
		}
		got, err := GetLastMessageIDFromFile(tmpFile)
		if err != nil {
			t.Errorf("GetLastMessageIDFromFile() error = %v", err)
		}
		if got != "" {
			t.Errorf("GetLastMessageIDFromFile() = %q, want empty string", got)
		}
	})

	t.Run("valid file with message IDs", func(t *testing.T) {
		t.Parallel()
		tmpFile := t.TempDir() + "/transcript.json"
		content := `{"messages": [{"id": "abc-123", "type": "user", "content": "hello"}]}`
		if err := os.WriteFile(tmpFile, []byte(content), 0o644); err != nil {
			t.Fatalf("failed to create test file: %v", err)
		}
		got, err := GetLastMessageIDFromFile(tmpFile)
		if err != nil {
			t.Errorf("GetLastMessageIDFromFile() error = %v", err)
		}
		if got != "abc-123" {
			t.Errorf("GetLastMessageIDFromFile() = %q, want 'abc-123'", got)
		}
	})

	t.Run("invalid JSON file", func(t *testing.T) {
		t.Parallel()
		tmpFile := t.TempDir() + "/invalid.json"
		if err := os.WriteFile(tmpFile, []byte("not valid json"), 0o644); err != nil {
			t.Fatalf("failed to create test file: %v", err)
		}
		_, err := GetLastMessageIDFromFile(tmpFile)
		if err == nil {
			t.Error("GetLastMessageIDFromFile() expected error for invalid JSON")
		}
	})
}

func TestExtractAllUserPrompts_ArrayContent(t *testing.T) {
	t.Parallel()

	// Real Gemini format with array content for user messages
	data := []byte(`{
  "messages": [
    {"type": "user", "content": [{"text": "first prompt"}]},
    {"type": "gemini", "content": "response 1"},
    {"type": "user", "content": [{"text": "second prompt"}]},
    {"type": "gemini", "content": "response 2"}
  ]
}`)

	prompts, err := ExtractAllUserPrompts(data)
	if err != nil {
		t.Fatalf("ExtractAllUserPrompts() error = %v", err)
	}

	if len(prompts) != 2 {
		t.Fatalf("ExtractAllUserPrompts() got %d prompts, want 2", len(prompts))
	}

	if prompts[0] != "first prompt" {
		t.Errorf("prompts[0] = %q, want %q", prompts[0], "first prompt")
	}
	if prompts[1] != "second prompt" {
		t.Errorf("prompts[1] = %q, want %q", prompts[1], "second prompt")
	}
}

func TestExtractModifiedFiles_ArrayContent(t *testing.T) {
	t.Parallel()

	// Real Gemini transcript format: user messages have array content
	data := []byte(`{
  "messages": [
    {"type": "user", "content": [{"text": "create a file"}]},
    {"type": "gemini", "content": "", "toolCalls": [{"name": "write_file", "args": {"file_path": "foo.go"}}]},
    {"type": "user", "content": [{"text": "edit the file"}]},
    {"type": "gemini", "content": "", "toolCalls": [{"name": "edit_file", "args": {"file_path": "bar.go"}}]}
  ]
}`)

	files, err := ExtractModifiedFiles(data)
	if err != nil {
		t.Fatalf("ExtractModifiedFiles() error = %v", err)
	}

	if len(files) != 2 {
		t.Errorf("ExtractModifiedFiles() got %d files, want 2", len(files))
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

func TestCalculateTokenUsage_BasicMessages(t *testing.T) {
	t.Parallel()

	// Gemini transcript with token usage in messages
	data := []byte(`{
  "messages": [
    {"id": "1", "type": "user", "content": "hello"},
    {"id": "2", "type": "gemini", "content": "hi there", "tokens": {"input": 10, "output": 20, "cached": 5, "thoughts": 0, "tool": 0, "total": 35}},
    {"id": "3", "type": "user", "content": "how are you?"},
    {"id": "4", "type": "gemini", "content": "I'm doing well", "tokens": {"input": 15, "output": 25, "cached": 3, "thoughts": 0, "tool": 0, "total": 43}}
  ]
}`)

	usage := CalculateTokenUsage(data, 0)

	// Should have 2 API calls (2 gemini messages)
	if usage.APICallCount != 2 {
		t.Errorf("APICallCount = %d, want 2", usage.APICallCount)
	}

	// Input tokens: 10 + 15 = 25
	if usage.InputTokens != 25 {
		t.Errorf("InputTokens = %d, want 25", usage.InputTokens)
	}

	// Output tokens: 20 + 25 = 45
	if usage.OutputTokens != 45 {
		t.Errorf("OutputTokens = %d, want 45", usage.OutputTokens)
	}

	// Cache read tokens: 5 + 3 = 8
	if usage.CacheReadTokens != 8 {
		t.Errorf("CacheReadTokens = %d, want 8", usage.CacheReadTokens)
	}
}

func TestCalculateTokenUsage_StartIndex(t *testing.T) {
	t.Parallel()

	// Gemini transcript with 4 messages - test starting from index 2
	data := []byte(`{
  "messages": [
    {"id": "1", "type": "user", "content": "hello"},
    {"id": "2", "type": "gemini", "content": "hi", "tokens": {"input": 10, "output": 20, "cached": 0, "total": 30}},
    {"id": "3", "type": "user", "content": "how are you?"},
    {"id": "4", "type": "gemini", "content": "great", "tokens": {"input": 15, "output": 25, "cached": 5, "total": 45}}
  ]
}`)

	// Start from index 2 - should only count the last gemini message
	usage := CalculateTokenUsage(data, 2)

	// Should have 1 API call (only the gemini message at index 3)
	if usage.APICallCount != 1 {
		t.Errorf("APICallCount = %d, want 1", usage.APICallCount)
	}

	// Only tokens from message at index 3
	if usage.InputTokens != 15 {
		t.Errorf("InputTokens = %d, want 15", usage.InputTokens)
	}

	if usage.OutputTokens != 25 {
		t.Errorf("OutputTokens = %d, want 25", usage.OutputTokens)
	}

	if usage.CacheReadTokens != 5 {
		t.Errorf("CacheReadTokens = %d, want 5", usage.CacheReadTokens)
	}
}

func TestCalculateTokenUsage_IgnoresUserMessages(t *testing.T) {
	t.Parallel()

	// Even if user messages have tokens (they shouldn't), they should be ignored
	data := []byte(`{
  "messages": [
    {"id": "1", "type": "user", "content": "hello", "tokens": {"input": 100, "output": 100, "cached": 100, "total": 300}},
    {"id": "2", "type": "gemini", "content": "hi", "tokens": {"input": 10, "output": 20, "cached": 5, "total": 35}}
  ]
}`)

	usage := CalculateTokenUsage(data, 0)

	// Should only count gemini message tokens
	if usage.APICallCount != 1 {
		t.Errorf("APICallCount = %d, want 1", usage.APICallCount)
	}

	if usage.InputTokens != 10 {
		t.Errorf("InputTokens = %d, want 10", usage.InputTokens)
	}

	if usage.OutputTokens != 20 {
		t.Errorf("OutputTokens = %d, want 20", usage.OutputTokens)
	}
}

func TestCalculateTokenUsage_EmptyTranscript(t *testing.T) {
	t.Parallel()

	data := []byte(`{"messages": []}`)
	usage := CalculateTokenUsage(data, 0)

	if usage.APICallCount != 0 {
		t.Errorf("APICallCount = %d, want 0", usage.APICallCount)
	}
	if usage.InputTokens != 0 {
		t.Errorf("InputTokens = %d, want 0", usage.InputTokens)
	}
	if usage.OutputTokens != 0 {
		t.Errorf("OutputTokens = %d, want 0", usage.OutputTokens)
	}
	if usage.CacheReadTokens != 0 {
		t.Errorf("CacheReadTokens = %d, want 0", usage.CacheReadTokens)
	}
}

func TestCalculateTokenUsage_InvalidJSON(t *testing.T) {
	t.Parallel()

	data := []byte(`not valid json`)
	usage := CalculateTokenUsage(data, 0)

	// Should return empty usage on parse error
	if usage.APICallCount != 0 {
		t.Errorf("APICallCount = %d, want 0", usage.APICallCount)
	}
}

func TestCalculateTokenUsage_MissingTokensField(t *testing.T) {
	t.Parallel()

	// Gemini message without tokens field
	data := []byte(`{
  "messages": [
    {"id": "1", "type": "user", "content": "hello"},
    {"id": "2", "type": "gemini", "content": "hi there"}
  ]
}`)

	usage := CalculateTokenUsage(data, 0)

	// No tokens to count
	if usage.APICallCount != 0 {
		t.Errorf("APICallCount = %d, want 0", usage.APICallCount)
	}
}

func TestGeminiCLIAgent_GetTranscriptPosition(t *testing.T) {
	t.Parallel()

	// Create a temp file with transcript data
	tmpFile := t.TempDir() + "/transcript.json"

	data := []byte(`{
  "messages": [
    {"type": "user", "content": "hello"},
    {"type": "gemini", "content": "hi"},
    {"type": "user", "content": "bye"}
  ]
}`)

	if err := writeTestFile(t, tmpFile, data); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	agent := &GeminiCLIAgent{}
	messageCount, err := agent.GetTranscriptPosition(tmpFile)
	if err != nil {
		t.Fatalf("GetTranscriptPosition() error = %v", err)
	}

	if messageCount != 3 {
		t.Errorf("GetTranscriptPosition() = %d, want 3", messageCount)
	}
}

func TestGeminiCLIAgent_GetTranscriptPosition_EmptyPath(t *testing.T) {
	t.Parallel()

	agent := &GeminiCLIAgent{}
	messageCount, err := agent.GetTranscriptPosition("")
	if err != nil {
		t.Fatalf("GetTranscriptPosition() error = %v", err)
	}

	if messageCount != 0 {
		t.Errorf("GetTranscriptPosition() = %d, want 0", messageCount)
	}
}

func TestGeminiCLIAgent_GetTranscriptPosition_NonexistentFile(t *testing.T) {
	t.Parallel()

	agent := &GeminiCLIAgent{}
	messageCount, err := agent.GetTranscriptPosition("/nonexistent/file.json")
	if err != nil {
		t.Fatalf("GetTranscriptPosition() error = %v", err)
	}

	// Should return 0 for nonexistent file
	if messageCount != 0 {
		t.Errorf("GetTranscriptPosition() = %d, want 0", messageCount)
	}
}

// writeTestFile is a helper to write test data to a file
func writeTestFile(t *testing.T, path string, data []byte) error {
	t.Helper()
	return os.WriteFile(path, data, 0o644)
}
