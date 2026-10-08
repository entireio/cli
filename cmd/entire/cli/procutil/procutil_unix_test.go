//go:build unix

package procutil

import (
	"bufio"
	"context"
	"os/exec"
	"testing"
	"time"
)

// A grandchild that inherits stdout keeps the pipe open after the parent exits.
// Without TerminateOnCancel, draining stdout to EOF blocks until the grandchild
// dies on its own (here ~60s). The group-kill on cancel must unblock it fast.
func TestTerminateOnCancel_UnblocksGrandchildHoldingPipe(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Parent prints "ready", then exits, leaving a backgrounded sleep that
	// inherited the stdout pipe.
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 60 & echo ready")
	TerminateOnCancel(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	r := bufio.NewReader(stdout)
	if _, err := r.ReadString('\n'); err != nil {
		t.Fatalf("read ready line: %v", err)
	}

	// Drain to EOF in the background, mirroring how the reviewer reads Events.
	drained := make(chan struct{})
	go func() {
		// Blocks on the open pipe until cancel closes it; the error on close is
		// expected and the unblock itself is the assertion.
		_, _ = r.ReadString('\n') //nolint:errcheck // unblock is the assertion
		close(drained)
	}()

	cancel()

	select {
	case <-drained:
	case <-time.After(15 * time.Second):
		t.Fatal("stdout drain did not unblock after cancel — Ctrl+C would hang")
	}

	_ = cmd.Wait() //nolint:errcheck // process was killed; exit error expected
}
