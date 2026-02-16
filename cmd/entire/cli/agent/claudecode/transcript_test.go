package claudecode

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

func TestParseTranscript(t *testing.T) {
	t.Parallel()

	data := []byte(`{"type":"user","uuid":"u1","message":{"content":"hello"}}
{"type":"assistant","uuid":"a1","message":{"content":[{"type":"text","text":"hi"}]}}
`)

	lines, err := ParseTranscript(data)
	if err != nil {
		t.Fatalf("ParseTranscript() error = %v", err)
	}

	if len(lines) != 2 {
		t.Errorf("ParseTranscript() got %d lines, want 2", len(lines))
	}

	if lines[0].Type != transcript.TypeUser || lines[0].UUID != "u1" {
		t.Errorf("First line = %+v, want type=user, uuid=u1", lines[0])
	}

	if lines[1].Type != transcript.TypeAssistant || lines[1].UUID != "a1" {
		t.Errorf("Second line = %+v, want type=assistant, uuid=a1", lines[1])
	}
}

func TestParseTranscript_SkipsMalformed(t *testing.T) {
	t.Parallel()

	data := []byte(`{"type":"user","uuid":"u1","message":{"content":"hello"}}
not valid json
{"type":"assistant","uuid":"a1","message":{"content":[]}}
`)

	lines, err := ParseTranscript(data)
	if err != nil {
		t.Fatalf("ParseTranscript() error = %v", err)
	}

	// Should skip the malformed line
	if len(lines) != 2 {
		t.Errorf("ParseTranscript() got %d lines, want 2 (skipping malformed)", len(lines))
	}
}

func TestSerializeTranscript(t *testing.T) {
	t.Parallel()

	lines := []TranscriptLine{
		{Type: "user", UUID: "u1"},
		{Type: "assistant", UUID: "a1"},
	}

	data, err := SerializeTranscript(lines)
	if err != nil {
		t.Fatalf("SerializeTranscript() error = %v", err)
	}

	// Parse back to verify round-trip
	parsed, err := ParseTranscript(data)
	if err != nil {
		t.Fatalf("ParseTranscript(serialized) error = %v", err)
	}

	if len(parsed) != 2 {
		t.Errorf("Round-trip got %d lines, want 2", len(parsed))
	}
}

func TestExtractModifiedFiles(t *testing.T) {
	t.Parallel()

	data := []byte(`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"foo.go"}}]}}
{"type":"assistant","uuid":"a2","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"bar.go"}}]}}
{"type":"assistant","uuid":"a3","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}
{"type":"assistant","uuid":"a4","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"foo.go"}}]}}
`)

	lines, err := ParseTranscript(data)
	if err != nil {
		t.Fatalf("ParseTranscript() error = %v", err)
	}
	files := ExtractModifiedFiles(lines)

	// Should have foo.go and bar.go (deduplicated, Bash not included)
	if len(files) != 2 {
		t.Errorf("ExtractModifiedFiles() got %d files, want 2", len(files))
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

func TestExtractLastUserPrompt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data string
		want string
	}{
		{
			name: "string content",
			data: `{"type":"user","uuid":"u1","message":{"content":"first"}}
{"type":"assistant","uuid":"a1","message":{"content":[]}}
{"type":"user","uuid":"u2","message":{"content":"second"}}`,
			want: "second",
		},
		{
			name: "array content with text block",
			data: `{"type":"user","uuid":"u1","message":{"content":[{"type":"text","text":"hello world"}]}}`,
			want: "hello world",
		},
		{
			name: "empty transcript",
			data: ``,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			lines, err := ParseTranscript([]byte(tt.data))
			if err != nil && tt.data != "" {
				t.Fatalf("ParseTranscript() error = %v", err)
			}
			got := ExtractLastUserPrompt(lines)
			if got != tt.want {
				t.Errorf("ExtractLastUserPrompt() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTruncateAtUUID(t *testing.T) {
	t.Parallel()

	data := []byte(`{"type":"user","uuid":"u1","message":{}}
{"type":"assistant","uuid":"a1","message":{}}
{"type":"user","uuid":"u2","message":{}}
{"type":"assistant","uuid":"a2","message":{}}
`)

	lines, err := ParseTranscript(data)
	if err != nil {
		t.Fatalf("ParseTranscript() error = %v", err)
	}

	tests := []struct {
		name     string
		uuid     string
		wantLen  int
		lastUUID string
	}{
		{"truncate at u1", "u1", 1, "u1"},
		{"truncate at a1", "a1", 2, "a1"},
		{"truncate at u2", "u2", 3, "u2"},
		{"truncate at a2", "a2", 4, "a2"},
		{"empty uuid returns all", "", 4, "a2"},
		{"unknown uuid returns all", "unknown", 4, "a2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			truncated := TruncateAtUUID(lines, tt.uuid)
			if len(truncated) != tt.wantLen {
				t.Errorf("TruncateAtUUID(%q) got %d lines, want %d", tt.uuid, len(truncated), tt.wantLen)
			}
			if len(truncated) > 0 && truncated[len(truncated)-1].UUID != tt.lastUUID {
				t.Errorf("TruncateAtUUID(%q) last UUID = %q, want %q", tt.uuid, truncated[len(truncated)-1].UUID, tt.lastUUID)
			}
		})
	}
}

func TestFindCheckpointUUID(t *testing.T) {
	t.Parallel()

	data := []byte(`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","id":"tool1"}]}}
{"type":"user","uuid":"u1","message":{"content":[{"type":"tool_result","tool_use_id":"tool1"}]}}
{"type":"assistant","uuid":"a2","message":{"content":[{"type":"tool_use","id":"tool2"}]}}
{"type":"user","uuid":"u2","message":{"content":[{"type":"tool_result","tool_use_id":"tool2"}]}}
`)

	lines, err := ParseTranscript(data)
	if err != nil {
		t.Fatalf("ParseTranscript() error = %v", err)
	}

	tests := []struct {
		toolUseID string
		wantUUID  string
		wantFound bool
	}{
		{"tool1", "u1", true},
		{"tool2", "u2", true},
		{"unknown", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.toolUseID, func(t *testing.T) {
			t.Parallel()
			uuid, found := FindCheckpointUUID(lines, tt.toolUseID)
			if found != tt.wantFound {
				t.Errorf("FindCheckpointUUID(%q) found = %v, want %v", tt.toolUseID, found, tt.wantFound)
			}
			if uuid != tt.wantUUID {
				t.Errorf("FindCheckpointUUID(%q) uuid = %q, want %q", tt.toolUseID, uuid, tt.wantUUID)
			}
		})
	}
}

// Token calculation tests - Claude Code specific token format

func TestCalculateTokenUsage_BasicMessages(t *testing.T) {
	transcript := []TranscriptLine{
		{
			Type: "assistant",
			UUID: "asst-1",
			Message: mustMarshal(t, map[string]interface{}{
				"id": "msg_001",
				"usage": map[string]int{
					"input_tokens":                10,
					"cache_creation_input_tokens": 100,
					"cache_read_input_tokens":     50,
					"output_tokens":               20,
				},
			}),
		},
		{
			Type: "assistant",
			UUID: "asst-2",
			Message: mustMarshal(t, map[string]interface{}{
				"id": "msg_002",
				"usage": map[string]int{
					"input_tokens":                5,
					"cache_creation_input_tokens": 200,
					"cache_read_input_tokens":     0,
					"output_tokens":               30,
				},
			}),
		},
	}

	usage := CalculateTokenUsage(transcript)

	if usage.APICallCount != 2 {
		t.Errorf("APICallCount = %d, want 2", usage.APICallCount)
	}
	if usage.InputTokens != 15 {
		t.Errorf("InputTokens = %d, want 15", usage.InputTokens)
	}
	if usage.CacheCreationTokens != 300 {
		t.Errorf("CacheCreationTokens = %d, want 300", usage.CacheCreationTokens)
	}
	if usage.CacheReadTokens != 50 {
		t.Errorf("CacheReadTokens = %d, want 50", usage.CacheReadTokens)
	}
	if usage.OutputTokens != 50 {
		t.Errorf("OutputTokens = %d, want 50", usage.OutputTokens)
	}
}

func TestCalculateTokenUsage_StreamingDeduplication(t *testing.T) {
	// Simulate streaming: multiple rows with same message ID, increasing output_tokens
	transcript := []TranscriptLine{
		{
			Type: "assistant",
			UUID: "asst-1",
			Message: mustMarshal(t, map[string]interface{}{
				"id": "msg_001",
				"usage": map[string]int{
					"input_tokens":                10,
					"cache_creation_input_tokens": 100,
					"cache_read_input_tokens":     50,
					"output_tokens":               1, // First streaming chunk
				},
			}),
		},
		{
			Type: "assistant",
			UUID: "asst-2",
			Message: mustMarshal(t, map[string]interface{}{
				"id": "msg_001", // Same message ID
				"usage": map[string]int{
					"input_tokens":                10,
					"cache_creation_input_tokens": 100,
					"cache_read_input_tokens":     50,
					"output_tokens":               5, // More output
				},
			}),
		},
		{
			Type: "assistant",
			UUID: "asst-3",
			Message: mustMarshal(t, map[string]interface{}{
				"id": "msg_001", // Same message ID
				"usage": map[string]int{
					"input_tokens":                10,
					"cache_creation_input_tokens": 100,
					"cache_read_input_tokens":     50,
					"output_tokens":               20, // Final output
				},
			}),
		},
	}

	usage := CalculateTokenUsage(transcript)

	// Should deduplicate to 1 API call with the highest output_tokens
	if usage.APICallCount != 1 {
		t.Errorf("APICallCount = %d, want 1 (should deduplicate by message ID)", usage.APICallCount)
	}
	if usage.OutputTokens != 20 {
		t.Errorf("OutputTokens = %d, want 20 (should take highest)", usage.OutputTokens)
	}
	// Input/cache tokens should not be duplicated
	if usage.InputTokens != 10 {
		t.Errorf("InputTokens = %d, want 10", usage.InputTokens)
	}
}

func TestCalculateTokenUsage_IgnoresUserMessages(t *testing.T) {
	transcript := []TranscriptLine{
		{
			Type:    "user",
			UUID:    "user-1",
			Message: mustMarshal(t, map[string]interface{}{"content": "hello"}),
		},
		{
			Type: "assistant",
			UUID: "asst-1",
			Message: mustMarshal(t, map[string]interface{}{
				"id": "msg_001",
				"usage": map[string]int{
					"input_tokens":                10,
					"cache_creation_input_tokens": 100,
					"cache_read_input_tokens":     0,
					"output_tokens":               20,
				},
			}),
		},
	}

	usage := CalculateTokenUsage(transcript)

	if usage.APICallCount != 1 {
		t.Errorf("APICallCount = %d, want 1", usage.APICallCount)
	}
}

func TestCalculateTokenUsage_EmptyTranscript(t *testing.T) {
	usage := CalculateTokenUsage(nil)

	if usage.APICallCount != 0 {
		t.Errorf("APICallCount = %d, want 0", usage.APICallCount)
	}
	if usage.InputTokens != 0 {
		t.Errorf("InputTokens = %d, want 0", usage.InputTokens)
	}
}

func TestExtractSpawnedAgentIDs_FromToolResult(t *testing.T) {
	transcript := []TranscriptLine{
		{
			Type: "user",
			UUID: "user-1",
			Message: mustMarshal(t, map[string]interface{}{
				"content": []map[string]interface{}{
					{
						"type":        "tool_result",
						"tool_use_id": "toolu_abc123",
						"content": []map[string]string{
							{"type": "text", "text": "Result from agent\n\nagentId: ac66d4b (for resuming)"},
						},
					},
				},
			}),
		},
	}

	agentIDs := ExtractSpawnedAgentIDs(transcript)

	if len(agentIDs) != 1 {
		t.Fatalf("Expected 1 agent ID, got %d", len(agentIDs))
	}
	if _, ok := agentIDs["ac66d4b"]; !ok {
		t.Errorf("Expected agent ID 'ac66d4b', got %v", agentIDs)
	}
	if agentIDs["ac66d4b"] != "toolu_abc123" {
		t.Errorf("Expected tool_use_id 'toolu_abc123', got %s", agentIDs["ac66d4b"])
	}
}

func TestExtractSpawnedAgentIDs_MultipleAgents(t *testing.T) {
	transcript := []TranscriptLine{
		{
			Type: "user",
			UUID: "user-1",
			Message: mustMarshal(t, map[string]interface{}{
				"content": []map[string]interface{}{
					{
						"type":        "tool_result",
						"tool_use_id": "toolu_001",
						"content": []map[string]string{
							{"type": "text", "text": "agentId: aaa1111"},
						},
					},
				},
			}),
		},
		{
			Type: "user",
			UUID: "user-2",
			Message: mustMarshal(t, map[string]interface{}{
				"content": []map[string]interface{}{
					{
						"type":        "tool_result",
						"tool_use_id": "toolu_002",
						"content": []map[string]string{
							{"type": "text", "text": "agentId: bbb2222"},
						},
					},
				},
			}),
		},
	}

	agentIDs := ExtractSpawnedAgentIDs(transcript)

	if len(agentIDs) != 2 {
		t.Fatalf("Expected 2 agent IDs, got %d", len(agentIDs))
	}
	if _, ok := agentIDs["aaa1111"]; !ok {
		t.Errorf("Expected agent ID 'aaa1111'")
	}
	if _, ok := agentIDs["bbb2222"]; !ok {
		t.Errorf("Expected agent ID 'bbb2222'")
	}
}

func TestExtractSpawnedAgentIDs_NoAgentID(t *testing.T) {
	transcript := []TranscriptLine{
		{
			Type: "user",
			UUID: "user-1",
			Message: mustMarshal(t, map[string]interface{}{
				"content": []map[string]interface{}{
					{
						"type":        "tool_result",
						"tool_use_id": "toolu_001",
						"content": []map[string]string{
							{"type": "text", "text": "Some result without agent ID"},
						},
					},
				},
			}),
		},
	}

	agentIDs := ExtractSpawnedAgentIDs(transcript)

	if len(agentIDs) != 0 {
		t.Errorf("Expected 0 agent IDs, got %d: %v", len(agentIDs), agentIDs)
	}
}

func TestExtractAgentIDFromText(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		expected string
	}{
		{
			name:     "standard format",
			text:     "agentId: ac66d4b (for resuming)",
			expected: "ac66d4b",
		},
		{
			name:     "at end of text",
			text:     "Result text\n\nagentId: abc1234",
			expected: "abc1234",
		},
		{
			name:     "no agent ID",
			text:     "Some text without agent ID",
			expected: "",
		},
		{
			name:     "empty text",
			text:     "",
			expected: "",
		},
		{
			name:     "agent ID with newline after",
			text:     "agentId: xyz9999\nMore text",
			expected: "xyz9999",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractAgentIDFromText(tt.text)
			if got != tt.expected {
				t.Errorf("extractAgentIDFromText(%q) = %q, want %q", tt.text, got, tt.expected)
			}
		})
	}
}

// mustMarshal is a test helper that marshals a value to JSON or fails the test
func mustMarshal(t *testing.T, v interface{}) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}
	return data
}

// TestCalculateTotalTokenUsage_PerCheckpoint verifies token usage
// is calculated per-checkpoint, not from the full conversation.
// This tests the core CalculateTotalTokenUsage function which should:
// - From line 0: count all turns
// - From line N: count only turns from line N onwards
func TestCalculateTotalTokenUsage_PerCheckpoint(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	transcriptPath := tmpDir + "/transcript.jsonl"

	// Build transcript with 3 turns:
	// Turn 1: user + assistant (100 input, 50 output)
	// Turn 2: user + assistant (200 input, 100 output)
	// Turn 3: user + assistant (300 input, 150 output)
	//
	// Lines:
	// 0: user message 1
	// 1: assistant response 1 (100/50 tokens)
	// 2: user message 2
	// 3: assistant response 2 (200/100 tokens)
	// 4: user message 3
	// 5: assistant response 3 (300/150 tokens)

	transcriptContent := []byte(
		`{"type":"user","uuid":"u1","message":{"content":"first prompt"}}` + "\n" +
			`{"type":"assistant","uuid":"a1","message":{"id":"m1","usage":{"input_tokens":100,"output_tokens":50}}}` + "\n" +
			`{"type":"user","uuid":"u2","message":{"content":"second prompt"}}` + "\n" +
			`{"type":"assistant","uuid":"a2","message":{"id":"m2","usage":{"input_tokens":200,"output_tokens":100}}}` + "\n" +
			`{"type":"user","uuid":"u3","message":{"content":"third prompt"}}` + "\n" +
			`{"type":"assistant","uuid":"a3","message":{"id":"m3","usage":{"input_tokens":300,"output_tokens":150}}}` + "\n",
	)
	if err := os.WriteFile(transcriptPath, transcriptContent, 0o600); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Test 1: From line 0 - all 3 turns = 600 input, 300 output
	usage1, err := CalculateTotalTokenUsage(transcriptPath, 0, "")
	if err != nil {
		t.Fatalf("CalculateTotalTokenUsage(0) error: %v", err)
	}
	if usage1.InputTokens != 600 || usage1.OutputTokens != 300 {
		t.Errorf("From line 0: got input=%d output=%d, want input=600 output=300",
			usage1.InputTokens, usage1.OutputTokens)
	}
	if usage1.APICallCount != 3 {
		t.Errorf("From line 0: got APICallCount=%d, want 3", usage1.APICallCount)
	}

	// Test 2: From line 2 (after turn 1) - turns 2+3 only = 500 input, 250 output
	usage2, err := CalculateTotalTokenUsage(transcriptPath, 2, "")
	if err != nil {
		t.Fatalf("CalculateTotalTokenUsage(2) error: %v", err)
	}
	if usage2.InputTokens != 500 || usage2.OutputTokens != 250 {
		t.Errorf("From line 2: got input=%d output=%d, want input=500 output=250",
			usage2.InputTokens, usage2.OutputTokens)
	}
	if usage2.APICallCount != 2 {
		t.Errorf("From line 2: got APICallCount=%d, want 2", usage2.APICallCount)
	}

	// Test 3: From line 4 (after turns 1+2) - turn 3 only = 300 input, 150 output
	usage3, err := CalculateTotalTokenUsage(transcriptPath, 4, "")
	if err != nil {
		t.Fatalf("CalculateTotalTokenUsage(4) error: %v", err)
	}
	if usage3.InputTokens != 300 || usage3.OutputTokens != 150 {
		t.Errorf("From line 4: got input=%d output=%d, want input=300 output=150",
			usage3.InputTokens, usage3.OutputTokens)
	}
	if usage3.APICallCount != 1 {
		t.Errorf("From line 4: got APICallCount=%d, want 1", usage3.APICallCount)
	}
}

// writeJSONLFile is a test helper that writes JSONL transcript lines to a file.
func writeJSONLFile(t *testing.T, path string, lines ...string) {
	t.Helper()
	var buf strings.Builder
	for _, line := range lines {
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(buf.String()), 0o600); err != nil {
		t.Fatalf("failed to write JSONL file %s: %v", path, err)
	}
}

// makeWriteToolLine returns a JSONL assistant line with a Write tool_use for the given file.
func makeWriteToolLine(t *testing.T, uuid, filePath string) string {
	t.Helper()
	data := mustMarshal(t, map[string]interface{}{
		"content": []map[string]interface{}{
			{
				"type":  "tool_use",
				"id":    "toolu_" + uuid,
				"name":  "Write",
				"input": map[string]string{"file_path": filePath},
			},
		},
	})
	line := mustMarshal(t, map[string]interface{}{
		"type":    "assistant",
		"uuid":    uuid,
		"message": json.RawMessage(data),
	})
	return string(line)
}

// makeEditToolLine returns a JSONL assistant line with an Edit tool_use for the given file.
func makeEditToolLine(t *testing.T, uuid, filePath string) string {
	t.Helper()
	data := mustMarshal(t, map[string]interface{}{
		"content": []map[string]interface{}{
			{
				"type":  "tool_use",
				"id":    "toolu_" + uuid,
				"name":  "Edit",
				"input": map[string]string{"file_path": filePath},
			},
		},
	})
	line := mustMarshal(t, map[string]interface{}{
		"type":    "assistant",
		"uuid":    uuid,
		"message": json.RawMessage(data),
	})
	return string(line)
}

// makeTaskToolUseLine returns a JSONL assistant line with a Task tool_use (spawning a subagent).
func makeTaskToolUseLine(t *testing.T, uuid, toolUseID string) string {
	t.Helper()
	data := mustMarshal(t, map[string]interface{}{
		"content": []map[string]interface{}{
			{
				"type":  "tool_use",
				"id":    toolUseID,
				"name":  "Task",
				"input": map[string]string{"prompt": "do something"},
			},
		},
	})
	line := mustMarshal(t, map[string]interface{}{
		"type":    "assistant",
		"uuid":    uuid,
		"message": json.RawMessage(data),
	})
	return string(line)
}

// makeTaskResultLine returns a JSONL user line with a tool_result containing agentId.
func makeTaskResultLine(t *testing.T, uuid, toolUseID, agentID string) string {
	t.Helper()
	data := mustMarshal(t, map[string]interface{}{
		"content": []map[string]interface{}{
			{
				"type":        "tool_result",
				"tool_use_id": toolUseID,
				"content":     "agentId: " + agentID,
			},
		},
	})
	line := mustMarshal(t, map[string]interface{}{
		"type":    "user",
		"uuid":    uuid,
		"message": json.RawMessage(data),
	})
	return string(line)
}

func TestExtractAllModifiedFiles_IncludesSubagentFiles(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	transcriptPath := tmpDir + "/transcript.jsonl"
	subagentsDir := tmpDir + "/tasks/toolu_task1"

	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatalf("failed to create subagents dir: %v", err)
	}

	// Main transcript: Write to main.go + Task call spawning subagent "sub1"
	writeJSONLFile(t, transcriptPath,
		makeWriteToolLine(t, "a1", "/repo/main.go"),
		makeTaskToolUseLine(t, "a2", "toolu_task1"),
		makeTaskResultLine(t, "u1", "toolu_task1", "sub1"),
	)

	// Subagent transcript: Write to helper.go + Edit to utils.go
	writeJSONLFile(t, subagentsDir+"/agent-sub1.jsonl",
		makeWriteToolLine(t, "sa1", "/repo/helper.go"),
		makeEditToolLine(t, "sa2", "/repo/utils.go"),
	)

	files, err := ExtractAllModifiedFiles(transcriptPath, 0, subagentsDir)
	if err != nil {
		t.Fatalf("ExtractAllModifiedFiles() error: %v", err)
	}

	if len(files) != 3 {
		t.Errorf("expected 3 files, got %d: %v", len(files), files)
	}

	wantFiles := map[string]bool{
		"/repo/main.go":   true,
		"/repo/helper.go": true,
		"/repo/utils.go":  true,
	}
	for _, f := range files {
		if !wantFiles[f] {
			t.Errorf("unexpected file %q in result", f)
		}
		delete(wantFiles, f)
	}
	for f := range wantFiles {
		t.Errorf("missing expected file %q", f)
	}
}

func TestExtractAllModifiedFiles_DeduplicatesAcrossAgents(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	transcriptPath := tmpDir + "/transcript.jsonl"
	subagentsDir := tmpDir + "/tasks/toolu_task1"

	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatalf("failed to create subagents dir: %v", err)
	}

	// Main transcript: Write to shared.go + Task call
	writeJSONLFile(t, transcriptPath,
		makeWriteToolLine(t, "a1", "/repo/shared.go"),
		makeTaskToolUseLine(t, "a2", "toolu_task1"),
		makeTaskResultLine(t, "u1", "toolu_task1", "sub1"),
	)

	// Subagent transcript: Also modifies shared.go (same file as main)
	writeJSONLFile(t, subagentsDir+"/agent-sub1.jsonl",
		makeEditToolLine(t, "sa1", "/repo/shared.go"),
	)

	files, err := ExtractAllModifiedFiles(transcriptPath, 0, subagentsDir)
	if err != nil {
		t.Fatalf("ExtractAllModifiedFiles() error: %v", err)
	}

	if len(files) != 1 {
		t.Errorf("expected 1 file (deduplicated), got %d: %v", len(files), files)
	}
	if len(files) > 0 && files[0] != "/repo/shared.go" {
		t.Errorf("expected /repo/shared.go, got %q", files[0])
	}
}

func TestExtractAllModifiedFiles_NoSubagents(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	transcriptPath := tmpDir + "/transcript.jsonl"

	// Main transcript: Write to a file, no Task calls
	writeJSONLFile(t, transcriptPath,
		makeWriteToolLine(t, "a1", "/repo/solo.go"),
	)

	files, err := ExtractAllModifiedFiles(transcriptPath, 0, tmpDir+"/nonexistent")
	if err != nil {
		t.Fatalf("ExtractAllModifiedFiles() error: %v", err)
	}

	if len(files) != 1 {
		t.Errorf("expected 1 file, got %d: %v", len(files), files)
	}
	if len(files) > 0 && files[0] != "/repo/solo.go" {
		t.Errorf("expected /repo/solo.go, got %q", files[0])
	}
}

func TestExtractAllModifiedFiles_SubagentOnlyChanges(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	transcriptPath := tmpDir + "/transcript.jsonl"
	subagentsDir := tmpDir + "/tasks/toolu_task1"

	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatalf("failed to create subagents dir: %v", err)
	}

	// Main transcript: ONLY a Task call, no direct file modifications
	// This is the key bug scenario - if we only look at the main transcript,
	// we miss all the subagent's file changes entirely.
	writeJSONLFile(t, transcriptPath,
		makeTaskToolUseLine(t, "a1", "toolu_task1"),
		makeTaskResultLine(t, "u1", "toolu_task1", "sub1"),
	)

	// Subagent transcript: Write to two files
	writeJSONLFile(t, subagentsDir+"/agent-sub1.jsonl",
		makeWriteToolLine(t, "sa1", "/repo/subagent_file1.go"),
		makeWriteToolLine(t, "sa2", "/repo/subagent_file2.go"),
	)

	files, err := ExtractAllModifiedFiles(transcriptPath, 0, subagentsDir)
	if err != nil {
		t.Fatalf("ExtractAllModifiedFiles() error: %v", err)
	}

	if len(files) != 2 {
		t.Errorf("expected 2 files from subagent, got %d: %v", len(files), files)
	}

	wantFiles := map[string]bool{
		"/repo/subagent_file1.go": true,
		"/repo/subagent_file2.go": true,
	}
	for _, f := range files {
		if !wantFiles[f] {
			t.Errorf("unexpected file %q in result", f)
		}
		delete(wantFiles, f)
	}
	for f := range wantFiles {
		t.Errorf("missing expected file %q", f)
	}
}
