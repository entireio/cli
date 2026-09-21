package strategy

import (
	"bytes"
	"context"
	"os/exec"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"

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

// TestPrePushCheckpointRefs_AlreadyRedactedRefShipsWhileSiblingStillQueued
// pins the per-ref delivery gate: prePushCheckpointRefs used to withhold the
// WHOLE flush the moment opfGateForCheckpointRefs reported any error, so a ref
// already rewritten and trailered by RewriteQueuedCheckpointRefsWithOPF (per
// PR #2533's per-ref scoping) still never reached the remote if a sibling ref
// failed. With real OPF throughput (~1.14s/KB) a queue nearly always has at
// least one ref mid-redaction, so that made the all-or-nothing gate the jam.
//
// Ref order is load-bearing for the same reason it is in
// TestRewriteQueuedCheckpointRefsWithOPF_FailingOPFRefDoesNotBlockOthers: the
// OPF breaker trips on the first runtime failure, so the succeeding ref must be
// processed first or it would never be scanned at all.
func TestPrePushCheckpointRefs_AlreadyRedactedRefShipsWhileSiblingStillQueued(t *testing.T) {
	// No t.Parallel: the fixture uses t.Chdir.
	const okID, failID = "a1b2c3d4e5f6", "b2c3d4e5f6a1"
	const failSentinel = "OPFBOOM"
	configureFakeOPF(t, &fakeRuntimeFailsOnSentinel{sentinel: failSentinel})
	bareDir, repo, refs := setupGitRefsOPFRepo(t, okID, failID)
	addGitRefsSessionWithTranscript(t, repo, failID, "sess-fail",
		"Hello, PERSONABC asked about "+failSentinel)

	var buf bytes.Buffer
	oldWriter := stderrWriter
	stderrWriter = &buf
	t.Cleanup(func() { stderrWriter = oldWriter })

	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"),
		"an OPF failure on one ref must not block the user's git push")

	lsCmd := exec.CommandContext(t.Context(), "git", "ls-remote", bareDir)
	lsCmd.Env = testutil.GitIsolatedEnv()
	out, lsErr := lsCmd.CombinedOutput()
	require.NoError(t, lsErr, "ls-remote failed: %s", out)
	require.Contains(t, string(out), refs[0].String(),
		"the ref whose OPF call succeeded must reach the remote despite its failing sibling")
	require.NotContains(t, string(out), refs[1].String(),
		"the failing ref must not reach the remote")

	stillQueued := queuedRefs(t, repo)
	require.Contains(t, stillQueued, refs[1], "the failing ref stays queued for the next push")
	require.NotContains(t, stillQueued, refs[0], "a ref that landed must leave the queue")
	require.Contains(t, buf.String(), "stay queued for the next push",
		"the refs left behind must still be visible to the user")
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

// TestFlushCheckpointRefsQueue_WithholdsARefEnqueuedAfterTheReadinessRead walks
// the exact timeline that made a pre-computed exclusion list fail open, in the
// order a concurrent session produces it:
//
//  1. the readiness read (RefsAwaitingOPF) — what the delivery path used to take
//     BEFORE calling the flush, and pass in as the set to exclude;
//  2. another session finalizes a checkpoint and enqueues its ref, untrailered.
//     That ref cannot be in the snapshot from step 1: it did not exist yet;
//  3. the flush drains the queue — and the new ref IS in what it drained.
//
// A flush that excludes the caller's snapshot ships that ref having never
// checked it for the OPF trailer, which is 8-layer content leaving the machine
// under an OPFRun decision. A flush that reads the trailer off the set it
// drained cannot: there is no set it did not check.
//
// So this test is also the guard against a regression to the two-step pattern.
// The flush is told only WHETHER the trailer is required; the step-1 snapshot is
// computed here purely to pin that it is blind to the racing ref, and is
// deliberately not passed anywhere.
func TestFlushCheckpointRefsQueue_WithholdsARefEnqueuedAfterTheReadinessRead(t *testing.T) {
	// No t.Parallel: the fixture uses t.Chdir.
	const earlyID, lateID = "a1b2c3d4e5f6", "b2c3d4e5f6a1"
	configureFakeOPF(t, &fakeOPFForRewrite{})
	bareDir, repo, refs := setupGitRefsOPFRepo(t, earlyID)
	earlyRef := refs[0]

	// The gate's inline rewrite: the queued ref is redacted and trailered, so it
	// is genuinely ready to ship.
	require.NoError(t, RewriteQueuedCheckpointRefsWithOPF(t.Context(), repo))

	// Step 1: the pre-Drain readiness read. Nothing is awaiting OPF yet.
	snapshot, err := RefsAwaitingOPF(t.Context(), repo)
	require.NoError(t, err)
	require.Empty(t, snapshot, "the rewritten ref carries the trailer, so nothing is awaiting OPF")

	// Step 2: a concurrent session finalizes a checkpoint, enqueuing a ref that
	// has never been near OPF.
	addGitRefsSession(t, repo, lateID, "sess-late")
	lateRef := mustRefName(t, id.MustCheckpointID(lateID))
	require.NotContains(t, snapshot, lateRef,
		"the racing ref must be invisible to the earlier read — that is the window")
	require.ElementsMatch(t, []plumbing.ReferenceName{earlyRef, lateRef}, queuedRefs(t, repo),
		"both refs are queued, so both are in what the flush drains")

	// Step 3: the flush. It is handed no exclusion set, only the requirement.
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
		"the withheld ref stays queued untouched, for the worker and the next push")
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
