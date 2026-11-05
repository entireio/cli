package strategy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
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
	id, _ := pushingGit()
	fmt.Printf("PUSHING_GIT_PID=%d\n", id.PID)
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

// gitIdentity fingerprints a git process the test started.
func gitIdentity(t *testing.T, pid int) proclive.Identity {
	t.Helper()
	id, ok := gitProcessIdentity(pid)
	if !ok {
		t.Skip("cannot fingerprint processes on this platform")
	}
	return id
}

// stopLater stops a test git process after a short delay and records when it
// did, so a test can assert the wait ended after the push did — an ordering,
// not a duration, which load on the machine cannot fail.
func stopLater(stop func()) func() (time.Time, bool) {
	var mu sync.Mutex
	var at time.Time
	go func() {
		time.Sleep(100 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		stop()
		at = time.Now()
	}()
	return func() (time.Time, bool) {
		mu.Lock()
		defer mu.Unlock()
		return at, !at.IsZero()
	}
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
	id := gitIdentity(t, pid)
	require.True(t, isLiveGitProcess(id))
	require.NoError(t, coord.NoteUserPush(checkpoint.UserPush{Process: id, At: time.Now()}))

	stopped := stopLater(stop)
	waitForUserPushes(t.Context(), coord)

	_, done := stopped()
	assert.True(t, done, "the wait returned before the push exited")
	st, err := coord.State()
	require.NoError(t, err)
	assert.Empty(t, st.UserPushes, "marks are consumed")
}

// A push recorded while the worker waits — `git push && git push --tags` —
// is waited for too.
func TestWaitForUserPushes_WaitsForPushRecordedMeanwhile(t *testing.T) {
	// A long settle, so recording the second push lands inside it however
	// slowly this goroutine is scheduled.
	shortenUserPushWaits(t, 2*time.Second, 10*time.Millisecond)
	coord := checkpoint.NewUploadCoordinator(t.TempDir())
	first, stopFirst := startGitProcess(t)
	second, stopSecond := startGitProcess(t)
	secondID := gitIdentity(t, second)
	require.NoError(t, coord.NoteUserPush(checkpoint.UserPush{Process: gitIdentity(t, first), At: time.Now()}))

	waited := make(chan struct{})
	go func() {
		waitForUserPushes(t.Context(), coord)
		close(waited)
	}()
	stopFirst()
	time.Sleep(100 * time.Millisecond) // into the settle after the first push exits
	require.NoError(t, coord.NoteUserPush(checkpoint.UserPush{Process: secondID, At: time.Now()}))
	stoppedSecond := stopLater(stopSecond)
	<-waited

	_, done := stoppedSecond()
	assert.True(t, done, "the wait returned before the push recorded meanwhile exited")
	assert.False(t, isLiveGitProcess(secondID))
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
		// The checkpoint push's own --porcelain form.
		{"To https://github.com/o/r.git\n!\trefs/entire/checkpoints/ab/c:refs/entire/checkpoints/ab/c\t[remote rejected] (failed)\nDone\n", true},
		{"!\trefs/entire/x:refs/entire/x\t[remote rejected] (missing necessary objects)\n", true},
		{"!\trefs/entire/x:refs/entire/x\t[remote rejected] (pre-receive hook declined)\n", false},
		{"!\trefs/entire/x:refs/entire/x\t[rejected] (fetch first)\n", false},
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

func TestBatchPushCheckpointRefs_RetriesPushRaceRejection(t *testing.T) {
	calls := 0
	swapCheckpointRefsPush(t, func(context.Context, remote.PushOptions) (remote.PushResult, error) {
		calls++
		if calls == 1 {
			return remote.PushResult{Output: "!\trefs/entire/x:refs/entire/x\t[remote rejected] (failed)\n"}, errors.New("git push: exit status 1")
		}
		return remote.PushResult{}, nil
	})
	require.NoError(t, batchPushCheckpointRefs(t.Context(), "origin", []checkpointRefPush{{name: "refs/entire/x"}}))
	assert.Equal(t, 2, calls)
}

func TestBatchPushCheckpointRefs_DoesNotRetryOtherFailures(t *testing.T) {
	calls := 0
	swapCheckpointRefsPush(t, func(context.Context, remote.PushOptions) (remote.PushResult, error) {
		calls++
		return remote.PushResult{Output: " ! [rejected] refs/entire/x -> refs/entire/x (fetch first)\n"}, errors.New("git push: exit status 1")
	})
	require.Error(t, batchPushCheckpointRefs(t.Context(), "origin", []checkpointRefPush{{name: "refs/entire/x"}}))
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

// The two-minute limit bounds taking new rounds, not a push still running: a
// slow push is waited for to its end, and then settled.
func TestWaitForUserPushes_LivePushOutlastsRoundLimit(t *testing.T) {
	shortenUserPushWaits(t, 200*time.Millisecond, 10*time.Millisecond)
	restore := userPushWaitLimit
	userPushWaitLimit = 50 * time.Millisecond
	t.Cleanup(func() { userPushWaitLimit = restore })
	coord := checkpoint.NewUploadCoordinator(t.TempDir())
	pid, stop := startGitProcess(t)
	require.NoError(t, coord.NoteUserPush(checkpoint.UserPush{Process: gitIdentity(t, pid), At: time.Now()}))

	stopped := stopLater(stop)
	waitForUserPushes(t.Context(), coord)

	at, done := stopped()
	require.True(t, done, "the round limit ended the wait before the push exited")
	assert.GreaterOrEqual(t, time.Since(at), userPushSettle, "and the settle follows the exit")
}

// A recorded PID now held by another git process (its start time differs) is
// the old push gone, not a push to wait for.
func TestWaitForUserPushes_ReusedPIDIsNotWaitedFor(t *testing.T) {
	shortenUserPushWaits(t, 10*time.Millisecond, 10*time.Millisecond)
	coord := checkpoint.NewUploadCoordinator(t.TempDir())
	pid, _ := startGitProcess(t)
	id := gitIdentity(t, pid)
	if id.Start == "" {
		t.Skip("no process start time on this platform")
	}
	reused := id
	reused.Start += "-earlier"
	require.NoError(t, coord.NoteUserPush(checkpoint.UserPush{Process: reused, At: time.Now()}))

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	waitForUserPushes(ctx, coord)

	require.NoError(t, ctx.Err(), "the reused PID was waited on until the deadline")
	assert.True(t, isLiveGitProcess(id), "the process itself is still running")
}

// An exited git its parent has not reaped yet is a zombie, not a live push.
func TestIsLiveGitProcess_ZombieIsNotLive(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("zombie processes are a Unix notion")
	}
	cmd := exec.CommandContext(t.Context(), "git", "--version")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Wait() }) //nolint:errcheck // reaping only
	id := proclive.Identity{PID: cmd.Process.Pid, Name: "git"}
	if got, ok := gitProcessIdentity(cmd.Process.Pid); ok {
		id = got
	}

	deadline := time.Now().Add(10 * time.Second)
	for isLiveGitProcess(id) && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	assert.False(t, isLiveGitProcess(id), "the exited, unreaped git still reads as live")
}

// TestIsPushRaceRejection_RealPorcelainOutput pins the pattern to what
// `git push --porcelain` really prints for a remote rejection, not to a
// hand-written string: a bare remote's pre-receive hook declines the push, and
// the race reasons GitHub gives are substituted into that same real line.
func TestIsPushRaceRejection_RealPorcelainOutput(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithNCheckpointRefs(t, 1)
	t.Chdir(workDir)
	require.NoError(t, os.WriteFile(filepath.Join(bareDir, "hooks", "pre-receive"), []byte("#!/bin/sh\nexit 1\n"), 0o755))

	res, err := remote.PushWithOptions(t.Context(), remote.PushOptions{
		Remote: bareDir, RefSpecs: []string{refs[0].String() + ":" + refs[0].String()}})
	require.Error(t, err)
	const declined = "(pre-receive hook declined)"
	require.Contains(t, res.Output, "[remote rejected] "+declined, "precondition: git's own rejection line")

	assert.False(t, isPushRaceRejection(res.Output), "a hook decline is not the race")
	for _, reason := range []string{"(failed)", "(missing necessary objects)"} {
		assert.True(t, isPushRaceRejection(strings.ReplaceAll(res.Output, declined, reason)), reason)
	}
}

// TestUserPushWait_WindowsAndUnix covers the process lookup, fingerprint and
// wait on every OS CI runs. Its name puts it in the Windows CI job (which runs
// only -run '(Windows|MSYS)'), where the Toolhelp lookup and handle wait would
// otherwise never run. A failure here means the worker could land its upload
// right on the user's push, which GitHub rejects.
func TestUserPushWait_WindowsAndUnix(t *testing.T) {
	shortenUserPushWaits(t, 10*time.Millisecond, 10*time.Millisecond)
	coord := checkpoint.NewUploadCoordinator(t.TempDir())
	pid, stop := startGitProcess(t)

	id, ok := gitProcessIdentity(pid)
	require.True(t, ok, "the running git process must be found")
	require.NotEmpty(t, id.Start, "and fingerprinted, so a reused PID is told apart")
	require.True(t, isLiveGitProcess(id))
	reused := id
	reused.Start += "-earlier"
	assert.False(t, isLiveGitProcess(reused), "a PID now held by another process is not the push")

	require.NoError(t, coord.NoteUserPush(checkpoint.UserPush{Process: id, At: time.Now()}))
	stopped := stopLater(stop)
	waitForUserPushes(t.Context(), coord)
	_, done := stopped()
	assert.True(t, done, "the wait returned before the push exited")
	assert.False(t, isLiveGitProcess(id))
}
