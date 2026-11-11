package strategy

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
)

// Pruning drops an unregistered checkout's record only when resolving the
// recorded path gives an answer (gone, or not a checkout); a failure to read
// the metadata says nothing about the record, so it is kept.
func TestMetadataReadFailed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	noGit := filepath.Join(dir, "no-git")
	require.NoError(t, os.MkdirAll(noGit, 0o750))
	badPointer := filepath.Join(dir, "bad-pointer")
	require.NoError(t, os.MkdirAll(badPointer, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(badPointer, ".git"), []byte("not a pointer\n"), 0o600))

	for _, tc := range []struct {
		name string
		path string
	}{
		{name: "removed", path: filepath.Join(dir, "gone")},
		{name: "a file in the path", path: filepath.Join(file, "checkout")},
		{name: "no .git entry", path: noGit},
		{name: "unparseable .git file", path: badPointer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := gitrepo.ResolveWorktreeMetadata(tc.path)
			require.Error(t, err)
			assert.False(t, metadataReadFailed(err), "%v reads as a failed check, so the record would never be pruned", err)
		})
	}

	t.Run("permission or I/O failure", func(t *testing.T) {
		t.Parallel()
		for _, errno := range []syscall.Errno{syscall.EACCES, syscall.EIO} {
			err := fmt.Errorf("inspect worktree root: %w", &fs.PathError{Op: "stat", Path: "/x", Err: errno})
			assert.True(t, metadataReadFailed(err), "%v reads as a removed checkout", err)
		}
	})
}
