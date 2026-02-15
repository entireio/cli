package codexcli

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestName(t *testing.T) {
	t.Parallel()
	ag := &CodexCLIAgent{}
	if ag.Name() != agent.AgentNameCodex {
		t.Errorf("Name() = %q, want %q", ag.Name(), agent.AgentNameCodex)
	}
}

func TestType(t *testing.T) {
	t.Parallel()
	ag := &CodexCLIAgent{}
	if ag.Type() != agent.AgentTypeCodex {
		t.Errorf("Type() = %q, want %q", ag.Type(), agent.AgentTypeCodex)
	}
}

func TestDescription(t *testing.T) {
	t.Parallel()
	ag := &CodexCLIAgent{}
	if ag.Description() == "" {
		t.Error("Description() should not be empty")
	}
}

func TestProtectedDirs(t *testing.T) {
	t.Parallel()
	ag := &CodexCLIAgent{}
	dirs := ag.ProtectedDirs()
	if dirs != nil {
		t.Errorf("ProtectedDirs() = %v, want nil", dirs)
	}
}

func TestSupportsHooks(t *testing.T) {
	t.Parallel()
	ag := &CodexCLIAgent{}
	if !ag.SupportsHooks() {
		t.Error("SupportsHooks() should return true")
	}
}

func TestFormatResumeCommand(t *testing.T) {
	t.Parallel()
	ag := &CodexCLIAgent{}
	result := ag.FormatResumeCommand("sess-123")
	expected := "codex --resume sess-123"
	if result != expected {
		t.Errorf("FormatResumeCommand() = %q, want %q", result, expected)
	}
}

func TestResolveSessionFile_WithKnownSession(t *testing.T) {
	t.Parallel()
	// Create a temp directory with a matching file
	tmpDir := t.TempDir()
	ag := &CodexCLIAgent{}

	// When no transcript file exists, it should return a best-guess path
	result := ag.ResolveSessionFile(tmpDir, "abc-123-def")
	if result == "" {
		t.Error("ResolveSessionFile() should return a non-empty path")
	}
	if !strings.Contains(result, "abc-123-def") {
		t.Errorf("ResolveSessionFile() = %q, should contain session ID", result)
	}
}

func TestParseHookInput_TurnComplete(t *testing.T) {
	t.Parallel()

	ag := &CodexCLIAgent{}
	input := `{"type":"agent-turn-complete","turn-id":"turn-1","thread-id":"thread-abc","input-messages":["fix the bug"],"last-assistant-message":"done"}`

	result, err := ag.ParseHookInput(agent.HookStop, strings.NewReader(input))
	if err != nil {
		t.Fatalf("ParseHookInput() error = %v", err)
	}

	if result.SessionID != "thread-abc" {
		t.Errorf("SessionID = %q, want %q", result.SessionID, "thread-abc")
	}
	if result.UserPrompt != "fix the bug" {
		t.Errorf("UserPrompt = %q, want %q", result.UserPrompt, "fix the bug")
	}
	if result.RawData["turn_id"] != "turn-1" {
		t.Errorf("RawData[turn_id] = %q, want %q", result.RawData["turn_id"], "turn-1")
	}
	if result.RawData["last_message"] != "done" {
		t.Errorf("RawData[last_message] = %q, want %q", result.RawData["last_message"], "done")
	}
}

func TestParseHookInput_EmptyInput(t *testing.T) {
	t.Parallel()

	ag := &CodexCLIAgent{}
	_, err := ag.ParseHookInput(agent.HookStop, strings.NewReader(""))
	if err == nil {
		t.Error("ParseHookInput() should error on empty input")
	}
}

func TestParseHookInput_InvalidJSON(t *testing.T) {
	t.Parallel()

	ag := &CodexCLIAgent{}
	_, err := ag.ParseHookInput(agent.HookStop, strings.NewReader("not json"))
	if err == nil {
		t.Error("ParseHookInput() should error on invalid JSON")
	}
}

func TestParseHookInput_NoInputMessages(t *testing.T) {
	t.Parallel()

	ag := &CodexCLIAgent{}
	input := `{"type":"agent-turn-complete","turn-id":"turn-1","thread-id":"thread-abc"}`

	result, err := ag.ParseHookInput(agent.HookStop, strings.NewReader(input))
	if err != nil {
		t.Fatalf("ParseHookInput() error = %v", err)
	}

	if result.UserPrompt != "" {
		t.Errorf("UserPrompt = %q, want empty", result.UserPrompt)
	}
}

func TestGetSessionID(t *testing.T) {
	t.Parallel()

	ag := &CodexCLIAgent{}
	input := &agent.HookInput{SessionID: "sess-456"}
	if ag.GetSessionID(input) != "sess-456" {
		t.Errorf("GetSessionID() = %q, want %q", ag.GetSessionID(input), "sess-456")
	}
}
