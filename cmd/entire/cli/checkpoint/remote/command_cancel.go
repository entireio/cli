package remote

import (
	"os/exec"

	"github.com/entireio/cli/cmd/entire/cli/execx"
)

// terminateOnCancel ensures the subprocess and any transport-helper descendants
// die when ctx is cancelled. See execx.TerminateOnCancel.
func terminateOnCancel(cmd *exec.Cmd) {
	execx.TerminateOnCancel(cmd)
}
