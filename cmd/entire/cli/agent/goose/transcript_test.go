package goose

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// realExportFixture is a genuine `goose session export --format json` document
// produced by goose v1.46.0. It is the regression guard for the field names this
// package depends on — in particular that the message array is "conversation".
const realExportFixture = "testdata/real_export_v1_46_0.json"

// writeTranscript materializes sampleExport in a temp dir and returns its path.
func writeTranscript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	if err := os.WriteFile(path, []byte(sampleExport), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return path
}

// sampleExport is a hand-built export exercising every content block type.
const sampleExport = `{
  "id": "20260819_1",
  "working_dir": "/repo",
  "model_config": {"model": "claude-sonnet-4"},
  "usage": {"input_tokens": 10, "output_tokens": 2, "total_tokens": 12,
            "cache_read_input_tokens": 3, "cache_write_input_tokens": 4},
  "accumulated_usage": {"input_tokens": 100, "output_tokens": 20, "total_tokens": 120,
            "cache_read_input_tokens": 30, "cache_write_input_tokens": 40},
  "custom_future_key": {"kept": true},
  "conversation": [
    {"id": "m0", "role": "user", "content": [{"type": "text", "text": "first prompt"}]},
    {"id": "m1", "role": "assistant", "content": [
      {"type": "text", "text": "working on it"},
      {"type": "toolRequest", "id": "t1", "toolCall": {"status": "success",
        "value": {"name": "developer__text_editor", "arguments": {"path": "a.txt"}}}}
    ]},
    {"id": "m2", "role": "user", "content": [{"type": "toolResponse", "text": "ignored"}]},
    {"id": "m3", "role": "user", "content": [{"type": "text", "text": "second prompt"}]},
    {"id": "m4", "role": "assistant", "content": [
      {"type": "toolRequest", "id": "t2", "toolCall": {"status": "success",
        "value": {"name": "shell", "arguments": {"command": "ls"}}}},
      {"type": "toolRequest", "id": "t3", "toolCall": {"status": "success",
        "value": {"name": "text_editor", "arguments": {"path": "b.txt"}}}}
    ]}
  ]
}`

func TestRealExportFixture_ParsesWithExpectedShape(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(realExportFixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	session, err := ParseExportSession(data)
	if err != nil {
		t.Fatalf("parse real export: %v", err)
	}

	// The single most breakable assumption: the array is named "conversation".
	// If Goose ever renames it, this fails loudly instead of yielding an
	// empty transcript.
	if len(session.Conversation) == 0 {
		t.Fatal("real export produced an empty conversation; the field may have been renamed")
	}
	if session.ID == "" {
		t.Error("expected a session id in the real export")
	}
	if session.Accumulated == nil {
		t.Fatal("expected accumulated_usage in the real export")
	}
	// Guards the export-vs-SQLite spelling difference documented in AGENT.md:
	// the export uses cache_read_input_tokens, the DB column is cache_read_tokens.
	if session.Accumulated.CacheReadTokens == 0 {
		t.Error("cache_read_input_tokens did not decode; check the export field spelling")
	}
}

func TestGetTranscriptPosition(t *testing.T) {
	t.Parallel()

	a := &GooseAgent{}
	path := writeTranscript(t)

	got, err := a.GetTranscriptPosition(path)
	if err != nil {
		t.Fatalf("GetTranscriptPosition: %v", err)
	}
	if got != 5 {
		t.Errorf("position = %d, want 5", got)
	}
}

func TestGetTranscriptPosition_MissingFileIsZero(t *testing.T) {
	t.Parallel()

	a := &GooseAgent{}
	got, err := a.GetTranscriptPosition(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("expected missing file to be non-fatal, got %v", err)
	}
	if got != 0 {
		t.Errorf("position = %d, want 0", got)
	}
}

func TestExtractModifiedFilesFromOffset(t *testing.T) {
	t.Parallel()

	a := &GooseAgent{}
	path := writeTranscript(t)

	t.Run("from start", func(t *testing.T) {
		t.Parallel()
		files, pos, err := a.ExtractModifiedFilesFromOffset(path, 0)
		if err != nil {
			t.Fatalf("extract: %v", err)
		}
		if pos != 5 {
			t.Errorf("position = %d, want 5", pos)
		}
		want := []string{"a.txt", "b.txt"}
		if len(files) != len(want) {
			t.Fatalf("files = %v, want %v", files, want)
		}
		for i := range want {
			if files[i] != want[i] {
				t.Errorf("files[%d] = %q, want %q", i, files[i], want[i])
			}
		}
	})

	t.Run("from offset skips earlier edits", func(t *testing.T) {
		t.Parallel()
		files, _, err := a.ExtractModifiedFilesFromOffset(path, 4)
		if err != nil {
			t.Fatalf("extract: %v", err)
		}
		if len(files) != 1 || files[0] != "b.txt" {
			t.Errorf("files = %v, want [b.txt]", files)
		}
	})

	t.Run("offset past end is empty, not a panic", func(t *testing.T) {
		t.Parallel()
		files, _, err := a.ExtractModifiedFilesFromOffset(path, 999)
		if err != nil {
			t.Fatalf("extract: %v", err)
		}
		if len(files) != 0 {
			t.Errorf("files = %v, want empty", files)
		}
	})
}

// A shell command is not a file edit; only editor tools contribute paths.
func TestExtractModifiedFiles_IgnoresShellTool(t *testing.T) {
	t.Parallel()

	const onlyShell = `{"conversation":[
      {"id":"m0","role":"assistant","content":[
        {"type":"toolRequest","id":"t","toolCall":{"status":"success",
          "value":{"name":"developer__shell","arguments":{"command":"rm -rf x"}}}}]}]}`

	files, err := ExtractModifiedFiles([]byte(onlyShell))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("files = %v, want empty (shell is not a file edit)", files)
	}
}

// The namespaced and bare spellings of a tool name must resolve identically.
// The Almanac records a live-verified case where the vendor's documented
// namespaced name never matched what was actually delivered.
func TestIsFileTool_ToleratesNamespacing(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		"developer__text_editor": true,
		"text_editor":            true,
		"TEXT_EDITOR":            true,
		"Edit":                   true, // via `goose session import` of a Claude transcript
		"Write":                  true,
		"developer__shell":       false,
		"shell":                  false,
		"":                       false,
	}
	for name, want := range cases {
		if got := isFileTool(name); got != want {
			t.Errorf("isFileTool(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestExtractPrompts_SkipsToolResponses(t *testing.T) {
	t.Parallel()

	a := &GooseAgent{}
	path := writeTranscript(t)

	prompts, err := a.ExtractPrompts(path, 0)
	if err != nil {
		t.Fatalf("ExtractPrompts: %v", err)
	}
	// m2 is a user-role message carrying only a toolResponse. Goose attributes
	// tool output to the user role, so it must not be read as a prompt.
	want := []string{"first prompt", "second prompt"}
	if len(prompts) != len(want) {
		t.Fatalf("prompts = %v, want %v", prompts, want)
	}
	for i := range want {
		if prompts[i] != want[i] {
			t.Errorf("prompts[%d] = %q, want %q", i, prompts[i], want[i])
		}
	}
}

func TestCalculateTokenUsage_PrefersAccumulated(t *testing.T) {
	t.Parallel()

	a := &GooseAgent{}
	usage, err := a.CalculateTokenUsage([]byte(sampleExport), 0)
	if err != nil {
		t.Fatalf("CalculateTokenUsage: %v", err)
	}
	if usage.InputTokens != 100 || usage.OutputTokens != 20 {
		t.Errorf("got input=%d output=%d, want 100/20 from accumulated_usage",
			usage.InputTokens, usage.OutputTokens)
	}
	if usage.CacheReadTokens != 30 || usage.CacheCreationTokens != 40 {
		t.Errorf("got cacheRead=%d cacheWrite=%d, want 30/40",
			usage.CacheReadTokens, usage.CacheCreationTokens)
	}
}

// Documents the interface deviation: Goose reports only session-level totals, so
// the offset argument cannot scope the result.
func TestCalculateTokenUsage_OffsetIsIgnored(t *testing.T) {
	t.Parallel()

	a := &GooseAgent{}
	atZero, err := a.CalculateTokenUsage([]byte(sampleExport), 0)
	if err != nil {
		t.Fatalf("CalculateTokenUsage: %v", err)
	}
	atFour, err := a.CalculateTokenUsage([]byte(sampleExport), 4)
	if err != nil {
		t.Fatalf("CalculateTokenUsage: %v", err)
	}
	if *atZero != *atFour {
		t.Error("expected identical totals; Goose has no per-message usage to scope by")
	}
}

func TestCalculateTokenUsage_NoUsageBlock(t *testing.T) {
	t.Parallel()

	a := &GooseAgent{}
	usage, err := a.CalculateTokenUsage([]byte(`{"conversation":[]}`), 0)
	if err != nil {
		t.Fatalf("CalculateTokenUsage: %v", err)
	}
	if usage.InputTokens != 0 || usage.OutputTokens != 0 {
		t.Errorf("expected zero usage, got %+v", usage)
	}
}

func TestExtractModel(t *testing.T) {
	t.Parallel()

	a := &GooseAgent{}
	model, err := a.ExtractModel([]byte(sampleExport))
	if err != nil {
		t.Fatalf("ExtractModel: %v", err)
	}
	if model != "claude-sonnet-4" {
		t.Errorf("model = %q, want claude-sonnet-4", model)
	}
}

func TestSliceFromMessage(t *testing.T) {
	t.Parallel()

	t.Run("preserves unmodelled envelope keys", func(t *testing.T) {
		t.Parallel()
		out, err := SliceFromMessage([]byte(sampleExport), 3)
		if err != nil {
			t.Fatalf("SliceFromMessage: %v", err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(out, &fields); err != nil {
			t.Fatalf("unmarshal slice: %v", err)
		}
		// custom_future_key is not named by ExportSession. It must survive, or a
		// future Goose field would be silently dropped on every checkpoint.
		if _, ok := fields["custom_future_key"]; !ok {
			t.Error("unmodelled top-level key was dropped by slicing")
		}
		var messages []json.RawMessage
		if err := json.Unmarshal(fields[conversationKey], &messages); err != nil {
			t.Fatalf("unmarshal conversation: %v", err)
		}
		if len(messages) != 2 {
			t.Errorf("sliced to %d messages, want 2", len(messages))
		}
	})

	t.Run("offset past end returns nil", func(t *testing.T) {
		t.Parallel()
		out, err := SliceFromMessage([]byte(sampleExport), 999)
		if err != nil {
			t.Fatalf("SliceFromMessage: %v", err)
		}
		if out != nil {
			t.Errorf("expected nil, got %s", out)
		}
	})

	t.Run("zero offset returns input unchanged", func(t *testing.T) {
		t.Parallel()
		out, err := SliceFromMessage([]byte(sampleExport), 0)
		if err != nil {
			t.Fatalf("SliceFromMessage: %v", err)
		}
		if string(out) != sampleExport {
			t.Error("zero offset should return the input unchanged")
		}
	})
}

func TestToolDetail(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args string
		want string
	}{
		{"command wins", `{"command":"ls -la","path":"x"}`, "ls -la"},
		{"path", `{"path":"a.txt"}`, "a.txt"},
		{"file_path fallback", `{"file_path":"b.txt"}`, "b.txt"},
		{"empty", `{}`, ""},
		{"malformed is empty, not an error", `not json`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ToolDetail(json.RawMessage(tc.args)); got != tc.want {
				t.Errorf("ToolDetail(%s) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestParseExportSession_EmptyIsNotAnError(t *testing.T) {
	t.Parallel()

	session, err := ParseExportSession(nil)
	if err != nil {
		t.Fatalf("expected nil error for empty input, got %v", err)
	}
	if session != nil {
		t.Error("expected nil session for empty input")
	}
}
