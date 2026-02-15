package opencode

import (
	"encoding/json"
	"os"
	"testing"
)

func TestParseTranscript(t *testing.T) {
	t.Parallel()

	data := []byte(`{"info":{"id":"1","sessionID":"s1","role":"user","time":{"created":1000,"completed":1001}},"parts":[{"type":"text","text":"hello"}]}
{"info":{"id":"2","sessionID":"s1","role":"assistant","time":{"created":1002,"completed":1003}},"parts":[{"type":"text","text":"hi there"}]}
`)

	entries, err := ParseTranscript(data)
	if err != nil {
		t.Fatalf("ParseTranscript() error = %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("ParseTranscript() got %d entries, want 2", len(entries))
	}

	if entries[0].Info.Role != MessageRoleUser || entries[0].Info.ID != "1" {
		t.Errorf("First entry = %+v, want role=user, id=1", entries[0].Info)
	}

	if entries[1].Info.Role != MessageRoleAssistant || entries[1].Info.ID != "2" {
		t.Errorf("Second entry = %+v, want role=assistant, id=2", entries[1].Info)
	}
}

func TestParseTranscript_SkipsMalformed(t *testing.T) {
	t.Parallel()

	data := []byte(`{"info":{"id":"1","role":"user"},"parts":[]}
not valid json
{"info":{"id":"2","role":"assistant"},"parts":[]}
`)

	entries, err := ParseTranscript(data)
	if err != nil {
		t.Fatalf("ParseTranscript() error = %v", err)
	}

	if len(entries) != 2 {
		t.Errorf("ParseTranscript() got %d entries, want 2 (skipping malformed)", len(entries))
	}
}

func TestParseTranscript_Empty(t *testing.T) {
	t.Parallel()

	entries, err := ParseTranscript([]byte(""))
	if err != nil {
		t.Fatalf("ParseTranscript() error = %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("ParseTranscript('') got %d entries, want 0", len(entries))
	}
}

func TestExtractModifiedFiles_FromSummaryDiffs(t *testing.T) {
	t.Parallel()

	data := []byte(`{"info":{"id":"1","role":"assistant","summary":{"title":"edit files","diffs":[{"file":"foo.go","additions":5,"deletions":0},{"file":"bar.go","additions":3,"deletions":1}]}},"parts":[{"type":"text","text":"done"}]}
`)

	files := ExtractModifiedFiles(data)

	if len(files) != 2 {
		t.Fatalf("ExtractModifiedFiles() got %d files, want 2", len(files))
	}

	hasFile := func(name string) bool {
		for _, f := range files {
			if f == name {
				return true
			}
		}
		return false
	}

	if !hasFile("foo.go") {
		t.Error("ExtractModifiedFiles() missing foo.go")
	}
	if !hasFile("bar.go") {
		t.Error("ExtractModifiedFiles() missing bar.go")
	}
}

func TestExtractModifiedFiles_FromPatchParts(t *testing.T) {
	t.Parallel()

	data := []byte(`{"info":{"id":"1","role":"assistant"},"parts":[{"type":"patch","filePath":"main.go"}]}
`)

	files := ExtractModifiedFiles(data)

	if len(files) != 1 || files[0] != "main.go" {
		t.Errorf("ExtractModifiedFiles() = %v, want [main.go]", files)
	}
}

func TestExtractModifiedFiles_FromToolState(t *testing.T) {
	t.Parallel()

	toolInput, err := json.Marshal(map[string]string{"file_path": "utils.go"})
	if err != nil {
		t.Fatalf("failed to marshal tool input: %v", err)
	}
	entry := TranscriptEntry{
		Info: TranscriptEntryInfo{ID: "1", Role: MessageRoleAssistant},
		Parts: []TranscriptPart{
			{
				Type: PartTypeTool,
				Tool: ToolWrite,
				State: &TranscriptToolState{
					Input: toolInput,
				},
			},
		},
	}

	files := extractFilesFromEntry(&entry)
	if len(files) != 1 || files[0] != "utils.go" {
		t.Errorf("extractFilesFromEntry() = %v, want [utils.go]", files)
	}
}

func TestExtractModifiedFiles_Deduplication(t *testing.T) {
	t.Parallel()

	data := []byte(`{"info":{"id":"1","role":"assistant","summary":{"title":"edit","diffs":[{"file":"foo.go"},{"file":"foo.go"}]}},"parts":[]}
{"info":{"id":"2","role":"assistant","summary":{"title":"edit again","diffs":[{"file":"foo.go"}]}},"parts":[]}
`)

	files := ExtractModifiedFiles(data)
	if len(files) != 1 {
		t.Errorf("ExtractModifiedFiles() got %d files, want 1 (deduplicated)", len(files))
	}
}

func TestExtractModifiedFiles_IgnoresUserRole(t *testing.T) {
	t.Parallel()

	data := []byte(`{"info":{"id":"1","role":"user","summary":{"title":"user stuff","diffs":[{"file":"should-not-appear.go"}]}},"parts":[]}
`)

	files := ExtractModifiedFiles(data)
	if len(files) != 0 {
		t.Errorf("ExtractModifiedFiles() = %v, want empty (should ignore user role)", files)
	}
}

func TestExtractModifiedFiles_IgnoresNonModifyTools(t *testing.T) {
	t.Parallel()

	toolInput, err := json.Marshal(map[string]string{"command": "ls"})
	if err != nil {
		t.Fatalf("failed to marshal tool input: %v", err)
	}
	entry := TranscriptEntry{
		Info: TranscriptEntryInfo{ID: "1", Role: MessageRoleAssistant},
		Parts: []TranscriptPart{
			{
				Type: PartTypeTool,
				Tool: "bash",
				State: &TranscriptToolState{
					Input: toolInput,
				},
			},
		},
	}

	files := extractFilesFromEntry(&entry)
	if len(files) != 0 {
		t.Errorf("extractFilesFromEntry() = %v, want empty for non-modify tool", files)
	}
}

func TestExtractLastUserPrompt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data string
		want string
	}{
		{
			name: "single user message",
			data: `{"info":{"id":"1","role":"user"},"parts":[{"type":"text","text":"hello world"}]}`,
			want: "hello world",
		},
		{
			name: "multiple user messages returns last",
			data: `{"info":{"id":"1","role":"user"},"parts":[{"type":"text","text":"first"}]}
{"info":{"id":"2","role":"assistant"},"parts":[{"type":"text","text":"response"}]}
{"info":{"id":"3","role":"user"},"parts":[{"type":"text","text":"second"}]}`,
			want: "second",
		},
		{
			name: "multiple text parts joined",
			data: `{"info":{"id":"1","role":"user"},"parts":[{"type":"text","text":"part1"},{"type":"text","text":"part2"}]}`,
			want: "part1\npart2",
		},
		{
			name: "empty transcript",
			data: ``,
			want: "",
		},
		{
			name: "only assistant messages",
			data: `{"info":{"id":"1","role":"assistant"},"parts":[{"type":"text","text":"response"}]}`,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ExtractLastUserPrompt([]byte(tt.data))
			if got != tt.want {
				t.Errorf("ExtractLastUserPrompt() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractAllUserPrompts(t *testing.T) {
	t.Parallel()

	data := []byte(`{"info":{"id":"1","role":"user"},"parts":[{"type":"text","text":"first prompt"}]}
{"info":{"id":"2","role":"assistant"},"parts":[{"type":"text","text":"response"}]}
{"info":{"id":"3","role":"user"},"parts":[{"type":"text","text":"second prompt"}]}
`)

	prompts, err := ExtractAllUserPrompts(data)
	if err != nil {
		t.Fatalf("ExtractAllUserPrompts() error = %v", err)
	}

	if len(prompts) != 2 {
		t.Fatalf("ExtractAllUserPrompts() got %d prompts, want 2", len(prompts))
	}

	if prompts[0] != "first prompt" {
		t.Errorf("prompts[0] = %q, want %q", prompts[0], "first prompt")
	}
	if prompts[1] != "second prompt" {
		t.Errorf("prompts[1] = %q, want %q", prompts[1], "second prompt")
	}
}

func TestExtractLastAssistantMessage(t *testing.T) {
	t.Parallel()

	data := []byte(`{"info":{"id":"1","role":"user"},"parts":[{"type":"text","text":"hello"}]}
{"info":{"id":"2","role":"assistant"},"parts":[{"type":"text","text":"first response"}]}
{"info":{"id":"3","role":"user"},"parts":[{"type":"text","text":"another prompt"}]}
{"info":{"id":"4","role":"assistant"},"parts":[{"type":"text","text":"last response"}]}
`)

	msg, err := ExtractLastAssistantMessage(data)
	if err != nil {
		t.Fatalf("ExtractLastAssistantMessage() error = %v", err)
	}

	if msg != "last response" {
		t.Errorf("ExtractLastAssistantMessage() = %q, want %q", msg, "last response")
	}
}

func TestExtractLastAssistantMessage_NoAssistant(t *testing.T) {
	t.Parallel()

	data := []byte(`{"info":{"id":"1","role":"user"},"parts":[{"type":"text","text":"hello"}]}
`)

	msg, err := ExtractLastAssistantMessage(data)
	if err != nil {
		t.Fatalf("ExtractLastAssistantMessage() error = %v", err)
	}

	if msg != "" {
		t.Errorf("ExtractLastAssistantMessage() = %q, want empty", msg)
	}
}

func TestExtractFilePathFromToolState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state *TranscriptToolState
		want  string
	}{
		{
			name:  "nil state",
			state: nil,
			want:  "",
		},
		{
			name:  "empty input",
			state: &TranscriptToolState{},
			want:  "",
		},
		{
			name: "file_path key",
			state: &TranscriptToolState{
				Input: mustMarshalJSON(t, map[string]string{"file_path": "foo.go"}),
			},
			want: "foo.go",
		},
		{
			name: "path key",
			state: &TranscriptToolState{
				Input: mustMarshalJSON(t, map[string]string{"path": "bar.go"}),
			},
			want: "bar.go",
		},
		{
			name: "filePath key",
			state: &TranscriptToolState{
				Input: mustMarshalJSON(t, map[string]string{"filePath": "baz.go"}),
			},
			want: "baz.go",
		},
		{
			name: "filename key",
			state: &TranscriptToolState{
				Input: mustMarshalJSON(t, map[string]string{"filename": "qux.go"}),
			},
			want: "qux.go",
		},
		{
			name: "no matching key",
			state: &TranscriptToolState{
				Input: mustMarshalJSON(t, map[string]string{"command": "ls"}),
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractFilePathFromToolState(tt.state)
			if got != tt.want {
				t.Errorf("extractFilePathFromToolState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGetTranscriptPosition_WithFile(t *testing.T) {
	tmpDir := t.TempDir()
	transcriptPath := tmpDir + "/transcript.jsonl"

	content := `{"info":{"id":"1","role":"user"},"parts":[{"type":"text","text":"hello"}]}
{"info":{"id":"2","role":"assistant"},"parts":[{"type":"text","text":"hi"}]}
{"info":{"id":"3","role":"user"},"parts":[{"type":"text","text":"bye"}]}
`
	if err := os.WriteFile(transcriptPath, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	ag := &OpenCodeAgent{}
	pos, err := ag.GetTranscriptPosition(transcriptPath)
	if err != nil {
		t.Fatalf("GetTranscriptPosition() error = %v", err)
	}

	if pos != 3 {
		t.Errorf("GetTranscriptPosition() = %d, want 3", pos)
	}
}

func TestExtractModifiedFilesFromOffset_WithFile(t *testing.T) {
	tmpDir := t.TempDir()
	transcriptPath := tmpDir + "/transcript.jsonl"

	content := `{"info":{"id":"1","role":"assistant","summary":{"title":"first","diffs":[{"file":"old.go"}]}},"parts":[]}
{"info":{"id":"2","role":"assistant","summary":{"title":"second","diffs":[{"file":"new.go"}]}},"parts":[]}
{"info":{"id":"3","role":"assistant","summary":{"title":"third","diffs":[{"file":"newer.go"}]}},"parts":[]}
`
	if err := os.WriteFile(transcriptPath, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	ag := &OpenCodeAgent{}

	// From offset 0 (all lines)
	files, pos, err := ag.ExtractModifiedFilesFromOffset(transcriptPath, 0)
	if err != nil {
		t.Fatalf("ExtractModifiedFilesFromOffset(0) error = %v", err)
	}
	if len(files) != 3 {
		t.Errorf("ExtractModifiedFilesFromOffset(0) files = %v, want 3 files", files)
	}
	if pos != 3 {
		t.Errorf("ExtractModifiedFilesFromOffset(0) pos = %d, want 3", pos)
	}

	// From offset 1 (skip first line)
	files, pos, err = ag.ExtractModifiedFilesFromOffset(transcriptPath, 1)
	if err != nil {
		t.Fatalf("ExtractModifiedFilesFromOffset(1) error = %v", err)
	}
	if len(files) != 2 {
		t.Errorf("ExtractModifiedFilesFromOffset(1) files = %v, want 2 files", files)
	}
	if pos != 3 {
		t.Errorf("ExtractModifiedFilesFromOffset(1) pos = %d, want 3", pos)
	}

	// From offset 2 (skip first two lines)
	files, pos, err = ag.ExtractModifiedFilesFromOffset(transcriptPath, 2)
	if err != nil {
		t.Fatalf("ExtractModifiedFilesFromOffset(2) error = %v", err)
	}
	if len(files) != 1 {
		t.Errorf("ExtractModifiedFilesFromOffset(2) files = %v, want 1 file", files)
	}
	if files[0] != "newer.go" {
		t.Errorf("ExtractModifiedFilesFromOffset(2) files[0] = %q, want %q", files[0], "newer.go")
	}
	if pos != 3 {
		t.Errorf("ExtractModifiedFilesFromOffset(2) pos = %d, want 3", pos)
	}
}

func TestChunkTranscript(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}

	line1 := `{"info":{"id":"1","role":"user"},"parts":[{"type":"text","text":"hello"}]}`
	line2 := `{"info":{"id":"2","role":"assistant"},"parts":[{"type":"text","text":"hi there"}]}`
	content := []byte(line1 + "\n" + line2 + "\n")

	// Set maxSize large enough for both lines
	chunks, err := ag.ChunkTranscript(content, len(content)+100)
	if err != nil {
		t.Fatalf("ChunkTranscript() error = %v", err)
	}
	if len(chunks) != 1 {
		t.Errorf("ChunkTranscript() got %d chunks, want 1", len(chunks))
	}
}

func TestReassembleTranscript(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}

	chunk1 := []byte(`{"info":{"id":"1","role":"user"},"parts":[]}` + "\n")
	chunk2 := []byte(`{"info":{"id":"2","role":"assistant"},"parts":[]}` + "\n")

	result, err := ag.ReassembleTranscript([][]byte{chunk1, chunk2})
	if err != nil {
		t.Fatalf("ReassembleTranscript() error = %v", err)
	}

	// Parse to verify valid JSONL
	entries, err := ParseTranscript(result)
	if err != nil {
		t.Fatalf("ReassembleTranscript result is not valid transcript: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("ReassembleTranscript() got %d entries, want 2", len(entries))
	}
}

// mustMarshalJSON is a test helper that marshals a value to JSON.
func mustMarshalJSON(t *testing.T, v interface{}) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}
	return data
}

func TestCalculateTokenUsage_BasicMessages(t *testing.T) {
	t.Parallel()

	// Two assistant messages with token usage in info
	data := []byte(`{"info":{"id":"1","sessionID":"s1","role":"user","time":{"created":1000,"completed":1001}},"parts":[{"type":"text","text":"hello"}]}
{"info":{"id":"2","sessionID":"s1","role":"assistant","time":{"created":1002,"completed":1003},"tokens":{"input":100,"output":50,"reasoning":10,"cache":{"read":20,"write":5}},"cost":0.01},"parts":[{"type":"text","text":"hi there"}]}
{"info":{"id":"3","sessionID":"s1","role":"user","time":{"created":1004,"completed":1005}},"parts":[{"type":"text","text":"how are you?"}]}
{"info":{"id":"4","sessionID":"s1","role":"assistant","time":{"created":1006,"completed":1007},"tokens":{"input":200,"output":80,"reasoning":15,"cache":{"read":30,"write":10}},"cost":0.02},"parts":[{"type":"text","text":"doing well"}]}
`)

	entries, err := ParseTranscript(data)
	if err != nil {
		t.Fatalf("ParseTranscript() error = %v", err)
	}

	usage := CalculateTokenUsage(entries, 0)

	if usage.APICallCount != 2 {
		t.Errorf("APICallCount = %d, want 2", usage.APICallCount)
	}
	// Input: 100 + 200 = 300
	if usage.InputTokens != 300 {
		t.Errorf("InputTokens = %d, want 300", usage.InputTokens)
	}
	// Output: 50 + 80 = 130
	if usage.OutputTokens != 130 {
		t.Errorf("OutputTokens = %d, want 130", usage.OutputTokens)
	}
	// CacheRead: 20 + 30 = 50
	if usage.CacheReadTokens != 50 {
		t.Errorf("CacheReadTokens = %d, want 50", usage.CacheReadTokens)
	}
	// CacheCreation: 5 + 10 = 15
	if usage.CacheCreationTokens != 15 {
		t.Errorf("CacheCreationTokens = %d, want 15", usage.CacheCreationTokens)
	}
}

func TestCalculateTokenUsage_StartIndex(t *testing.T) {
	t.Parallel()

	data := []byte(`{"info":{"id":"1","sessionID":"s1","role":"user","time":{"created":1000,"completed":1001}},"parts":[{"type":"text","text":"hello"}]}
{"info":{"id":"2","sessionID":"s1","role":"assistant","time":{"created":1002,"completed":1003},"tokens":{"input":100,"output":50,"reasoning":0,"cache":{"read":0,"write":0}},"cost":0.01},"parts":[{"type":"text","text":"hi"}]}
{"info":{"id":"3","sessionID":"s1","role":"user","time":{"created":1004,"completed":1005}},"parts":[{"type":"text","text":"more"}]}
{"info":{"id":"4","sessionID":"s1","role":"assistant","time":{"created":1006,"completed":1007},"tokens":{"input":200,"output":80,"reasoning":0,"cache":{"read":10,"write":5}},"cost":0.02},"parts":[{"type":"text","text":"ok"}]}
`)

	entries, err := ParseTranscript(data)
	if err != nil {
		t.Fatalf("ParseTranscript() error = %v", err)
	}

	// Start from index 2 — should only count the second assistant message
	usage := CalculateTokenUsage(entries, 2)

	if usage.APICallCount != 1 {
		t.Errorf("APICallCount = %d, want 1", usage.APICallCount)
	}
	if usage.InputTokens != 200 {
		t.Errorf("InputTokens = %d, want 200", usage.InputTokens)
	}
	if usage.OutputTokens != 80 {
		t.Errorf("OutputTokens = %d, want 80", usage.OutputTokens)
	}
	if usage.CacheReadTokens != 10 {
		t.Errorf("CacheReadTokens = %d, want 10", usage.CacheReadTokens)
	}
}

func TestCalculateTokenUsage_IgnoresUserMessages(t *testing.T) {
	t.Parallel()

	// Even if a user message somehow had tokens, they should be ignored
	entries := []TranscriptEntry{
		{
			Info: TranscriptEntryInfo{
				ID:   "1",
				Role: MessageRoleUser,
				Tokens: &TranscriptTokens{
					Input: 999, Output: 999,
					Cache: TranscriptTokensCache{Read: 999, Write: 999},
				},
			},
		},
		{
			Info: TranscriptEntryInfo{
				ID:   "2",
				Role: MessageRoleAssistant,
				Tokens: &TranscriptTokens{
					Input: 10, Output: 20,
					Cache: TranscriptTokensCache{Read: 5, Write: 3},
				},
			},
		},
	}

	usage := CalculateTokenUsage(entries, 0)

	if usage.APICallCount != 1 {
		t.Errorf("APICallCount = %d, want 1", usage.APICallCount)
	}
	if usage.InputTokens != 10 {
		t.Errorf("InputTokens = %d, want 10", usage.InputTokens)
	}
	if usage.OutputTokens != 20 {
		t.Errorf("OutputTokens = %d, want 20", usage.OutputTokens)
	}
}

func TestCalculateTokenUsage_EmptyTranscript(t *testing.T) {
	t.Parallel()

	usage := CalculateTokenUsage(nil, 0)
	if usage.APICallCount != 0 {
		t.Errorf("APICallCount = %d, want 0", usage.APICallCount)
	}
	if usage.InputTokens != 0 {
		t.Errorf("InputTokens = %d, want 0", usage.InputTokens)
	}
}

func TestCalculateTokenUsage_DeduplicatesByMessageID(t *testing.T) {
	t.Parallel()

	// Same message ID appearing twice (e.g., streaming duplicates)
	entries := []TranscriptEntry{
		{
			Info: TranscriptEntryInfo{
				ID:   "msg-1",
				Role: MessageRoleAssistant,
				Tokens: &TranscriptTokens{
					Input: 100, Output: 50,
					Cache: TranscriptTokensCache{Read: 10, Write: 5},
				},
			},
		},
		{
			Info: TranscriptEntryInfo{
				ID:   "msg-1", // duplicate
				Role: MessageRoleAssistant,
				Tokens: &TranscriptTokens{
					Input: 100, Output: 50,
					Cache: TranscriptTokensCache{Read: 10, Write: 5},
				},
			},
		},
	}

	usage := CalculateTokenUsage(entries, 0)

	if usage.APICallCount != 1 {
		t.Errorf("APICallCount = %d, want 1 (deduplicated)", usage.APICallCount)
	}
	if usage.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100 (not double-counted)", usage.InputTokens)
	}
}

func TestCalculateTokenUsage_NoTokensField(t *testing.T) {
	t.Parallel()

	// Assistant message without tokens field
	entries := []TranscriptEntry{
		{
			Info: TranscriptEntryInfo{
				ID:   "1",
				Role: MessageRoleAssistant,
				// No Tokens field
			},
		},
	}

	usage := CalculateTokenUsage(entries, 0)

	if usage.APICallCount != 0 {
		t.Errorf("APICallCount = %d, want 0 (no tokens)", usage.APICallCount)
	}
}

func TestCalculateTokenUsageFromData(t *testing.T) {
	t.Parallel()

	data := []byte(`{"info":{"id":"1","sessionID":"s1","role":"assistant","time":{"created":1000,"completed":1001},"tokens":{"input":50,"output":25,"reasoning":0,"cache":{"read":10,"write":5}},"cost":0.005},"parts":[{"type":"text","text":"done"}]}
`)

	usage := CalculateTokenUsageFromData(data, 0)

	if usage.APICallCount != 1 {
		t.Errorf("APICallCount = %d, want 1", usage.APICallCount)
	}
	if usage.InputTokens != 50 {
		t.Errorf("InputTokens = %d, want 50", usage.InputTokens)
	}
	if usage.OutputTokens != 25 {
		t.Errorf("OutputTokens = %d, want 25", usage.OutputTokens)
	}
}

func TestCalculateTokenUsageFromData_Empty(t *testing.T) {
	t.Parallel()

	usage := CalculateTokenUsageFromData([]byte(""), 0)
	if usage.APICallCount != 0 {
		t.Errorf("APICallCount = %d, want 0", usage.APICallCount)
	}
}

func TestCalculateTokenUsageFromFile(t *testing.T) {
	tmpDir := t.TempDir()
	path := tmpDir + "/transcript.jsonl"

	content := `{"info":{"id":"1","sessionID":"s1","role":"assistant","time":{"created":1000,"completed":1001},"tokens":{"input":75,"output":30,"reasoning":5,"cache":{"read":15,"write":8}},"cost":0.01},"parts":[{"type":"text","text":"result"}]}
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	usage, err := CalculateTokenUsageFromFile(path, 0)
	if err != nil {
		t.Fatalf("CalculateTokenUsageFromFile() error = %v", err)
	}

	if usage.APICallCount != 1 {
		t.Errorf("APICallCount = %d, want 1", usage.APICallCount)
	}
	if usage.InputTokens != 75 {
		t.Errorf("InputTokens = %d, want 75", usage.InputTokens)
	}
	if usage.CacheReadTokens != 15 {
		t.Errorf("CacheReadTokens = %d, want 15", usage.CacheReadTokens)
	}
	if usage.CacheCreationTokens != 8 {
		t.Errorf("CacheCreationTokens = %d, want 8", usage.CacheCreationTokens)
	}
}

func TestCalculateTokenUsageFromFile_EmptyPath(t *testing.T) {
	t.Parallel()

	usage, err := CalculateTokenUsageFromFile("", 0)
	if err != nil {
		t.Fatalf("CalculateTokenUsageFromFile('') error = %v", err)
	}
	if usage.APICallCount != 0 {
		t.Errorf("APICallCount = %d, want 0", usage.APICallCount)
	}
}

func TestCalculateTokenUsageFromFile_NonExistent(t *testing.T) {
	t.Parallel()

	_, err := CalculateTokenUsageFromFile("/nonexistent/path.jsonl", 0)
	if err == nil {
		t.Error("CalculateTokenUsageFromFile() expected error for nonexistent file")
	}
}
