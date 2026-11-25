package strategy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// enableBackgroundUpload opts this test into the hand-off, which in-process
// tests otherwise skip, clears the CI markers a CI runner sets, records spawns
// instead of forking, and shortens the worker's settle delays. Not
// parallel-safe: it swaps package state.
func enableBackgroundUpload(t *testing.T) *[]string {
	t.Helper()
	for _, name := range ciEnvVars {
		t.Setenv(name, "")
	}
	t.Setenv(CheckpointUploadForegroundEnv, "")
	shortenUserPushWaits(t, 10*time.Millisecond, 10*time.Millisecond)
	restoreAllowed, restoreSpawn := backgroundUploadInTests, checkpointUploadSpawn
	var spawned []string
	backgroundUploadInTests = true
	checkpointUploadSpawn = func(root string) bool { spawned = append(spawned, root); return true }
	t.Cleanup(func() { backgroundUploadInTests, checkpointUploadSpawn = restoreAllowed, restoreSpawn })
	return &spawned
}

// openUploadTestRepo opens workDir, closed when the test ends.
func openUploadTestRepo(t *testing.T, workDir string) *git.Repository {
	t.Helper()
	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	t.Cleanup(func() { repo.Close() })
	return repo
}

func uploadCoordinator(t *testing.T, repo *git.Repository) *checkpoint.UploadCoordinator {
	t.Helper()
	coord, err := checkpoint.UploadCoordinatorForRepo(repo)
	require.NoError(t, err)
	return coord
}

// TestPrePush_HandsQueueToWorkerWithoutPushing: the hook pushes nothing itself
// — pushed from the hook, checkpoints land just before git's own pack and
// GitHub rejects the user's push — and leaves the queue, a request and a mark
// for this push to one background worker.
func TestPrePush_HandsQueueToWorkerWithoutPushing(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 3)
	countFile := filepath.Join(t.TempDir(), "attempts")
	prepareGitRefsPrePush(t, workDir, bareDir)
	installCountingHook(t, bareDir, countFile, 0)
	spawned := enableBackgroundUpload(t)

	repo := openUploadTestRepo(t, workDir)
	queue := enqueueRefs(t, repo, refs)

	restore := captureStderr(t)
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	output := restore()

	assert.Equal(t, 0, countedAttempts(t, countFile), "the hook must not push")
	assert.Len(t, *spawned, 1, "one background worker")
	assert.Contains(t, output, "Uploading checkpoints in the background")
	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.ElementsMatch(t, refs, remaining)
	st, err := uploadCoordinator(t, repo).State()
	require.NoError(t, err)
	require.NotNil(t, st.Job.Request, "the hand-off is recorded for the worker")
	assert.Equal(t, "origin", st.Job.Request.Remote)
	assert.Len(t, st.Job.UserPushes, 1, "the worker waits for this push")
}

// TestPrePush_RecordsUserPushWithEmptyQueue: a push with nothing queued starts
// no worker but is still recorded, so a worker uploading an earlier push's
// checkpoints waits for it too (`git push && git push --tags`).
func TestPrePush_RecordsUserPushWithEmptyQueue(t *testing.T) {
	workDir, bareDir, _ := setupRepoWithNCheckpointRefs(t, 1)
	prepareGitRefsPrePush(t, workDir, bareDir)
	spawned := enableBackgroundUpload(t)

	repo := openUploadTestRepo(t, workDir)

	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))

	assert.Empty(t, *spawned)
	st, err := uploadCoordinator(t, repo).State()
	require.NoError(t, err)
	assert.Nil(t, st.Job.Request)
	assert.Len(t, st.Job.UserPushes, 1)
}

// TestPrePush_RunningWorkerOwnsTheQueue: while a worker holds the upload lock,
// the hook neither pushes the same refs alongside it nor waits for it — it
// leaves a request the worker picks up after its current pass.
func TestPrePush_RunningWorkerOwnsTheQueue(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 3)
	countFile := filepath.Join(t.TempDir(), "attempts")
	prepareGitRefsPrePush(t, workDir, bareDir)
	installCountingHook(t, bareDir, countFile, 0)
	enableBackgroundUpload(t)

	repo := openUploadTestRepo(t, workDir)
	queue := enqueueRefs(t, repo, refs)
	coord := uploadCoordinator(t, repo)
	release, err := coord.Lock(t.Context())
	require.NoError(t, err)
	defer release()

	restore := captureStderr(t)
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	output := restore()

	assert.Equal(t, 0, countedAttempts(t, countFile), "nothing pushed alongside the running worker")
	assert.Contains(t, output, "Uploading checkpoints in the background")
	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.ElementsMatch(t, refs, remaining)
	st, err := coord.State()
	require.NoError(t, err)
	require.NotNil(t, st.Job.Request)
}

// TestPrePush_ForegroundEnvKeepsUploadInline: `entire trail create` and the
// test harnesses set the foreground env; then nothing is handed off and the
// pre-push budget applies as before.
func TestPrePush_ForegroundEnvKeepsUploadInline(t *testing.T) {
	spawned := enableBackgroundUpload(t)
	t.Setenv(CheckpointUploadForegroundEnv, "1")
	assert.False(t, backgroundCheckpointUploadEnabled(t.Context()))
	t.Setenv(CheckpointUploadForegroundEnv, "")
	t.Setenv("CI", "true")
	assert.False(t, backgroundCheckpointUploadEnabled(t.Context()), "CI checkouts are discarded with the job")
	t.Setenv("CI", "")
	assert.False(t, backgroundCheckpointUploadEnabled(withinUploadWorker(t.Context())), "the worker never hands off")
	assert.Empty(t, *spawned)
}

// TestRunCheckpointUploadWorker_DrainsQueueAndRecords runs the worker body
// in-process against a healthy remote.
func TestRunCheckpointUploadWorker_DrainsQueueAndRecords(t *testing.T) {
	shrinkRefPushChunkSize(t, 2)
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 5)
	prepareGitRefsPrePush(t, workDir, bareDir)

	repo := openUploadTestRepo(t, workDir)
	queue := enqueueRefs(t, repo, refs)
	coord := uploadCoordinator(t, repo)
	require.NoError(t, coord.Request(checkpoint.UploadRequest{Remote: "origin"}))

	RunCheckpointUploadWorker(t.Context())

	for _, ref := range refs {
		assert.Equal(t, refHashOf(t, repo, ref), remoteRefHash(t, bareDir, ref))
	}
	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.Empty(t, remaining)
	st, err := coord.State()
	require.NoError(t, err)
	assert.Nil(t, st.Job.Request, "the request was consumed")
	require.NotNil(t, st.Last)
	assert.False(t, st.Last.FinishedAt.IsZero())
	assert.Equal(t, len(refs), st.Last.Done)
	assert.Empty(t, st.Last.Error)
}

// TestRunCheckpointUploadWorker_FailureReachesNextPush: the worker has no
// terminal, so a run that left refs queued is recorded and the next push says
// so once.
func TestRunCheckpointUploadWorker_FailureReachesNextPush(t *testing.T) {
	queued := maxConsecutiveRefPushFailures + 1
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, queued)
	prepareGitRefsPrePush(t, workDir, bareDir)
	installCheckpointRejectHook(t, bareDir, false, filepath.Join(t.TempDir(), "attempts"))

	repo := openUploadTestRepo(t, workDir)
	enqueueRefs(t, repo, refs)
	coord := uploadCoordinator(t, repo)
	require.NoError(t, coord.Request(checkpoint.UploadRequest{Remote: "origin"}))

	restore := captureStderr(t)
	RunCheckpointUploadWorker(t.Context())
	restore()

	st, err := coord.State()
	require.NoError(t, err)
	require.NotNil(t, st.Last)
	assert.NotEmpty(t, st.Last.Error)
	assert.Contains(t, st.Last.Detail, checkpointUnblockURL, "the remote's reason survives the worker's null stderr")

	buf := captureStderrWriter(t)
	restore = captureStderr(t)
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	restore()
	assert.Contains(t, buf.String(), "Last background checkpoint upload stopped: "+st.Last.Error)
	assert.Contains(t, buf.String(), checkpointUnblockURL, "the next push shows why, not only that")

	st, err = coord.State()
	require.NoError(t, err)
	assert.Empty(t, st.Last.Error, "reported once")
	assert.Empty(t, st.Last.Detail, "reported once")
}

// TestRunCheckpointUploadWorker_ExitsWhenLockHeld: one worker per repository.
func TestRunCheckpointUploadWorker_ExitsWhenLockHeld(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 2)
	prepareGitRefsPrePush(t, workDir, bareDir)

	repo := openUploadTestRepo(t, workDir)
	queue := enqueueRefs(t, repo, refs)
	coord := uploadCoordinator(t, repo)
	require.NoError(t, coord.Request(checkpoint.UploadRequest{Remote: "origin"}))
	release, err := coord.Lock(t.Context())
	require.NoError(t, err)
	defer release()

	RunCheckpointUploadWorker(t.Context())

	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.ElementsMatch(t, refs, remaining, "the lock holder owns the queue")
	st, err := coord.State()
	require.NoError(t, err)
	assert.NotNil(t, st.Job.Request, "the request stays for the holder")
}

// TestPushQueuedCheckpointRefs_WaitsForBackgroundWorker: the explicit
// migration push never pushes alongside a running worker.
func TestPushQueuedCheckpointRefs_WaitsForBackgroundWorker(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 2)
	prepareGitRefsPrePush(t, workDir, bareDir)

	repo := openUploadTestRepo(t, workDir)
	enqueueRefs(t, repo, refs)
	release, err := uploadCoordinator(t, repo).Lock(t.Context())
	require.NoError(t, err)
	defer release()

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	_, _, err = PushQueuedCheckpointRefs(ctx, repo, "origin")
	require.ErrorContains(t, err, "wait for background checkpoint upload")
	assertRefsAbsentFromRemote(t, bareDir, refs, "nothing pushed while the worker held the queue")
}

// TestOPFPrePushDecision_HonorsCarriedDecision: the worker acts on the hook's
// decision and never re-resolves it.
func TestOPFPrePushDecision_HonorsCarriedDecision(t *testing.T) {
	t.Parallel()
	got, err := opfPrePushDecision(withCarriedOPFDecision(t.Context(), checkpoint.UploadOPFSkip))
	require.NoError(t, err)
	assert.Equal(t, OPFSkip, got)
	got, err = opfPrePushDecision(withCarriedOPFDecision(t.Context(), checkpoint.UploadOPFRun))
	require.NoError(t, err)
	assert.Equal(t, OPFRun, got)
}

// TestReleaseUploadLock_StartsWorkerForWaitingRequest: a push that found the
// lock held left a request and relied on the holder; a holder that is not a
// worker must start one on release, or the request waits for the next push.
func TestReleaseUploadLock_StartsWorkerForWaitingRequest(t *testing.T) {
	workDir, _, _ := setupRepoWithNCheckpointRefs(t, 1)
	prepareGitRefsPrePush(t, workDir, t.TempDir())
	spawned := enableBackgroundUpload(t)

	repo := openUploadTestRepo(t, workDir)
	coord := uploadCoordinator(t, repo)

	release, err := coord.Lock(t.Context())
	require.NoError(t, err)
	releaseUploadLock(t.Context(), coord, release)
	assert.Empty(t, *spawned, "no request, no worker")

	release, err = coord.Lock(t.Context())
	require.NoError(t, err)
	require.NoError(t, coord.Request(checkpoint.UploadRequest{Remote: "origin"}))
	releaseUploadLock(t.Context(), coord, release)
	assert.Len(t, *spawned, 1)

	release, err = coord.Lock(t.Context())
	require.NoError(t, err)
	releaseUploadLock(withinUploadWorker(t.Context()), coord, release)
	assert.Len(t, *spawned, 1, "the worker loops over requests itself")

	t.Setenv(CheckpointUploadForegroundEnv, "1")
	release, err = coord.Lock(t.Context())
	require.NoError(t, err)
	releaseUploadLock(t.Context(), coord, release)
	assert.Len(t, *spawned, 1, "uploads kept inline never start a background process")
}

// TestRunCheckpointUploadWorker_WithholdsWhenRequiredOPFIsOff: the hook
// decided OPF runs; a worker that sees OPF off must not ship the queue as is.
func TestRunCheckpointUploadWorker_WithholdsWhenRequiredOPFIsOff(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 2)
	prepareGitRefsPrePush(t, workDir, bareDir)

	repo := openUploadTestRepo(t, workDir)
	queue := enqueueRefs(t, repo, refs)
	coord := uploadCoordinator(t, repo)
	require.NoError(t, coord.Request(checkpoint.UploadRequest{Remote: "origin", OPFDecision: checkpoint.UploadOPFRun}))

	RunCheckpointUploadWorker(t.Context())

	assertRefsAbsentFromRemote(t, bareDir, refs, "nothing ships unscanned")
	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.ElementsMatch(t, refs, remaining)
	st, err := coord.State()
	require.NoError(t, err)
	require.NotNil(t, st.Last)
	assert.Contains(t, st.Last.Error, "privacy filter")
}

// TestPrePush_ForegroundNeverHandsOffWhenLockHeld: an inline-only push (CI,
// trail create, the setting) that finds another upload holding the lock
// leaves its refs queued rather than starting a background process.
func TestPrePush_ForegroundNeverHandsOffWhenLockHeld(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 2)
	prepareGitRefsPrePush(t, workDir, bareDir)
	spawned := enableBackgroundUpload(t)
	t.Setenv(CheckpointUploadForegroundEnv, "1")
	restoreWait := foregroundUploadLockWait
	foregroundUploadLockWait = 200 * time.Millisecond
	t.Cleanup(func() { foregroundUploadLockWait = restoreWait })

	repo := openUploadTestRepo(t, workDir)
	enqueueRefs(t, repo, refs)
	coord := uploadCoordinator(t, repo)
	release, err := coord.Lock(t.Context())
	require.NoError(t, err)
	defer release()

	restore := captureStderr(t)
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	output := restore()

	assert.Empty(t, *spawned)
	assert.Contains(t, output, "stay queued for the next push")
	st, err := coord.State()
	require.NoError(t, err)
	assert.Nil(t, st.Job.Request, "no request is left for a background process")
}

// TestRunCheckpointUploadWorker_RespawnsForRequestLeftAtExit: a worker that
// runs out of passes while pushes keep asking for more hands the last request
// to a fresh worker instead of leaving it for the next push.
func TestRunCheckpointUploadWorker_RespawnsForRequestLeftAtExit(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 1)
	prepareGitRefsPrePush(t, workDir, bareDir)
	spawned := enableBackgroundUpload(t)

	repo := openUploadTestRepo(t, workDir)
	enqueueRefs(t, repo, refs)
	coord := uploadCoordinator(t, repo)
	require.NoError(t, coord.Request(checkpoint.UploadRequest{Remote: "origin"}))
	// Every pass fails to reach the remote, and a push asks again meanwhile.
	passes := 0
	swapCheckpointRefsPush(t, func(context.Context, remote.PushOptions) (remote.PushResult, error) {
		passes++
		if err := coord.Request(checkpoint.UploadRequest{Remote: "origin"}); err != nil {
			return remote.PushResult{}, err
		}
		return remote.PushResult{}, errors.New("ssh: Could not resolve hostname example.invalid")
	})

	restore := captureStderr(t)
	RunCheckpointUploadWorker(t.Context())
	restore()

	assert.Equal(t, checkpointUploadMaxPasses, passes)
	assert.True(t, coord.HasRequest())
	assert.Len(t, *spawned, 1, "a fresh worker takes the request left at exit")
}

// TestPrePush_InlineRecordsUserPush: an inline upload records its push, so a
// worker left from an earlier push waits for it too.
func TestPrePush_InlineRecordsUserPush(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 1)
	prepareGitRefsPrePush(t, workDir, bareDir)
	t.Setenv(CheckpointUploadForegroundEnv, "1")

	repo := openUploadTestRepo(t, workDir)
	enqueueRefs(t, repo, refs)

	restore := captureStderr(t)
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	restore()

	st, err := uploadCoordinator(t, repo).State()
	require.NoError(t, err)
	assert.Len(t, st.Job.UserPushes, 1)
}

// TestRunCheckpointUploadWorker_BudgetEndsPassAsStop: the pass budget bounds
// the batch itself, so running out reads as a budget stop with the ref left
// queued, not as a push failure retried on a dead context.
func TestRunCheckpointUploadWorker_BudgetEndsPassAsStop(t *testing.T) {
	shrinkRefPushChunkSize(t, 1)
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 1)
	prepareGitRefsPrePush(t, workDir, bareDir)
	hook := "#!/bin/sh\ncat >/dev/null\nsleep 10\nexit 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(bareDir, "hooks", "pre-receive"), []byte(hook), 0o755))
	restoreBudget := checkpointUploadDeliveryBudget
	checkpointUploadDeliveryBudget = time.Second
	t.Cleanup(func() { checkpointUploadDeliveryBudget = restoreBudget })

	repo := openUploadTestRepo(t, workDir)
	queue := enqueueRefs(t, repo, refs)
	coord := uploadCoordinator(t, repo)
	require.NoError(t, coord.Request(checkpoint.UploadRequest{Remote: "origin"}))

	restore := captureStderr(t)
	RunCheckpointUploadWorker(t.Context())
	restore()

	st, err := coord.State()
	require.NoError(t, err)
	require.NotNil(t, st.Last)
	assert.Contains(t, st.Last.Error, "exhausted")
	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.Equal(t, refs, remaining)
}

// TestRunCheckpointUploadPass_OPFGateRunsAfterPushWait: the wait for the user's
// pushes can last minutes, and checkpoints queued meanwhile must still pass the
// privacy filter, so the gate runs after it, right before the queue is drained.
func TestRunCheckpointUploadPass_OPFGateRunsAfterPushWait(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 1)
	prepareGitRefsPrePush(t, workDir, bareDir)
	repo := openUploadTestRepo(t, workDir)
	enqueueRefs(t, repo, refs)
	coord := uploadCoordinator(t, repo)

	var steps []string
	restoreWait, restoreGate := uploadPassWait, uploadPassOPFGate
	t.Cleanup(func() { uploadPassWait, uploadPassOPFGate = restoreWait, restoreGate })
	uploadPassWait = func(context.Context, *checkpoint.UploadCoordinator) { steps = append(steps, "wait") }
	uploadPassOPFGate = func(ctx context.Context, repo *git.Repository) error {
		steps = append(steps, "gate")
		return restoreGate(ctx, repo)
	}

	require.NoError(t, coord.Request(checkpoint.UploadRequest{Remote: "origin"}))
	restore := captureStderr(t)
	runCheckpointUploadPass(t.Context(), repo, coord)
	restore()

	require.GreaterOrEqual(t, len(steps), 2)
	assert.Equal(t, []string{"wait", "gate"}, steps[:2])
}

// TestBackgroundCheckpointUploadEnabled_SettingOptsOut: the documented escape
// hatch for environments the CI detection misses, read from settings.json.
func TestBackgroundCheckpointUploadEnabled_SettingOptsOut(t *testing.T) {
	enableBackgroundUpload(t)
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	t.Chdir(tmp)
	paths.ClearWorktreeRootCache()

	writeSettingsJSON(t, tmp, `{"enabled":true,"strategy_options":{"background_checkpoint_upload":true}}`)
	assert.True(t, backgroundCheckpointUploadEnabled(t.Context()), "precondition: on when nothing opts out")

	writeSettingsJSON(t, tmp, `{"enabled":true,"strategy_options":{"background_checkpoint_upload":false}}`)
	assert.False(t, backgroundCheckpointUploadEnabled(t.Context()))
}

// TestRunCheckpointUploadWorker_KeepsNoticesForNextPush: the worker's stderr is
// the null device, so a notice the pass writes (here, that a multi-URL remote's
// other push URLs get no checkpoints) is recorded and shown by the next push,
// while the process-wide stderrWriter is left alone.
func TestRunCheckpointUploadWorker_KeepsNoticesForNextPush(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 2)
	prepareGitRefsPrePush(t, workDir, bareDir)
	testutil.RunGit(t, workDir, "remote", "set-url", "--add", "--push", "origin", bareDir)
	testutil.RunGit(t, workDir, "remote", "set-url", "--add", "--push", "origin", t.TempDir())

	repo := openUploadTestRepo(t, workDir)
	enqueueRefs(t, repo, refs)
	coord := uploadCoordinator(t, repo)
	require.NoError(t, coord.Request(checkpoint.UploadRequest{Remote: "origin"}))

	processWide := captureStderrWriter(t)
	restore := captureStderr(t)
	RunCheckpointUploadWorker(t.Context())
	restore()

	assert.Empty(t, processWide.String(), "the pass does not write to the process-wide writer")
	st, err := coord.State()
	require.NoError(t, err)
	require.NotNil(t, st.Last)
	assert.Contains(t, st.Last.Announcement, "Checkpoints go to one repository")

	restore = captureStderr(t)
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	restore()
	assert.Contains(t, processWide.String(), "Checkpoints go to one repository", "the next push shows it")
}

// TestRunCheckpointUploadPass_TakesRequestAfterWait: a push recorded while the
// worker waits merges its decision into the request this pass uses, so its
// "run" is not overridden by an earlier push's "skip".
func TestRunCheckpointUploadPass_TakesRequestAfterWait(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 1)
	prepareGitRefsPrePush(t, workDir, bareDir)
	repo := openUploadTestRepo(t, workDir)
	enqueueRefs(t, repo, refs)
	coord := uploadCoordinator(t, repo)
	require.NoError(t, coord.Request(checkpoint.UploadRequest{Remote: "origin", OPFDecision: checkpoint.UploadOPFSkip}))

	restoreWait := uploadPassWait
	t.Cleanup(func() { uploadPassWait = restoreWait })
	uploadPassWait = func(context.Context, *checkpoint.UploadCoordinator) {
		assert.NoError(t, coord.Request(checkpoint.UploadRequest{Remote: "origin", OPFDecision: checkpoint.UploadOPFRun}))
	}

	restore := captureStderr(t)
	runCheckpointUploadPass(t.Context(), repo, coord)
	restore()
	// OPF is off in this process, so a carried "run" withholds the upload;
	// the earlier "skip" would have pushed.
	st, err := coord.State()
	require.NoError(t, err)
	require.NotNil(t, st.Last)
	assert.Contains(t, st.Last.Error, "privacy filter could not run")
	assertRefsAbsentFromRemote(t, bareDir, refs, "nothing ships unscanned")
}

// TestEffectiveOPFDecision_SkipCoversOnlyItsQueue: a skip answered for one
// queue does not cover checkpoints queued after it.
func TestEffectiveOPFDecision_SkipCoversOnlyItsQueue(t *testing.T) {
	t.Parallel()
	workDir, _, refs := setupRepoWithNCheckpointRefs(t, 2)
	repo := openUploadTestRepo(t, workDir)
	enqueueRefs(t, repo, refs[:1])
	entries, err := queuedEntries(t.Context(), repo)
	require.NoError(t, err)
	req := checkpoint.UploadRequest{OPFDecision: checkpoint.UploadOPFSkip, QueueDigest: checkpoint.QueueDigest(entries)}

	assert.Equal(t, checkpoint.UploadOPFSkip, effectiveOPFDecision(t.Context(), repo, req))
	enqueueRefs(t, repo, refs[1:])
	assert.Equal(t, checkpoint.UploadOPFUnset, effectiveOPFDecision(t.Context(), repo, req),
		"a checkpoint queued after the answer gets the non-interactive rules")
	run := checkpoint.UploadRequest{OPFDecision: checkpoint.UploadOPFRun}
	assert.Equal(t, checkpoint.UploadOPFRun, effectiveOPFDecision(t.Context(), repo, run))
}
