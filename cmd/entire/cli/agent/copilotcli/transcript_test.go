package copilotcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// testJSONLLines returns JSONL lines matching the real Copilot CLI transcript format
// with tool.execution_complete events for file modification tracking.
var testJSONLLines = []string{
	`{"type":"session.start","data":{"sessionId":"abc123"},"id":"1","timestamp":"2026-03-03T00:00:00Z","parentId":""}`,
	`{"type":"user.message","data":{"content":"create hello.txt"},"id":"2","timestamp":"2026-03-03T00:00:01Z","parentId":""}`,
	`{"type":"assistant.turn_start","data":{},"id":"3","timestamp":"2026-03-03T00:00:02Z","parentId":""}`,
	`{"type":"assistant.message","data":{"content":"I'll create that file.","toolRequests":[{"toolCallId":"tc1"}]},"id":"4","timestamp":"2026-03-03T00:00:03Z","parentId":"3"}`,
	`{"type":"tool.execution_complete","data":{"toolCallId":"tc1","toolTelemetry":{"properties":{"filePaths":"[\"/tmp/test/hello.txt\"]"},"metrics":{"linesAdded":1,"linesRemoved":0}}},"id":"5","timestamp":"2026-03-03T00:00:04Z","parentId":"4"}`,
	`{"type":"assistant.message","data":{"content":"Created hello.txt.","toolRequests":[]},"id":"6","timestamp":"2026-03-03T00:00:05Z","parentId":"3"}`,
	`{"type":"assistant.turn_end","data":{},"id":"7","timestamp":"2026-03-03T00:00:06Z","parentId":"3"}`,
}

func writeTestJSONL(t *testing.T, lines []string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write test JSONL: %v", err)
	}
	return path
}

// TestCopilotImplementsTranscriptAnalyzer is a compile-time interface check.
// The var _ check in transcript.go is the real guard; this test makes it visible
// in test output.
func TestCopilotImplementsTranscriptAnalyzer(t *testing.T) {
	t.Parallel()
	var a agent.Agent = &CopilotCLIAgent{}
	if _, ok := a.(agent.TranscriptAnalyzer); !ok {
		t.Fatal("CopilotCLIAgent must implement agent.TranscriptAnalyzer")
	}
}

func TestExtractModifiedFilesFromEvents(t *testing.T) {
	t.Parallel()

	t.Run("extracts files from tool.execution_complete", func(t *testing.T) {
		t.Parallel()
		content := strings.Join(testJSONLLines, "\n") + "\n"
		events := parseEventsFromBytes([]byte(content))
		files := extractModifiedFilesFromEvents(events)
		if len(files) != 1 {
			t.Fatalf("expected 1 file, got %d: %v", len(files), files)
		}
		if files[0] != "/tmp/test/hello.txt" {
			t.Errorf("expected '/tmp/test/hello.txt', got %q", files[0])
		}
	})

	t.Run("handles empty events", func(t *testing.T) {
		t.Parallel()
		files := extractModifiedFilesFromEvents(nil)
		if len(files) != 0 {
			t.Errorf("expected 0 files for nil events, got %d", len(files))
		}
	})

	t.Run("deduplicates files", func(t *testing.T) {
		t.Parallel()
		// Two tool.execution_complete events touching the same file
		lines := []string{
			`{"type":"tool.execution_complete","data":{"toolCallId":"tc1","toolTelemetry":{"properties":{"filePaths":"[\"/tmp/test/hello.txt\"]"},"metrics":{"linesAdded":1,"linesRemoved":0}}},"id":"5","timestamp":"2026-03-03T00:00:04Z","parentId":"4"}`,
			`{"type":"tool.execution_complete","data":{"toolCallId":"tc2","toolTelemetry":{"properties":{"filePaths":"[\"/tmp/test/hello.txt\",\"/tmp/test/world.txt\"]"},"metrics":{"linesAdded":2,"linesRemoved":0}}},"id":"8","timestamp":"2026-03-03T00:00:07Z","parentId":"6"}`,
		}
		content := strings.Join(lines, "\n") + "\n"
		events := parseEventsFromBytes([]byte(content))
		files := extractModifiedFilesFromEvents(events)
		if len(files) != 2 {
			t.Fatalf("expected 2 deduplicated files, got %d: %v", len(files), files)
		}
		if files[0] != "/tmp/test/hello.txt" {
			t.Errorf("expected first file '/tmp/test/hello.txt', got %q", files[0])
		}
		if files[1] != "/tmp/test/world.txt" {
			t.Errorf("expected second file '/tmp/test/world.txt', got %q", files[1])
		}
	})
}

func TestExtractPromptsFromEvents(t *testing.T) {
	t.Parallel()

	t.Run("extracts user messages", func(t *testing.T) {
		t.Parallel()
		content := strings.Join(testJSONLLines, "\n") + "\n"
		events := parseEventsFromBytes([]byte(content))
		prompts := extractPromptsFromEvents(events)
		if len(prompts) != 1 {
			t.Fatalf("expected 1 prompt, got %d: %v", len(prompts), prompts)
		}
		if prompts[0] != "create hello.txt" {
			t.Errorf("expected 'create hello.txt', got %q", prompts[0])
		}
	})

	t.Run("multi-turn conversation", func(t *testing.T) {
		t.Parallel()
		lines := append(testJSONLLines, //nolint:gocritic // append to copy is intentional
			`{"type":"user.message","data":{"content":"now delete it"},"id":"8","timestamp":"2026-03-03T00:01:00Z","parentId":"7"}`,
		)
		content := strings.Join(lines, "\n") + "\n"
		events := parseEventsFromBytes([]byte(content))
		prompts := extractPromptsFromEvents(events)
		if len(prompts) != 2 {
			t.Fatalf("expected 2 prompts, got %d: %v", len(prompts), prompts)
		}
		if prompts[0] != "create hello.txt" {
			t.Errorf("expected first prompt 'create hello.txt', got %q", prompts[0])
		}
		if prompts[1] != "now delete it" {
			t.Errorf("expected second prompt 'now delete it', got %q", prompts[1])
		}
	})
}

func TestExtractSummaryFromEvents(t *testing.T) {
	t.Parallel()

	t.Run("returns last assistant text", func(t *testing.T) {
		t.Parallel()
		content := strings.Join(testJSONLLines, "\n") + "\n"
		events := parseEventsFromBytes([]byte(content))
		summary := extractSummaryFromEvents(events)
		if summary != "Created hello.txt." {
			t.Errorf("expected 'Created hello.txt.', got %q", summary)
		}
	})

	t.Run("empty events returns empty string", func(t *testing.T) {
		t.Parallel()
		summary := extractSummaryFromEvents(nil)
		if summary != "" {
			t.Errorf("expected empty summary, got %q", summary)
		}
	})
}

func TestGetTranscriptPositionCopilot(t *testing.T) {
	t.Parallel()

	t.Run("counts lines", func(t *testing.T) {
		t.Parallel()
		ag := &CopilotCLIAgent{}
		path := writeTestJSONL(t, testJSONLLines)

		pos, err := ag.GetTranscriptPosition(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pos != 7 {
			t.Errorf("expected 7 lines, got %d", pos)
		}
	})

	t.Run("nonexistent file returns 0", func(t *testing.T) {
		t.Parallel()
		ag := &CopilotCLIAgent{}

		pos, err := ag.GetTranscriptPosition("/nonexistent/path/events.jsonl")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pos != 0 {
			t.Errorf("expected 0 for nonexistent file, got %d", pos)
		}
	})

	t.Run("empty path returns 0", func(t *testing.T) {
		t.Parallel()
		ag := &CopilotCLIAgent{}

		pos, err := ag.GetTranscriptPosition("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pos != 0 {
			t.Errorf("expected 0 for empty path, got %d", pos)
		}
	})
}

func TestExtractModifiedFilesFromOffset(t *testing.T) {
	t.Parallel()

	t.Run("from beginning", func(t *testing.T) {
		t.Parallel()
		ag := &CopilotCLIAgent{}
		path := writeTestJSONL(t, testJSONLLines)

		files, pos, err := ag.ExtractModifiedFilesFromOffset(path, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pos != 7 {
			t.Errorf("expected position 7, got %d", pos)
		}
		if len(files) != 1 {
			t.Fatalf("expected 1 file, got %d: %v", len(files), files)
		}
		if files[0] != "/tmp/test/hello.txt" {
			t.Errorf("expected '/tmp/test/hello.txt', got %q", files[0])
		}
	})

	t.Run("from after tool execution", func(t *testing.T) {
		t.Parallel()
		ag := &CopilotCLIAgent{}
		path := writeTestJSONL(t, testJSONLLines)

		// Offset 5 means skip first 5 lines (tool.execution_complete is line 5)
		// so only lines 6 and 7 remain (assistant.message and assistant.turn_end)
		files, pos, err := ag.ExtractModifiedFilesFromOffset(path, 5)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pos != 7 {
			t.Errorf("expected position 7, got %d", pos)
		}
		if len(files) != 0 {
			t.Fatalf("expected 0 files from offset 5, got %d: %v", len(files), files)
		}
	})

	t.Run("empty path returns nil", func(t *testing.T) {
		t.Parallel()
		ag := &CopilotCLIAgent{}

		files, pos, err := ag.ExtractModifiedFilesFromOffset("", 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pos != 0 {
			t.Errorf("expected position 0, got %d", pos)
		}
		if files != nil {
			t.Errorf("expected nil files, got %v", files)
		}
	})
}

func TestExtractPrompts(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}
	path := writeTestJSONL(t, testJSONLLines)

	t.Run("from beginning", func(t *testing.T) {
		t.Parallel()
		prompts, err := ag.ExtractPrompts(path, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(prompts) != 1 {
			t.Fatalf("expected 1 prompt, got %d: %v", len(prompts), prompts)
		}
		if prompts[0] != "create hello.txt" {
			t.Errorf("expected 'create hello.txt', got %q", prompts[0])
		}
	})

	t.Run("from offset past user message", func(t *testing.T) {
		t.Parallel()
		// Offset 2 means skip first 2 lines (session.start and user.message)
		prompts, err := ag.ExtractPrompts(path, 2)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(prompts) != 0 {
			t.Fatalf("expected 0 prompts from offset 2, got %d: %v", len(prompts), prompts)
		}
	})
}

func TestExtractSummary(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}
	path := writeTestJSONL(t, testJSONLLines)

	summary, err := ag.ExtractSummary(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary != "Created hello.txt." {
		t.Errorf("expected 'Created hello.txt.', got %q", summary)
	}
}

func TestExtractSummary_EmptyTranscript(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}
	lines := []string{
		`{"type":"session.start","data":{"sessionId":"abc123"},"id":"1","timestamp":"2026-03-03T00:00:00Z","parentId":""}`,
	}
	path := writeTestJSONL(t, lines)

	summary, err := ag.ExtractSummary(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary != "" {
		t.Errorf("expected empty summary, got %q", summary)
	}
}
