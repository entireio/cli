package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestHookNames(t *testing.T) {
	t.Parallel()
	a := &AntigravityAgent{}
	names := a.HookNames()
	want := []string{
		HookNamePreToolUse,
		HookNamePostToolUse,
		HookNamePreInvocation,
		HookNamePostInvocation,
		HookNameStop,
	}
	if len(names) != len(want) {
		t.Fatalf("HookNames() returned %d names, want %d: %v", len(names), len(want), names)
	}
	for i, n := range want {
		if names[i] != n {
			t.Errorf("HookNames()[%d] = %q, want %q", i, names[i], n)
		}
	}
}

func TestParseHookEvent_PreInvocationEmitsTurnStart(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/hook_stdin_pre_invocation.json")
	if err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{}
	ev, err := a.ParseHookEvent(context.Background(), HookNamePreInvocation, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if ev == nil {
		t.Fatal("expected non-nil event for pre-invocation")
	}
	if ev.Type != agent.TurnStart {
		t.Errorf("Type = %v, want TurnStart", ev.Type)
	}
	if ev.SessionID != testConversationID {
		t.Errorf("SessionID = %q, want %q", ev.SessionID, testConversationID)
	}
	if ev.SessionRef != testTranscriptPath {
		t.Errorf("SessionRef = %q, want %q", ev.SessionRef, testTranscriptPath)
	}
}

func TestParseHookEvent_PostInvocationEmitsTurnEnd(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/hook_stdin_post_invocation.json")
	if err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{}
	ev, err := a.ParseHookEvent(context.Background(), HookNamePostInvocation, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if ev == nil {
		t.Fatal("expected non-nil event for post-invocation")
	}
	if ev.Type != agent.TurnEnd {
		t.Errorf("Type = %v, want TurnEnd", ev.Type)
	}
	if ev.SessionID != testConversationID {
		t.Errorf("SessionID = %q, want %q", ev.SessionID, testConversationID)
	}
	if ev.SessionRef != testTranscriptPath {
		t.Errorf("SessionRef = %q, want %q", ev.SessionRef, testTranscriptPath)
	}
}

func TestParseHookEvent_Stop_FullyIdleTrueEmitsSessionEnd(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/hook_stdin_stop.json")
	if err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{}
	ev, err := a.ParseHookEvent(context.Background(), HookNameStop, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if ev == nil {
		t.Fatal("expected non-nil event for stop with fullyIdle=true")
	}
	if ev.Type != agent.SessionEnd {
		t.Errorf("Type = %v, want SessionEnd", ev.Type)
	}
	if ev.SessionID != testConversationID {
		t.Errorf("SessionID = %q, want %q", ev.SessionID, testConversationID)
	}
}

func TestParseHookEvent_Stop_FullyIdleFalseReturnsNil(t *testing.T) {
	t.Parallel()
	// Synthesize a stop payload with fullyIdle=false
	payload := StopPayload{
		CommonPayload: CommonPayload{
			ConversationID: testConversationID,
			TranscriptPath: testTranscriptPath,
			WorkspacePaths: []string{testWorkspacePath},
		},
		ExecutionNum:      1,
		TerminationReason: "background_tasks",
		FullyIdle:         false,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{}
	ev, err := a.ParseHookEvent(context.Background(), HookNameStop, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if ev != nil {
		t.Errorf("expected nil event for stop with fullyIdle=false, got %+v", ev)
	}
}

func TestParseHookEvent_PreToolUse_WriteToFileExtractsModifiedFiles(t *testing.T) {
	t.Parallel()
	// Synthesize a PreToolUse payload with write_to_file (Overwrite=true → ModifiedFiles)
	type writeArgs struct {
		TargetFile string `json:"TargetFile"`
		Overwrite  bool   `json:"Overwrite"`
	}
	argsJSON, err := json.Marshal(writeArgs{TargetFile: "src/main.go", Overwrite: true})
	if err != nil {
		t.Fatal(err)
	}
	payload := PreToolUsePayload{
		CommonPayload: CommonPayload{
			ConversationID: testConversationID,
			TranscriptPath: testTranscriptPath,
			WorkspacePaths: []string{testWorkspacePath},
		},
		ToolCall: ToolCall{
			Name: "write_to_file",
			Args: json.RawMessage(argsJSON),
		},
		StepIdx: 1,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{}
	ev, err := a.ParseHookEvent(context.Background(), HookNamePreToolUse, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if ev == nil {
		t.Fatal("expected non-nil event for write_to_file tool")
	}
	if ev.Type != agent.ToolUse {
		t.Errorf("Type = %v, want ToolUse", ev.Type)
	}
	if len(ev.ModifiedFiles) != 1 || ev.ModifiedFiles[0] != "src/main.go" {
		t.Errorf("ModifiedFiles = %v, want [src/main.go]", ev.ModifiedFiles)
	}
	if len(ev.NewFiles) != 0 {
		t.Errorf("NewFiles = %v, want empty (Overwrite=true → ModifiedFiles)", ev.NewFiles)
	}
}

func TestParseHookEvent_PreToolUse_WriteToFileNewFile(t *testing.T) {
	t.Parallel()
	// Overwrite=false → NewFiles
	type writeArgs struct {
		TargetFile string `json:"TargetFile"`
		Overwrite  bool   `json:"Overwrite"`
	}
	argsJSON, err := json.Marshal(writeArgs{TargetFile: "src/new.go", Overwrite: false})
	if err != nil {
		t.Fatal(err)
	}
	payload := PreToolUsePayload{
		CommonPayload: CommonPayload{
			ConversationID: testConversationID,
			TranscriptPath: testTranscriptPath,
			WorkspacePaths: []string{testWorkspacePath},
		},
		ToolCall: ToolCall{
			Name: "write_to_file",
			Args: json.RawMessage(argsJSON),
		},
		StepIdx: 2,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{}
	ev, err := a.ParseHookEvent(context.Background(), HookNamePreToolUse, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if ev == nil {
		t.Fatal("expected non-nil event for write_to_file (new file)")
	}
	if ev.Type != agent.ToolUse {
		t.Errorf("Type = %v, want ToolUse", ev.Type)
	}
	if len(ev.NewFiles) != 1 || ev.NewFiles[0] != "src/new.go" {
		t.Errorf("NewFiles = %v, want [src/new.go]", ev.NewFiles)
	}
	if len(ev.ModifiedFiles) != 0 {
		t.Errorf("ModifiedFiles = %v, want empty (Overwrite=false → NewFiles)", ev.ModifiedFiles)
	}
}

func TestParseHookEvent_PreToolUse_ReplaceFileContent(t *testing.T) {
	t.Parallel()
	type replaceArgs struct {
		TargetFile string `json:"TargetFile"`
	}
	argsJSON, err := json.Marshal(replaceArgs{TargetFile: "src/foo.go"})
	if err != nil {
		t.Fatal(err)
	}
	payload := PreToolUsePayload{
		CommonPayload: CommonPayload{
			ConversationID: testConversationID,
			TranscriptPath: testTranscriptPath,
		},
		ToolCall: ToolCall{Name: "replace_file_content", Args: json.RawMessage(argsJSON)},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{}
	ev, err := a.ParseHookEvent(context.Background(), HookNamePreToolUse, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if ev == nil {
		t.Fatal("expected non-nil event for replace_file_content")
	}
	if ev.Type != agent.ToolUse {
		t.Errorf("Type = %v, want ToolUse", ev.Type)
	}
	if len(ev.ModifiedFiles) != 1 || ev.ModifiedFiles[0] != "src/foo.go" {
		t.Errorf("ModifiedFiles = %v, want [src/foo.go]", ev.ModifiedFiles)
	}
}

func TestParseHookEvent_PreToolUse_NonMutatingToolReturnsNil(t *testing.T) {
	t.Parallel()
	// Use the testdata fixture which uses run_command (non-mutating)
	data, err := os.ReadFile("testdata/hook_stdin_pre_tool_use.json")
	if err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{}
	ev, err := a.ParseHookEvent(context.Background(), HookNamePreToolUse, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if ev != nil {
		t.Errorf("expected nil event for non-mutating tool run_command, got %+v", ev)
	}
}

func TestParseHookEvent_PostToolUseReturnsNil(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/hook_stdin_post_tool_use.json")
	if err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{}
	ev, err := a.ParseHookEvent(context.Background(), HookNamePostToolUse, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if ev != nil {
		t.Errorf("expected nil event for post-tool-use, got %+v", ev)
	}
}
