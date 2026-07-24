package claudecode

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// GenerateText sends a prompt to the Claude CLI and returns the raw text response.
// Implements the agent.TextGenerator interface.
//
// Model defaults to "haiku" for fast, cheap generation (the summarize package
// overrides to "sonnet" via ResolveModel for quality).
//
// Classification order:
//  1. A cleanly-parsed is_error:true envelope on stdout — checked first
//     because Claude's primary failure mode is exit 0 with is_error:true. This
//     wins over stderr and over ctx sentinels.
//  2. Context sentinels (ctx canceled/deadline) — passthrough, not typed.
//  3. CLIMissing — typed error for "install the binary" remediation.
//  4. Any other run error — stderr classified by HTTP status, then by auth
//     phrase. Reached even when stdout held non-JSON bytes: unparseable stdout
//     must not mask a real error on stderr (see classifyClaudeEnvelope).
//  5. Exit 0 with empty stdout — typed Unknown with "empty output" message.
//
// Exit 0 with unparseable stdout is handled inside step 1, which is the only
// caller that can distinguish it from a failed run.
func (c *ClaudeCodeAgent) GenerateText(ctx context.Context, prompt string, model string) (string, error) {
	if model == "" {
		model = "haiku"
	}
	args := []string{
		"--print", "--output-format", "json",
		"--model", model, "--setting-sources", "",
	}
	res, runErr := agent.RunIsolatedTextGeneratorCLIRaw(ctx, c.CommandRunner, "claude", args, prompt)

	if env := classifyClaudeEnvelope(res.Stdout, runErr); env != nil {
		env.ExitCode = res.ExitCode
		env.Cause = runErr
		return "", env
	}

	if runErr != nil {
		if errors.Is(runErr, context.Canceled) {
			return "", context.Canceled
		}
		if errors.Is(runErr, context.DeadlineExceeded) {
			return "", context.DeadlineExceeded
		}
		if agent.IsExecNotFoundErr(runErr) {
			return "", &agent.TextGenError{
				Kind:     agent.TextGenErrorCLIMissing,
				Provider: agent.AgentNameClaudeCode,
				Cause:    runErr,
			}
		}
		// Prefer stderr, fall back to stdout, then to the run error, so Message
		// is never empty — a launch failure (permission denied, exec format
		// error) produces no output and only runErr describes it. Classify
		// against the FULL text and truncate only for display, so a status line
		// or auth phrase past the 500-byte cap is still seen.
		raw := strings.TrimSpace(string(res.Stderr))
		if raw == "" {
			raw = strings.TrimSpace(string(res.Stdout))
		}
		if raw == "" {
			raw = runErr.Error()
		}
		kind := agent.ClassifyStderrHTTPStatus(raw)
		if kind == agent.TextGenErrorUnknown && containsAuthPhrase(raw) {
			// Claude's CLI sometimes exits non-zero with auth failure text on
			// stderr before any envelope is produced (e.g. "Invalid API key"
			// with exit 2). Reuses containsAuthPhrase/envelopeAuthPhrases from
			// envelope_parser.go — one list, two call sites (envelope result
			// text and raw stderr).
			kind = agent.TextGenErrorAuth
		}
		return "", &agent.TextGenError{
			Kind:     kind,
			Provider: agent.AgentNameClaudeCode,
			Message:  agent.TruncateStderr(raw),
			ExitCode: res.ExitCode,
			Cause:    runErr,
		}
	}

	// Success path. Envelope was nil (stdout empty) or envelope.IsError was false.
	if len(res.Stdout) == 0 {
		return "", &agent.TextGenError{
			Kind:     agent.TextGenErrorUnknown,
			Provider: agent.AgentNameClaudeCode,
			Message:  "claude CLI returned empty output",
		}
	}
	result, _, parseErr := parseGenerateTextResponse(res.Stdout)
	if parseErr != nil {
		return "", &agent.TextGenError{
			Kind:     agent.TextGenErrorUnknown,
			Provider: agent.AgentNameClaudeCode,
			Message:  fmt.Sprintf("unexpected parse failure on success path: %v", parseErr),
			Cause:    parseErr,
		}
	}
	return result, nil
}
