//go:build integration

package integration

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// =============================================================================
// D6 / D7 -- git-refs push-queue semantics (integration).
// =============================================================================

// TestGitRefsQueue_MultiWorktreeSharedQueueDrains is D6: the git-refs push queue
// lives in the shared git common dir, so a session in a linked worktree and a
// session in the main worktree both enqueue into ONE queue. A single pre-push
// from the main worktree pushes BOTH worktrees' checkpoint refs, drains the queue
// to empty, and prunes a stale entry (a queued ref that no longer exists locally),
// exercising partitionLocalRefs.
func TestGitRefsQueue_MultiWorktreeSharedQueueDrains(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.CheckpointStore = StoreGitRefs
	bareDir := env.SetupBareRemote()

	// Session in the main worktree enqueues its checkpoint ref into the shared queue.
	_ = createCheckpointedCommit(t, env, "main worktree work", "main_wt.go", "package mainwt", "main worktree work")
	queueAfterMain := env.PushQueueRefs()
	require.NotEmpty(t, queueAfterMain, "main worktree session should enqueue a checkpoint ref")

	// Session in a linked worktree (shares the git common dir, so the SAME queue).
	wt := env.AddWorktree("feature/wt-second")
	_ = createCheckpointedCommit(t, wt, "linked worktree work", "wt.go", "package wt", "linked worktree work")

	// The shared queue (read from the main worktree) now holds an additional
	// distinct ref enqueued by the linked worktree's session.
	realRefs := env.PushQueueRefs()
	require.Greater(t, len(realRefs), len(queueAfterMain),
		"the linked worktree's session should enqueue an additional ref into the shared common-dir queue")

	// A stale entry (no such local ref) must be pruned, not pushed or retried forever.
	staleRef := checkpointRefName("ffffffffffff")
	enqueueCheckpointRef(t, env, staleRef)

	// One pre-push from the main worktree drains everything pushable in the queue.
	env.RunPrePush("origin")

	for _, ref := range realRefs {
		require.True(t, refExists(t, bareDir, ref),
			"both worktrees' checkpoint refs must reach the remote via the shared queue: %s", ref)
	}
	require.False(t, refExists(t, bareDir, staleRef),
		"a stale queue entry must not be pushed")
	require.Empty(t, env.PushQueueRefs(),
		"the shared queue must be drained and compacted after a successful push (stale entry pruned)")
}

// TestGitRefsQueue_TwoRemotesQueueClearedByFirstPush is D7: it PINS the known
// two-remote queue-clearing gap (test plan decision D-2).
//
// KNOWN BUG / PINNED CURRENT BEHAVIOR: the push queue records only the ref, not
// which remote it was pushed to. A successful pre-push to ANY remote drains the
// queue, so a second remote never receives those refs. This test asserts that
// current (buggy) behavior on purpose, so a regression is visible and so the
// fix is a deliberate, reviewed change.
//
// WHEN THE FIX LANDS (per-remote push tracking, or "drain only when pushed to the
// fetch-resolution target"): flip the final assertion — the second remote SHOULD
// then receive the checkpoint refs.
func TestGitRefsQueue_TwoRemotesQueueClearedByFirstPush(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.CheckpointStore = StoreGitRefs

	bareOrigin := env.SetupBareRemote()
	bareSecond := env.SetupNamedBareRemote("second")

	cp := createCheckpointedCommit(t, env, "Add module", "mod.go", "package mod", "Add module")
	require.NotEmpty(t, cp)
	require.Contains(t, env.PushQueueRefs(), checkpointRefName(cp), "checkpoint ref should be queued")

	// Push to origin first — this drains the queue.
	env.RunPrePush("origin")
	require.True(t, env.CheckpointExistsOnRemote(bareOrigin, cp),
		"origin should receive the checkpoint ref")
	require.Empty(t, env.PushQueueRefs(),
		"the queue is drained by the first push regardless of remote (root of the D-2 gap)")

	// Now push to the second remote. Because the queue is already empty, nothing is
	// sent — the second remote permanently misses the checkpoint.
	env.RunPrePush("second")

	// PINNED CURRENT BEHAVIOR (test plan D-2): the second remote never gets the ref.
	// Flip this to require.True when per-remote queue tracking lands.
	require.False(t, env.CheckpointExistsOnRemote(bareSecond, cp),
		"KNOWN BUG (D-2): the second remote misses checkpoint refs the first push drained; "+
			"flip to require.True when per-remote push tracking lands")
}
