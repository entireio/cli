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
	"time"
)

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

// KillWaitDelay bounds how long Wait blocks after ctx-cancel before exec
// force-closes the subprocess I/O pipes. Without it, a descendant that inherited
// the output pipe (a sandbox or transport helper the direct child spawned) keeps
// the pipe open after the child is killed, so the stdout/stderr copy blocks
// forever and the context deadline is silently defeated.
const KillWaitDelay = 10 * time.Second

// TerminateOnCancel makes cmd and its descendants die when ctx is cancelled.
// exec.Cmd's default Cancel only kills the direct child, leaving a grandchild
// (e.g. a sandbox helper or transport helper) alive and holding the output pipe
// open, which blocks Wait/Run indefinitely past the deadline. A new process
// group lets Cancel SIGKILL the whole tree; WaitDelay is the backstop that
// force-closes the pipes if a descendant escapes the group.
//
// cmd must be created with exec.CommandContext so Cancel runs on ctx-done. Do
// not combine with NonInteractive on the same cmd: NonInteractive sets Setsid,
// which conflicts with the Setpgid this sets.
func TerminateOnCancel(cmd *exec.Cmd) {
	cmd.WaitDelay = KillWaitDelay
	killProcessGroupOnCancel(cmd)
}
