package opencode

import (
	"context"
	"os/exec"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// openCodeExecutable returns the program to run for an OpenCode command: the
// binary agent.SummaryCLIPath resolves (the opencode on PATH, else
// ~/.opencode/bin/opencode), else the bare name, so the command fails with its
// own not-found error.
func openCodeExecutable() string {
	if path, ok := agent.SummaryCLIPath(agent.AgentNameOpenCode); ok {
		return path
	}
	return openCodeBinary
}

// openCodeCommand is the CommandRunner used when none is set. Runners are
// called with the program name; this one runs openCodeExecutable instead.
func openCodeCommand(ctx context.Context, _ string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, openCodeExecutable(), args...)
}
