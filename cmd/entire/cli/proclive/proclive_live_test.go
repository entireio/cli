//go:build linux || darwin

package proclive

import (
	"os"
	"os/exec"
	"testing"
)

// startSleeper spawns a real long-lived child bound to the test context (so it
// is killed when the test ends) and returns its PID and a captured Identity.
func startSleeper(t *testing.T) (int, Identity) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		// The context kill (on test end) signals the process; reap it here to
		// avoid a zombie. A non-nil "signal: killed" error is expected.
		if err := cmd.Wait(); err != nil {
			t.Logf("sleeper wait: %v", err)
		}
	})
	pid := cmd.Process.Pid
	_, name, start, err := procStat(pid)
	if err != nil {
		t.Fatalf("procStat(child %d): %v", pid, err)
	}
	return pid, Identity{PID: pid, Start: start, Name: name}
}

func TestCheck_LiveProcessIsAlive(t *testing.T) {
	t.Parallel()
	_, id := startSleeper(t)
	if got := Check(id); got != LivenessAlive {
		t.Errorf("Check(live) = %v, want alive", got)
	}
}

func TestCheck_ExitedProcessIsDead(t *testing.T) {
	t.Parallel()
	cmd := exec.CommandContext(t.Context(), "sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	_, name, start, err := procStat(pid)
	if err != nil {
		t.Fatalf("procStat(child %d): %v", pid, err)
	}
	id := Identity{PID: pid, Start: start, Name: name}

	// Kill and reap, then the recorded identity must read as dead. (A PID reused
	// within the test window would mismatch Start and still be Dead.)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Logf("wait after kill: %v", err) // expected: "signal: killed"
	}

	if got := Check(id); got != LivenessDead {
		t.Errorf("Check(exited) = %v, want dead", got)
	}
}

func TestCheck_StartMismatchIsDead(t *testing.T) {
	t.Parallel()
	// Our own process is alive, but a bogus start fingerprint must read as PID
	// reuse → Dead.
	id := Identity{PID: os.Getpid(), Start: "0.000000-not-a-real-fingerprint"}
	if got := Check(id); got != LivenessDead {
		t.Errorf("Check(start mismatch) = %v, want dead", got)
	}
}

func TestProcStat_Self(t *testing.T) {
	t.Parallel()
	ppid, name, start, err := procStat(os.Getpid())
	if err != nil {
		t.Fatalf("procStat(self): %v", err)
	}
	if ppid <= 0 {
		t.Errorf("ppid = %d, want > 0", ppid)
	}
	if name == "" {
		t.Errorf("name is empty")
	}
	if start == "" {
		t.Errorf("start is empty")
	}
}

func TestResolveOwner_ReturnsSomething(t *testing.T) {
	t.Parallel()
	// Under `go test` the ancestor chain (test binary ← go ← shell ← ...) should
	// resolve to some non-shell owner. We can't assert which, but if it resolves
	// it must be self-consistent and currently alive.
	id, ok := ResolveOwner()
	if !ok {
		t.Skip("no stable owner resolved in this environment")
	}
	if id.PID <= 0 {
		t.Errorf("resolved PID = %d, want > 0", id.PID)
	}
	if got := Check(id); got != LivenessAlive {
		t.Errorf("resolved owner Check = %v, want alive", got)
	}
}
