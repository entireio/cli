package remote

import (
	"os/exec"
	"time"
)

// killWaitDelay bounds the wait after ctx-cancel: a transport-helper grandchild
// (e.g. git-remote-entire) can keep the output pipe open after `git` is SIGKILLed,
// otherwise blocking CombinedOutput indefinitely.
const killWaitDelay = 10 * time.Second

// terminateOnCancel ensures the subprocess and any transport-helper descendants
// die when ctx is cancelled.
func terminateOnCancel(cmd *exec.Cmd) {
	cmd.WaitDelay = killWaitDelay
	killProcessGroupOnCancel(cmd)
}
