package opencode

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Compile-time checks
var (
	_ agent.TranscriptAnalyzer = (*OpenCodeAgent)(nil)
	_ agent.TokenCalculator    = (*OpenCodeAgent)(nil)
)

const testTranscriptJSON = `{
	"session_id": "test-session",
	"messages": [
		{
			"id": "msg-1",
			"role": "user",
			"content": "Fix the bug in main.go",
			"time": {"created": 1708300000}
		},
		{
			"id": "msg-2",
			"role": "assistant",
			"content": "I'll fix the bug.",
			"time": {"created": 1708300001, "completed": 1708300005},
			"tokens": {"input": 150, "output": 80, "reasoning": 10, "cache": {"read": 5, "write": 15}},
			"cost": 0.003,
			"parts": [
				{"type": "text", "text": "I'll fix the bug."},
				{"type": "tool", "tool": "edit_file", "callID": "call-1",
					"state": {"status": "completed", "input": {"file_path": "main.go"}, "output": "Applied edit"}}
			]
		},
		{
			"id": "msg-3",
			"role": "user",
			"content": "Also fix util.go",
			"time": {"created": 1708300010}
		},
		{
			"id": "msg-4",
			"role": "assistant",
			"content": "Done fixing util.go.",
			"time": {"created": 1708300011, "completed": 1708300015},
			"tokens": {"input": 200, "output": 100, "reasoning": 5, "cache": {"read": 10, "write": 20}},
			"cost": 0.005,
			"parts": [
				{"type": "tool", "tool": "write_file", "callID": "call-2",
					"state": {"status": "completed", "input": {"file_path": "util.go"}, "output": "File written"}},
				{"type": "text", "text": "Done fixing util.go."}
			]
		}
	]
}`

func writeTestTranscript(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test-session.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write test transcript: %v", err)
	}
	return path
}

func TestGetTranscriptPosition(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	path := writeTestTranscript(t, testTranscriptJSON)

	pos, err := ag.GetTranscriptPosition(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pos != 4 {
		t.Errorf("expected position 4 (4 messages), got %d", pos)
	}
}

func TestGetTranscriptPosition_NonexistentFile(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}

	pos, err := ag.GetTranscriptPosition("/nonexistent/path.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pos != 0 {
		t.Errorf("expected position 0 for nonexistent file, got %d", pos)
	}
}

func TestExtractModifiedFilesFromOffset(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	path := writeTestTranscript(t, testTranscriptJSON)

	// From offset 0 — should get both main.go and util.go
	files, pos, err := ag.ExtractModifiedFilesFromOffset(path, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pos != 4 {
		t.Errorf("expected position 4, got %d", pos)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d: %v", len(files), files)
	}

	// From offset 2 — should only get util.go (messages 3 and 4)
	files, pos, err = ag.ExtractModifiedFilesFromOffset(path, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pos != 4 {
		t.Errorf("expected position 4, got %d", pos)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d: %v", len(files), files)
	}
	if files[0] != "util.go" {
		t.Errorf("expected 'util.go', got %q", files[0])
	}
}

func TestExtractPrompts(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	path := writeTestTranscript(t, testTranscriptJSON)

	// From offset 0 — both prompts
	prompts, err := ag.ExtractPrompts(path, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prompts) != 2 {
		t.Fatalf("expected 2 prompts, got %d: %v", len(prompts), prompts)
	}
	if prompts[0] != "Fix the bug in main.go" {
		t.Errorf("expected first prompt 'Fix the bug in main.go', got %q", prompts[0])
	}

	// From offset 2 — only second prompt
	prompts, err = ag.ExtractPrompts(path, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prompts) != 1 {
		t.Fatalf("expected 1 prompt from offset 2, got %d", len(prompts))
	}
	if prompts[0] != "Also fix util.go" {
		t.Errorf("expected 'Also fix util.go', got %q", prompts[0])
	}
}

func TestExtractSummary(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	path := writeTestTranscript(t, testTranscriptJSON)

	summary, err := ag.ExtractSummary(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary != "Done fixing util.go." {
		t.Errorf("expected summary 'Done fixing util.go.', got %q", summary)
	}
}

func TestExtractSummary_EmptyTranscript(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	path := writeTestTranscript(t, `{"session_id": "empty", "messages": []}`)

	summary, err := ag.ExtractSummary(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary != "" {
		t.Errorf("expected empty summary, got %q", summary)
	}
}

func TestCalculateTokenUsage(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	path := writeTestTranscript(t, testTranscriptJSON)

	// From offset 0 — both assistant messages
	usage, err := ag.CalculateTokenUsage(path, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage == nil {
		t.Fatal("expected non-nil usage")
	}
	if usage.InputTokens != 350 {
		t.Errorf("expected 350 input tokens, got %d", usage.InputTokens)
	}
	if usage.OutputTokens != 180 {
		t.Errorf("expected 180 output tokens, got %d", usage.OutputTokens)
	}
	if usage.CacheReadTokens != 15 {
		t.Errorf("expected 15 cache read tokens, got %d", usage.CacheReadTokens)
	}
	if usage.CacheCreationTokens != 35 {
		t.Errorf("expected 35 cache creation tokens, got %d", usage.CacheCreationTokens)
	}
	if usage.APICallCount != 2 {
		t.Errorf("expected 2 API calls, got %d", usage.APICallCount)
	}
}

func TestCalculateTokenUsage_FromOffset(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	path := writeTestTranscript(t, testTranscriptJSON)

	usage, err := ag.CalculateTokenUsage(path, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.InputTokens != 200 {
		t.Errorf("expected 200 input tokens, got %d", usage.InputTokens)
	}
	if usage.OutputTokens != 100 {
		t.Errorf("expected 100 output tokens, got %d", usage.OutputTokens)
	}
	if usage.APICallCount != 1 {
		t.Errorf("expected 1 API call, got %d", usage.APICallCount)
	}
}

func TestCalculateTokenUsage_NonexistentFile(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}

	usage, err := ag.CalculateTokenUsage("/nonexistent/path.json", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage != nil {
		t.Errorf("expected nil usage for nonexistent file, got %+v", usage)
	}
}

func TestSliceFromMessage_ReturnsFullWhenZeroOffset(t *testing.T) {
	t.Parallel()
	data := []byte(testTranscriptJSON)

	result := SliceFromMessage(data, 0)
	if string(result) != string(data) {
		t.Error("expected full transcript returned for offset 0")
	}
}

func TestSliceFromMessage_SlicesFromOffset(t *testing.T) {
	t.Parallel()
	data := []byte(testTranscriptJSON)

	// Offset 2 should return messages starting from index 2 (msg-3, msg-4)
	result := SliceFromMessage(data, 2)
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	parsed, err := ParseTranscript(result)
	if err != nil {
		t.Fatalf("failed to parse sliced transcript: %v", err)
	}
	if len(parsed.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(parsed.Messages))
	}
	if parsed.Messages[0].ID != "msg-3" {
		t.Errorf("expected first message ID 'msg-3', got %q", parsed.Messages[0].ID)
	}
	if parsed.SessionID != "test-session" {
		t.Errorf("expected session ID preserved, got %q", parsed.SessionID)
	}
}

func TestSliceFromMessage_OffsetBeyondLength(t *testing.T) {
	t.Parallel()
	data := []byte(testTranscriptJSON)

	result := SliceFromMessage(data, 100)
	if result != nil {
		t.Errorf("expected nil for offset beyond message count, got %d bytes", len(result))
	}
}

func TestSliceFromMessage_EmptyData(t *testing.T) {
	t.Parallel()

	result := SliceFromMessage(nil, 0)
	if result != nil {
		t.Errorf("expected nil for nil data, got %d bytes", len(result))
	}

	// []byte{} with offset 0 returns the input as-is (passthrough)
	result = SliceFromMessage([]byte{}, 0)
	if len(result) != 0 {
		t.Errorf("expected empty bytes, got %d bytes", len(result))
	}
}

func TestSliceFromMessage_InvalidJSON(t *testing.T) {
	t.Parallel()

	result := SliceFromMessage([]byte("not json"), 1)
	if result != nil {
		t.Errorf("expected nil for invalid JSON, got %d bytes", len(result))
	}
}

func TestChunkTranscript_SmallContent(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	content := []byte(testTranscriptJSON)

	// maxSize larger than content — should return single chunk
	chunks, err := ag.ChunkTranscript(content, len(content)+1000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for small content, got %d", len(chunks))
	}
	if string(chunks[0]) != string(content) {
		t.Error("expected chunk to match original content")
	}
}

func TestChunkTranscript_SplitsLargeContent(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	content := []byte(testTranscriptJSON)

	// Use a small maxSize to force splitting (each message is ~200-300 bytes)
	chunks, err := ag.ChunkTranscript(content, 350)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks for small maxSize, got %d", len(chunks))
	}

	// Each chunk should be valid JSON
	for i, chunk := range chunks {
		parsed, parseErr := ParseTranscript(chunk)
		if parseErr != nil {
			t.Fatalf("chunk %d: failed to parse: %v", i, parseErr)
		}
		if parsed.SessionID != "test-session" {
			t.Errorf("chunk %d: expected session_id 'test-session', got %q", i, parsed.SessionID)
		}
		if len(parsed.Messages) == 0 {
			t.Errorf("chunk %d: expected at least 1 message", i)
		}
	}
}

func TestChunkTranscript_RoundTrip(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	content := []byte(testTranscriptJSON)

	// Split into chunks
	chunks, err := ag.ChunkTranscript(content, 350)
	if err != nil {
		t.Fatalf("chunk error: %v", err)
	}

	// Reassemble
	reassembled, err := ag.ReassembleTranscript(chunks)
	if err != nil {
		t.Fatalf("reassemble error: %v", err)
	}

	// Parse both and compare messages
	original, parseErr := ParseTranscript(content)
	if parseErr != nil {
		t.Fatalf("failed to parse original: %v", parseErr)
	}
	result, parseErr := ParseTranscript(reassembled)
	if parseErr != nil {
		t.Fatalf("failed to parse reassembled: %v", parseErr)
	}

	if result.SessionID != original.SessionID {
		t.Errorf("session_id mismatch: %q vs %q", result.SessionID, original.SessionID)
	}
	if len(result.Messages) != len(original.Messages) {
		t.Fatalf("message count mismatch: %d vs %d", len(result.Messages), len(original.Messages))
	}
	for i, msg := range result.Messages {
		if msg.ID != original.Messages[i].ID {
			t.Errorf("message %d: ID mismatch %q vs %q", i, msg.ID, original.Messages[i].ID)
		}
	}
}

func TestChunkTranscript_EmptyMessages(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	content := []byte(`{"session_id": "empty", "messages": []}`)

	chunks, err := ag.ChunkTranscript(content, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for empty messages, got %d", len(chunks))
	}
}

func TestReassembleTranscript_SingleChunk(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	content := []byte(testTranscriptJSON)

	result, err := ag.ReassembleTranscript([][]byte{content})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(result) != string(content) {
		t.Error("single chunk reassembly should return original content")
	}
}

func TestReassembleTranscript_Empty(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}

	result, err := ag.ReassembleTranscript(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil for empty chunks, got %d bytes", len(result))
	}
}

func TestExtractModifiedFiles(t *testing.T) {
	t.Parallel()

	files, err := ExtractModifiedFiles([]byte(testTranscriptJSON))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d: %v", len(files), files)
	}
	if files[0] != "main.go" {
		t.Errorf("expected first file 'main.go', got %q", files[0])
	}
	if files[1] != "util.go" {
		t.Errorf("expected second file 'util.go', got %q", files[1])
	}
}
