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

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
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
	// AgentName is an identifier used in log entries and the error-message
	// prefix wrapped into *TextGenerationError (e.g., "codex").
	AgentName string

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

// providerStreamError marks a parser error decoded from an explicit provider
// error event. The private wrapper is only transport metadata: callers still
// observe and match the original error through Unwrap.
type providerStreamError struct {
	err error
}

func (e *providerStreamError) Error() string { return e.err.Error() }
func (e *providerStreamError) Unwrap() error { return e.err }

// MarkProviderStreamError marks an error decoded from an explicit provider
// stream event. It lets the subprocess template distinguish decoded provider
// failures from EOF/parser failures caused by killing the subprocess. A
// semantically specific provider failure can then outrank a concurrent context
// error while an unclassified provider failure preserves both causes.
func MarkProviderStreamError(err error) error {
	if err == nil {
		return nil
	}
	return &providerStreamError{err: err}
}

func isProviderStreamError(err error) bool {
	var marked *providerStreamError
	return errors.As(err, &marked)
}

// Generate runs one streaming generation and returns the final result text.
//
// Error shapes by failure point:
//   - Pre-subprocess (StdoutPipe failure): plain wrapped error, since no
//     stderr/stdout exists yet to diagnose with.
//   - cmd.Start failure: *TextGenerationError with provider metadata, including
//     CLIMissing classification when the executable cannot be found.
//   - Anything after Start (parse error, non-zero exit, ctx cancellation):
//     *TextGenerationError carrying captured stderr and the stdout byte
//     count from countingReader, matching RunIsolatedTextGeneratorCLI's
//     error shape so the explain layer's diagnostic path can read both.
//   - LooksLikeUnrecognizedFlag predicate match: ErrUnrecognizedStreamingFlag
//     sentinel so the caller can fall back to non-streaming.
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
		provider := types.AgentName(t.AgentName)
		capturedStderr := strings.TrimSpace(stderr.String())
		kind := TextGenerationErrorUnknown
		message := fmt.Sprintf("%s stream start: %v", t.AgentName, err)
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			kind = TextGenerationErrorCLIMissing
			message = fmt.Sprintf("%s not found: %v", streamingProviderLabel(provider, t.AgentName), err)
		}
		cause := err
		if kind == TextGenerationErrorUnknown && ctx.Err() != nil {
			cause = errors.Join(err, ctx.Err())
		}
		return "", newTextGenerationError(provider, kind, message, cause, capturedStderr, 0, -1)
	}

	counter := &countingReader{r: stdout}
	var doneProgress *GenerationProgress
	parserProgress := progress
	if progress != nil {
		parserProgress = func(event GenerationProgress) {
			if event.Phase == PhaseDone {
				done := event
				doneProgress = &done
				return
			}
			progress(event)
		}
	}
	result, parseErr := t.Parser(counter, parserProgress)

	// Drain through the counter so StdoutBytes reflects the full subprocess
	// output even when the parser exited early (e.g. on a recognized
	// in-stream error). Reading from stdout directly would bypass counter.n.
	if _, drainErr := io.Copy(io.Discard, counter); drainErr != nil {
		logging.Debug(ctx, "draining stream stdout",
			slog.String("agent", t.AgentName),
			slog.String("error", drainErr.Error()))
	}
	waitErr := cmd.Wait()
	stderrStr := strings.TrimSpace(stderr.String())
	provider := types.AgentName(t.AgentName)

	if waitErr != nil && t.LooksLikeUnrecognizedFlag != nil && t.LooksLikeUnrecognizedFlag(stderrStr) {
		attrs := streamingFallbackLogAttrs(t.AgentName, stderrStr)
		logging.Warn(ctx, "CLI rejected streaming flags; caller should fall back to non-streaming",
			attrs[0], attrs[1])
		return "", ErrUnrecognizedStreamingFlag
	}

	if parseErr == nil && waitErr == nil {
		if doneProgress != nil {
			progress(*doneProgress)
		}
		return result, nil
	}
	cause := streamingFailureCause(parseErr, waitErr)

	// A semantically specific provider error decoded from stdout is authoritative
	// even if the context completes concurrently. Unknown provider/parser errors
	// can be consequences of a killed subprocess, so context remains discoverable.
	if isProviderStreamError(parseErr) {
		kind := classifyStreamingFailure(provider, stderrStr, parseErr)
		if kind != TextGenerationErrorUnknown || ctx.Err() == nil {
			message := parseErr.Error()
			return "", newTextGenerationError(
				provider, kind, message, fmt.Errorf("%s stream failed: %w", t.AgentName, cause),
				stderrStr, counter.n, streamingExitCode(waitErr),
			)
		}
	}

	kind := classifyStreamingFailure(provider, stderrStr, cause)
	if kind == TextGenerationErrorUnknown && ctx.Err() != nil {
		cause = errors.Join(cause, ctx.Err())
	}
	message := streamingFailureMessage(provider, t.AgentName, stderrStr, cause)
	return "", newTextGenerationError(
		provider, kind, message, fmt.Errorf("%s stream failed: %w", t.AgentName, cause),
		stderrStr, counter.n, streamingExitCode(waitErr),
	)
}

func streamingFailureMessage(
	provider types.AgentName,
	fallback string,
	stderr string,
	cause error,
) string {
	if stderr != "" {
		return stderr
	}
	return fmt.Sprintf("%s stream failed: %v", streamingProviderLabel(provider, fallback), cause)
}

func streamingFailureCause(parseErr, waitErr error) error {
	if parseErr == nil {
		return waitErr
	}
	if waitErr == nil {
		return parseErr
	}
	return errors.Join(parseErr, waitErr)
}

func classifyStreamingFailure(provider types.AgentName, stderr string, cause error) TextGenerationErrorKind {
	detail := stderr
	if cause != nil {
		if detail != "" {
			detail += "\n"
		}
		detail += cause.Error()
	}
	return classifyTextGenerationFailure(provider, detail)
}

func streamingExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func streamingProviderLabel(provider types.AgentName, fallback string) string {
	if label := SummaryProviderErrorLabel(provider); label != "" {
		return label
	}
	return fallback
}

func streamingFallbackLogAttrs(agentName, stderr string) []slog.Attr {
	return []slog.Attr{
		slog.String("agent", agentName),
		slog.Int("stderr_bytes", len(stderr)),
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
