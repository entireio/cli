package agent

import (
	"context"
	"errors"
	"fmt"
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
	_, _, _, err := RunIsolatedTextGeneratorCLI(context.Background(), runner, "test", "test-agent", nil, "")
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
	_, capturedStderr, stdoutBytes, err := RunIsolatedTextGeneratorCLI(context.Background(), runner, "test", "myagent", nil, "")
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
	// The captured-output return values feed the explain timeout diagnostic;
	// callers wrap them into *TextGenerationError.
	if capturedStderr != "some error" {
		t.Fatalf("capturedStderr = %q, want %q", capturedStderr, "some error")
	}
	if stdoutBytes != 0 {
		t.Fatalf("stdoutBytes = %d, want 0 (nothing was written to stdout)", stdoutBytes)
	}
}

func TestRunIsolatedTextGeneratorCLI_NonZeroExitFallsBackToStdout(t *testing.T) {
	t.Parallel()

	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "echo 'stdout detail'; exit 1")
	}
	_, capturedStderr, stdoutBytes, err := RunIsolatedTextGeneratorCLI(context.Background(), runner, "test", "myagent", nil, "")
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}
	if !strings.Contains(err.Error(), "stdout detail") {
		t.Fatalf("error = %q, want it to contain stdout as fallback detail", err.Error())
	}
	if capturedStderr != "" {
		t.Fatalf("capturedStderr = %q, want empty (nothing was written to stderr)", capturedStderr)
	}
	if stdoutBytes == 0 {
		t.Fatal("stdoutBytes = 0, want the stdout the CLI produced to be counted")
	}
}

func TestRunIsolatedTextGeneratorCLI_BinaryNotFound(t *testing.T) {
	t.Parallel()

	_, _, _, err := RunIsolatedTextGeneratorCLI(context.Background(), nil, "nonexistent-binary-12345", "myagent", nil, "")
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
	result, _, _, err := RunIsolatedTextGeneratorCLI(context.Background(), nil, "echo", "echo", []string{"hello"}, "")
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

	_, _, _, err := RunIsolatedTextGeneratorCLI(ctx, runner, "test", "test", nil, "")
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestRunIsolatedTextGeneratorCLI_DeadlineCarriesPartialOutput(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == windowsOS {
		t.Skip("uses POSIX shell command")
	}

	// The CLI produces some output on both streams, then stalls until the
	// deadline kills it. The sentinel must be preserved AND the captured
	// evidence returned, so the timeout diagnostic can say "was generating
	// output when killed" with the real stderr instead of guessing.
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c",
			"echo 'partial output'; echo 'stalled talking to API' >&2; exec sleep 10")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, capturedStderr, stdoutBytes, err := RunIsolatedTextGeneratorCLI(ctx, runner, "test", "test", nil, "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
	if capturedStderr != "stalled talking to API" {
		t.Fatalf("capturedStderr = %q, want the stderr written before the kill", capturedStderr)
	}
	if stdoutBytes == 0 {
		t.Fatal("stdoutBytes = 0, want the partial stdout to be counted")
	}
}

func TestTextGenerationError_PreservesSentinelAndPayload(t *testing.T) {
	t.Parallel()

	err := &TextGenerationError{Err: context.DeadlineExceeded, Stderr: "stalled", StdoutBytes: 42}

	// The explain layer routes timeouts with errors.Is and recovers the
	// evidence with errors.As; both must survive additional wrapping.
	wrapped := fmt.Errorf("summary generation failed: %w", err)
	if !errors.Is(wrapped, context.DeadlineExceeded) {
		t.Fatal("context.DeadlineExceeded sentinel must survive TextGenerationError.Unwrap")
	}
	var genErr *TextGenerationError
	if !errors.As(wrapped, &genErr) {
		t.Fatal("errors.As must recover *TextGenerationError through wrapping")
	}
	if genErr.Stderr != "stalled" {
		t.Fatalf("Stderr = %q, want %q", genErr.Stderr, "stalled")
	}
	if genErr.StdoutBytes != 42 {
		t.Fatalf("StdoutBytes = %d, want 42", genErr.StdoutBytes)
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
