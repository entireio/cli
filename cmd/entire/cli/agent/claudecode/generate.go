package claudecode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// GenerateText sends a prompt to the Claude CLI and returns the raw text response.
// Implements the agent.TextGenerator interface.
// The model parameter hints which model to use (e.g., "haiku", "sonnet").
// If empty, defaults to "haiku" for fast, cheap generation.
//
// Unlike most agents, this implementation runs the subprocess directly rather
// than through agent.RunIsolatedTextGeneratorCLI. The shared helper collapses
// stderr + exit code into a single formatted error string, but Claude CLI
// returns operational errors (auth, rate limit, invalid model) as exit 0
// with is_error:true in the JSON envelope on stdout — so we need structured
// access to stdout, stderr and the exit code to produce the typed ClaudeError
// values that formatCheckpointSummaryError maps to actionable messages.
func (c *ClaudeCodeAgent) GenerateText(ctx context.Context, prompt string, model string) (string, error) {
	claudePath := "claude"
	if model == "" {
		model = "haiku"
	}

	commandRunner := c.CommandRunner
	if commandRunner == nil {
		commandRunner = exec.CommandContext
	}

	cmd := commandRunner(ctx, claudePath,
		"--print", "--output-format", "json",
		"--model", model, "--setting-sources", "")

	// Isolate from the user's git repo to prevent recursive hook triggers
	// and index pollution (matches agent.RunIsolatedTextGeneratorCLI behavior).
	cmd.Dir = os.TempDir()
	cmd.Env = agent.StripGitEnv(os.Environ())
	cmd.Stdin = strings.NewReader(prompt)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Prefer a structured Claude error over bare ctx sentinels: if the CLI
		// already emitted an is_error envelope on stdout, surface that even when
		// the context happens to be done. Otherwise the user loses actionable
		// auth/rate-limit/config diagnostics whenever ctx and the subprocess
		// both fail at roughly the same time.
		if _, env, parseErr := parseGenerateTextResponse(stdout.Bytes()); parseErr == nil && env != nil && env.IsError {
			result := ""
			if env.Result != nil {
				result = *env.Result
			}
			exitCode := 0
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				exitCode = exitErr.ExitCode()
			}
			return "", classifyEnvelopeError(result, env.APIErrorStatus, exitCode)
		}
		// No structured signal on stdout — ctx cancellation is next most
		// informative, since the rest is a guess.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", context.DeadlineExceeded
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return "", context.Canceled
		}
		if isExecNotFound(err) {
			return "", &ClaudeError{Kind: ClaudeErrorCLIMissing, Cause: err}
		}
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		return "", classifyStderrError(stderr.String(), exitCode)
	}

	// Exit 0: parse the response and check for is_error (Claude CLI returns
	// most operational errors as exit 0 with is_error:true in the envelope).
	result, env, err := parseGenerateTextResponse(stdout.Bytes())
	if err != nil {
		return "", &ClaudeError{Kind: ClaudeErrorUnknown, Message: fmt.Sprintf("failed to parse claude CLI response: %v", err), Cause: err}
	}
	if env != nil && env.IsError {
		return "", classifyEnvelopeError(result, env.APIErrorStatus, 0)
	}

	return result, nil
}

// isExecNotFound returns true only when err indicates the binary was not
// found on PATH or at the given absolute path. It intentionally excludes
// other *exec.Error causes (permission denied, invalid executable format),
// which should surface as a generic failure so operators aren't misdirected
// to a reinstall when the real problem is a broken/inaccessible binary.
func isExecNotFound(err error) bool {
	var execErr *exec.Error
	if errors.As(err, &execErr) && errors.Is(execErr.Err, exec.ErrNotFound) {
		return true
	}
	return errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist)
}
