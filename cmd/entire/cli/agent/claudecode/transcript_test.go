package claudecode

import (
	"encoding/json"
	"testing"
)

// Transcript type constants for tests
const (
	testTypeUser      = "user"
	testTypeAssistant = "assistant"
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

	if lines[0].Type != testTypeUser || lines[0].UUID != "u1" {
		t.Errorf("First line = %+v, want type=user, uuid=u1", lines[0])
	}

	if lines[1].Type != testTypeAssistant || lines[1].UUID != "a1" {
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
