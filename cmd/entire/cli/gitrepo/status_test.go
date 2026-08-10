package gitrepo

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6"
)

func TestStatus_Cache(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		newContext    func() context.Context
		wantSeesWrite bool
	}{
		{
			name:          "without cache every call re-reads the worktree",
			newContext:    context.Background,
			wantSeesWrite: true,
		},
		{
			name:          "with cache the first result is reused",
			newContext:    func() context.Context { return WithStatusCache(context.Background()) },
			wantSeesWrite: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			initRepoWithFile(t, dir, "tracked.txt", "initial")

			repo, err := OpenPath(dir)
			require.NoError(t, err)
			defer repo.Close()

			ctx := tt.newContext()

			_, err = Status(ctx, repo)
			require.NoError(t, err)

			// Writing after the first read is what distinguishes a reused result
			// from a fresh walk: only a fresh walk reports new.txt.
			require.NoError(t, os.WriteFile(filepath.Join(dir, "new.txt"), []byte("hello"), 0o600))

			status, err := Status(ctx, repo)
			require.NoError(t, err)

			entry, ok := status["new.txt"]
			if !tt.wantSeesWrite {
				require.False(t, ok, "second call should have reused the cached result")
				return
			}
			require.True(t, ok, "second call should have re-read the worktree")
			require.Equal(t, git.Untracked, entry.Worktree)
		})
	}
}

func TestStatus_CacheKeysPerWorktree(t *testing.T) {
	t.Parallel()

	dirA := t.TempDir()
	initRepoWithFile(t, dirA, "tracked.txt", "initial")

	dirB := t.TempDir()
	initRepoWithFile(t, dirB, "tracked.txt", "initial")
	require.NoError(t, os.WriteFile(filepath.Join(dirB, "only-in-b.txt"), []byte("hello"), 0o600))

	repoA, err := OpenPath(dirA)
	require.NoError(t, err)
	defer repoA.Close()

	repoB, err := OpenPath(dirB)
	require.NoError(t, err)
	defer repoB.Close()

	ctx := WithStatusCache(context.Background())

	statusA, err := Status(ctx, repoA)
	require.NoError(t, err)
	require.NotContains(t, statusA, "only-in-b.txt")

	statusB, err := Status(ctx, repoB)
	require.NoError(t, err)
	require.Contains(t, statusB, "only-in-b.txt",
		"a second worktree must not reuse the first worktree's cached entry")
}
