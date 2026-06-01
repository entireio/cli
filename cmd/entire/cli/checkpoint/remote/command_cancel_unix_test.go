//go:build unix

package remote

import (
	"bufio"
	"context"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
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

// End-to-end: a backgrounded grandchild inherits stdout, so cancelling the parent
// must kill the whole group — otherwise the read blocks for the full sleep.
func TestTerminateOnCancel_KillsProcessGroup(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// `sleep 60 &` backgrounds a grandchild that holds stdout; "ready" prints after
	// it backgrounds so we cancel strictly after it exists (no race). `wait` keeps
	// the shell alive as group leader so only a group-wide kill ends both.
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 60 & echo ready; wait")
	terminateOnCancel(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Wait for "ready" so the grandchild is up and holding the pipe.
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

	// Deadline must stay strictly below killWaitDelay: that backstop would force
	// the pipe closed on its own, letting this test pass even if group-kill regressed.
	select {
	case <-done:
	case <-time.After(killWaitDelay / 2):
		t.Fatal("read did not return after cancellation; the pipe-holding grandchild outlived the group-kill")
	}
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
	case <-time.After(killWaitDelay):
		t.Fatal("Wait did not return after Cancel; the group-kill handler may have regressed")
	}
}
