package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
)

func TestUpdatePersistentRef_StopsWaitingWhenContextDeadlineExpires(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	refName := plumbing.ReferenceName("refs/entire/test-deadline")
	holdPersistentRefLock(t, repo, refName)

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	buildCalled := false
	err := updatePersistentRef(ctx, repo, refName, func() (plumbing.Hash, plumbing.Hash, error) {
		buildCalled = true
		return plumbing.ZeroHash, plumbing.ZeroHash, nil
	})

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.False(t, buildCalled, "the ref builder must not run without the lock")
}

func TestCASPersistentRef_StopsWaitingWhenContextDeadlineExpires(t *testing.T) {
	t.Parallel()
	repo, initial := setupBranchTestRepo(t)
	refName := plumbing.ReferenceName("refs/entire/test-cas-deadline")
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, initial)))
	holdPersistentRefLock(t, repo, refName)

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	err := CASPersistentRef(ctx, repo, refName, plumbing.ZeroHash, initial)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	ref, err := repo.Reference(refName, true)
	require.NoError(t, err)
	require.Equal(t, initial, ref.Hash())
}

func TestCASPersistentRef_DoesNotWaitForPersistentRefFlock(t *testing.T) {
	t.Parallel()
	repo, initial := setupBranchTestRepo(t)
	refName := plumbing.ReferenceName("refs/entire/test-cas-wait")
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, initial)))
	initialCommit, err := repo.CommitObject(initial)
	require.NoError(t, err)
	replacement, err := CreateCommit(
		t.Context(), repo, initialCommit.TreeHash, initial, "replacement", "Test", "test@test.com",
	)
	require.NoError(t, err)
	_, commonDir, err := repositoryDirs(repo)
	require.NoError(t, err)
	lockRoot, lockName, err := persistentRefLock(commonDir, refName)
	require.NoError(t, err)
	releaseHolder, err := flock.AcquireIn(lockRoot, lockName)
	require.NoError(t, err)
	released := false
	t.Cleanup(func() {
		if !released {
			releaseHolder()
		}
	})

	result := make(chan error, 1)
	go func() {
		result <- CASPersistentRef(context.Background(), repo, refName, replacement, initial)
	}()

	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		releaseHolder()
		released = true
		err := <-result
		t.Fatalf("CAS waited for the persistent writer flock: %v", err)
	}

	ref, err := repo.Reference(refName, true)
	require.NoError(t, err)
	require.Equal(t, replacement, ref.Hash())
	releaseHolder()
	released = true
}

func TestCASPersistentRef_RejectsSymbolicRef(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, initial := setupBranchTestRepo(t)
	head, err := repo.Head()
	require.NoError(t, err)
	initialCommit, err := repo.CommitObject(initial)
	require.NoError(t, err)
	newTip, err := CreateCommit(ctx, repo, initialCommit.TreeHash, initial, "replacement", "Test", "test@test.com")
	require.NoError(t, err)

	refName := plumbing.ReferenceName("refs/entire/test-symbolic-cas")
	require.NoError(t, repo.Storer.SetReference(plumbing.NewSymbolicReference(refName, head.Name())))

	err = CASPersistentRef(ctx, repo, refName, newTip, initial)
	require.ErrorIs(t, err, gitrepo.ErrRefSymbolic)

	target, err := repo.Reference(head.Name(), false)
	require.NoError(t, err)
	require.Equal(t, initial, target.Hash(), "CAS must not update the target of a symbolic ref")
	updated, err := repo.Reference(refName, false)
	require.NoError(t, err)
	require.Equal(t, plumbing.SymbolicReference, updated.Type())
	require.Equal(t, head.Name(), updated.Target(), "CAS must leave the symbolic ref intact")
}

func TestCASPersistentRef_DoesNotClassifyRefNamespaceConflictAsContention(t *testing.T) {
	t.Parallel()
	repo, initial := setupBranchTestRepo(t)
	parentRef := plumbing.ReferenceName("refs/entire/blocked")
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(parentRef, initial)))

	err := CASPersistentRef(
		context.Background(),
		repo,
		plumbing.ReferenceName(parentRef.String()+"/child"),
		initial,
		plumbing.ZeroHash,
	)

	require.Error(t, err)
	require.NotErrorIs(t, err, gitrepo.ErrRefCASConflict,
		"a permanent ref namespace conflict must not be reported as a stale value")
	require.NotErrorIs(t, err, gitrepo.ErrRefLocked,
		"a permanent ref namespace conflict must not be reported as transient contention")
	require.Contains(t, err.Error(), "cannot lock ref")
}

func TestRetryPersistentRefLockContention_RetriesOnlyLockErrors(t *testing.T) {
	t.Parallel()
	refName := plumbing.ReferenceName("refs/entire/test-opf-lock-retry")

	t.Run("lock contention is retried", func(t *testing.T) {
		t.Parallel()
		attempts := 0
		err := retryPersistentRefLockContention(t.Context(), refName, func() error {
			attempts++
			if attempts < 3 {
				return fmt.Errorf("attempt %d: %w", attempts, gitrepo.ErrRefLocked)
			}
			return nil
		})

		require.NoError(t, err)
		require.Equal(t, 3, attempts)
	})

	t.Run("CAS conflict is terminal", func(t *testing.T) {
		t.Parallel()
		attempts := 0
		err := retryPersistentRefLockContention(t.Context(), refName, func() error {
			attempts++
			return fmt.Errorf("stale rewrite: %w", gitrepo.ErrRefCASConflict)
		})

		require.ErrorIs(t, err, gitrepo.ErrRefCASConflict)
		require.Equal(t, 1, attempts)
	})

	t.Run("symbolic rejection takes precedence over abort lock error", func(t *testing.T) {
		t.Parallel()
		attempts := 0
		err := retryPersistentRefLockContention(t.Context(), refName, func() error {
			attempts++
			return errors.Join(gitrepo.ErrRefSymbolic, gitrepo.ErrRefLocked)
		})

		require.ErrorIs(t, err, gitrepo.ErrRefSymbolic)
		require.ErrorIs(t, err, gitrepo.ErrRefLocked)
		require.Equal(t, 1, attempts)
	})

	t.Run("lock retry budget is bounded", func(t *testing.T) {
		t.Parallel()
		attempts := 0
		err := retryPersistentRefLockContention(t.Context(), refName, func() error {
			attempts++
			return fmt.Errorf("still locked: %w", gitrepo.ErrRefLocked)
		})

		require.ErrorIs(t, err, gitrepo.ErrRefLocked)
		require.Equal(t, shadowRefMaxRetries, attempts)
	})
}

func holdPersistentRefLock(t *testing.T, repo *git.Repository, refName plumbing.ReferenceName) {
	t.Helper()
	_, commonDir, err := repositoryDirs(repo)
	require.NoError(t, err)
	lockRoot, lockName, err := persistentRefLock(commonDir, refName)
	require.NoError(t, err)
	release, err := flock.AcquireIn(lockRoot, lockName)
	require.NoError(t, err)
	t.Cleanup(release)
}

func TestUpdatePersistentRef_RebuildsAfterCASConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, initial := setupBranchTestRepo(t)
	refName := plumbing.ReferenceName("refs/entire/test-cas")
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, initial)))
	repoRoot, _, err := repositoryDirs(repo)
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

func TestUpdatePersistentRef_RebuildsAfterRefIsDeleted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, initial := setupBranchTestRepo(t)
	refName := plumbing.ReferenceName("refs/entire/test-cas-deleted")
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, initial)))

	calls := 0
	err := updatePersistentRef(ctx, repo, refName, func() (plumbing.Hash, plumbing.Hash, error) {
		calls++
		if calls == 1 {
			require.NoError(t, repo.Storer.RemoveReference(refName))
			return initial, initial, nil
		}
		return initial, plumbing.ZeroHash, nil
	})

	require.NoError(t, err)
	require.Equal(t, 2, calls, "the writer must rebuild after its expected ref is deleted")
	ref, err := repo.Reference(refName, true)
	require.NoError(t, err)
	require.Equal(t, initial, ref.Hash())
}

func TestUpdatePersistentRef_DoesNotRetryDirectoryAtRefPath(t *testing.T) {
	t.Parallel()
	repo, initial := setupBranchTestRepo(t)
	_, commonDir, err := repositoryDirs(repo)
	require.NoError(t, err)
	refName := plumbing.ReferenceName("refs/entire/directory-ref")
	refPath := filepath.Join(commonDir, filepath.FromSlash(refName.String()))
	require.NoError(t, os.MkdirAll(refPath, 0o755))

	builds := 0
	err = updatePersistentRef(t.Context(), repo, refName, func() (plumbing.Hash, plumbing.Hash, error) {
		builds++
		return initial, initial, nil
	})

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrShadowRefBusy)
	require.NotErrorIs(t, err, gitrepo.ErrRefCASConflict)
	require.Equal(t, 1, builds, "a directory cannot be repaired by rebuilding checkpoint commits")
	info, statErr := os.Stat(refPath)
	require.NoError(t, statErr)
	require.True(t, info.IsDir())
}

func TestUpdatePersistentRef_NoOpConflictKeepsExistingCommit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, initial := setupBranchTestRepo(t)
	refName := plumbing.ReferenceName("refs/entire/test-cas-noop")
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, initial)))
	repoRoot, _, err := repositoryDirs(repo)
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
