package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/stretchr/testify/require"
)

// A repository can ship a symlink at .claude, because the working tree arrives
// by clone. Before this was anchored, writeManagedScaffold did os.MkdirAll on
// two levels below it and os.WriteFile through it, landing the file wherever the
// link pointed and reporting Created.
func TestWriteManagedScaffold_RefusesASymlinkedAgentDirectory(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, target string }{
		{"pointing outside the worktree", ""},
		{"pointing elsewhere inside the worktree", "vendor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			worktree := t.TempDir()

			var dest string
			if tc.target == "" {
				dest = t.TempDir()
			} else {
				dest = filepath.Join(worktree, tc.target)
				require.NoError(t, os.MkdirAll(dest, 0o750))
			}
			link := filepath.Join(worktree, ".claude")
			if err := os.Symlink(dest, link); err != nil {
				t.Skipf("symlink not supported: %v", err)
			}

			rel := filepath.Join(".claude", "skills", "entire", "SKILL.md")
			_, err := writeManagedScaffold(worktree, rel, []byte("managed\n"), func([]byte) bool { return true })
			require.ErrorIs(t, err, osroot.ErrSymlinkedPath)

			entries, readErr := os.ReadDir(dest)
			require.NoError(t, readErr)
			require.Empty(t, entries, "nothing may be created through the link")
			info, lerr := os.Lstat(link)
			require.NoError(t, lerr)
			require.NotZero(t, info.Mode()&os.ModeSymlink, "the link itself must be left alone")
		})
	}
}

// The ordinary path still works, including creating the nested directories.
func TestWriteManagedScaffold_CreatesThroughARealDirectory(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	rel := filepath.Join(".claude", "skills", "entire", "SKILL.md")

	res, err := writeManagedScaffold(worktree, rel, []byte("managed\n"), func([]byte) bool { return true })
	require.NoError(t, err)
	require.Equal(t, managedScaffoldCreated, res.Status)

	got, err := os.ReadFile(filepath.Join(worktree, rel))
	require.NoError(t, err)
	require.Equal(t, "managed\n", string(got))

	res, err = writeManagedScaffold(worktree, rel, []byte("managed\n"), func([]byte) bool { return true })
	require.NoError(t, err)
	require.Equal(t, managedScaffoldUnchanged, res.Status)

	res, err = writeManagedScaffold(worktree, rel, []byte("changed\n"), func([]byte) bool { return false })
	require.NoError(t, err)
	require.Equal(t, managedScaffoldSkippedConflict, res.Status)
}
