package copilotcli

import (
	"context"
	"fmt"

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

	result, err := agent.RunIsolatedTextGeneratorCLI(ctx, c.CommandRunner, "copilot", "copilot", args, prompt)
	if err != nil {
		return "", fmt.Errorf("copilot text generation failed: %w", err)
	}
	return result, nil
}
