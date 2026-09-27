package antigravity

import (
	"encoding/json"
	"os"
	"testing"
)

const (
	testConversationID = "ec33ebf9-0cba-4100-8142-c61503f6c587"
	testTranscriptPath = "/workspace/project/.gemini/jetski/transcript.jsonl"
	testWorkspacePath  = "/workspace/project"
)

// The payload structs decode only the fields the integration consumes; the
// fixtures carry agy's full documented payloads, so these tests also pin that
// unknown fields (workspacePaths, stepIdx, initialNumSteps, executionNum,
// terminationReason, error, artifactDirectoryPath) are tolerated.

func TestParsePreToolUsePayload(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/hook_stdin_pre_tool_use.json")
	if err != nil {
		t.Fatal(err)
	}
	var p PreToolUsePayload
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("unmarshal PreToolUsePayload: %v", err)
	}
	if p.ConversationID != testConversationID {
		t.Errorf("ConversationID = %q", p.ConversationID)
	}
	if p.ToolCall.Name != "run_command" {
		t.Errorf("ToolCall.Name = %q", p.ToolCall.Name)
	}
	if p.TranscriptPath != testTranscriptPath {
		t.Errorf("TranscriptPath = %q", p.TranscriptPath)
	}
}

func TestParsePreInvocationPayload(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/hook_stdin_pre_invocation.json")
	if err != nil {
		t.Fatal(err)
	}
	var p InvocationPayload
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("unmarshal InvocationPayload (PreInvocation): %v", err)
	}
	if p.ConversationID != testConversationID {
		t.Errorf("ConversationID = %q", p.ConversationID)
	}
	// Fixture mirrors a real agy 1.0.0 follow-up PreInvocation: invocationNum=1
	// (0-indexed in agy's wire format). See parsePreInvocation comment block.
	if p.InvocationNum != 1 {
		t.Errorf("InvocationNum = %d", p.InvocationNum)
	}
	if p.TranscriptPath != testTranscriptPath {
		t.Errorf("TranscriptPath = %q", p.TranscriptPath)
	}
}

func TestParseStopPayload(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/hook_stdin_stop.json")
	if err != nil {
		t.Fatal(err)
	}
	var p StopPayload
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("unmarshal StopPayload: %v", err)
	}
	if p.ConversationID != testConversationID {
		t.Errorf("ConversationID = %q", p.ConversationID)
	}
	if !p.FullyIdle {
		t.Error("FullyIdle should be true")
	}
	if p.TranscriptPath != testTranscriptPath {
		t.Errorf("TranscriptPath = %q", p.TranscriptPath)
	}
}
