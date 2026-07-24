package copilotcli

import (
	"context"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// GenerateText sends a prompt to the Copilot CLI and returns the raw text response.
//
// The prompt is piped via stdin rather than -p to avoid argv size limits.
// --allow-all-tools is required for non-interactive mode; --disable-builtin-mcps
// skips loading the GitHub MCP server that isn't needed for summary text
// generation and reduces per-call input tokens.
func (c *CopilotCLIAgent) GenerateText(ctx context.Context, prompt string, model string) (string, error) {
	args := []string{"--allow-all-tools", "--disable-builtin-mcps"}
	if model != "" {
		args = append(args, "--model", model)
	}
	res, runErr := agent.RunIsolatedTextGeneratorCLIRaw(ctx, c.CommandRunner, "copilot", args, prompt)
	return agent.HandleTextGenResult(res, runErr, agent.AgentNameCopilotCLI, "copilot CLI returned empty output", nil) //nolint:wrapcheck // return unwrapped: the explain layer renders label+message from the typed error, so a wrap prefix would leak into user output. errors.As (*TextGenError) / errors.Is (ctx sentinel) must reach it unflattened.
}
