package cli

import (
	"os"
	"path/filepath"
	"testing"
)

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

func TestGetTranscriptPosition_BasicMessages(t *testing.T) {
	content := `{"type":"user","uuid":"user-1","message":{"content":"Hello"}}
{"type":"assistant","uuid":"asst-1","message":{"content":[{"type":"text","text":"Hi"}]}}
{"type":"user","uuid":"user-2","message":{"content":"Bye"}}`

	tmpFile := createTempTranscript(t, content)

	pos, err := GetTranscriptPosition(tmpFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if pos.LineCount != 3 {
		t.Errorf("LineCount = %d, want 3", pos.LineCount)
	}
	if pos.LastUUID != "user-2" {
		t.Errorf("LastUUID = %q, want 'user-2'", pos.LastUUID)
	}
}

func TestGetTranscriptPosition_WithSummaryRows(t *testing.T) {
	// Summary rows have leafUuid but no uuid field - they should not be tracked
	content := `{"type":"summary","leafUuid":"leaf-1","summary":"Previous context"}
{"type":"summary","leafUuid":"leaf-2","summary":"More context"}
{"type":"user","uuid":"user-1","message":{"content":"Hello"}}
{"type":"assistant","uuid":"asst-1","message":{"content":[{"type":"text","text":"Hi"}]}}`

	tmpFile := createTempTranscript(t, content)

	pos, err := GetTranscriptPosition(tmpFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if pos.LineCount != 4 {
		t.Errorf("LineCount = %d, want 4", pos.LineCount)
	}
	// LastUUID should be from user/assistant messages, not summary rows
	if pos.LastUUID != "asst-1" {
		t.Errorf("LastUUID = %q, want 'asst-1'", pos.LastUUID)
	}
}

func TestGetTranscriptPosition_EmptyFile(t *testing.T) {
	tmpFile := createTempTranscript(t, "")

	pos, err := GetTranscriptPosition(tmpFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if pos.LineCount != 0 {
		t.Errorf("LineCount = %d, want 0", pos.LineCount)
	}
	if pos.LastUUID != "" {
		t.Errorf("LastUUID = %q, want empty", pos.LastUUID)
	}
}

func TestGetTranscriptPosition_NonExistentFile(t *testing.T) {
	pos, err := GetTranscriptPosition("/nonexistent/path/transcript.jsonl")
	if err != nil {
		t.Fatalf("unexpected error for non-existent file: %v", err)
	}

	// Should return empty position for non-existent file
	if pos.LineCount != 0 {
		t.Errorf("LineCount = %d, want 0", pos.LineCount)
	}
	if pos.LastUUID != "" {
		t.Errorf("LastUUID = %q, want empty", pos.LastUUID)
	}
}

func TestGetTranscriptPosition_EmptyPath(t *testing.T) {
	pos, err := GetTranscriptPosition("")
	if err != nil {
		t.Fatalf("unexpected error for empty path: %v", err)
	}

	if pos.LineCount != 0 {
		t.Errorf("LineCount = %d, want 0", pos.LineCount)
	}
	if pos.LastUUID != "" {
		t.Errorf("LastUUID = %q, want empty", pos.LastUUID)
	}
}

func TestGetTranscriptPosition_OnlySummaryRows(t *testing.T) {
	// File with only summary rows (no uuid field, only leafUuid)
	content := `{"type":"summary","leafUuid":"leaf-1","summary":"Context 1"}
{"type":"summary","leafUuid":"leaf-2","summary":"Context 2"}`

	tmpFile := createTempTranscript(t, content)

	pos, err := GetTranscriptPosition(tmpFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if pos.LineCount != 2 {
		t.Errorf("LineCount = %d, want 2", pos.LineCount)
	}
	// No uuid field in summary rows, so LastUUID should be empty
	if pos.LastUUID != "" {
		t.Errorf("LastUUID = %q, want empty (summary rows don't have uuid)", pos.LastUUID)
	}
}

func TestGetTranscriptPosition_MixedWithMalformedLines(t *testing.T) {
	content := `{"type":"user","uuid":"user-1","message":{"content":"Hello"}}
not valid json
{"type":"assistant","uuid":"asst-1","message":{"content":[{"type":"text","text":"Hi"}]}}
{broken json
{"type":"user","uuid":"user-2","message":{"content":"Final"}}`

	tmpFile := createTempTranscript(t, content)

	pos, err := GetTranscriptPosition(tmpFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// All lines count, including malformed
	if pos.LineCount != 5 {
		t.Errorf("LineCount = %d, want 5", pos.LineCount)
	}
	// But LastUUID should be from last valid line with uuid
	if pos.LastUUID != "user-2" {
		t.Errorf("LastUUID = %q, want 'user-2'", pos.LastUUID)
	}
}
