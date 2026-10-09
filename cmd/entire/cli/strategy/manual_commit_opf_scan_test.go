package strategy

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6"
	gitconfig "github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/redact"
)

// swapOPFScanSpawn replaces the detached-spawn seam with a recorder and returns
// the remotes it was asked to scan for. execx.SpawnDetached is a no-op under
// `go test`, and the test binary does not understand `__opf_scan` anyway.
func swapOPFScanSpawn(t *testing.T) *[]string {
	t.Helper()
	remotes := make([]string, 0, 1)
	old := opfScanSpawn
	opfScanSpawn = func(_, remote string) { remotes = append(remotes, remote) }
	t.Cleanup(func() { opfScanSpawn = old })
	return &remotes
}

// The worker scans what pre-push held back and delivers it itself: after one
// run every queued ref is redacted, trailered, on the remote, and off the queue.
func TestRunOPFScan_ScansAndDeliversGitRefs(t *testing.T) {
	fake := &fakeOPFForRewrite{}
	configureFakeOPF(t, fake)
	bareDir, repo, refs := setupGitRefsOPFRepo(t, "a1b2c3d4e5f6", "b2c3d4e5f6a1")
	resetRedactionConfiguredForTest()
	t.Cleanup(resetRedactionConfiguredForTest)
	spawns := swapOPFScanSpawn(t)

	require.NoError(t, RunOPFScan(t.Context(), "origin"))

	require.Positive(t, fake.batchCallCount(), "the worker must run the model")
	require.Empty(t, *spawns, "the worker must never spawn another worker")
	require.Empty(t, queuedRefs(t, repo), "delivered refs leave the queue")
	for _, ref := range refs {
		tip := remoteRefHash(t, bareDir, ref)
		require.NotEmpty(t, tip, "ref %s must reach the remote", ref)
		commit, err := repo.CommitObject(plumbing.NewHash(tip))
		require.NoError(t, err)
		require.True(t, trailers.HasOPFApplied(commit.Message))
		require.NotContains(t, treeContents(t, repo, commit.Hash), "PERSONABC")
	}
}

// A long backlog is scanned in batches bounded by one ref's raw cap rather than
// loaded whole, so the detached worker's memory does not grow with the queue.
// Every ref still gets scanned, and each batch is delivered as it finishes.
func TestRunOPFScan_BoundsEachScanBatchByTheRawCap(t *testing.T) {
	configureFakeOPF(t, &fakeOPFForRewrite{})
	bareDir, repo, refs := setupGitRefsOPFRepo(t, "a1b2c3d4e5f6", "b2c3d4e5f6a1", "c3d4e5f6a1b2")
	resetRedactionConfiguredForTest()
	t.Cleanup(resetRedactionConfiguredForTest)
	swapOPFScanSpawn(t)

	maxRaw, maxLeaf, totalRaw := 0, 0, 0
	for _, ref := range refs {
		raw, leaf := refRewriteSizes(t, repo, ref)
		maxRaw, maxLeaf, totalRaw = max(maxRaw, raw), max(maxLeaf, leaf), totalRaw+raw
	}
	limit := max(maxLeaf, (maxRaw+rawByteCapMultiplier-1)/rawByteCapMultiplier)
	rawCap := rawByteCapForBatchLimit(limit)
	require.Greater(t, totalRaw, rawCap, "fixture: the whole backlog must exceed one raw cap")
	t.Setenv(batchEnvVar, strconv.Itoa(limit))

	var batchRaw, deliveredBefore []int
	old := opfScanBlobs
	opfScanBlobs = func(ctx context.Context, blobs []redact.NamedBlob, cache redact.OPFSpanCache) error {
		n := 0
		for _, b := range blobs {
			n += len(b.Content)
		}
		batchRaw = append(batchRaw, n)
		deliveredBefore = append(deliveredBefore, len(refs)-len(queuedRefs(t, repo)))
		return old(ctx, blobs, cache)
	}
	t.Cleanup(func() { opfScanBlobs = old })

	require.NoError(t, RunOPFScan(t.Context(), "origin"))

	require.Greater(t, len(batchRaw), 1, "the backlog must be split across scans")
	require.Positive(t, deliveredBefore[len(deliveredBefore)-1],
		"each batch is delivered before the next is scanned, not after the whole backlog")
	for _, n := range batchRaw {
		require.LessOrEqual(t, n, rawCap, "no scan batch may exceed the raw cap")
	}
	require.Empty(t, queuedRefs(t, repo), "every ref is still delivered")
	for _, ref := range refs {
		require.NotEmpty(t, remoteRefHash(t, bareDir, ref))
	}
}

// A push spawns a worker whenever none is running, even right after another
// one was spawned and finished; while one runs, it leaves the work to it.
func TestMaybeSpawnOPFScan_GatesOnTheWorkerLockNotTime(t *testing.T) {
	configureFakeOPF(t, &fakeOPFForRewrite{})
	setupGitRefsOPFRepo(t, "a1b2c3d4e5f6")
	spawns := swapOPFScanSpawn(t)

	maybeSpawnOPFScan(t.Context(), "origin")
	maybeSpawnOPFScan(t.Context(), "origin")
	require.Len(t, *spawns, 2, "no worker is running, so each push must start one")

	release, held := acquireOPFScanWorkerLock(t.Context())
	require.False(t, held)
	defer release()
	maybeSpawnOPFScan(t.Context(), "origin")
	require.Len(t, *spawns, 2, "a running worker owns the new work")
}

// Work held by a push while the worker still held its lock (so the push spawned
// nothing) is picked up by the worker once it releases the lock.
func TestRunOPFScan_PicksUpWorkHeldJustBeforeRelease(t *testing.T) {
	configureFakeOPF(t, &fakeOPFForRewrite{})
	bareDir, repo, refs := setupGitRefsOPFRepo(t, "a1b2c3d4e5f6")
	resetRedactionConfiguredForTest()
	t.Cleanup(resetRedactionConfiguredForTest)
	swapOPFScanSpawn(t)

	const lateID = "b2c3d4e5f6a1"
	added := false
	old := opfScanBeforeRelease
	opfScanBeforeRelease = func() {
		if !added {
			added = true
			addGitRefsSession(t, repo, lateID, "sess-late")
		}
	}
	t.Cleanup(func() { opfScanBeforeRelease = old })

	require.NoError(t, RunOPFScan(t.Context(), "origin"))

	require.NotEmpty(t, remoteRefHash(t, bareDir, refs[0]))
	require.NotEmpty(t, remoteRefHash(t, bareDir, mustRefName(t, id.MustCheckpointID(lateID))),
		"work held just before the worker released its lock must still be delivered")
	require.Empty(t, queuedRefs(t, repo))
}

// A ref whose unscanned history is over the bootstrap limit is refused by the
// rewrite, so the worker does not spend model time on it.
func TestRunOPFScan_SkipsRefsOverTheBootstrapLimit(t *testing.T) {
	fake := &fakeOPFForRewrite{}
	configureFakeOPF(t, fake)
	_, repo, _ := setupGitRefsOPFRepo(t, "a1b2c3d4e5f6")
	addGitRefsSession(t, repo, "a1b2c3d4e5f6", "sess-2")
	t.Setenv("ENTIRE_OPF_BOOTSTRAP_LIMIT", "1")
	resetRedactionConfiguredForTest()
	t.Cleanup(resetRedactionConfiguredForTest)
	swapOPFScanSpawn(t)

	require.NoError(t, RunOPFScan(t.Context(), "origin"))
	require.Zero(t, fake.batchCallCount(), "an over-limit ref must not reach the model")
}

// On git-refs a user push that sends checkpoint refs itself (`--mirror`, an
// explicit refspec) is refused while their commits lack the OPF trailer, since
// withholding Entire's own push cannot stop it. A ref with the trailer goes
// through, and so does everything after an explicit opt-out for this push.
func TestPrePushCheckpointRefs_RefusesOuterPushOfUnverifiedRefs(t *testing.T) {
	configureFakeOPF(t, &fakeOPFForRewrite{})
	_, repo, refs := setupGitRefsOPFRepo(t, "a1b2c3d4e5f6")
	swapOPFScanSpawn(t)
	outer := func() context.Context {
		tip := refHashes(t, repo, refs)[0].String()
		return WithPrePushRefs(t.Context(), []PrePushRef{{
			LocalRef: refs[0].String(), LocalSHA: tip, RemoteRef: refs[0].String(), RemoteSHA: strings.Repeat("0", 40),
		}})
	}

	err := NewManualCommitStrategy().PrePushFromGitHook(outer(), "origin")
	require.ErrorIs(t, err, ErrOuterPushCarriesUnverifiedCheckpoints)

	t.Setenv("ENTIRE_OPF", "no")
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(outer(), "origin"),
		"an explicit opt-out for this push lets it through")
	t.Setenv("ENTIRE_OPF", "")

	_, repo, refs = setupGitRefsOPFRepo(t, "b2c3d4e5f6a1")
	require.NoError(t, RewriteQueuedCheckpointRefsWithOPF(t.Context(), repo))
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(outer(), "origin"),
		"a ref that carries the trailer may be pushed")
}

// On git-refs too, a held checkpoint ref tells a user whose hook does not pass
// the ref list that an outer push carrying it cannot be checked.
func TestPrePushCheckpointRefs_HeldRefWarnsWhenRefsAreUnknown(t *testing.T) {
	configureFakeOPF(t, &fakeOPFForRewrite{})
	setupGitRefsOPFRepo(t, "a1b2c3d4e5f6")
	swapOPFScanSpawn(t)
	var buf bytes.Buffer
	oldWriter := stderrWriter
	stderrWriter = &buf
	t.Cleanup(func() { stderrWriter = oldWriter })

	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	require.Contains(t, buf.String(), "does not pass the refs being pushed")

	buf.Reset()
	main := []PrePushRef{{LocalRef: "refs/heads/main", LocalSHA: strings.Repeat("1", 40), RemoteRef: "refs/heads/main"}}
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(WithPrePushRefs(t.Context(), main), "origin"))
	require.NotContains(t, buf.String(), "does not pass the refs being pushed")
}

// A worker already running owns the work: a second one must leave it alone
// rather than repeat the same model calls.
func TestRunOPFScan_SkipsWhileAnotherWorkerHoldsTheLock(t *testing.T) {
	fake := &fakeOPFForRewrite{}
	configureFakeOPF(t, fake)
	bareDir, repo, refs := setupGitRefsOPFRepo(t, "a1b2c3d4e5f6")
	resetRedactionConfiguredForTest()
	t.Cleanup(resetRedactionConfiguredForTest)

	release, held := acquireOPFScanWorkerLock(t.Context())
	require.False(t, held)
	require.NoError(t, RunOPFScan(t.Context(), "origin"))
	require.Zero(t, fake.batchCallCount(), "a second worker must not call the model")
	assertRefsAbsentFromRemote(t, bareDir, refs, "a second worker must not deliver")
	require.Equal(t, refs, queuedRefs(t, repo))

	release()
	resetRedactionConfiguredForTest()
	require.NoError(t, RunOPFScan(t.Context(), "origin"))
	require.NotEmpty(t, remoteRefHash(t, bareDir, refs[0]), "once the lock is free the worker delivers")
}

// On git-branch the worker scans the unpushed v1 chain and pushes v1 itself.
func TestRunOPFScan_ScansAndDeliversV1(t *testing.T) {
	fake := &fakeOPFForRewrite{}
	configureFakeOPF(t, fake)
	dir, repo, originalTip := setupV1RepoInDir(t)
	remoteDir := filepath.Join(t.TempDir(), "origin.git")
	_, err := git.PlainInit(remoteDir, true)
	require.NoError(t, err)
	_, err = repo.CreateRemote(&gitconfig.RemoteConfig{Name: "origin", URLs: []string{remoteDir}})
	require.NoError(t, err)
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	resetRedactionConfiguredForTest()
	t.Cleanup(resetRedactionConfiguredForTest)
	// The remote already has the user's branch, so v1 is not deferred as a
	// would-be default branch on an empty remote.
	for _, args := range [][]string{{"push", "-q", "origin", "HEAD:refs/heads/main"}, {"update-ref", "refs/remotes/origin/main", "HEAD"}} {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = testutil.GitIsolatedEnv()
		out, runErr := cmd.CombinedOutput()
		require.NoError(t, runErr, "git %v: %s", args, out)
	}

	require.NoError(t, RunOPFScan(t.Context(), "origin"))

	require.Positive(t, fake.batchCallCount())
	remote, err := git.PlainOpen(remoteDir)
	require.NoError(t, err)
	ref, err := remote.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err, "the worker must push v1")
	require.NotEqual(t, originalTip, ref.Hash())
	commit, err := remote.CommitObject(ref.Hash())
	require.NoError(t, err)
	require.True(t, trailers.HasOPFApplied(commit.Message))
	require.NotContains(t, treeContents(t, repo, ref.Hash()), "PERSONABC")
}

func TestCheckpointsAwaitingOPF_GitRefs(t *testing.T) {
	configureFakeOPF(t, &fakeOPFForRewrite{})
	_, repo, _ := setupGitRefsOPFRepo(t, "a1b2c3d4e5f6", "b2c3d4e5f6a1")

	n, err := CheckpointsAwaitingOPF(t.Context(), repo)
	require.NoError(t, err)
	require.Equal(t, 2, n)

	require.NoError(t, RewriteQueuedCheckpointRefsWithOPF(t.Context(), repo))
	n, err = CheckpointsAwaitingOPF(t.Context(), repo)
	require.NoError(t, err)
	require.Zero(t, n, "rewritten refs no longer wait for OPF")
}

func TestCheckpointsAwaitingOPF_V1(t *testing.T) {
	configureFakeOPF(t, &fakeOPFForRewrite{})
	dir, repo, _ := setupV1RepoInDir(t)
	t.Chdir(dir)

	n, err := CheckpointsAwaitingOPF(t.Context(), repo)
	require.NoError(t, err)
	require.Positive(t, n, "an unscanned v1 chain waits for OPF")

	_, err = RewriteUnpushedV1WithOPF(t.Context(), repo, "origin")
	require.NoError(t, err)
	n, err = CheckpointsAwaitingOPF(t.Context(), repo)
	require.NoError(t, err)
	require.Zero(t, n, "a trailered v1 tip no longer waits for OPF")
}

// A cache that cannot be opened is a real failure, not a scan in progress: the
// refs are withheld with the cause, and no worker is started, because it would
// need the same cache.
func TestPrePushCheckpointRefs_UnusableCacheIsReportedNotPending(t *testing.T) {
	configureFakeOPF(t, &fakeOPFForRewrite{})
	bareDir, repo, refs := setupGitRefsOPFRepo(t, "a1b2c3d4e5f6")
	commonDir, err := gitdir.CommonDir(t.Context())
	require.NoError(t, err)
	// A regular file where the cache directory belongs makes it unopenable.
	require.NoError(t, os.WriteFile(filepath.Join(commonDir, checkpoint.OPFSpanCacheDirName), []byte("x"), 0o600))
	spawns := swapOPFScanSpawn(t)

	var buf bytes.Buffer
	oldWriter := stderrWriter
	stderrWriter = &buf
	t.Cleanup(func() { stderrWriter = oldWriter })

	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))

	require.Contains(t, buf.String(), "scan cache", "the real cause must reach the user")
	require.NotContains(t, buf.String(), opfScanPendingNotice, "an unusable cache is not a scan in progress")
	require.Empty(t, *spawns, "a worker cannot help without the cache")
	require.Equal(t, refs, queuedRefs(t, repo))
	assertRefsAbsentFromRemote(t, bareDir, refs, "nothing may ship without a scan")
}

// A ref that hits a cap and an unscanned sibling must not hide each other, in
// either queue order: the pending signal is what starts the worker, and the cap
// error is what tells the user a ref is stuck.
func TestPrePushCheckpointRefs_CapErrorAndPendingSiblingAreBothReported(t *testing.T) {
	for _, capped := range []int{0, 1} {
		t.Run(strconv.Itoa(capped), func(t *testing.T) {
			configureFakeOPF(t, &fakeOPFForRewrite{})
			cpIDs := []string{"a1b2c3d4e5f6", "b2c3d4e5f6a1"}
			_, repo, refs := setupGitRefsOPFRepo(t, cpIDs...)
			addGitRefsSession(t, repo, cpIDs[capped], "sess-2")
			require.Equal(t, refs[capped], queuedRefs(t, repo)[capped], "the capped ref keeps its queue position")
			t.Setenv("ENTIRE_OPF_BOOTSTRAP_LIMIT", "1")
			resetRedactionConfiguredForTest()
			t.Cleanup(resetRedactionConfiguredForTest)
			spawns := swapOPFScanSpawn(t)

			var buf bytes.Buffer
			oldWriter := stderrWriter
			stderrWriter = &buf
			t.Cleanup(func() { stderrWriter = oldWriter })

			require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))

			require.Equal(t, []string{"origin"}, *spawns, "the pending sibling needs the worker")
			require.Contains(t, buf.String(), opfScanPendingNotice)
			require.Contains(t, buf.String(), "bootstrap", "the cap must still be reported")
		})
	}
}
