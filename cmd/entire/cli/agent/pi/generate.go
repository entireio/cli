package pi

import (
	"context"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// GenerateText sends a prompt to Pi in non-interactive text mode and returns
// the raw response. The prompt is passed as a positional message because Pi's
// CLI consumes prompts from argv in --print mode.
//
// No extra stderr classifier: no captured Pi failure fixtures exist yet, so
// classification stays on the shared HTTP-status baseline with Pi's stderr
// surfaced verbatim.
func (a *PiAgent) GenerateText(ctx context.Context, prompt string, model string) (string, error) {
	args := []string{"--print", "--no-tools", "--no-session"}
	if model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, prompt)

	res, runErr := agent.RunIsolatedTextGeneratorCLIRaw(ctx, a.CommandRunner, "pi", args, "")
	return agent.HandleTextGenResult(res, runErr, agent.AgentNamePi, "pi CLI returned empty output", nil) //nolint:wrapcheck // return unwrapped: the explain layer renders label+message from the typed error, so a wrap prefix would leak into user output. errors.As (*TextGenError) / errors.Is (ctx sentinel) must reach it unflattened.
}
