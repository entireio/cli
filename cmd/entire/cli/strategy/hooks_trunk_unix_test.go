//go:build unix

package strategy

import (
	"context"
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

// The chain must not open a FIFO left as a backup: an executable FIFO
// passes [ -x ], and on macOS exec of that FIFO blocks waiting for a writer,
// hanging every commit or push. Linux rejects the exec at once, so this was
// green in CI and failed on macOS. The chain only runs a regular executable,
// so this backup is skipped.
func TestChainedHook_FIFOBackupDoesNotHang(t *testing.T) {
	t.Parallel()
	f := newHooksFixture(t)
	bin := t.TempDir()
	writeExec(t, filepath.Join(bin, "entire"), "#!/bin/sh\n")
	f.write("post-commit", "#!/bin/sh\n")
	f.installExecutable(specFor(t, "post-commit"))
	backup := filepath.Join(f.dir, "post-commit"+backupSuffix)
	if err := os.Remove(backup); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(backup, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(backup, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	out, err := runHook(ctx, bin, filepath.Join(f.dir, "post-commit"))
	if ctx.Err() != nil {
		t.Fatalf("post-commit hung on a FIFO backup:\n%s", out)
	}
	if err != nil {
		t.Fatalf("post-commit should skip a FIFO backup, got err=%v\n%s", err, out)
	}
}
