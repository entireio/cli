package pi

import (
	"context"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// GenerateText sends a prompt to Pi in non-interactive text mode and returns
// the raw response. The prompt is passed as a positional message because Pi's
// CLI consumes prompts from argv in --print mode.
func (a *PiAgent) GenerateText(ctx context.Context, prompt string, model string) (string, error) {
	args := []string{"--print", "--no-tools", "--no-session"}
	if model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, prompt)

	result, err := agent.RunIsolatedTextGeneratorCLI(ctx, nil, "pi", "pi", args, "")
	if err != nil {
		return "", fmt.Errorf("pi text generation failed: %w", err)
	}
	return result, nil
}
