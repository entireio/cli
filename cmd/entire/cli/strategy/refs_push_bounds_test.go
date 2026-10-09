package strategy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	checkpointremote "github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupRepoWithNCheckpointRefs is setupRepoWithCheckpointRefs for a queue large
// enough to out-run the fallback's bounds. IDs shard on their last two
// characters, so the counter keeps every ref in a distinct shard.
func setupRepoWithNCheckpointRefs(t *testing.T, n int) (string, string, []plumbing.ReferenceName) {
	t.Helper()
	require.Less(t, n, 256, "the ID counter must stay inside one hex byte")

	workDir := t.TempDir()
	testutil.InitRepo(t, workDir)
	testutil.WriteFile(t, workDir, "README.md", "# test")
	testutil.GitAdd(t, workDir, "README.md")
	testutil.GitCommit(t, workDir, "init")

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)

	refs := make([]plumbing.ReferenceName, 0, n)
	for i := range n {
		ref := mustRefName(t, id.MustCheckpointID(fmt.Sprintf("%012x", i)))
		require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(ref, head.Hash())))
		refs = append(refs, ref)
	}

	bareDir := t.TempDir()
	testutil.RunGit(t, bareDir, "init", "--bare")
	return workDir, bareDir, refs
}

func countedAttempts(t *testing.T, countFile string) int {
	t.Helper()
	data, err := os.ReadFile(countFile)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	require.NoError(t, err)
	return len(strings.Fields(string(data)))
}

// prepareGitRefsPrePush wires the repo the way the git-refs pre-push path
// expects: CWD-based resolution, git-refs primary, and an "origin" remote.
func prepareGitRefsPrePush(t *testing.T, workDir, remoteTarget string) {
	t.Helper()
	t.Chdir(workDir) // Also captures process-global stderr.
	paths.ClearWorktreeRootCache()
	t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", "git-refs")
	testutil.AddRemote(t, workDir, "origin", remoteTarget)
}

// TestFlushCheckpointRefs_StopsAfterConsecutiveFailures pins the bound on the
// fallback's fan-out. A remote refusing every ref refuses all of them, so the
// flush must stop rather than spend one network round-trip per queued ref.
func TestFlushCheckpointRefs_StopsAfterConsecutiveFailures(t *testing.T) {
	queued := maxConsecutiveRefPushFailures + 4
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, queued)
	countFile := filepath.Join(t.TempDir(), "attempts")
	prepareGitRefsPrePush(t, workDir, bareDir)
	installCheckpointRejectHook(t, bareDir, false, countFile)

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	queue := enqueueRefs(t, repo, refs)

	restore := captureStderr(t)
	err = NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin")
	output := restore()
	require.NoError(t, err, "a bounded flush must still never block the user's push")

	// One batch attempt, then the fallback stops after the cap.
	assert.Equal(t, 1+maxConsecutiveRefPushFailures, countedAttempts(t, countFile),
		"the fallback must not walk the whole queue against a refusing remote")
	assert.Contains(t, output, "Stopped retrying")
	assert.Contains(t, output, fmt.Sprintf("%d consecutive failures", maxConsecutiveRefPushFailures))
	assert.Contains(t, output, fmt.Sprintf("%d checkpoint ref(s) stay queued", queued),
		"refs that were attempted and failed stay queued too, not just the ones never reached")

	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.ElementsMatch(t, refs, remaining, "nothing landed, so nothing may leave the queue")
	assertRefsAbsentFromRemote(t, bareDir, refs, "rejected refs must stay local")
}

// TestFlushCheckpointRefs_StopsWhenBudgetExhausted covers the other bound: slow
// failures, where the per-ref budget alone lets a large queue run for hours.
func TestFlushCheckpointRefs_StopsWhenBudgetExhausted(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 4)
	countFile := filepath.Join(t.TempDir(), "attempts")
	prepareGitRefsPrePush(t, workDir, bareDir)
	installCheckpointRejectHook(t, bareDir, false, countFile)

	restoreBudget := checkpointFlushBudget
	checkpointFlushBudget = time.Nanosecond
	t.Cleanup(func() { checkpointFlushBudget = restoreBudget })

	l, logErr := logging.New(logging.Config{Root: entiredir.OpenerAt(workDir), Dir: logging.LogsName})
	require.NoError(t, logErr)
	t.Cleanup(func() { _ = l.Close() })

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	queue := enqueueRefs(t, repo, refs)

	restore := captureStderr(t)
	err = NewManualCommitStrategy().PrePushFromGitHook(logging.WithLogger(t.Context(), l), "origin")
	output := restore()
	require.NoError(t, err)
	require.NoError(t, l.Close()) // flush the buffered writer

	assert.Contains(t, output, "budget")
	assert.Contains(t, output, "exhausted")
	assert.Contains(t, output, fmt.Sprintf("%d checkpoint ref(s) stay queued", len(refs)),
		"nothing landed, so the whole queue stays")
	assert.LessOrEqual(t, countedAttempts(t, countFile), 2, "the deadline must cut the fallback, not let it walk the queue")

	// The batch always attempts its first chunk; a budget that cuts it ends
	// the flush there rather than "retrying" on the spent budget.
	logged, readErr := os.ReadFile(filepath.Join(workDir, logging.LogsDir, "entire.log"))
	require.NoError(t, readErr)
	assert.Contains(t, string(logged), "batch push cut by the flush budget")
	assert.NotContains(t, output, "retrying")

	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.ElementsMatch(t, refs, remaining)
}

// TestFlushCheckpointRefs_LogsBatchFailureCause pins the diagnostic hole this
// path left on an unreachable remote: the batch error is not a rejection, so
// nothing downstream re-derives it, and it used to reach neither the terminal
// nor .entire/logs.
func TestFlushCheckpointRefs_LogsBatchFailureCause(t *testing.T) {
	workDir, _, refs := setupRepoWithCheckpointRefs(t)
	missing := filepath.Join(t.TempDir(), "missing.git")
	prepareGitRefsPrePush(t, workDir, missing)

	l, logErr := logging.New(logging.Config{Root: entiredir.OpenerAt(workDir), Dir: logging.LogsName})
	require.NoError(t, logErr)
	t.Cleanup(func() { _ = l.Close() })
	logCtx := logging.WithLogger(t.Context(), l)

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	enqueueRefs(t, repo, refs)

	restore := captureStderr(t)
	err = NewManualCommitStrategy().PrePushFromGitHook(logCtx, "origin")
	output := restore()
	require.NoError(t, err, "an unreachable remote must never block the user's push")
	require.NoError(t, l.Close()) // flush the buffered writer

	data, readErr := os.ReadFile(filepath.Join(workDir, logging.LogsDir, "entire.log"))
	require.NoError(t, readErr)
	logged := string(data)

	assert.Contains(t, logged, "batch checkpoint ref push failed",
		"a wholesale failure must be recorded before the per-ref retries bury it")
	assert.Contains(t, logged, "does not appear to be a git repository",
		"the log must carry git's own reason, not just the fact of failure")
	assert.NotContains(t, output, "does not appear to be a git repository",
		"the batch error is logged, not printed into the user's git push")
}

// installSelectiveRejectHook rejects any push whose ref updates include a ref
// other than allowRef. A pre-receive hook declines the whole push, so a batch
// carrying one blocked ref takes every healthy ref down with it — the shape that
// forces the per-ref fallback to be the only way the healthy refs can land.
func installSelectiveRejectHook(t *testing.T, bareDir, allowRef string) {
	t.Helper()
	hook := "#!/bin/sh\nblocked=0\nwhile read -r old new ref; do\n" +
		"  [ \"$ref\" = '" + allowRef + "' ] || blocked=1\ndone\n" +
		"if [ \"$blocked\" = 1 ]; then echo '" + checkpointRejectReason + "' >&2; exit 1; fi\nexit 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(bareDir, "hooks", "pre-receive"), []byte(hook), 0o755))
}

// TestFlushCheckpointRefs_BlockedPrefixDoesNotStarveHealthyRefs pins the fairness
// half of the bound. The queue drains in first-seen order, so a prefix that
// always fails — five checkpoints blocked by a ruleset, say — would otherwise be
// retried in the same order on every push and the healthy ref behind them would
// never be attempted at all, despite the abort line promising another try.
func TestFlushCheckpointRefs_BlockedPrefixDoesNotStarveHealthyRefs(t *testing.T) {
	blocked := maxConsecutiveRefPushFailures
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, blocked+1)
	healthy := refs[blocked]
	prepareGitRefsPrePush(t, workDir, bareDir)
	installSelectiveRejectHook(t, bareDir, healthy.String())

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	queue := enqueueRefs(t, repo, refs)

	// First flush: the blocked prefix exhausts the cap and the healthy ref is
	// never reached, which is exactly why it must not stay last in the queue.
	restore := captureStderr(t)
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	restore()
	assertRefsAbsentFromRemote(t, bareDir, refs, "nothing can land while the prefix is attempted first")

	// Second flush: the rotated queue puts the healthy ref first, so it lands.
	restore = captureStderr(t)
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	restore()

	assert.Equal(t, remoteRefHash(t, bareDir, healthy),
		refHashOf(t, repo, healthy), "a healthy ref must not be starved by a permanently blocked prefix")
	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.ElementsMatch(t, refs[:blocked], remaining, "only the blocked refs stay queued")
}

func refHashOf(t *testing.T, repo *git.Repository, ref plumbing.ReferenceName) string {
	t.Helper()
	r, err := repo.Reference(ref, true)
	require.NoError(t, err)
	return r.Hash().String()
}

// TestFlushCheckpointRefs_BudgetAbortRotatesToo pins that rotation is not
// confined to the consecutive-failure cap. A slow remote exhausts the budget at
// roughly the same position on every push, so without rotating here too the tail
// of the queue starves exactly as it does behind a failing prefix — rotation is
// fair scheduling, not a verdict on the ref that happened to be in flight.
func TestFlushCheckpointRefs_BudgetAbortRotatesToo(t *testing.T) {
	shrinkRefPushChunkSize(t, 1) // the cut chunk is exactly the first ref
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 4)
	prepareGitRefsPrePush(t, workDir, bareDir)
	installCheckpointRejectHook(t, bareDir, false, "")

	restoreBudget := checkpointFlushBudget
	checkpointFlushBudget = time.Nanosecond
	t.Cleanup(func() { checkpointFlushBudget = restoreBudget })

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	queue := enqueueRefs(t, repo, refs)

	restore := captureStderr(t)
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	output := restore()
	require.Contains(t, output, "exhausted", "precondition: this flush must stop on the budget, not the failure cap")

	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.Equal(t, append(append([]plumbing.ReferenceName{}, refs[1:]...), refs[0]),
		remaining, "the attempted ref moves to the back so the next push starts somewhere new")
}

func shrinkRefPushChunkSize(t *testing.T, n int) {
	t.Helper()
	restore := checkpointRefPushChunkSize
	checkpointRefPushChunkSize = n
	t.Cleanup(func() { checkpointRefPushChunkSize = restore })
}

// installCountingHook accepts every push, recording one line per push in
// countFile, and sleeps on every push after the first sleepAfter.
func installCountingHook(t *testing.T, bareDir, countFile string, sleepAfter int) {
	t.Helper()
	hook := "#!/bin/sh\ncat >/dev/null\n" +
		"n=$(cat '" + countFile + "' 2>/dev/null | wc -l)\necho attempt >> '" + countFile + "'\n"
	if sleepAfter > 0 {
		hook += fmt.Sprintf("[ \"$n\" -ge %d ] && sleep 30\n", sleepAfter)
	}
	hook += "exit 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(bareDir, "hooks", "pre-receive"), []byte(hook), 0o755))
}

// installBlockRefHook declines any push whose ref updates include blockedRef —
// the whole push, as a ruleset does, so the blocked ref's chunk fails with it.
func installBlockRefHook(t *testing.T, bareDir, blockedRef string) {
	t.Helper()
	hook := "#!/bin/sh\nblocked=0\nwhile read -r old new ref; do\n" +
		"  [ \"$ref\" = '" + blockedRef + "' ] && blocked=1\ndone\n" +
		"if [ \"$blocked\" = 1 ]; then echo '" + checkpointRejectReason + "' >&2; exit 1; fi\nexit 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(bareDir, "hooks", "pre-receive"), []byte(hook), 0o755))
}

// TestFlushCheckpointRefs_PushesBacklogInChunks pins the chunked batch: a
// backlog larger than one chunk goes out a chunk per push and fully drains.
func TestFlushCheckpointRefs_PushesBacklogInChunks(t *testing.T) {
	shrinkRefPushChunkSize(t, 3)
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 8)
	countFile := filepath.Join(t.TempDir(), "attempts")
	prepareGitRefsPrePush(t, workDir, bareDir)
	installCountingHook(t, bareDir, countFile, 0)

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	queue := enqueueRefs(t, repo, refs)

	restore := captureStderr(t)
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	output := restore()

	assert.Equal(t, 3, countedAttempts(t, countFile), "8 refs in chunks of 3 is three pushes")
	assert.Contains(t, output, "Pushing 8 checkpoint ref(s)")
	assert.Contains(t, output, " done")
	for _, ref := range refs {
		assert.Equal(t, refHashOf(t, repo, ref), remoteRefHash(t, bareDir, ref))
	}
	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.Empty(t, remaining)
}

// TestFlushCheckpointRefs_BudgetKeepsLandedChunks is the hang this bound
// exists for: a batch upload that stalls must not hold the user's git push past
// the flush budget, and the chunks that landed before it must leave the queue so
// the next push starts after them instead of re-sending the whole backlog.
func TestFlushCheckpointRefs_BudgetKeepsLandedChunks(t *testing.T) {
	shrinkRefPushChunkSize(t, 2)
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 6)
	countFile := filepath.Join(t.TempDir(), "attempts")
	prepareGitRefsPrePush(t, workDir, bareDir)
	installCountingHook(t, bareDir, countFile, 1) // first push lands, the rest stall

	restoreBudget := checkpointFlushBudget
	checkpointFlushBudget = 3 * time.Second
	t.Cleanup(func() { checkpointFlushBudget = restoreBudget })

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	queue := enqueueRefs(t, repo, refs)

	restore := captureStderr(t)
	start := time.Now()
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	elapsed := time.Since(start)
	output := restore()

	assert.Less(t, elapsed, 25*time.Second, "a stalled upload must be cut by the budget, not waited out")
	assert.Contains(t, output, "exhausted")
	assert.Contains(t, output, "4 checkpoint ref(s) stay queued")
	for _, ref := range refs[:2] {
		assert.Equal(t, refHashOf(t, repo, ref), remoteRefHash(t, bareDir, ref), "the first chunk landed")
	}
	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.ElementsMatch(t, refs[2:], remaining, "landed chunks leave the queue; the rest stay")
}

// TestFlushCheckpointRefs_RejectedChunkDoesNotStopLaterChunks pins that a
// rejection fails only its own chunk: later chunks still go out in the batch,
// and the rejected chunk's healthy refs land through the per-ref fallback.
func TestFlushCheckpointRefs_RejectedChunkDoesNotStopLaterChunks(t *testing.T) {
	shrinkRefPushChunkSize(t, 2)
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 6)
	prepareGitRefsPrePush(t, workDir, bareDir)
	installBlockRefHook(t, bareDir, refs[0].String())

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	queue := enqueueRefs(t, repo, refs)

	restore := captureStderr(t)
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	output := restore()

	assert.Contains(t, output, "retrying 2 ref(s) individually", "only the rejected chunk falls back")
	assert.Contains(t, output, "pushed 5 of 6")
	for _, ref := range refs[1:] {
		assert.Equal(t, refHashOf(t, repo, ref), remoteRefHash(t, bareDir, ref))
	}
	assertRefsAbsentFromRemote(t, bareDir, refs[:1], "the blocked ref must stay local")
	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.Equal(t, refs[:1], remaining)
}

// TestFlushCheckpointRefs_StopsBatchAfterConsecutiveChunkFailures pins the
// batch phase's own cap: a remote refusing every push refuses every chunk, so
// the batch stops instead of sending the whole backlog to be refused.
func TestFlushCheckpointRefs_StopsBatchAfterConsecutiveChunkFailures(t *testing.T) {
	shrinkRefPushChunkSize(t, 2)
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 8)
	countFile := filepath.Join(t.TempDir(), "attempts")
	prepareGitRefsPrePush(t, workDir, bareDir)
	installCheckpointRejectHook(t, bareDir, false, countFile)

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	queue := enqueueRefs(t, repo, refs)

	restore := captureStderr(t)
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	output := restore()

	// Two chunk pushes, then one per-ref retry for each of their four refs.
	assert.Equal(t, maxConsecutiveChunkPushFailures+4, countedAttempts(t, countFile))
	assert.Contains(t, output, fmt.Sprintf("%d consecutive failed batches", maxConsecutiveChunkPushFailures))
	assert.Contains(t, output, "8 checkpoint ref(s) stay queued")
	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.ElementsMatch(t, refs, remaining)
}

// TestPushQueuedCheckpointRefs_BatchNotBudgeted pins the migration push's
// exemption: the user asked for that upload and waits on it, so the batch is
// not cut by the pre-push budget.
func TestPushQueuedCheckpointRefs_BatchNotBudgeted(t *testing.T) {
	shrinkRefPushChunkSize(t, 2)
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 5)
	prepareGitRefsPrePush(t, workDir, bareDir)

	restoreBudget := checkpointFlushBudget
	checkpointFlushBudget = time.Nanosecond
	t.Cleanup(func() { checkpointFlushBudget = restoreBudget })

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	queue := enqueueRefs(t, repo, refs)

	restore := captureStderr(t)
	pushed, err := flushCheckpointRefsQueue(t.Context(), repo, pushSettings{remote: bareDir}, false)
	restore()
	require.NoError(t, err)
	assert.Equal(t, len(refs), pushed)
	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.Empty(t, remaining)
}

// TestFlushCheckpointRefs_UnreachableRemoteStopsAtOnce pins the dead-host
// bound: when git cannot even connect, neither later chunks nor per-ref retries
// can either, so the flush makes one attempt, says why, and keeps the queue.
func TestFlushCheckpointRefs_UnreachableRemoteStopsAtOnce(t *testing.T) {
	shrinkRefPushChunkSize(t, 2)
	workDir, _, refs := setupRepoWithNCheckpointRefs(t, 6)
	prepareGitRefsPrePush(t, workDir, "ssh://git@example.invalid/repo.git")

	// A fake ssh client: counts connections and fails the way OpenSSH does
	// against a refused port, without touching the network. Named "ssh" so git
	// takes it for OpenSSH and skips its "-G" probe of unrecognized clients.
	countFile := filepath.Join(t.TempDir(), "attempts")
	fakeSSH := filepath.Join(t.TempDir(), "ssh")
	script := "#!/bin/sh\necho attempt >> '" + countFile + "'\n" +
		"echo 'ssh: connect to host example.invalid port 22: Connection refused' >&2\nexit 255\n"
	require.NoError(t, os.WriteFile(fakeSSH, []byte(script), 0o755))
	t.Setenv("GIT_SSH_COMMAND", fakeSSH)

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	queue := enqueueRefs(t, repo, refs)

	restore := captureStderr(t)
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	output := restore()

	assert.Equal(t, 1, countedAttempts(t, countFile), "no further chunk or per-ref retry against a dead host")
	assert.Contains(t, output, "Couldn't reach origin: ssh: connect to host example.invalid port 22: Connection refused")
	assert.Contains(t, output, "6 checkpoint ref(s) stay queued")
	assert.NotContains(t, output, "retrying")
	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.Equal(t, refs, remaining, "nothing is dropped or reordered")
}

// installSlowMultiRefHook stalls any push carrying more than one ref, and
// accepts single-ref pushes at once: a link too slow for a whole chunk.
func installSlowMultiRefHook(t *testing.T, bareDir string) {
	t.Helper()
	hook := "#!/bin/sh\nn=$(grep -c refs/entire/)\n[ \"$n\" -gt 1 ] && sleep 30\nexit 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(bareDir, "hooks", "pre-receive"), []byte(hook), 0o755))
}

// TestFlushCheckpointRefs_ChunkTooSlowForBudgetShrinks pins progress on a
// link too slow to land one chunk within the budget: without adapting, the
// same head-of-queue chunk is cut on every push and nothing ever lands.
func TestFlushCheckpointRefs_ChunkTooSlowForBudgetShrinks(t *testing.T) {
	shrinkRefPushChunkSize(t, 4)
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 4)
	prepareGitRefsPrePush(t, workDir, bareDir)
	installSlowMultiRefHook(t, bareDir)
	restoreBudget := checkpointFlushBudget
	checkpointFlushBudget = 2 * time.Second
	t.Cleanup(func() { checkpointFlushBudget = restoreBudget })

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	queue := enqueueRefs(t, repo, refs)

	restore := captureStderr(t)
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	output := restore()
	assert.Contains(t, output, "Stopped pushing: budget (2s) exhausted; 4 checkpoint ref(s) stay queued")
	assert.NotContains(t, output, "retrying", "a budget cut is not a push failure to retry")
	assert.Equal(t, 2, queue.ChunkSizeHint(checkpointRefPushChunkSize), "the cut chunk halves the next one")

	for range 2 { // 2 per push is still too slow; then single refs land.
		restore = captureStderr(t)
		require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
		restore()
	}
	for _, ref := range refs {
		assert.Equal(t, refHashOf(t, repo, ref), remoteRefHash(t, bareDir, ref))
	}
	assert.Equal(t, 2, queue.ChunkSizeHint(checkpointRefPushChunkSize), "a clean flush grows it back")
}

// TestPushQueuedCheckpointRefs_FallbackGetsFreshBudget: the migration push
// leaves its batch unbounded, so a slow batch must not leave its per-ref
// fallback a budget the batch already spent.
func TestPushQueuedCheckpointRefs_FallbackGetsFreshBudget(t *testing.T) {
	shrinkRefPushChunkSize(t, 2)
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 6)
	prepareGitRefsPrePush(t, workDir, bareDir)
	hook := "#!/bin/sh\nblocked=0\nwhile read -r old new ref; do\n" +
		"  [ \"$ref\" = '" + refs[0].String() + "' ] && blocked=1\ndone\nsleep 1.5\n" +
		"if [ \"$blocked\" = 1 ]; then echo '" + checkpointRejectReason + "' >&2; exit 1; fi\nexit 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(bareDir, "hooks", "pre-receive"), []byte(hook), 0o755))
	restoreBudget := checkpointFlushBudget
	checkpointFlushBudget = 4 * time.Second
	t.Cleanup(func() { checkpointFlushBudget = restoreBudget })

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	enqueueRefs(t, repo, refs)

	restore := captureStderr(t)
	_, err = flushCheckpointRefsQueue(t.Context(), repo, pushSettings{remote: bareDir}, false)
	restore()
	require.Error(t, err, "the blocked ref still fails")
	assert.Equal(t, refHashOf(t, repo, refs[1]), remoteRefHash(t, bareDir, refs[1]),
		"the blocked ref's healthy chunk-mate lands through the fallback despite the slow batch")
}

// TestFlushCheckpointRefs_PartialDeliveryStillCountsAsDelivered: a chunked
// flush can land chunks and then fail (here, SSH auth on a later chunk). The
// landed chunks reached the remote, so the pre-push acts on them — capture,
// misdirection warning — rather than reading the error as "nothing synced".
func TestFlushCheckpointRefs_PartialDeliveryStillCountsAsDelivered(t *testing.T) {
	shrinkRefPushChunkSize(t, 2)
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 4)
	prepareGitRefsPrePush(t, workDir, bareDir)
	hook := "#!/bin/sh\ngrep -q '" + refs[2].String() + "' && { echo 'git@github.com: Permission denied (publickey).' >&2; exit 1; }\nexit 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(bareDir, "hooks", "pre-receive"), []byte(hook), 0o755))

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	enqueueRefs(t, repo, refs)

	restore := captureStderr(t)
	pushed, err := flushCheckpointRefsQueue(checkpointremote.WithNonInteractiveSSH(t.Context()), repo, pushSettings{remote: bareDir}, true)
	output := restore()
	require.Error(t, err)
	assert.Equal(t, 2, pushed, "the first chunk landed before the auth failure and is reported as pushed")
	assert.Contains(t, output, "SSH authentication failed")
}

// TestFlushCheckpointRefs_RejectionProvesRemoteReachable: once the remote has
// answered a chunk — even with a rejection — a later connect error is
// transient, not "unreachable": the flush carries on to the per-ref fallback
// instead of abandoning refs it could still land.
func TestFlushCheckpointRefs_RejectionProvesRemoteReachable(t *testing.T) {
	shrinkRefPushChunkSize(t, 2)
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 4)
	prepareGitRefsPrePush(t, workDir, "ssh://git@example.invalid"+bareDir)
	installBlockRefHook(t, bareDir, refs[0].String())

	// A fake ssh that runs the remote command locally, except the second
	// connection, which fails the way an unreachable host does.
	countFile := filepath.Join(t.TempDir(), "attempts")
	fakeSSH := filepath.Join(t.TempDir(), "ssh")
	script := "#!/bin/sh\necho attempt >> '" + countFile + "'\n" +
		"n=$(wc -l < '" + countFile + "' | tr -d ' ')\n" +
		"if [ \"$n\" = 2 ]; then echo 'ssh: connect to host example.invalid port 22: Connection refused' >&2; exit 255; fi\n" +
		"for last; do :; done\nexec sh -c \"$last\"\n"
	require.NoError(t, os.WriteFile(fakeSSH, []byte(script), 0o755))
	t.Setenv("GIT_SSH_COMMAND", fakeSSH)

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	enqueueRefs(t, repo, refs)

	restore := captureStderr(t)
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	output := restore()

	assert.NotContains(t, output, "Couldn't reach", "the remote answered the first chunk")
	assert.Equal(t, refHashOf(t, repo, refs[1]), remoteRefHash(t, bareDir, refs[1]),
		"the rejected chunk's healthy ref lands through the fallback")
}
