package antigravity

import (
	"context"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// GenerateText submits a non-interactive prompt to the Antigravity CLI. The
// binary is `agy`; -p is the short alias for --print (single-prompt mode).
func (a *AntigravityAgent) GenerateText(ctx context.Context, prompt string, model string) (string, error) {
	args := []string{"-p", " "}
	if model != "" {
		args = append(args, "--model", model)
	}
	result, err := agent.RunIsolatedTextGeneratorCLI(ctx, a.CommandRunner, "agy", "antigravity", args, prompt)
	if err != nil {
		return "", fmt.Errorf("antigravity text generation failed: %w", err)
	}
	return result, nil
}
