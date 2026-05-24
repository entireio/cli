//go:build unix

package execx

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// grandchildHoldingStdoutCmd returns a shell snippet that backgrounds a sleep
// (the grandchild) before waiting indefinitely in the parent. The grandchild
// inherits the parent's stdout, so any pipe-reading caller blocks on it long
// after the parent has been killed. The sleep is short enough that the
// regression-pinning test costs ~3s rather than 5s on every CI run while
// still leaving a clear gap above the 2s assertion threshold.
const grandchildHoldingStdoutCmd = "sleep 3 & echo started; wait"

// Regression: exec.CommandContext alone does not unblock CombinedOutput when
// the killed child has grandchildren still holding the inherited stdout pipe.
// This test pins that behavior so reviewers can see why KillOnCancel exists.
// If a future Go release fixes this in the stdlib, this assertion fails and
// KillOnCancel can be reconsidered.
func TestExecCommandContext_HangsOnGrandchildHoldingPipe(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", grandchildHoldingStdoutCmd)

	start := time.Now()
	// We expect CombinedOutput to return with an error (killed by ctx) — the
	// point of the test is the duration, so the error is intentionally ignored.
	_, _ = cmd.CombinedOutput() //nolint:errcheck // duration is what we assert on
	elapsed := time.Since(start)

	if elapsed < 2*time.Second {
		t.Fatalf("expected CombinedOutput to hang on grandchild stdout; got elapsed=%s (Go may have fixed this — consider removing KillOnCancel)", elapsed)
	}
}

// KillOnCancel must terminate the whole process group and release the pipes
// shortly after the context fires, even when grandchildren hold stdout.
func TestKillOnCancel_TerminatesGrandchildHoldingPipe(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", grandchildHoldingStdoutCmd)
	KillOnCancel(cmd, 500*time.Millisecond)

	start := time.Now()
	_, err := cmd.CombinedOutput()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from cancelled command")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("KillOnCancel should unblock CombinedOutput well before the grandchild exits; got elapsed=%s", elapsed)
	}
}

func TestKillOnCancel_SetsSetpgid(t *testing.T) {
	t.Parallel()
	cmd := exec.CommandContext(context.Background(), "/bin/true")
	KillOnCancel(cmd, time.Second)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatalf("KillOnCancel did not set Setpgid: %+v", cmd.SysProcAttr)
	}
	if cmd.WaitDelay != time.Second {
		t.Fatalf("KillOnCancel WaitDelay = %s; want 1s", cmd.WaitDelay)
	}
	if cmd.Cancel == nil {
		t.Fatal("KillOnCancel did not set Cancel")
	}
}
