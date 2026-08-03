package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// TextGenerationError carries captured subprocess output alongside a
// TextGenerator's error so the explain layer can build a meaningful
// timeout diagnostic ("provider produced no output" vs "was generating
// output when killed"). Wraps the original error so errors.As against
// the inner type (e.g. *ClaudeError) keeps working.
type TextGenerationError struct {
	Err         error
	Stderr      string
	StdoutBytes int
}

func (e *TextGenerationError) Error() string { return e.Err.Error() }
func (e *TextGenerationError) Unwrap() error { return e.Err }

// TextCommandRunner matches exec.CommandContext and allows tests to inject a runner.
type TextCommandRunner func(ctx context.Context, name string, args ...string) *exec.Cmd

// RunIsolatedTextGeneratorCLI executes a text-generation CLI in an isolated temp
// directory with all GIT_* environment variables removed. This avoids recursive
// hook triggers and repo side effects while preserving provider-specific flags.
//
// Returns (result, capturedStderr, stdoutByteCount, err). capturedStderr and
// stdoutByteCount are populated even on error so callers can wrap them into a
// *agent.TextGenerationError for timeout diagnostics.
func RunIsolatedTextGeneratorCLI(ctx context.Context, runner TextCommandRunner, binary, displayName string, args []string, stdin string) (string, string, int, error) {
	if runner == nil {
		runner = exec.CommandContext
	}

	cmd := runner(ctx, binary, args...)
	cmd.Dir = os.TempDir()
	cmd.Env = StripGitEnv(os.Environ())
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		capturedStderr := strings.TrimSpace(stderr.String())
		stdoutBytes := stdout.Len()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", capturedStderr, stdoutBytes, context.DeadlineExceeded
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return "", capturedStderr, stdoutBytes, context.Canceled
		}
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			return "", capturedStderr, stdoutBytes, fmt.Errorf("%s CLI not found: %w", displayName, err)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			detail := capturedStderr
			if detail == "" {
				detail = strings.TrimSpace(stdout.String())
			}
			if detail == "" {
				detail = err.Error()
			}
			return "", capturedStderr, stdoutBytes, fmt.Errorf("%s CLI failed (exit %d): %s: %w", displayName, exitErr.ExitCode(), detail, err)
		}
		return "", capturedStderr, stdoutBytes, fmt.Errorf("failed to run %s CLI: %w", displayName, err)
	}

	result := strings.TrimSpace(stdout.String())
	if result == "" {
		return "", "", 0, fmt.Errorf("%s CLI returned empty output", displayName)
	}
	return result, "", stdout.Len(), nil
}

// summaryProviderBinaries maps agent names to the CLI binary that
// RunIsolatedTextGeneratorCLI will exec. Used by IsSummaryCLIAvailable to
// check PATH instead of repo-level DetectPresence, because a repo can use
// one agent for development while a different agent generates summaries.
//
// This is the single source of truth for summary-capable provider binaries.
// Callers outside this package that need the binary name (e.g., the explain
// diagnostic's "run `claude` directly" suggestion) should use
// SummaryCLIBinaryName rather than duplicating the mapping.
var summaryProviderBinaries = map[types.AgentName]string{
	AgentNameClaudeCode: "claude",
	AgentNameCodex:      "codex",
	AgentNameCopilotCLI: "copilot",
	AgentNameCursor:     "agent",
	AgentNameGemini:     "gemini",
	AgentNamePi:         "pi",
}

// SummaryCLIBinaryName returns the CLI binary name for a summary-capable
// agent (e.g. "claude" for ClaudeCode, "agent" for Cursor). Returns "" for
// agents that are not summary-capable; callers should treat that as "we
// don't know" rather than guessing.
func SummaryCLIBinaryName(name types.AgentName) string {
	return summaryProviderBinaries[name]
}

// IsSummaryCLIAvailable reports whether the CLI binary for a summary-capable
// agent is on PATH. This is distinct from DetectPresence, which checks
// repo-level agent configuration — a repo configured with Claude Code for
// development can still use Codex or Gemini for summary generation as long
// as the binary is installed.
func IsSummaryCLIAvailable(name types.AgentName) bool {
	binary := SummaryCLIBinaryName(name)
	if binary == "" {
		return false
	}
	_, err := exec.LookPath(binary)
	return err == nil
}

func StripGitEnv(env []string) []string {
	filtered := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, "GIT_") {
			filtered = append(filtered, e)
		}
	}
	return filtered
}
