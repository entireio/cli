package claudecode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseTranscript(t *testing.T) {
	t.Parallel()

	data := []byte(`{"type":"user","uuid":"u1","message":{"content":"hello"}}
{"type":"assistant","uuid":"a1","message":{"content":[{"type":"text","text":"hi"}]}}
`)

	lines, err := transcript.ParseFromBytes(data)
	if err != nil {
		t.Fatalf("ParseFromBytes() error = %v", err)
	}

	if len(lines) != 2 {
		t.Errorf("ParseFromBytes() got %d lines, want 2", len(lines))
	}

	if lines[0].Type != transcript.TypeUser || lines[0].UUID != "u1" {
		t.Errorf("First line = %+v, want type=user, uuid=u1", lines[0])
	}

	if lines[1].Type != transcript.TypeAssistant || lines[1].UUID != "a1" {
		t.Errorf("Second line = %+v, want type=assistant, uuid=a1", lines[1])
	}
}

func TestExtractSkillEvents_SkillToolUse(t *testing.T) {
	t.Parallel()

	data := []byte(`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","id":"toolu_123","name":"Skill","input":{"skill":"trigger-analysis"}}]}}
`)

	events, err := (&ClaudeCodeAgent{}).ExtractSkillEvents(data, 0)
	if err != nil {
		t.Fatalf("ExtractSkillEvents() error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("ExtractSkillEvents() got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.EventType != agent.SkillEventTypeToolInvocation {
		t.Errorf("EventType = %q", ev.EventType)
	}
	if ev.Skill.Name != "trigger-analysis" {
		t.Errorf("Skill.Name = %q", ev.Skill.Name)
	}
	if ev.Source.Signal != agent.SkillSignalClaudeSkillToolUse || ev.Source.Confidence != agent.SkillConfidenceExplicit {
		t.Errorf("Source = %+v", ev.Source)
	}
	if ev.TranscriptAnchor == nil || ev.TranscriptAnchor.ToolUseID != "toolu_123" {
		t.Errorf("TranscriptAnchor = %+v", ev.TranscriptAnchor)
	}
	if ev.Collapse.Target != agent.SkillCollapseTargetToolPair || !ev.Collapse.DefaultCollapsed {
		t.Errorf("Collapse = %+v", ev.Collapse)
	}
}

func TestParseTranscript_SkipsMalformed(t *testing.T) {
	t.Parallel()

	data := []byte(`{"type":"user","uuid":"u1","message":{"content":"hello"}}
not valid json
{"type":"assistant","uuid":"a1","message":{"content":[]}}
`)

	lines, err := transcript.ParseFromBytes(data)
	if err != nil {
		t.Fatalf("ParseFromBytes() error = %v", err)
	}

	// Should skip the malformed line
	if len(lines) != 2 {
		t.Errorf("ParseFromBytes() got %d lines, want 2 (skipping malformed)", len(lines))
	}
}

func TestExtractModifiedFiles(t *testing.T) {
	t.Parallel()

	data := []byte(`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"foo.go"}}]}}
{"type":"assistant","uuid":"a2","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"bar.go"}}]}}
{"type":"assistant","uuid":"a3","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}
{"type":"assistant","uuid":"a4","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"foo.go"}}]}}
`)

	lines, err := transcript.ParseFromBytes(data)
	if err != nil {
		t.Fatalf("ParseFromBytes() error = %v", err)
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

// Token calculation tests - Claude Code specific token format

func TestCalculateTokenUsage_BasicMessages(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

	usage := CalculateTokenUsage(nil)

	if usage.APICallCount != 0 {
		t.Errorf("APICallCount = %d, want 0", usage.APICallCount)
	}
	if usage.InputTokens != 0 {
		t.Errorf("InputTokens = %d, want 0", usage.InputTokens)
	}
}

func TestExtractSpawnedAgentIDs_FromToolResult(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

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
			t.Parallel()

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

	transcriptData := []byte(
		`{"type":"user","uuid":"u1","message":{"content":"first prompt"}}` + "\n" +
			`{"type":"assistant","uuid":"a1","message":{"id":"m1","usage":{"input_tokens":100,"output_tokens":50}}}` + "\n" +
			`{"type":"user","uuid":"u2","message":{"content":"second prompt"}}` + "\n" +
			`{"type":"assistant","uuid":"a2","message":{"id":"m2","usage":{"input_tokens":200,"output_tokens":100}}}` + "\n" +
			`{"type":"user","uuid":"u3","message":{"content":"third prompt"}}` + "\n" +
			`{"type":"assistant","uuid":"a3","message":{"id":"m3","usage":{"input_tokens":300,"output_tokens":150}}}` + "\n",
	)

	c := &ClaudeCodeAgent{}

	// Test 1: From line 0 - all 3 turns = 600 input, 300 output
	usage1, err := c.CalculateTotalTokenUsage(transcriptData, 0, "")
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
	usage2, err := c.CalculateTotalTokenUsage(transcriptData, 2, "")
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
	usage3, err := c.CalculateTotalTokenUsage(transcriptData, 4, "")
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

// buildJSONL is a test helper that builds JSONL bytes from transcript lines.
func buildJSONL(lines ...string) []byte {
	var buf strings.Builder
	for _, line := range lines {
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	return []byte(buf.String())
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
	subagentsDir := tmpDir + "/tasks/toolu_task1"
	c := &ClaudeCodeAgent{}

	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatalf("failed to create subagents dir: %v", err)
	}

	// Main transcript as bytes: Write to main.go + Task call spawning subagent "sub1"
	transcriptData := buildJSONL(
		makeWriteToolLine(t, "a1", "/repo/main.go"),
		makeTaskToolUseLine(t, "a2", "toolu_task1"),
		makeTaskResultLine(t, "u1", "toolu_task1", "sub1"),
	)

	// Subagent transcript: Write to helper.go + Edit to utils.go (still on disk)
	writeJSONLFile(t, subagentsDir+"/agent-sub1.jsonl",
		makeWriteToolLine(t, "sa1", "/repo/helper.go"),
		makeEditToolLine(t, "sa2", "/repo/utils.go"),
	)

	files, err := c.ExtractAllModifiedFiles(transcriptData, 0, subagentsDir)
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
	subagentsDir := tmpDir + "/tasks/toolu_task1"
	c := &ClaudeCodeAgent{}

	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatalf("failed to create subagents dir: %v", err)
	}

	// Main transcript as bytes: Write to shared.go + Task call
	transcriptData := buildJSONL(
		makeWriteToolLine(t, "a1", "/repo/shared.go"),
		makeTaskToolUseLine(t, "a2", "toolu_task1"),
		makeTaskResultLine(t, "u1", "toolu_task1", "sub1"),
	)

	// Subagent transcript: Also modifies shared.go (same file as main)
	writeJSONLFile(t, subagentsDir+"/agent-sub1.jsonl",
		makeEditToolLine(t, "sa1", "/repo/shared.go"),
	)

	files, err := c.ExtractAllModifiedFiles(transcriptData, 0, subagentsDir)
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
	c := &ClaudeCodeAgent{}

	// Main transcript as bytes: Write to a file, no Task calls
	transcriptData := buildJSONL(
		makeWriteToolLine(t, "a1", "/repo/solo.go"),
	)

	files, err := c.ExtractAllModifiedFiles(transcriptData, 0, tmpDir+"/nonexistent")
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
	subagentsDir := tmpDir + "/tasks/toolu_task1"
	c := &ClaudeCodeAgent{}

	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatalf("failed to create subagents dir: %v", err)
	}

	// Main transcript as bytes: ONLY a Task call, no direct file modifications
	// This is the key bug scenario - if we only look at the main transcript,
	// we miss all the subagent's file changes entirely.
	transcriptData := buildJSONL(
		makeTaskToolUseLine(t, "a1", "toolu_task1"),
		makeTaskResultLine(t, "u1", "toolu_task1", "sub1"),
	)

	// Subagent transcript: Write to two files (still on disk)
	writeJSONLFile(t, subagentsDir+"/agent-sub1.jsonl",
		makeWriteToolLine(t, "sa1", "/repo/subagent_file1.go"),
		makeWriteToolLine(t, "sa2", "/repo/subagent_file2.go"),
	)

	files, err := c.ExtractAllModifiedFiles(transcriptData, 0, subagentsDir)
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

// Regression for #329: a subagent spawned BEFORE the checkpoint's startLine
// must still be discovered, because it can keep modifying files in later turns.
// The Task spawn/result live in lines before startLine; only the full transcript
// scan finds them.
func TestExtractAllModifiedFiles_FindsSubagentSpawnedBeforeStartLine(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	subagentsDir := tmpDir + "/tasks/toolu_task1"
	c := &ClaudeCodeAgent{}
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatalf("failed to create subagents dir: %v", err)
	}

	transcriptData := buildJSONL(
		makeTaskToolUseLine(t, "a1", "toolu_taskA"),        // line 0 (before startLine)
		makeTaskResultLine(t, "uA", "toolu_taskA", "subA"), // line 1 (before startLine)
		makeWriteToolLine(t, "a2", "/repo/main.go"),        // line 2 (>= startLine)
	)
	writeJSONLFile(t, subagentsDir+"/agent-subA.jsonl",
		makeWriteToolLine(t, "sa1", "/repo/helper.go"),
	)

	files, err := c.ExtractAllModifiedFiles(transcriptData, 2, subagentsDir)
	if err != nil {
		t.Fatalf("ExtractAllModifiedFiles() error: %v", err)
	}

	got := make(map[string]bool, len(files))
	for _, f := range files {
		got[f] = true
	}
	if !got["/repo/main.go"] {
		t.Errorf("missing main-agent file /repo/main.go: %v", files)
	}
	if !got["/repo/helper.go"] {
		t.Errorf("subagent spawned before startLine was not discovered; missing /repo/helper.go: %v", files)
	}
}

// Regression for #329: subagent token usage must be counted even when the
// subagent was spawned before the checkpoint's startLine.
func TestCalculateTotalTokenUsage_CountsSubagentSpawnedBeforeStartLine(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	subagentsDir := tmpDir + "/tasks/toolu_task1"
	c := &ClaudeCodeAgent{}
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatalf("failed to create subagents dir: %v", err)
	}

	// Subagent spawned in lines 0-1 (before startLine=2); main usage on line 2.
	transcriptData := buildJSONL(
		makeTaskToolUseLine(t, "a1", "toolu_taskB"),
		makeTaskResultLine(t, "uB", "toolu_taskB", "subB"),
		`{"type":"assistant","uuid":"a2","message":{"id":"m2","usage":{"input_tokens":300,"output_tokens":150}}}`,
	)
	writeJSONLFile(t, subagentsDir+"/agent-subB.jsonl",
		`{"type":"assistant","uuid":"sa1","message":{"id":"sm1","usage":{"input_tokens":50,"output_tokens":25}}}`,
	)

	usage, err := c.CalculateTotalTokenUsage(transcriptData, 2, subagentsDir)
	if err != nil {
		t.Fatalf("CalculateTotalTokenUsage() error: %v", err)
	}
	if usage.SubagentTokens == nil {
		t.Fatal("subagent spawned before startLine was not counted (SubagentTokens is nil)")
	}
	if usage.SubagentTokens.InputTokens != 50 || usage.SubagentTokens.OutputTokens != 25 {
		t.Errorf("subagent tokens = input %d output %d, want input 50 output 25",
			usage.SubagentTokens.InputTokens, usage.SubagentTokens.OutputTokens)
	}
}

// makeWorkflowLaunchLines returns the parent-transcript lines of a Workflow
// call and its async_launched result. Claude Code (2.1.291) names the run in
// the result text as "Run ID: <runId>"; it names no agent IDs.
func makeWorkflowLaunchLines(t *testing.T, toolUseID, runID string) []string {
	t.Helper()
	use := mustMarshal(t, map[string]interface{}{
		"type": "assistant", "uuid": "wf-use-" + toolUseID,
		"message": map[string]interface{}{"content": []map[string]interface{}{
			{"type": "tool_use", "id": toolUseID, "name": "Workflow", "input": map[string]string{"name": "append-lines"}},
		}},
	})
	result := mustMarshal(t, map[string]interface{}{
		"type": "user", "uuid": "wf-result-" + toolUseID,
		"message": map[string]interface{}{"content": []map[string]interface{}{
			{"type": "tool_result", "tool_use_id": toolUseID, "content": "Workflow launched in background. Task ID: w73qhtvvh\nSummary: Three agents\nRun ID: " + runID + "\nTranscript dir: <session>/subagents/workflows/" + runID},
		}},
	})
	return []string{string(use), string(result)}
}

// TestCalculateTotalTokenUsage_IncludesWorkflowAgents is the #2685 token
// regression. Workflow agents are launched by a background Workflow tool call
// whose result names a run but no agent IDs, so the "agentId:" scan never
// finds them; Claude Code writes their transcripts under
// <subagentsDir>/workflows/<runId>/agent-<id>.jsonl beside a meta file per
// agent and a run journal. The layout mirrors a real Claude Code 2.1.291 run.
func TestCalculateTotalTokenUsage_IncludesWorkflowAgents(t *testing.T) {
	t.Parallel()

	subagentsDir := filepath.Join(t.TempDir(), "sess", "subagents")
	runDir := filepath.Join(subagentsDir, "workflows", "wf_e5264e60-494")
	require.NoError(t, os.MkdirAll(filepath.Join(runDir, "nested"), 0o755))

	// The parent launched one direct Agent subagent (found via "agentId:") and
	// one Workflow, whose tool result carries a run ID but no agent IDs.
	lines := []string{
		makeTaskToolUseLine(t, "a1", "toolu_direct"),
		makeTaskResultLine(t, "u1", "toolu_direct", "direct1"),
	}
	lines = append(lines, makeWorkflowLaunchLines(t, "toolu_wf", "wf_e5264e60-494")...)
	lines = append(lines, `{"type":"assistant","uuid":"a2","message":{"id":"m-main","usage":{"input_tokens":1000,"output_tokens":100}}}`)
	writeJSONLFile(t, filepath.Join(subagentsDir, "agent-direct1.jsonl"),
		`{"type":"assistant","uuid":"d1","message":{"id":"m-direct","usage":{"input_tokens":1,"output_tokens":2}}}`)

	// Two workflow agents. The second streams one message as two rows; only
	// the final row counts.
	writeJSONLFile(t, filepath.Join(runDir, "agent-ae3d7b8f2930c8787.jsonl"),
		`{"type":"user","isSidechain":true,"agentId":"ae3d7b8f2930c8787","message":{"role":"user","content":"append a line"}}`,
		`{"type":"assistant","isSidechain":true,"agentId":"ae3d7b8f2930c8787","message":{"id":"m-w1","usage":{"input_tokens":10,"cache_creation_input_tokens":20,"cache_read_input_tokens":30,"output_tokens":40}}}`)
	writeJSONLFile(t, filepath.Join(runDir, "agent-ac82c55f48a03882b.jsonl"),
		`{"type":"assistant","isSidechain":true,"agentId":"ac82c55f48a03882b","message":{"id":"m-w2","usage":{"input_tokens":100,"output_tokens":1}}}`,
		`{"type":"assistant","isSidechain":true,"agentId":"ac82c55f48a03882b","message":{"id":"m-w2","usage":{"input_tokens":100,"output_tokens":200}}}`)

	// Not agent transcripts: the per-agent meta file, the run journal, a file
	// one level too deep, a copy of the direct agent's transcript (counted
	// once, from its own location), and a run this transcript never launched.
	require.NoError(t, os.WriteFile(filepath.Join(runDir, "agent-ae3d7b8f2930c8787.meta.json"),
		[]byte(`{"agentType":"workflow-subagent","description":"agent 1"}`), 0o600))
	writeJSONLFile(t, filepath.Join(runDir, "journal.jsonl"),
		`{"type":"assistant","message":{"id":"m-journal","usage":{"input_tokens":5000,"output_tokens":5000}}}`)
	writeJSONLFile(t, filepath.Join(runDir, "nested", "agent-deep.jsonl"),
		`{"type":"assistant","message":{"id":"m-deep","usage":{"input_tokens":7000,"output_tokens":7000}}}`)
	writeJSONLFile(t, filepath.Join(runDir, "agent-direct1.jsonl"),
		`{"type":"assistant","message":{"id":"m-direct","usage":{"input_tokens":9000,"output_tokens":9000}}}`)
	require.NoError(t, os.MkdirAll(filepath.Join(subagentsDir, "workflows", "wf_unlaunched"), 0o755))
	writeJSONLFile(t, filepath.Join(subagentsDir, "workflows", "wf_unlaunched", "agent-other.jsonl"),
		`{"type":"assistant","message":{"id":"m-other","usage":{"input_tokens":8000,"output_tokens":8000}}}`)

	usage, err := (&ClaudeCodeAgent{}).CalculateTotalTokenUsage(buildJSONL(lines...), 0, subagentsDir)
	require.NoError(t, err)

	assert.Equal(t, 1000, usage.InputTokens, "main usage must not absorb subagent usage")
	assert.Equal(t, 100, usage.OutputTokens)
	require.NotNil(t, usage.SubagentTokens, "workflow agents were not counted")
	assert.Equal(t, agent.TokenUsage{
		InputTokens:         1 + 10 + 100,
		CacheCreationTokens: 20,
		CacheReadTokens:     30,
		OutputTokens:        2 + 40 + 200,
		APICallCount:        3,
	}, *usage.SubagentTokens)
}

// TestCalculateTotalTokenUsage_WorkflowAgentsFollowTheTranscriptPrefix: a
// run's agents count only once the transcript has launched the run. Import
// splits a session into turns and computes each turn's subagent total from
// the transcript prefix up to that turn, then rescopes consecutive totals into
// per-turn deltas; counting every run on disk would put every workflow's
// tokens on the first turn.
func TestCalculateTotalTokenUsage_WorkflowAgentsFollowTheTranscriptPrefix(t *testing.T) {
	t.Parallel()

	subagentsDir := filepath.Join(t.TempDir(), "sess", "subagents")
	runDir := filepath.Join(subagentsDir, "workflows", "wf_1")
	require.NoError(t, os.MkdirAll(runDir, 0o755))
	writeJSONLFile(t, filepath.Join(runDir, "agent-a6d78754c07df829a.jsonl"),
		`{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":3,"output_tokens":4}}}`)

	before := []string{`{"type":"assistant","uuid":"a1","message":{"id":"m0","usage":{"input_tokens":1,"output_tokens":1}}}`}
	after := append(append([]string{}, before...), makeWorkflowLaunchLines(t, "toolu_wf", "wf_1")...)

	usage, err := (&ClaudeCodeAgent{}).CalculateTotalTokenUsage(buildJSONL(before...), 0, subagentsDir)
	require.NoError(t, err)
	assert.Nil(t, usage.SubagentTokens, "a run the prefix has not launched must not count")

	usage, err = (&ClaudeCodeAgent{}).CalculateTotalTokenUsage(buildJSONL(after...), 0, subagentsDir)
	require.NoError(t, err)
	require.NotNil(t, usage.SubagentTokens)
	assert.Equal(t, 3, usage.SubagentTokens.InputTokens)
	assert.Equal(t, 4, usage.SubagentTokens.OutputTokens)
	assert.Equal(t, 1, usage.SubagentTokens.APICallCount)
}

// TestCalculateTotalTokenUsage_AgentIDResultDoesNotHideWorkflowTranscript: an
// agent ID named by an "agentId:" result whose only transcript is in a
// launched Workflow run is still counted, from the run directory.
func TestCalculateTotalTokenUsage_AgentIDResultDoesNotHideWorkflowTranscript(t *testing.T) {
	t.Parallel()

	subagentsDir := filepath.Join(t.TempDir(), "sess", "subagents")
	runDir := filepath.Join(subagentsDir, "workflows", "wf_1")
	require.NoError(t, os.MkdirAll(runDir, 0o755))
	writeJSONLFile(t, filepath.Join(runDir, "agent-shared1.jsonl"),
		`{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":3,"output_tokens":4}}}`)

	lines := []string{
		makeTaskToolUseLine(t, "a1", "toolu_send"),
		makeTaskResultLine(t, "u1", "toolu_send", "shared1"),
	}
	lines = append(lines, makeWorkflowLaunchLines(t, "toolu_wf", "wf_1")...)

	usage, err := (&ClaudeCodeAgent{}).CalculateTotalTokenUsage(buildJSONL(lines...), 0, subagentsDir)
	require.NoError(t, err)
	require.NotNil(t, usage.SubagentTokens, "the workflow transcript was dropped for the absent direct path")
	assert.Equal(t, 3, usage.SubagentTokens.InputTokens)
}

// TestCalculateTotalTokenUsage_WorkflowRunFromStructuredResult pins that a
// Workflow run is found from the launch's structured toolUseResult even when
// the result text does not use the "Run ID:" wording, so a change in Claude
// Code's prose cannot silently drop workflow agents' tokens.
func TestCalculateTotalTokenUsage_WorkflowRunFromStructuredResult(t *testing.T) {
	t.Parallel()

	subagentsDir := filepath.Join(t.TempDir(), "sess", "subagents")
	runDir := filepath.Join(subagentsDir, "workflows", "wf_e5264e60-494")
	require.NoError(t, os.MkdirAll(runDir, 0o755))
	writeJSONLFile(t, filepath.Join(runDir, "agent-ae3d7b8f2930c8787.jsonl"),
		`{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":5,"output_tokens":6}}}`)

	result := mustMarshal(t, map[string]interface{}{
		"type": "user", "uuid": "wf-result",
		"message": map[string]interface{}{"content": []map[string]interface{}{
			{"type": "tool_result", "tool_use_id": "toolu_wf", "content": "Workflow started."},
		}},
		"toolUseResult": map[string]interface{}{
			"status": "async_launched", "taskType": "local_workflow", "runId": "wf_e5264e60-494",
		},
	})
	usage, err := (&ClaudeCodeAgent{}).CalculateTotalTokenUsage(buildJSONL(string(result)), 0, subagentsDir)
	require.NoError(t, err)
	require.NotNil(t, usage.SubagentTokens)
	assert.Equal(t, 5, usage.SubagentTokens.InputTokens)
	assert.Equal(t, 6, usage.SubagentTokens.OutputTokens)
}

// TestExtractWorkflowRunIDs_EveryRunInAResult pins that a result naming more
// than one run (e.g. a resumed run citing its source) yields each of them.
func TestExtractWorkflowRunIDs_EveryRunInAResult(t *testing.T) {
	t.Parallel()

	result := mustMarshal(t, map[string]interface{}{
		"type": "user", "uuid": "wf-result",
		"message": map[string]interface{}{"content": []map[string]interface{}{
			{"type": "tool_result", "tool_use_id": "toolu_wf", "content": "Run ID: wf_new-1\nResumed from Run ID: wf_old-2\nRun ID: wf_new-1"},
		}},
	})
	parsed, err := transcript.ParseFromBytes(buildJSONL(string(result)))
	require.NoError(t, err)
	assert.Equal(t, []string{"wf_new-1", "wf_old-2"}, ExtractWorkflowRunIDs(parsed))
}

// TestCalculateTotalTokenUsage_SameWorkflowAgentInTwoRunsCountsOnce pins that
// an agent ID present in two launched runs is counted once, from its newest
// transcript, not summed or taken from whichever run is read last.
func TestCalculateTotalTokenUsage_SameWorkflowAgentInTwoRunsCountsOnce(t *testing.T) {
	t.Parallel()

	subagentsDir := filepath.Join(t.TempDir(), "sess", "subagents")
	const agentID = "ae3d7b8f2930c8787"
	older := filepath.Join(subagentsDir, "workflows", "wf_2", "agent-"+agentID+".jsonl")
	newer := filepath.Join(subagentsDir, "workflows", "wf_1", "agent-"+agentID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(older), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(newer), 0o755))
	writeJSONLFile(t, older, `{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":1,"output_tokens":1}}}`)
	writeJSONLFile(t, newer, `{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":7,"output_tokens":8}}}`)
	base := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(older, base, base))
	require.NoError(t, os.Chtimes(newer, base.Add(time.Minute), base.Add(time.Minute)))

	lines := append(makeWorkflowLaunchLines(t, "toolu_wf1", "wf_1"), makeWorkflowLaunchLines(t, "toolu_wf2", "wf_2")...)
	usage, err := (&ClaudeCodeAgent{}).CalculateTotalTokenUsage(buildJSONL(lines...), 0, subagentsDir)
	require.NoError(t, err)
	require.NotNil(t, usage.SubagentTokens)
	assert.Equal(t, 7, usage.SubagentTokens.InputTokens)
	assert.Equal(t, 8, usage.SubagentTokens.OutputTokens)
	assert.Equal(t, 1, usage.SubagentTokens.APICallCount)
}

// forkedSkillResultLine is the parent transcript's tool_result line for a
// Skill call that ran forked (Claude Code 2.1.291): the text does not name the
// agent; the structured toolUseResult does.
func forkedSkillResultLine(toolUseID, agentID string) string {
	return fmt.Sprintf(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"Skill \"fanout-fork\" launched (forked execution, running in the background)."}]},`+
		`"toolUseResult":{"success":true,"commandName":"fanout-fork","status":"forked","background":true,"agentId":%q}}`, toolUseID, agentID)
}

func TestForkedSkillAgentIDs(t *testing.T) {
	t.Parallel()

	data := buildJSONL(
		forkedSkillResultLine("toolu_skill", "a80ff32f89f7dadc4"),
		// An inline skill launched no agent.
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_inline","content":"Launching skill: fanout"}]},"toolUseResult":{"success":true,"commandName":"fanout"}}`,
		// Not path-safe: dropped.
		forkedSkillResultLine("toolu_bad", "../escape"),
		// The same shape on an assistant line is not a tool result.
		`{"type":"assistant","toolUseResult":{"status":"forked","agentId":"aother"}}`,
	)

	assert.Equal(t, map[string]string{"a80ff32f89f7dadc4": "toolu_skill"}, forkedSkillAgentIDs(data))
}

// A forked skill's agent writes its transcript beside an Agent call's, and
// its tokens and files belong to the parent session just the same.
func TestForkedSkillAgentCountsTowardSession(t *testing.T) {
	t.Parallel()

	subagentsDir := filepath.Join(t.TempDir(), "sess", "subagents")
	require.NoError(t, os.MkdirAll(subagentsDir, 0o755))
	data := buildJSONL(
		forkedSkillResultLine("toolu_skill", "a80ff32f89f7dadc4"),
		`{"type":"assistant","uuid":"a2","message":{"id":"m-main","usage":{"input_tokens":1000,"output_tokens":100}}}`,
	)
	writeJSONLFile(t, filepath.Join(subagentsDir, "agent-a80ff32f89f7dadc4.jsonl"),
		makeWriteToolLine(t, "f1", "/repo/c.txt"),
		`{"type":"assistant","uuid":"f2","message":{"id":"m-fork","usage":{"input_tokens":10,"output_tokens":20}}}`)

	c := &ClaudeCodeAgent{}
	usage, err := c.CalculateTotalTokenUsage(data, 0, subagentsDir)
	require.NoError(t, err)
	assert.Equal(t, 1000, usage.InputTokens, "main usage must not absorb the forked agent's")
	require.NotNil(t, usage.SubagentTokens, "forked skill agent was not counted")
	assert.Equal(t, 10, usage.SubagentTokens.InputTokens)
	assert.Equal(t, 20, usage.SubagentTokens.OutputTokens)

	files, err := c.ExtractAllModifiedFiles(data, 0, subagentsDir)
	require.NoError(t, err)
	assert.Contains(t, files, "/repo/c.txt")
}
