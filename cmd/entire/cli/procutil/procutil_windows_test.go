//go:build windows

package procutil

import (
	"bufio"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// A grandchild that inherits stdout keeps the pipe open after the parent exits.
// Without TerminateOnCancel, draining stdout to EOF blocks until the grandchild
// dies on its own (here ~60s). The tree-kill on cancel must unblock it fast.
func TestTerminateOnCancel_UnblocksGrandchildHoldingPipeWindows(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Parent prints "ready", then exits, leaving a backgrounded ping that
	// inherited the stdout pipe.
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
		t.Fatalf("read ready line: line=%q err=%v", line, err)
	}

	drained := make(chan struct{})
	go func() {
		_, _ = r.ReadString('\n')
		close(drained)
	}()

	cancel()

	select {
	case <-drained:
	case <-time.After(terminateWaitDelay / 2):
		t.Fatal("stdout drain did not unblock after cancel; grandchild process tree outlived the kill")
	}

	_ = cmd.Wait()
}
