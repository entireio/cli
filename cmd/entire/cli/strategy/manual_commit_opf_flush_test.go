package strategy

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/redact"
)

// Shared fixture shorthand: two checkpoint refs, the second of which is made to
// carry far more prose-leaf content than the cap allows.
const (
	flushFitsID      = "a1b2c3d4e5f6"
	flushOversizedID = "b2c3d4e5f6a1"
)

// swapOPFFlushSpawn replaces the detached-spawn seam with a recorder for the
// duration of the test, and returns the slice of worktree roots it was asked to
// spawn in. Without the seam the decision is unobservable: execx.SpawnDetached
// is a deliberate no-op under `go test`, and a real `go test` binary does not
// understand `__opf_flush` as an argument anyway.
func swapOPFFlushSpawn(t *testing.T) *[]string {
	t.Helper()
	roots := make([]string, 0, 1)
	old := opfFlushSpawn
	opfFlushSpawn = func(worktreeRoot string) { roots = append(roots, worktreeRoot) }
	t.Cleanup(func() { opfFlushSpawn = old })
	return &roots
}

// addOversizedRef grows oversizedID's ref past the per-ref leaf-byte cap and
// sets that cap low enough for the other ref to stay comfortably under it.
func addOversizedRef(t *testing.T, repo *git.Repository) {
	t.Helper()
	addGitRefsSessionWithTranscript(t, repo, flushOversizedID, "sess-oversized",
		strings.Repeat("the quick brown fox jumps over PERSONABC again ", 200))
	t.Setenv(batchEnvVar, "2000")
}

// The point of the whole change: the pre-push hook hands leftover OPF work to a
// detached child instead of doing it inline. With a WORKING OPF runtime and one
// ref the per-ref cap skips, the gate leaves a genuine backlog behind — so the
// hook must spawn exactly one flush child, and still return (the user's push is
// never blocked on the model call for the ref it could not finish).
func TestPrePushCheckpointRefs_SpawnsOPFFlushWhenBacklogRemains(t *testing.T) {
	configureFakeOPF(t, &fakeOPFForRewrite{})
	_, repo, refs := setupGitRefsOPFRepo(t, flushFitsID, flushOversizedID)
	addOversizedRef(t, repo)
	spawns := swapOPFFlushSpawn(t)

	var buf bytes.Buffer
	oldWriter := stderrWriter
	stderrWriter = &buf
	t.Cleanup(func() { stderrWriter = oldWriter })

	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"),
		"leftover OPF work must not fail the user's git push")

	require.Len(t, *spawns, 1, "exactly one detached flush child per push with a backlog")
	assert.ElementsMatch(t, refs, queuedRefs(t, repo),
		"the withheld flush leaves every ref queued for the worker and the next push")
}

// The mirror case, and the reason maybeSpawnOPFFlush pre-checks at all: when the
// inline gate finished the work there is nothing for a child to do, so forking
// one would cost a process to discover that. Mirrors the countSweepableZombies
// pre-check in maybeSpawnSessionSweep.
func TestPrePushCheckpointRefs_NoOPFFlushSpawnWhenNothingAwaitsOPF(t *testing.T) {
	configureFakeOPF(t, &fakeOPFForRewrite{})
	setupGitRefsOPFRepo(t, flushFitsID)
	spawns := swapOPFFlushSpawn(t)

	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))

	assert.Empty(t, *spawns, "a fully rewritten queue must not fork a child that would exit immediately")
}

// OPF off is the common-case fast path: no queue read, no marker write, no
// child. The pre-check must short-circuit before any of it.
func TestPrePushCheckpointRefs_NoOPFFlushSpawnWhenOPFDisabled(t *testing.T) {
	redact.ResetOPFConfigForTest()
	t.Cleanup(redact.ResetOPFConfigForTest)
	setupGitRefsOPFRepo(t, flushFitsID)
	spawns := swapOPFFlushSpawn(t)

	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))

	assert.Empty(t, *spawns, "OPF being off is not a backlog")
}

// An explicit opt-out for this push (ENTIRE_OPF=no) leaves the whole queue
// un-rewritten, which looks exactly like a backlog. It is not one: the user
// declined the model call, so handing it to a child a moment later would run
// the very scan they turned off — and would rewrite refs the flush is pushing
// as-is right now, leaving the local ref diverged from what landed.
func TestPrePushCheckpointRefs_NoOPFFlushSpawnWhenUserSkippedOPF(t *testing.T) {
	configureFakeOPF(t, &fakeOPFForRewrite{})
	t.Setenv("ENTIRE_OPF", "no")
	setupGitRefsOPFRepo(t, flushFitsID)
	spawns := swapOPFFlushSpawn(t)

	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))

	assert.Empty(t, *spawns, "a declined scan must not reappear as a background scan")
}

// A ref the cap keeps skipping re-nominates a spawn on every push. The shared
// throttle marker collapses a burst of pushes to one child, so the second push
// in the same window must not fork another.
func TestPrePushCheckpointRefs_OPFFlushSpawnIsThrottled(t *testing.T) {
	configureFakeOPF(t, &fakeOPFForRewrite{})
	_, repo, _ := setupGitRefsOPFRepo(t, flushFitsID, flushOversizedID)
	addOversizedRef(t, repo)
	spawns := swapOPFFlushSpawn(t)

	var buf bytes.Buffer
	oldWriter := stderrWriter
	stderrWriter = &buf
	t.Cleanup(func() { stderrWriter = oldWriter })

	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))

	assert.Len(t, *spawns, 1, "a second push inside the throttle window must reuse the in-flight child")
}

// The worker's contract, and the reason it is safe to run unattended: it
// rewrites the backlog and stops there. Delivery stays with the pre-push gate
// on the user's own push, so nothing may reach the remote from a detached
// process and the refs must still be queued when it finishes.
func TestRunOPFFlush_RewritesTheBacklogButNeverPushes(t *testing.T) {
	configureFakeOPF(t, &fakeOPFForRewrite{})
	bareDir, repo, refs := setupGitRefsOPFRepo(t, flushFitsID)
	resetRedactionConfiguredForTest()
	t.Cleanup(resetRedactionConfiguredForTest)

	require.NoError(t, RunOPFFlush(t.Context()))

	tip, err := repo.CommitObject(refHashes(t, repo, refs)[0])
	require.NoError(t, err)
	assert.True(t, trailers.HasOPFApplied(tip.Message), "the worker must rewrite what it found")
	assert.NotContains(t, treeContents(t, repo, tip.Hash), "PERSONABC")

	assert.ElementsMatch(t, refs, queuedRefs(t, repo),
		"the worker rewrites but never pushes, so the ref stays queued for the gate")
	lsCmd := exec.CommandContext(t.Context(), "git", "ls-remote", bareDir)
	lsCmd.Env = testutil.GitIsolatedEnv()
	out, lsErr := lsCmd.CombinedOutput()
	require.NoError(t, lsErr, "ls-remote failed: %s", out)
	assert.NotContains(t, string(out), refs[0].String(), "a background process must not deliver anything")
}

// The visibility surface's input: a ref that fails every pass is counted, and
// only once it has failed StuckOPFFailureThreshold times running does it become
// something worth telling a human about. Below the threshold it is
// indistinguishable from "the worker has not got to it yet", which is the whole
// distinction the count exists to draw.
func TestRunOPFFlush_CountsConsecutiveFailuresUntilStuck(t *testing.T) {
	configureFakeOPF(t, &fakeOPFForRewrite{})
	_, repo, _ := setupGitRefsOPFRepo(t, flushFitsID, flushOversizedID)
	addOversizedRef(t, repo)
	resetRedactionConfiguredForTest()
	t.Cleanup(resetRedactionConfiguredForTest)
	oversizedRef := mustRefName(t, id.MustCheckpointID(flushOversizedID))

	for pass := 1; pass < checkpoint.StuckOPFFailureThreshold; pass++ {
		require.NoError(t, RunOPFFlush(t.Context()))
		stuck, err := checkpoint.StuckOPFRefs(t.Context(), repo)
		require.NoError(t, err)
		assert.Empty(t, stuck, "pass %d is still within retry range, not stuck", pass)
	}

	require.NoError(t, RunOPFFlush(t.Context()))
	stuck, err := checkpoint.StuckOPFRefs(t.Context(), repo)
	require.NoError(t, err)
	assert.Equal(t, []plumbing.ReferenceName{oversizedRef}, stuck,
		"a ref that has failed the threshold number of passes in a row is stuck")
}

// "Consecutive" has to mean consecutive: a ref that finally succeeds must leave
// the stuck list, or a transient failure would brand it forever.
func TestRunOPFFlush_SuccessClearsAPreviouslyFailingRef(t *testing.T) {
	configureFakeOPF(t, &fakeOPFForRewrite{})
	_, repo, _ := setupGitRefsOPFRepo(t, flushFitsID, flushOversizedID)
	addOversizedRef(t, repo)
	resetRedactionConfiguredForTest()
	t.Cleanup(resetRedactionConfiguredForTest)

	for range checkpoint.StuckOPFFailureThreshold {
		require.NoError(t, RunOPFFlush(t.Context()))
	}
	stuck, err := checkpoint.StuckOPFRefs(t.Context(), repo)
	require.NoError(t, err)
	require.Len(t, stuck, 1, "fixture must reach the stuck state first")

	// Lift the cap the ref was tripping and let the worker try again.
	t.Setenv(batchEnvVar, "unlimited")
	require.NoError(t, RunOPFFlush(t.Context()))

	stuck, err = checkpoint.StuckOPFRefs(t.Context(), repo)
	require.NoError(t, err)
	assert.Empty(t, stuck, "a successful rewrite resets the ref's consecutive-failure count")
}
