package jsonutil

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWriteFileAtomic_CreatesNewFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "out.json")
	data := []byte(`{"hello":"world"}`)

	if err := WriteFileAtomic(target, data, 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("content mismatch: got %q want %q", got, data)
	}
}

func TestWriteFileAtomic_ReplacesExistingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "out.json")
	if err := os.WriteFile(target, []byte("old contents"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	newData := []byte("new contents")
	if err := WriteFileAtomic(target, newData, 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(newData) {
		t.Errorf("content not replaced: got %q want %q", got, newData)
	}
}

// AppliesPermission verifies the Chmod-before-rename step actually lands the
// requested mode on the final file. os.CreateTemp defaults to 0o600 so
// without the Chmod a 0o644 caller would silently get a tighter mode.
func TestWriteFileAtomic_AppliesPermission(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "out.json")

	if err := WriteFileAtomic(target, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("perm: got %#o want %#o", got, 0o600)
	}
}

// LeavesNoTempOnSuccess guards against the removeTmp defer being skipped or
// the temp suffix changing in a way that breaks cleanup.
func TestWriteFileAtomic_LeavesNoTempOnSuccess(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "out.json")

	if err := WriteFileAtomic(target, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected exactly one entry in dir, got %d: %v", len(entries), names)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

// CleansUpTempOnRenameFailure reaches the rename step and forces it to fail
// (renaming a regular file onto a non-empty directory is rejected on every
// POSIX filesystem, and on Windows). The removeTmp defer must clear the
// orphan so /tmp doesn't accumulate junk across many failed writes.
func TestWriteFileAtomic_CleansUpTempOnRenameFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "out.json")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "occupant"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed dir: %v", err)
	}

	err := WriteFileAtomic(target, []byte("x"), 0o644)
	if err == nil {
		t.Fatal("expected error when target is a non-empty directory")
	}

	info, statErr := os.Stat(target)
	if statErr != nil {
		t.Fatalf("stat target: %v", statErr)
	}
	if !info.IsDir() {
		t.Error("target should still be a directory after failed rename")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file after failed rename: %s", e.Name())
		}
	}
}

// DanglingSymlink pins the stow/chezmoi link-farm case: an existing symlink
// whose target file does not exist yet must be written THROUGH (creating the
// target), not replaced by a regular file — EvalSymlinks errors on a dangling
// link exactly like on a missing file, and the old fallback took that error as
// "no symlink here".
func TestWriteFileAtomicFollowingSymlinks_DanglingSymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "settings.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	data := []byte(`{"a":1}`)
	if err := WriteFileAtomicFollowingSymlinks(link, data, 0o600); err != nil {
		t.Fatalf("WriteFileAtomicFollowingSymlinks: %v", err)
	}

	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("dangling symlink was replaced by a regular file")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("target file not created through the link: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("target content: got %q want %q", got, data)
	}
}

// PreservesExistingMode pins that a rewrite keeps the file's existing mode
// instead of forcing perm onto it — a user's 0644 settings file must not come
// back 0600 from every hook install.
func TestWriteFileAtomicFollowingSymlinks_PreservesExistingMode(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "out.json")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := WriteFileAtomicFollowingSymlinks(target, []byte("new"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomicFollowingSymlinks: %v", err)
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("perm after rewrite: got %#o want %#o", got, 0o644)
	}
}

func TestWriteFileAtomic_ParentMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "does-not-exist", "out.json")

	err := WriteFileAtomic(target, []byte("x"), 0o644)
	if err == nil {
		t.Fatal("expected error when parent dir is missing")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected ErrNotExist; got: %v", err)
	}
}

// A dotfile manager can create the link before the target's directory exists;
// the write must create that directory and land through the link.
func TestWriteFileAtomicFollowingSymlinks_DanglingSymlinkMissingTargetDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "entire", "settings.json")
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	data := []byte(`{"a":1}`)
	if err := WriteFileAtomicFollowingSymlinks(link, data, 0o600); err != nil {
		t.Fatalf("WriteFileAtomicFollowingSymlinks: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("target not created through the link: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("target content: got %q want %q", got, data)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link was replaced by a regular file (err=%v)", err)
	}
}

// A symlink cycle that is not self-referential (a -> b -> a) is detected as a
// revisit rather than walked to the hop cap, and the write reports ELOOP.
func TestWriteFileAtomicFollowingSymlinks_CycleFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	if err := os.Symlink(b, a); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Fatal(err)
	}

	if got := resolveWriteTarget(a); got != a && got != b {
		t.Fatalf("resolveWriteTarget on a cycle = %q, want one of the cycle members", got)
	}
	if err := WriteFileAtomicFollowingSymlinks(a, []byte("x"), 0o600); err == nil {
		t.Fatal("writing through a symlink cycle must fail")
	}
}
