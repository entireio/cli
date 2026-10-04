package auth

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// fakeProcs is a process table: pid -> (ppid, start, comm).
type fakeProcs map[int]procInfo

// The process tree of a push, as git really builds it:
//
//	100 git        the user's `git push`
//	150 git        `git remote-entire` wrapper for the outer helper (us)
//	200 entire     the pre-push hook
//	300 git        the hook's nested `git push`
//	350 git        `git remote-entire` wrapper for the nested helper
//	360 git-remote-entire   the nested helper (the peer)
//	900 bash       an unrelated agent shell
func pushTree() fakeProcs {
	return fakeProcs{
		100: {ppid: 1, start: 1_000, comm: "git"},
		150: {ppid: 100, start: 1_050, comm: "git"},
		200: {ppid: 100, start: 1_100, comm: "entire"},
		300: {ppid: 200, start: 1_200, comm: "git"},
		350: {ppid: 300, start: 1_250, comm: "git"},
		360: {ppid: 350, start: 1_260, comm: "git-remote-entire"},
		900: {ppid: 1, start: 500, comm: "bash"},
	}
}

// unlockFixture installs a fake process table and peer identity and points
// the cache dir at a short temp path (unix socket paths are length-limited).
// The server under test is the outer helper, child of wrapper 150; peer is
// the pid the server will see on every connection.
// fakeTree mutates the fixture's process view safely while a server runs.
type fakeTree struct {
	setProc func(pid int, info procInfo)
	setPPID func(pid int)
}

func unlockFixture(t *testing.T, procs fakeProcs, peer int) fakeTree {
	t.Helper()
	var tableMu sync.Mutex
	ppid := 150 // the outer helper's parent: the git remote-entire wrapper
	tree := fakeTree{
		setProc: func(pid int, info procInfo) {
			tableMu.Lock()
			defer tableMu.Unlock()
			procs[pid] = info
		},
		setPPID: func(pid int) {
			tableMu.Lock()
			defer tableMu.Unlock()
			ppid = pid
		},
	}
	dir, err := os.MkdirTemp("/tmp", "entire-unlock-") //nolint:usetesting // t.TempDir is too long for a unix socket path on macOS
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		osroot.Forget(filepath.Join(dir, "entire"))
		_ = os.RemoveAll(dir)
	})
	t.Setenv("XDG_CACHE_HOME", dir)

	prevProc, prevPeer, prevPPID, prevWalk, prevServe := procLookup, peerLookup, currentPPID, walkStartPID, serveBundles
	procLookup = func(pid int) (procInfo, error) {
		tableMu.Lock()
		defer tableMu.Unlock()
		info, ok := procs[pid]
		if !ok {
			return procInfo{}, os.ErrNotExist
		}
		return info, nil
	}
	peerLookup = func(*net.UnixConn) (int, int, error) { return peer, os.Getuid(), nil }
	currentPPID = func() int {
		tableMu.Lock()
		defer tableMu.Unlock()
		return ppid
	}
	walkStartPID = func() int { return 150 }
	prompted.Store(true)
	forgetBundles()
	t.Cleanup(func() {
		procLookup, peerLookup, currentPPID, walkStartPID, serveBundles = prevProc, prevPeer, prevPPID, prevWalk, prevServe
		prompted.Store(false)
		forgetBundles()
	})
	return tree
}

// serveCurrentBundles freezes what the server hands out, so a test can then
// clear the shared cache to play a fresh client process.
func serveCurrentBundles() {
	fixed := snapshotBundles()
	serveBundles = func() map[string]tokenBundle { return fixed }
}

func socketExists(t *testing.T, pid int) bool {
	t.Helper()
	path := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "entire", unlockDirName, unlockSocketName(pid))
	_, err := os.Lstat(path)
	return err == nil
}

func TestUnlock_AnchorsOnEveryGitAncestor(t *testing.T) {
	unlockFixture(t, pushTree(), 360)
	stop, err := StartUnlockServer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if !socketExists(t, 150) || !socketExists(t, 100) {
		t.Fatal("expected sockets for the wrapper (150) and the user's git (100)")
	}
	if socketExists(t, 200) {
		t.Fatal("non-git ancestor must not get a socket")
	}
	stop()
	if socketExists(t, 150) || socketExists(t, 100) {
		t.Fatal("stop must remove the sockets")
	}
}

func TestUnlock_NestedHelperGetsBundles(t *testing.T) {
	unlockFixture(t, pushTree(), 360)
	rememberBundle("se1:abc|123", tokenBundle{Version: 1, Issuer: "https://core", Handle: "h", Access: "acc"})
	serveCurrentBundles()
	stop, err := StartUnlockServer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// The nested helper walks 350 -> 300 -> 200 -> 100 and finds git-100.
	walkStartPID = func() int { return 350 }
	forgetBundles()
	got := fetchUnlockedBundles()
	if got == nil || got["se1:abc|123"].Access != "acc" {
		t.Fatalf("nested helper did not receive bundles: %+v", got)
	}
}

func TestUnlock_UnrelatedPeerRefused(t *testing.T) {
	unlockFixture(t, pushTree(), 900)
	rememberBundle("se1:abc|123", tokenBundle{Version: 1, Access: "acc"})
	serveCurrentBundles()
	stop, err := StartUnlockServer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// Even a client that knows the socket name gets nothing.
	walkStartPID = func() int { return 100 }
	forgetBundles()
	if got := fetchUnlockedBundles(); got != nil {
		t.Fatalf("unrelated peer received bundles: %+v", got)
	}
}

func TestUnlock_ReusedGitPIDRefused(t *testing.T) {
	tree := unlockFixture(t, pushTree(), 360)
	rememberBundle("se1:abc|123", tokenBundle{Version: 1, Access: "acc"})
	serveCurrentBundles()
	stop, err := StartUnlockServer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// git exited and pid 100 now belongs to a newer process.
	tree.setProc(100, procInfo{ppid: 1, start: 9_000, comm: "git"})
	walkStartPID = func() int { return 350 }
	forgetBundles()
	if got := fetchUnlockedBundles(); got != nil {
		t.Fatalf("reused pid received bundles: %+v", got)
	}
}

func TestUnlock_ReparentedServerRefuses(t *testing.T) {
	tree := unlockFixture(t, pushTree(), 360)
	rememberBundle("se1:abc|123", tokenBundle{Version: 1, Access: "acc"})
	serveCurrentBundles()
	stop, err := StartUnlockServer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// The serving helper's parent is now launchd: its git chain is gone.
	tree.setPPID(1)
	forgetBundles()
	path := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "entire", unlockDirName, unlockSocketName(100))
	if got := dialUnlock(path); got != nil {
		t.Fatalf("reparented server still served: %+v", got)
	}
}

func TestUnlock_NoServerWithoutPrompt(t *testing.T) {
	unlockFixture(t, pushTree(), 360)
	prompted.Store(false)
	stop, err := StartUnlockServer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	stop()
	walkStartPID = func() int { return 350 }
	if got := fetchUnlockedBundles(); got != nil {
		t.Fatalf("server ran without a prompt: %+v", got)
	}
}

func TestUnlock_NoGitAncestorNoServer(t *testing.T) {
	procs := pushTree()
	unlockFixture(t, procs, 360)
	// Started from a shell directly: no git above us.
	currentPPID = func() int { return 900 }
	stop, err := StartUnlockServer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if socketExists(t, 900) || socketExists(t, 100) {
		t.Fatal("no socket expected without a git ancestor")
	}
}

func TestUnlock_OpenSealedSlotUsesUnlock(t *testing.T) {
	unlockFixture(t, pushTree(), 360)
	fs := &fakeSealer{}
	SetSealerForTesting(t, fs)
	prompted.Store(true)
	sl, err := protection.sealer()
	if err != nil {
		t.Fatal(err)
	}
	enc, err := sealSlot(sl, tokenBundle{Issuer: "https://core.example.test", Handle: "h", Access: "acc"}, 600)
	if err != nil {
		t.Fatal(err)
	}
	serveCurrentBundles()
	stop, err := StartUnlockServer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// A fresh nested process: nothing cached, but an unlock is reachable.
	walkStartPID = func() int { return 350 }
	forgetBundles()
	b, _, err := openSealedSlot(enc, "https://core.example.test", "h", "test")
	if err != nil {
		t.Fatal(err)
	}
	if b.Access != "acc" || fs.count() != 0 {
		t.Fatalf("unlock not used: bundle=%+v prompts=%d", b, fs.count())
	}
}
