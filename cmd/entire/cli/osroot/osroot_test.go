package osroot_test

import (
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
