package geminicli

import (
	"context"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// GenerateText sends a prompt to the Gemini CLI and returns the raw text response.
//
// The prompt is piped to the Gemini CLI via stdin rather than embedded in argv.
// Per gemini --help, the -p/--prompt flag is appended to any input read from
// stdin; we pass a single-space placeholder to trigger headless (non-interactive)
// mode and let stdin carry the actual content, avoiding argv size limits.
func (g *GeminiCLIAgent) GenerateText(ctx context.Context, prompt string, model string) (string, error) {
	args := []string{"-p", " "}
	if model != "" {
		args = append(args, "--model", model)
	}

	result, err := agent.RunIsolatedTextGeneratorCLI(ctx, g.CommandRunner, "gemini", "gemini", args, prompt)
	if err != nil {
		return "", fmt.Errorf("gemini text generation failed: %w", err)
	}
	return result, nil
}
