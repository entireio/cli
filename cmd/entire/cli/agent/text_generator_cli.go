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

// TextCommandRunner matches exec.CommandContext and allows tests to inject a runner.
type TextCommandRunner func(ctx context.Context, name string, args ...string) *exec.Cmd

// RunIsolatedTextGeneratorCLI executes a text-generation CLI in an isolated temp
// directory with all GIT_* environment variables removed. This avoids recursive
// hook triggers and repo side effects while preserving provider-specific flags.
func RunIsolatedTextGeneratorCLI(ctx context.Context, runner TextCommandRunner, binary, displayName string, args []string, stdin string) (string, error) {
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
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", context.DeadlineExceeded
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return "", context.Canceled
		}
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			return "", fmt.Errorf("%s CLI not found: %w", displayName, err)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			detail := strings.TrimSpace(stderr.String())
			if detail == "" {
				detail = strings.TrimSpace(stdout.String())
			}
			if detail == "" {
				detail = err.Error()
			}
			return "", fmt.Errorf("%s CLI failed (exit %d): %s: %w", displayName, exitErr.ExitCode(), detail, err)
		}
		return "", fmt.Errorf("failed to run %s CLI: %w", displayName, err)
	}

	result := strings.TrimSpace(stdout.String())
	if result == "" {
		return "", fmt.Errorf("%s CLI returned empty output", displayName)
	}
	return result, nil
}

// summaryProviderBinaries maps agent names to the CLI binary that
// RunIsolatedTextGeneratorCLI will exec. Used by IsSummaryCLIAvailable to
// check PATH instead of repo-level DetectPresence, because a repo can use
// one agent for development while a different agent generates summaries.
var summaryProviderBinaries = map[types.AgentName]string{
	AgentNameClaudeCode: "claude",
	AgentNameCodex:      "codex",
	AgentNameCopilotCLI: "copilot",
	AgentNameCursor:     "agent",
	AgentNameGemini:     "gemini",
}

// IsSummaryCLIAvailable reports whether the CLI binary for a summary-capable
// agent is on PATH. This is distinct from DetectPresence, which checks
// repo-level agent configuration — a repo configured with Claude Code for
// development can still use Codex or Gemini for summary generation as long
// as the binary is installed.
func IsSummaryCLIAvailable(name types.AgentName) bool {
	binary, ok := summaryProviderBinaries[name]
	if !ok {
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
