package claudecode

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestIsInstalled_Found(t *testing.T) {
	t.Parallel()

	c := &ClaudeCodeAgent{
		LookPath: func(file string) (string, error) {
			if file == "claude" {
				return "/usr/bin/claude", nil
			}
			return "", exec.ErrNotFound
		},
	}
	installed, err := c.IsInstalled()

	if err != nil {
		t.Fatalf("IsInstalled() error = %v", err)
	}
	if !installed {
		t.Error("IsInstalled() = false, want true")
	}
}

func TestIsInstalled_NotFound(t *testing.T) {
	t.Parallel()

	c := &ClaudeCodeAgent{
		LookPath: func(_ string) (string, error) {
			return "", exec.ErrNotFound
		},
	}
	installed, err := c.IsInstalled()

	if err != nil {
		t.Fatalf("IsInstalled() error = %v", err)
	}
	if installed {
		t.Error("IsInstalled() = true, want false")
	}
}

func TestIsInstalled_OSError(t *testing.T) {
	t.Parallel()

	c := &ClaudeCodeAgent{
		LookPath: func(_ string) (string, error) {
			return "", errors.New("permission denied")
		},
	}
	installed, err := c.IsInstalled()

	if err == nil {
		t.Fatal("IsInstalled() should return error for OS errors")
	}
	if installed {
		t.Error("IsInstalled() = true, want false on error")
	}
}

func TestResolveSessionFile(t *testing.T) {
	t.Parallel()
	ag := &ClaudeCodeAgent{}
	result := ag.ResolveSessionFile("/home/user/.claude/projects/foo", "abc-123-def")
	expected := "/home/user/.claude/projects/foo/abc-123-def.jsonl"
	if result != expected {
		t.Errorf("ResolveSessionFile() = %q, want %q", result, expected)
	}
}

func TestProtectedDirs(t *testing.T) {
	t.Parallel()
	ag := &ClaudeCodeAgent{}
	dirs := ag.ProtectedDirs()
	if len(dirs) != 1 || dirs[0] != ".claude" {
		t.Errorf("ProtectedDirs() = %v, want [.claude]", dirs)
	}
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
