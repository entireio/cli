package transcript

import (
	"encoding/json"
	"testing"
)

func TestParseFromBytes_ValidJSONL(t *testing.T) {
	content := []byte(`{"type":"user","uuid":"u1","message":{"content":"hello"}}
{"type":"assistant","uuid":"a1","message":{"content":[{"type":"text","text":"hi"}]}}
`)

	lines, err := ParseFromBytes(content)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}

	if lines[0].Type != "user" || lines[0].UUID != "u1" {
		t.Errorf("unexpected first line: %+v", lines[0])
	}

	if lines[1].Type != "assistant" || lines[1].UUID != "a1" {
		t.Errorf("unexpected second line: %+v", lines[1])
	}
}

func TestParseFromBytes_EmptyContent(t *testing.T) {
	lines, err := ParseFromBytes([]byte{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(lines) != 0 {
		t.Errorf("expected 0 lines, got %d", len(lines))
	}
}

func TestParseFromBytes_MalformedLinesSkipped(t *testing.T) {
	content := []byte(`{"type":"user","uuid":"u1","message":{"content":"hello"}}
not valid json
{"type":"assistant","uuid":"a1","message":{"content":[{"type":"text","text":"hi"}]}}
`)

	lines, err := ParseFromBytes(content)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Malformed line should be skipped
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines (skipping malformed), got %d", len(lines))
	}
}

func TestParseFromBytes_NoTrailingNewline(t *testing.T) {
	content := []byte(`{"type":"user","uuid":"u1","message":{"content":"hello"}}`)

	lines, err := ParseFromBytes(content)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
}

func TestExtractUserContent_StringContent(t *testing.T) {
	msg := UserMessage{Content: "Hello, world!"}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	content := ExtractUserContent(raw)

	if content != "Hello, world!" {
		t.Errorf("expected 'Hello, world!', got '%s'", content)
	}
}

func TestExtractUserContent_ArrayContent(t *testing.T) {
	// Array with text blocks
	raw := []byte(`{"content":[{"type":"text","text":"First part"},{"type":"text","text":"Second part"}]}`)

	content := ExtractUserContent(raw)

	expected := "First part\n\nSecond part"
	if content != expected {
		t.Errorf("expected '%s', got '%s'", expected, content)
	}
}

func TestExtractUserContent_EmptyMessage(t *testing.T) {
	content := ExtractUserContent([]byte(`{}`))

	if content != "" {
		t.Errorf("expected empty string, got '%s'", content)
	}
}

func TestExtractUserContent_InvalidJSON(t *testing.T) {
	content := ExtractUserContent([]byte(`not valid json`))

	if content != "" {
		t.Errorf("expected empty string for invalid JSON, got '%s'", content)
	}
}

func TestExtractUserContent_StripsIDETags(t *testing.T) {
	msg := UserMessage{Content: "<ide_opened_file>file.go</ide_opened_file>Hello, world!"}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	content := ExtractUserContent(raw)

	if content != "Hello, world!" {
		t.Errorf("expected IDE tags stripped, got '%s'", content)
	}
}

func TestExtractUserContent_ToolResultsIgnored(t *testing.T) {
	// Tool result content should return empty (not a user prompt)
	raw := []byte(`{"content":[{"type":"tool_result","tool_use_id":"123","content":"result"}]}`)

	content := ExtractUserContent(raw)

	if content != "" {
		t.Errorf("expected empty string for tool results, got '%s'", content)
	}
}

func TestSliceFromLine_SkipsFirstNLines(t *testing.T) {
	// 5 JSONL lines
	content := []byte(`{"type":"user","uuid":"u1","message":{"content":"prompt 1"}}
{"type":"assistant","uuid":"a1","message":{"content":[{"type":"text","text":"response 1"}]}}
{"type":"user","uuid":"u2","message":{"content":"prompt 2"}}
{"type":"assistant","uuid":"a2","message":{"content":[{"type":"text","text":"response 2"}]}}
{"type":"user","uuid":"u3","message":{"content":"prompt 3"}}
`)

	// Skip first 2 lines, should get lines 3-5
	sliced := SliceFromLine(content, 2)

	lines, err := ParseFromBytes(sliced)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(lines) != 3 {
		t.Fatalf("expected 3 lines after skipping 2, got %d", len(lines))
	}

	if lines[0].UUID != "u2" {
		t.Errorf("expected first line to be u2, got %s", lines[0].UUID)
	}
	if lines[1].UUID != "a2" {
		t.Errorf("expected second line to be a2, got %s", lines[1].UUID)
	}
	if lines[2].UUID != "u3" {
		t.Errorf("expected third line to be u3, got %s", lines[2].UUID)
	}
}

func TestSliceFromLine_ZeroReturnsAll(t *testing.T) {
	content := []byte(`{"type":"user","uuid":"u1","message":{"content":"prompt 1"}}
{"type":"user","uuid":"u2","message":{"content":"prompt 2"}}
`)

	sliced := SliceFromLine(content, 0)

	lines, err := ParseFromBytes(sliced)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
}

func TestSliceFromLine_SkipMoreThanExists(t *testing.T) {
	content := []byte(`{"type":"user","uuid":"u1","message":{"content":"prompt 1"}}
`)

	// Skip more lines than exist
	sliced := SliceFromLine(content, 10)

	if len(sliced) != 0 {
		t.Errorf("expected empty slice when skipping more lines than exist, got %d bytes", len(sliced))
	}
}

func TestSliceFromLine_EmptyContent(t *testing.T) {
	sliced := SliceFromLine([]byte{}, 5)

	if len(sliced) != 0 {
		t.Errorf("expected empty slice for empty content, got %d bytes", len(sliced))
	}
}

func TestSliceFromLine_NoTrailingNewline(t *testing.T) {
	// No trailing newline
	content := []byte(`{"type":"user","uuid":"u1","message":{"content":"prompt 1"}}
{"type":"user","uuid":"u2","message":{"content":"prompt 2"}}`)

	sliced := SliceFromLine(content, 1)

	lines, err := ParseFromBytes(sliced)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(lines) != 1 {
		t.Fatalf("expected 1 line after skipping 1, got %d", len(lines))
	}

	if lines[0].UUID != "u2" {
		t.Errorf("expected line to be u2, got %s", lines[0].UUID)
	}
}

func TestParseFromBytes_PiMessageFormat(t *testing.T) {
	t.Parallel()

	content := []byte(`{"type":"message","id":"u1","message":{"role":"user","content":"hello from pi"}}
{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"text","text":"I can help"}]}}
`)

	lines, err := ParseFromBytes(content)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}

	if lines[0].Type != TypeUser || lines[0].UUID != "u1" {
		t.Fatalf("unexpected user line: %+v", lines[0])
	}

	if got := ExtractUserContent(lines[0].Message); got != "hello from pi" {
		t.Fatalf("expected extracted user prompt, got %q", got)
	}

	if lines[1].Type != TypeAssistant || lines[1].UUID != "a1" {
		t.Fatalf("unexpected assistant line: %+v", lines[1])
	}

	var assistant AssistantMessage
	if err := json.Unmarshal(lines[1].Message, &assistant); err != nil {
		t.Fatalf("failed to decode assistant message: %v", err)
	}
	if len(assistant.Content) != 1 || assistant.Content[0].Type != ContentTypeText {
		t.Fatalf("unexpected assistant content: %+v", assistant.Content)
	}
}

func TestParseFromBytes_PiToolCallNormalizesPathToFilePath(t *testing.T) {
	t.Parallel()

	content := []byte(`{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"toolCall","name":"write","arguments":{"path":"cmd/main.go"}}]}}
`)

	lines, err := ParseFromBytes(content)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}

	var assistant AssistantMessage
	if err := json.Unmarshal(lines[0].Message, &assistant); err != nil {
		t.Fatalf("failed to decode assistant message: %v", err)
	}
	if len(assistant.Content) != 1 {
		t.Fatalf("expected 1 assistant block, got %d", len(assistant.Content))
	}

	block := assistant.Content[0]
	if block.Type != ContentTypeToolUse {
		t.Fatalf("expected tool_use block, got %q", block.Type)
	}
	if block.Name != "write" {
		t.Fatalf("expected tool name write, got %q", block.Name)
	}

	var input ToolInput
	if err := json.Unmarshal(block.Input, &input); err != nil {
		t.Fatalf("failed to decode tool input: %v", err)
	}
	if input.FilePath != "cmd/main.go" {
		t.Fatalf("expected file_path cmd/main.go, got %q", input.FilePath)
	}
}
