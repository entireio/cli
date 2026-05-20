package antigravity

import (
	"context"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// GenerateText submits prompt via stdin using the -p flag (mirrors `gemini -p`); flag correctness will be verified in Chunk 3 Task 9 against the real `antigravity --help` output.
func (a *AntigravityAgent) GenerateText(ctx context.Context, prompt string, model string) (string, error) {
	args := []string{"-p", " "}
	if model != "" {
		args = append(args, "--model", model)
	}
	result, err := agent.RunIsolatedTextGeneratorCLI(ctx, a.CommandRunner, "antigravity", "antigravity", args, prompt)
	if err != nil {
		return "", fmt.Errorf("antigravity text generation failed: %w", err)
	}
	return result, nil
}
