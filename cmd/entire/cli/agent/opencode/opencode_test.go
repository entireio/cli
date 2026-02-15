package opencode

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestResolveSessionFile(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	result := ag.ResolveSessionFile("/home/user/.opencode/sessions", "sess-abc-123")
	expected := "/home/user/.opencode/sessions/sess-abc-123.jsonl"
	if result != expected {
		t.Errorf("ResolveSessionFile() = %q, want %q", result, expected)
	}
}

func TestProtectedDirs(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	dirs := ag.ProtectedDirs()
	if len(dirs) != 1 || dirs[0] != ".opencode" {
		t.Errorf("ProtectedDirs() = %v, want [.opencode]", dirs)
	}
}

func TestName(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	if ag.Name() != agent.AgentNameOpenCode {
		t.Errorf("Name() = %q, want %q", ag.Name(), agent.AgentNameOpenCode)
	}
}

func TestType(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	if ag.Type() != agent.AgentTypeOpenCode {
		t.Errorf("Type() = %q, want %q", ag.Type(), agent.AgentTypeOpenCode)
	}
}

func TestDescription(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	desc := ag.Description()
	if desc == "" {
		t.Error("Description() should not be empty")
	}
}

func TestSupportsHooks(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	if !ag.SupportsHooks() {
		t.Error("SupportsHooks() should return true")
	}
}

func TestGetHookConfigPath(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	path := ag.GetHookConfigPath()
	if path != ".opencode/plugins/entire.ts" {
		t.Errorf("GetHookConfigPath() = %q, want %q", path, ".opencode/plugins/entire.ts")
	}
}

func TestFormatResumeCommand(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	cmd := ag.FormatResumeCommand("sess-123")
	expected := "opencode --resume sess-123"
	if cmd != expected {
		t.Errorf("FormatResumeCommand() = %q, want %q", cmd, expected)
	}
}

func TestGetSessionID(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	input := &agent.HookInput{SessionID: "test-session-id"}
	if ag.GetSessionID(input) != "test-session-id" {
		t.Errorf("GetSessionID() = %q, want %q", ag.GetSessionID(input), "test-session-id")
	}
}

func TestParseHookInput_SessionStart(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	input := `{"session_id":"sess-123","session_ref":"sess-123","timestamp":"2026-01-13T12:00:00Z","transcript_path":"/tmp/sessions/sess-123.jsonl"}`

	result, err := ag.ParseHookInput(agent.HookSessionStart, strings.NewReader(input))
	if err != nil {
		t.Fatalf("ParseHookInput() error = %v", err)
	}

	if result.SessionID != "sess-123" {
		t.Errorf("SessionID = %q, want %q", result.SessionID, "sess-123")
	}
	if result.SessionRef != "/tmp/sessions/sess-123.jsonl" {
		t.Errorf("SessionRef = %q, want %q", result.SessionRef, "/tmp/sessions/sess-123.jsonl")
	}
	if result.HookType != agent.HookSessionStart {
		t.Errorf("HookType = %q, want %q", result.HookType, agent.HookSessionStart)
	}
}

func TestParseHookInput_Stop(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	input := `{"session_id":"sess-456","session_ref":"sess-456","timestamp":"2026-01-13T12:00:00Z","transcript_path":"/tmp/sessions/sess-456.jsonl"}`

	result, err := ag.ParseHookInput(agent.HookStop, strings.NewReader(input))
	if err != nil {
		t.Fatalf("ParseHookInput() error = %v", err)
	}

	if result.SessionID != "sess-456" {
		t.Errorf("SessionID = %q, want %q", result.SessionID, "sess-456")
	}
	if result.HookType != agent.HookStop {
		t.Errorf("HookType = %q, want %q", result.HookType, agent.HookStop)
	}
}

func TestParseHookInput_TaskStart(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	input := `{"session_id":"sess-789","session_ref":"sess-789","timestamp":"2026-01-13T12:00:00Z","transcript_path":"/tmp/sessions/sess-789.jsonl","tool_name":"Task","tool_use_id":"tool-abc"}`

	result, err := ag.ParseHookInput(agent.HookPreToolUse, strings.NewReader(input))
	if err != nil {
		t.Fatalf("ParseHookInput() error = %v", err)
	}

	if result.ToolName != "Task" {
		t.Errorf("ToolName = %q, want %q", result.ToolName, "Task")
	}
	if result.ToolUseID != "tool-abc" {
		t.Errorf("ToolUseID = %q, want %q", result.ToolUseID, "tool-abc")
	}
}

func TestParseHookInput_EmptyInput(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	_, err := ag.ParseHookInput(agent.HookSessionStart, strings.NewReader(""))
	if err == nil {
		t.Error("ParseHookInput() should error on empty input")
	}
}

func TestParseHookInput_InvalidJSON(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	_, err := ag.ParseHookInput(agent.HookSessionStart, strings.NewReader("not json"))
	if err == nil {
		t.Error("ParseHookInput() should error on invalid JSON")
	}
}

func TestParseHookInput_TranscriptPathMapsToSessionRef(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	// When transcript_path is set, it should override session_ref
	input := `{"session_id":"sess-1","session_ref":"original-ref","transcript_path":"/path/to/transcript.jsonl"}`

	result, err := ag.ParseHookInput(agent.HookStop, strings.NewReader(input))
	if err != nil {
		t.Fatalf("ParseHookInput() error = %v", err)
	}

	if result.SessionRef != "/path/to/transcript.jsonl" {
		t.Errorf("SessionRef = %q, want %q (transcript_path should override session_ref)", result.SessionRef, "/path/to/transcript.jsonl")
	}
}

func TestParseHookInput_SubagentTranscriptPath(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	input := `{"session_id":"sess-1","transcript_path":"/tmp/main.jsonl","subagent_transcript_path":"/tmp/subagent.jsonl"}`

	result, err := ag.ParseHookInput(agent.HookPostToolUse, strings.NewReader(input))
	if err != nil {
		t.Fatalf("ParseHookInput() error = %v", err)
	}

	if result.RawData["subagent_transcript_path"] != "/tmp/subagent.jsonl" {
		t.Errorf("subagent_transcript_path = %v, want %q", result.RawData["subagent_transcript_path"], "/tmp/subagent.jsonl")
	}
}

func TestReadSession_EmptySessionRef(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	_, err := ag.ReadSession(&agent.HookInput{})
	if err == nil {
		t.Error("ReadSession() should error when SessionRef is empty")
	}
}

func TestWriteSession_NilSession(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	err := ag.WriteSession(nil)
	if err == nil {
		t.Error("WriteSession(nil) should error")
	}
}

func TestWriteSession_WrongAgent(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	err := ag.WriteSession(&agent.AgentSession{AgentName: "other-agent"})
	if err == nil {
		t.Error("WriteSession() should error for wrong agent name")
	}
}

func TestWriteSession_EmptySessionRef(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	err := ag.WriteSession(&agent.AgentSession{
		AgentName: agent.AgentNameOpenCode,
	})
	if err == nil {
		t.Error("WriteSession() should error when SessionRef is empty")
	}
}

func TestWriteSession_EmptyNativeData(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	err := ag.WriteSession(&agent.AgentSession{
		AgentName:  agent.AgentNameOpenCode,
		SessionRef: "/tmp/test.jsonl",
	})
	if err == nil {
		t.Error("WriteSession() should error when NativeData is empty")
	}
}

func TestGetTranscriptPosition_EmptyPath(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	pos, err := ag.GetTranscriptPosition("")
	if err != nil {
		t.Fatalf("GetTranscriptPosition('') error = %v", err)
	}
	if pos != 0 {
		t.Errorf("GetTranscriptPosition('') = %d, want 0", pos)
	}
}

func TestGetTranscriptPosition_NonexistentFile(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	pos, err := ag.GetTranscriptPosition("/nonexistent/path.jsonl")
	if err != nil {
		t.Fatalf("GetTranscriptPosition(nonexistent) error = %v", err)
	}
	if pos != 0 {
		t.Errorf("GetTranscriptPosition(nonexistent) = %d, want 0", pos)
	}
}

func TestExtractModifiedFilesFromOffset_EmptyPath(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	files, pos, err := ag.ExtractModifiedFilesFromOffset("", 0)
	if err != nil {
		t.Fatalf("ExtractModifiedFilesFromOffset('', 0) error = %v", err)
	}
	if len(files) != 0 || pos != 0 {
		t.Errorf("ExtractModifiedFilesFromOffset('', 0) = %v, %d; want nil, 0", files, pos)
	}
}

// Compile-time interface assertions
var (
	_ agent.Agent              = (*OpenCodeAgent)(nil)
	_ agent.TranscriptAnalyzer = (*OpenCodeAgent)(nil)
	_ agent.TranscriptChunker  = (*OpenCodeAgent)(nil)
)
