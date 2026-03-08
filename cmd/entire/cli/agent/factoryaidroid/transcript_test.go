package factoryaidroid

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

func TestParseDroidTranscript_NormalizesEnvelope(t *testing.T) {
	t.Parallel()

	// Real Droid format: type is always "message", role is inside the inner message
	data := []byte(
		`{"type":"session_start","id":"sess-1","title":"test"}` + "\n" +
			`{"type":"message","id":"m1","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}` + "\n" +
			`{"type":"message","id":"m2","message":{"role":"assistant","content":[{"type":"text","text":"hi there"}]}}` + "\n",
	)

	lines, _, err := ParseDroidTranscriptFromBytes(data, 0)
	if err != nil {
		t.Fatalf("ParseDroidTranscriptFromBytes() error = %v", err)
	}

	// session_start should be skipped
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2 (session_start should be skipped)", len(lines))
	}

	// First line should be normalized to type="user"
	if lines[0].Type != transcript.TypeUser {
		t.Errorf("lines[0].Type = %q, want %q", lines[0].Type, transcript.TypeUser)
	}
	if lines[0].UUID != "m1" {
		t.Errorf("lines[0].UUID = %q, want \"m1\"", lines[0].UUID)
	}

	// Second line should be normalized to type="assistant"
	if lines[1].Type != transcript.TypeAssistant {
		t.Errorf("lines[1].Type = %q, want %q", lines[1].Type, transcript.TypeAssistant)
	}
	if lines[1].UUID != "m2" {
		t.Errorf("lines[1].UUID = %q, want \"m2\"", lines[1].UUID)
	}
}

func TestParseDroidTranscript_StartLineOffset(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	path := tmpDir + "/transcript.jsonl"

	data := []byte(
		`{"type":"session_start","id":"s1"}` + "\n" +
			`{"type":"message","id":"m1","message":{"role":"user","content":"hello"}}` + "\n" +
			`{"type":"message","id":"m2","message":{"role":"assistant","content":"hi"}}` + "\n" +
			`{"type":"message","id":"m3","message":{"role":"user","content":"bye"}}` + "\n",
	)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("failed to write: %v", err)
	}

	// Read from line 2 onward (skip session_start + first message)
	lines, totalLines, err := ParseDroidTranscript(path, 2)
	if err != nil {
		t.Fatalf("ParseDroidTranscript() error = %v", err)
	}

	if totalLines != 4 {
		t.Errorf("totalLines = %d, want 4", totalLines)
	}

	// Lines 2 and 3 are messages, both should be parsed
	if len(lines) != 2 {
		t.Fatalf("got %d lines from offset 2, want 2", len(lines))
	}
	if lines[0].Type != transcript.TypeAssistant {
		t.Errorf("lines[0].Type = %q, want %q", lines[0].Type, transcript.TypeAssistant)
	}
	if lines[1].Type != transcript.TypeUser {
		t.Errorf("lines[1].Type = %q, want %q", lines[1].Type, transcript.TypeUser)
	}
}

func TestParseDroidTranscriptFromBytes_StartLineSkipsNonMessageEntries(t *testing.T) {
	t.Parallel()

	// Transcript: session_start(0), message(1), session_event(2), message(3), message(4)
	// Raw line indices:  0            1            2                3            4
	data := []byte(
		`{"type":"session_start","id":"s1"}` + "\n" +
			`{"type":"message","id":"m1","message":{"role":"user","content":"hello"}}` + "\n" +
			`{"type":"session_event","data":"some event"}` + "\n" +
			`{"type":"message","id":"m2","message":{"role":"assistant","content":"hi"}}` + "\n" +
			`{"type":"message","id":"m3","message":{"role":"user","content":"bye"}}` + "\n",
	)

	// With startLine=0, all 3 messages should be returned
	allLines, _, err := ParseDroidTranscriptFromBytes(data, 0)
	if err != nil {
		t.Fatalf("ParseDroidTranscriptFromBytes(0) error = %v", err)
	}
	if len(allLines) != 3 {
		t.Fatalf("startLine=0: got %d lines, want 3", len(allLines))
	}

	// With startLine=2, skip raw lines 0-1 (session_start + m1).
	// Lines 2 (session_event) is skipped by filter, lines 3-4 (m2, m3) are messages.
	fromLine2, _, err := ParseDroidTranscriptFromBytes(data, 2)
	if err != nil {
		t.Fatalf("ParseDroidTranscriptFromBytes(2) error = %v", err)
	}
	if len(fromLine2) != 2 {
		t.Fatalf("startLine=2: got %d lines, want 2", len(fromLine2))
	}
	if fromLine2[0].UUID != "m2" {
		t.Errorf("startLine=2: lines[0].UUID = %q, want \"m2\"", fromLine2[0].UUID)
	}
	if fromLine2[1].UUID != "m3" {
		t.Errorf("startLine=2: lines[1].UUID = %q, want \"m3\"", fromLine2[1].UUID)
	}

	// With startLine=3, skip raw lines 0-2 (session_start + m1 + session_event).
	// Lines 3-4 (m2, m3) are messages.
	fromLine3, _, err := ParseDroidTranscriptFromBytes(data, 3)
	if err != nil {
		t.Fatalf("ParseDroidTranscriptFromBytes(3) error = %v", err)
	}
	if len(fromLine3) != 2 {
		t.Fatalf("startLine=3: got %d lines, want 2", len(fromLine3))
	}
	if fromLine3[0].UUID != "m2" {
		t.Errorf("startLine=3: lines[0].UUID = %q, want \"m2\"", fromLine3[0].UUID)
	}

	// With startLine beyond end, should return no lines
	beyondEnd, _, err := ParseDroidTranscriptFromBytes(data, 100)
	if err != nil {
		t.Fatalf("ParseDroidTranscriptFromBytes(100) error = %v", err)
	}
	if len(beyondEnd) != 0 {
		t.Fatalf("startLine=100: got %d lines, want 0", len(beyondEnd))
	}
}

func TestParseDroidTranscript_RealDroidFormat(t *testing.T) {
	t.Parallel()

	// Test with a realistic Droid transcript snippet including tool use
	data := []byte(
		`{"type":"session_start","id":"5734e7ee","title":"test session"}` + "\n" +
			`{"type":"message","id":"msg-1","message":{"role":"user","content":[{"type":"text","text":"update main.go"}]}}` + "\n" +
			`{"type":"message","id":"msg-2","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_01","name":"Edit","input":{"file_path":"/repo/main.go","old_str":"old","new_str":"new"}}]}}` + "\n" +
			`{"type":"message","id":"msg-3","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":"success"}]}}` + "\n" +
			`{"type":"message","id":"msg-4","message":{"role":"assistant","content":[{"type":"text","text":"Done!"}]}}` + "\n",
	)

	lines, _, err := ParseDroidTranscriptFromBytes(data, 0)
	if err != nil {
		t.Fatalf("ParseDroidTranscriptFromBytes() error = %v", err)
	}

	if len(lines) != 4 {
		t.Fatalf("got %d lines, want 4", len(lines))
	}

	// Verify ExtractModifiedFiles works with the parsed Droid lines
	files := ExtractModifiedFiles(lines)
	if len(files) != 1 {
		t.Fatalf("ExtractModifiedFiles() got %d files, want 1", len(files))
	}
	if files[0] != "/repo/main.go" {
		t.Errorf("ExtractModifiedFiles() got %q, want /repo/main.go", files[0])
	}
}

func TestExtractModifiedFiles(t *testing.T) {
	t.Parallel()

	// Droid format: {"type":"message","id":"...","message":{"role":"assistant","content":[...]}}
	data := []byte(`{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"tool_use","name":"Write","input":{"file_path":"foo.go"}}]}}
{"type":"message","id":"a2","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":"bar.go"}}]}}
{"type":"message","id":"a3","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}
{"type":"message","id":"a4","message":{"role":"assistant","content":[{"type":"tool_use","name":"Write","input":{"file_path":"foo.go"}}]}}
`)

	lines, _, err := ParseDroidTranscriptFromBytes(data, 0)
	if err != nil {
		t.Fatalf("ParseDroidTranscriptFromBytes() error = %v", err)
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

func TestExtractModifiedFiles_Empty(t *testing.T) {
	t.Parallel()

	files := ExtractModifiedFiles(nil)
	if files != nil {
		t.Errorf("ExtractModifiedFiles(nil) = %v, want nil", files)
	}
}

func TestCalculateTokenUsage_BasicMessages(t *testing.T) {
	t.Parallel()

	lines := []TranscriptLine{
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

	usage := CalculateTokenUsage(lines)

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
	lines := []TranscriptLine{
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

	usage := CalculateTokenUsage(lines)

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

	lines := []TranscriptLine{
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

	usage := CalculateTokenUsage(lines)

	if usage.APICallCount != 1 {
		t.Errorf("APICallCount = %d, want 1", usage.APICallCount)
	}
}

func TestExtractSpawnedAgentIDs_FromToolResult(t *testing.T) {
	t.Parallel()

	lines := []TranscriptLine{
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

	agentIDs := ExtractSpawnedAgentIDs(lines)

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

	lines := []TranscriptLine{
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

	agentIDs := ExtractSpawnedAgentIDs(lines)

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

	lines := []TranscriptLine{
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

	agentIDs := ExtractSpawnedAgentIDs(lines)

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

func TestCalculateTotalTokenUsageFromTranscript_PerCheckpoint(t *testing.T) {
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

	// Droid format: outer type is always "message", role is inside the inner message
	transcriptContent := []byte(
		`{"type":"message","id":"u1","message":{"role":"user","content":"first prompt"}}` + "\n" +
			`{"type":"message","id":"a1","message":{"role":"assistant","id":"m1","usage":{"input_tokens":100,"output_tokens":50}}}` + "\n" +
			`{"type":"message","id":"u2","message":{"role":"user","content":"second prompt"}}` + "\n" +
			`{"type":"message","id":"a2","message":{"role":"assistant","id":"m2","usage":{"input_tokens":200,"output_tokens":100}}}` + "\n" +
			`{"type":"message","id":"u3","message":{"role":"user","content":"third prompt"}}` + "\n" +
			`{"type":"message","id":"a3","message":{"role":"assistant","id":"m3","usage":{"input_tokens":300,"output_tokens":150}}}` + "\n",
	)
	if err := os.WriteFile(transcriptPath, transcriptContent, 0o600); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Test 1: From line 0 - all 3 turns = 600 input, 300 output
	usage1, err := CalculateTotalTokenUsageFromTranscript(transcriptPath, 0, "")
	if err != nil {
		t.Fatalf("CalculateTotalTokenUsageFromTranscript(0) error: %v", err)
	}
	if usage1.InputTokens != 600 || usage1.OutputTokens != 300 {
		t.Errorf("From line 0: got input=%d output=%d, want input=600 output=300",
			usage1.InputTokens, usage1.OutputTokens)
	}
	if usage1.APICallCount != 3 {
		t.Errorf("From line 0: got APICallCount=%d, want 3", usage1.APICallCount)
	}

	// Test 2: From line 2 (after turn 1) - turns 2+3 only = 500 input, 250 output
	usage2, err := CalculateTotalTokenUsageFromTranscript(transcriptPath, 2, "")
	if err != nil {
		t.Fatalf("CalculateTotalTokenUsageFromTranscript(2) error: %v", err)
	}
	if usage2.InputTokens != 500 || usage2.OutputTokens != 250 {
		t.Errorf("From line 2: got input=%d output=%d, want input=500 output=250",
			usage2.InputTokens, usage2.OutputTokens)
	}
	if usage2.APICallCount != 2 {
		t.Errorf("From line 2: got APICallCount=%d, want 2", usage2.APICallCount)
	}

	// Test 3: From line 4 (after turns 1+2) - turn 3 only = 300 input, 150 output
	usage3, err := CalculateTotalTokenUsageFromTranscript(transcriptPath, 4, "")
	if err != nil {
		t.Fatalf("CalculateTotalTokenUsageFromTranscript(4) error: %v", err)
	}
	if usage3.InputTokens != 300 || usage3.OutputTokens != 150 {
		t.Errorf("From line 4: got input=%d output=%d, want input=300 output=150",
			usage3.InputTokens, usage3.OutputTokens)
	}
	if usage3.APICallCount != 1 {
		t.Errorf("From line 4: got APICallCount=%d, want 1", usage3.APICallCount)
	}
}

func TestExtractAllModifiedFilesFromTranscript_IncludesSubagentFiles(t *testing.T) {
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

	files, err := ExtractAllModifiedFilesFromTranscript(transcriptPath, 0, subagentsDir)
	if err != nil {
		t.Fatalf("ExtractAllModifiedFilesFromTranscript() error: %v", err)
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

func TestExtractAllModifiedFilesFromTranscript_DeduplicatesAcrossAgents(t *testing.T) {
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

	files, err := ExtractAllModifiedFilesFromTranscript(transcriptPath, 0, subagentsDir)
	if err != nil {
		t.Fatalf("ExtractAllModifiedFilesFromTranscript() error: %v", err)
	}

	if len(files) != 1 {
		t.Errorf("expected 1 file (deduplicated), got %d: %v", len(files), files)
	}
	if len(files) > 0 && files[0] != "/repo/shared.go" {
		t.Errorf("expected /repo/shared.go, got %q", files[0])
	}
}

func TestExtractAllModifiedFilesFromTranscript_NoSubagents(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	transcriptPath := tmpDir + "/transcript.jsonl"

	// Main transcript: Write to a file, no Task calls
	writeJSONLFile(t, transcriptPath,
		makeWriteToolLine(t, "a1", "/repo/solo.go"),
	)

	files, err := ExtractAllModifiedFilesFromTranscript(transcriptPath, 0, tmpDir+"/nonexistent")
	if err != nil {
		t.Fatalf("ExtractAllModifiedFilesFromTranscript() error: %v", err)
	}

	if len(files) != 1 {
		t.Errorf("expected 1 file, got %d: %v", len(files), files)
	}
	if len(files) > 0 && files[0] != "/repo/solo.go" {
		t.Errorf("expected /repo/solo.go, got %q", files[0])
	}
}

func TestExtractAllModifiedFilesFromTranscript_SubagentOnlyChanges(t *testing.T) {
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

	files, err := ExtractAllModifiedFilesFromTranscript(transcriptPath, 0, subagentsDir)
	if err != nil {
		t.Fatalf("ExtractAllModifiedFilesFromTranscript() error: %v", err)
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

// mustMarshal is a test helper that marshals a value to JSON or fails the test.
func mustMarshal(t *testing.T, v interface{}) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}
	return data
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

// makeFileToolLine returns a Droid-format JSONL line with a file-modifying tool_use.
func makeFileToolLine(t *testing.T, toolName, id, filePath string) string {
	t.Helper()
	innerMsg := mustMarshal(t, map[string]interface{}{
		"role": "assistant",
		"content": []map[string]interface{}{
			{
				"type":  "tool_use",
				"id":    "toolu_" + id,
				"name":  toolName,
				"input": map[string]string{"file_path": filePath},
			},
		},
	})
	line := mustMarshal(t, map[string]interface{}{
		"type":    "message",
		"id":      id,
		"message": json.RawMessage(innerMsg),
	})
	return string(line)
}

// makeWriteToolLine returns a Droid-format JSONL line with a Write tool_use for the given file.
func makeWriteToolLine(t *testing.T, id, filePath string) string {
	t.Helper()
	return makeFileToolLine(t, "Write", id, filePath)
}

// makeEditToolLine returns a Droid-format JSONL line with an Edit tool_use for the given file.
func makeEditToolLine(t *testing.T, id, filePath string) string {
	t.Helper()
	return makeFileToolLine(t, "Edit", id, filePath)
}

// makeTaskToolUseLine returns a Droid-format JSONL line with a Task tool_use (spawning a subagent).
func makeTaskToolUseLine(t *testing.T, id, toolUseID string) string {
	t.Helper()
	innerMsg := mustMarshal(t, map[string]interface{}{
		"role": "assistant",
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
		"type":    "message",
		"id":      id,
		"message": json.RawMessage(innerMsg),
	})
	return string(line)
}

// makeTaskResultLine returns a Droid-format JSONL user line with a tool_result containing agentId.
func makeTaskResultLine(t *testing.T, id, toolUseID, agentID string) string {
	t.Helper()
	innerMsg := mustMarshal(t, map[string]interface{}{
		"role": "user",
		"content": []map[string]interface{}{
			{
				"type":        "tool_result",
				"tool_use_id": toolUseID,
				"content":     "agentId: " + agentID,
			},
		},
	})
	line := mustMarshal(t, map[string]interface{}{
		"type":    "message",
		"id":      id,
		"message": json.RawMessage(innerMsg),
	})
	return string(line)
}

// makeUserTextLine returns a Droid-format JSONL line with a user text message (array content).
func makeUserTextLine(t *testing.T, id, text string) string {
	t.Helper()
	innerMsg := mustMarshal(t, map[string]interface{}{
		"role": "user",
		"content": []map[string]interface{}{
			{"type": "text", "text": text},
		},
	})
	line := mustMarshal(t, map[string]interface{}{
		"type":    "message",
		"id":      id,
		"message": json.RawMessage(innerMsg),
	})
	return string(line)
}

// makeAssistantTextLine returns a Droid-format JSONL line with an assistant text message.
func makeAssistantTextLine(t *testing.T, id, text string) string {
	t.Helper()
	innerMsg := mustMarshal(t, map[string]interface{}{
		"role": "assistant",
		"content": []map[string]interface{}{
			{"type": "text", "text": text},
		},
	})
	line := mustMarshal(t, map[string]interface{}{
		"type":    "message",
		"id":      id,
		"message": json.RawMessage(innerMsg),
	})
	return string(line)
}

// makeAssistantTokenLine returns a Droid-format JSONL line with an assistant message that has usage data.
func makeAssistantTokenLine(t *testing.T, id, msgID string, inputTokens, outputTokens int) string {
	t.Helper()
	innerMsg := mustMarshal(t, map[string]interface{}{
		"role": "assistant",
		"id":   msgID,
		"usage": map[string]int{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
		},
	})
	line := mustMarshal(t, map[string]interface{}{
		"type":    "message",
		"id":      id,
		"message": json.RawMessage(innerMsg),
	})
	return string(line)
}

func TestExtractPrompts(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	transcriptPath := tmpDir + "/transcript.jsonl"

	writeJSONLFile(t, transcriptPath,
		makeUserTextLine(t, "u1", "Fix the login bug"),
		makeAssistantTextLine(t, "a1", "I'll fix the login bug."),
		makeUserTextLine(t, "u2", "Now add tests"),
	)

	ag := &FactoryAIDroidAgent{}
	prompts, err := ag.ExtractPrompts(transcriptPath, 0)
	if err != nil {
		t.Fatalf("ExtractPrompts() error = %v", err)
	}

	if len(prompts) != 2 {
		t.Fatalf("ExtractPrompts() got %d prompts, want 2", len(prompts))
	}
	if prompts[0] != "Fix the login bug" {
		t.Errorf("prompts[0] = %q, want %q", prompts[0], "Fix the login bug")
	}
	if prompts[1] != "Now add tests" {
		t.Errorf("prompts[1] = %q, want %q", prompts[1], "Now add tests")
	}
}

func TestExtractPrompts_StripsIDETags(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	transcriptPath := tmpDir + "/transcript.jsonl"

	// User message with IDE context tags injected by VSCode extension
	promptWithTags := `<ide_opened_file>/repo/main.go</ide_opened_file>Fix the bug`
	writeJSONLFile(t, transcriptPath,
		makeUserTextLine(t, "u1", promptWithTags),
	)

	ag := &FactoryAIDroidAgent{}
	prompts, err := ag.ExtractPrompts(transcriptPath, 0)
	if err != nil {
		t.Fatalf("ExtractPrompts() error = %v", err)
	}

	if len(prompts) != 1 {
		t.Fatalf("ExtractPrompts() got %d prompts, want 1", len(prompts))
	}
	if prompts[0] != "Fix the bug" {
		t.Errorf("prompts[0] = %q, want %q (IDE tags should be stripped)", prompts[0], "Fix the bug")
	}
}

func TestExtractPrompts_WithOffset(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	transcriptPath := tmpDir + "/transcript.jsonl"

	writeJSONLFile(t, transcriptPath,
		makeUserTextLine(t, "u1", "First prompt"),
		makeAssistantTextLine(t, "a1", "Done."),
		makeUserTextLine(t, "u2", "Second prompt"),
		makeAssistantTextLine(t, "a2", "Done again."),
	)

	ag := &FactoryAIDroidAgent{}
	// Skip first 2 lines (first user+assistant turn)
	prompts, err := ag.ExtractPrompts(transcriptPath, 2)
	if err != nil {
		t.Fatalf("ExtractPrompts() error = %v", err)
	}

	if len(prompts) != 1 {
		t.Fatalf("ExtractPrompts() got %d prompts, want 1", len(prompts))
	}
	if prompts[0] != "Second prompt" {
		t.Errorf("prompts[0] = %q, want %q", prompts[0], "Second prompt")
	}
}

func TestExtractSummary(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	transcriptPath := tmpDir + "/transcript.jsonl"

	writeJSONLFile(t, transcriptPath,
		makeUserTextLine(t, "u1", "Fix the bug"),
		makeAssistantTextLine(t, "a1", "Working on it..."),
		makeUserTextLine(t, "u2", "Thanks"),
		makeAssistantTextLine(t, "a2", "All done! The login bug is fixed."),
	)

	ag := &FactoryAIDroidAgent{}
	summary, err := ag.ExtractSummary(transcriptPath)
	if err != nil {
		t.Fatalf("ExtractSummary() error = %v", err)
	}

	if summary != "All done! The login bug is fixed." {
		t.Errorf("ExtractSummary() = %q, want %q", summary, "All done! The login bug is fixed.")
	}
}

func TestExtractSummary_SkipsToolUseBlocks(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	transcriptPath := tmpDir + "/transcript.jsonl"

	// Last assistant message has tool_use (no text), second-to-last has text
	writeJSONLFile(t, transcriptPath,
		makeUserTextLine(t, "u1", "Edit main.go"),
		makeAssistantTextLine(t, "a1", "I updated the file."),
		makeWriteToolLine(t, "a2", "/repo/main.go"),
	)

	ag := &FactoryAIDroidAgent{}
	summary, err := ag.ExtractSummary(transcriptPath)
	if err != nil {
		t.Fatalf("ExtractSummary() error = %v", err)
	}

	// Should find "I updated the file." since the tool_use message has no text block
	if summary != "I updated the file." {
		t.Errorf("ExtractSummary() = %q, want %q", summary, "I updated the file.")
	}
}

func TestExtractSummary_EmptyTranscript(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	transcriptPath := tmpDir + "/transcript.jsonl"
	if err := os.WriteFile(transcriptPath, []byte(""), 0o600); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	ag := &FactoryAIDroidAgent{}
	summary, err := ag.ExtractSummary(transcriptPath)
	if err != nil {
		t.Fatalf("ExtractSummary() error = %v", err)
	}

	if summary != "" {
		t.Errorf("ExtractSummary() = %q, want empty string", summary)
	}
}

func TestParseDroidTranscript_MalformedLines(t *testing.T) {
	t.Parallel()

	// Transcript with some broken JSON lines interspersed with valid ones
	data := []byte(
		`{"type":"message","id":"m1","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}` + "\n" +
			`{"broken json` + "\n" +
			`not even close to json` + "\n" +
			`{"type":"message","id":"m2","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}` + "\n" +
			`{"type":"session_event","data":"ignored"}` + "\n",
	)

	lines, _, err := ParseDroidTranscriptFromBytes(data, 0)
	if err != nil {
		t.Fatalf("ParseDroidTranscriptFromBytes() error = %v", err)
	}

	// Only the 2 valid "message" type lines should be parsed
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2 (malformed lines should be silently skipped)", len(lines))
	}
	if lines[0].Type != transcript.TypeUser {
		t.Errorf("lines[0].Type = %q, want %q", lines[0].Type, transcript.TypeUser)
	}
	if lines[1].Type != transcript.TypeAssistant {
		t.Errorf("lines[1].Type = %q, want %q", lines[1].Type, transcript.TypeAssistant)
	}
}

func TestCalculateTotalTokenUsageFromTranscript_WithSubagentFiles(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	transcriptPath := tmpDir + "/transcript.jsonl"
	subagentsDir := tmpDir + "/tasks/toolu_task1"

	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatalf("failed to create subagents dir: %v", err)
	}

	// Main transcript: assistant message with tokens + Task spawning subagent "sub1"
	writeJSONLFile(t, transcriptPath,
		makeAssistantTokenLine(t, "a1", "msg_main1", 100, 50),
		makeTaskToolUseLine(t, "a2", "toolu_task2"),
		makeTaskResultLine(t, "u2", "toolu_task2", "sub99"),
	)

	// Subagent transcript: assistant message with its own tokens
	writeJSONLFile(t, subagentsDir+"/agent-sub99.jsonl",
		makeAssistantTokenLine(t, "sa1", "msg_sub1", 200, 80),
		makeAssistantTokenLine(t, "sa2", "msg_sub2", 150, 60),
	)

	usage, err := CalculateTotalTokenUsageFromTranscript(transcriptPath, 0, subagentsDir)
	if err != nil {
		t.Fatalf("CalculateTotalTokenUsageFromTranscript() error: %v", err)
	}

	// Main agent: 100 input, 50 output, 1 API call
	if usage.InputTokens != 100 {
		t.Errorf("main InputTokens = %d, want 100", usage.InputTokens)
	}
	if usage.OutputTokens != 50 {
		t.Errorf("main OutputTokens = %d, want 50", usage.OutputTokens)
	}
	if usage.APICallCount != 1 {
		t.Errorf("main APICallCount = %d, want 1", usage.APICallCount)
	}

	// Subagent tokens should be aggregated
	if usage.SubagentTokens == nil {
		t.Fatal("SubagentTokens is nil, expected subagent token data")
	}
	if usage.SubagentTokens.InputTokens != 350 {
		t.Errorf("subagent InputTokens = %d, want 350 (200+150)", usage.SubagentTokens.InputTokens)
	}
	if usage.SubagentTokens.OutputTokens != 140 {
		t.Errorf("subagent OutputTokens = %d, want 140 (80+60)", usage.SubagentTokens.OutputTokens)
	}
	if usage.SubagentTokens.APICallCount != 2 {
		t.Errorf("subagent APICallCount = %d, want 2", usage.SubagentTokens.APICallCount)
	}
}

func TestCleanModelName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "custom prefix stripped",
			raw:  "custom:Gemini-2.5-Pro-0",
			want: "Gemini-2.5-Pro-0",
		},
		{
			name: "no prefix unchanged",
			raw:  "claude-opus-4-6",
			want: "claude-opus-4-6",
		},
		{
			name: "empty string",
			raw:  "",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := cleanModelName(tt.raw)
			if got != tt.want {
				t.Errorf("cleanModelName(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestExtractModelFromTranscript_SettingsFile(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	transcriptPath := tmpDir + "/session.jsonl"
	settingsPath := tmpDir + "/session.settings.json"

	// Write a transcript file (content doesn't matter for model extraction)
	if err := os.WriteFile(transcriptPath, []byte(`{"type":"session_start"}`+"\n"), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Write the settings file with the model
	settingsData := `{"model":"custom:Gemini-2.5-Pro-0","reasoningEffort":"none"}`
	if err := os.WriteFile(settingsPath, []byte(settingsData), 0o644); err != nil {
		t.Fatalf("failed to write settings: %v", err)
	}

	model := ExtractModelFromTranscript(transcriptPath)
	if model != "Gemini-2.5-Pro-0" {
		t.Errorf("ExtractModelFromTranscript() = %q, want %q", model, "Gemini-2.5-Pro-0")
	}
}

func TestExtractModelFromTranscript_NoCustomPrefix(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	transcriptPath := tmpDir + "/session.jsonl"
	settingsPath := tmpDir + "/session.settings.json"

	if err := os.WriteFile(transcriptPath, []byte(`{"type":"session_start"}`+"\n"), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	settingsData := `{"model":"claude-opus-4-6"}`
	if err := os.WriteFile(settingsPath, []byte(settingsData), 0o644); err != nil {
		t.Fatalf("failed to write settings: %v", err)
	}

	model := ExtractModelFromTranscript(transcriptPath)
	if model != "claude-opus-4-6" {
		t.Errorf("ExtractModelFromTranscript() = %q, want %q", model, "claude-opus-4-6")
	}
}

func TestExtractModelFromTranscript_NoSettingsFile(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	transcriptPath := tmpDir + "/session.jsonl"

	// Write transcript but no settings file
	if err := os.WriteFile(transcriptPath, []byte(`{"type":"session_start"}`+"\n"), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	model := ExtractModelFromTranscript(transcriptPath)
	if model != "" {
		t.Errorf("ExtractModelFromTranscript() = %q, want empty", model)
	}
}

func TestExtractModelFromTranscript_CorruptSettingsFile(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	transcriptPath := tmpDir + "/session.jsonl"
	settingsPath := tmpDir + "/session.settings.json"

	if err := os.WriteFile(transcriptPath, []byte(`{"type":"session_start"}`+"\n"), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Write invalid JSON to the settings file
	if err := os.WriteFile(settingsPath, []byte(`{not valid json`), 0o644); err != nil {
		t.Fatalf("failed to write settings: %v", err)
	}

	model := ExtractModelFromTranscript(transcriptPath)
	if model != "" {
		t.Errorf("ExtractModelFromTranscript() = %q, want empty for corrupt settings", model)
	}
}

func TestExtractModelFromTranscript_EmptyPath(t *testing.T) {
	t.Parallel()

	model := ExtractModelFromTranscript("")
	if model != "" {
		t.Errorf("ExtractModelFromTranscript(\"\") = %q, want empty", model)
	}
}
