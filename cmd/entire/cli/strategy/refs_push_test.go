package strategy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// mustRefName builds a checkpoint ref for a known-valid ID in tests.
func mustRefName(t *testing.T, cid id.CheckpointID) plumbing.ReferenceName {
	t.Helper()
	ref, err := checkpoint.RefName(cid)
	require.NoError(t, err)
	return ref
}

// setupRepoWithCheckpointRefs creates a work repo with two per-checkpoint refs
// pointing at HEAD, plus a fresh bare remote. Returns (workDir, bareDir, refs).
func setupRepoWithCheckpointRefs(t *testing.T) (string, string, []plumbing.ReferenceName) {
	t.Helper()
	return setupRepoWithNCheckpointRefs(t, 2)
}

func TestPartitionLocalRefs(t *testing.T) {
	t.Parallel()
	workDir, _, refs := setupRepoWithCheckpointRefs(t)
	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)

	stale := mustRefName(t, id.MustCheckpointID("ffffffffffff"))
	existing, missing := partitionLocalRefs(repo, append([]plumbing.ReferenceName{stale}, refs...))

	assert.ElementsMatch(t, refs, existing, "local refs are pushable")
	assert.Equal(t, []plumbing.ReferenceName{stale}, missing, "absent ref is stale")
}

func TestBatchPushCheckpointRefs(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)

	require.NoError(t, pushCheckpointRefsAtTips(t, repo, bareDir, refs))

	// All refs now exist on the bare remote.
	lsCmd := exec.CommandContext(context.Background(), "git", "ls-remote", bareDir)
	lsCmd.Env = testutil.GitIsolatedEnv()
	out, err := lsCmd.CombinedOutput()
	require.NoError(t, err, "ls-remote failed: %s", out)
	remoteRefs := string(out)
	for _, ref := range refs {
		assert.Contains(t, remoteRefs, ref.String(), "ref should be present on the remote after batch push")
	}
}

func TestBatchPushCheckpointRefs_PinsVerifiedHash(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	refName := refs[0]
	verified, err := repo.Reference(refName, true)
	require.NoError(t, err)

	testutil.WriteFile(t, workDir, "later.txt", "later")
	testutil.GitAdd(t, workDir, "later.txt")
	testutil.GitCommit(t, workDir, "later")
	later, err := repo.Head()
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, later.Hash())))

	candidate := checkpointRefPush{
		token: checkpoint.PushQueueEntry{Ref: refName, Hash: verified.Hash()},
		name:  refName,
		hash:  verified.Hash(),
	}
	require.NoError(t, batchPushCheckpointRefs(t.Context(), bareDir, []checkpointRefPush{candidate}))

	assert.Equal(t, verified.Hash().String(), remoteRefHash(t, bareDir, refName),
		"the remote must receive the captured hash, not the ref's later tip")
	current, err := repo.Reference(refName, true)
	require.NoError(t, err)
	assert.Equal(t, later.Hash(), current.Hash(), "pushing the captured hash must not rewind the local ref")
}

func TestBatchPushCheckpointRefs_Empty(t *testing.T) {
	t.Parallel()
	// No refs → no git invocation, no error.
	require.NoError(t, batchPushCheckpointRefs(context.Background(), "unused-target", nil))
}

// TestBatchPushCheckpointRefs_AllowsFastForward: advancing a checkpoint ref to a
// descendant commit (the normal case) pushes fine without force.
func TestBatchPushCheckpointRefs_AllowsFastForward(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)

	require.NoError(t, pushCheckpointRefsAtTips(t, repo, bareDir, refs))

	// Advance refs[0] to a child commit (fast-forward).
	testutil.WriteFile(t, workDir, "two.txt", "second")
	testutil.GitAdd(t, workDir, "two.txt")
	testutil.GitCommit(t, workDir, "second")
	head2, err := repo.Head()
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refs[0], head2.Hash())))

	require.NoError(t, pushCheckpointRefsAtTips(t, repo, bareDir, refs[:1]), "fast-forward update should push without force")
	assert.Equal(t, head2.Hash().String(), remoteRefHash(t, bareDir, refs[0]),
		"remote ref should advance to the descendant commit")
}

// TestBatchPushCheckpointRefs_RejectsNonFastForward: a divergent (non-descendant)
// update is rejected, and the remote ref is left untouched — the safety property
// that distinguishes this from a force push (we have no server-side ref
// protection).
func TestBatchPushCheckpointRefs_RejectsNonFastForward(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)

	require.NoError(t, pushCheckpointRefsAtTips(t, repo, bareDir, refs))
	original := remoteRefHash(t, bareDir, refs[0])

	// Point refs[0] at an orphan commit (no parent) — not a descendant of what was
	// pushed, so the update is non-fast-forward.
	runGit := func(args ...string) string {
		return strings.TrimSpace(testutil.RunGit(t, workDir, args...))
	}
	tree := runGit("rev-parse", "HEAD^{tree}")
	orphan := runGit("commit-tree", tree, "-m", "divergent")
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refs[0], plumbing.NewHash(orphan))))

	err = pushCheckpointRefsAtTips(t, repo, bareDir, refs[:1])
	require.Error(t, err, "a non-fast-forward update must be rejected, not force-pushed")
	assert.Equal(t, original, remoteRefHash(t, bareDir, refs[0]),
		"remote ref must be unchanged after a rejected non-fast-forward push")
}

// TestPushCheckpointRefWithRecovery_MergesDivergedRef: when a checkpoint ref has
// diverged on the remote (the same checkpoint advanced differently elsewhere), the
// recovery fetches the remote tip and replays the local-only commit on top, so the
// retry is a fast-forward — preserving the remote's change instead of overwriting
// it. Non-overlapping changes merge.
func TestPushCheckpointRefWithRecovery_MergesDivergedRef(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	ctx := context.Background()
	ref := refs[0]

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	head := func() plumbing.Hash {
		h, e := repo.Head()
		require.NoError(t, e)
		return h.Hash()
	}
	setRef := func(h plumbing.Hash) {
		require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(ref, h)))
	}

	c1 := head()
	require.NoError(t, pushCheckpointRefsAtTips(t, repo, bareDir, []plumbing.ReferenceName{ref})) // remote ref = C1

	// Remote advances: C2 (child of C1) adds b.txt; point the ref at it and push.
	testutil.WriteFile(t, workDir, "b.txt", "b")
	testutil.GitAdd(t, workDir, "b.txt")
	testutil.GitCommit(t, workDir, "add b")
	setRef(head())
	require.NoError(t, pushCheckpointRefsAtTips(t, repo, bareDir, []plumbing.ReferenceName{ref})) // remote ref = C2

	// Local diverges: reset to C1 and make C3 (sibling of C2) adding c.txt.
	testutil.GitReset(t, workDir, c1.String())
	testutil.WriteFile(t, workDir, "c.txt", "c")
	testutil.GitAdd(t, workDir, "c.txt")
	testutil.GitCommit(t, workDir, "add c")
	setRef(head())

	// C3 is not a descendant of the remote's C2 → the batch and individual
	// pushes are rejected, then recovery replays C3's delta onto C2.
	queue := enqueueRefs(t, repo, []plumbing.ReferenceName{ref})
	restore := captureStderr(t)
	pushed, pushErr := flushCheckpointRefsQueue(ctx, repo, pushSettings{remote: bareDir})
	output := restore()
	require.NoError(t, pushErr, "diverged ref should be recovered by fetch+replay, not rejected")
	assert.Equal(t, 1, pushed)
	assert.NotContains(t, output, "Warning:", "plain divergence should recover quietly")
	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.Empty(t, remaining, "recovered ref landed and must leave the queue")

	files := remoteRefFiles(t, bareDir, ref)
	assert.Contains(t, files, "b.txt", "remote-only change must be preserved (not overwritten)")
	assert.Contains(t, files, "c.txt", "local-only change must be replayed on top")
}

func TestPushCheckpointRefWithRecovery_PreservesConcurrentGenerationOnCASConflict(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	ctx := context.Background()
	ref := refs[0]

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)
	base := head.Hash()
	require.NoError(t, pushCheckpointRefsAtTips(t, repo, bareDir, []plumbing.ReferenceName{ref}))

	// Advance the remote from base.
	testutil.WriteFile(t, workDir, "remote.txt", "remote")
	testutil.GitAdd(t, workDir, "remote.txt")
	testutil.GitCommit(t, workDir, "remote generation")
	head, err = repo.Head()
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(ref, head.Hash())))
	require.NoError(t, pushCheckpointRefsAtTips(t, repo, bareDir, []plumbing.ReferenceName{ref}))
	remoteTip := head.Hash()

	// Create a disconnected queued generation that recovery will try to replay.
	// This covers the former ReconcileDisconnectedMetadataRef path, whose final
	// update was unconditional before checkpoint recovery became generation-safe.
	testutil.GitReset(t, workDir, base.String())
	testutil.WriteFile(t, workDir, "local.txt", "local")
	testutil.GitAdd(t, workDir, "local.txt")
	tree := strings.TrimSpace(testutil.RunGit(t, workDir, "write-tree"))
	candidate := plumbing.NewHash(strings.TrimSpace(testutil.RunGit(t, workDir, "commit-tree", tree, "-m", "local generation")))
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(ref, candidate)))
	queue := enqueueRefs(t, repo, []plumbing.ReferenceName{ref})

	// At the recovery install boundary, simulate a checkpoint writer advancing
	// the same ref and queueing that newer generation. The real CAS must reject
	// the stale recovery rather than overwrite it.
	oldCAS := checkpointRefRecoveryCAS
	var concurrent plumbing.Hash
	checkpointRefRecoveryCAS = func(
		casCtx context.Context,
		casRepo *git.Repository,
		refName plumbing.ReferenceName,
		newHash, expectedOld plumbing.Hash,
	) error {
		require.Equal(t, expectedOld, candidate)
		runGit := func(args ...string) string {
			return strings.TrimSpace(testutil.RunGit(t, workDir, args...))
		}
		testutil.WriteFile(t, workDir, "concurrent.txt", "concurrent")
		testutil.GitAdd(t, workDir, "concurrent.txt")
		concurrentTree := runGit("write-tree")
		concurrent = plumbing.NewHash(runGit("commit-tree", concurrentTree, "-p", candidate.String(), "-m", "concurrent generation"))
		require.NoError(t, casRepo.Storer.SetReference(plumbing.NewHashReference(refName, concurrent)))
		require.NoError(t, queue.EnqueueEntry(checkpoint.PushQueueEntry{Ref: refName, Hash: concurrent}))
		return checkpoint.CASPersistentRef(casCtx, casRepo, refName, newHash, expectedOld)
	}
	t.Cleanup(func() { checkpointRefRecoveryCAS = oldCAS })

	restore := captureStderr(t)
	pushed, pushErr := flushCheckpointRefsQueue(ctx, repo, pushSettings{remote: bareDir})
	restore()
	require.NoError(t, pushErr, "losing the CAS to a concurrent write is superseded, not a failed push")
	assert.Zero(t, pushed)

	localRef, err := repo.Reference(ref, true)
	require.NoError(t, err)
	assert.Equal(t, concurrent, localRef.Hash(), "recovery must not overwrite the concurrent checkpoint generation")
	assert.Equal(t, remoteTip.String(), remoteRefHash(t, bareDir, ref), "a CAS conflict must not push a stale recovered tip")
	queued, err := queue.PeekEntries()
	require.NoError(t, err)
	require.Len(t, queued, 1)
	assert.Equal(t, concurrent, queued[0].Hash, "the concurrent generation must remain queued")

	// The losing recovered commit is only a derived intermediate: the next
	// delivery replays the queued winner onto the remote and preserves all three
	// sources of data without keeping that stale intermediate under another ref.
	checkpointRefRecoveryCAS = oldCAS
	restore = captureStderr(t)
	pushed, pushErr = flushCheckpointRefsQueue(ctx, repo, pushSettings{remote: bareDir})
	restore()
	require.NoError(t, pushErr)
	assert.Equal(t, 1, pushed)
	queued, err = queue.PeekEntries()
	require.NoError(t, err)
	assert.Empty(t, queued)
	files := remoteRefFiles(t, bareDir, ref)
	assert.Contains(t, files, "remote.txt")
	assert.Contains(t, files, "local.txt")
	assert.Contains(t, files, "concurrent.txt")
}

// recoveryCAS is the signature of the checkpointRefRecoveryCAS seam.
type recoveryCAS func(ctx context.Context, repo *git.Repository, refName plumbing.ReferenceName, newHash, expectedOld plumbing.Hash) error

// stubRecoveryCAS replaces the recovery install for one test.
func stubRecoveryCAS(t *testing.T, cas recoveryCAS) {
	t.Helper()
	old := checkpointRefRecoveryCAS
	checkpointRefRecoveryCAS = cas
	t.Cleanup(func() { checkpointRefRecoveryCAS = old })
}

// loseRecoveryCAS moves the ref to base just before the install, as a concurrent
// session write would, so the real CAS reports a conflict.
func loseRecoveryCAS(t *testing.T, base plumbing.Hash) recoveryCAS {
	t.Helper()
	return func(ctx context.Context, repo *git.Repository, refName plumbing.ReferenceName, newHash, expectedOld plumbing.Hash) error {
		require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, base)))
		return checkpoint.CASPersistentRef(ctx, repo, refName, newHash, expectedOld)
	}
}

// divergeCheckpointRefs pushes n checkpoint refs at a remote generation, points
// them locally at a sibling generation, and queues them, so each one needs
// recovery. It returns the work repo, the remote, the refs, their common base,
// and the queue.
func divergeCheckpointRefs(t *testing.T, n int) (*git.Repository, string, []plumbing.ReferenceName, plumbing.Hash, *checkpoint.PushQueue) {
	t.Helper()
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, n)
	t.Chdir(workDir)
	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)
	base := head.Hash()
	setRefs := func(h plumbing.Hash) {
		for _, ref := range refs {
			require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(ref, h)))
		}
	}

	testutil.WriteFile(t, workDir, "remote.txt", "remote")
	testutil.GitAdd(t, workDir, "remote.txt")
	testutil.GitCommit(t, workDir, "remote generation")
	head, err = repo.Head()
	require.NoError(t, err)
	setRefs(head.Hash())
	require.NoError(t, pushCheckpointRefsAtTips(t, repo, bareDir, refs))

	testutil.GitReset(t, workDir, base.String())
	testutil.WriteFile(t, workDir, "local.txt", "local")
	testutil.GitAdd(t, workDir, "local.txt")
	testutil.GitCommit(t, workDir, "local generation")
	head, err = repo.Head()
	require.NoError(t, err)
	setRefs(head.Hash())
	return repo, bareDir, refs, base, enqueueRefs(t, repo, refs)
}

// A recovery that loses its CAS to a concurrent write leaves the newer
// generation local and queued. That is not a failed push: it must not count
// toward maxConsecutiveRefPushFailures, so a burst of concurrent writes cannot
// stop the retry before the remaining refs are attempted.
func TestFlushCheckpointRefsQueue_SupersededRecoveriesDoNotStopTheRetry(t *testing.T) {
	const n = maxConsecutiveRefPushFailures + 1
	repo, bareDir, _, base, queue := divergeCheckpointRefs(t, n)
	lose := loseRecoveryCAS(t, base)
	installs := 0
	stubRecoveryCAS(t, func(ctx context.Context, casRepo *git.Repository, refName plumbing.ReferenceName, newHash, expectedOld plumbing.Hash) error {
		installs++
		return lose(ctx, casRepo, refName, newHash, expectedOld)
	})

	restore := captureStderr(t)
	pushed, pushErr := flushCheckpointRefsQueue(context.Background(), repo, pushSettings{remote: bareDir})
	output := restore()
	require.NoError(t, pushErr, output)
	assert.Zero(t, pushed)
	assert.Equal(t, n, installs, "every superseded ref must still be attempted")
	assert.NotContains(t, output, "Stopped retrying")
	queued, err := queue.PeekEntries()
	require.NoError(t, err)
	assert.Len(t, queued, n, "superseded refs stay queued for the next push")

	// With no concurrent write, the next push delivers every superseded ref.
	stubRecoveryCAS(t, checkpoint.CASPersistentRef)
	restore = captureStderr(t)
	pushed, pushErr = flushCheckpointRefsQueue(context.Background(), repo, pushSettings{remote: bareDir})
	output = restore()
	require.NoError(t, pushErr, output)
	assert.Equal(t, n, pushed)
	queued, err = queue.PeekEntries()
	require.NoError(t, err)
	assert.Empty(t, queued)
}

// Superseded refs were attempted but did not fail, so the flush error counts
// only the real failures among the attempts.
func TestFlushCheckpointRefsQueue_ReportsOnlyRealFailures(t *testing.T) {
	repo, bareDir, refs, base, _ := divergeCheckpointRefs(t, 3)
	lose := loseRecoveryCAS(t, base)
	stubRecoveryCAS(t, func(ctx context.Context, casRepo *git.Repository, refName plumbing.ReferenceName, newHash, expectedOld plumbing.Hash) error {
		if refName == refs[0] {
			return errors.New("simulated install failure")
		}
		return lose(ctx, casRepo, refName, newHash, expectedOld)
	})

	restore := captureStderr(t)
	pushed, pushErr := flushCheckpointRefsQueue(context.Background(), repo, pushSettings{remote: bareDir})
	restore()
	assert.Zero(t, pushed)
	require.ErrorContains(t, pushErr, "1 of 3 attempted checkpoint refs failed to push")
}

// go-git loads a repository handle's pack index once and does not reload it on
// a miss, so recovery must replay from a handle that can see objects its fetch
// wrote into a new pack. With two diverged refs, the first recovery loads the
// index before the second fetch lands.
func TestFlushCheckpointRefsQueue_RecoversARefFetchedIntoANewPack(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	ctx := context.Background()
	// Keep every fetch as a pack, as git does once a fetch brings at least
	// fetch.unpackLimit objects.
	testutil.RunGit(t, workDir, "config", "fetch.unpackLimit", "1")

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)
	base := head.Hash()
	require.NoError(t, pushCheckpointRefsAtTips(t, repo, bareDir, refs))

	// Advance each ref on the remote from another clone, so its objects reach
	// the work repo only through recovery's fetch.
	otherDir := t.TempDir()
	testutil.InitRepo(t, otherDir)
	for i, ref := range refs {
		testutil.RunGit(t, otherDir, "fetch", "-q", bareDir, ref.String())
		testutil.RunGit(t, otherDir, "checkout", "-q", "--detach", "FETCH_HEAD")
		name := fmt.Sprintf("remote-%d.txt", i)
		testutil.WriteFile(t, otherDir, name, name)
		testutil.GitAdd(t, otherDir, name)
		testutil.GitCommit(t, otherDir, "remote generation "+name)
		testutil.RunGit(t, otherDir, "push", "-q", bareDir, "HEAD:"+ref.String())
	}

	for i, ref := range refs {
		testutil.GitReset(t, workDir, base.String())
		name := fmt.Sprintf("local-%d.txt", i)
		testutil.WriteFile(t, workDir, name, name)
		testutil.GitAdd(t, workDir, name)
		testutil.GitCommit(t, workDir, "local generation "+name)
		head, err = repo.Head()
		require.NoError(t, err)
		require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(ref, head.Hash())))
	}
	queue := enqueueRefs(t, repo, refs)

	restore := captureStderr(t)
	pushed, pushErr := flushCheckpointRefsQueue(ctx, repo, pushSettings{remote: bareDir})
	output := restore()
	require.NoError(t, pushErr, output)
	assert.Equal(t, 2, pushed)
	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.Empty(t, remaining, "both recovered refs landed and must leave the queue")
	for i, ref := range refs {
		files := remoteRefFiles(t, bareDir, ref)
		assert.Contains(t, files, fmt.Sprintf("remote-%d.txt", i), "remote-only change must be preserved")
		assert.Contains(t, files, fmt.Sprintf("local-%d.txt", i), "local-only change must be replayed on top")
	}
}

// pushCheckpointRefsAtTips pins each ref to its current local tip, as the flush
// does, and pushes them in one batch. Tests use it to seed or advance the remote.
func pushCheckpointRefsAtTips(t *testing.T, repo *git.Repository, target string, refs []plumbing.ReferenceName) error {
	t.Helper()
	pushes := make([]checkpointRefPush, 0, len(refs))
	for _, name := range refs {
		ref, err := repo.Reference(name, true)
		require.NoError(t, err)
		pushes = append(pushes, checkpointRefPush{
			token: checkpoint.PushQueueEntry{Ref: name, Hash: ref.Hash()},
			name:  name,
			hash:  ref.Hash(),
		})
	}
	return batchPushCheckpointRefs(t.Context(), target, pushes)
}

// enqueueRefs seeds the repo's push queue with the given refs.
func enqueueRefs(t *testing.T, repo *git.Repository, refs []plumbing.ReferenceName) *checkpoint.PushQueue {
	t.Helper()
	queue, err := checkpoint.PushQueueForRepo(context.Background(), repo)
	require.NoError(t, err)
	for _, ref := range refs {
		require.NoError(t, queue.EnqueueEntry(checkpoint.PushQueueEntry{Ref: ref}))
	}
	return queue
}

func TestPushQueuedCheckpointRefs(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	queue := enqueueRefs(t, repo, refs)

	pushed, pushDisabled, err := PushQueuedCheckpointRefs(context.Background(), repo, bareDir)
	require.NoError(t, err)
	assert.False(t, pushDisabled)
	assert.Equal(t, len(refs), pushed)

	for _, ref := range refs {
		assert.NotEmpty(t, remoteRefHash(t, bareDir, ref), "ref should be on the remote")
	}
	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.Empty(t, remaining, "pushed refs are removed from the queue")
}

func TestPushQueuedCheckpointRefs_PushDisabled(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()

	// push_sessions disabled: the push is a no-op, and the caller must be able
	// to tell that apart from an empty queue (pushed==0 with pushing enabled).
	require.NoError(t, os.MkdirAll(filepath.Join(workDir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(workDir, ".entire", "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"push_sessions": false}}`),
		0o600,
	))

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	queue := enqueueRefs(t, repo, refs)

	pushed, pushDisabled, err := PushQueuedCheckpointRefs(context.Background(), repo, bareDir)
	require.NoError(t, err)
	assert.True(t, pushDisabled, "push_sessions=false must be reported as disabled")
	assert.Equal(t, 0, pushed)

	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.ElementsMatch(t, refs, remaining, "disabled push leaves refs queued")
}

func TestPushQueuedCheckpointRefs_FailureLeavesRefsQueued(t *testing.T) {
	workDir, _, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	queue := enqueueRefs(t, repo, refs)

	badTarget := filepath.Join(t.TempDir(), "missing.git")
	pushed, _, err := PushQueuedCheckpointRefs(context.Background(), repo, badTarget)
	require.ErrorContains(t, err, "failed to push")
	assert.Equal(t, 0, pushed)

	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.ElementsMatch(t, refs, remaining, "failed push leaves refs queued")
}

// remoteRefFiles lists the files in the tree a ref points at on the bare remote.
func remoteRefFiles(t *testing.T, bareDir string, ref plumbing.ReferenceName) string {
	t.Helper()
	c := exec.CommandContext(context.Background(), "git", "-C", bareDir, "ls-tree", "-r", "--name-only", ref.String())
	c.Env = testutil.GitIsolatedEnv()
	out, err := c.CombinedOutput()
	require.NoError(t, err, "ls-tree failed: %s", out)
	return string(out)
}

// assertRefsAbsentFromRemote fails if any of refs reached the bare remote.
func assertRefsAbsentFromRemote(t *testing.T, bareDir string, refs []plumbing.ReferenceName, msg string) {
	t.Helper()
	out := testutil.RunGit(t, bareDir, "ls-remote", bareDir)
	for _, ref := range refs {
		assert.NotContains(t, out, ref.String(), msg)
	}
}

// remoteRefHash returns the object hash a ref points at on the bare remote.
func remoteRefHash(t *testing.T, bareDir string, ref plumbing.ReferenceName) string {
	t.Helper()
	lsCmd := exec.CommandContext(context.Background(), "git", "ls-remote", bareDir, ref.String())
	lsCmd.Env = testutil.GitIsolatedEnv()
	out, err := lsCmd.CombinedOutput()
	require.NoError(t, err, "ls-remote failed: %s", out)
	fields := strings.Fields(strings.TrimSpace(string(out)))
	require.NotEmpty(t, fields, "ref %s not found on remote", ref)
	return fields[0]
}

// TestPushQueuedCheckpointRefs_OPFAppliedBeforePush pins the gate this path was
// missing: with OPF enabled, the migration command's opt-in push must re-redact
// the queued refs before they leave the machine, so what lands on the remote is
// the rewritten, OPF-applied commit rather than the 8-layer one.
func TestPushQueuedCheckpointRefs_OPFAppliedBeforePush(t *testing.T) {
	configureFakeOPF(t, &fakeOPFForRewrite{})
	bareDir, repo, refs := setupGitRefsOPFRepo(t, "a1b2c3d4e5f6", "b2c3d4e5f6a1")
	before := refHashes(t, repo, refs)

	pushed, pushDisabled, err := PushQueuedCheckpointRefs(t.Context(), repo, bareDir)
	require.NoError(t, err)
	assert.False(t, pushDisabled)
	assert.Equal(t, len(refs), pushed)

	after := refHashes(t, repo, refs)
	for i, ref := range refs {
		require.NotEqual(t, before[i], after[i], "ref %s must be rewritten before it is pushed", ref)
		commit, commitErr := repo.CommitObject(after[i])
		require.NoError(t, commitErr)
		assert.True(t, trailers.HasOPFApplied(commit.Message), "pushed commit must carry the OPF trailer")
		assert.NotContains(t, treeContents(t, repo, after[i]), "PERSONABC", "the sentinel must be scrubbed")
		assert.Equal(t, after[i].String(), remoteRefHash(t, bareDir, ref),
			"the rewritten commit is what reached the remote, not the 8-layer one")
	}
	assert.Empty(t, queuedRefs(t, repo), "pushed refs leave the queue")
}

// TestPushQueuedCheckpointRefs_OPFFailureWithholdsPush is the fail-closed half:
// when OPF is enabled but the rewrite cannot run, nothing may reach the remote.
func TestPushQueuedCheckpointRefs_OPFFailureWithholdsPush(t *testing.T) {
	configureFakeOPF(t, &fakeRuntimeAlwaysFails{})
	bareDir, repo, refs := setupGitRefsOPFRepo(t, "a1b2c3d4e5f6", "b2c3d4e5f6a1")
	before := refHashes(t, repo, refs)

	pushed, pushDisabled, err := PushQueuedCheckpointRefs(t.Context(), repo, bareDir)
	require.Error(t, err)
	assert.False(t, pushDisabled)
	assert.Equal(t, 0, pushed)

	assert.Equal(t, before, refHashes(t, repo, refs), "a withheld push must leave every ref unmoved")
	assert.ElementsMatch(t, refs, queuedRefs(t, repo), "withheld refs stay queued")
	assertRefsAbsentFromRemote(t, bareDir, refs, "withheld push must not reach the remote")
}

// A queued ref that cannot be read (here a symbolic-ref loop, which fails with
// something other than not-found) has no tip to pin. It must still stay queued
// and be reported, not be withheld silently or pruned as stale.
func TestFlushCheckpointRefsQueue_UnreadableRefStaysQueuedAndIsReported(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	ctx := context.Background()
	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)

	loop := plumbing.ReferenceName("refs/entire/loop")
	require.NoError(t, repo.Storer.SetReference(plumbing.NewSymbolicReference(refs[0], loop)))
	require.NoError(t, repo.Storer.SetReference(plumbing.NewSymbolicReference(loop, refs[0])))
	queue := enqueueRefs(t, repo, refs)

	restore := captureStderr(t)
	pushed, pushErr := flushCheckpointRefsQueue(ctx, repo, pushSettings{remote: bareDir})
	output := restore()

	require.NoError(t, pushErr)
	assert.Equal(t, 1, pushed, "the readable ref still ships")
	assert.Contains(t, output, "1 checkpoint ref(s) could not be read and stay queued for the next push (for example "+refs[0].String()+")",
		"the warning names a ref so the user can find it")
	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.Equal(t, []plumbing.ReferenceName{refs[0]}, remaining, "the unreadable ref stays queued")

	// Once the ref is readable again, the next push delivers it.
	head, err := repo.Head()
	require.NoError(t, err)
	require.NoError(t, repo.Storer.RemoveReference(loop))
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refs[0], head.Hash())))
	restore = captureStderr(t)
	pushed, pushErr = flushCheckpointRefsQueue(ctx, repo, pushSettings{remote: bareDir})
	restore()
	require.NoError(t, pushErr)
	assert.Equal(t, 1, pushed)
	assert.Equal(t, head.Hash().String(), remoteRefHash(t, bareDir, refs[0]))
	remaining, err = queue.Drain()
	require.NoError(t, err)
	assert.Empty(t, remaining)
}
