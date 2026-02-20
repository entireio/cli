package opencode

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// testTranscriptJSONL is a JSONL transcript with 4 messages (one per line).
const testTranscriptJSONL = `{"id":"msg-1","role":"user","content":"Fix the bug in main.go","time":{"created":1708300000}}
{"id":"msg-2","role":"assistant","content":"I'll fix the bug.","time":{"created":1708300001,"completed":1708300005},"tokens":{"input":150,"output":80,"reasoning":10,"cache":{"read":5,"write":15}},"cost":0.003,"parts":[{"type":"text","text":"I'll fix the bug."},{"type":"tool","tool":"edit","callID":"call-1","state":{"status":"completed","input":{"file_path":"main.go"},"output":"Applied edit"}}]}
{"id":"msg-3","role":"user","content":"Also fix util.go","time":{"created":1708300010}}
{"id":"msg-4","role":"assistant","content":"Done fixing util.go.","time":{"created":1708300011,"completed":1708300015},"tokens":{"input":200,"output":100,"reasoning":5,"cache":{"read":10,"write":20}},"cost":0.005,"parts":[{"type":"tool","tool":"write","callID":"call-2","state":{"status":"completed","input":{"file_path":"util.go"},"output":"File written"}},{"type":"text","text":"Done fixing util.go."}]}
`

func writeTestTranscript(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test-session.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write test transcript: %v", err)
	}
	return path
}

func TestParseMessages(t *testing.T) {
	t.Parallel()

	messages, err := ParseMessages([]byte(testTranscriptJSONL))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(messages) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(messages))
	}
	if messages[0].ID != "msg-1" {
		t.Errorf("expected first message ID 'msg-1', got %q", messages[0].ID)
	}
	if messages[0].Role != "user" {
		t.Errorf("expected first message role 'user', got %q", messages[0].Role)
	}
}

func TestParseMessages_Empty(t *testing.T) {
	t.Parallel()

	messages, err := ParseMessages(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if messages != nil {
		t.Errorf("expected nil for nil data, got %d messages", len(messages))
	}

	messages, err = ParseMessages([]byte(""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if messages != nil {
		t.Errorf("expected nil for empty data, got %d messages", len(messages))
	}
}

func TestParseMessages_InvalidLines(t *testing.T) {
	t.Parallel()

	// Invalid lines are silently skipped
	data := "not json\n{\"id\":\"msg-1\",\"role\":\"user\",\"content\":\"hello\"}\n"
	messages, err := ParseMessages([]byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("expected 1 valid message, got %d", len(messages))
	}
	if messages[0].Content != "hello" {
		t.Errorf("expected content 'hello', got %q", messages[0].Content)
	}
}

func TestGetTranscriptPosition(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	path := writeTestTranscript(t, testTranscriptJSONL)

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

	pos, err := ag.GetTranscriptPosition("/nonexistent/path.jsonl")
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
	path := writeTestTranscript(t, testTranscriptJSONL)

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
	path := writeTestTranscript(t, testTranscriptJSONL)

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
	path := writeTestTranscript(t, testTranscriptJSONL)

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
	path := writeTestTranscript(t, "")

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
	path := writeTestTranscript(t, testTranscriptJSONL)

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
	path := writeTestTranscript(t, testTranscriptJSONL)

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

	usage, err := ag.CalculateTokenUsage("/nonexistent/path.jsonl", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage != nil {
		t.Errorf("expected nil usage for nonexistent file, got %+v", usage)
	}
}

func TestChunkTranscript_SmallContent(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	content := []byte(testTranscriptJSONL)

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
	content := []byte(testTranscriptJSONL)

	// Use a maxSize that fits individual lines but forces splitting (assistant lines are ~370-400 bytes)
	chunks, err := ag.ChunkTranscript(content, 500)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks for small maxSize, got %d", len(chunks))
	}

	// Each chunk should contain valid JSONL
	for i, chunk := range chunks {
		messages, parseErr := ParseMessages(chunk)
		if parseErr != nil {
			t.Fatalf("chunk %d: failed to parse: %v", i, parseErr)
		}
		if len(messages) == 0 {
			t.Errorf("chunk %d: expected at least 1 message", i)
		}
	}
}

func TestChunkTranscript_RoundTrip(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	content := []byte(testTranscriptJSONL)

	// Split into chunks (maxSize must fit individual JSONL lines)
	chunks, err := ag.ChunkTranscript(content, 500)
	if err != nil {
		t.Fatalf("chunk error: %v", err)
	}

	// Reassemble
	reassembled, err := ag.ReassembleTranscript(chunks)
	if err != nil {
		t.Fatalf("reassemble error: %v", err)
	}

	// Parse both and compare messages
	original, parseErr := ParseMessages(content)
	if parseErr != nil {
		t.Fatalf("failed to parse original: %v", parseErr)
	}
	result, parseErr := ParseMessages(reassembled)
	if parseErr != nil {
		t.Fatalf("failed to parse reassembled: %v", parseErr)
	}

	if len(result) != len(original) {
		t.Fatalf("message count mismatch: %d vs %d", len(result), len(original))
	}
	for i, msg := range result {
		if msg.ID != original[i].ID {
			t.Errorf("message %d: ID mismatch %q vs %q", i, msg.ID, original[i].ID)
		}
	}
}

func TestChunkTranscript_EmptyContent(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}

	chunks, err := ag.ChunkTranscript([]byte(""), 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 0 {
		t.Fatalf("expected 0 chunks for empty content, got %d", len(chunks))
	}
}

func TestReassembleTranscript_SingleChunk(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	content := []byte(testTranscriptJSONL)

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
	if len(result) != 0 {
		t.Errorf("expected empty result for nil chunks, got %d bytes", len(result))
	}
}

func TestExtractModifiedFiles(t *testing.T) {
	t.Parallel()

	files, err := ExtractModifiedFiles([]byte(testTranscriptJSONL))
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

// Compile-time interface checks are in transcript.go.
// Verify the unused import guard by referencing the agent package.
var _ = agent.AgentNameOpenCode
