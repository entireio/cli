package openclaw

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestOpenClawAgentName(t *testing.T) {
	ag := NewOpenClawAgent()
	if ag.Name() != agent.AgentNameOpenClaw {
		t.Errorf("expected Name() = %q, got %q", agent.AgentNameOpenClaw, ag.Name())
	}
}

func TestOpenClawAgentType(t *testing.T) {
	ag := NewOpenClawAgent()
	if ag.Type() != agent.AgentTypeOpenClaw {
		t.Errorf("expected Type() = %q, got %q", agent.AgentTypeOpenClaw, ag.Type())
	}
}

func TestOpenClawSupportsHooks(t *testing.T) {
	ag := NewOpenClawAgent()
	if !ag.SupportsHooks() {
		t.Error("expected SupportsHooks() = true")
	}
}

func TestOpenClawProtectedDirs(t *testing.T) {
	ag := NewOpenClawAgent()
	dirs := ag.ProtectedDirs()
	if len(dirs) != 1 || dirs[0] != ".openclaw" {
		t.Errorf("expected ProtectedDirs() = [\".openclaw\"], got %v", dirs)
	}
}

func TestOpenClawFormatResumeCommand(t *testing.T) {
	ag := NewOpenClawAgent()
	cmd := ag.FormatResumeCommand("test-session-123")
	expected := "openclaw session resume test-session-123"
	if cmd != expected {
		t.Errorf("expected FormatResumeCommand() = %q, got %q", expected, cmd)
	}
}

func TestOpenClawResolveSessionFile(t *testing.T) {
	ag := NewOpenClawAgent()
	path := ag.ResolveSessionFile("/home/user/.openclaw/sessions", "abc123")
	if !strings.HasSuffix(path, "abc123.jsonl") {
		t.Errorf("expected path ending with abc123.jsonl, got %q", path)
	}
}

func TestOpenClawParseHookInput(t *testing.T) {
	ag := NewOpenClawAgent()
	input := `{"session_id": "sess-123", "transcript_path": "/tmp/sess.jsonl", "prompt": "hello"}`
	reader := strings.NewReader(input)

	hookInput, err := ag.ParseHookInput(agent.HookUserPromptSubmit, reader)
	if err != nil {
		t.Fatalf("ParseHookInput failed: %v", err)
	}

	if hookInput.SessionID != "sess-123" {
		t.Errorf("expected SessionID = %q, got %q", "sess-123", hookInput.SessionID)
	}
	if hookInput.SessionRef != "/tmp/sess.jsonl" {
		t.Errorf("expected SessionRef = %q, got %q", "/tmp/sess.jsonl", hookInput.SessionRef)
	}
	if hookInput.UserPrompt != "hello" {
		t.Errorf("expected UserPrompt = %q, got %q", "hello", hookInput.UserPrompt)
	}
}

func TestOpenClawParseHookInputEmpty(t *testing.T) {
	ag := NewOpenClawAgent()
	reader := strings.NewReader("")

	_, err := ag.ParseHookInput(agent.HookStop, reader)
	if err == nil {
		t.Error("expected error for empty input")
	}
}

func TestExtractModifiedFiles(t *testing.T) {
	jsonl := `{"role":"user","content":"fix the bug"}
{"role":"assistant","content":"I'll fix it","tool_calls":[{"name":"Edit","input":{"file_path":"main.go"}},{"name":"Write","input":{"path":"new_file.go"}}]}
{"role":"assistant","content":"done","tool_calls":[{"name":"exec","input":{"command":"go build"}}]}
`
	lines, err := ParseTranscript([]byte(jsonl))
	if err != nil {
		t.Fatalf("ParseTranscript failed: %v", err)
	}

	files := ExtractModifiedFiles(lines)
	if len(files) != 2 {
		t.Fatalf("expected 2 modified files, got %d: %v", len(files), files)
	}
	if files[0] != "main.go" {
		t.Errorf("expected files[0] = %q, got %q", "main.go", files[0])
	}
	if files[1] != "new_file.go" {
		t.Errorf("expected files[1] = %q, got %q", "new_file.go", files[1])
	}
}

func TestExtractLastUserPrompt(t *testing.T) {
	jsonl := `{"role":"user","content":"first prompt"}
{"role":"assistant","content":"response"}
{"role":"user","content":"second prompt"}
`
	lines, err := ParseTranscript([]byte(jsonl))
	if err != nil {
		t.Fatalf("ParseTranscript failed: %v", err)
	}

	prompt := ExtractLastUserPrompt(lines)
	if prompt != "second prompt" {
		t.Errorf("expected %q, got %q", "second prompt", prompt)
	}
}

func TestGetHookNames(t *testing.T) {
	ag := &OpenClawAgent{}
	names := ag.GetHookNames()
	expected := []string{"session-start", "session-end", "stop", "user-prompt-submit"}
	if len(names) != len(expected) {
		t.Fatalf("expected %d hook names, got %d", len(expected), len(names))
	}
	for i, name := range names {
		if name != expected[i] {
			t.Errorf("expected hook name[%d] = %q, got %q", i, expected[i], name)
		}
	}
}
