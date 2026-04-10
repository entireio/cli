package codex

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const sampleRollout = `{"timestamp":"2026-03-25T11:31:11.752Z","type":"session_meta","payload":{"id":"019d24c3","timestamp":"2026-03-25T11:31:10.922Z","cwd":"/tmp/repo","originator":"codex_exec","cli_version":"0.116.0","source":"exec"}}
{"timestamp":"2026-03-25T11:31:11.754Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1"}}
{"timestamp":"2026-03-25T11:31:11.754Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Create a file called hello.txt"}]}}
{"timestamp":"2026-03-25T11:31:12.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":5000,"cached_input_tokens":4000,"output_tokens":100,"reasoning_output_tokens":20,"total_tokens":5120}}}}
{"timestamp":"2026-03-25T11:31:13.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Creating the file now."}]}}
{"timestamp":"2026-03-25T11:31:14.000Z","type":"response_item","payload":{"type":"custom_tool_call","status":"completed","call_id":"call_1","name":"apply_patch","input":"*** Begin Patch\n*** Add File: hello.txt\n+Hello World\n*** End Patch\n"}}
{"timestamp":"2026-03-25T11:31:14.500Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_1","output":{"type":"text","text":"Success. Updated: A hello.txt"}}}
{"timestamp":"2026-03-25T11:31:15.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":10000,"cached_input_tokens":8000,"output_tokens":200,"reasoning_output_tokens":50,"total_tokens":10250}}}}
{"timestamp":"2026-03-25T11:31:16.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Now create docs/readme.md too"}]}}
{"timestamp":"2026-03-25T11:31:17.000Z","type":"response_item","payload":{"type":"custom_tool_call","status":"completed","call_id":"call_2","name":"apply_patch","input":"*** Begin Patch\n*** Add File: docs/readme.md\n+# Readme\n*** Update File: hello.txt\n-Hello World\n+Hello World!\n*** End Patch\n"}}
{"timestamp":"2026-03-25T11:31:18.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":15000,"cached_input_tokens":12000,"output_tokens":300,"reasoning_output_tokens":80,"total_tokens":15380}}}}
{"timestamp":"2026-03-25T11:31:19.000Z","type":"event_msg","payload":{"type":"task_complete"}}
`

func writeSampleRollout(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(sampleRollout), 0o600))
	return path
}

func TestGetTranscriptPosition(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	path := writeSampleRollout(t)

	pos, err := ag.GetTranscriptPosition(path)
	require.NoError(t, err)
	require.Equal(t, 12, pos)
}

func TestGetTranscriptPosition_EmptyPath(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}

	pos, err := ag.GetTranscriptPosition("")
	require.NoError(t, err)
	require.Equal(t, 0, pos)
}

func TestGetTranscriptPosition_NonexistentFile(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}

	pos, err := ag.GetTranscriptPosition("/nonexistent/file.jsonl")
	require.NoError(t, err)
	require.Equal(t, 0, pos)
}

func TestExtractModifiedFilesFromOffset(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	path := writeSampleRollout(t)

	// From beginning — should find all files
	files, pos, err := ag.ExtractModifiedFilesFromOffset(path, 0)
	require.NoError(t, err)
	require.Equal(t, 12, pos)
	require.ElementsMatch(t, []string{"hello.txt", "docs/readme.md"}, files)
}

func TestExtractModifiedFilesFromOffset_WithOffset(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	path := writeSampleRollout(t)

	// Skip first 7 lines (past the first apply_patch) — should only find second patch files
	files, pos, err := ag.ExtractModifiedFilesFromOffset(path, 7)
	require.NoError(t, err)
	require.Equal(t, 12, pos)
	require.ElementsMatch(t, []string{"docs/readme.md", "hello.txt"}, files)
}

func TestExtractModifiedFilesFromOffset_PastEnd(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	path := writeSampleRollout(t)

	files, pos, err := ag.ExtractModifiedFilesFromOffset(path, 100)
	require.NoError(t, err)
	require.Equal(t, 12, pos)
	require.Empty(t, files)
}

func TestCalculateTokenUsage(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}

	// From offset 0 (no baseline), should return full cumulative total
	usage, err := ag.CalculateTokenUsage([]byte(sampleRollout), 0)
	require.NoError(t, err)
	require.NotNil(t, usage)

	require.Equal(t, 15000, usage.InputTokens)
	require.Equal(t, 12000, usage.CacheReadTokens)
	require.Equal(t, 380, usage.OutputTokens) // 300 + 80 reasoning
	require.Equal(t, 3, usage.APICallCount)
}

func TestCalculateTokenUsage_WithOffset(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}

	// Skip past first token_count (line 4) — baseline is {5000, 4000, 120}
	// Last after offset is {15000, 12000, 380} → delta = {10000, 8000, 260}
	usage, err := ag.CalculateTokenUsage([]byte(sampleRollout), 4)
	require.NoError(t, err)
	require.NotNil(t, usage)

	require.Equal(t, 10000, usage.InputTokens)    // 15000 - 5000
	require.Equal(t, 8000, usage.CacheReadTokens) // 12000 - 4000
	require.Equal(t, 260, usage.OutputTokens)     // (300+80) - (100+20)
	require.Equal(t, 2, usage.APICallCount)
}

func TestCalculateTokenUsage_NoData(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}

	usage, err := ag.CalculateTokenUsage([]byte(`{"timestamp":"t","type":"session_meta","payload":{}}`), 0)
	require.NoError(t, err)
	require.Nil(t, usage)
}

func TestExtractPrompts(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	path := writeSampleRollout(t)

	prompts, err := ag.ExtractPrompts(path, 0)
	require.NoError(t, err)
	require.Equal(t, []string{
		"Create a file called hello.txt",
		"Now create docs/readme.md too",
	}, prompts)
}

func TestExtractPrompts_WithOffset(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	path := writeSampleRollout(t)

	// Skip past first user message (line 3)
	prompts, err := ag.ExtractPrompts(path, 8)
	require.NoError(t, err)
	require.Equal(t, []string{"Now create docs/readme.md too"}, prompts)
}

func TestExtractPrompts_NonexistentFile(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}

	prompts, err := ag.ExtractPrompts("/nonexistent/file.jsonl", 0)
	require.NoError(t, err)
	require.Nil(t, prompts)
}

func TestExtractFilesFromApplyPatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "single add",
			input: "*** Begin Patch\n*** Add File: hello.txt\n+content\n*** End Patch",
			want:  []string{"hello.txt"},
		},
		{
			name:  "add and update",
			input: "*** Begin Patch\n*** Add File: a.txt\n+x\n*** Update File: b.txt\n-old\n+new\n*** End Patch",
			want:  []string{"a.txt", "b.txt"},
		},
		{
			name:  "delete",
			input: "*** Begin Patch\n*** Delete File: old.txt\n*** End Patch",
			want:  []string{"old.txt"},
		},
		{
			name:  "deduplicates",
			input: "*** Add File: a.txt\n*** Update File: a.txt",
			want:  []string{"a.txt"},
		},
		{
			name:  "no matches",
			input: "some random text",
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractFilesFromApplyPatch(tt.input)
			if tt.want == nil {
				require.Nil(t, got)
			} else {
				require.Equal(t, tt.want, got)
			}
		})
	}
}

func TestSplitJSONL(t *testing.T) {
	t.Parallel()

	input := "{\"a\":1}\n{\"b\":2}\n\n{\"c\":3}\n"
	lines := splitJSONL([]byte(input))
	require.Len(t, lines, 3)
	require.Contains(t, string(lines[0]), `"a"`)
	require.Contains(t, string(lines[2]), `"c"`)
}

func TestSanitizeRestoredTranscript_StripsEncryptedItems(t *testing.T) {
	t.Parallel()

	input := []byte(`{"timestamp":"2026-03-25T11:31:11.752Z","type":"session_meta","payload":{"id":"019d24c3","timestamp":"2026-03-25T11:31:10.922Z","cwd":"/tmp/repo","originator":"codex_exec","cli_version":"0.116.0","source":"exec"}}
{"timestamp":"2026-03-25T11:31:11.754Z","type":"response_item","payload":{"type":"reasoning","summary":[{"text":"brief"}],"encrypted_content":"REDACTED"}}
{"timestamp":"2026-03-25T11:31:11.755Z","type":"response_item","payload":{"type":"compaction","encrypted_content":"REDACTED"}}
{"timestamp":"2026-03-25T11:31:11.756Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}
`)

	got := string(sanitizeRestoredTranscript(input))
	require.Contains(t, got, `"type":"reasoning"`)
	require.NotContains(t, got, `"encrypted_content":"REDACTED"`)
	require.NotContains(t, got, `"type":"compaction"`)
	require.Contains(t, got, `"type":"message"`)
}

func TestSanitizeRestoredTranscript_StripsEncryptedItemsFromCompactedHistory(t *testing.T) {
	t.Parallel()

	input := []byte(`{"timestamp":"2026-03-25T11:31:11.752Z","type":"session_meta","payload":{"id":"019d24c3","timestamp":"2026-03-25T11:31:10.922Z","cwd":"/tmp/repo","originator":"codex_exec","cli_version":"0.116.0","source":"exec"}}
{"timestamp":"2026-03-25T11:31:11.754Z","type":"compacted","payload":{"message":"","replacement_history":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},{"type":"reasoning","summary":[{"text":"brief"}],"encrypted_content":"REDACTED"},{"type":"compaction","encrypted_content":"REDACTED"},{"type":"compaction_summary","encrypted_content":"REDACTED"}]}}
`)

	got := string(sanitizeRestoredTranscript(input))
	require.Contains(t, got, `"type":"compacted"`)
	require.Contains(t, got, `"type":"reasoning"`)
	require.Contains(t, got, `"type":"message"`)
	require.NotContains(t, got, `"encrypted_content":"REDACTED"`)
	require.NotContains(t, got, `"type":"compaction"`)
	require.NotContains(t, got, `"type":"compaction_summary"`)
}
