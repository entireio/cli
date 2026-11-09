// Package procutil holds helpers for cancelling spawned subprocesses.
package procutil

import (
	"os/exec"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/execx"
)

// terminateWaitDelay backstops Wait/Run after ctx-cancel so cancellation is
// bounded even if a descendant keeps an output pipe open.
const terminateWaitDelay = 5 * time.Second

// TerminateOnCancel configures cmd so cancellation cannot leave Wait/Run
// blocked forever on inherited output pipes. Call after building cmd, before
// Start/Run.
//
// Context cancellation kills cmd's whole process tree (execx.KillTreeOnCancel),
// so agent grandchildren (sandbox helpers, MCP servers, etc.) die instead of
// holding stdout/stderr pipes open: on Unix through a new process group, on
// Windows by walking the process table. WaitDelay bounds Wait/Run for any
// descendant that escapes.
func TerminateOnCancel(cmd *exec.Cmd) {
	cmd.WaitDelay = terminateWaitDelay
	execx.KillTreeOnCancel(cmd)
}
