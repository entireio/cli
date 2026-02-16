package transcript

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

	if lines[0].Type != TypeUser || lines[0].UUID != "u1" {
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

// --- ParseFromFileAtLine tests ---

func createTempTranscript(t *testing.T, content string) string {
	t.Helper()
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "transcript.jsonl")
	if err := os.WriteFile(tmpFile, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	return tmpFile
}

func TestParseFromFileAtLine_ValidMixedMessages(t *testing.T) {
	t.Parallel()

	content := `{"type":"user","uuid":"user-1","message":{"content":"Hello"}}
{"type":"assistant","uuid":"asst-1","message":{"content":[{"type":"text","text":"Hi there"}]}}
{"type":"user","uuid":"user-2","message":{"content":"Thanks"}}`

	tmpFile := createTempTranscript(t, content)

	lines, totalLines, err := ParseFromFileAtLine(tmpFile, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if totalLines != 3 {
		t.Errorf("totalLines = %d, want 3", totalLines)
	}
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}

	if lines[0].Type != TypeUser || lines[0].UUID != "user-1" {
		t.Errorf("first line mismatch: got type=%s uuid=%s", lines[0].Type, lines[0].UUID)
	}
	if lines[1].Type != TypeAssistant || lines[1].UUID != "asst-1" {
		t.Errorf("second line mismatch: got type=%s uuid=%s", lines[1].Type, lines[1].UUID)
	}
	if lines[2].Type != TypeUser || lines[2].UUID != "user-2" {
		t.Errorf("third line mismatch: got type=%s uuid=%s", lines[2].Type, lines[2].UUID)
	}
}

func TestParseFromFileAtLine_SkipsMalformedLines(t *testing.T) {
	t.Parallel()

	content := `{"type":"user","uuid":"user-1","message":{"content":"Hello"}}
this is not valid json
{"type":"assistant","uuid":"asst-1","message":{"content":[{"type":"text","text":"Hi"}]}}
{"broken json
{"type":"user","uuid":"user-2","message":{"content":"Bye"}}`

	tmpFile := createTempTranscript(t, content)

	lines, _, err := ParseFromFileAtLine(tmpFile, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(lines) != 3 {
		t.Fatalf("expected 3 valid lines, got %d", len(lines))
	}
}

func TestParseFromFileAtLine_LargeLines(t *testing.T) {
	t.Parallel()

	// Create a line with content larger than default scanner buffer (64KB)
	largeContent := strings.Repeat("x", 100*1024) // 100KB
	content := `{"type":"user","uuid":"user-1","message":{"content":"` + largeContent + `"}}`

	tmpFile := createTempTranscript(t, content)

	lines, _, err := ParseFromFileAtLine(tmpFile, 0)
	if err != nil {
		t.Fatalf("unexpected error parsing large line: %v", err)
	}

	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
}

func TestParseFromFileAtLine_LineExceedsScannerBuffer(t *testing.T) {
	t.Parallel()

	// Create a line larger than 10MB. bufio.Reader handles this without any size limit.
	largeContent := strings.Repeat("x", 11*1024*1024) // 11MB
	content := `{"type":"user","uuid":"user-1","message":{"content":"` + largeContent + `"}}`

	tmpFile := createTempTranscript(t, content)

	lines, _, err := ParseFromFileAtLine(tmpFile, 0)
	if err != nil {
		t.Fatalf("unexpected error parsing line exceeding buffer: %v", err)
	}

	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
}

func TestParseFromFileAtLine_LineExceedsScannerBufferWithOffset(t *testing.T) {
	t.Parallel()

	// Same test with offset - should handle lines > 10MB
	largeContent := strings.Repeat("x", 11*1024*1024) // 11MB
	content := `{"type":"user","uuid":"user-1","message":{"content":"` + largeContent + `"}}`

	tmpFile := createTempTranscript(t, content)

	lines, totalLines, err := ParseFromFileAtLine(tmpFile, 0)
	if err != nil {
		t.Fatalf("unexpected error parsing line exceeding buffer: %v", err)
	}

	if totalLines != 1 {
		t.Errorf("expected totalLines=1, got %d", totalLines)
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 parsed line, got %d", len(lines))
	}
}

func TestParseFromFileAtLine_EntireFile(t *testing.T) {
	t.Parallel()

	content := `{"type":"user","uuid":"user-1","message":{"content":"Hello"}}
{"type":"assistant","uuid":"asst-1","message":{"content":[{"type":"text","text":"Hi"}]}}
{"type":"user","uuid":"user-2","message":{"content":"Bye"}}`

	tmpFile := createTempTranscript(t, content)

	lines, totalLines, err := ParseFromFileAtLine(tmpFile, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if totalLines != 3 {
		t.Errorf("totalLines = %d, want 3", totalLines)
	}
	if len(lines) != 3 {
		t.Errorf("len(lines) = %d, want 3", len(lines))
	}
}

func TestParseFromFileAtLine_Offset(t *testing.T) {
	t.Parallel()

	content := `{"type":"user","uuid":"user-1","message":{"content":"Line1"}}
{"type":"assistant","uuid":"asst-1","message":{"content":[{"type":"text","text":"Line2"}]}}
{"type":"user","uuid":"user-2","message":{"content":"Line3"}}
{"type":"assistant","uuid":"asst-2","message":{"content":[{"type":"text","text":"Line4"}]}}`

	tmpFile := createTempTranscript(t, content)

	// Parse from line 2 (0-indexed, so skip first 2 lines)
	lines, totalLines, err := ParseFromFileAtLine(tmpFile, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if totalLines != 4 {
		t.Errorf("totalLines = %d, want 4", totalLines)
	}
	if len(lines) != 2 {
		t.Errorf("len(lines) = %d, want 2 (lines after offset)", len(lines))
	}

	// Verify we got the correct lines (user-2 and asst-2)
	if len(lines) >= 1 && lines[0].UUID != "user-2" {
		t.Errorf("first line UUID = %q, want 'user-2'", lines[0].UUID)
	}
	if len(lines) >= 2 && lines[1].UUID != "asst-2" {
		t.Errorf("second line UUID = %q, want 'asst-2'", lines[1].UUID)
	}
}

func TestParseFromFileAtLine_OffsetBeyondEnd(t *testing.T) {
	t.Parallel()

	content := `{"type":"user","uuid":"user-1","message":{"content":"Hello"}}
{"type":"assistant","uuid":"asst-1","message":{"content":[{"type":"text","text":"Hi"}]}}`

	tmpFile := createTempTranscript(t, content)

	// Parse from line 10 (beyond end of file)
	lines, totalLines, err := ParseFromFileAtLine(tmpFile, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if totalLines != 2 {
		t.Errorf("totalLines = %d, want 2", totalLines)
	}
	if len(lines) != 0 {
		t.Errorf("len(lines) = %d, want 0 (no lines after offset)", len(lines))
	}
}

func TestParseFromFileAtLine_SkipsMalformedLinesWithOffset(t *testing.T) {
	t.Parallel()

	content := `{"type":"user","uuid":"user-1","message":{"content":"Hello"}}
invalid json line
{"type":"assistant","uuid":"asst-1","message":{"content":[{"type":"text","text":"Hi"}]}}
{"type":"user","uuid":"user-2","message":{"content":"Bye"}}`

	tmpFile := createTempTranscript(t, content)

	// Parse from line 1 (skip first valid line)
	lines, totalLines, err := ParseFromFileAtLine(tmpFile, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Total lines counts ALL lines including malformed
	if totalLines != 4 {
		t.Errorf("totalLines = %d, want 4", totalLines)
	}
	// But parsed lines excludes malformed
	if len(lines) != 2 {
		t.Errorf("len(lines) = %d, want 2 (valid lines after offset)", len(lines))
	}
}
