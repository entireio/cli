package opencode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

// testExportJSON is an export JSON transcript with 4 messages.
var testExportJSON = func() string {
	session := ExportSession{
		Info: SessionInfo{ID: "test-session-id"},
		Messages: []ExportMessage{
			{
				Info: MessageInfo{ID: "msg-1", Role: "user", Time: Time{Created: 1708300000}},
				Parts: []Part{
					{Type: "text", Text: "Fix the bug in main.go"},
				},
			},
			{
				Info: MessageInfo{
					ID: "msg-2", Role: "assistant",
					Time:   Time{Created: 1708300001, Completed: 1708300005},
					Tokens: &Tokens{Input: 150, Output: 80, Reasoning: 10, Cache: Cache{Read: 5, Write: 15}},
					Cost:   0.003,
				},
				Parts: []Part{
					{Type: "text", Text: "I'll fix the bug."},
					{Type: "tool", Tool: "edit", CallID: "call-1", State: &ToolState{Status: "completed", Input: map[string]any{"filePath": "main.go"}, Output: "Applied edit"}},
				},
			},
			{
				Info: MessageInfo{ID: "msg-3", Role: "user", Time: Time{Created: 1708300010}},
				Parts: []Part{
					{Type: "text", Text: "Also fix util.go"},
				},
			},
			{
				Info: MessageInfo{
					ID: "msg-4", Role: "assistant",
					Time:   Time{Created: 1708300011, Completed: 1708300015},
					Tokens: &Tokens{Input: 200, Output: 100, Reasoning: 5, Cache: Cache{Read: 10, Write: 20}},
					Cost:   0.005,
				},
				Parts: []Part{
					{Type: "tool", Tool: "write", CallID: "call-2", State: &ToolState{Status: "completed", Input: map[string]any{"filePath": "util.go"}, Output: "File written"}},
					{Type: "text", Text: "Done fixing util.go."},
				},
			},
		},
	}
	data, err := json.Marshal(session)
	if err != nil {
		panic(err)
	}
	return string(data)
}()

func writeTestTranscript(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test-session.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write test transcript: %v", err)
	}
	return path
}

func TestParseExportSession(t *testing.T) {
	t.Parallel()

	session, err := ParseExportSession([]byte(testExportJSON))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, session, "expected non-nil session")
	if len(session.Messages) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(session.Messages))
	}
	if session.Messages[0].Info.ID != "msg-1" {
		t.Errorf("expected first message ID 'msg-1', got %q", session.Messages[0].Info.ID)
	}
	if session.Messages[0].Info.Role != "user" {
		t.Errorf("expected first message role 'user', got %q", session.Messages[0].Info.Role)
	}
}

func TestParseExportSession_Empty(t *testing.T) {
	t.Parallel()

	session, err := ParseExportSession(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if session != nil {
		t.Errorf("expected nil for nil data, got %+v", session)
	}

	session, err = ParseExportSession([]byte(""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if session != nil {
		t.Errorf("expected nil for empty data, got %+v", session)
	}
}

func TestParseExportSession_InvalidJSON(t *testing.T) {
	t.Parallel()

	_, err := ParseExportSession([]byte("not json"))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestGetTranscriptPosition(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	path := writeTestTranscript(t, testExportJSON)

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
	path := writeTestTranscript(t, testExportJSON)

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

func TestExtractFilePaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state *ToolState
		want  []string
	}{
		{
			name:  "camelCase filePath from input",
			state: &ToolState{Input: map[string]any{"filePath": "/repo/main.go"}},
			want:  []string{"/repo/main.go"},
		},
		{
			name:  "path key from input",
			state: &ToolState{Input: map[string]any{"path": "/repo/main.go"}},
			want:  []string{"/repo/main.go"},
		},
		{
			name:  "filePath takes priority over path in input",
			state: &ToolState{Input: map[string]any{"filePath": "/a.go", "path": "/b.go"}},
			want:  []string{"/a.go"},
		},
		{
			name:  "empty input",
			state: &ToolState{Input: map[string]any{}},
			want:  nil,
		},
		{
			name:  "nil state",
			state: nil,
			want:  nil,
		},
		{
			name: "metadata files (apply_patch / codex)",
			state: &ToolState{
				Input: map[string]any{"patchText": "*** Begin Patch\n***"},
				Metadata: &ToolStateMetadata{
					Files: []ToolFileInfo{
						{FilePath: "/repo/main.go", RelativePath: "main.go"},
					},
				},
			},
			want: []string{"/repo/main.go"},
		},
		{
			name: "metadata files with multiple files",
			state: &ToolState{
				Input: map[string]any{"patchText": "..."},
				Metadata: &ToolStateMetadata{
					Files: []ToolFileInfo{
						{FilePath: "/repo/a.go"},
						{FilePath: "/repo/b.go"},
					},
				},
			},
			want: []string{"/repo/a.go", "/repo/b.go"},
		},
		{
			name: "metadata takes priority over input",
			state: &ToolState{
				Input: map[string]any{"filePath": "/input/file.go"},
				Metadata: &ToolStateMetadata{
					Files: []ToolFileInfo{
						{FilePath: "/meta/file.go"},
					},
				},
			},
			want: []string{"/meta/file.go"},
		},
		{
			name: "empty metadata falls back to input",
			state: &ToolState{
				Input:    map[string]any{"filePath": "/repo/main.go"},
				Metadata: &ToolStateMetadata{},
			},
			want: []string{"/repo/main.go"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractFilePaths(tt.state)
			if len(got) != len(tt.want) {
				t.Fatalf("extractFilePaths() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("extractFilePaths()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// testApplyPatchExportJSON simulates OpenCode with codex models that use apply_patch tool.
var testApplyPatchExportJSON = func() string {
	session := ExportSession{
		Info: SessionInfo{ID: "test-apply-patch"},
		Messages: []ExportMessage{
			{
				Info: MessageInfo{ID: "msg-1", Role: "user", Time: Time{Created: 1708300000}},
				Parts: []Part{
					{Type: "text", Text: "Fix the table layout"},
				},
			},
			{
				Info: MessageInfo{
					ID: "msg-2", Role: "assistant",
					Time:   Time{Created: 1708300001, Completed: 1708300005},
					Tokens: &Tokens{Input: 200, Output: 100, Cache: Cache{}},
				},
				Parts: []Part{
					{Type: "text", Text: "I'll fix the layout."},
					{
						Type: "tool", Tool: "apply_patch", CallID: "call-1",
						State: &ToolState{
							Status: "completed",
							Input:  map[string]any{"patchText": "*** Begin Patch\n*** Update File: /repo/layout.py\n@@\n-old\n+new\n*** End Patch"},
							Output: "Success. Updated the following files:\nM layout.py",
							Metadata: &ToolStateMetadata{
								Files: []ToolFileInfo{
									{FilePath: "/repo/layout.py", RelativePath: "layout.py"},
								},
							},
						},
					},
				},
			},
			{
				Info: MessageInfo{ID: "msg-3", Role: "user", Time: Time{Created: 1708300010}},
				Parts: []Part{
					{Type: "text", Text: "Also fix the resize handler"},
				},
			},
			{
				Info: MessageInfo{
					ID: "msg-4", Role: "assistant",
					Time:   Time{Created: 1708300011, Completed: 1708300015},
					Tokens: &Tokens{Input: 250, Output: 120, Cache: Cache{}},
				},
				Parts: []Part{
					{
						Type: "tool", Tool: "apply_patch", CallID: "call-2",
						State: &ToolState{
							Status: "completed",
							Input:  map[string]any{"patchText": "*** Begin Patch\n*** Update File: /repo/layout.py\n*** Update File: /repo/resize.py\n*** End Patch"},
							Output: "Success.",
							Metadata: &ToolStateMetadata{
								Files: []ToolFileInfo{
									{FilePath: "/repo/layout.py", RelativePath: "layout.py"},
									{FilePath: "/repo/resize.py", RelativePath: "resize.py"},
								},
							},
						},
					},
				},
			},
		},
	}
	data, err := json.Marshal(session)
	if err != nil {
		panic(err)
	}
	return string(data)
}()

func TestExtractModifiedFilesFromOffset_ApplyPatch(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	path := writeTestTranscript(t, testApplyPatchExportJSON)

	// From offset 0 — should find layout.py and resize.py (deduplicated)
	files, pos, err := ag.ExtractModifiedFilesFromOffset(path, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pos != 4 {
		t.Errorf("expected position 4, got %d", pos)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files (deduplicated), got %d: %v", len(files), files)
	}

	// From offset 2 — should find layout.py and resize.py from msg-4
	files, _, err = ag.ExtractModifiedFilesFromOffset(path, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files from offset 2, got %d: %v", len(files), files)
	}
}

func TestExtractModifiedFiles_ApplyPatch(t *testing.T) {
	t.Parallel()

	files, err := ExtractModifiedFiles([]byte(testApplyPatchExportJSON))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d: %v", len(files), files)
	}
	if files[0] != "/repo/layout.py" {
		t.Errorf("expected first file '/repo/layout.py', got %q", files[0])
	}
	if files[1] != "/repo/resize.py" {
		t.Errorf("expected second file '/repo/resize.py', got %q", files[1])
	}
}

// testCamelCaseExportJSON uses camelCase "filePath" keys matching real OpenCode export format.
var testCamelCaseExportJSON = func() string {
	session := ExportSession{
		Info: SessionInfo{ID: "test-camelcase"},
		Messages: []ExportMessage{
			{
				Info: MessageInfo{ID: "msg-1", Role: "user", Time: Time{Created: 1708300000}},
				Parts: []Part{
					{Type: "text", Text: "Fix the bug"},
				},
			},
			{
				Info: MessageInfo{ID: "msg-2", Role: "assistant", Time: Time{Created: 1708300001, Completed: 1708300005}},
				Parts: []Part{
					{Type: "tool", Tool: "write", CallID: "call-1", State: &ToolState{Status: "completed", Input: map[string]any{"filePath": "/repo/new_file.rb", "content": "puts 'hello'"}, Output: "Wrote file"}},
				},
			},
			{
				Info: MessageInfo{ID: "msg-3", Role: "user", Time: Time{Created: 1708300010}},
				Parts: []Part{
					{Type: "text", Text: "Now edit it"},
				},
			},
			{
				Info: MessageInfo{ID: "msg-4", Role: "assistant", Time: Time{Created: 1708300011, Completed: 1708300015}},
				Parts: []Part{
					{Type: "tool", Tool: "edit", CallID: "call-2", State: &ToolState{Status: "completed", Input: map[string]any{"filePath": "/repo/new_file.rb", "oldString": "hello", "newString": "world"}, Output: "Edit applied"}},
				},
			},
		},
	}
	data, err := json.Marshal(session)
	if err != nil {
		panic(err)
	}
	return string(data)
}()

func TestExtractModifiedFilesFromOffset_CamelCaseFilePath(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	path := writeTestTranscript(t, testCamelCaseExportJSON)

	files, pos, err := ag.ExtractModifiedFilesFromOffset(path, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pos != 4 {
		t.Errorf("expected position 4, got %d", pos)
	}
	// Both write (msg-2) and edit (msg-4) reference the same file, so deduplicated to 1
	if len(files) != 1 {
		t.Fatalf("expected 1 file (deduplicated), got %d: %v", len(files), files)
	}
	if files[0] != "/repo/new_file.rb" {
		t.Errorf("expected '/repo/new_file.rb', got %q", files[0])
	}

	// From offset 2 — should still find the edit in msg-4
	files, _, err = ag.ExtractModifiedFilesFromOffset(path, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d: %v", len(files), files)
	}
}

func TestCalculateTokenUsage(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}

	// From offset 0 — both assistant messages
	usage, err := ag.CalculateTokenUsage([]byte(testExportJSON), 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, usage, "expected non-nil usage")
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

	usage, err := ag.CalculateTokenUsage([]byte(testExportJSON), 2)
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

func TestCalculateTokenUsage_EmptyData(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}

	usage, err := ag.CalculateTokenUsage(nil, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage != nil {
		t.Errorf("expected nil usage for empty data, got %+v", usage)
	}
}

func TestChunkTranscript_SmallContent(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	content := []byte(testExportJSON)

	// maxSize larger than content — should return single chunk
	chunks, err := ag.ChunkTranscript(context.Background(), content, len(content)+1000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for small content, got %d", len(chunks))
	}
}

func TestChunkTranscript_SplitsLargeContent(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	content := []byte(testExportJSON)

	// Use a maxSize that forces splitting
	chunks, err := ag.ChunkTranscript(context.Background(), content, 500)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks for small maxSize, got %d", len(chunks))
	}

	// Each chunk should contain valid export JSON
	for i, chunk := range chunks {
		session, parseErr := ParseExportSession(chunk)
		if parseErr != nil {
			t.Fatalf("chunk %d: failed to parse: %v", i, parseErr)
		}
		if session == nil || len(session.Messages) == 0 {
			t.Errorf("chunk %d: expected at least 1 message", i)
		}
	}
}

func TestChunkTranscript_RoundTrip(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	content := []byte(testExportJSON)

	// Split into chunks
	chunks, err := ag.ChunkTranscript(context.Background(), content, 500)
	if err != nil {
		t.Fatalf("chunk error: %v", err)
	}

	// Reassemble
	reassembled, err := ag.ReassembleTranscript(chunks)
	if err != nil {
		t.Fatalf("reassemble error: %v", err)
	}

	// Parse both and compare messages
	original, parseErr := ParseExportSession(content)
	if parseErr != nil {
		t.Fatalf("failed to parse original: %v", parseErr)
	}
	result, parseErr := ParseExportSession(reassembled)
	if parseErr != nil {
		t.Fatalf("failed to parse reassembled: %v", parseErr)
	}

	if len(result.Messages) != len(original.Messages) {
		t.Fatalf("message count mismatch: %d vs %d", len(result.Messages), len(original.Messages))
	}
	for i, msg := range result.Messages {
		if msg.Info.ID != original.Messages[i].Info.ID {
			t.Errorf("message %d: ID mismatch %q vs %q", i, msg.Info.ID, original.Messages[i].Info.ID)
		}
	}
}

func TestChunkTranscript_EmptyContent(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}

	emptySession := ExportSession{Info: SessionInfo{ID: "empty"}, Messages: []ExportMessage{}}
	data, err := json.Marshal(emptySession)
	if err != nil {
		t.Fatalf("failed to marshal empty session: %v", err)
	}

	chunks, err := ag.ChunkTranscript(context.Background(), data, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Empty messages should return single chunk with the original content
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for empty messages, got %d", len(chunks))
	}
}

func TestReassembleTranscript_SingleChunk(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	content := []byte(testExportJSON)

	result, err := ag.ReassembleTranscript([][]byte{content})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the result is valid JSON
	session, parseErr := ParseExportSession(result)
	if parseErr != nil {
		t.Fatalf("failed to parse result: %v", parseErr)
	}
	if len(session.Messages) != 4 {
		t.Errorf("expected 4 messages, got %d", len(session.Messages))
	}
}

func TestReassembleTranscript_Empty(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}

	result, err := ag.ReassembleTranscript(nil)
	if err == nil {
		t.Fatal("expected error for nil chunks, got nil")
	}
	if result != nil {
		t.Errorf("expected nil result for nil chunks, got %d bytes", len(result))
	}
}

func TestExtractModifiedFiles(t *testing.T) {
	t.Parallel()

	files, err := ExtractModifiedFiles([]byte(testExportJSON))
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

func TestExtractAllUserPrompts(t *testing.T) {
	t.Parallel()

	prompts, err := ExtractAllUserPrompts([]byte(testExportJSON))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prompts) != 2 {
		t.Fatalf("expected 2 prompts, got %d: %v", len(prompts), prompts)
	}
	if prompts[0] != "Fix the bug in main.go" {
		t.Errorf("expected first prompt 'Fix the bug in main.go', got %q", prompts[0])
	}
	if prompts[1] != "Also fix util.go" {
		t.Errorf("expected second prompt 'Also fix util.go', got %q", prompts[1])
	}
}

func TestExtractAllUserPrompts_SystemReminderOnly(t *testing.T) {
	t.Parallel()

	session := ExportSession{
		Info: SessionInfo{ID: "test-sysreminder"},
		Messages: []ExportMessage{
			{
				Info: MessageInfo{ID: "msg-1", Role: "user"},
				Parts: []Part{
					{Type: "text", Text: "Fix the bug"},
				},
			},
			{
				Info: MessageInfo{ID: "msg-2", Role: "user"},
				Parts: []Part{
					{Type: "text", Text: "<system-reminder>\nAs you answer the user's questions, you can use the following context:\nContents of CLAUDE.md...\n</system-reminder>"},
				},
			},
			{
				Info: MessageInfo{ID: "msg-3", Role: "user"},
				Parts: []Part{
					{Type: "text", Text: "Now fix util.go"},
				},
			},
		},
	}
	data, err := json.Marshal(session)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	prompts, err := ExtractAllUserPrompts(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prompts) != 2 {
		t.Fatalf("expected 2 prompts (system-reminder excluded), got %d: %v", len(prompts), prompts)
	}
	if prompts[0] != "Fix the bug" {
		t.Errorf("expected 'Fix the bug', got %q", prompts[0])
	}
	if prompts[1] != "Now fix util.go" {
		t.Errorf("expected 'Now fix util.go', got %q", prompts[1])
	}
}

func TestExtractAllUserPrompts_SystemReminderMixed(t *testing.T) {
	t.Parallel()

	session := ExportSession{
		Info: SessionInfo{ID: "test-mixed"},
		Messages: []ExportMessage{
			{
				Info: MessageInfo{ID: "msg-1", Role: "user"},
				Parts: []Part{
					{Type: "text", Text: "Fix the bug\n<system-reminder>\nCLAUDE.md contents here\n</system-reminder>"},
				},
			},
		},
	}
	data, err := json.Marshal(session)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	prompts, err := ExtractAllUserPrompts(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prompts) != 1 {
		t.Fatalf("expected 1 prompt, got %d: %v", len(prompts), prompts)
	}
	if prompts[0] != "Fix the bug" {
		t.Errorf("expected 'Fix the bug', got %q", prompts[0])
	}
}

func TestExtractAllUserPrompts_SystemReminderWithWhitespace(t *testing.T) {
	t.Parallel()

	session := ExportSession{
		Info: SessionInfo{ID: "test-whitespace"},
		Messages: []ExportMessage{
			{
				Info: MessageInfo{ID: "msg-1", Role: "user"},
				Parts: []Part{
					{Type: "text", Text: "  \n<system-reminder>\nsome context\n</system-reminder>\n  "},
				},
			},
		},
	}
	data, err := json.Marshal(session)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	prompts, err := ExtractAllUserPrompts(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prompts) != 0 {
		t.Fatalf("expected 0 prompts (whitespace + system-reminder), got %d: %v", len(prompts), prompts)
	}
}

func TestIsSystemReminderOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "exact system-reminder",
			content: "<system-reminder>context here</system-reminder>",
			want:    true,
		},
		{
			name:    "with surrounding whitespace",
			content: "  \n<system-reminder>context</system-reminder>\n  ",
			want:    true,
		},
		{
			name:    "not system-reminder",
			content: "Fix the bug",
			want:    false,
		},
		{
			name:    "mixed content",
			content: "Fix the bug\n<system-reminder>context</system-reminder>",
			want:    false,
		},
		{
			name:    "empty",
			content: "",
			want:    false,
		},
		{
			// Multiple blocks with real content between them — starts with open tag
			// and ends with close tag, but is NOT system-reminder-only.
			name:    "real content between multiple blocks",
			content: "<system-reminder>a</system-reminder>Fix this<system-reminder>b</system-reminder>",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isSystemReminderOnly(tt.content); got != tt.want {
				t.Errorf("isSystemReminderOnly(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

func TestStripSystemReminders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "no system-reminder",
			content: "Fix the bug",
			want:    "Fix the bug",
		},
		{
			name:    "only system-reminder",
			content: "<system-reminder>context</system-reminder>",
			want:    "",
		},
		{
			name:    "mixed content",
			content: "Fix the bug\n<system-reminder>context</system-reminder>",
			want:    "Fix the bug",
		},
		{
			name:    "system-reminder in middle",
			content: "First part\n<system-reminder>context</system-reminder>\nSecond part",
			want:    "First part\n\nSecond part",
		},
		{
			name:    "multiple system-reminders",
			content: "<system-reminder>a</system-reminder>Fix this<system-reminder>b</system-reminder>",
			want:    "Fix this",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := stripSystemReminders(tt.content); got != tt.want {
				t.Errorf("stripSystemReminders(%q) = %q, want %q", tt.content, got, tt.want)
			}
		})
	}
}

// Compile-time interface checks are in transcript.go.
// Verify the unused import guard by referencing the agent package.
var _ = agent.AgentNameOpenCode
