package gitrepo

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// TestStatus_SkipsNestedCheckouts pins the repository-boundary rule: untracked
// files inside a nested git checkout belong to that checkout, not to this one.
// git's own status never descends into a directory containing a .git entry;
// go-git's walk does, so Status filters what the walk over-reports. Tracked
// entries survive the filter wherever they sit, matching git.
func TestStatus_SkipsNestedCheckouts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	initRepoWithFile(t, dir, "tracked.txt", "initial")

	// Commit a file under vendor/lib BEFORE it becomes a nested checkout, so
	// the filter can be shown to keep tracked entries inside such a directory.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "vendor", "lib"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "vendor", "lib", "kept.txt"), []byte("v1"), 0o600))
	commitAll(t, dir, "add vendor/lib/kept.txt")

	// vendor/lib becomes a nested clone: .git DIRECTORY plus untracked files,
	// one shallow and one deep.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "vendor", "lib", ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "vendor", "lib", "inner.go"), []byte("x"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "vendor", "lib", "src", "deep"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "vendor", "lib", "src", "deep", "leaf.go"), []byte("x"), 0o600))

	// worktrees/agent-1 is a linked worktree: .git is a FILE pointing at the
	// real gitdir elsewhere.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "worktrees", "agent-1"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "worktrees", "agent-1", ".git"), []byte("gitdir: /elsewhere/.git/worktrees/agent-1\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "worktrees", "agent-1", "task.md"), []byte("x"), 0o600))

	// This repository's own new files: at the root and in a plain subdirectory.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "new.txt"), []byte("x"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "src"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src", "feature.go"), []byte("x"), 0o600))

	// Modify the tracked file that now sits inside the nested checkout.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "vendor", "lib", "kept.txt"), []byte("v2"), 0o600))

	repo, err := OpenPath(dir)
	require.NoError(t, err)
	defer repo.Close()

	status, err := Status(context.Background(), repo)
	require.NoError(t, err)

	// Own untracked files are reported.
	require.Contains(t, status, "new.txt")
	require.Contains(t, status, "src/feature.go")

	// Untracked files inside nested checkouts are not, whether the boundary is
	// a .git directory or a .git file, at any depth.
	require.NotContains(t, status, "vendor/lib/inner.go")
	require.NotContains(t, status, "vendor/lib/src/deep/leaf.go")
	require.NotContains(t, status, "worktrees/agent-1/task.md")

	// Tracked entries survive the filter even inside a nested checkout.
	require.Contains(t, status, "vendor/lib/kept.txt")
	require.Equal(t, git.Modified, status["vendor/lib/kept.txt"].Worktree)
}

// commitAll stages and commits every pending change in dir.
func commitAll(t *testing.T, dir, message string) {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	defer repo.Close()

	worktree, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, worktree.AddWithOptions(&git.AddOptions{All: true}))
	_, err = worktree.Commit(message, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	require.NoError(t, err)
}
