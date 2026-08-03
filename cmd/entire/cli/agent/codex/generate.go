package codex

import (
	"context"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// GenerateText sends a prompt to the Codex CLI and returns the raw text response.
func (c *CodexAgent) GenerateText(ctx context.Context, prompt string, model string) (string, error) {
	args := []string{"exec", "--skip-git-repo-check"}
	if model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, "-")

	result, capturedStderr, stdoutBytes, err := agent.RunIsolatedTextGeneratorCLI(ctx, c.CommandRunner, "codex", "codex", args, prompt)
	if err != nil {
		return "", &agent.TextGenerationError{
			Err:         fmt.Errorf("codex text generation failed: %w", err),
			Stderr:      capturedStderr,
			StdoutBytes: stdoutBytes,
		}
	}
	return result, nil
}
