package claudecode

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

const windowsOS = "windows"

func TestClaudeCodeAgent_LaunchCmd(t *testing.T) {
	t.Parallel()
	a := NewClaudeCodeAgent()
	launcher, ok := a.(agent.Launcher)
	if !ok {
		t.Fatal("ClaudeCodeAgent does not implement agent.Launcher")
	}
	// Binary may not be on PATH in CI; ErrNotFound is acceptable for this test.
	cmd, err := launcher.LaunchCmd(context.Background(), "hello world")
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			t.Skip("claude binary not on PATH; skipping cmd shape check")
		}
		t.Fatalf("LaunchCmd: %v", err)
	}
	if cmd == nil {
		t.Fatal("nil cmd")
	}
	if cmd.Path == "" {
		t.Error("cmd.Path empty")
	}
	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, "hello world") {
		t.Errorf("args missing prompt: %v", cmd.Args)
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

func TestGenerateText_ArrayResponse(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == windowsOS {
		t.Skip("POSIX shell")
	}
	ag := &ClaudeCodeAgent{
		CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			response := `[{"type":"system","subtype":"init"},{"type":"assistant","message":"Working on it"},{"type":"result","result":"final generated text"}]`
			return exec.CommandContext(ctx, "sh", "-c", "printf '%s' '"+response+"'")
		},
	}

	result, err := ag.GenerateText(context.Background(), "prompt", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result != "final generated text" {
		t.Fatalf("GenerateText() = %q, want %q", result, "final generated text")
	}
}

func TestGenerateText_EnvelopeErrorReturnsTextGenError(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == windowsOS {
		t.Skip("POSIX shell")
	}
	ag := &ClaudeCodeAgent{
		CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			response := `{"type":"result","subtype":"success","is_error":true,"api_error_status":401,"result":"Auth required"}`
			return exec.CommandContext(ctx, "sh", "-c", "printf '%s' '"+response+"'")
		},
	}
	_, err := ag.GenerateText(context.Background(), "prompt", "")
	var tge *agent.TextGenError
	if !errors.As(err, &tge) {
		t.Fatalf("err = %v; want *agent.TextGenError", err)
	}
	if tge.Kind != agent.TextGenErrorAuth {
		t.Fatalf("Kind = %v; want %v", tge.Kind, agent.TextGenErrorAuth)
	}
}

func TestGenerateText_CLIMissing(t *testing.T) {
	t.Parallel()
	ag := &ClaudeCodeAgent{
		CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "/nonexistent/binary/that/does/not/exist")
		},
	}
	_, err := ag.GenerateText(context.Background(), "prompt", "")
	var tge *agent.TextGenError
	if !errors.As(err, &tge) {
		t.Fatalf("err = %v; want *agent.TextGenError", err)
	}
	if tge.Kind != agent.TextGenErrorCLIMissing {
		t.Fatalf("Kind = %v; want %v", tge.Kind, agent.TextGenErrorCLIMissing)
	}
}

func TestGenerateText_StderrAuthFallback(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == windowsOS {
		t.Skip("POSIX shell")
	}
	ag := &ClaudeCodeAgent{
		CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "sh", "-c", "printf 'Invalid API key' 1>&2; exit 2")
		},
	}
	_, err := ag.GenerateText(context.Background(), "prompt", "")
	var tge *agent.TextGenError
	if !errors.As(err, &tge) {
		t.Fatalf("err = %v; want *agent.TextGenError", err)
	}
	if tge.Kind != agent.TextGenErrorAuth {
		t.Fatalf("Kind = %v; want %v", tge.Kind, agent.TextGenErrorAuth)
	}
}

// TestGenerateText_UnparseableStdoutDoesNotMaskStderr pins the live half of the
// PR #1005 cursor-bot finding. The bot reported that unparseable stdout
// preempted ctx cancellation; the same early return also preempted stderr
// classification, so a real 401 on stderr surfaced as a JSON-parse complaint.
// Claude can emit a node warning or a progress line on stdout and still fail
// with an actionable error on stderr.
func TestGenerateText_UnparseableStdoutDoesNotMaskStderr(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == windowsOS {
		t.Skip("POSIX shell")
	}
	ag := &ClaudeCodeAgent{
		CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "sh", "-c",
				`printf 'warning: something on stdout'; printf 'ERROR: 401 Unauthorized' 1>&2; exit 1`)
		},
	}
	_, err := ag.GenerateText(context.Background(), "prompt", "")
	var tge *agent.TextGenError
	if !errors.As(err, &tge) {
		t.Fatalf("err = %v; want *agent.TextGenError", err)
	}
	if tge.Kind != agent.TextGenErrorAuth {
		t.Errorf("Kind = %q; want auth (stderr must win over the stdout parse failure)", tge.Kind)
	}
	if strings.Contains(tge.Message, "failed to parse") {
		t.Errorf("Message = %q; must not be a JSON-parse complaint", tge.Message)
	}
	if tge.Message != "ERROR: 401 Unauthorized" {
		t.Errorf("Message = %q; want the stderr verbatim", tge.Message)
	}
}

// TestGenerateText_LaunchFailureCarriesDiagnostic pins that a launch failure
// which is NOT "binary missing" (permission denied, exec format error) still
// reaches the user with something actionable. Such a process produces no
// stdout, no stderr and no exit code, so runErr is the only description of it.
func TestGenerateText_LaunchFailureCarriesDiagnostic(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == windowsOS {
		t.Skip("POSIX file permissions")
	}
	noperm := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(noperm, []byte("#!/bin/sh\necho hi\n"), 0o000); err != nil {
		t.Fatalf("write: %v", err)
	}
	ag := &ClaudeCodeAgent{
		CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return exec.CommandContext(ctx, noperm)
		},
	}
	_, err := ag.GenerateText(context.Background(), "prompt", "")
	var tge *agent.TextGenError
	if !errors.As(err, &tge) {
		t.Fatalf("err = %v; want *agent.TextGenError", err)
	}
	if tge.Kind == agent.TextGenErrorCLIMissing {
		t.Error("permission denied must not be reported as CLI-missing (misdirects to a reinstall)")
	}
	if tge.Message == "" {
		t.Error("Message is empty; the user has no way to learn what went wrong")
	}
}
