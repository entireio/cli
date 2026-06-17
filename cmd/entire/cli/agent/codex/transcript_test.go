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
{"timestamp":"2026-03-25T11:31:12.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":5000,"cached_input_tokens":4000,"output_tokens":100,"reasoning_output_tokens":20,"total_tokens":5100}}}}
{"timestamp":"2026-03-25T11:31:13.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Creating the file now."}]}}
{"timestamp":"2026-03-25T11:31:14.000Z","type":"response_item","payload":{"type":"custom_tool_call","status":"completed","call_id":"call_1","name":"apply_patch","input":"*** Begin Patch\n*** Add File: hello.txt\n+Hello World\n*** End Patch\n"}}
{"timestamp":"2026-03-25T11:31:14.500Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_1","output":{"type":"text","text":"Success. Updated: A hello.txt"}}}
{"timestamp":"2026-03-25T11:31:15.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":10000,"cached_input_tokens":8000,"output_tokens":200,"reasoning_output_tokens":50,"total_tokens":10200}}}}
{"timestamp":"2026-03-25T11:31:16.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Now create docs/readme.md too"}]}}
{"timestamp":"2026-03-25T11:31:17.000Z","type":"response_item","payload":{"type":"custom_tool_call","status":"completed","call_id":"call_2","name":"apply_patch","input":"*** Begin Patch\n*** Add File: docs/readme.md\n+# Readme\n*** Update File: hello.txt\n-Hello World\n+Hello World!\n*** End Patch\n"}}
{"timestamp":"2026-03-25T11:31:18.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":15000,"cached_input_tokens":12000,"output_tokens":300,"reasoning_output_tokens":80,"total_tokens":15300}}}}
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

	require.Equal(t, 3000, usage.InputTokens)
	require.Equal(t, 12000, usage.CacheReadTokens)
	require.Equal(t, 300, usage.OutputTokens)
	require.Equal(t, 3, usage.APICallCount)
	require.Equal(t, 15300, usage.InputTokens+usage.CacheReadTokens+usage.OutputTokens)
}

func TestCalculateTokenUsage_WithOffset(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}

	// Skip past first token_count (line 4) — baseline is {5000, 4000, 100}
	// Last after offset is {15000, 12000, 300} → delta = {10000, 8000, 200}
	usage, err := ag.CalculateTokenUsage([]byte(sampleRollout), 4)
	require.NoError(t, err)
	require.NotNil(t, usage)

	require.Equal(t, 2000, usage.InputTokens)     // (15000 - 5000) - (12000 - 4000)
	require.Equal(t, 8000, usage.CacheReadTokens) // 12000 - 4000
	require.Equal(t, 200, usage.OutputTokens)     // 300 - 100
	require.Equal(t, 2, usage.APICallCount)
	require.Equal(t, 10200, usage.InputTokens+usage.CacheReadTokens+usage.OutputTokens)
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

func TestClassifyApplyPatchPaths(t *testing.T) {
	t.Parallel()

	added, modified, deleted := classifyApplyPatchPaths(
		"*** Begin Patch\n*** Add File: a.txt\n+hi\n*** Update File: b.txt\n@@\n-old\n+new\n*** Delete File: c.txt\n*** End Patch\n",
	)
	require.Equal(t, []string{"a.txt"}, added)
	require.Equal(t, []string{"b.txt"}, modified)
	require.Equal(t, []string{"c.txt"}, deleted)
}

func TestClassifyApplyPatchPaths_AddWinsOverUpdate(t *testing.T) {
	t.Parallel()
	// If a path appears with both Add and Update verbs (envelopes shouldn't do
	// this, but we're defensive), the more specific intent — Add — wins so
	// callers route the file into NewFiles rather than ModifiedFiles.
	added, modified, deleted := classifyApplyPatchPaths(
		"*** Add File: a.txt\n*** Update File: a.txt\n",
	)
	require.Equal(t, []string{"a.txt"}, added)
	require.Empty(t, modified)
	require.Empty(t, deleted)
}

func TestClassifyApplyPatchPaths_Empty(t *testing.T) {
	t.Parallel()
	added, modified, deleted := classifyApplyPatchPaths("*** Begin Patch\n*** End Patch\n")
	require.Empty(t, added)
	require.Empty(t, modified)
	require.Empty(t, deleted)
}

func TestClassifyApplyPatchPaths_MoveTo(t *testing.T) {
	t.Parallel()
	// Codex apply_patch encodes renames as "*** Update File: <old>\n*** Move
	// to: <new>". Both paths must be tracked: the source is being deleted
	// (renamed away), the destination is being created.
	added, modified, deleted := classifyApplyPatchPaths(
		"*** Begin Patch\n" +
			"*** Update File: src/old.rs\n" +
			"*** Move to: src/new.rs\n" +
			"@@\n-old\n+new\n" +
			"*** End Patch\n",
	)
	require.Equal(t, []string{"src/new.rs"}, added)
	require.Empty(t, modified)
	require.Equal(t, []string{"src/old.rs"}, deleted)
}

func TestClassifyApplyPatchPaths_MoveToWithSiblingHunks(t *testing.T) {
	t.Parallel()
	// A patch can mix Move-to renames with regular Add/Delete entries — the
	// Move handler must scope to the most recent Update File, not collapse
	// unrelated entries.
	added, modified, deleted := classifyApplyPatchPaths(
		"*** Begin Patch\n" +
			"*** Delete File: gone.txt\n" +
			"*** Update File: a.rs\n" +
			"*** Move to: b.rs\n" +
			"@@\n-x\n+y\n" +
			"*** Add File: brand-new.go\n" +
			"+package main\n" +
			"*** End Patch\n",
	)
	require.Equal(t, []string{"b.rs", "brand-new.go"}, added)
	require.Empty(t, modified)
	require.Equal(t, []string{"a.rs", "gone.txt"}, deleted)
}

// TestClassifyApplyPatchPaths_MoveDoesNotOverwriteStickyAdd pins the
// sticky-verb invariant against the Move-to handler. A path already
// classified as Add must survive a later Update+Move-to that names it
// as the source. Codex itself doesn't emit envelopes shaped like this,
// but the invariant is documented and we don't want a quiet downgrade
// if the grammar ever loosens.
func TestClassifyApplyPatchPaths_MoveDoesNotOverwriteStickyAdd(t *testing.T) {
	t.Parallel()
	added, modified, deleted := classifyApplyPatchPaths(
		"*** Begin Patch\n" +
			"*** Add File: foo.txt\n" +
			"+content\n" +
			"*** Update File: foo.txt\n" +
			"*** Move to: bar.txt\n" +
			"@@\n-x\n+y\n" +
			"*** End Patch\n",
	)
	require.Equal(t, []string{"bar.txt", "foo.txt"}, added)
	require.Empty(t, modified)
	require.Empty(t, deleted)
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
