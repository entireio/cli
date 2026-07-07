package antigravity

import (
	"context"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// GenerateText submits a non-interactive prompt to the Antigravity CLI. The
// binary is `agy`; -p is the short alias for --print (single-prompt mode).
//
// The prompt is piped via stdin with a single-space -p placeholder, mirroring
// the Gemini CLI convention (agy's predecessor): the placeholder triggers
// headless mode while stdin carries the actual content, avoiding argv size
// limits. agy's --help doesn't document the stdin behavior, so it was verified
// live (agy 1.0.16, 2026-07-07): a prompt piped to `agy -p " "` is what the
// model receives and answers — the space is not summarized in its place.
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
