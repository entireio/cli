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
	if p.StepIdx != 19 {
		t.Errorf("StepIdx = %d", p.StepIdx)
	}
	if p.TranscriptPath != testTranscriptPath {
		t.Errorf("TranscriptPath = %q", p.TranscriptPath)
	}
	if len(p.WorkspacePaths) != 1 || p.WorkspacePaths[0] != testWorkspacePath {
		t.Errorf("WorkspacePaths = %v", p.WorkspacePaths)
	}
}

func TestParsePostToolUsePayload(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/hook_stdin_post_tool_use.json")
	if err != nil {
		t.Fatal(err)
	}
	var p PostToolUsePayload
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("unmarshal PostToolUsePayload: %v", err)
	}
	if p.ConversationID != testConversationID {
		t.Errorf("ConversationID = %q", p.ConversationID)
	}
	if p.StepIdx != 5 {
		t.Errorf("StepIdx = %d", p.StepIdx)
	}
	if p.Error != "exit status 1" {
		t.Errorf("Error = %q", p.Error)
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
	if p.InvocationNum != 3 {
		t.Errorf("InvocationNum = %d", p.InvocationNum)
	}
	if p.InitialNumSteps != 10 {
		t.Errorf("InitialNumSteps = %d", p.InitialNumSteps)
	}
	if p.TranscriptPath != testTranscriptPath {
		t.Errorf("TranscriptPath = %q", p.TranscriptPath)
	}
}

func TestParsePostInvocationPayload(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/hook_stdin_post_invocation.json")
	if err != nil {
		t.Fatal(err)
	}
	var p PostInvocationPayload
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("unmarshal PostInvocationPayload: %v", err)
	}
	if p.ConversationID != testConversationID {
		t.Errorf("ConversationID = %q", p.ConversationID)
	}
	if p.InvocationNum != 4 {
		t.Errorf("InvocationNum = %d", p.InvocationNum)
	}
	if p.InitialNumSteps != 12 {
		t.Errorf("InitialNumSteps = %d", p.InitialNumSteps)
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
	if p.ExecutionNum != 1 {
		t.Errorf("ExecutionNum = %d", p.ExecutionNum)
	}
	if p.TerminationReason != "model_stop" {
		t.Errorf("TerminationReason = %q", p.TerminationReason)
	}
	if !p.FullyIdle {
		t.Error("FullyIdle should be true")
	}
	if p.Error != "" {
		t.Errorf("Error = %q, want empty", p.Error)
	}
	if p.TranscriptPath != testTranscriptPath {
		t.Errorf("TranscriptPath = %q", p.TranscriptPath)
	}
}
