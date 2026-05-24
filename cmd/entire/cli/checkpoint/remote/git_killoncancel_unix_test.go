//go:build unix

package remote

import (
	"context"
	"testing"
)

// newCommand must wire up KillOnCancel so that git push/fetch subprocesses
// don't strand grandchildren (git-remote-https, credential helpers) holding
// the inherited stdio pipes when their context fires. Without this,
// tryPushSessionsCommon's 2-minute timeout was effectively unbounded — the
// real-world symptom that motivated this guard.
func TestNewCommand_AppliesKillOnCancel(t *testing.T) {
	t.Parallel()

	cmd := newCommand(context.Background(), "status")

	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatalf("newCommand should set Setpgid for process-group kill; got %+v", cmd.SysProcAttr)
	}
	if cmd.Cancel == nil {
		t.Fatal("newCommand should set Cancel to kill the process group")
	}
	if cmd.WaitDelay <= 0 {
		t.Fatalf("newCommand should set WaitDelay; got %s", cmd.WaitDelay)
	}
}
