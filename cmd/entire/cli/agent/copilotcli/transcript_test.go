package copilotcli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testFileHello       = "/tmp/test/hello.txt"
	testPromptCreateTxt = "create hello.txt"
	testModelSonnet     = "claude-sonnet-4.6"
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

func TestExtractModifiedFilesFromEvents(t *testing.T) {
	t.Parallel()

	t.Run("extracts files from tool.execution_complete", func(t *testing.T) {
		t.Parallel()
		content := strings.Join(testJSONLLines, "\n") + "\n"
		events, _ := parseEventsFromBytes([]byte(content)) //nolint:errcheck // test input is always valid
		files := extractModifiedFilesFromEvents(events)
		if len(files) != 1 {
			t.Fatalf("expected 1 file, got %d: %v", len(files), files)
		}
		if files[0] != testFileHello {
			t.Errorf("expected %q, got %q", testFileHello, files[0])
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
		events, _ := parseEventsFromBytes([]byte(content)) //nolint:errcheck // test input is always valid
		files := extractModifiedFilesFromEvents(events)
		if len(files) != 2 {
			t.Fatalf("expected 2 deduplicated files, got %d: %v", len(files), files)
		}
		if files[0] != testFileHello {
			t.Errorf("expected first file %q, got %q", testFileHello, files[0])
		}
		if files[1] != "/tmp/test/world.txt" {
			t.Errorf("expected second file '/tmp/test/world.txt', got %q", files[1])
		}
	})
}

func TestExtractPromptsFromEvents(t *testing.T) {
	t.Parallel()

	t.Run("multi-turn conversation", func(t *testing.T) {
		t.Parallel()
		lines := append(testJSONLLines, //nolint:gocritic // append to copy is intentional
			`{"type":"user.message","data":{"content":"now delete it"},"id":"8","timestamp":"2026-03-03T00:01:00Z","parentId":"7"}`,
		)
		content := strings.Join(lines, "\n") + "\n"
		events, _ := parseEventsFromBytes([]byte(content)) //nolint:errcheck // test input is always valid
		prompts := extractPromptsFromEvents(events)
		if len(prompts) != 2 {
			t.Fatalf("expected 2 prompts, got %d: %v", len(prompts), prompts)
		}
		if prompts[0] != testPromptCreateTxt {
			t.Errorf("expected first prompt %q, got %q", testPromptCreateTxt, prompts[0])
		}
		if prompts[1] != "now delete it" {
			t.Errorf("expected second prompt 'now delete it', got %q", prompts[1])
		}
	})
}

func TestExtractSummaryFromEvents(t *testing.T) {
	t.Parallel()

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

	t.Run("counts lines without trailing newline", func(t *testing.T) {
		t.Parallel()
		ag := &CopilotCLIAgent{}
		dir := t.TempDir()
		path := filepath.Join(dir, "events.jsonl")
		// Write 3 lines WITHOUT a trailing newline
		content := strings.Join(testJSONLLines[:3], "\n") // no trailing \n
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("failed to write test file: %v", err)
		}

		pos, err := ag.GetTranscriptPosition(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pos != 3 {
			t.Errorf("expected 3 lines (no trailing newline), got %d", pos)
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
		if files[0] != testFileHello {
			t.Errorf("expected %q, got %q", testFileHello, files[0])
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
		if prompts[0] != testPromptCreateTxt {
			t.Errorf("expected %q, got %q", testPromptCreateTxt, prompts[0])
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

func TestParseEventsFromBytes_MalformedLines(t *testing.T) {
	t.Parallel()
	lines := []string{
		`{"type":"user.message","data":{"content":"hello"},"id":"1","timestamp":"2026-03-03T00:00:00Z","parentId":""}`,
		`THIS IS NOT JSON`,
		`{"type":"user.message","data":{"content":"world"},"id":"2","timestamp":"2026-03-03T00:00:01Z","parentId":""}`,
	}
	content := strings.Join(lines, "\n") + "\n"
	events, _ := parseEventsFromBytes([]byte(content)) //nolint:errcheck // test input is always valid
	if len(events) != 2 {
		t.Fatalf("expected 2 events (malformed line skipped), got %d", len(events))
	}
	if events[0].Type != eventTypeUserMessage {
		t.Errorf("expected first event type %q, got %q", eventTypeUserMessage, events[0].Type)
	}
	if events[1].Type != eventTypeUserMessage {
		t.Errorf("expected second event type %q, got %q", eventTypeUserMessage, events[1].Type)
	}
}

func TestExtractSummary_SkipsEmptyContentAssistantMessages(t *testing.T) {
	t.Parallel()
	// Simulates -p (headless) mode where assistant.message has content: ""
	// and tool requests but no text. Summary should fall back to the earlier
	// assistant message that has text.
	lines := []string{
		`{"type":"assistant.message","data":{"content":"I'll create that file.","toolRequests":[{"toolCallId":"tc1"}]},"id":"1","timestamp":"2026-03-03T00:00:00Z","parentId":""}`,
		`{"type":"assistant.message","data":{"content":"","toolRequests":[{"toolCallId":"tc2"}]},"id":"2","timestamp":"2026-03-03T00:00:01Z","parentId":""}`,
	}
	content := strings.Join(lines, "\n") + "\n"
	events, _ := parseEventsFromBytes([]byte(content)) //nolint:errcheck // test input is always valid
	summary := extractSummaryFromEvents(events)
	if summary != "I'll create that file." {
		t.Errorf("expected summary from earlier message, got %q", summary)
	}
}

func TestExtractModifiedFilesFromEvents_EmptyAndMalformedFilePaths(t *testing.T) {
	t.Parallel()

	t.Run("empty filePaths string", func(t *testing.T) {
		t.Parallel()
		lines := []string{
			`{"type":"tool.execution_complete","data":{"toolCallId":"tc1","toolTelemetry":{"properties":{"filePaths":""},"metrics":{"linesAdded":0,"linesRemoved":0}}},"id":"1","timestamp":"2026-03-03T00:00:00Z","parentId":""}`,
		}
		content := strings.Join(lines, "\n") + "\n"
		events, _ := parseEventsFromBytes([]byte(content)) //nolint:errcheck // test input is always valid
		files := extractModifiedFilesFromEvents(events)
		if len(files) != 0 {
			t.Errorf("expected 0 files for empty filePaths, got %d: %v", len(files), files)
		}
	})

	t.Run("malformed filePaths JSON", func(t *testing.T) {
		t.Parallel()
		lines := []string{
			`{"type":"tool.execution_complete","data":{"toolCallId":"tc1","toolTelemetry":{"properties":{"filePaths":"not-valid-json"},"metrics":{"linesAdded":1,"linesRemoved":0}}},"id":"1","timestamp":"2026-03-03T00:00:00Z","parentId":""}`,
		}
		content := strings.Join(lines, "\n") + "\n"
		events, _ := parseEventsFromBytes([]byte(content)) //nolint:errcheck // test input is always valid
		files := extractModifiedFilesFromEvents(events)
		if len(files) != 0 {
			t.Errorf("expected 0 files for malformed filePaths, got %d: %v", len(files), files)
		}
	})
}

// --- Token Usage Tests ---

func TestCalculateTokenUsage_SessionShutdown(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}

	lines := append(testJSONLLines, //nolint:gocritic // append to copy is intentional
		`{"type":"session.shutdown","data":{"modelMetrics":[{"modelId":"claude-sonnet-4.6","requests":{"count":3},"usage":{"inputTokens":64807,"outputTokens":289,"cacheReadTokens":42625,"cacheWriteTokens":100}}]},"id":"99","timestamp":"2026-03-03T00:01:00Z","parentId":""}`,
	)
	content := []byte(strings.Join(lines, "\n") + "\n")

	usage, err := ag.CalculateTokenUsage(content, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.InputTokens != 64807 {
		t.Errorf("InputTokens = %d, want 64807", usage.InputTokens)
	}
	if usage.OutputTokens != 289 {
		t.Errorf("OutputTokens = %d, want 289", usage.OutputTokens)
	}
	if usage.CacheReadTokens != 42625 {
		t.Errorf("CacheReadTokens = %d, want 42625", usage.CacheReadTokens)
	}
	if usage.CacheCreationTokens != 100 {
		t.Errorf("CacheCreationTokens = %d, want 100", usage.CacheCreationTokens)
	}
	if usage.APICallCount != 3 {
		t.Errorf("APICallCount = %d, want 3", usage.APICallCount)
	}
}

func TestCalculateTokenUsage_MultiModel(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}

	lines := []string{
		`{"type":"session.shutdown","data":{"modelMetrics":[` +
			`{"modelId":"gpt-4.1","requests":{"count":2},"usage":{"inputTokens":1000,"outputTokens":200,"cacheReadTokens":0,"cacheWriteTokens":0}},` +
			`{"modelId":"claude-sonnet-4.6","requests":{"count":1},"usage":{"inputTokens":500,"outputTokens":100,"cacheReadTokens":300,"cacheWriteTokens":50}}` +
			`]},"id":"1","timestamp":"2026-03-03T00:00:00Z","parentId":""}`,
	}
	content := []byte(strings.Join(lines, "\n") + "\n")

	usage, err := ag.CalculateTokenUsage(content, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.InputTokens != 1500 {
		t.Errorf("InputTokens = %d, want 1500", usage.InputTokens)
	}
	if usage.OutputTokens != 300 {
		t.Errorf("OutputTokens = %d, want 300", usage.OutputTokens)
	}
	if usage.CacheReadTokens != 300 {
		t.Errorf("CacheReadTokens = %d, want 300", usage.CacheReadTokens)
	}
	if usage.APICallCount != 3 {
		t.Errorf("APICallCount = %d, want 3", usage.APICallCount)
	}
}

func TestCalculateTokenUsage_FallbackToAssistantMessages(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}

	// No session.shutdown — should fall back to assistant.message outputTokens
	lines := []string{
		`{"type":"assistant.message","data":{"content":"hello","outputTokens":150},"id":"1","timestamp":"2026-03-03T00:00:00Z","parentId":""}`,
		`{"type":"assistant.message","data":{"content":"world","outputTokens":114},"id":"2","timestamp":"2026-03-03T00:00:01Z","parentId":""}`,
	}
	content := []byte(strings.Join(lines, "\n") + "\n")

	usage, err := ag.CalculateTokenUsage(content, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.InputTokens != 0 {
		t.Errorf("InputTokens = %d, want 0 (fallback has no input tokens)", usage.InputTokens)
	}
	if usage.OutputTokens != 264 {
		t.Errorf("OutputTokens = %d, want 264", usage.OutputTokens)
	}
	if usage.APICallCount != 2 {
		t.Errorf("APICallCount = %d, want 2", usage.APICallCount)
	}
}

func TestCalculateTokenUsage_EmptyTranscript(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}

	usage, err := ag.CalculateTokenUsage([]byte{}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.OutputTokens != 0 {
		t.Errorf("OutputTokens = %d, want 0", usage.OutputTokens)
	}
}

func TestCalculateTokenUsage_WithOffset(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}

	// session.shutdown is on line 2; offset 1 skips line 1 but still sees shutdown
	lines := []string{
		`{"type":"user.message","data":{"content":"hello"},"id":"1","timestamp":"2026-03-03T00:00:00Z","parentId":""}`,
		`{"type":"session.shutdown","data":{"modelMetrics":[{"modelId":"claude-sonnet-4.6","requests":{"count":1},"usage":{"inputTokens":500,"outputTokens":50,"cacheReadTokens":0,"cacheWriteTokens":0}}]},"id":"2","timestamp":"2026-03-03T00:00:01Z","parentId":""}`,
	}
	content := []byte(strings.Join(lines, "\n") + "\n")

	usage, err := ag.CalculateTokenUsage(content, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.InputTokens != 500 {
		t.Errorf("InputTokens = %d, want 500", usage.InputTokens)
	}
}

func TestExtractModelFromTranscript_ModelChangeEvent(t *testing.T) {
	t.Parallel()

	lines := []string{
		`{"type":"session.start","data":{"sessionId":"abc123"},"id":"1","timestamp":"2026-03-03T00:00:00Z","parentId":""}`,
		`{"type":"session.model_change","data":{"newModel":"claude-sonnet-4.6"},"id":"2","timestamp":"2026-03-03T00:00:01Z","parentId":"1"}`,
		`{"type":"user.message","data":{"content":"hello"},"id":"3","timestamp":"2026-03-03T00:00:02Z","parentId":""}`,
	}
	path := writeTestJSONL(t, lines)

	model := ExtractModelFromTranscript(context.Background(), path)
	if model != testModelSonnet {
		t.Errorf("ExtractModelFromTranscript() = %q, want %q", model, testModelSonnet)
	}
}

func TestExtractModelFromTranscript_FallbackToToolExecComplete(t *testing.T) {
	t.Parallel()

	// No session.model_change, but tool.execution_complete has a model field
	lines := []string{
		`{"type":"session.start","data":{"sessionId":"abc123"},"id":"1","timestamp":"2026-03-03T00:00:00Z","parentId":""}`,
		`{"type":"user.message","data":{"content":"hello"},"id":"2","timestamp":"2026-03-03T00:00:01Z","parentId":""}`,
		`{"type":"tool.execution_complete","data":{"toolCallId":"tc1","model":"claude-sonnet-4.6","toolTelemetry":{"properties":{},"metrics":{}}},"id":"3","timestamp":"2026-03-03T00:00:02Z","parentId":""}`,
	}
	path := writeTestJSONL(t, lines)

	model := ExtractModelFromTranscript(context.Background(), path)
	if model != testModelSonnet {
		t.Errorf("ExtractModelFromTranscript() = %q, want %q (fallback to tool.execution_complete)", model, testModelSonnet)
	}
}

func TestExtractModelFromTranscript_ModelChangeTakesPrecedence(t *testing.T) {
	t.Parallel()

	// Both session.model_change and tool.execution_complete present — model_change wins
	lines := []string{
		`{"type":"session.start","data":{"sessionId":"abc123"},"id":"1","timestamp":"2026-03-03T00:00:00Z","parentId":""}`,
		`{"type":"session.model_change","data":{"newModel":"gpt-4.1"},"id":"2","timestamp":"2026-03-03T00:00:01Z","parentId":"1"}`,
		`{"type":"tool.execution_complete","data":{"toolCallId":"tc1","model":"claude-sonnet-4.6","toolTelemetry":{"properties":{},"metrics":{}}},"id":"3","timestamp":"2026-03-03T00:00:02Z","parentId":""}`,
	}
	path := writeTestJSONL(t, lines)

	model := ExtractModelFromTranscript(context.Background(), path)
	if model != "gpt-4.1" {
		t.Errorf("ExtractModelFromTranscript() = %q, want %q (model_change takes precedence)", model, "gpt-4.1")
	}
}

func TestExtractModelFromTranscript_MultipleModelChanges(t *testing.T) {
	t.Parallel()

	lines := []string{
		`{"type":"session.start","data":{"sessionId":"abc123"},"id":"1","timestamp":"2026-03-03T00:00:00Z","parentId":""}`,
		`{"type":"session.model_change","data":{"newModel":"gpt-4.1"},"id":"2","timestamp":"2026-03-03T00:00:01Z","parentId":"1"}`,
		`{"type":"user.message","data":{"content":"switch model"},"id":"3","timestamp":"2026-03-03T00:00:02Z","parentId":""}`,
		`{"type":"session.model_change","data":{"previousModel":"gpt-4.1","newModel":"claude-sonnet-4.6"},"id":"4","timestamp":"2026-03-03T00:00:03Z","parentId":"1"}`,
	}
	path := writeTestJSONL(t, lines)

	model := ExtractModelFromTranscript(context.Background(), path)
	if model != testModelSonnet {
		t.Errorf("ExtractModelFromTranscript() = %q, want %q (last model change)", model, testModelSonnet)
	}
}

func TestExtractModelFromEvents(t *testing.T) {
	t.Parallel()

	t.Run("returns last model", func(t *testing.T) {
		t.Parallel()
		lines := []string{
			`{"type":"session.model_change","data":{"newModel":"gpt-4.1"},"id":"1","timestamp":"2026-03-03T00:00:00Z","parentId":""}`,
			`{"type":"session.model_change","data":{"newModel":"claude-sonnet-4.6"},"id":"2","timestamp":"2026-03-03T00:00:01Z","parentId":""}`,
		}
		content := strings.Join(lines, "\n") + "\n"
		events, _ := parseEventsFromBytes([]byte(content)) //nolint:errcheck // test input is always valid
		model := extractModelFromEvents(events)
		if model != testModelSonnet {
			t.Errorf("extractModelFromEvents() = %q, want %q", model, testModelSonnet)
		}
	})

	t.Run("returns empty for no events", func(t *testing.T) {
		t.Parallel()
		model := extractModelFromEvents(nil)
		if model != "" {
			t.Errorf("extractModelFromEvents(nil) = %q, want empty", model)
		}
	})

	t.Run("skips empty newModel", func(t *testing.T) {
		t.Parallel()
		lines := []string{
			`{"type":"session.model_change","data":{"newModel":"gpt-4.1"},"id":"1","timestamp":"2026-03-03T00:00:00Z","parentId":""}`,
			`{"type":"session.model_change","data":{"newModel":""},"id":"2","timestamp":"2026-03-03T00:00:01Z","parentId":""}`,
		}
		content := strings.Join(lines, "\n") + "\n"
		events, _ := parseEventsFromBytes([]byte(content)) //nolint:errcheck // test input is always valid
		model := extractModelFromEvents(events)
		if model != "gpt-4.1" {
			t.Errorf("extractModelFromEvents() = %q, want %q (skip empty newModel)", model, "gpt-4.1")
		}
	})

	t.Run("fallback skips empty and malformed model in tool events", func(t *testing.T) {
		t.Parallel()
		lines := []string{
			// tool.execution_complete with empty model field
			`{"type":"tool.execution_complete","data":{"toolCallId":"tc1","model":"","toolTelemetry":{"properties":{},"metrics":{}}},"id":"1","timestamp":"2026-03-03T00:00:00Z","parentId":""}`,
			// tool.execution_complete with no model field at all (malformed data)
			`{"type":"tool.execution_complete","data":{"toolCallId":"tc2","toolTelemetry":{"properties":{},"metrics":{}}},"id":"2","timestamp":"2026-03-03T00:00:01Z","parentId":""}`,
		}
		content := strings.Join(lines, "\n") + "\n"
		events, _ := parseEventsFromBytes([]byte(content)) //nolint:errcheck // test input is always valid
		model := extractModelFromEvents(events)
		if model != "" {
			t.Errorf("extractModelFromEvents() = %q, want empty (no valid model in fallback)", model)
		}
	})
}
