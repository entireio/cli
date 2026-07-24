package codex

import (
	"context"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// GenerateText sends a prompt to the Codex CLI and returns the raw text response.
func (c *CodexAgent) GenerateText(ctx context.Context, prompt, model string) (string, error) {
	args := []string{"exec", "--skip-git-repo-check"}
	if model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, "-")
	res, runErr := agent.RunIsolatedTextGeneratorCLIRaw(ctx, c.CommandRunner, "codex", args, prompt)
	return agent.HandleTextGenResult(res, runErr, agent.AgentNameCodex, "codex CLI returned empty output", nil) //nolint:wrapcheck // return unwrapped: the explain layer renders label+message from the typed error, so a wrap prefix would leak into user output. errors.As (*TextGenError) / errors.Is (ctx sentinel) must reach it unflattened.
}
