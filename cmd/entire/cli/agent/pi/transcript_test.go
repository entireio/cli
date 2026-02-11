package pi

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseTranscript_Empty(t *testing.T) {
	t.Parallel()

	entries, err := ParseTranscript([]byte{})
	if err != nil {
		t.Fatalf("ParseTranscript() error = %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("ParseTranscript() len = %d, want 0", len(entries))
	}
}

func TestParseTranscript_InvalidLineIgnored(t *testing.T) {
	t.Parallel()

	data := []byte("not-json\n")
	entries, err := ParseTranscript(data)
	if err != nil {
		t.Fatalf("ParseTranscript() error = %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("ParseTranscript() len = %d, want 0", len(entries))
	}
}

func TestExtractAllUserPrompts(t *testing.T) {
	t.Parallel()

	data := []byte(`{"type":"session","id":"s1"}
{"type":"message","id":"1","message":{"role":"user","content":"first prompt"}}
{"type":"message","id":"2","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]}}
{"type":"message","id":"3","message":{"role":"user","content":[{"type":"text","text":"second"},{"type":"text","text":"prompt"}]}}
`)

	prompts, err := ExtractAllUserPrompts(data)
	if err != nil {
		t.Fatalf("ExtractAllUserPrompts() error = %v", err)
	}

	want := []string{"first prompt", "second\n\nprompt"}
	if !reflect.DeepEqual(prompts, want) {
		t.Fatalf("ExtractAllUserPrompts() = %#v, want %#v", prompts, want)
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
			name: "multiple user prompts",
			data: `{"type":"message","id":"1","message":{"role":"user","content":"first"}}
{"type":"message","id":"2","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]}}
{"type":"message","id":"3","message":{"role":"user","content":[{"type":"text","text":"second"},{"type":"text","text":"prompt"}]}}`,
			want: "second\n\nprompt",
		},
		{
			name: "no user prompts",
			data: `{"type":"message","id":"1","message":{"role":"assistant","content":"hello"}}`,
			want: "",
		},
		{
			name: "single user prompt",
			data: `{"type":"message","id":"1","message":{"role":"user","content":"only"}}`,
			want: "only",
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
				t.Fatalf("ExtractLastUserPrompt() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractLastAssistantMessage(t *testing.T) {
	t.Parallel()

	data := []byte(`{"type":"message","id":"1","message":{"role":"assistant","content":[{"type":"toolCall","name":"write","arguments":{"path":"a.txt"}}]}}
{"type":"message","id":"2","message":{"role":"assistant","content":[{"type":"text","text":"final assistant response"}]}}
`)

	msg, err := ExtractLastAssistantMessage(data)
	if err != nil {
		t.Fatalf("ExtractLastAssistantMessage() error = %v", err)
	}
	if msg != "final assistant response" {
		t.Fatalf("ExtractLastAssistantMessage() = %q, want %q", msg, "final assistant response")
	}
}

func TestExtractModifiedFiles(t *testing.T) {
	t.Parallel()

	data := []byte(`{"type":"message","id":"1","message":{"role":"assistant","content":[{"type":"toolCall","name":"write","arguments":{"path":"a.txt"}},{"type":"toolCall","name":"edit","arguments":{"file_path":"b.txt"}}]}}
{"type":"message","id":"2","message":{"role":"toolResult","toolName":"write","details":{"path":"c.txt"}}}
{"type":"message","id":"3","message":{"role":"toolResult","toolName":"edit","details":{"filename":"d.txt"}}}
`)

	files, err := ExtractModifiedFiles(data)
	if err != nil {
		t.Fatalf("ExtractModifiedFiles() error = %v", err)
	}

	wantSet := map[string]bool{"a.txt": true, "b.txt": true, "c.txt": true, "d.txt": true}
	for _, file := range files {
		delete(wantSet, file)
	}
	if len(wantSet) != 0 {
		t.Fatalf("missing expected files: %#v (got %#v)", wantSet, files)
	}
}

func TestExtractModifiedFiles_NoToolUses(t *testing.T) {
	t.Parallel()

	data := []byte(`{"type":"message","id":"1","message":{"role":"assistant","content":[{"type":"text","text":"no tool call"}]}}
{"type":"message","id":"2","message":{"role":"user","content":"hello"}}`)

	files, err := ExtractModifiedFiles(data)
	if err != nil {
		t.Fatalf("ExtractModifiedFiles() error = %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("ExtractModifiedFiles() len = %d, want 0", len(files))
	}
}

func TestParseTranscript_MalformedLineTolerance(t *testing.T) {
	t.Parallel()

	data := []byte(`{"type":"message","id":"1","message":{"role":"user","content":"ok"}}
not-json-line
{"type":"message","id":"2","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}
`)

	entries, err := ParseTranscript(data)
	if err != nil {
		t.Fatalf("ParseTranscript() error = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("ParseTranscript() len = %d, want 2", len(entries))
	}
}

func TestCalculateTokenUsageFromTranscript(t *testing.T) {
	t.Parallel()

	data := []byte(`{"type":"message","id":"1","message":{"role":"assistant","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":2}}}
{"type":"message","id":"2","message":{"role":"assistant","tokens":{"inputTokens":"7","outputTokens":"3","cacheCreationTokens":1}}}
{"type":"message","id":"3","message":{"role":"user","content":"ignored"}}
`)

	usage := CalculateTokenUsageFromTranscript(data, 0)
	if usage.APICallCount != 2 {
		t.Fatalf("APICallCount = %d, want 2", usage.APICallCount)
	}
	if usage.InputTokens != 17 {
		t.Fatalf("InputTokens = %d, want 17", usage.InputTokens)
	}
	if usage.OutputTokens != 8 {
		t.Fatalf("OutputTokens = %d, want 8", usage.OutputTokens)
	}
	if usage.CacheReadTokens != 2 {
		t.Fatalf("CacheReadTokens = %d, want 2", usage.CacheReadTokens)
	}
	if usage.CacheCreationTokens != 1 {
		t.Fatalf("CacheCreationTokens = %d, want 1", usage.CacheCreationTokens)
	}
}

func TestCalculateTokenUsageFromTranscript_StartOffset(t *testing.T) {
	t.Parallel()

	data := []byte(`{"type":"message","id":"1","message":{"role":"assistant","usage":{"input_tokens":100,"output_tokens":50}}}
{"type":"message","id":"2","message":{"role":"user","content":"ignore"}}
{"type":"message","id":"3","message":{"role":"assistant","usage":{"input_tokens":5,"output_tokens":2}}}
`)

	usage := CalculateTokenUsageFromTranscript(data, 2)
	if usage.APICallCount != 1 {
		t.Fatalf("APICallCount = %d, want 1", usage.APICallCount)
	}
	if usage.InputTokens != 5 {
		t.Fatalf("InputTokens = %d, want 5", usage.InputTokens)
	}
	if usage.OutputTokens != 2 {
		t.Fatalf("OutputTokens = %d, want 2", usage.OutputTokens)
	}
}

func TestCalculateTokenUsageFromTranscript_EmptyTranscript(t *testing.T) {
	t.Parallel()

	usage := CalculateTokenUsageFromTranscript([]byte{}, 0)
	if usage.APICallCount != 0 || usage.InputTokens != 0 || usage.OutputTokens != 0 {
		t.Fatalf("usage = %+v, want all zeros", usage)
	}
}

func TestCalculateTokenUsageFromTranscript_InvalidJSON(t *testing.T) {
	t.Parallel()

	usage := CalculateTokenUsageFromTranscript([]byte("not-json\n"), 0)
	if usage.APICallCount != 0 || usage.InputTokens != 0 || usage.OutputTokens != 0 {
		t.Fatalf("usage = %+v, want all zeros", usage)
	}
}

func TestCalculateTokenUsageFromTranscript_MissingTokenFields(t *testing.T) {
	t.Parallel()

	data := []byte(`{"type":"message","id":"1","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}`)
	usage := CalculateTokenUsageFromTranscript(data, 0)

	if usage.APICallCount != 1 {
		t.Fatalf("APICallCount = %d, want 1", usage.APICallCount)
	}
	if usage.InputTokens != 0 || usage.OutputTokens != 0 {
		t.Fatalf("token counts should be zero when token fields are missing: %+v", usage)
	}
}

func TestCalculateTokenUsageFromFile_EmptyPath(t *testing.T) {
	t.Parallel()

	usage, err := CalculateTokenUsageFromFile("", 0)
	if err != nil {
		t.Fatalf("CalculateTokenUsageFromFile() error = %v", err)
	}
	if usage.APICallCount != 0 {
		t.Fatalf("APICallCount = %d, want 0", usage.APICallCount)
	}
}

func TestCalculateTokenUsageFromFile_NonexistentFile(t *testing.T) {
	t.Parallel()

	usage, err := CalculateTokenUsageFromFile(filepath.Join(t.TempDir(), "missing.jsonl"), 0)
	if err != nil {
		t.Fatalf("CalculateTokenUsageFromFile() error = %v", err)
	}
	if usage.APICallCount != 0 {
		t.Fatalf("APICallCount = %d, want 0", usage.APICallCount)
	}
}

func TestCalculateTokenUsageFromFile_InvalidPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, err := CalculateTokenUsageFromFile(dir, 0)
	if err == nil {
		t.Fatal("CalculateTokenUsageFromFile() expected error for directory path")
	}
}

func TestParseTranscriptFromLine_OffsetAndTotalLines(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	content := `{"type":"message","id":"1","message":{"role":"user","content":"one"}}
{"type":"message","id":"2","message":{"role":"assistant","content":[{"type":"text","text":"two"}]}}
{"type":"message","id":"3","message":{"role":"assistant","content":[{"type":"text","text":"three"}]}}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	entries, totalLines, err := ParseTranscriptFromLine(path, 1)
	if err != nil {
		t.Fatalf("ParseTranscriptFromLine() error = %v", err)
	}
	if totalLines != 3 {
		t.Fatalf("totalLines = %d, want 3", totalLines)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}
	if entries[0].EntryID() != "2" || entries[1].EntryID() != "3" {
		t.Fatalf("unexpected parsed IDs from offset: %#v", []string{entries[0].EntryID(), entries[1].EntryID()})
	}
}

func TestParseTranscriptFromLine_EmptyPath(t *testing.T) {
	t.Parallel()

	entries, totalLines, err := ParseTranscriptFromLine("", 0)
	if err != nil {
		t.Fatalf("ParseTranscriptFromLine() error = %v", err)
	}
	if totalLines != 0 {
		t.Fatalf("totalLines = %d, want 0", totalLines)
	}
	if len(entries) != 0 {
		t.Fatalf("len(entries) = %d, want 0", len(entries))
	}
}

func TestParseTranscriptFromLine_NonexistentFile(t *testing.T) {
	t.Parallel()

	entries, totalLines, err := ParseTranscriptFromLine(filepath.Join(t.TempDir(), "missing.jsonl"), 0)
	if err != nil {
		t.Fatalf("ParseTranscriptFromLine() error = %v", err)
	}
	if totalLines != 0 {
		t.Fatalf("totalLines = %d, want 0", totalLines)
	}
	if len(entries) != 0 {
		t.Fatalf("len(entries) = %d, want 0", len(entries))
	}
}

func TestFindCheckpointEntryID(t *testing.T) {
	t.Parallel()

	data := []byte(`{"type":"message","id":"1","message":{"role":"assistant","content":[{"type":"toolCall","name":"write","id":"tool-1","arguments":{"path":"a.txt"}}]}}
{"type":"message","id":"2","message":{"role":"toolResult","toolName":"write","toolCallId":"tool-1","details":{"path":"a.txt"}}}
`)

	id, ok := FindCheckpointEntryID(data, "tool-1")
	if !ok {
		t.Fatalf("FindCheckpointEntryID() ok = false, want true")
	}
	if id != "2" {
		t.Fatalf("FindCheckpointEntryID() id = %q, want %q", id, "2")
	}
}

func TestFindCheckpointEntryID_NotFound(t *testing.T) {
	t.Parallel()

	data := []byte(`{"type":"message","id":"1","message":{"role":"assistant","content":[{"type":"toolCall","name":"write","id":"tool-1","arguments":{"path":"a.txt"}}]}}`)

	_, ok := FindCheckpointEntryID(data, "missing-tool")
	if ok {
		t.Fatal("FindCheckpointEntryID() expected ok=false for missing tool id")
	}
}
