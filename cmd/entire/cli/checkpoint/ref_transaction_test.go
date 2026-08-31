package checkpoint

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

func TestCompareAndSwapRef_CreateUpdateAndConflict(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "initial.txt", "initial")
	testutil.GitAdd(t, dir, "initial.txt")
	testutil.GitCommit(t, dir, "initial")
	repo, err := gitrepo.OpenPath(dir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("read HEAD: %v", err)
	}
	refName := plumbing.ReferenceName("refs/entire/checkpoints/01/example")

	if err := CompareAndSwapRef(t.Context(), repo, refName, head.Hash(), plumbing.ZeroHash); err != nil {
		t.Fatalf("create ref: %v", err)
	}
	if err := CompareAndSwapRef(t.Context(), repo, refName, head.Hash(), plumbing.ZeroHash); !errors.Is(err, ErrRefConflict) {
		t.Fatalf("second create error = %v, want ErrRefConflict", err)
	}

	otherName := plumbing.ReferenceName("refs/entire/test-target")
	if err := CompareAndSwapRef(t.Context(), repo, otherName, head.Hash(), plumbing.ZeroHash); err != nil {
		t.Fatalf("create second ref: %v", err)
	}
	if err := CompareAndSwapRef(t.Context(), repo, refName, head.Hash(), head.Hash()); err != nil {
		t.Fatalf("idempotent hash update with matching expected tip: %v", err)
	}
}

func TestCompareAndSwapRef_CreateSHA256Ref(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	testutil.RunGit(t, dir, "init", "--object-format=sha256", ".")
	testutil.RunGit(t, dir, "config", "user.name", "Test User")
	testutil.RunGit(t, dir, "config", "user.email", "test@example.com")
	testutil.RunGit(t, dir, "config", "commit.gpgsign", "false")
	testutil.WriteFile(t, dir, "initial.txt", "initial")
	testutil.RunGit(t, dir, "add", "initial.txt")
	testutil.RunGit(t, dir, "commit", "--no-gpg-sign", "-m", "initial")
	repo, err := gitrepo.OpenPath(dir)
	if err != nil {
		t.Fatalf("open SHA-256 repo: %v", err)
	}
	defer repo.Close()
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("read HEAD: %v", err)
	}
	if got := head.Hash().HexSize(); got != 64 {
		t.Fatalf("HEAD hex width = %d, want 64", got)
	}
	refName := plumbing.ReferenceName("refs/entire/checkpoints/01/sha256")
	if err := CompareAndSwapRef(t.Context(), repo, refName, head.Hash(), plumbing.ZeroHash); err != nil {
		t.Fatalf("create SHA-256 ref: %v", err)
	}
}

func TestCompareAndSwapRefs_IsAtomic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "initial.txt", "initial")
	testutil.GitAdd(t, dir, "initial.txt")
	testutil.GitCommit(t, dir, "initial")
	repo, err := gitrepo.OpenPath(dir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()
	initial, err := repo.Head()
	if err != nil {
		t.Fatalf("read initial HEAD: %v", err)
	}
	testutil.WriteFile(t, dir, "next.txt", "next")
	testutil.GitAdd(t, dir, "next.txt")
	testutil.GitCommit(t, dir, "next")
	next, err := repo.Head()
	if err != nil {
		t.Fatalf("read next HEAD: %v", err)
	}

	first := plumbing.ReferenceName("refs/entire/atomic/first")
	second := plumbing.ReferenceName("refs/entire/atomic/second")
	if err := CompareAndSwapRefs(t.Context(), repo, []RefUpdate{
		{Ref: first, New: initial.Hash(), Expected: plumbing.ZeroHash},
		{Ref: second, New: initial.Hash(), Expected: plumbing.ZeroHash},
	}); err != nil {
		t.Fatalf("create refs: %v", err)
	}

	err = CompareAndSwapRefs(t.Context(), repo, []RefUpdate{
		{Ref: first, New: next.Hash(), Expected: initial.Hash()},
		{Ref: second, New: next.Hash(), Expected: plumbing.ZeroHash},
	})
	if !errors.Is(err, ErrRefConflict) {
		t.Fatalf("atomic conflict error = %v, want ErrRefConflict", err)
	}
	firstAfter, err := ReadRefHash(repo, first)
	if err != nil {
		t.Fatalf("read first ref: %v", err)
	}
	secondAfter, err := ReadRefHash(repo, second)
	if err != nil {
		t.Fatalf("read second ref: %v", err)
	}
	if firstAfter != initial.Hash() || secondAfter != initial.Hash() {
		t.Fatalf("partial update: first=%s second=%s, want both %s", firstAfter, secondAfter, initial.Hash())
	}
}

func TestRunRefTransaction_RebuildsAfterConflict(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "initial.txt", "initial")
	testutil.GitAdd(t, dir, "initial.txt")
	testutil.GitCommit(t, dir, "initial")
	repo, err := gitrepo.OpenPath(dir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("read HEAD: %v", err)
	}
	refName := plumbing.ReferenceName("refs/entire/checkpoints/01/retry")
	calls := 0
	got, err := RunRefTransaction(t.Context(), repo, refName, func(current plumbing.Hash) (plumbing.Hash, bool, error) {
		calls++
		if calls == 1 {
			if err := CompareAndSwapRef(t.Context(), repo, refName, head.Hash(), plumbing.ZeroHash); err != nil {
				return plumbing.ZeroHash, false, err
			}
			return head.Hash(), true, nil
		}
		if current != head.Hash() {
			t.Fatalf("retry current = %s, want %s", current, head.Hash())
		}
		return current, false, nil
	})
	if err != nil {
		t.Fatalf("RunRefTransaction: %v", err)
	}
	if got != head.Hash() {
		t.Fatalf("result = %s, want %s", got, head.Hash())
	}
	if calls != 2 {
		t.Fatalf("mutation calls = %d, want 2", calls)
	}
}

func TestMoveRefIfUnchanged_CreatesDestinationAndDeletesSource(t *testing.T) {
	t.Parallel()
	repo, hash := setupMoveRefRepo(t)
	source := plumbing.ReferenceName("refs/heads/entire/source")
	destination := plumbing.ReferenceName("refs/heads/entire/destination")

	if err := CompareAndSwapRef(t.Context(), repo, source, hash, plumbing.ZeroHash); err != nil {
		t.Fatalf("create source: %v", err)
	}
	if err := MoveRefIfUnchanged(t.Context(), repo, source, destination, hash); err != nil {
		t.Fatalf("move refs: %v", err)
	}
	assertRefHash(t, repo, source, plumbing.ZeroHash)
	assertRefHash(t, repo, destination, hash)
}

func TestMoveRefIfUnchanged_ExistingDestinationIsIdempotent(t *testing.T) {
	t.Parallel()
	repo, hash := setupMoveRefRepo(t)
	source := plumbing.ReferenceName("refs/heads/entire/source")
	destination := plumbing.ReferenceName("refs/heads/entire/destination")

	if err := CompareAndSwapRef(t.Context(), repo, source, hash, plumbing.ZeroHash); err != nil {
		t.Fatalf("create source: %v", err)
	}
	if err := CompareAndSwapRef(t.Context(), repo, destination, hash, plumbing.ZeroHash); err != nil {
		t.Fatalf("create destination: %v", err)
	}
	if err := MoveRefIfUnchanged(t.Context(), repo, source, destination, hash); err != nil {
		t.Fatalf("idempotent move: %v", err)
	}
	assertRefHash(t, repo, source, plumbing.ZeroHash)
	assertRefHash(t, repo, destination, hash)
}

func TestMoveRefIfUnchanged_DifferentDestinationPreservesSource(t *testing.T) {
	t.Parallel()
	repo, hash := setupMoveRefRepo(t)
	otherHash := setupMoveRefRepoWithSecondCommit(t, repo)
	source := plumbing.ReferenceName("refs/heads/entire/source")
	destination := plumbing.ReferenceName("refs/heads/entire/destination")

	if err := CompareAndSwapRef(t.Context(), repo, source, hash, plumbing.ZeroHash); err != nil {
		t.Fatalf("create source: %v", err)
	}
	if err := CompareAndSwapRef(t.Context(), repo, destination, otherHash, plumbing.ZeroHash); err != nil {
		t.Fatalf("create destination: %v", err)
	}
	if err := MoveRefIfUnchanged(t.Context(), repo, source, destination, hash); !errors.Is(err, ErrRefMoveConflict) {
		t.Fatalf("move error = %v, want ErrRefMoveConflict", err)
	}
	assertRefHash(t, repo, source, hash)
	assertRefHash(t, repo, destination, otherHash)
}

func TestMoveRefIfUnchanged_MovedSourceChangesNeitherRef(t *testing.T) {
	t.Parallel()
	repo, hash := setupMoveRefRepo(t)
	otherHash := setupMoveRefRepoWithSecondCommit(t, repo)
	source := plumbing.ReferenceName("refs/heads/entire/source")
	destination := plumbing.ReferenceName("refs/heads/entire/destination")

	if err := CompareAndSwapRef(t.Context(), repo, source, otherHash, plumbing.ZeroHash); err != nil {
		t.Fatalf("create source: %v", err)
	}
	if err := MoveRefIfUnchanged(t.Context(), repo, source, destination, hash); !errors.Is(err, ErrRefMoveConflict) {
		t.Fatalf("move error = %v, want ErrRefMoveConflict", err)
	}
	assertRefHash(t, repo, source, otherHash)
	assertRefHash(t, repo, destination, plumbing.ZeroHash)
}

func TestMoveRefIfUnchanged_SourceAdvancesAfterObservationChangesNeitherRef(t *testing.T) {
	t.Parallel()
	repo, hash := setupMoveRefRepo(t)
	otherHash := setupMoveRefRepoWithSecondCommit(t, repo)
	source := plumbing.ReferenceName("refs/heads/entire/source")
	destination := plumbing.ReferenceName("refs/heads/entire/destination")

	if err := CompareAndSwapRef(t.Context(), repo, source, hash, plumbing.ZeroHash); err != nil {
		t.Fatalf("create source: %v", err)
	}
	ctx := withBeforeRefCAS(t.Context(), func() {
		if err := CompareAndSwapRef(context.Background(), repo, source, otherHash, hash); err != nil {
			t.Fatalf("advance source: %v", err)
		}
	})
	if err := MoveRefIfUnchanged(ctx, repo, source, destination, hash); !errors.Is(err, ErrRefMoveConflict) {
		t.Fatalf("move error = %v, want ErrRefMoveConflict", err)
	}
	assertRefHash(t, repo, source, otherHash)
	assertRefHash(t, repo, destination, plumbing.ZeroHash)
}

func TestMoveRefIfUnchanged_MissingSourceWithMatchingDestinationIsIdempotent(t *testing.T) {
	t.Parallel()
	repo, hash := setupMoveRefRepo(t)
	source := plumbing.ReferenceName("refs/heads/entire/source")
	destination := plumbing.ReferenceName("refs/heads/entire/destination")

	if err := CompareAndSwapRef(t.Context(), repo, destination, hash, plumbing.ZeroHash); err != nil {
		t.Fatalf("create destination: %v", err)
	}
	if err := MoveRefIfUnchanged(t.Context(), repo, source, destination, hash); err != nil {
		t.Fatalf("idempotent missing-source move: %v", err)
	}
	assertRefHash(t, repo, source, plumbing.ZeroHash)
	assertRefHash(t, repo, destination, hash)
}

func TestMoveShadowBranch_WaitsForQueuedWriterBeforeCheckingSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "initial.txt", "initial")
	testutil.GitAdd(t, dir, "initial.txt")
	testutil.GitCommit(t, dir, "initial")
	oldBase := testutil.GetHeadHash(t, dir)

	repo, err := gitrepo.OpenPath(dir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()
	store := newEphemeralStore(repo, DefaultV1Refs())
	oldBranch := ShadowBranchNameForCommit(oldBase, "")

	writerAtCAS := make(chan struct{})
	releaseWriter := make(chan struct{})
	var releaseWriterOnce sync.Once
	releaseQueuedWriter := func() {
		releaseWriterOnce.Do(func() { close(releaseWriter) })
	}
	defer releaseQueuedWriter()
	writerDone := make(chan struct {
		result WriteEphemeralResult
		err    error
	}, 1)
	writerCtx := withBeforeRefCAS(t.Context(), func() {
		close(writerAtCAS)
		<-releaseWriter
	})
	go func() {
		result, writeErr := store.Write(writerCtx, Step{
			SessionID:     "queued-writer",
			BaseCommit:    oldBase,
			CommitMessage: "queued checkpoint",
			AuthorName:    "Test",
			AuthorEmail:   "test@example.com",
		})
		writerDone <- struct {
			result WriteEphemeralResult
			err    error
		}{result: result, err: writeErr}
	}()

	select {
	case <-writerAtCAS:
	case <-time.After(5 * time.Second):
		t.Fatal("queued writer did not reach ref publication")
	}

	testutil.WriteFile(t, dir, "advanced.txt", "advanced")
	testutil.GitAdd(t, dir, "advanced.txt")
	testutil.GitCommit(t, dir, "advance HEAD")
	newBase := testutil.GetHeadHash(t, dir)
	newBranch := ShadowBranchNameForCommit(newBase, "")

	moveDone := make(chan struct {
		moved bool
		err   error
	}, 1)
	go func() {
		moved, moveErr := MoveShadowBranch(t.Context(), repo, oldBranch, newBranch)
		moveDone <- struct {
			moved bool
			err   error
		}{moved: moved, err: moveErr}
	}()

	select {
	case result := <-moveDone:
		t.Fatalf("migration completed before queued writer published: moved=%v err=%v", result.moved, result.err)
	case <-time.After(100 * time.Millisecond):
	}

	releaseQueuedWriter()
	var writeResult struct {
		result WriteEphemeralResult
		err    error
	}
	select {
	case writeResult = <-writerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("queued writer did not finish")
	}
	if writeResult.err != nil {
		t.Fatalf("queued writer: %v", writeResult.err)
	}
	var moveResult struct {
		moved bool
		err   error
	}
	select {
	case moveResult = <-moveDone:
	case <-time.After(5 * time.Second):
		t.Fatal("shadow branch migration did not finish")
	}
	if moveResult.err != nil {
		t.Fatalf("move shadow branch: %v", moveResult.err)
	}
	if !moveResult.moved {
		t.Fatal("move shadow branch reported no source after waiting for queued writer")
	}

	assertRefHash(t, repo, plumbing.NewBranchReferenceName(oldBranch), plumbing.ZeroHash)
	assertRefHash(t, repo, plumbing.NewBranchReferenceName(newBranch), writeResult.result.CommitHash)
}

func setupMoveRefRepo(t *testing.T) (*git.Repository, plumbing.Hash) {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "initial.txt", "initial")
	testutil.GitAdd(t, dir, "initial.txt")
	testutil.GitCommit(t, dir, "initial")
	repo, err := gitrepo.OpenPath(dir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("read HEAD: %v", err)
	}
	return repo, head.Hash()
}

func setupMoveRefRepoWithSecondCommit(t *testing.T, repo *git.Repository) plumbing.Hash {
	t.Helper()
	_, err := repo.Head()
	if err != nil {
		t.Fatalf("read initial HEAD: %v", err)
	}
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("open worktree: %v", err)
	}
	dir := worktree.Filesystem().Root()
	testutil.WriteFile(t, dir, "second.txt", "second")
	testutil.GitAdd(t, dir, "second.txt")
	testutil.GitCommit(t, dir, "second")
	second, err := repo.Head()
	if err != nil {
		t.Fatalf("read second HEAD: %v", err)
	}
	return second.Hash()
}

func assertRefHash(t *testing.T, repo *git.Repository, refName plumbing.ReferenceName, want plumbing.Hash) {
	t.Helper()
	got, err := ReadRefHash(repo, refName)
	if err != nil {
		t.Fatalf("read %s: %v", refName, err)
	}
	if got != want {
		t.Fatalf("%s = %s, want %s", refName, got, want)
	}
}
