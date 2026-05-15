package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// StreamingGeneratorTemplate is the shared subprocess-lifecycle wrapper for
// streaming text generators. Per-agent code provides BuildCmd (argv) and
// Parser (stdout → progress events); the template handles Start, drain,
// Wait, stderr capture, and error wrapping.
//
// Parallels review/types/template.ReviewerTemplate (established in PR #1192).
//
// Fields must be non-nil before Generate is called; nil values cause
// Generate to return ErrTemplateMisconfigured.
type StreamingGeneratorTemplate struct {
	// AgentName is an identifier used in log entries (e.g., "codex").
	AgentName string

	// DisplayName is the user-facing CLI binary name used in *TextGenerationError
	// wrapping (e.g., "codex"). Often the same as AgentName but kept separate
	// for cases where they differ (e.g., Cursor's agent is named "cursor"
	// but its binary is "agent").
	DisplayName string

	// BuildCmd constructs the *exec.Cmd for one streaming call. Implementations
	// MUST set cmd.Stdin to the prompt and cmd.Args to the agent's
	// streaming-mode invocation. The template will set cmd.Dir = os.TempDir()
	// and cmd.Env = StripGitEnv(os.Environ()) before Start.
	BuildCmd func(ctx context.Context, prompt, model string) *exec.Cmd

	// Parser consumes the agent's stdout stream and dispatches progress
	// callbacks. Returns the final result text on success. Must read until
	// stdout EOF before returning so the template can call Wait cleanly.
	// progress may be nil; Parser must handle that.
	Parser func(stdout io.Reader, progress ProgressFn) (result string, err error)

	// LooksLikeUnrecognizedFlag is optional. When non-nil and the subprocess
	// fails with stderr matching the predicate, the caller can fall back to
	// the agent's non-streaming GenerateText path. The template surfaces
	// this signal via ErrUnrecognizedStreamingFlag so the caller decides.
	LooksLikeUnrecognizedFlag func(stderr string) bool
}

// ErrTemplateMisconfigured is returned when required template fields are nil.
var ErrTemplateMisconfigured = errors.New("streaming template misconfigured")

// ErrUnrecognizedStreamingFlag is returned when LooksLikeUnrecognizedFlag
// indicates the CLI rejected a streaming-specific flag. Callers that
// implement a fallback should errors.Is this to detect.
var ErrUnrecognizedStreamingFlag = errors.New("CLI rejected streaming flag")

// Generate runs one streaming generation and returns the final result text.
// On error, returns *TextGenerationError carrying captured stderr and the
// stdout byte count, matching RunIsolatedTextGeneratorCLI's error shape.
func (t *StreamingGeneratorTemplate) Generate(
	ctx context.Context,
	prompt, model string,
	progress ProgressFn,
) (string, error) {
	if t.BuildCmd == nil || t.Parser == nil {
		return "", ErrTemplateMisconfigured
	}

	cmd := t.BuildCmd(ctx, prompt, model)
	cmd.Dir = os.TempDir()
	cmd.Env = StripGitEnv(os.Environ())

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("%s stream stdout pipe: %w", t.AgentName, err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return "", &TextGenerationError{
				Err:         ctx.Err(),
				Stderr:      strings.TrimSpace(stderr.String()),
				StdoutBytes: 0,
			}
		}
		return "", fmt.Errorf("%s stream start: %w", t.AgentName, err)
	}

	counter := &countingReader{r: stdout}
	result, parseErr := t.Parser(counter, progress)

	if _, drainErr := io.Copy(io.Discard, stdout); drainErr != nil {
		logging.Debug(ctx, "draining stream stdout",
			slog.String("agent", t.AgentName),
			slog.String("error", drainErr.Error()))
	}
	waitErr := cmd.Wait()

	if ctx.Err() != nil {
		stderrStr := strings.TrimSpace(stderr.String())
		return "", &TextGenerationError{
			Err:         ctx.Err(),
			Stderr:      stderrStr,
			StdoutBytes: counter.n,
		}
	}

	if parseErr == nil && waitErr == nil {
		return result, nil
	}

	stderrStr := strings.TrimSpace(stderr.String())
	if waitErr != nil && t.LooksLikeUnrecognizedFlag != nil && t.LooksLikeUnrecognizedFlag(stderrStr) {
		logging.Warn(ctx, "CLI rejected streaming flags; caller should fall back to non-streaming",
			slog.String("agent", t.AgentName),
			slog.String("stderr", stderrStr))
		return "", ErrUnrecognizedStreamingFlag
	}

	wrappedErr := waitErr
	if wrappedErr == nil {
		wrappedErr = parseErr
	}
	return "", &TextGenerationError{
		Err:         fmt.Errorf("%s stream failed: %w", t.AgentName, wrappedErr),
		Stderr:      stderrStr,
		StdoutBytes: counter.n,
	}
}

// countingReader passes bytes through and counts them. Used by the template
// so the diagnostic path can ask "did the subprocess produce any output?".
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err //nolint:wrapcheck // io.Reader contract requires passthrough (including io.EOF) without wrapping
}
