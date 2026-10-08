// Package procutil holds helpers for cancelling spawned subprocesses.
package procutil

import (
	"os/exec"
	"time"
)

// terminateWaitDelay backstops Wait/Run after ctx-cancel so cancellation is
// bounded even if a descendant keeps an output pipe open.
const terminateWaitDelay = 5 * time.Second

// TerminateOnCancel configures cmd so cancellation cannot leave Wait/Run
// blocked forever on inherited output pipes. Call after building cmd, before
// Start/Run.
//
// On Unix, cmd starts in a new process group and context cancellation SIGKILLs
// the whole group, so agent grandchildren (sandbox helpers, MCP servers, etc.)
// die instead of holding stdout/stderr pipes open. On Windows, process-tree
// killing is not implemented here; only WaitDelay bounds how long Wait/Run can
// remain blocked after cancellation.
func TerminateOnCancel(cmd *exec.Cmd) {
	cmd.WaitDelay = terminateWaitDelay
	killProcessGroupOnCancel(cmd)
}
