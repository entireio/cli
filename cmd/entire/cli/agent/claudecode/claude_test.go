package claudecode

import (
	"context"
	"encoding/json"
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

func flagValue(args []string, name string) (string, bool) {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func TestBuildGenerateArgs_IsolatesSettingSources(t *testing.T) {
	t.Parallel()
	// Isolation is the security-critical invariant: --setting-sources must be
	// empty so user-level hooks and tool permissions (e.g. bypassPermissions)
	// are never loaded for this internal, injection-exposed call.
	args := buildGenerateArgs("haiku", "")
	got, ok := flagValue(args, "--setting-sources")
	if !ok {
		t.Fatalf("--setting-sources flag missing from args: %v", args)
	}
	if got != "" {
		t.Fatalf("--setting-sources = %q, want %q (must load no sources)", got, "")
	}
	// With no settings path, we inject nothing extra.
	if _, ok := flagValue(args, "--settings"); ok {
		t.Fatalf("--settings must be absent when there is no settings path: %v", args)
	}
}

func TestBuildGenerateArgs_PassesSettingsAsPath(t *testing.T) {
	t.Parallel()
	// The injected settings must be passed as a file path, not inline JSON, so a
	// key-bearing apiKeyHelper never lands in argv (ps / /proc/<pid>/cmdline).
	path := "/tmp/entire-claude-auth-123.json"
	args := buildGenerateArgs("haiku", path)

	if got, _ := flagValue(args, "--setting-sources"); got != "" {
		t.Fatalf("--setting-sources = %q, want empty", got)
	}
	got, ok := flagValue(args, "--settings")
	if !ok {
		t.Fatalf("--settings flag missing: %v", args)
	}
	if got != path {
		t.Fatalf("--settings = %q, want the file path %q", got, path)
	}
	// Guard against regressing to inline JSON in argv.
	if strings.Contains(got, "{") {
		t.Fatalf("--settings must be a path, not inline JSON: %q", got)
	}
}

func TestBuildStreamingGenerateArgs_KeepsIsolationAndAuthContract(t *testing.T) {
	t.Parallel()
	// The streaming argv must keep the same isolation (--setting-sources "")
	// and auth-injection (--settings <path>) contract as buildGenerateArgs;
	// dropping the injection silently breaks apiKeyHelper (API-billing) auth
	// on every streaming call.
	args := buildStreamingGenerateArgs("haiku", "")
	got, ok := flagValue(args, "--setting-sources")
	if !ok {
		t.Fatalf("--setting-sources flag missing from args: %v", args)
	}
	if got != "" {
		t.Fatalf("--setting-sources = %q, want %q (must load no sources)", got, "")
	}
	if got, ok := flagValue(args, "--output-format"); !ok || got != "stream-json" {
		t.Fatalf("--output-format = %q, want stream-json: %v", got, args)
	}
	if _, ok := flagValue(args, "--settings"); ok {
		t.Fatalf("--settings must be absent when there is no settings path: %v", args)
	}

	path := "/tmp/entire-claude-auth-123.json"
	args = buildStreamingGenerateArgs("haiku", path)
	got, ok = flagValue(args, "--settings")
	if !ok {
		t.Fatalf("--settings flag missing: %v", args)
	}
	if got != path {
		t.Fatalf("--settings = %q, want the file path %q", got, path)
	}
}

func TestWriteAuthSettingsFile_WritesOnlyAPIKeyHelper0600(t *testing.T) {
	t.Parallel()
	helper := `echo "sk-ant-secret"` // could embed a literal key
	path, cleanup, err := writeAuthSettingsFile(helper)
	if err != nil {
		t.Fatalf("writeAuthSettingsFile: %v", err)
	}
	if cleanup == nil {
		t.Fatal("cleanup func is nil")
	}
	defer cleanup()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat settings file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("settings file perm = %o, want 0600", perm)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings file: %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("settings file is not valid JSON: %v (%s)", err, data)
	}
	if settings["apiKeyHelper"] != helper {
		t.Fatalf("apiKeyHelper = %v, want %q", settings["apiKeyHelper"], helper)
	}
	if len(settings) != 1 {
		t.Fatalf("settings file must contain only apiKeyHelper, got %v", settings)
	}

	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cleanup did not remove settings file (stat err=%v)", err)
	}
}

func TestWriteAuthSettingsFile_EmptyHelperNoFile(t *testing.T) {
	t.Parallel()
	path, cleanup, err := writeAuthSettingsFile("")
	if err != nil {
		t.Fatalf("writeAuthSettingsFile(\"\"): %v", err)
	}
	if path != "" {
		t.Fatalf("path = %q, want empty for no apiKeyHelper", path)
	}
	if cleanup != nil {
		t.Fatal("cleanup should be nil when no file is written")
	}
}

func TestReadUserAPIKeyHelper_FromClaudeConfigDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "settings.json"),
		[]byte(`{"apiKeyHelper":"echo secret-cmd","permissions":{"defaultMode":"bypassPermissions"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readUserAPIKeyHelper(); got != "echo secret-cmd" {
		t.Fatalf("readUserAPIKeyHelper() = %q, want %q", got, "echo secret-cmd")
	}
}

func TestReadUserAPIKeyHelper_MissingFileReturnsEmpty(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // no settings.json inside
	if got := readUserAPIKeyHelper(); got != "" {
		t.Fatalf("readUserAPIKeyHelper() = %q, want empty for missing file", got)
	}
}

// TestGenerateText_InjectsAPIKeyHelperIntoArgv pins the apiKeyHelper injection
// through the REAL GenerateText argv, not just through buildGenerateArgs in
// isolation. Without this, deleting the injection from GenerateText and passing
// buildGenerateArgs(model, "") leaves every existing test green while
// API-billing users silently lose auth on the non-streaming path — the exact
// regression #964 was written to fix.
//
// Also pins the security-critical --setting-sources "" isolation end-to-end.
//
// t.Setenv: no t.Parallel.
func TestGenerateText_InjectsAPIKeyHelperIntoArgv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "settings.json"),
		[]byte(`{"apiKeyHelper":"echo test-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var gotArgs []string
	ag := &ClaudeCodeAgent{
		CommandRunner: func(ctx context.Context, _ string, args ...string) *exec.Cmd {
			gotArgs = args
			return exec.CommandContext(ctx, "true")
		},
	}
	_, _ = ag.GenerateText(context.Background(), "prompt", "haiku") //nolint:errcheck // asserting on captured argv, not the result

	settings, ok := flagValue(gotArgs, "--settings")
	if !ok {
		t.Fatalf("--settings absent from GenerateText argv: %v", gotArgs)
	}
	if settings == "" || strings.Contains(settings, "{") {
		t.Errorf("--settings = %q; want a file path, never inline JSON (argv is ps-visible)", settings)
	}
	src, ok := flagValue(gotArgs, "--setting-sources")
	if !ok || src != "" {
		t.Errorf("--setting-sources = %q (present=%v); want empty (load nothing)", src, ok)
	}
}

// TestGenerateText_NoAPIKeyHelperOmitsSettings is the negative half: with no
// apiKeyHelper configured we must not pass --settings at all.
//
// t.Setenv: no t.Parallel.
func TestGenerateText_NoAPIKeyHelperOmitsSettings(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // no settings.json inside

	var gotArgs []string
	ag := &ClaudeCodeAgent{
		CommandRunner: func(ctx context.Context, _ string, args ...string) *exec.Cmd {
			gotArgs = args
			return exec.CommandContext(ctx, "true")
		},
	}
	_, _ = ag.GenerateText(context.Background(), "prompt", "haiku") //nolint:errcheck // asserting on captured argv, not the result

	if v, ok := flagValue(gotArgs, "--settings"); ok {
		t.Errorf("--settings = %q; must be absent with no apiKeyHelper", v)
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
