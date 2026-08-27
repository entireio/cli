//go:build unix

package remote

import (
	"context"
	"testing"
)

// Not parallel: uses t.Setenv. Clearing ENTIRE_CHECKPOINT_TOKEN keeps the test
// hermetic — otherwise newCommand spawns git against the ambient repo.
func TestKillProcessGroupOnCancel_SetsSetpgidAndCancel(t *testing.T) {
	t.Setenv(CheckpointTokenEnvVar, "")

	cmd := newCommand(context.Background(), "push", "origin", "main")

	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Error("Setpgid = false; want true so the whole process group can be killed")
	}
	if cmd.Cancel == nil {
		t.Fatal("Cancel = nil; want a group-kill handler")
	}
}
