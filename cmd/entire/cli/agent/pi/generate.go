package pi

import (
	"context"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// GenerateText sends a prompt to Pi in non-interactive text mode and returns
// the raw response.
//
// The prompt is piped via stdin, which Pi reads in --print mode when no
// message is given on the command line. It carries condensed transcript
// content, and argv is readable by other local users through the process
// table (ps, /proc/<pid>/cmdline) for the life of the run.
func (a *PiAgent) GenerateText(ctx context.Context, prompt string, model string) (string, error) {
	args := []string{"--print", "--no-tools", "--no-session"}
	if model != "" {
		args = append(args, "--model", model)
	}

	result, capturedStderr, stdoutBytes, err := agent.RunIsolatedTextGeneratorCLI(ctx, a.CommandRunner, "pi", "pi", args, prompt)
	if err != nil {
		return "", &agent.TextGenerationError{
			Err:         fmt.Errorf("pi text generation failed: %w", err),
			Stderr:      capturedStderr,
			StdoutBytes: stdoutBytes,
		}
	}
	return result, nil
}
