//go:build windows

package proclive

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// helperRoleEnv tells a re-exec'd test binary to act as one level of a process
// tree. It holds the roles from this level down, comma-separated: "exit"
// starts the next level and exits, and "sleep" prints "ready <pid>" and sleeps.
const helperRoleEnv = "ENTIRE_PROCLIVE_HELPER_ROLE"

// TestProcliveHelperProcess is not a test: it is the process the Windows tests
// start, and does nothing when run directly.
func TestProcliveHelperProcess(t *testing.T) {
	t.Parallel()
	roles := os.Getenv(helperRoleEnv)
	if roles == "" {
		return
	}
	role, rest, _ := strings.Cut(roles, ",")
	switch role {
	case "sleep":
		if _, err := os.Stdout.WriteString("ready " + strconv.Itoa(os.Getpid()) + "\n"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(60 * time.Second)
	case "exit":
		child := helperCommand(context.Background(), rest)
		child.Stdout = os.Stdout
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown role %q", role)
	}
}

func helperCommand(ctx context.Context, roles string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProcliveHelperProcess$")
	cmd.Env = append(os.Environ(), helperRoleEnv+"="+roles)
	return cmd
}

// startHelper starts the tree described by roles and returns the command and
// the PID of its sleeping leaf. Cleanup kills and waits for the command, and
// kills the leaf.
func startHelper(t *testing.T, roles string) (*exec.Cmd, int) {
	t.Helper()
	cmd := helperCommand(t.Context(), roles)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() {
		// A test that already waited gets ErrProcessDone and "Wait was
		// already called" here, which is fine.
		_ = cmd.Process.Kill() //nolint:errcheck // see above
		_ = cmd.Wait()         //nolint:errcheck // see above
	})
	r := bufio.NewReader(stdout)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("did not see the helper's ready line: %v", err)
		}
		pidText, ok := strings.CutPrefix(strings.TrimSpace(line), "ready ")
		if !ok {
			continue // the test framework's own output from a level that exited
		}
		pid, err := strconv.Atoi(pidText)
		if err != nil {
			t.Fatalf("parse helper pid %q: %v", pidText, err)
		}
		if pid != cmd.Process.Pid {
			t.Cleanup(func() {
				if leaf, err := os.FindProcess(pid); err == nil {
					_ = leaf.Kill()    //nolint:errcheck // cleanup; the leaf may already be gone
					_ = leaf.Release() //nolint:errcheck // closes the handle FindProcess opened
				}
			})
		}
		return cmd, pid
	}
}

func TestProcStatWindows_Self(t *testing.T) {
	t.Parallel()
	ppid, name, start, err := procStat(os.Getpid())
	if err != nil {
		t.Fatalf("procStat(self): %v", err)
	}
	if ppid <= 0 {
		t.Errorf("ppid = %d, want > 0", ppid)
	}
	if !strings.HasSuffix(strings.ToLower(name), ".exe") {
		t.Errorf("name = %q, want the executable's base name", name)
	}
	if _, err := strconv.ParseInt(start, 10, 64); err != nil {
		t.Errorf("start = %q, want a creation time: %v", start, err)
	}
}

func TestProcStatWindows_ReportsALivingParent(t *testing.T) {
	t.Parallel()
	_, pid := startHelper(t, "sleep")
	ppid, _, _, err := procStat(pid)
	if err != nil {
		t.Fatalf("procStat(child): %v", err)
	}
	if ppid != os.Getpid() {
		t.Fatalf("ppid = %d, want the test process %d", ppid, os.Getpid())
	}
}

// Windows keeps a process's parent PID after the parent exits, so the walk
// has to end there itself, and the chain it has is every living ancestor.
func TestProcStatWindows_ParentThatExitedEndsTheChain(t *testing.T) {
	t.Parallel()
	cmd, leaf := startHelper(t, "exit,sleep")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait for the exiting parent: %v", err)
	}

	ppid, _, _, err := procStat(leaf)
	if err != nil {
		t.Fatalf("procStat(leaf): %v", err)
	}
	if ppid != noLivingParent {
		t.Fatalf("ppid = %d, want noLivingParent once the parent has exited", ppid)
	}
	chain, complete := walkAncestry(procStat, leaf, "", "host", maxAncestryDepth)
	if len(chain) != 1 || chain[0].PID != leaf || !complete {
		t.Fatalf("walkAncestry = %+v, complete=%v; want just the leaf, complete", chain, complete)
	}
}

func TestCheckWindows_LiveThenExitedProcess(t *testing.T) {
	t.Parallel()
	cmd, pid := startHelper(t, "sleep")
	id, ok := IdentityOf(pid)
	if !ok {
		t.Fatal("IdentityOf(child) should resolve on Windows")
	}
	if got := Check(id); got != LivenessAlive {
		t.Fatalf("Check(live) = %v, want alive", got)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	_ = cmd.Wait() //nolint:errcheck // killed; the exit error is expected
	if got := Check(id); got != LivenessDead {
		t.Fatalf("Check(exited) = %v, want dead", got)
	}
}

// A handle keeps an exited process, and its PID, in the table. It must still
// read as dead.
func TestCheckWindows_ExitedProcessHeldOpenIsDead(t *testing.T) {
	t.Parallel()
	cmd, pid := startHelper(t, "sleep")
	id, ok := IdentityOf(pid)
	if !ok {
		t.Fatal("IdentityOf(child) should resolve on Windows")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	var got Liveness
	if err := cmd.Process.WithHandle(func(handle uintptr) {
		if _, err := syscall.WaitForSingleObject(syscall.Handle(handle), syscall.INFINITE); err != nil {
			t.Errorf("wait for exit: %v", err)
		}
		got = Check(id) // Go still holds its handle here
	}); err != nil {
		t.Fatalf("WithHandle: %v", err)
	}
	_ = cmd.Wait() //nolint:errcheck // killed; the exit error is expected
	if got != LivenessDead {
		t.Fatalf("Check(exited, still open) = %v, want dead", got)
	}
}

func TestCheckWindows_StartMismatchIsDead(t *testing.T) {
	t.Parallel()
	id := Identity{PID: os.Getpid(), Start: "1"}
	if got := Check(id); got != LivenessDead {
		t.Errorf("Check(start mismatch) = %v, want dead", got)
	}
}

func TestCurrentAncestryWindows(t *testing.T) {
	t.Parallel()
	ancestry, ok := CurrentAncestry()
	if !ok {
		t.Fatal("CurrentAncestry should resolve on Windows")
	}
	if parent, ok := IdentityOf(os.Getppid()); ok && Check(parent) == LivenessAlive {
		if depth := ancestry.Depth(parent); depth != 0 {
			t.Errorf("the parent must be the nearest ancestor (depth 0), got %d; chain %+v", depth, ancestry.Chain())
		}
	}
	_, pid := startHelper(t, "sleep")
	child, ok := IdentityOf(pid)
	if !ok {
		t.Fatal("IdentityOf(child) should resolve on Windows")
	}
	if depth := ancestry.Depth(child); depth != -1 {
		t.Errorf("a child process is not an ancestor, got depth %d", depth)
	}
	if got := ancestry.Excludes(child); got != ancestry.Complete() {
		t.Errorf("Excludes(child) = %v, want %v (whether the walk is complete); chain %+v", got, ancestry.Complete(), ancestry.Chain())
	}
}

func TestResolveOwnerWindows(t *testing.T) {
	t.Parallel()
	id, ok := ResolveOwner()
	if !ok {
		t.Skip("no owner resolved in this environment")
	}
	if isTransient(id.Name) {
		t.Errorf("resolved owner %q is a shell or the hook binary", id.Name)
	}
	if got := Check(id); got != LivenessAlive {
		t.Errorf("resolved owner Check = %v, want alive", got)
	}
}

func TestProcessCreatedAfterWindows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		p, other    process
		wantCreated bool
	}{
		{"later sequence number", process{seq: 11, created: 5}, process{seq: 10, created: 9}, true},
		{"earlier sequence number", process{seq: 9, created: 15}, process{seq: 10, created: 9}, false},
		{"clock fallback, later", process{created: 10}, process{seq: 10, created: 9}, true},
		{"clock fallback, same tick", process{created: 9}, process{created: 9}, false},
		{"clock fallback, earlier", process{seq: 11, created: 8}, process{created: 9}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.p.createdAfter(tc.other); got != tc.wantCreated {
				t.Fatalf("createdAfter = %v, want %v", got, tc.wantCreated)
			}
		})
	}
}
