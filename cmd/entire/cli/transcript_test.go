package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseTranscript_ValidMixedMessages(t *testing.T) {
	content := `{"type":"user","uuid":"user-1","message":{"content":"Hello"}}
{"type":"assistant","uuid":"asst-1","message":{"content":[{"type":"text","text":"Hi there"}]}}
{"type":"user","uuid":"user-2","message":{"content":"Thanks"}}`

	tmpFile := createTempTranscript(t, content)

	lines, err := parseTranscript(tmpFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}

	if lines[0].Type != "user" || lines[0].UUID != "user-1" {
		t.Errorf("first line mismatch: got type=%s uuid=%s", lines[0].Type, lines[0].UUID)
	}
	if lines[1].Type != "assistant" || lines[1].UUID != "asst-1" {
		t.Errorf("second line mismatch: got type=%s uuid=%s", lines[1].Type, lines[1].UUID)
	}
	if lines[2].Type != "user" || lines[2].UUID != "user-2" {
		t.Errorf("third line mismatch: got type=%s uuid=%s", lines[2].Type, lines[2].UUID)
	}
}

func TestParseTranscript_SkipsMalformedLines(t *testing.T) {
	content := `{"type":"user","uuid":"user-1","message":{"content":"Hello"}}
this is not valid json
{"type":"assistant","uuid":"asst-1","message":{"content":[{"type":"text","text":"Hi"}]}}
{"broken json
{"type":"user","uuid":"user-2","message":{"content":"Bye"}}`

	tmpFile := createTempTranscript(t, content)

	lines, err := parseTranscript(tmpFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(lines) != 3 {
		t.Fatalf("expected 3 valid lines, got %d", len(lines))
	}
}

func TestParseTranscript_LargeLines(t *testing.T) {
	// Create a line with content larger than default scanner buffer (64KB)
	// but smaller than our 10MB limit
	largeContent := strings.Repeat("x", 100*1024) // 100KB
	content := `{"type":"user","uuid":"user-1","message":{"content":"` + largeContent + `"}}`

	tmpFile := createTempTranscript(t, content)

	lines, err := parseTranscript(tmpFile)
	if err != nil {
		t.Fatalf("unexpected error parsing large line: %v", err)
	}

	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
}

func TestParseTranscript_LineExceedsScannerBuffer(t *testing.T) {
	// Create a line larger than the 10MB ScannerBufferSize limit.
	// This simulates a transcript with a very large tool output (e.g., reading a huge file).
	// The current implementation using bufio.Scanner will fail with "token too long".
	largeContent := strings.Repeat("x", 11*1024*1024) // 11MB - exceeds 10MB limit
	content := `{"type":"user","uuid":"user-1","message":{"content":"` + largeContent + `"}}`

	tmpFile := createTempTranscript(t, content)

	lines, err := parseTranscript(tmpFile)
	if err != nil {
		t.Fatalf("unexpected error parsing line exceeding buffer: %v", err)
	}

	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
}

func TestParseTranscriptFromLine_LineExceedsScannerBuffer(t *testing.T) {
	// Same test for parseTranscriptFromLine - should handle lines > 10MB
	largeContent := strings.Repeat("x", 11*1024*1024) // 11MB
	content := `{"type":"user","uuid":"user-1","message":{"content":"` + largeContent + `"}}`

	tmpFile := createTempTranscript(t, content)

	lines, totalLines, err := parseTranscriptFromLine(tmpFile, 0)
	if err != nil {
		t.Fatalf("unexpected error parsing line exceeding buffer: %v", err)
	}

	if totalLines != 1 {
		t.Errorf("expected totalLines=1, got %d", totalLines)
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 parsed line, got %d", len(lines))
	}
}

func TestExtractLastUserPrompt_StringContent(t *testing.T) {
	transcript := []transcriptLine{
		{Type: "user", UUID: "u1", Message: []byte(`{"content":"First prompt"}`)},
		{Type: "assistant", UUID: "a1", Message: []byte(`{"content":[{"type":"text","text":"Response 1"}]}`)},
		{Type: "user", UUID: "u2", Message: []byte(`{"content":"Second prompt"}`)},
		{Type: "assistant", UUID: "a2", Message: []byte(`{"content":[{"type":"text","text":"Response 2"}]}`)},
		{Type: "user", UUID: "u3", Message: []byte(`{"content":"Last prompt"}`)},
	}

	prompt := extractLastUserPrompt(transcript)
	if prompt != "Last prompt" {
		t.Errorf("expected 'Last prompt', got '%s'", prompt)
	}
}

func TestExtractLastUserPrompt_ArrayContent(t *testing.T) {
	transcript := []transcriptLine{
		{Type: "user", UUID: "u1", Message: []byte(`{"content":"First prompt"}`)},
		{Type: "assistant", UUID: "a1", Message: []byte(`{"content":[{"type":"text","text":"Response"}]}`)},
		{Type: "user", UUID: "u2", Message: []byte(`{"content":[{"type":"text","text":"Last part 1"},{"type":"text","text":"Last part 2"}]}`)},
	}

	prompt := extractLastUserPrompt(transcript)
	expected := "Last part 1\n\nLast part 2"
	if prompt != expected {
		t.Errorf("expected %q, got %q", expected, prompt)
	}
}

func TestExtractLastUserPrompt_SkipsToolResults(t *testing.T) {
	// Tool results have array content without text blocks
	transcript := []transcriptLine{
		{Type: "user", UUID: "u1", Message: []byte(`{"content":"Real user prompt"}`)},
		{Type: "assistant", UUID: "a1", Message: []byte(`{"content":[{"type":"text","text":"Response"}]}`)},
		{Type: "user", UUID: "u2", Message: []byte(`{"content":[{"type":"tool_result","tool_use_id":"123","content":"tool output"}]}`)},
	}

	prompt := extractLastUserPrompt(transcript)
	if prompt != "Real user prompt" {
		t.Errorf("expected 'Real user prompt', got '%s'", prompt)
	}
}

func TestExtractLastUserPrompt_EmptyTranscript(t *testing.T) {
	transcript := []transcriptLine{}

	prompt := extractLastUserPrompt(transcript)
	if prompt != "" {
		t.Errorf("expected empty string, got '%s'", prompt)
	}
}

func TestExtractLastUserPrompt_NoUserMessages(t *testing.T) {
	transcript := []transcriptLine{
		{Type: "assistant", UUID: "a1", Message: []byte(`{"content":[{"type":"text","text":"Response"}]}`)},
	}

	prompt := extractLastUserPrompt(transcript)
	if prompt != "" {
		t.Errorf("expected empty string, got '%s'", prompt)
	}
}

func TestExtractModifiedFiles_AllToolTypes(t *testing.T) {
	transcript := []transcriptLine{
		{Type: "assistant", Message: []byte(`{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"/path/write.go"}}]}`)},
		{Type: "assistant", Message: []byte(`{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/path/edit.go"}}]}`)},
		{Type: "assistant", Message: []byte(`{"content":[{"type":"tool_use","name":"mcp__acp__Write","input":{"file_path":"/path/mcp_write.go"}}]}`)},
		{Type: "assistant", Message: []byte(`{"content":[{"type":"tool_use","name":"mcp__acp__Edit","input":{"file_path":"/path/mcp_edit.go"}}]}`)},
		{Type: "assistant", Message: []byte(`{"content":[{"type":"tool_use","name":"NotebookEdit","input":{"notebook_path":"/path/notebook.ipynb"}}]}`)},
		{Type: "assistant", Message: []byte(`{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/path/read.go"}}]}`)},
	}

	files := extractModifiedFiles(transcript)

	expected := []string{"/path/write.go", "/path/edit.go", "/path/mcp_write.go", "/path/mcp_edit.go", "/path/notebook.ipynb"}
	if len(files) != len(expected) {
		t.Fatalf("expected %d files, got %d: %v", len(expected), len(files), files)
	}

	for i, exp := range expected {
		if files[i] != exp {
			t.Errorf("file %d: expected %s, got %s", i, exp, files[i])
		}
	}
}

func TestExtractModifiedFiles_Deduplicates(t *testing.T) {
	transcript := []transcriptLine{
		{Type: "assistant", Message: []byte(`{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"/path/file.go"}}]}`)},
		{Type: "assistant", Message: []byte(`{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/path/file.go"}}]}`)},
		{Type: "assistant", Message: []byte(`{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/path/file.go"}}]}`)},
	}

	files := extractModifiedFiles(transcript)

	if len(files) != 1 {
		t.Fatalf("expected 1 deduplicated file, got %d: %v", len(files), files)
	}
	if files[0] != "/path/file.go" {
		t.Errorf("expected /path/file.go, got %s", files[0])
	}
}

func TestFindLastUserUUID_AndFilterTranscript(t *testing.T) {
	transcript := []transcriptLine{
		{Type: "user", UUID: "u1", Message: []byte(`{"content":"First prompt"}`)},
		{Type: "assistant", UUID: "a1", Message: []byte(`{"content":[{"type":"text","text":"Response 1"}]}`)},
		{Type: "user", UUID: "u2", Message: []byte(`{"content":"Second prompt"}`)},
		{Type: "assistant", UUID: "a2", Message: []byte(`{"content":[{"type":"text","text":"Response 2"}]}`)},
	}

	lastUUID := findLastUserUUID(transcript)
	if lastUUID != "u2" {
		t.Errorf("expected last user UUID 'u2', got '%s'", lastUUID)
	}

	filtered := filterTranscriptAfterUUID(transcript, lastUUID)
	if len(filtered) != 1 {
		t.Fatalf("expected 1 entry after last user, got %d", len(filtered))
	}
	if filtered[0].UUID != "a2" {
		t.Errorf("expected filtered to contain 'a2', got '%s'", filtered[0].UUID)
	}
}

func createTempTranscript(t *testing.T, content string) string {
	t.Helper()
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "transcript.jsonl")
	if err := os.WriteFile(tmpFile, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	return tmpFile
}

func TestAgentTranscriptPath(t *testing.T) {
	tests := []struct {
		name          string
		transcriptDir string
		agentID       string
		expected      string
	}{
		{
			name:          "standard path",
			transcriptDir: "/home/user/.claude/projects/myproject",
			agentID:       "agent_abc123",
			expected:      "/home/user/.claude/projects/myproject/agent-agent_abc123.jsonl",
		},
		{
			name:          "empty agent ID",
			transcriptDir: "/path/to/transcripts",
			agentID:       "",
			expected:      "/path/to/transcripts/agent-.jsonl",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AgentTranscriptPath(tt.transcriptDir, tt.agentID)
			if got != tt.expected {
				t.Errorf("AgentTranscriptPath() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestFindCheckpointUUID(t *testing.T) {
	transcript := []transcriptLine{
		{Type: "user", UUID: "u1", Message: []byte(`{"content":"First prompt"}`)},
		{Type: "assistant", UUID: "a1", Message: []byte(`{"content":[{"type":"tool_use","id":"toolu_task1","name":"Task","input":{}}]}`)},
		{Type: "user", UUID: "u2", Message: []byte(`{"content":[{"type":"tool_result","tool_use_id":"toolu_task1","content":"Task completed"}]}`)},
		{Type: "assistant", UUID: "a2", Message: []byte(`{"content":[{"type":"text","text":"Done"}]}`)},
		{Type: "assistant", UUID: "a3", Message: []byte(`{"content":[{"type":"tool_use","id":"toolu_task2","name":"Task","input":{}}]}`)},
		{Type: "user", UUID: "u3", Message: []byte(`{"content":[{"type":"tool_result","tool_use_id":"toolu_task2","content":"Second task done"}]}`)},
	}

	tests := []struct {
		name      string
		toolUseID string
		wantUUID  string
		wantFound bool
	}{
		{
			name:      "find first task result",
			toolUseID: "toolu_task1",
			wantUUID:  "u2",
			wantFound: true,
		},
		{
			name:      "find second task result",
			toolUseID: "toolu_task2",
			wantUUID:  "u3",
			wantFound: true,
		},
		{
			name:      "non-existent tool use ID",
			toolUseID: "toolu_nonexistent",
			wantUUID:  "",
			wantFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotUUID, gotFound := FindCheckpointUUID(transcript, tt.toolUseID)
			if gotFound != tt.wantFound {
				t.Errorf("FindCheckpointUUID() found = %v, want %v", gotFound, tt.wantFound)
			}
			if gotUUID != tt.wantUUID {
				t.Errorf("FindCheckpointUUID() uuid = %v, want %v", gotUUID, tt.wantUUID)
			}
		})
	}
}

func TestTruncateTranscriptAtUUID(t *testing.T) {
	transcript := []transcriptLine{
		{Type: "user", UUID: "u1", Message: []byte(`{"content":"First"}`)},
		{Type: "assistant", UUID: "a1", Message: []byte(`{"content":[]}`)},
		{Type: "user", UUID: "u2", Message: []byte(`{"content":"Second"}`)},
		{Type: "assistant", UUID: "a2", Message: []byte(`{"content":[]}`)},
		{Type: "user", UUID: "u3", Message: []byte(`{"content":"Third"}`)},
	}

	tests := []struct {
		name         string
		uuid         string
		expectedLen  int
		expectedLast string
	}{
		{
			name:         "truncate at u2",
			uuid:         "u2",
			expectedLen:  3,
			expectedLast: "u2",
		},
		{
			name:         "truncate at first",
			uuid:         "u1",
			expectedLen:  1,
			expectedLast: "u1",
		},
		{
			name:         "truncate at last",
			uuid:         "u3",
			expectedLen:  5,
			expectedLast: "u3",
		},
		{
			name:         "uuid not found - return all",
			uuid:         "nonexistent",
			expectedLen:  5,
			expectedLast: "u3",
		},
		{
			name:         "empty uuid - return all",
			uuid:         "",
			expectedLen:  5,
			expectedLast: "u3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := TruncateTranscriptAtUUID(transcript, tt.uuid)
			if len(result) != tt.expectedLen {
				t.Errorf("TruncateTranscriptAtUUID() len = %d, want %d", len(result), tt.expectedLen)
			}
			if len(result) > 0 && result[len(result)-1].UUID != tt.expectedLast {
				t.Errorf("TruncateTranscriptAtUUID() last UUID = %s, want %s", result[len(result)-1].UUID, tt.expectedLast)
			}
		})
	}
}

func TestExtractLastUserPrompt_StripsIDETags(t *testing.T) {
	// Test that IDE tags are stripped from array content (VSCode format)
	transcript := []transcriptLine{
		{Type: "user", UUID: "u1", Message: []byte(`{"content":[{"type":"text","text":"<ide_opened_file>The user opened /path/file.md</ide_opened_file>"},{"type":"text","text":"make the returned number red"}]}`)},
	}

	prompt := extractLastUserPrompt(transcript)
	expected := "make the returned number red"
	if prompt != expected {
		t.Errorf("expected %q, got %q", expected, prompt)
	}
}

func TestExtractLastUserPrompt_StripsIDETagsFromStringContent(t *testing.T) {
	// Test that IDE tags are stripped from string content
	transcript := []transcriptLine{
		{Type: "user", UUID: "u1", Message: []byte(`{"content":"<ide_selection>some code</ide_selection>\n\nfix this bug"}`)},
	}

	prompt := extractLastUserPrompt(transcript)
	expected := "fix this bug"
	if prompt != expected {
		t.Errorf("expected %q, got %q", expected, prompt)
	}
}

func TestParseTranscriptFromLine_EntireFile(t *testing.T) {
	content := `{"type":"user","uuid":"user-1","message":{"content":"Hello"}}
{"type":"assistant","uuid":"asst-1","message":{"content":[{"type":"text","text":"Hi"}]}}
{"type":"user","uuid":"user-2","message":{"content":"Bye"}}`

	tmpFile := createTempTranscript(t, content)

	// Parse from start (line 0)
	lines, totalLines, err := parseTranscriptFromLine(tmpFile, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if totalLines != 3 {
		t.Errorf("totalLines = %d, want 3", totalLines)
	}
	if len(lines) != 3 {
		t.Errorf("len(lines) = %d, want 3", len(lines))
	}
}

func TestParseTranscriptFromLine_Offset(t *testing.T) {
	content := `{"type":"user","uuid":"user-1","message":{"content":"Line1"}}
{"type":"assistant","uuid":"asst-1","message":{"content":[{"type":"text","text":"Line2"}]}}
{"type":"user","uuid":"user-2","message":{"content":"Line3"}}
{"type":"assistant","uuid":"asst-2","message":{"content":[{"type":"text","text":"Line4"}]}}`

	tmpFile := createTempTranscript(t, content)

	// Parse from line 2 (0-indexed, so skip first 2 lines)
	lines, totalLines, err := parseTranscriptFromLine(tmpFile, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if totalLines != 4 {
		t.Errorf("totalLines = %d, want 4", totalLines)
	}
	if len(lines) != 2 {
		t.Errorf("len(lines) = %d, want 2 (lines after offset)", len(lines))
	}

	// Verify we got the correct lines (user-2 and asst-2)
	if len(lines) >= 1 && lines[0].UUID != "user-2" {
		t.Errorf("first line UUID = %q, want 'user-2'", lines[0].UUID)
	}
	if len(lines) >= 2 && lines[1].UUID != "asst-2" {
		t.Errorf("second line UUID = %q, want 'asst-2'", lines[1].UUID)
	}
}

func TestParseTranscriptFromLine_OffsetBeyondEnd(t *testing.T) {
	content := `{"type":"user","uuid":"user-1","message":{"content":"Hello"}}
{"type":"assistant","uuid":"asst-1","message":{"content":[{"type":"text","text":"Hi"}]}}`

	tmpFile := createTempTranscript(t, content)

	// Parse from line 10 (beyond end of file)
	lines, totalLines, err := parseTranscriptFromLine(tmpFile, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if totalLines != 2 {
		t.Errorf("totalLines = %d, want 2", totalLines)
	}
	if len(lines) != 0 {
		t.Errorf("len(lines) = %d, want 0 (no lines after offset)", len(lines))
	}
}

func TestParseTranscriptFromLine_SkipsMalformedLines(t *testing.T) {
	content := `{"type":"user","uuid":"user-1","message":{"content":"Hello"}}
invalid json line
{"type":"assistant","uuid":"asst-1","message":{"content":[{"type":"text","text":"Hi"}]}}
{"type":"user","uuid":"user-2","message":{"content":"Bye"}}`

	tmpFile := createTempTranscript(t, content)

	// Parse from line 1 (skip first valid line)
	lines, totalLines, err := parseTranscriptFromLine(tmpFile, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Total lines counts ALL lines including malformed (they're still lines in the file)
	if totalLines != 4 {
		t.Errorf("totalLines = %d, want 4", totalLines)
	}
	// But parsed lines excludes malformed
	if len(lines) != 2 {
		t.Errorf("len(lines) = %d, want 2 (valid lines after offset)", len(lines))
	}
}
