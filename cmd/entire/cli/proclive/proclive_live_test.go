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

// Commit attribution asks a different question than liveness: not "is the
// recorded owner alive" but "is the recorded owner an ANCESTOR of the process
// asking". A commit hook whose ancestry contains a session's owner was spawned
// (however indirectly) by that session's agent, in whatever worktree — the
// identity-linking contract (PR #2013), served by the same Identity that
// captureSessionOwner already persists every turn start.
func TestHasAncestor_ParentMatches(t *testing.T) {
	parent, ok := IdentityOf(os.Getppid())
	if !ok {
		t.Fatal("IdentityOf(parent) should resolve on a supported platform")
	}
	if !HasAncestor(parent) {
		t.Fatalf("the test process's parent %+v must be reported as an ancestor", parent)
	}
}

func TestHasAncestor_RejectsRecycledPID(t *testing.T) {
	parent, ok := IdentityOf(os.Getppid())
	if !ok {
		t.Fatal("IdentityOf(parent) should resolve")
	}
	parent.Start += "-not-the-same-boot-instant"
	if HasAncestor(parent) {
		t.Fatal("same PID with a different start fingerprint is a recycled PID, not an ancestor")
	}
}

func TestHasAncestor_RejectsForeignHost(t *testing.T) {
	parent, ok := IdentityOf(os.Getppid())
	if !ok {
		t.Fatal("IdentityOf(parent) should resolve")
	}
	parent.Host += "-elsewhere"
	if HasAncestor(parent) {
		t.Fatal("a PID recorded on another host is meaningless here and must never match")
	}
}

func TestHasAncestor_NonAncestorDoesNotMatch(t *testing.T) {
	pid, _ := startSleeper(t)
	sibling, ok := IdentityOf(pid)
	if !ok {
		t.Fatal("IdentityOf(sleeper) should resolve")
	}
	if HasAncestor(sibling) {
		t.Fatal("a sibling child process is not an ancestor")
	}
}

func TestHasAncestor_EmptyIdentityNeverMatches(t *testing.T) {
	if HasAncestor(Identity{}) {
		t.Fatal("the zero identity must never match")
	}
}

func TestCurrentAncestry_DepthAndGuards(t *testing.T) {
	ancestry, ok := CurrentAncestry()
	if !ok {
		t.Fatal("CurrentAncestry should resolve on a supported platform")
	}
	parent, ok := IdentityOf(os.Getppid())
	if !ok {
		t.Fatal("IdentityOf(parent) should resolve")
	}
	if depth := ancestry.Depth(parent); depth != 0 {
		t.Fatalf("the parent must be the nearest ancestor (depth 0), got %d", depth)
	}
	recycled := parent
	recycled.Start += "-recycled"
	if depth := ancestry.Depth(recycled); depth != -1 {
		t.Fatalf("same PID with a different start fingerprint must not match, got depth %d", depth)
	}
	foreign := parent
	foreign.Host += "-elsewhere"
	if depth := ancestry.Depth(foreign); depth != -1 {
		t.Fatalf("an identity recorded on another host must not match, got depth %d", depth)
	}
	if chain := ancestry.Chain(); len(chain) == 0 || chain[0].PID != os.Getppid() {
		t.Fatalf("the chain must start at the parent, got %+v", chain)
	}
}
