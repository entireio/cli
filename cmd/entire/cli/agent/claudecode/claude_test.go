package claudecode

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestIsInstalled(t *testing.T) {
	t.Parallel()

	c := &ClaudeCodeAgent{}
	installed, err := c.IsInstalled()

	// Should not return an error regardless of whether claude is installed
	if err != nil {
		t.Fatalf("IsInstalled() error = %v", err)
	}

	// installed is environment-dependent, just verify it's a valid bool
	_ = installed
}

func TestParseHookInput_UserPromptSubmit(t *testing.T) {
	t.Parallel()

	c := &ClaudeCodeAgent{}
	input := `{"session_id":"sess-123","transcript_path":"/tmp/transcript.jsonl","prompt":"Fix the login bug"}`

	result, err := c.ParseHookInput(agent.HookUserPromptSubmit, strings.NewReader(input))
	if err != nil {
		t.Fatalf("ParseHookInput() error = %v", err)
	}

	if result.SessionID != "sess-123" {
		t.Errorf("SessionID = %q, want %q", result.SessionID, "sess-123")
	}
	if result.SessionRef != "/tmp/transcript.jsonl" {
		t.Errorf("SessionRef = %q, want %q", result.SessionRef, "/tmp/transcript.jsonl")
	}
	if result.UserPrompt != "Fix the login bug" {
		t.Errorf("UserPrompt = %q, want %q", result.UserPrompt, "Fix the login bug")
	}
}

func TestParseHookInput_SessionStart_NoPrompt(t *testing.T) {
	t.Parallel()

	c := &ClaudeCodeAgent{}
	input := `{"session_id":"sess-456","transcript_path":"/tmp/transcript.jsonl"}`

	result, err := c.ParseHookInput(agent.HookSessionStart, strings.NewReader(input))
	if err != nil {
		t.Fatalf("ParseHookInput() error = %v", err)
	}

	if result.SessionID != "sess-456" {
		t.Errorf("SessionID = %q, want %q", result.SessionID, "sess-456")
	}
	if result.UserPrompt != "" {
		t.Errorf("UserPrompt = %q, want empty", result.UserPrompt)
	}
}
