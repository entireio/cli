package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// NewForegroundCommand builds an exec.Cmd wired to the caller's terminal.
// Agent launchers use this for commands the user should interact with directly.
func NewForegroundCommand(_ context.Context, binary string, args ...string) (*exec.Cmd, error) {
	bin, err := exec.LookPath(binary)
	if err != nil {
		return nil, fmt.Errorf("%s binary not on PATH: %w", binary, err)
	}
	// Foreground agents are interactive terminal processes that handle SIGINT
	// themselves. Binding them to the root command context would let
	// exec.CommandContext SIGKILL them on Ctrl+C before they restore terminal
	// state or persist session data.
	cmd := exec.CommandContext(context.Background(), bin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	return cmd, nil
}
