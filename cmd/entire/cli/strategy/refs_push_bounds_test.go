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
	assert.Contains(t, output, "stay queued for the next push")

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

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	queue := enqueueRefs(t, repo, refs)

	restore := captureStderr(t)
	err = NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin")
	output := restore()
	require.NoError(t, err)

	// An exhausted budget still buys one attempt: aborting before any work
	// would starve the queue instead of draining it a little at a time. The
	// remaining count pins that, and holds whether or not the already-dead
	// deadline let the attempt reach the remote at all.
	assert.Contains(t, output, "budget")
	assert.Contains(t, output, "exhausted")
	assert.Contains(t, output, fmt.Sprintf("%d checkpoint ref(s) stay queued", len(refs)-1))
	assert.LessOrEqual(t, countedAttempts(t, countFile), 2, "the deadline must cut the fallback, not let it walk the queue")

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
