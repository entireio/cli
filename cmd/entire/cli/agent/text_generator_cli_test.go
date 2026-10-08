package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/execx"
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

// Reproduces the `entire review` judge hang: the provider CLI backgrounds a
// grandchild that inherits stdout and outlives it, so killing only the direct
// child leaves the output pipe open and cmd.Run blocks past the deadline. The
// call must return once the deadline fires instead of hanging forever.
func TestRunIsolatedTextGeneratorCLI_ReturnsOnDeadlineWithPipeHoldingGrandchild(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == windowsOS {
		t.Skip("uses POSIX shell command")
	}

	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		// `sleep 60 &` backgrounds a grandchild holding stdout; `wait` keeps the
		// shell alive so only a group-wide kill (or the WaitDelay backstop) ends both.
		return exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 60 & echo ready; wait")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, _, _, err := RunIsolatedTextGeneratorCLI(ctx, runner, "test", "test", nil, "")
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected context.DeadlineExceeded, got %v", err)
		}
	case <-time.After(execx.KillWaitDelay + 5*time.Second):
		t.Fatal("RunIsolatedTextGeneratorCLI hung past the deadline: a grandchild held the output pipe open")
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

func TestRunIsolatedTextGeneratorCLI_EnvironmentOverrides(t *testing.T) {
	t.Parallel()
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd { return exec.CommandContext(ctx, "env") }
	out, _, _, err := RunIsolatedTextGeneratorCLI(t.Context(), runner, "test", "test", nil, "", "ENTIRE_GENERATION_PROBE=first", "ENTIRE_GENERATION_PROBE=last", "GIT_DIR=must-not-leak")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ENTIRE_GENERATION_PROBE=last") || strings.Contains(out, "ENTIRE_GENERATION_PROBE=first") || strings.Contains(out, "GIT_DIR=") {
		t.Fatalf("overrides not applied or Git environment leaked: %q", out)
	}
}

// The prompt carries untrusted transcript content and the agent CLIs let
// their file tools reach the working directory without approval, so each run
// gets a fresh empty directory, never the shared system temp dir, and the
// directory is gone afterwards.
func TestRunIsolatedTextGeneratorCLI_RunsInFreshEmptyDir(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == windowsOS {
		t.Skip("uses sh")
	}
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", `pwd -P; ls -A`)
	}
	out, _, _, err := RunIsolatedTextGeneratorCLI(context.Background(), runner, "test", "test-agent", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lines := strings.Split(out, "\n")
	dir := lines[0]
	sharedTemp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(dir) == filepath.Clean(sharedTemp) {
		t.Fatalf("ran in the shared temp dir %q", dir)
	}
	if len(lines) > 1 {
		t.Errorf("working dir %q is not empty: %q", dir, lines[1:])
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("working dir %q still exists after the run (stat err = %v)", dir, err)
	}
}

// The caller's working directory is usually the repository, and a shell
// exports it as PWD. exec.Cmd does not rewrite PWD for an explicit Env, so
// without TextGenerationEnv the generator would be told where the repository
// is even though it runs elsewhere. env reports the environment it received
// without normalizing PWD, as a shell would.
func TestRunIsolatedTextGeneratorCLI_DoesNotLeakTheCallersDirectory(t *testing.T) {
	t.Setenv("PWD", "/repo/must-not-leak")
	t.Setenv("OLDPWD", "/repo/must-not-leak-either")
	t.Setenv("git_dir", "/repo/.git") // Windows names are case-insensitive
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd { return exec.CommandContext(ctx, "env") }
	dir, cleanup, err := NewTextGenerationDir()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	out, _, _, err := RunIsolatedTextGeneratorCLIIn(t.Context(), runner, dir, "test", "test", nil, "")
	if err != nil {
		t.Fatalf("RunIsolatedTextGeneratorCLIIn: %v", err)
	}
	// Report only the offending entries: the rest is the test's environment.
	for _, kv := range strings.Split(out, "\n") {
		if strings.Contains(kv, "/repo/") {
			t.Errorf("generator environment names the caller's directory: %s", kv)
		}
	}
	if !slices.Contains(strings.Split(out, "\n"), "PWD="+dir) {
		t.Errorf("PWD is not the generation directory %q", dir)
	}
}
