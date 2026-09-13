package remote

import (
	"context"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
)

// Not parallel: uses t.Setenv. Clearing ENTIRE_CHECKPOINT_TOKEN keeps the test
// hermetic — otherwise newCommand spawns git against the ambient repo.
func TestNewCommand_TerminatesOnCancel(t *testing.T) {
	t.Setenv(CheckpointTokenEnvVar, "")

	cmd := newCommand(context.Background(), "push", "origin", "main")

	if cmd.WaitDelay != execx.KillWaitDelay {
		t.Errorf("WaitDelay = %v; want %v", cmd.WaitDelay, execx.KillWaitDelay)
	}
	if cmd.Cancel == nil {
		t.Error("Cancel = nil; want a cancellation handler that terminates the process")
	}
}
