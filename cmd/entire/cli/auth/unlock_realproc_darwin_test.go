//go:build darwin

package auth

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

const unlockChildEnv = "ENTIRE_UNLOCK_CHILD_TEST"

// TestUnlock_RealProcessTree exercises the real kernel lookups: this test
// binary serves under its own parent (the go test runner) and re-executes
// itself as a child, which must find and be served by the socket.
func TestUnlock_RealProcessTree(t *testing.T) {
	if os.Getenv(unlockChildEnv) == "1" {
		unlockChildMain()
		return
	}
	dir, err := os.MkdirTemp("/tmp", "entire-unlock-real-") //nolint:usetesting // t.TempDir is too long for a unix socket path on macOS
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		osroot.Forget(filepath.Join(dir, "entire"))
		_ = os.RemoveAll(dir)
	})
	t.Setenv("XDG_CACHE_HOME", dir)
	prompted.Store(true)
	forgetBundles()
	t.Cleanup(func() {
		prompted.Store(false)
		forgetBundles()
	})
	rememberBundle("se1:real|1", tokenBundle{Version: 1, Issuer: "https://core", Handle: "h", Access: "acc"})

	// Anchor explicitly on our parent (the go test runner); it is not git.
	parent, err := procLookup(os.Getppid())
	if err != nil {
		t.Fatalf("lookup parent: %v", err)
	}
	t.Logf("parent pid=%d comm=%q", os.Getppid(), parent.comm)
	stop, err := startUnlockServer(context.Background(), []anchor{{pid: os.Getppid(), start: parent.start}})
	if err != nil {
		t.Fatalf("startUnlockServer: %v", err)
	}
	defer stop()

	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run", "^TestUnlock_RealProcessTree$")
	cmd.Env = append(os.Environ(), unlockChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	t.Logf("child output:\n%s", out)
	if err != nil {
		t.Fatalf("child failed: %v", err)
	}
	if !strings.Contains(string(out), "CHILD-RESULT: served 1") {
		t.Fatalf("child was not served; see output above")
	}
}

// unlockChildMain runs in the re-executed child and reports its walk.
func unlockChildMain() {
	dir, err := userdirs.CacheDirChecked()
	fmt.Printf("CHILD cache dir %s err=%v\n", dir, err)
	pid := walkStartPID()
	for depth := 0; depth < unlockMaxDepth && pid > 1; depth++ {
		path := filepath.Join(dir, unlockDirName, unlockSocketName(pid))
		_, statErr := os.Lstat(path)
		info, lookErr := procLookup(pid)
		fmt.Printf("CHILD ancestor pid=%d socket=%v lookup=%+v err=%v\n", pid, statErr == nil, info, lookErr)
		if statErr == nil {
			m := dialUnlock(path)
			fmt.Printf("CHILD dial -> nil=%v len=%d\n", m == nil, len(m))
		}
		if lookErr != nil {
			break
		}
		pid = info.ppid
	}
	m := fetchUnlockedBundles()
	fmt.Printf("CHILD-RESULT: served %d\n", len(m))
}
