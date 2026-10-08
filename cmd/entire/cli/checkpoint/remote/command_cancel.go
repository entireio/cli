package remote

import (
	"os/exec"

	"github.com/entireio/cli/cmd/entire/cli/execx"
)

// killWaitDelay is the wait bound applied after ctx-cancel. A transport-helper
// grandchild (e.g. git-remote-entire) can keep the output pipe open after `git`
// is SIGKILLed, otherwise blocking CombinedOutput indefinitely.
const killWaitDelay = execx.KillWaitDelay

// terminateOnCancel ensures the subprocess and any transport-helper descendants
// die when ctx is cancelled. See execx.TerminateOnCancel.
func terminateOnCancel(cmd *exec.Cmd) {
	execx.TerminateOnCancel(cmd)
}
