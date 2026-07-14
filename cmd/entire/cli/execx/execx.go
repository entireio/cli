// Package execx provides explicit helpers for spawning subprocesses with a
// chosen TTY attachment mode, replacing env-var signalling with real OS state.
//
// Use NonInteractive when the subprocess must not prompt (tests, automation,
// hooks that shouldn't block). Use Interactive when the subprocess should
// inherit the parent's controlling TTY (the default for exec.Command).
package execx

import (
	"context"
	"os/exec"
)

// Interactive returns an *exec.Cmd that inherits the parent's controlling TTY.
// Equivalent to exec.CommandContext; provided for symmetry and intent clarity.
func Interactive(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

// NonInteractive returns an *exec.Cmd detached from the parent's controlling
// TTY. In the child, /dev/tty cannot be opened, so
// interactive.CanPromptInteractively() returns false — no env var required.
//
// On Windows the child runs with DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
// so it has no inherited console.
func NonInteractive(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	detachFromTTY(cmd)
	return cmd
}
