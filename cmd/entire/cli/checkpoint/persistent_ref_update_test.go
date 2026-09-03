package checkpoint

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

func TestPersistentRefLock_RejectsSymlinkedLockFile(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == windowsOS {
		t.Skip("symlink creation requires privileges on some Windows builders")
	}

	repo, _ := setupBranchTestRepo(t)
	refName := plumbing.ReferenceName("refs/entire/test-symlink")
	_, commonDir, err := repositoryDirs(context.Background(), repo)
	require.NoError(t, err)
	_, lockName, err := persistentRefLock(commonDir, refName)
	require.NoError(t, err)
	target := filepath.Join(commonDir, "config")
	before, err := os.ReadFile(target)
	require.NoError(t, err)
	require.NoError(t, os.Symlink("../config", filepath.Join(commonDir, filepath.FromSlash(lockName))))

	err = withPersistentRefFlock(context.Background(), commonDir, refName, func() error {
		return nil
	})
	require.ErrorIs(t, err, osroot.ErrSymlinkedPath)
	after, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestUpdatePersistentRef_StopsWaitingWhenContextDeadlineExpires(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	refName := plumbing.ReferenceName("refs/entire/test-deadline")

	_, commonDir, err := repositoryDirs(context.Background(), repo)
	require.NoError(t, err)
	root, lockName, err := persistentRefLock(commonDir, refName)
	require.NoError(t, err)
	release, err := flock.AcquireIn(root, lockName)
	require.NoError(t, err)
	t.Cleanup(release)

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	buildCalled := false
	err = updatePersistentRef(ctx, repo, refName, func() (plumbing.Hash, plumbing.Hash, error) {
		buildCalled = true
		return plumbing.ZeroHash, plumbing.ZeroHash, nil
	})

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.False(t, buildCalled, "the ref builder must not run without the lock")
}

func TestUpdatePersistentRef_RebuildsAfterCASConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, initial := setupBranchTestRepo(t)
	refName := plumbing.ReferenceName("refs/entire/test-cas")
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, initial)))
	repoRoot, _, err := repositoryDirs(ctx, repo)
	require.NoError(t, err)

	var (
		calls       int
		seenParents []plumbing.Hash
		externalTip plumbing.Hash
	)
	err = updatePersistentRef(ctx, repo, refName, func() (plumbing.Hash, plumbing.Hash, error) {
		calls++
		ref, err := repo.Reference(refName, true)
		if err != nil {
			return plumbing.ZeroHash, plumbing.ZeroHash, err
		}
		parent := ref.Hash()
		seenParents = append(seenParents, parent)
		commit, err := repo.CommitObject(parent)
		if err != nil {
			return plumbing.ZeroHash, plumbing.ZeroHash, err
		}
		marker := fmt.Sprintf("attempt-%d", calls)
		blob, err := CreateBlobFromContent(repo, []byte(marker))
		if err != nil {
			return plumbing.ZeroHash, plumbing.ZeroHash, err
		}
		tree, err := ApplyTreeChanges(ctx, repo, commit.TreeHash, []TreeChange{{
			Path:  "cas-marker.txt",
			Entry: &object.TreeEntry{Name: "cas-marker.txt", Mode: filemode.Regular, Hash: blob},
		}})
		if err != nil {
			return plumbing.ZeroHash, plumbing.ZeroHash, err
		}
		newCommit, err := CreateCommit(ctx, repo, tree, parent, marker, "Test", "test@test.com")
		if err != nil {
			return plumbing.ZeroHash, plumbing.ZeroHash, err
		}

		if calls == 1 {
			externalTip, err = CreateCommit(ctx, repo, commit.TreeHash, parent, "external writer", "Test", "test@test.com")
			if err != nil {
				return plumbing.ZeroHash, plumbing.ZeroHash, err
			}
			if err := casUpdateRef(ctx, repoRoot, refName, externalTip, parent); err != nil {
				return plumbing.ZeroHash, plumbing.ZeroHash, err
			}
		}
		return newCommit, parent, nil
	})
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	require.Equal(t, []plumbing.Hash{initial, externalTip}, seenParents)

	ref, err := repo.Reference(refName, true)
	require.NoError(t, err)
	finalCommit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	require.Equal(t, externalTip, finalCommit.ParentHashes[0])
	finalTree, err := finalCommit.Tree()
	require.NoError(t, err)
	file, err := finalTree.File("cas-marker.txt")
	require.NoError(t, err)
	contents, err := file.Contents()
	require.NoError(t, err)
	require.Equal(t, "attempt-2", contents)
}

func TestUpdatePersistentRef_NoOpConflictKeepsExistingCommit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, initial := setupBranchTestRepo(t)
	refName := plumbing.ReferenceName("refs/entire/test-cas-noop")
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, initial)))
	repoRoot, _, err := repositoryDirs(ctx, repo)
	require.NoError(t, err)

	calls := 0
	err = updatePersistentRef(ctx, repo, refName, func() (plumbing.Hash, plumbing.Hash, error) {
		calls++
		ref, err := repo.Reference(refName, true)
		if err != nil {
			return plumbing.ZeroHash, plumbing.ZeroHash, err
		}
		current := ref.Hash()
		if calls == 1 {
			commit, err := repo.CommitObject(current)
			if err != nil {
				return plumbing.ZeroHash, plumbing.ZeroHash, err
			}
			external, err := CreateCommit(ctx, repo, commit.TreeHash, current, "external writer", "Test", "test@test.com")
			if err != nil {
				return plumbing.ZeroHash, plumbing.ZeroHash, err
			}
			if err := casUpdateRef(ctx, repoRoot, refName, external, current); err != nil {
				return plumbing.ZeroHash, plumbing.ZeroHash, err
			}
		}
		return current, current, nil
	})
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	_, err = repo.CommitObject(initial)
	require.NoError(t, err, "CAS cleanup must not delete an existing no-op target")
}
