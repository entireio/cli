//go:build windows

package execx

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// treeKillRoleEnv tells a re-exec'd test binary to act as one level of a
// process tree. It holds the roles from this level down, comma-separated:
// "wait" starts the next level and waits for it, "exit" starts it and exits,
// "orphan" starts it and exits once its own parent has exited, and "sleep"
// prints "ready <pid>" and sleeps. Every level inherits stdout.
const treeKillRoleEnv = "ENTIRE_EXECX_TREEKILL_ROLE"

// TestTreeKillHelperProcess is not a test: it is the process the tree-kill
// tests start, and does nothing when run directly.
func TestTreeKillHelperProcess(t *testing.T) {
	t.Parallel()
	roles := os.Getenv(treeKillRoleEnv)
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
	case "wait", "exit", "orphan":
		var parent windows.Handle
		if role == "orphan" {
			// Open the parent first, so its PID can't be reused before we wait.
			ppid, ok := toPID(os.Getppid())
			if !ok {
				t.Fatalf("parent PID %d", os.Getppid())
			}
			h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, ppid)
			if err != nil {
				t.Fatalf("open parent: %v", err)
			}
			parent = h
		}
		child := treeKillHelper(context.Background(), rest)
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		switch role {
		case "wait":
			_ = child.Wait() //nolint:errcheck // killed by the test; the exit error is expected
		case "orphan":
			_, _ = windows.WaitForSingleObject(parent, windows.INFINITE) //nolint:errcheck // exits either way
		}
	default:
		t.Fatalf("unknown role %q", role)
	}
}

// treeKillHelper returns a command that re-execs this test binary as the
// process tree described by roles.
func treeKillHelper(ctx context.Context, roles string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTreeKillHelperProcess$")
	cmd.Env = append(os.Environ(), treeKillRoleEnv+"="+roles)
	return cmd
}

// startTree starts the tree described by roles under TerminateOnCancel and
// returns the command, its buffered stdout, and a handle to the sleeping leaf,
// opened before anything is cancelled so its PID cannot be reused meanwhile.
func startTree(ctx context.Context, t *testing.T, roles string) (*exec.Cmd, *bufio.Reader, windows.Handle) {
	t.Helper()
	cmd := treeKillHelper(ctx, roles)
	TerminateOnCancel(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill() //nolint:errcheck // cleanup after a failed test; usually already gone
	})

	r := bufio.NewReader(stdout)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("did not see the leaf's ready line: %v", err)
		}
		pidText, ok := strings.CutPrefix(strings.TrimSpace(line), "ready ")
		if !ok {
			continue // the test framework's own output from a level that exited
		}
		pid, err := strconv.ParseUint(pidText, 10, 32)
		if err != nil {
			t.Fatalf("parse leaf pid %q: %v", pidText, err)
		}
		leaf, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(pid))
		if err != nil {
			t.Fatalf("open leaf %d: %v", pid, err)
		}
		t.Cleanup(func() {
			_ = windows.TerminateProcess(leaf, 1) //nolint:errcheck // cleanup after a failed test
			_ = windows.CloseHandle(leaf)         //nolint:errcheck // cleanup
		})
		return cmd, r, leaf
	}
}

// waitExited reports whether handle's process exits within timeout.
func waitExited(handle windows.Handle, timeout time.Duration) bool {
	event, err := windows.WaitForSingleObject(handle, uint32(timeout.Milliseconds()))
	return err == nil && event == windows.WAIT_OBJECT_0
}

// drainAndWait reads stdout to EOF and waits for cmd, failing the test if that
// takes long enough for KillWaitDelay to have closed the pipe instead: only a
// tree kill closes it sooner.
func drainAndWait(t *testing.T, cmd *exec.Cmd, stdout io.Reader) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, stdout) //nolint:errcheck // draining until the pipe closes
		_ = cmd.Wait()                     //nolint:errcheck // killed by cancel; the exit error is expected
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(KillWaitDelay / 2):
		t.Fatal("drain did not return after cancel; a descendant kept stdout open")
	}
}

// A cancelled command's descendants must die with it, or the one holding
// stdout keeps the drain (and so Wait) blocked until KillWaitDelay.
func TestTerminateOnCancel_KillsProcessTreeWindows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		roles string
	}{
		{"child that the parent waits for", "wait,sleep"},
		{"child of a parent that already exited", "exit,sleep"},
		{"grandchild", "wait,wait,sleep"},
		{"grandchild under a parent that already exited", "exit,wait,sleep"},
		{"grandchild under a child that exits when its parent dies", "wait,orphan,sleep"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cmd, stdout, leaf := startTree(ctx, t, tc.roles)

			cancel()
			drainAndWait(t, cmd, stdout)
			if !waitExited(leaf, 5*time.Second) {
				t.Fatal("the leaf process is still running after cancel")
			}
		})
	}
}

// The walk must stop at cmd's own tree: a process the test started beside it
// is no descendant and has to survive the cancel.
func TestTerminateOnCancel_SparesUnrelatedProcessesWindows(t *testing.T) {
	t.Parallel()
	_, _, bystander := startTree(t.Context(), t, "sleep")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd, stdout, leaf := startTree(ctx, t, "wait,sleep")
	cancel()
	drainAndWait(t, cmd, stdout)

	if !waitExited(leaf, 5*time.Second) {
		t.Fatal("the cancelled tree's leaf is still running")
	}
	if waitExited(bystander, 0) {
		t.Fatal("cancel killed a process outside the command's tree")
	}
}

// Once Wait has reaped the process its PID may belong to anyone, so Cancel
// must not walk from it, and reports the process done as the default Cancel
// does.
func TestKillProcessGroupOnCancel_AfterWaitWindows(t *testing.T) {
	t.Parallel()
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^$")
	killProcessGroupOnCancel(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := cmd.Cancel(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Cancel after Wait = %v; want os.ErrProcessDone", err)
	}
}

func TestTerminateOnCancel_SetsWaitDelayAndTreeKillWindows(t *testing.T) {
	t.Parallel()
	cmd := exec.CommandContext(context.Background(), "entire")
	TerminateOnCancel(cmd)
	if cmd.WaitDelay != KillWaitDelay {
		t.Errorf("WaitDelay = %v; want %v", cmd.WaitDelay, KillWaitDelay)
	}
	if cmd.Cancel == nil {
		t.Error("Cancel = nil; want the tree-kill handler")
	}
}

// fakeTable stands in for OpenProcess: what the kernel would report for each
// PID's handle, and which handles were released.
type fakeTable struct {
	procs    map[uint32]openedProc
	released []uint32
}

func (f *fakeTable) open(pid uint32) (openedProc, error) {
	proc, ok := f.procs[pid]
	if !ok {
		return openedProc{}, windows.ERROR_INVALID_PARAMETER
	}
	proc.handle = windows.Handle(pid)
	return proc, nil
}

func (f *fakeTable) release(proc openedProc) {
	f.released = append(f.released, uint32(proc.handle))
}

func foundPIDs(found []openedProc) []uint32 {
	pids := make([]uint32, 0, len(found))
	for _, proc := range found {
		pids = append(pids, uint32(proc.handle))
	}
	return pids
}

func TestFindDescendantsWindows(t *testing.T) {
	t.Parallel()
	const root = 100
	clockOnly := birth{created: 10}
	sequenced := birth{seq: 100, created: 50}
	for _, tc := range []struct {
		name         string
		root         birth
		entries      []processEntry
		procs        map[uint32]openedProc
		want         []uint32
		wantReleased []uint32
	}{
		{
			name:    "follows each generation, parents first",
			entries: []processEntry{{300, 200}, {200, root}, {400, 300}, {0, 0}},
			procs: map[uint32]openedProc{
				200: {ppid: root, born: birth{created: 20}},
				300: {ppid: 200, born: birth{created: 30}},
				400: {ppid: 300, born: birth{created: 40}},
			},
			want: []uint32{200, 300, 400},
		},
		{
			name:    "skips a process older than the parent it lists",
			entries: []processEntry{{200, root}, {300, 200}},
			procs: map[uint32]openedProc{
				// Started by an earlier holder of PID 100, before root existed.
				200: {ppid: root, born: birth{created: 5}},
				300: {ppid: 200, born: birth{created: 30}},
			},
			wantReleased: []uint32{200},
		},
		{
			name:    "skips a PID reused after the table was read",
			entries: []processEntry{{200, root}},
			procs: map[uint32]openedProc{
				// The table said root started PID 200; the process holding it now
				// was started by someone else.
				200: {ppid: 999, born: birth{created: 50}},
			},
			wantReleased: []uint32{200},
		},
		{
			name:    "cannot vouch for children of a process it cannot open",
			entries: []processEntry{{200, root}, {300, 200}},
			procs: map[uint32]openedProc{
				300: {ppid: 200, born: birth{created: 30}},
			},
		},
		{
			name:    "accepts a child created in the same clock tick as its parent",
			entries: []processEntry{{200, root}},
			procs:   map[uint32]openedProc{200: {ppid: root, born: birth{created: 10}}},
			want:    []uint32{200},
		},
		{
			name:    "orders by sequence number, not the clock, when both have one",
			root:    sequenced,
			entries: []processEntry{{200, root}, {300, root}},
			procs: map[uint32]openedProc{
				// Created after root, but the clock was set back in between.
				200: {ppid: root, born: birth{seq: 110, created: 40}},
				// Created before root, by an earlier holder of PID 100, though
				// the clock reads later.
				300: {ppid: root, born: birth{seq: 90, created: 60}},
			},
			want:         []uint32{200},
			wantReleased: []uint32{300},
		},
		{
			name:    "rejects a sequence number equal to its parent's",
			root:    sequenced,
			entries: []processEntry{{200, root}},
			procs: map[uint32]openedProc{
				200: {ppid: root, born: birth{seq: 100, created: 50}},
			},
			wantReleased: []uint32{200},
		},
		{
			name:    "falls back to the clock when one side has no sequence number",
			root:    sequenced,
			entries: []processEntry{{200, root}, {300, root}},
			procs: map[uint32]openedProc{
				200: {ppid: root, born: birth{created: 50}},
				300: {ppid: root, born: birth{created: 49}},
			},
			want:         []uint32{200},
			wantReleased: []uint32{300},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			table := &fakeTable{procs: tc.procs}
			rootBirth := tc.root
			if rootBirth == (birth{}) {
				rootBirth = clockOnly
			}
			tree := map[uint32]birth{root: rootBirth}
			found := findDescendants(tc.entries, tree, table.open, table.release)

			if got := foundPIDs(found); !slices.Equal(got, tc.want) {
				t.Errorf("found %v; want %v", got, tc.want)
			}
			if !slices.Equal(table.released, tc.wantReleased) {
				t.Errorf("released %v; want %v", table.released, tc.wantReleased)
			}
			for _, pid := range tc.want {
				if _, ok := tree[pid]; !ok {
					t.Errorf("found PID %d was not added to the tree", pid)
				}
			}
		})
	}
}

// A later pass must find only what the earlier ones missed.
func TestFindDescendantsWindows_SkipsMembers(t *testing.T) {
	t.Parallel()
	table := &fakeTable{procs: map[uint32]openedProc{
		200: {ppid: 100, born: birth{created: 20}},
		300: {ppid: 200, born: birth{created: 30}},
	}}
	tree := map[uint32]birth{100: {created: 10}, 200: {created: 20}}
	found := findDescendants([]processEntry{{200, 100}, {300, 200}}, tree, table.open, table.release)
	if got := foundPIDs(found); !slices.Equal(got, []uint32{300}) {
		t.Fatalf("found %v; want [300]", got)
	}
}
