//go:build unix

package versioncheck

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// realRunInstaller shells out to the installer command, streaming stdin/stdout/stderr
// so password prompts and progress output reach the user.
func realRunInstaller(ctx context.Context, cmdStr string) error {
	c := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("installer exited: %w", err)
	}
	return nil
}
