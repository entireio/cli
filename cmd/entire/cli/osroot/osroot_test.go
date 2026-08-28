package osroot_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("world"), 0o644))

	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()

	data, err := osroot.ReadFile(root, "hello.txt")
	require.NoError(t, err)
	assert.Equal(t, "world", string(data))
}

func TestReadFile_NotFound(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()

	_, err = osroot.ReadFile(root, "missing.txt")
	assert.Error(t, err)
}

func TestReadFile_TraversalBlocked(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	outsideDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outsideDir, "secret.txt"), []byte("secret"), 0o644))

	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()

	_, err = osroot.ReadFile(root, "../secret.txt")
	assert.Error(t, err)
}

func TestReadFileNoFollow_RejectsLeafSymlink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "target.txt"), []byte("secret"), 0o600))
	if err := os.Symlink("target.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()

	_, err = osroot.ReadFileNoFollow(root, "link.txt")
	require.ErrorIs(t, err, osroot.ErrSymlinkedPath)
}

func TestOpenFileNoFollow_RejectsLeafSymlink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	require.NoError(t, os.WriteFile(target, []byte("original"), 0o600))
	if err := os.Symlink("target.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()

	_, err = osroot.OpenFileNoFollow(root, "link.txt", os.O_WRONLY|os.O_APPEND, 0o600)
	require.ErrorIs(t, err, osroot.ErrSymlinkedPath)
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "original", string(got))
}

func TestMkdirAll(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()

	require.NoError(t, osroot.MkdirAll(root, "a/b/c", 0o755))

	info, err := os.Stat(filepath.Join(dir, "a", "b", "c"))
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func TestMkdirAll_Idempotent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()

	require.NoError(t, osroot.MkdirAll(root, "x/y", 0o755))
	// Creating an existing tree must not error.
	require.NoError(t, osroot.MkdirAll(root, "x/y", 0o755))
}

func TestMkdirAll_TraversalBlocked(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	outsideDir := t.TempDir()

	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()

	for _, name := range []string{"../escape", "../../escape/deep", "a/../../escape"} {
		err := osroot.MkdirAll(root, name, 0o755)
		require.Error(t, err, "MkdirAll(%q) must be rejected", name)
	}

	// Nothing must have been created outside the root.
	entries, err := os.ReadDir(outsideDir)
	require.NoError(t, err)
	assert.Empty(t, entries, "no directories should be created outside the root")
}

func TestWriteFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()

	err = osroot.WriteFile(root, "output.txt", []byte("data"), 0o600)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(dir, "output.txt"))
	require.NoError(t, err)
	assert.Equal(t, "data", string(data))
}

func TestWriteFile_Overwrite(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "existing.txt"), []byte("old"), 0o644))

	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()

	err = osroot.WriteFile(root, "existing.txt", []byte("new"), 0o600)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(dir, "existing.txt"))
	require.NoError(t, err)
	assert.Equal(t, "new", string(data))
}

func TestWriteFile_TraversalBlocked(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()

	err = osroot.WriteFile(root, "../escape.txt", []byte("bad"), 0o600)
	assert.Error(t, err)
}

func TestRemove(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "delete-me.txt"), []byte("bye"), 0o644))

	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()

	err = osroot.Remove(root, "delete-me.txt")
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(dir, "delete-me.txt"))
	assert.True(t, os.IsNotExist(err))
}

func TestRemove_NotFound(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()

	err = osroot.Remove(root, "nonexistent.txt")
	assert.NoError(t, err)
}

func TestRemove_TraversalBlocked(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	outsideDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outsideDir, "protected.txt"), []byte("safe"), 0o644))

	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()

	err = osroot.Remove(root, "../protected.txt")
	require.Error(t, err)

	_, err = os.Stat(filepath.Join(outsideDir, "protected.txt"))
	require.NoError(t, err)
}

func TestSymlinkTraversal_ReadBlocked(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	outsideDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outsideDir, "secret.txt"), []byte("secret"), 0o644))

	// Create a symlink inside the root that points outside
	require.NoError(t, os.Symlink(outsideDir, filepath.Join(dir, "escape")))

	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()

	// os.Root should block following the symlink
	_, err = osroot.ReadFile(root, "escape/secret.txt")
	assert.Error(t, err, "symlink traversal should be blocked by os.Root")
}

func TestSymlinkTraversal_WriteBlocked(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	outsideDir := t.TempDir()

	// Create a symlink inside the root that points outside
	require.NoError(t, os.Symlink(outsideDir, filepath.Join(dir, "escape")))

	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()

	// os.Root should block following the symlink
	err = osroot.WriteFile(root, "escape/evil.txt", []byte("bad"), 0o600)
	require.Error(t, err, "symlink traversal should be blocked by os.Root")

	// Verify file was not created outside
	_, err = os.Stat(filepath.Join(outsideDir, "evil.txt"))
	require.ErrorIs(t, err, os.ErrNotExist, "file should not be created outside root")
}

func TestReadDir_ListsSortedEntries(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tmp", "nested"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, name := range []string{"c.json", "a.json", "b.json"} {
		if err := os.WriteFile(filepath.Join(dir, "tmp", name), []byte("{}"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("os.OpenRoot: %v", err)
	}
	defer root.Close()

	entries, err := osroot.ReadDir(root, "tmp")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	want := []string{"a.json", "b.json", "c.json", "nested"}
	if len(got) != len(want) {
		t.Fatalf("entries = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entries = %v, want %v", got, want)
		}
	}
}

func TestReadDir_MissingDirIsNotExist(t *testing.T) {
	t.Parallel()

	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("os.OpenRoot: %v", err)
	}
	defer root.Close()

	if _, err := osroot.ReadDir(root, "tmp"); !os.IsNotExist(err) {
		t.Errorf("ReadDir on a missing directory = %v, want a not-exist error", err)
	}
}

func TestReadDir_RejectsEscapingName(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "inner"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	root, err := os.OpenRoot(filepath.Join(dir, "inner"))
	if err != nil {
		t.Fatalf("os.OpenRoot: %v", err)
	}
	defer root.Close()

	if _, err := osroot.ReadDir(root, ".."); err == nil {
		t.Error("expected an escaping name to be rejected")
	}
}

func TestMkdirAllNoSymlink_CreatesNestedDirs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()

	require.NoError(t, osroot.MkdirAllNoSymlink(root, "metadata/session-1/tasks", 0o750))

	info, err := os.Stat(filepath.Join(dir, "metadata", "session-1", "tasks"))
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func TestMkdirAllNoSymlink_IsIdempotent(t *testing.T) {
	t.Parallel()

	root, err := os.OpenRoot(t.TempDir())
	require.NoError(t, err)
	defer root.Close()

	require.NoError(t, osroot.MkdirAllNoSymlink(root, "tmp", 0o750))
	require.NoError(t, osroot.MkdirAllNoSymlink(root, "tmp", 0o750))
}

// The point of the function: a symlink already sitting at the path is refused by
// name rather than silently written through.
func TestMkdirAllNoSymlink_RejectsSymlinkedLeaf(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(dir, "logs")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()

	err = osroot.MkdirAllNoSymlink(root, "logs", 0o750)
	require.ErrorIs(t, err, osroot.ErrSymlinkedPath)
	assert.Contains(t, err.Error(), "logs")
}

// A symlinked ancestor is the more dangerous case: without the check, every file
// written under it lands somewhere the caller never named.
func TestMkdirAllNoSymlink_RejectsSymlinkedParent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(dir, "metadata")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()

	err = osroot.MkdirAllNoSymlink(root, "metadata/session-1", 0o750)
	require.ErrorIs(t, err, osroot.ErrSymlinkedPath)

	// Nothing was created at the target: the refusal happens before any Mkdir.
	entries, readErr := os.ReadDir(elsewhere)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "a refused create must not have written through the link")
}

// A symlink that stays inside the root is refused too. os.Root would follow it
// happily, so the kernel's containment is not the property being tested here.
func TestMkdirAllNoSymlink_RejectsSymlinkThatStaysInsideRoot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "real"), 0o750))
	if err := os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "tmp")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()

	require.ErrorIs(t, osroot.MkdirAllNoSymlink(root, "tmp", 0o750), osroot.ErrSymlinkedPath)
}

func TestSymlinkPaths_FindsNestedSymlinksWithoutFollowing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	elsewhere := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(elsewhere, "secret.txt"), []byte("x"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "metadata", "s1"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.json"), []byte("{}"), 0o600))
	if err := os.Symlink(elsewhere, filepath.Join(dir, "logs")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	require.NoError(t, os.Symlink(elsewhere, filepath.Join(dir, "metadata", "s1", "assets")))

	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()

	got, err := osroot.SymlinkPaths(root, ".")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"logs", "metadata/s1/assets"}, got,
		"both the top-level and the nested symlink are reported, and neither is descended into")
}

func TestSymlinkPaths_CleanTreeAndMissingDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "logs"), 0o750))
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()

	got, err := osroot.SymlinkPaths(root, ".")
	require.NoError(t, err)
	assert.Empty(t, got)

	got, err = osroot.SymlinkPaths(root, "absent")
	require.NoError(t, err, "nothing there is nothing wrong")
	assert.Empty(t, got)
}

func TestOpenChild_RejectsSymlinkedDirectoryInsideRoot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "elsewhere"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "elsewhere", "state.json"), []byte("other repo"), 0o600))
	// A link that stays INSIDE the root: os.Root follows this one, which is
	// exactly why OpenChild exists.
	if err := os.Symlink("elsewhere", filepath.Join(dir, "entire-sessions")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()

	_, err = osroot.OpenChild(root, "entire-sessions")
	require.ErrorIs(t, err, osroot.ErrSymlinkedPath)
}

func TestOpenChild_OpensRealDirectoryAndIsNotShared(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "entire-sessions"), 0o750))
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()

	child, err := osroot.OpenChild(root, "entire-sessions")
	require.NoError(t, err)
	require.NoError(t, osroot.WriteFile(child, "s.json", []byte("ok"), 0o600))
	// Owned by the caller, unlike SharedChild: closing it must be safe, and a
	// second call must hand back an independent handle.
	require.NoError(t, child.Close())

	again, err := osroot.OpenChild(root, "entire-sessions")
	require.NoError(t, err)
	defer again.Close()
	got, err := osroot.ReadFile(again, "s.json")
	require.NoError(t, err)
	require.Equal(t, "ok", string(got))
}

func TestOpenChild_MissingDirectoryIsClassifiable(t *testing.T) {
	t.Parallel()

	root, err := os.OpenRoot(t.TempDir())
	require.NoError(t, err)
	defer root.Close()

	_, err = osroot.OpenChild(root, "entire-sessions")
	require.True(t, os.IsNotExist(err), "want a not-exist error, got %v", err)
}

// fs.WalkDir takes its walk ROOT's DirEntry from fs.Stat, which follows a
// symlink; only entries beneath it come from ReadDir, which does not. Every
// callback in this codebase inspected d.Type() for ModeSymlink and so covered
// the entries but never the root. filepath.Walk, which these walks were
// converted from, did lstat its root.
func TestWalkDirNoSymlinks_RefusesSymlinkedWalkRoot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "real"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "real", "secret.txt"), []byte("leak"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "meta"), 0o750))
	if err := os.Symlink("../real", filepath.Join(dir, "meta", "sess")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()

	// Plain fs.WalkDir descends into it. This is the behaviour being guarded
	// against, asserted so the test fails loudly if the stdlib ever changes.
	var followed []string
	require.NoError(t, fs.WalkDir(root.FS(), "meta/sess", func(name string, _ fs.DirEntry, err error) error {
		if err == nil {
			followed = append(followed, name)
		}
		return err
	}))
	require.Contains(t, followed, "meta/sess/secret.txt", "fs.WalkDir no longer follows a symlinked walk root; simplify accordingly")

	var visited []string
	err = osroot.WalkDirNoSymlinks(root, "meta/sess", func(name string, _ fs.DirEntry, err error) error {
		visited = append(visited, name)
		return err
	})
	require.ErrorIs(t, err, osroot.ErrSymlinkedPath)
	require.Empty(t, visited, "the callback must not run at all for a symlinked walk root")
}

func TestWalkDirNoSymlinks_RefusesSymlinkedEntry(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "meta"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "outside.txt"), []byte("leak"), 0o600))
	if err := os.Symlink("../outside.txt", filepath.Join(dir, "meta", "link.txt")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()

	err = osroot.WalkDirNoSymlinks(root, "meta", func(_ string, _ fs.DirEntry, err error) error { return err })
	require.ErrorIs(t, err, osroot.ErrSymlinkedPath)
}

func TestWalkDirNoSymlinks_NonDirectoryRootIsNotReportedAsASymlink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "meta"), []byte("regular file"), 0o600))
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()

	err = osroot.WalkDirNoSymlinks(root, "meta", func(_ string, _ fs.DirEntry, err error) error { return err })
	require.ErrorIs(t, err, osroot.ErrWalkRootNotDirectory)
	require.NotErrorIs(t, err, osroot.ErrSymlinkedPath, "'replace this link' and 'this is a file' are different remedies")
}

func TestSymlinkPaths_ReportsASymlinkedWalkRootRatherThanDescending(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "real"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "real", "a.txt"), []byte("x"), 0o600))
	if err := os.Symlink("real", filepath.Join(dir, "logs")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()

	found, err := osroot.SymlinkPaths(root, "logs")
	require.NoError(t, err)
	require.Equal(t, []string{"logs"}, found)
}
