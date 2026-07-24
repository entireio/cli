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

func TestRunIsolatedTextGeneratorCLIRaw_Success(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == windowsOS {
		// `echo` is a cmd.exe builtin on Windows, not an executable on PATH,
		// so exec.LookPath cannot resolve it.
		t.Skip("POSIX echo")
	}
	res, err := RunIsolatedTextGeneratorCLIRaw(context.Background(), nil, "echo", []string{"hello"}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Pin the raw bytes, trailing newline included: RunIsolated...Raw must not
	// trim. Trimming is HandleTextGenResult's job (see
	// TestHandleTextGenResult_TrimsStdout), and Claude's envelope parser needs
	// the unmodified stdout.
	if string(res.Stdout) != "hello\n" {
		t.Errorf("Stdout = %q; want %q", res.Stdout, "hello\n")
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d; want 0", res.ExitCode)
	}
}

func TestRunIsolatedTextGeneratorCLIRaw_NonZeroExitReturnsBothResultAndError(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == windowsOS {
		t.Skip("POSIX shell")
	}
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "printf 'stdout data'; printf 'stderr data' 1>&2; exit 7")
	}
	res, err := RunIsolatedTextGeneratorCLIRaw(context.Background(), runner, "sh", nil, "")
	if err == nil {
		t.Fatal("want non-nil err on non-zero exit")
	}
	if string(res.Stdout) != "stdout data" {
		t.Errorf("Stdout = %q; want 'stdout data' even on failure", res.Stdout)
	}
	if string(res.Stderr) != "stderr data" {
		t.Errorf("Stderr = %q; want 'stderr data'", res.Stderr)
	}
	if res.ExitCode != 7 {
		t.Errorf("ExitCode = %d; want 7", res.ExitCode)
	}
}

func TestRunIsolatedTextGeneratorCLIRaw_BinaryNotFoundReturnsExecError(t *testing.T) {
	t.Parallel()
	_, err := RunIsolatedTextGeneratorCLIRaw(context.Background(), nil, "definitely-not-installed-binary-xyz", nil, "")
	if err == nil {
		t.Fatal("want error for missing binary")
	}
	// Downstream Classifier will use isExecNotFoundErr on this; the helper
	// should NOT pre-format it, just return the raw error.
	var execErr *exec.Error
	if !errors.As(err, &execErr) {
		t.Errorf("want wrappable *exec.Error; got %T: %v", err, err)
	}
}

func TestRunIsolatedTextGeneratorCLIRaw_StdinDelivered(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == windowsOS {
		t.Skip("POSIX shell")
	}
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "cat")
	}
	res, err := RunIsolatedTextGeneratorCLIRaw(context.Background(), runner, "cat", nil, "hello via stdin")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(res.Stdout) != "hello via stdin" {
		t.Errorf("Stdout = %q; want stdin echoed back", res.Stdout)
	}
}

func TestRunIsolatedTextGeneratorCLIRaw_CanceledContextPreservesSentinel(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == windowsOS {
		t.Skip("POSIX shell")
	}
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "sleep 10")
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	_, err := RunIsolatedTextGeneratorCLIRaw(ctx, runner, "sh", nil, "")
	if err == nil {
		t.Fatal("want cancellation error")
	}
	// The caller's Classifier passes ctx errors through; the helper must not
	// wrap them in a way that defeats errors.Is.
	//
	// Assert against err ONLY. A previous version also accepted
	// errors.Is(ctx.Err(), context.Canceled), which is unconditionally true
	// here (cmd.Run returns after cancel() fires), so the whole assertion
	// could never fail and the sentinel-preservation contract was unpinned.
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled in err chain; got %v", err)
	}
}

// TestHandleTextGenResult_DeadlineCarriesPartialOutput pins the composition of
// the two error types: on a timeout the ctx sentinel must survive for errors.Is
// AND the captured evidence must survive for errors.As, so explain's timeout
// diagnostic can say "was generating output when killed" with the real stderr
// instead of guessing.
//
// This is the regression guard for the #964/#1005 reconciliation. Returning a
// bare sentinel from HandleTextGenResult loses the evidence; returning a bare
// *TextGenError loses the sentinel. Both failures are silent — the diagnostic
// simply degrades to its no-information branch.
func TestHandleTextGenResult_DeadlineCarriesPartialOutput(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == windowsOS {
		t.Skip("uses POSIX shell command")
	}

	// The CLI produces output on both streams, then stalls until the deadline
	// kills it.
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c",
			"echo 'partial output'; echo 'stalled talking to API' >&2; exec sleep 10")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	res, runErr := RunIsolatedTextGeneratorCLIRaw(ctx, runner, "sh", nil, "")
	_, err := HandleTextGenResult(res, runErr, AgentNameCodex, "empty", nil)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("errors.Is lost the ctx sentinel: %T %v", err, err)
	}
	var failure *TextGenerationError
	if !errors.As(err, &failure) {
		t.Fatalf("errors.As lost the evidence carrier: %T %v", err, err)
	}
	if failure.Stderr != "stalled talking to API" {
		t.Errorf("Stderr = %q, want the stderr written before the kill", failure.Stderr)
	}
	if failure.StdoutBytes == 0 {
		t.Error("StdoutBytes = 0, want the partial stdout to be counted")
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
