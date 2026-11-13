//go:build windows

package execx

import (
	"bufio"
	"context"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// Windows ignores CREATE_NO_WINDOW once DETACHED_PROCESS is set, so each helper
// must set exactly its own console flag; routing one through the other would
// silently undo it.
func TestWindowsDetachHelpersSetDistinctConsoleFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		detach func(*exec.Cmd)
		want   uint32
	}{
		{"detachFromTTY", detachFromTTY, windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS},
		{"detachBackgroundWorker", detachBackgroundWorker, windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd := exec.CommandContext(t.Context(), "entire")
			tt.detach(cmd)
			if cmd.SysProcAttr == nil {
				t.Fatal("SysProcAttr = nil; want creation flags set")
			}
			if got := cmd.SysProcAttr.CreationFlags; got != tt.want {
				t.Fatalf("CreationFlags = %#x; want %#x", got, tt.want)
			}
		})
	}
}

func TestTerminateOnCancel_SetsWaitDelayAndCancelWindows(t *testing.T) {
	t.Parallel()
	cmd := exec.CommandContext(context.Background(), "cmd.exe", "/c", "exit 0")
	TerminateOnCancel(cmd)

	if cmd.WaitDelay != KillWaitDelay {
		t.Errorf("WaitDelay = %v; want %v", cmd.WaitDelay, KillWaitDelay)
	}
	if cmd.Cancel == nil {
		t.Error("Cancel = nil; want process tree kill handler")
	}
}

// End-to-end: a backgrounded grandchild inherits stdout, so cancelling the parent
// must kill the whole tree — otherwise the read blocks for the full sleep.
func TestTerminateOnCancel_KillsProcessTreeWindows(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, "cmd.exe", "/c", "start /b ping -n 60 127.0.0.1 & echo ready")
	TerminateOnCancel(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	r := bufio.NewReader(stdout)
	if line, err := r.ReadString('\n'); err != nil || !strings.Contains(line, "ready") {
		t.Fatalf("did not observe ready marker: line=%q err=%v", line, err)
	}

	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, r)
		_ = cmd.Wait()
		close(done)
	}()

	cancel()

	// Deadline must stay strictly below KillWaitDelay: that backstop would force
	// the pipe closed on its own, letting this test pass even if tree-kill regressed.
	select {
	case <-done:
	case <-time.After(KillWaitDelay / 2):
		t.Fatal("read did not return after cancellation; grandchild process tree outlived the kill")
	}
}
