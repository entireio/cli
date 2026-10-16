package strategy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

const pushingGitPIDProbeEnv = "ENTIRE_TEST_PUSHING_GIT_PID_PROBE"

// TestHelperPrintPushingGitPID is not a test: TestPushingGitPID_FindsGitAncestor
// runs the test binary as a git alias, the way git runs a hook, and this prints
// what the hook would record.
func TestHelperPrintPushingGitPID(t *testing.T) {
	t.Parallel()
	if os.Getenv(pushingGitPIDProbeEnv) != "1" {
		t.Skip("helper process only")
	}
	fmt.Printf("PUSHING_GIT_PID=%d\n", pushingGitPID())
}

func TestPushingGitPID_FindsGitAncestor(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	require.NoError(t, err)
	// git → sh → test binary, like git → sh → entire for a pre-push hook.
	cmd := exec.CommandContext(t.Context(), "git", "-c",
		"alias.probe=!\""+filepath.ToSlash(exe)+"\" -test.run=^TestHelperPrintPushingGitPID$", "probe")
	cmd.Env = append(os.Environ(), pushingGitPIDProbeEnv+"=1")
	var out bytes.Buffer
	cmd.Stdout = &out
	require.NoError(t, cmd.Start())
	gitPID := cmd.Process.Pid
	require.NoError(t, cmd.Wait(), out.String())

	_, after, found := strings.Cut(out.String(), "PUSHING_GIT_PID=")
	require.True(t, found, out.String())
	got, err := strconv.Atoi(strings.Fields(after)[0])
	require.NoError(t, err)
	assert.Equal(t, gitPID, got)
}

// startGitProcess starts a git process that runs until the returned stop is
// called. stop also reaps it: an unreaped child stays visible as git.
func startGitProcess(t *testing.T) (pid int, stop func()) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "cat-file", "--batch")
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	var once sync.Once
	stop = func() {
		once.Do(func() {
			_ = stdin.Close()
			_ = cmd.Wait() //nolint:errcheck // a stopped helper's exit status is irrelevant
		})
	}
	t.Cleanup(stop)
	return cmd.Process.Pid, stop
}

// shortenUserPushWaits makes the settle delays test-sized. Not parallel-safe.
func shortenUserPushWaits(t *testing.T, settle, unknown time.Duration) {
	t.Helper()
	restoreSettle, restoreUnknown := userPushSettle, unknownUserPushDelay
	userPushSettle, unknownUserPushDelay = settle, unknown
	t.Cleanup(func() { userPushSettle, unknownUserPushDelay = restoreSettle, restoreUnknown })
}

func TestWaitForUserPushes_WaitsForGitToExit(t *testing.T) {
	shortenUserPushWaits(t, 10*time.Millisecond, 10*time.Millisecond)
	coord := checkpoint.NewUploadCoordinator(t.TempDir())
	pid, stop := startGitProcess(t)
	require.True(t, isLiveGitProcess(pid))
	require.NoError(t, coord.NoteUserPush(checkpoint.UserPush{PID: pid, At: time.Now()}))

	go func() { time.Sleep(300 * time.Millisecond); stop() }()
	start := time.Now()
	waitForUserPushes(t.Context(), coord)

	assert.GreaterOrEqual(t, time.Since(start), 300*time.Millisecond)
	st, err := coord.State()
	require.NoError(t, err)
	assert.Empty(t, st.UserPushes, "marks are consumed")
}

// A push recorded while the worker waits — `git push && git push --tags` —
// is waited for too.
func TestWaitForUserPushes_WaitsForPushRecordedMeanwhile(t *testing.T) {
	shortenUserPushWaits(t, 200*time.Millisecond, 10*time.Millisecond)
	coord := checkpoint.NewUploadCoordinator(t.TempDir())
	first, stopFirst := startGitProcess(t)
	second, stopSecond := startGitProcess(t)
	require.NoError(t, coord.NoteUserPush(checkpoint.UserPush{PID: first, At: time.Now()}))

	go func() {
		time.Sleep(100 * time.Millisecond)
		stopFirst()
		time.Sleep(50 * time.Millisecond) // inside the settle after the first exits
		assert.NoError(t, coord.NoteUserPush(checkpoint.UserPush{PID: second, At: time.Now()}))
		time.Sleep(400 * time.Millisecond)
		stopSecond()
	}()
	start := time.Now()
	waitForUserPushes(t.Context(), coord)

	assert.GreaterOrEqual(t, time.Since(start), 550*time.Millisecond)
	assert.False(t, isLiveGitProcess(second))
}

// A push whose git process the hook could not find gets a fixed delay.
func TestWaitForUserPushes_UnknownPIDWaitsFixedDelay(t *testing.T) {
	shortenUserPushWaits(t, 10*time.Millisecond, 300*time.Millisecond)
	coord := checkpoint.NewUploadCoordinator(t.TempDir())
	require.NoError(t, coord.NoteUserPush(checkpoint.UserPush{At: time.Now()}))

	start := time.Now()
	waitForUserPushes(t.Context(), coord)
	assert.GreaterOrEqual(t, time.Since(start), 280*time.Millisecond)
}

func TestIsPushRaceRejection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		output string
		want   bool
	}{
		{"To https://github.com/o/r.git\n ! [remote rejected] main -> main (failed)\nerror: failed to push some refs", true},
		{" ! [remote rejected] main -> main (missing necessary objects)\n", true},
		{"remote: fatal error in commit_refs\n ! [remote rejected] x -> x (failure)", true},
		{" ! [remote rejected] main -> main (pre-receive hook declined)\n", false},
		{" ! [rejected]        main -> main (fetch first)\n", false},
		{"fatal: unable to access 'https://github.com/o/r.git/': Could not resolve host", false},
	} {
		assert.Equal(t, tc.want, isPushRaceRejection(tc.output), tc.output)
	}
}

// swapCheckpointRefsPush replaces the batch push. Not parallel-safe.
func swapCheckpointRefsPush(t *testing.T, push func(context.Context, remote.PushOptions) (remote.PushResult, error)) {
	t.Helper()
	restorePush, restoreDelays := pushCheckpointRefs, pushRaceRetryDelays
	pushCheckpointRefs, pushRaceRetryDelays = push, []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { pushCheckpointRefs, pushRaceRetryDelays = restorePush, restoreDelays })
}

func TestBatchPushRefs_RetriesPushRaceRejection(t *testing.T) {
	calls := 0
	swapCheckpointRefsPush(t, func(context.Context, remote.PushOptions) (remote.PushResult, error) {
		calls++
		if calls == 1 {
			return remote.PushResult{Output: " ! [remote rejected] refs/entire/x -> refs/entire/x (failed)\n"}, errors.New("git push: exit status 1")
		}
		return remote.PushResult{}, nil
	})
	require.NoError(t, batchPushRefs(t.Context(), "origin", []plumbing.ReferenceName{"refs/entire/x"}))
	assert.Equal(t, 2, calls)
}

func TestBatchPushRefs_DoesNotRetryOtherFailures(t *testing.T) {
	calls := 0
	swapCheckpointRefsPush(t, func(context.Context, remote.PushOptions) (remote.PushResult, error) {
		calls++
		return remote.PushResult{Output: " ! [rejected] refs/entire/x -> refs/entire/x (fetch first)\n"}, errors.New("git push: exit status 1")
	})
	require.Error(t, batchPushRefs(t.Context(), "origin", []plumbing.ReferenceName{"refs/entire/x"}))
	assert.Equal(t, 1, calls)
}

// Inline uploads (CI, the setting) settle before git sends the user's pack.
func TestPrePush_InlineUploadSettlesAfterPushing(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 1)
	prepareGitRefsPrePush(t, workDir, bareDir)
	t.Setenv("CI", "true")
	t.Setenv(CheckpointUploadForegroundEnv, "")
	restore := inlineUploadSettle
	inlineUploadSettle = 300 * time.Millisecond
	t.Cleanup(func() { inlineUploadSettle = restore })

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	enqueueRefs(t, repo, refs)

	start := time.Now()
	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	assert.GreaterOrEqual(t, time.Since(start), 300*time.Millisecond)
	out := testutil.RunGit(t, bareDir, "for-each-ref", "--format=%(refname)", "refs/entire/")
	assert.Contains(t, out, refs[0].String(), "pushed inline")
}

// `entire trail create` runs git push itself: the hook only records the upload,
// and the command runs it once the push returns.
func TestUploadRequestedCheckpoints_RunsWhatTheHookRecorded(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 3)
	countFile := filepath.Join(t.TempDir(), "attempts")
	prepareGitRefsPrePush(t, workDir, bareDir)
	installCountingHook(t, bareDir, countFile, 0)
	spawned := enableBackgroundUpload(t)
	t.Setenv(CheckpointUploadAfterPushEnv, "1")

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	defer repo.Close()
	queue := enqueueRefs(t, repo, refs)

	require.NoError(t, NewManualCommitStrategy().PrePushFromGitHook(t.Context(), "origin"))
	assert.Equal(t, 0, countedAttempts(t, countFile), "the hook must not push")
	assert.Empty(t, *spawned, "no worker: the caller uploads")

	var out bytes.Buffer
	assert.Equal(t, 3, UploadRequestedCheckpoints(t.Context(), &out))
	assert.Contains(t, out.String(), "Pushed 3 checkpoint ref(s)")
	remaining, err := queue.Drain()
	require.NoError(t, err)
	assert.Empty(t, remaining)
	assert.False(t, uploadCoordinator(t, repo).HasRequest())
}
