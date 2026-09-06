//go:build unix

package execx

import (
	"bufio"
	"context"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestNonInteractive_SetsSetsidUnix(t *testing.T) {
	t.Parallel()
	cmd := NonInteractive(context.Background(), "/bin/true")
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Errorf("NonInteractive: Setsid = false; want true")
	}
}

func TestTerminateOnCancel_SetsWaitDelayAndGroupKill(t *testing.T) {
	t.Parallel()
	cmd := exec.CommandContext(context.Background(), "/bin/true")
	TerminateOnCancel(cmd)

	if cmd.WaitDelay != KillWaitDelay {
		t.Errorf("WaitDelay = %v; want %v", cmd.WaitDelay, KillWaitDelay)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Error("Setpgid = false; want true so the whole process group can be killed")
	}
	if cmd.Cancel == nil {
		t.Error("Cancel = nil; want a group-kill handler")
	}
}

// hangCmdArgs backgrounds a grandchild that holds stdout; "ready" prints after
// it backgrounds so callers cancel strictly after it exists (no race). `wait`
// keeps the shell alive as group leader so only a group-wide kill ends both.
var hangCmdArgs = []string{"/bin/sh", "-c", "sleep 60 & echo ready; wait"}

// assertGroupKillReleasesPipe starts cmd, waits until the grandchild holds
// stdout, then cancels and requires the pipe to close promptly. Its deadline
// stays strictly below KillWaitDelay: that backstop force-closes the pipes on
// its own, so a looser bound would pass even if the group-kill regressed.
func assertGroupKillReleasesPipe(t *testing.T, cmd *exec.Cmd, cancel context.CancelFunc) {
	t.Helper()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	br := bufio.NewReader(stdout)
	if line, err := br.ReadString('\n'); err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("did not observe ready marker: line=%q err=%v", line, err)
	}

	done := make(chan error, 1)
	go func() {
		_, _ = io.Copy(io.Discard, br) //nolint:errcheck // draining to block until the pipe closes; copy errors are irrelevant
		done <- cmd.Wait()
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(KillWaitDelay / 2):
		t.Fatal("read did not return after cancellation; the pipe-holding grandchild outlived the group-kill")
	}
}

// End-to-end: a backgrounded grandchild inherits stdout, so cancelling the parent
// must kill the whole group — otherwise the read blocks for the full sleep.
func TestTerminateOnCancel_KillsProcessGroup(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, hangCmdArgs[0], hangCmdArgs[1:]...)
	TerminateOnCancel(cmd)

	assertGroupKillReleasesPipe(t, cmd, cancel)
}

// Regression guard: TerminateOnCancel must skip Setpgid on a NonInteractive cmd
// (see killProcessGroupOnCancel for the EPERM reason) and still reach the
// grandchild, since the setsid child is already its own process-group leader.
func TestTerminateOnCancel_ComposesWithNonInteractive(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := NonInteractive(ctx, hangCmdArgs[0], hangCmdArgs[1:]...)
	TerminateOnCancel(cmd)

	if cmd.SysProcAttr.Setpgid {
		t.Error("Setpgid = true alongside Setsid; setpgid on a session leader fails with EPERM")
	}

	assertGroupKillReleasesPipe(t, cmd, cancel)
}

// Cancel must return cleanly *and* actually terminate the leader.
func TestKillProcessGroupOnCancel_TerminatesLeader(t *testing.T) {
	t.Parallel()

	// exec requires CommandContext when Cancel is non-nil; we invoke Cancel directly.
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", "sleep 60 & wait")
	killProcessGroupOnCancel(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	if err := cmd.Cancel(); err != nil {
		t.Fatalf("Cancel returned %v; want nil", err)
	}

	// Bound the wait ourselves — no WaitDelay is set here, so a group-kill
	// regression would block on the full 60s sleep instead of failing fast.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err == nil {
			t.Error("Wait returned nil; the killed shell should report a termination error")
		}
	case <-time.After(KillWaitDelay):
		t.Fatal("Wait did not return after Cancel; the group-kill handler may have regressed")
	}
}
