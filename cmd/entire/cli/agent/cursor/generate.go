package cursor

import (
	"context"
	"fmt"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// GenerateText sends a prompt to the Cursor agent CLI and returns the raw text response.
//
// The prompt is piped via stdin rather than as a positional argument, avoiding
// argv size limits. --print triggers non-interactive mode; --force --trust
// are required for headless operation per cursor-agent --help.
func (c *CursorAgent) GenerateText(ctx context.Context, prompt string, model string) (string, error) {
	args := []string{"--print", "--force", "--trust", "--workspace", os.TempDir()}
	if model != "" {
		args = append(args, "--model", model)
	}

	result, err := agent.RunIsolatedTextGeneratorCLI(ctx, c.CommandRunner, "agent", "cursor", args, prompt)
	if err != nil {
		return "", fmt.Errorf("cursor text generation failed: %w", err)
	}
	return result, nil
}
