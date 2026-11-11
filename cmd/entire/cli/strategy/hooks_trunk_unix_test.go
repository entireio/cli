//go:build unix

package strategy

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO backup must not hang CheckGitHookState. The classification read
// used to follow the path unconditionally; this hung for the full timeout
// before the type check was added.
func TestCheckGitHookState_FIFOBackupDoesNotHang(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeOldChain(t, dir, "#!/bin/sh\necho user hook\n")
	backup := filepath.Join(dir, prePushHook+backupSuffix)
	if err := os.Remove(backup); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(backup, 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan GitHookState, 1)
	go func() { done <- gitHookStateInHooksDir(dir) }()
	select {
	case got := <-done:
		if got != GitHooksCurrent {
			t.Errorf("old chain to a FIFO backup = %v, want GitHooksCurrent", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gitHookStateInHooksDir hung on a FIFO backup")
	}
}
