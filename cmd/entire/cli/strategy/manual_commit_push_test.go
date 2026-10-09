package strategy

import (
	"context"
	"os/exec"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDeferCheckpointPushOnEmptyRemote_UsesLocalTrackingRefs verifies the guard
// decides purely from local remote-tracking refs, with no network access: a
// remote with no refs/remotes/<remote>/* is treated as possibly-empty (defer),
// and one with any tracking ref is treated as established (publish).
func TestDeferCheckpointPushOnEmptyRemote_UsesLocalTrackingRefs(t *testing.T) {
	// No t.Parallel: uses t.Chdir.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)

	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		require.NoError(t, cmd.Run(), "git %v", args)
	}
	run("commit", "--allow-empty", "-m", "init")
	// A deliberately unreachable URL: the guard must never dial it.
	run("remote", "add", "origin", "https://example.invalid/repo.git")

	t.Chdir(dir)
	ctx := context.Background()
	ps := pushSettings{remote: "origin"}

	// No remote-tracking refs yet → possibly a brand-new remote → defer.
	require.True(t, deferCheckpointPushOnEmptyRemote(ctx, ps),
		"a remote with no tracking refs must defer")

	// A push straight to a bare URL is not a configured remote; git never records
	// a tracking ref for it, so the guard must publish rather than defer forever.
	require.False(t,
		deferCheckpointPushOnEmptyRemote(ctx, pushSettings{remote: "https://example.invalid/repo.git"}),
		"a bare-URL push target must not defer")

	// git records a remote-tracking ref after the first successful push; simulate
	// that locally (no network). The remote is now established → publish.
	run("update-ref", "refs/remotes/origin/main", "HEAD")
	require.False(t, deferCheckpointPushOnEmptyRemote(ctx, ps),
		"a remote with a tracking ref must not defer")

	// A configured separate checkpoint remote is always exempt.
	require.False(t,
		deferCheckpointPushOnEmptyRemote(ctx, pushSettings{remote: "origin", checkpointURL: "https://example.invalid/cp.git"}),
		"a dedicated checkpoint remote is exempt from the guard")
}

// TestPrePushCheckpointRefs_RedactedRefShipsWhileFailedSiblingStaysQueued pins
// per-ref delivery on the pre-push path: a ref OPF finished ships in the same
// push even though a sibling could not be redacted, and that sibling stays
// queued without reaching the remote.
func TestPrePushCheckpointRefs_RedactedRefShipsWhileFailedSiblingStaysQueued(t *testing.T) {
	// No t.Parallel: the fixture uses t.Chdir.
	const okID, failID = "a1b2c3d4e5f6", "b2c3d4e5f6a1"
	const failSentinel = "OPFBOOM"
	configureFakeOPF(t, &fakeRuntimeFailsOnSentinel{sentinel: failSentinel})
	bareDir, repo, refs := setupGitRefsOPFRepo(t, okID)
	addGitRefsSessionWithTranscript(t, repo, failID, "sess-fail",
		"Hello, PERSONABC asked about "+failSentinel)
	refs = append(refs, mustRefName(t, id.MustCheckpointID(failID)))

	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"),
		"a ref OPF could not finish must not block the user's git push")

	require.NotEmpty(t, remoteRefHash(t, bareDir, refs[0]),
		"the redacted ref must reach the remote")
	assertRefsAbsentFromRemote(t, bareDir, []plumbing.ReferenceName{refs[1]},
		"the ref OPF could not finish must not reach the remote")
	require.Equal(t, []plumbing.ReferenceName{refs[1]}, queuedRefs(t, repo),
		"only the ref OPF could not finish stays queued")
}

// TestPushQueuedCheckpointRefs_ShipsReadyRefsAndReportsTheRest is the explicit
// push's half of the same gate. It cannot log-and-swallow like the pre-push
// path, so it has to say both things at once: the refs that were ready went,
// AND some are still queued. pushed being non-zero next to a non-nil error is
// the contract `doctor migrate-checkpoints` depends on to report what landed.
func TestPushQueuedCheckpointRefs_ShipsReadyRefsAndReportsTheRest(t *testing.T) {
	// No t.Parallel: the fixture uses t.Chdir.
	const okID, failID = "a1b2c3d4e5f6", "b2c3d4e5f6a1"
	const failSentinel = "OPFBOOM"
	configureFakeOPF(t, &fakeRuntimeFailsOnSentinel{sentinel: failSentinel})
	bareDir, repo, refs := setupGitRefsOPFRepo(t, okID, failID)
	addGitRefsSessionWithTranscript(t, repo, failID, "sess-fail",
		"Hello, PERSONABC asked about "+failSentinel)

	pushed, pushDisabled, err := PushQueuedCheckpointRefs(t.Context(), repo, bareDir)

	require.ErrorContains(t, err, "1 checkpoint ref(s) stay queued",
		"the leftover must be reported, and counted, not swallowed")
	require.False(t, pushDisabled)
	require.Equal(t, 1, pushed, "a non-nil error must not hide the ref that actually landed")

	require.NotEmpty(t, remoteRefHash(t, bareDir, refs[0]),
		"the already-redacted ref must reach the remote")
	require.Equal(t, []plumbing.ReferenceName{refs[1]}, queuedRefs(t, repo),
		"only the ref OPF could not finish stays queued")
}

// TestFlushCheckpointRefsQueue_WithholdsARefEnqueuedAfterTheRewrite walks the
// timeline where a concurrent session enqueues an untrailered ref after the
// inline rewrite finished but before the flush drains the queue. The flush is
// told only WHETHER the trailer is required and reads it off the set it
// drained, so the racing ref is checked like every other and held back rather
// than shipped as 8-layer content under an OPFRun decision.
func TestFlushCheckpointRefsQueue_WithholdsARefEnqueuedAfterTheRewrite(t *testing.T) {
	// No t.Parallel: the fixture uses t.Chdir.
	const earlyID, lateID = "a1b2c3d4e5f6", "b2c3d4e5f6a1"
	configureFakeOPF(t, &fakeOPFForRewrite{})
	bareDir, repo, refs := setupGitRefsOPFRepo(t, earlyID)
	earlyRef := refs[0]

	// The gate's inline rewrite: the queued ref is redacted and trailered, so it
	// is genuinely ready to ship.
	require.NoError(t, RewriteQueuedCheckpointRefsWithOPF(t.Context(), repo))

	// A concurrent session finalizes a checkpoint, enqueuing a ref that has
	// never been near OPF.
	addGitRefsSession(t, repo, lateID, "sess-late")
	lateRef := mustRefName(t, id.MustCheckpointID(lateID))
	require.ElementsMatch(t, []plumbing.ReferenceName{earlyRef, lateRef}, queuedRefs(t, repo),
		"both refs are queued, so both are in what the flush drains")

	// The flush. It is handed no exclusion set, only the requirement.
	pushed, withheld, err := flushCheckpointRefsQueue(
		t.Context(), repo, pushSettings{remote: bareDir}, true)
	require.NoError(t, err)
	assert.Equal(t, 1, pushed, "the trailered ref must still ship; per-ref delivery is the point")
	assert.Equal(t, 1, withheld, "the racing untrailered ref must be counted as held back")

	assert.NotEmpty(t, remoteRefHash(t, bareDir, earlyRef),
		"the already-redacted ref must reach the remote")
	assertRefsAbsentFromRemote(t, bareDir, []plumbing.ReferenceName{lateRef},
		"a ref enqueued after the readiness read must not ship unchecked")
	assert.Equal(t, []plumbing.ReferenceName{lateRef}, queuedRefs(t, repo),
		"the withheld ref stays queued untouched for the next push")
}

func TestCheckpointRefDelivery_PreservesSameRefGenerationAdvancedAfterVerification(t *testing.T) {
	// No t.Parallel: the fixture uses t.Chdir and OPF test configuration.
	const checkpointID = "a1b2c3d4e5f6"
	configureFakeOPF(t, &fakeOPFForRewrite{})
	bareDir, repo, refs := setupGitRefsOPFRepo(t, checkpointID)
	refName := refs[0]
	require.NoError(t, RewriteQueuedCheckpointRefsWithOPF(t.Context(), repo))

	queue, err := checkpoint.PushQueueForRepo(t.Context(), repo)
	require.NoError(t, err)
	drained, err := queue.DrainEntries()
	require.NoError(t, err)
	ready, awaiting, stale := partitionCheckpointRefPushes(repo, drained, true)
	require.Len(t, ready, 1)
	require.Empty(t, awaiting)
	require.Empty(t, stale)
	verifiedHash := ready[0].hash

	addGitRefsSession(t, repo, checkpointID, "sess-later")
	current, err := repo.Reference(refName, true)
	require.NoError(t, err)
	require.NotEqual(t, verifiedHash, current.Hash())

	require.NoError(t, batchPushCheckpointRefs(t.Context(), bareDir, ready))
	require.NoError(t, queue.RemoveEntries(checkpointRefPushTokens(ready)))
	assert.Equal(t, verifiedHash.String(), remoteRefHash(t, bareDir, refName),
		"delivery must stay pinned to the hash that passed the trailer check")

	remaining, err := queue.DrainEntries()
	require.NoError(t, err)
	require.Equal(t, []checkpoint.PushQueueEntry{{Ref: refName, Hash: current.Hash()}}, remaining,
		"cleanup for the delivered generation must preserve the newer generation")

	pushed, withheld, err := flushCheckpointRefsQueue(
		t.Context(), repo, pushSettings{remote: bareDir}, true)
	require.NoError(t, err)
	assert.Zero(t, pushed)
	assert.Equal(t, 1, withheld, "the newer untrailered generation must remain fail-closed")
	assert.Equal(t, verifiedHash.String(), remoteRefHash(t, bareDir, refName))
}

// TestPrePushCheckpointRefs_OPFSkipShipsTheWholeQueueUnchanged is the other half
// of the delivery gate: an explicit opt-out for this push (ENTIRE_OPF=no) is not
// a backlog. Nothing is rewritten, so every queued ref is untrailered — and the
// per-ref filter must not mistake that for "still redacting" and withhold the
// queue the user deliberately chose to ship as-is, 8-layer and untagged.
func TestPrePushCheckpointRefs_OPFSkipShipsTheWholeQueueUnchanged(t *testing.T) {
	// No t.Parallel: uses t.Setenv and the fixture's t.Chdir.
	configureFakeOPF(t, &fakeOPFForRewrite{})
	t.Setenv("ENTIRE_OPF", "no")
	bareDir, repo, refs := setupGitRefsOPFRepo(t, "a1b2c3d4e5f6", "b2c3d4e5f6a1")
	before := refHashes(t, repo, refs)

	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))

	require.Equal(t, before, refHashes(t, repo, refs), "an opted-out push must rewrite nothing")
	for i, ref := range refs {
		require.Equal(t, before[i].String(), remoteRefHash(t, bareDir, ref),
			"every ref must ship as written when the user opted out of OPF")
	}
	require.Empty(t, queuedRefs(t, repo), "pushed refs leave the queue")
}
