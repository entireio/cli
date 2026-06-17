package agent

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

const windowsOS = "windows"

func TestRunIsolatedTextGeneratorCLI_EmptyOutput(t *testing.T) {
	t.Parallel()

	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "echo", "-n", "")
	}
	// On some systems echo -n "" still prints a newline; use printf for reliable empty output
	if runtime.GOOS != windowsOS {
		runner = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "printf", "")
		}
	}
	_, err := RunIsolatedTextGeneratorCLI(context.Background(), runner, "test", "test-agent", nil, "")
	if err == nil {
		t.Fatal("expected error for empty output")
	}
	if !strings.Contains(err.Error(), "test-agent CLI returned empty output") {
		t.Fatalf("error = %q, want it to contain %q", err.Error(), "test-agent CLI returned empty output")
	}
}

func TestRunIsolatedTextGeneratorCLI_NonZeroExit(t *testing.T) {
	t.Parallel()

	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "echo 'some error' >&2; exit 1")
	}
	_, err := RunIsolatedTextGeneratorCLI(context.Background(), runner, "test", "myagent", nil, "")
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}
	errMsg := err.Error()
	if !strings.Contains(errMsg, "myagent CLI failed (exit 1)") {
		t.Fatalf("error = %q, want it to contain exit code info", errMsg)
	}
	if !strings.Contains(errMsg, "some error") {
		t.Fatalf("error = %q, want it to contain stderr detail", errMsg)
	}
}

func TestRunIsolatedTextGeneratorCLI_NonZeroExitFallsBackToStdout(t *testing.T) {
	t.Parallel()

	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "echo 'stdout detail'; exit 1")
	}
	_, err := RunIsolatedTextGeneratorCLI(context.Background(), runner, "test", "myagent", nil, "")
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}
	if !strings.Contains(err.Error(), "stdout detail") {
		t.Fatalf("error = %q, want it to contain stdout as fallback detail", err.Error())
	}
}

func TestRunIsolatedTextGeneratorCLI_BinaryNotFound(t *testing.T) {
	t.Parallel()

	_, err := RunIsolatedTextGeneratorCLI(context.Background(), nil, "nonexistent-binary-12345", "myagent", nil, "")
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
	if !strings.Contains(err.Error(), "myagent CLI not found") {
		t.Fatalf("error = %q, want it to contain 'not found'", err.Error())
	}
}

func TestRunIsolatedTextGeneratorCLI_NilRunnerDefaultsToExec(t *testing.T) {
	t.Parallel()

	// With nil runner, it defaults to exec.CommandContext, so "echo" should work
	result, err := RunIsolatedTextGeneratorCLI(context.Background(), nil, "echo", "echo", []string{"hello"}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "hello" {
		t.Fatalf("result = %q, want %q", result, "hello")
	}
}

func TestRunIsolatedTextGeneratorCLI_CanceledContextPreservesSentinel(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == windowsOS {
		t.Skip("uses POSIX shell command")
	}

	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "sleep 10")
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	_, err := RunIsolatedTextGeneratorCLI(ctx, runner, "test", "test", nil, "")
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestStripGitEnv(t *testing.T) {
	t.Parallel()

	env := []string{
		"HOME=/home/user",
		"GIT_DIR=/some/dir",
		"PATH=/usr/bin",
		"GIT_WORK_TREE=/some/tree",
		"EDITOR=vim",
	}
	filtered := StripGitEnv(env)

	for _, e := range filtered {
		if strings.HasPrefix(e, "GIT_") {
			t.Fatalf("GIT_ variable not stripped: %s", e)
		}
	}
	if len(filtered) != 3 {
		t.Fatalf("expected 3 entries, got %d: %v", len(filtered), filtered)
	}
}
