package claudecode

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

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

const streamEventTypeSystem = "system"

// GenerateTextStreaming runs the Claude CLI in stream-json mode, dispatches
// progress events to the optional callback, and returns the final result text.
// Implements the agent.StreamingTextGenerator interface.
//
// If the CLI rejects the stream-json flags (older Claude CLI), this falls back
// to the non-streaming GenerateText path — without progress events.
func (c *ClaudeCodeAgent) GenerateTextStreaming(
	ctx context.Context,
	prompt, model string,
	progress agent.ProgressFn,
) (string, error) {
	if model == "" {
		model = "haiku"
	}

	commandRunner := c.CommandRunner
	if commandRunner == nil {
		commandRunner = exec.CommandContext
	}

	// Re-inject only the user's apiKeyHelper (via a 0600 file, never argv) so
	// API-billing auth keeps working under --setting-sources "" isolation —
	// same contract as GenerateText (see buildGenerateArgs). Best-effort: if
	// extracting/writing the helper fails, run without it (env/keychain auth
	// still work) rather than failing the call.
	settingsPath, cleanup, err := writeAuthSettingsFile(readUserAPIKeyHelper())
	if err != nil {
		settingsPath = ""
	}
	if cleanup != nil {
		defer cleanup()
	}

	cmd := commandRunner(ctx, "claude", buildStreamingGenerateArgs(model, settingsPath)...)

	cmd.Dir = os.TempDir()
	cmd.Env = agent.StripGitEnv(os.Environ())
	cmd.Stdin = strings.NewReader(prompt)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", streamFailure("", 0, 0, err, fmt.Sprintf("claude stream stdout pipe: %v", err))
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		// A missing/unexecutable binary must classify as CLIMissing so the user
		// gets "not installed or not on PATH" rather than a raw exec error.
		if agent.IsExecNotFoundErr(err) {
			return "", &agent.TextGenerationError{
				Err: &agent.TextGenError{
					Kind:     agent.TextGenErrorCLIMissing,
					Provider: agent.AgentNameClaudeCode,
					Cause:    err,
				},
			}
		}
		return "", streamFailure("", 0, 0, err, fmt.Sprintf("claude stream start: %v", err))
	}

	// Count stdout bytes so the timeout diagnostic can distinguish "provider
	// produced no output" from "was generating output when killed".
	counted := &countingReader{r: stdout}
	final, malformed, parseErr := streamClaudeResponse(counted, makeProgressDispatcher(progress))

	// Drain any unread stdout so the subprocess can exit cleanly even if the
	// scanner aborted early (e.g. bufio.ErrTooLong on an oversized line).
	// Without this, a blocked pipe would deadlock cmd.Wait().
	if _, drainErr := io.Copy(io.Discard, counted); drainErr != nil {
		logging.Debug(ctx, "draining claude stream stdout", slog.String("error", drainErr.Error()))
	}
	waitErr := cmd.Wait()

	if malformed > 0 {
		logging.Warn(ctx, "skipped malformed claude stream lines", slog.Int("count", malformed))
	}

	// Specific envelope error outranks a generic ctx-cancel message.
	if final != nil && final.IsError {
		return "", &agent.TextGenerationError{
			Err:         envelopeErrorMessage(final),
			Stderr:      agent.TruncateStderr(stderr.String()),
			StdoutBytes: counted.n,
		}
	}

	if final != nil {
		// A fully decoded success wins over a context-caused kill that lands
		// after the provider completed (e.g. the deadline firing between the
		// result envelope and cmd.Wait). A non-context process failure — the
		// CLI exiting non-zero on its own — remains authoritative.
		if waitErr != nil && !isContextKill(ctx, waitErr) {
			return "", streamFailure(stderr.String(), counted.n, exitCodeOf(waitErr), waitErr,
				fmt.Sprintf("claude stream failed: %v", waitErr))
		}
		if final.Result == nil {
			return "", streamFailure(stderr.String(), counted.n, 0, nil, "claude returned empty result")
		}
		if progress != nil {
			progress(agent.GenerationProgress{
				Phase:        agent.PhaseDone,
				OutputTokens: outputTokensFromUsage(final.Usage),
				DurationMs:   final.DurationMs,
			})
		}
		return *final.Result, nil
	}

	if ctx.Err() != nil {
		// Wrap the sentinel in a *agent.TextGenerationError so the explain
		// layer's timeout diagnostic gets its evidence (captured stderr and
		// how much stdout arrived before the kill) instead of a bare sentinel
		// that forces it to fabricate a cause. errors.Is against the sentinel
		// keeps working through Unwrap.
		sentinel := context.Canceled
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			sentinel = context.DeadlineExceeded
		}
		return "", &agent.TextGenerationError{
			Err: sentinel,
			// Capped like every other evidence site. attempt.stderrCaptured is
			// rendered straight into an explainRow with no truncation of its
			// own, so an unbounded buffer (a CLI stuck in a retry loop spamming
			// warnings for the whole timeout window) would bury the cause and
			// try rows this PR formats.
			Stderr:      agent.TruncateStderr(stderr.String()),
			StdoutBytes: counted.n,
		}
	}

	// No envelope: check if the CLI rejected streaming flags (older version).
	if waitErr != nil {
		stderrStr := stderr.String()
		if looksLikeUnrecognizedFlag(stderrStr) {
			logging.Warn(ctx, "claude CLI rejected stream-json flags; falling back to non-streaming (no progress output)",
				slog.String("stderr", strings.TrimSpace(stderrStr)))
			return c.GenerateText(ctx, prompt, model)
		}
		return "", streamFailure(stderrStr, counted.n, exitCodeOf(waitErr), waitErr,
			fmt.Sprintf("claude stream failed: %v", waitErr))
	}

	if parseErr != nil {
		return "", streamFailure(stderr.String(), counted.n, 0, parseErr,
			fmt.Sprintf("claude stream parse: %v", parseErr))
	}
	return "", streamFailure(stderr.String(), counted.n, 0, nil,
		"claude exited without producing a result")
}

// streamFailure classifies a streaming failure the same way the non-streaming
// path does — HTTP status on stderr, then Claude's auth-phrase fallback — and
// attaches the captured evidence.
//
// EVERY failure return in GenerateTextStreaming goes through this or an
// explicit *TextGenerationError — there are no bare fmt.Errorf failure returns
// left in this file, and TestGenerateTextStreaming_ClassifiesStderrFailures
// pins that for the auth-phrase, 401, 429 and 404 shapes.
//
// Why it matters: TextGeneratorAdapter prefers streaming, so this is the path
// `explain --generate` actually takes for Claude. A stale key (claude exits 2,
// "Invalid API key" on stderr, no envelope) must produce "Claude
// authentication failed" with a remediation row, not a raw Go error string via
// formatCheckpointSummaryError's default branch.
func streamFailure(stderrBuf string, stdoutBytes int, exitCode int, cause error, fallbackMsg string) error {
	stderrStr := strings.TrimSpace(stderrBuf)
	msg := stderrStr
	if msg == "" {
		msg = fallbackMsg
	}
	kind := agent.ClassifyStderrHTTPStatus(stderrStr)
	if kind == agent.TextGenErrorUnknown && containsAuthPhrase(stderrStr) {
		kind = agent.TextGenErrorAuth
	}
	return &agent.TextGenerationError{
		Err: &agent.TextGenError{
			Kind:     kind,
			Provider: agent.AgentNameClaudeCode,
			Message:  agent.TruncateStderr(msg),
			ExitCode: exitCode,
			Cause:    cause,
		},
		Stderr:      agent.TruncateStderr(stderrBuf),
		StdoutBytes: stdoutBytes,
	}
}

// exitCodeOf returns the process exit code from err, or 0 when err is not an
// *exec.ExitError (a launch failure produces no exit code).
func exitCodeOf(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 0
}

// envelopeErrorMessage formats an is_error result envelope as a typed
// *agent.TextGenError so the explain layer's renderTextGenError maps it to
// actionable user guidance (auth / rate-limit / config / cli-missing).
//
// Scope, precisely: both Claude paths share classifyEnvelopeFields, so they
// cannot drift *on the envelope*. They are NOT at parity otherwise — the
// non-streaming path additionally classifies stderr (HTTP status, then auth
// phrase) for failures that produce no envelope, and this one does not. A
// stale key that makes claude exit 2 with "Invalid API key" on stderr and no
// envelope therefore reaches the user as an untyped "claude stream failed: …"
// with no remediation row, whereas the non-streaming path names it an auth
// failure.
//
// That asymmetry predates the TextGenError work (it is unchanged from the
// pre-#1005 streaming path) and matters more than it looks: TextGeneratorAdapter
// prefers streaming, so this is the path `explain --generate` actually takes for
// Claude. Closing it means routing the returns around line 148 through the same
// classification as generate.go's stderr block.
//
// No exit code is stamped: envelope errors arrive on stdout while the CLI
// itself exits successfully — Claude's is_error envelope semantics distinguish
// "operational failure with structured details" from "subprocess crash".
func envelopeErrorMessage(final *streamEvent) error {
	resultText := ""
	if final.Result != nil {
		resultText = *final.Result
	}
	return classifyEnvelopeFields(resultText, final.APIErrorStatus)
}

// makeProgressDispatcher returns a per-event handler that translates raw
// stream events into agent.GenerationProgress callbacks. PhaseDone is
// emitted by GenerateTextStreaming after cmd.Wait, because it needs data
// from the parsed final envelope.
func makeProgressDispatcher(progress agent.ProgressFn) func(streamEvent) {
	if progress == nil {
		return func(streamEvent) {}
	}
	// Accumulate raw character count; compute the token estimate from the
	// running total. Per-delta `len(text)/4` would truncate to 0 for tiny
	// deltas (single-character or single-token streaming) and the UI would
	// stay at "~0 tokens" until a chunky delta arrived.
	var totalChars int
	return func(ev streamEvent) {
		switch {
		case ev.Type == streamEventTypeSystem && ev.Subtype == "status" && ev.Status == "requesting":
			progress(agent.GenerationProgress{Phase: agent.PhaseConnecting})
		case ev.Type == streamEventTypeStreamEvent && ev.Event.Type == "message_start":
			p := agent.GenerationProgress{Phase: agent.PhaseFirstToken, TTFTms: ev.TTFTms}
			if ev.Event.Message != nil && ev.Event.Message.Usage != nil {
				p.InputTokens = ev.Event.Message.Usage.InputTokens
				p.CachedInputTokens = ev.Event.Message.Usage.CacheReadInputTokens
			}
			progress(p)
		case ev.Type == streamEventTypeStreamEvent && ev.Event.Type == "content_block_delta" && ev.Event.Delta != nil:
			text := ev.Event.Delta.Text
			if text == "" {
				text = ev.Event.Delta.Thinking
			}
			totalChars += len(text)
			progress(agent.GenerationProgress{Phase: agent.PhaseGenerating, OutputTokens: totalChars / 4})
		}
	}
}

func outputTokensFromUsage(u *messageUsage) int {
	if u == nil {
		return 0
	}
	return u.OutputTokens
}

// isContextKill reports whether waitErr looks like exec.CommandContext's kill
// triggered by ctx being done (signal termination while ctx.Err() is set), as
// opposed to the CLI exiting on its own. On Windows a context kill reports a
// normal exit code, so this returns false there and real process failures keep
// their precedence over a decoded result.
func isContextKill(ctx context.Context, waitErr error) bool {
	if ctx.Err() == nil {
		return false
	}
	var exitErr *exec.ExitError
	return errors.As(waitErr, &exitErr) && exitErr.ProcessState != nil && !exitErr.Exited()
}

// countingReader passes reads through and counts bytes seen, so the timeout
// diagnostic can report how much stdout the subprocess produced before dying.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err //nolint:wrapcheck // pass-through reader: wrapping would break io.EOF detection in the scanner
}

// looksLikeUnrecognizedFlag returns true if stderr indicates the CLI
// rejected one of the streaming-specific flags (older Claude CLI). Requires
// both a rejection phrase AND a streaming flag name to avoid false-positives
// on unrelated errors that happen to contain "unknown option".
func looksLikeUnrecognizedFlag(stderr string) bool {
	lower := strings.ToLower(stderr)
	hasRejectPhrase := strings.Contains(lower, "unrecognized option") ||
		strings.Contains(lower, "unknown flag") ||
		strings.Contains(lower, "unknown option") ||
		strings.Contains(lower, "invalid option")
	if !hasRejectPhrase {
		return false
	}
	return strings.Contains(lower, "stream-json") ||
		strings.Contains(lower, "verbose") ||
		strings.Contains(lower, "include-partial")
}
