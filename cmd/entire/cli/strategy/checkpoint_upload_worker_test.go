package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
)

// enableBackgroundUpload opts this test into the hand-off, which in-process
// tests otherwise skip, clears the CI markers a CI runner sets, and records
// spawns instead of forking. Not parallel-safe: it swaps package state.
func enableBackgroundUpload(t *testing.T, inlineBudget time.Duration) *[]string {
	t.Helper()
	for _, name := range ciEnvVars {
		t.Setenv(name, "")
	}
	t.Setenv(CheckpointUploadForegroundEnv, "")
	restoreAllowed, restoreSpawn, restoreBudget := backgroundUploadInTests, checkpointUploadSpawn, checkpointInlineUploadBudget
	var spawned []string
	backgroundUploadInTests = true
	checkpointUploadSpawn = func(root string) { spawned = append(spawned, root) }
	checkpointInlineUploadBudget = inlineBudget
	t.Cleanup(func() {
		backgroundUploadInTests, checkpointUploadSpawn, checkpointInlineUploadBudget = restoreAllowed, restoreSpawn, restoreBudget
	})
	return &spawned
}

func uploadCoordinator(t *testing.T, repo *git.Repository) *checkpoint.UploadCoordinator {
	t.Helper()
	coord, err := checkpoint.UploadCoordinatorForRepo(repo)
	require.NoError(t, err)
	return coord
}

// TestPrePush_HandsBacklogToBackgroundWorker is the hybrid: the hook uploads
// what fits its inline budget, then hands the rest to one background worker
// instead of holding the user's push — and says so, rather than "stay queued".
func TestPrePush_HandsBacklogToBackgroundWorker(t *testing.T) {
	shrinkRefPushChunkSize(t, 2)
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 6)
	countFile := filepath.Join(t.TempDir(), "attempts")
	prepareGitRefsPrePush(t, workDir, bareDir)
	installCountingHook(t, bareDir, countFile, 1) // first chunk lands, the rest stall
	spawned := enableBackgroundUpload(t, 5*time.Second)

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	queue := enqueueRefs(t, repo, refs)

	restore := captureStderr(t)
	start := time.Now()
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	elapsed := time.Since(start)
	output := restore()

	assert.Less(t, elapsed, 25*time.Second, "the hook must stop at its inline budget, not wait out the stall")
	assert.Len(t, *spawned, 1, "one background worker for the remainder")
	assert.Contains(t, output, "Uploading the remaining 4 checkpoint ref(s) in the background")
	assert.NotContains(t, output, "Stopped pushing", "a handed-off stop is not reported as left for the next push")

	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.ElementsMatch(t, refs[2:], remaining, "the landed chunk left the queue; the worker gets the rest")
	st, err := uploadCoordinator(t, repo).State()
	require.NoError(t, err)
	require.NotNil(t, st.Request, "the hand-off is recorded for the worker")
	assert.Equal(t, "origin", st.Request.Remote)
}

// TestPrePush_NoHandOffWhenRemoteUnreachable: only a budget stop is handed off.
// A worker would fail against an unreachable remote exactly as the hook did.
func TestPrePush_NoHandOffWhenRemoteUnreachable(t *testing.T) {
	workDir, _, refs := setupRepoWithNCheckpointRefs(t, 3)
	prepareGitRefsPrePush(t, workDir, "ssh://git@example.invalid/repo.git")
	fakeSSH := filepath.Join(t.TempDir(), "ssh")
	require.NoError(t, os.WriteFile(fakeSSH,
		[]byte("#!/bin/sh\necho 'ssh: connect to host example.invalid port 22: Connection refused' >&2\nexit 255\n"), 0o755))
	t.Setenv("GIT_SSH_COMMAND", fakeSSH)
	spawned := enableBackgroundUpload(t, 5*time.Second)

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	enqueueRefs(t, repo, refs)

	restore := captureStderr(t)
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	output := restore()

	assert.Empty(t, *spawned)
	assert.Contains(t, output, "Couldn't reach origin")
	assert.NotContains(t, output, "in the background")
}

// TestPrePush_RunningWorkerOwnsTheQueue: while a worker holds the upload lock,
// the hook neither pushes the same refs alongside it nor waits for it — it
// leaves a request the worker picks up after its current pass.
func TestPrePush_RunningWorkerOwnsTheQueue(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 3)
	countFile := filepath.Join(t.TempDir(), "attempts")
	prepareGitRefsPrePush(t, workDir, bareDir)
	installCountingHook(t, bareDir, countFile, 0)
	enableBackgroundUpload(t, 5*time.Second)

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	queue := enqueueRefs(t, repo, refs)
	coord := uploadCoordinator(t, repo)
	release, err := coord.LockWorker(t.Context())
	require.NoError(t, err)
	defer release()

	restore := captureStderr(t)
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	output := restore()

	assert.Equal(t, 0, countedAttempts(t, countFile), "nothing pushed alongside the running worker")
	assert.Contains(t, output, "already running in the background")
	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.ElementsMatch(t, refs, remaining)
	st, err := coord.State()
	require.NoError(t, err)
	require.NotNil(t, st.Request)
}

// TestPrePush_ForegroundEnvKeepsUploadInline: `entire trail create` and the
// test harnesses set the foreground env; then nothing is handed off and the
// pre-push budget applies as before.
func TestPrePush_ForegroundEnvKeepsUploadInline(t *testing.T) {
	spawned := enableBackgroundUpload(t, time.Second)
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

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
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
	assert.Nil(t, st.Request, "the request was consumed")
	require.NotNil(t, st.Last)
	assert.False(t, st.Last.FinishedAt.IsZero())
	assert.Equal(t, len(refs), st.Last.Pushed)
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

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
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
	running, lastErr := CheckpointUploadStatus(t.Context())
	assert.False(t, running)
	assert.Equal(t, st.Last.Error, lastErr)

	buf := captureStderrWriter(t)
	restore = captureStderr(t)
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	restore()
	assert.Contains(t, buf.String(), "Last background checkpoint upload stopped: "+st.Last.Error)

	st, err = coord.State()
	require.NoError(t, err)
	assert.Empty(t, st.Last.Error, "reported once")
}

// TestRunCheckpointUploadWorker_ExitsWhenLockHeld: one worker per repository.
func TestRunCheckpointUploadWorker_ExitsWhenLockHeld(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 2)
	prepareGitRefsPrePush(t, workDir, bareDir)

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	queue := enqueueRefs(t, repo, refs)
	coord := uploadCoordinator(t, repo)
	require.NoError(t, coord.Request(checkpoint.UploadRequest{Remote: "origin"}))
	release, err := coord.LockWorker(t.Context())
	require.NoError(t, err)
	defer release()

	RunCheckpointUploadWorker(t.Context())

	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.ElementsMatch(t, refs, remaining, "the lock holder owns the queue")
	st, err := coord.State()
	require.NoError(t, err)
	assert.NotNil(t, st.Request, "the request stays for the holder")
}

// TestPushQueuedCheckpointRefs_WaitsForBackgroundWorker: the explicit
// migration push never pushes alongside a running worker.
func TestPushQueuedCheckpointRefs_WaitsForBackgroundWorker(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 2)
	prepareGitRefsPrePush(t, workDir, bareDir)

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	enqueueRefs(t, repo, refs)
	release, err := uploadCoordinator(t, repo).LockWorker(t.Context())
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
	spawned := enableBackgroundUpload(t, 5*time.Second)

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	coord := uploadCoordinator(t, repo)

	release, err := coord.LockWorker(t.Context())
	require.NoError(t, err)
	releaseUploadLock(t.Context(), coord, release)
	assert.Empty(t, *spawned, "no request, no worker")

	release, err = coord.LockWorker(t.Context())
	require.NoError(t, err)
	require.NoError(t, coord.Request(checkpoint.UploadRequest{Remote: "origin"}))
	releaseUploadLock(t.Context(), coord, release)
	assert.Len(t, *spawned, 1)

	release, err = coord.LockWorker(t.Context())
	require.NoError(t, err)
	releaseUploadLock(withinUploadWorker(t.Context()), coord, release)
	assert.Len(t, *spawned, 1, "the worker loops over requests itself")
}

// TestRunCheckpointUploadWorker_WithholdsWhenRequiredOPFIsOff: the hook
// decided OPF runs; a worker that sees OPF off must not ship the queue as is.
func TestRunCheckpointUploadWorker_WithholdsWhenRequiredOPFIsOff(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 2)
	prepareGitRefsPrePush(t, workDir, bareDir)

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
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
