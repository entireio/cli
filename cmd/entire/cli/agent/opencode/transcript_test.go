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
