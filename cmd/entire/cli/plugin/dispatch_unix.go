//go:build unix

package plugin

import (
	"context"
	"os/exec"
)

// buildExecCommand returns an *exec.Cmd that runs the plugin directly. The
// shebang of script plugins is honored by the OS.
func buildExecCommand(ctx context.Context, p *Plugin, args []string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, p.ExecPath, args...), nil
}
