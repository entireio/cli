package strategy

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

type opfThatAddsRefOnFirstBatch struct {
	fakeOPFForRewrite

	once sync.Once
	add  func()
}

func (f *opfThatAddsRefOnFirstBatch) RedactBatch(
	ctx context.Context,
	inputs []string,
	categories []string,
) ([][]redact.Span, error) {
	f.once.Do(f.add)
	return f.fakeOPFForRewrite.RedactBatch(ctx, inputs, categories)
}

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
// The fixture's single repeated leaf is exactly 9400 bytes; 7000 trips the
// leaf-byte cap while keeping the raw-byte ceiling that scales off the same
// env var (7000 × rawByteCapMultiplier(2) = 14000) above this fixture's
// actual raw content (~12.4 KB across the unpushed commits), so the
// leaf-byte cap is what these tests exercise, not the raw ceiling.
func addOversizedRef(t *testing.T, repo *git.Repository) {
	t.Helper()
	addGitRefsSessionWithTranscript(t, repo, flushOversizedID, "sess-oversized",
		strings.Repeat("the quick brown fox jumps over PERSONABC again ", 200))
	t.Setenv(batchEnvVar, "7000")
}

// Normal pre-push hands every untrailered ref to the detached worker without
// invoking OPF itself. The cap difference is irrelevant to the hook: both refs
// remain queued until background processing advances them.
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
		"normal pre-push leaves every untrailered ref queued for the worker")
}

func TestPrePushCheckpointRefs_DoesNotRunOPFInline(t *testing.T) {
	fake := &fakeOPFForRewrite{}
	configureFakeOPF(t, fake)
	bareDir, repo, refs := setupGitRefsOPFRepo(t, flushFitsID)
	spawns := swapOPFFlushSpawn(t)

	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))

	assert.Zero(t, fake.batchCallCount(), "normal pre-push must leave model inference to the detached worker")
	require.Len(t, *spawns, 1, "an ordinary untrailered ref must schedule the detached worker")
	assert.Equal(t, refs, queuedRefs(t, repo), "untrailered refs stay queued until the worker rewrites them")
	assertRefsAbsentFromRemote(t, bareDir, refs, "pre-push must not deliver content before background OPF finishes")
}

// The mirror case, and the reason maybeSpawnOPFFlush pre-checks at all: when all
// queued refs already carry the trailer, forking a child would only make it
// rediscover that there is no work.
func TestPrePushCheckpointRefs_NoOPFFlushSpawnWhenNothingAwaitsOPF(t *testing.T) {
	configureFakeOPF(t, &fakeOPFForRewrite{})
	_, repo, _ := setupGitRefsOPFRepo(t, flushFitsID)
	require.NoError(t, RewriteQueuedCheckpointRefsWithOPF(t.Context(), repo))
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

// Equal backlog counts do not imply stagnation. If the worker finishes A while
// a concurrent checkpoint write adds B, {A} becomes {B}; another pass is needed
// even though both sets contain one ref.
func TestRunOPFFlush_RetriesWhenBacklogSetChangesAtSameSize(t *testing.T) {
	_, repo, refs := setupGitRefsOPFRepo(t, flushFitsID)
	secondRef := mustRefName(t, id.MustCheckpointID(flushOversizedID))
	fake := &opfThatAddsRefOnFirstBatch{
		add: func() {
			addGitRefsSession(t, repo, flushOversizedID, "sess-added-during-flush")
		},
	}
	configureFakeOPF(t, fake)
	resetRedactionConfiguredForTest()
	t.Cleanup(resetRedactionConfiguredForTest)

	require.NoError(t, RunOPFFlush(t.Context()))
	require.Equal(t, 2, fake.batchCallCount(), "the changed backlog set must receive another pass")

	for _, refName := range append(refs, secondRef) {
		ref, err := repo.Reference(refName, true)
		require.NoError(t, err)
		commit, err := repo.CommitObject(ref.Hash())
		require.NoError(t, err)
		require.True(t, trailers.HasOPFApplied(commit.Message), "ref %s must be rewritten", refName)
	}
}
