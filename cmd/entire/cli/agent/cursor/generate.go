package cursor

import (
	"context"
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
	res, runErr := agent.RunIsolatedTextGeneratorCLIRaw(ctx, c.CommandRunner, "agent", args, prompt)
	return agent.HandleTextGenResult(res, runErr, agent.AgentNameCursor, "cursor CLI returned empty output", nil) //nolint:wrapcheck // return unwrapped: the explain layer renders label+message from the typed error, so a wrap prefix would leak into user output. errors.As (*TextGenError) / errors.Is (ctx sentinel) must reach it unflattened.
}
