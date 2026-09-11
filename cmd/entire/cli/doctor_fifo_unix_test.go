//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/worktreedir"
)

// TestScanForSymlinkedComponent_NonTraversableComponent pins the allowlist. An
// earlier revision tested only for a regular file, so a FIFO, socket or device
// node where a directory belongs came back clean and doctor printed nothing —
// while os.Root and every hook install fail on it.
//
// Unix-only by build constraint rather than by a runtime t.Skip: syscall.Mkfifo
// does not exist on Windows at all, and a runtime guard still has to compile.
func TestScanForSymlinkedComponent_NonTraversableComponent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, claudeDirName), 0o600); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
	root, err := worktreedir.OpenAt(dir)
	if err != nil {
		t.Fatal(err)
	}

	name, outcome := scanForSymlinkedComponent(root, claudeDirName+"/settings.json")
	if outcome != componentScanWrongType {
		t.Errorf("outcome = %v, want componentScanWrongType for a FIFO", outcome)
	}
	if name != claudeDirName {
		t.Errorf("name = %q, want %s", name, claudeDirName)
	}
}

// TestScanForSymlinkedComponent_FifoAtTheLeaf is the half that matters most, and
// the one an earlier revision missed by gating the type check on `prefix !=
// name`. A FIFO here does not fail the config read, it blocks it: every agent
// reads through osroot.OpenNoFollow, whose open(2) has no O_NONBLOCK, so
// `entire doctor` hangs in openat until interrupted. Reported so the condition
// is at least nameable.
func TestScanForSymlinkedComponent_FifoAtTheLeaf(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, claudeDirName), 0o750); err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(dir, claudeDirName, "settings.json")
	if err := syscall.Mkfifo(leaf, 0o600); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
	root, err := worktreedir.OpenAt(dir)
	if err != nil {
		t.Fatal(err)
	}

	name, outcome := scanForSymlinkedComponent(root, claudeDirName+"/settings.json")
	if outcome != componentScanWrongType {
		t.Errorf("outcome = %v, want componentScanWrongType for a FIFO leaf", outcome)
	}
	if name != claudeDirName+"/settings.json" {
		t.Errorf("name = %q, want the leaf itself", name)
	}
}
