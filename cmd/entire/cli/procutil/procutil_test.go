package procutil

import (
	"context"
	"os/exec"
	"testing"
)

func TestTerminateOnCancel_SetsWaitDelayAndCancel(t *testing.T) {
	t.Parallel()
	cmd := exec.CommandContext(context.Background(), "entire")
	TerminateOnCancel(cmd)
	if cmd.WaitDelay != terminateWaitDelay {
		t.Errorf("WaitDelay = %v; want %v", cmd.WaitDelay, terminateWaitDelay)
	}
	if cmd.Cancel == nil {
		t.Error("Cancel = nil; want the process-tree kill")
	}
}
