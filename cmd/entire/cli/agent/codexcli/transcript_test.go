package codexcli

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const testBasicThreadID = "0199a213-81c0-7800-8aa1-bbab2a035a53"

const basicSessionJSONL = `{"type":"thread.started","thread_id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"reasoning","text":"**Scanning project structure**"}}
{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"bash -lc ls","aggregated_output":"","exit_code":null,"status":"in_progress"}}
{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"bash -lc ls","aggregated_output":"docs\nsrc\nREADME.md\n","exit_code":0,"status":"completed"}}
{"type":"item.completed","item":{"id":"item_2","type":"file_change","changes":[{"path":"src/main.go","kind":"update"},{"path":"src/util.go","kind":"add"}],"status":"completed"}}
{"type":"item.completed","item":{"id":"item_3","type":"agent_message","text":"Done. I updated src/main.go and created src/util.go."}}
{"type":"turn.completed","usage":{"input_tokens":24763,"cached_input_tokens":24448,"output_tokens":122}}
`

const multiTurnJSONL = `{"type":"thread.started","thread_id":"abc12345-def6-7890-abcd-ef1234567890"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"command_execution","command":"bash -lc cat README.md","aggregated_output":"# Project\n","exit_code":0,"status":"completed"}}
{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"I see the project structure. Let me make the changes."}}
{"type":"turn.completed","usage":{"input_tokens":1000,"cached_input_tokens":500,"output_tokens":50}}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_2","type":"file_change","changes":[{"path":"README.md","kind":"update"}],"status":"completed"}}
{"type":"item.completed","item":{"id":"item_3","type":"file_change","changes":[{"path":"docs/guide.md","kind":"add"}],"status":"completed"}}
{"type":"item.completed","item":{"id":"item_4","type":"agent_message","text":"Updated README.md and added docs/guide.md."}}
{"type":"turn.completed","usage":{"input_tokens":2000,"cached_input_tokens":1800,"output_tokens":100}}
`

const withErrorsJSONL = `{"type":"thread.started","thread_id":"err-session-001"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"command_execution","command":"bash -lc false","aggregated_output":"","exit_code":1,"status":"failed"}}
{"type":"item.completed","item":{"id":"item_1","type":"error","message":"command output truncated"}}
{"type":"turn.failed","error":{"message":"model response stream ended unexpectedly"}}
{"type":"error","message":"stream error: broken pipe"}
`

func TestParseEventStream_BasicSession(t *testing.T) {
	t.Parallel()

	session, err := ParseEventStream([]byte(basicSessionJSONL))
	if err != nil {
		t.Fatalf("ParseEventStream() error = %v", err)
	}

	if session.ThreadID != testBasicThreadID {
		t.Errorf("ThreadID = %q, want %q", session.ThreadID, testBasicThreadID)
	}

	if len(session.Messages) != 1 {
		t.Errorf("Messages count = %d, want 1", len(session.Messages))
	} else if session.Messages[0] != "Done. I updated src/main.go and created src/util.go." {
		t.Errorf("Messages[0] = %q, unexpected", session.Messages[0])
	}

	if len(session.Commands) != 1 {
		t.Errorf("Commands count = %d, want 1", len(session.Commands))
	} else {
		if session.Commands[0].Command != "bash -lc ls" {
			t.Errorf("Commands[0].Command = %q, want %q", session.Commands[0].Command, "bash -lc ls")
		}
		if session.Commands[0].ExitCode == nil || *session.Commands[0].ExitCode != 0 {
			t.Errorf("Commands[0].ExitCode unexpected")
		}
	}

	if len(session.ModifiedFiles) != 2 {
		t.Errorf("ModifiedFiles count = %d, want 2", len(session.ModifiedFiles))
	}

	wantFiles := map[string]bool{"src/main.go": true, "src/util.go": true}
	for _, f := range session.ModifiedFiles {
		if !wantFiles[f] {
			t.Errorf("unexpected modified file: %q", f)
		}
	}

	if session.TokenUsage.InputTokens != 24763 {
		t.Errorf("InputTokens = %d, want 24763", session.TokenUsage.InputTokens)
	}
	if session.TokenUsage.CacheReadTokens != 24448 {
		t.Errorf("CacheReadTokens = %d, want 24448", session.TokenUsage.CacheReadTokens)
	}
	if session.TokenUsage.OutputTokens != 122 {
		t.Errorf("OutputTokens = %d, want 122", session.TokenUsage.OutputTokens)
	}
	if session.TokenUsage.APICallCount != 1 {
		t.Errorf("APICallCount = %d, want 1", session.TokenUsage.APICallCount)
	}
}

func TestParseEventStream_MultiTurn(t *testing.T) {
	t.Parallel()

	session, err := ParseEventStream([]byte(multiTurnJSONL))
	if err != nil {
		t.Fatalf("ParseEventStream() error = %v", err)
	}

	if session.ThreadID != "abc12345-def6-7890-abcd-ef1234567890" {
		t.Errorf("ThreadID = %q, unexpected", session.ThreadID)
	}

	if len(session.Messages) != 2 {
		t.Errorf("Messages count = %d, want 2", len(session.Messages))
	}

	if session.TokenUsage.InputTokens != 3000 {
		t.Errorf("InputTokens = %d, want 3000", session.TokenUsage.InputTokens)
	}
	if session.TokenUsage.CacheReadTokens != 2300 {
		t.Errorf("CacheReadTokens = %d, want 2300", session.TokenUsage.CacheReadTokens)
	}
	if session.TokenUsage.OutputTokens != 150 {
		t.Errorf("OutputTokens = %d, want 150", session.TokenUsage.OutputTokens)
	}
	if session.TokenUsage.APICallCount != 2 {
		t.Errorf("APICallCount = %d, want 2", session.TokenUsage.APICallCount)
	}

	if len(session.ModifiedFiles) != 2 {
		t.Errorf("ModifiedFiles count = %d, want 2", len(session.ModifiedFiles))
	}

	wantFiles := map[string]bool{"README.md": true, "docs/guide.md": true}
	for _, f := range session.ModifiedFiles {
		if !wantFiles[f] {
			t.Errorf("unexpected modified file: %q", f)
		}
	}
}

func TestParseEventStream_WithErrors(t *testing.T) {
	t.Parallel()

	session, err := ParseEventStream([]byte(withErrorsJSONL))
	if err != nil {
		t.Fatalf("ParseEventStream() error = %v", err)
	}

	if session.ThreadID != "err-session-001" {
		t.Errorf("ThreadID = %q, unexpected", session.ThreadID)
	}

	if len(session.Errors) != 2 {
		t.Errorf("Errors count = %d, want 2", len(session.Errors))
	}

	if len(session.Commands) != 1 {
		t.Errorf("Commands count = %d, want 1", len(session.Commands))
	} else {
		if session.Commands[0].ExitCode == nil || *session.Commands[0].ExitCode != 1 {
			t.Errorf("Commands[0].ExitCode unexpected")
		}
		if session.Commands[0].Status != "failed" {
			t.Errorf("Commands[0].Status = %q, want %q", session.Commands[0].Status, "failed")
		}
	}

	if session.TokenUsage.APICallCount != 0 {
		t.Errorf("APICallCount = %d, want 0", session.TokenUsage.APICallCount)
	}
}

func TestParseEventStream_MalformedLines(t *testing.T) {
	t.Parallel()

	data := []byte(`{"type":"thread.started","thread_id":"test-123"}
not valid json at all
{"type":"turn.started"}
{"broken json
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"hello"}}
{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":10}}
`)

	session, err := ParseEventStream(data)
	if err != nil {
		t.Fatalf("ParseEventStream() error = %v", err)
	}

	if session.ThreadID != "test-123" {
		t.Errorf("ThreadID = %q, want %q", session.ThreadID, "test-123")
	}
	if len(session.Messages) != 1 {
		t.Errorf("Messages count = %d, want 1", len(session.Messages))
	}
	if session.TokenUsage.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", session.TokenUsage.InputTokens)
	}
}

func TestParseEventStream_UnknownEvents(t *testing.T) {
	t.Parallel()

	data := []byte(`{"type":"thread.started","thread_id":"test-456"}
{"type":"unknown.future.event","data":"something"}
{"type":"item.completed","item":{"id":"item_0","type":"future_item_type","data":"something"}}
{"type":"turn.completed","usage":{"input_tokens":50,"cached_input_tokens":0,"output_tokens":5}}
`)

	session, err := ParseEventStream(data)
	if err != nil {
		t.Fatalf("ParseEventStream() error = %v", err)
	}

	if session.ThreadID != "test-456" {
		t.Errorf("ThreadID = %q, want %q", session.ThreadID, "test-456")
	}
	if session.TokenUsage.InputTokens != 50 {
		t.Errorf("InputTokens = %d, want 50", session.TokenUsage.InputTokens)
	}
}

func TestParseEventStream_EmptyInput(t *testing.T) {
	t.Parallel()

	session, err := ParseEventStream([]byte{})
	if err != nil {
		t.Fatalf("ParseEventStream() error = %v", err)
	}

	if session.ThreadID != "" {
		t.Errorf("ThreadID = %q, want empty", session.ThreadID)
	}
	if len(session.Messages) != 0 {
		t.Errorf("Messages count = %d, want 0", len(session.Messages))
	}
}

func TestParseEventStream_FileChangeDeduplicate(t *testing.T) {
	t.Parallel()

	data := []byte(`{"type":"thread.started","thread_id":"dedup-test"}
{"type":"item.completed","item":{"id":"item_0","type":"file_change","changes":[{"path":"foo.go","kind":"update"}],"status":"completed"}}
{"type":"item.completed","item":{"id":"item_1","type":"file_change","changes":[{"path":"foo.go","kind":"update"},{"path":"bar.go","kind":"add"}],"status":"completed"}}
{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":5}}
`)

	session, err := ParseEventStream(data)
	if err != nil {
		t.Fatalf("ParseEventStream() error = %v", err)
	}

	if len(session.ModifiedFiles) != 2 {
		t.Errorf("ModifiedFiles count = %d, want 2 (deduplicated)", len(session.ModifiedFiles))
	}

	if len(session.FileChanges) != 3 {
		t.Errorf("FileChanges count = %d, want 3 (not deduplicated)", len(session.FileChanges))
	}
}

func TestExtractLastMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		messages []string
		want     string
	}{
		{"empty", nil, ""},
		{"single", []string{"hello"}, "hello"},
		{"multiple", []string{"first", "second", "third"}, "third"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := &ParsedSession{Messages: tt.messages}
			got := ExtractLastMessage(s)
			if got != tt.want {
				t.Errorf("ExtractLastMessage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseEventStreamFromFile(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "session.jsonl")
	if err := os.WriteFile(path, []byte(basicSessionJSONL), 0o600); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}

	session, err := ParseEventStreamFromFile(path)
	if err != nil {
		t.Fatalf("ParseEventStreamFromFile() error = %v", err)
	}

	if session.ThreadID != testBasicThreadID {
		t.Errorf("ThreadID = %q, unexpected", session.ThreadID)
	}
}

func TestGetTranscriptPosition(t *testing.T) {
	t.Parallel()

	t.Run("existing file", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		path := filepath.Join(tmpDir, "session.jsonl")
		if err := os.WriteFile(path, []byte(basicSessionJSONL), 0o600); err != nil {
			t.Fatalf("failed to write temp file: %v", err)
		}

		count, err := GetTranscriptPosition(path)
		if err != nil {
			t.Fatalf("GetTranscriptPosition() error = %v", err)
		}
		if count != 8 {
			t.Errorf("GetTranscriptPosition() = %d, want 8", count)
		}
	})

	t.Run("nonexistent file", func(t *testing.T) {
		t.Parallel()
		count, err := GetTranscriptPosition("nonexistent.jsonl")
		if err != nil {
			t.Fatalf("GetTranscriptPosition() error = %v", err)
		}
		if count != 0 {
			t.Errorf("GetTranscriptPosition() = %d, want 0", count)
		}
	})

	t.Run("empty path", func(t *testing.T) {
		t.Parallel()
		count, err := GetTranscriptPosition("")
		if err != nil {
			t.Fatalf("GetTranscriptPosition() error = %v", err)
		}
		if count != 0 {
			t.Errorf("GetTranscriptPosition() = %d, want 0", count)
		}
	})

	t.Run("permission error", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		path := filepath.Join(tmpDir, "noperm.jsonl")
		if err := os.WriteFile(path, []byte("data\n"), 0o000); err != nil {
			t.Fatalf("failed to write temp file: %v", err)
		}
		count, err := GetTranscriptPosition(path)
		if err == nil {
			t.Skip("file permissions not enforced (likely running as root)")
		}
		if count != 0 {
			t.Errorf("GetTranscriptPosition() = %d, want 0 on error", count)
		}
	})
}

func TestParseEventStreamFromFile_MissingFile(t *testing.T) {
	t.Parallel()

	_, err := ParseEventStreamFromFile("/nonexistent/path/session.jsonl")
	if err == nil {
		t.Error("ParseEventStreamFromFile() should return error for nonexistent file")
	}
}

func TestParseEventStream_EmptyLines(t *testing.T) {
	t.Parallel()

	data := []byte(`{"type":"thread.started","thread_id":"empty-lines-test"}` + "\n" +
		"\n" +
		`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":5}}` + "\n" +
		"\n")

	session, err := ParseEventStream(data)
	if err != nil {
		t.Fatalf("ParseEventStream() error = %v", err)
	}
	if session.ThreadID != "empty-lines-test" {
		t.Errorf("ThreadID = %q, want %q", session.ThreadID, "empty-lines-test")
	}
	if session.TokenUsage.InputTokens != 10 {
		t.Errorf("InputTokens = %d, want 10", session.TokenUsage.InputTokens)
	}
}

func TestParseEventStream_EmptyItemField(t *testing.T) {
	t.Parallel()

	// item.completed with no item field → nil RawMessage → len(raw) == 0
	data := []byte(`{"type":"thread.started","thread_id":"empty-item-test"}
{"type":"item.completed"}
{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":5}}
`)

	session, err := ParseEventStream(data)
	if err != nil {
		t.Fatalf("ParseEventStream() error = %v", err)
	}
	if session.ThreadID != "empty-item-test" {
		t.Errorf("ThreadID = %q, want %q", session.ThreadID, "empty-item-test")
	}
	if len(session.Messages) != 0 {
		t.Errorf("Messages count = %d, want 0", len(session.Messages))
	}
}

func TestParseEventStream_InvalidItemEnvelope(t *testing.T) {
	t.Parallel()

	// item field is a JSON number, not an object — envelope unmarshal fails
	data := []byte(`{"type":"thread.started","thread_id":"bad-envelope-test"}
{"type":"item.completed","item":42}
{"type":"item.completed","item":"not an object"}
{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":5}}
`)

	session, err := ParseEventStream(data)
	if err != nil {
		t.Fatalf("ParseEventStream() error = %v", err)
	}
	if session.ThreadID != "bad-envelope-test" {
		t.Errorf("ThreadID = %q, want %q", session.ThreadID, "bad-envelope-test")
	}
	if len(session.Messages) != 0 {
		t.Errorf("Messages count = %d, want 0", len(session.Messages))
	}
}

func TestExtractModifiedFiles(t *testing.T) {
	t.Parallel()

	t.Run("with files", func(t *testing.T) {
		t.Parallel()
		s := &ParsedSession{ModifiedFiles: []string{"foo.go", "bar.go"}}
		files := ExtractModifiedFiles(s)
		if len(files) != 2 {
			t.Errorf("ExtractModifiedFiles() count = %d, want 2", len(files))
		}
	})

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		s := &ParsedSession{}
		files := ExtractModifiedFiles(s)
		if len(files) != 0 {
			t.Errorf("ExtractModifiedFiles() count = %d, want 0", len(files))
		}
	})
}

func TestParseEvents_ScannerError(t *testing.T) {
	t.Parallel()

	data := `{"type":"thread.started","thread_id":"test"}` + "\n"
	r := &errorReader{
		data:  []byte(data),
		errAt: len(data) / 2,
		err:   errors.New("injected IO error"),
	}
	scanner := bufio.NewScanner(r)

	_, err := parseEvents(scanner)
	if err == nil {
		t.Error("parseEvents() should return error on scanner failure")
	}
}

func TestCountLines_ReadError(t *testing.T) {
	t.Parallel()

	data := "line1\nline2\n"
	r := &errorReader{
		data:  []byte(data),
		errAt: len(data) / 2,
		err:   errors.New("injected IO error"),
	}

	_, err := countLines(r)
	if err == nil {
		t.Error("countLines() should return error on reader failure")
	}
}
