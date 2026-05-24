package strategy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withShortPushTimeout replaces pushAttemptTimeout for the test duration and
// restores it via t.Cleanup. Lets us trigger errPushTimedOut without waiting
// the production 2-minute deadline.
func withShortPushTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	original := pushAttemptTimeout
	pushAttemptTimeout = d
	t.Cleanup(func() { pushAttemptTimeout = original })
}

// Regression: a colleague's terminal once showed "Warning: couldn't sync …:
// fetch failed:" with nothing after the colon, because the wrapping discarded
// git's underlying error whenever git produced no stdout. The wrapped error
// must now appear in the message even when output is empty.
//
// Not parallel: t.Chdir() for OpenRepository.
func TestFetchAndRebase_FetchFailure_IncludesUnderlyingError(t *testing.T) {
	tmpDir := setupRepoWithCheckpointBranch(t)
	t.Chdir(tmpDir)

	nonExistent := filepath.Join(t.TempDir(), "does-not-exist")
	err := fetchAndRebaseSessionsCommon(context.Background(), nonExistent, paths.MetadataBranchName)

	require.Error(t, err)
	msg := err.Error()
	require.True(t, strings.HasPrefix(msg, "fetch failed: "), "expected fetch failed prefix, got %q", msg)
	detail := strings.TrimPrefix(msg, "fetch failed: ")
	require.NotEmpty(t, strings.TrimSpace(detail), "fetch failed must carry a non-empty cause; got %q", msg)
}

// Regression: when the outer context already fired its deadline (e.g. a
// hook-level timeout shorter than fetchAttemptTimeout), the inner localCtx
// also reports DeadlineExceeded because it derives from outer. A naive check
// against the inner ctx alone would misreport this as errFetchTimedOut and
// swallow the real outer cancellation cause. The fix gates the sentinel on
// the outer ctx still being alive.
//
// Not parallel: t.Chdir() for OpenRepository.
func TestFetchAndRebase_OuterDeadlineFired_DoesNotReturnFetchTimedOut(t *testing.T) {
	tmpDir := setupRepoWithCheckpointBranch(t)
	t.Chdir(tmpDir)

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	defer cancel()

	nonExistent := filepath.Join(t.TempDir(), "does-not-exist")
	err := fetchAndRebaseSessionsCommon(ctx, nonExistent, paths.MetadataBranchName)
	require.Error(t, err)
	assert.NotErrorIs(t, err, errFetchTimedOut,
		"outer-context deadline must not be misreported as inner fetch timeout; got %v", err)
}

// On inner-push timeout, doPushBranch must end the dot line with " timed out",
// skip the sync→retry cascade (same network condition would also trip the
// fetch deadline), and surface the DEBUG-logging hint.
//
// Not parallel: t.Chdir() and stderr capture.
func TestDoPushBranch_TimedOut_PrintsTimedOutAndSkipsSync(t *testing.T) {
	workDir, bareDir := setupBareRemoteWithCheckpointBranch(t)
	t.Chdir(workDir)

	// Force every push attempt to time out immediately. KillOnCancel +
	// WaitDelay ensures the call returns within milliseconds.
	// Zero timeout makes localCtx fire DeadlineExceeded synchronously, so the
	// remote.Push call sees an already-cancelled context and we exercise the
	// errPushTimedOut path without racing the network or local push.
	withShortPushTimeout(t, 0)

	restore := captureStderr(t)
	err := doPushBranch(context.Background(), bareDir, paths.MetadataBranchName)
	output := restore()

	require.NoError(t, err, "doPushBranch should swallow the error (graceful degradation)")
	assert.Contains(t, output, "timed out", "dot line must end with 'timed out'")
	assert.NotContains(t, output, "Syncing", "must not cascade into sync on inner-push timeout")
	assert.Contains(t, output, "log_level", "should hint at enabling DEBUG logging")
}

// When the outer context is already cancelled (user Ctrl-C, hook deadline),
// doPushBranch must bail without a misleading sync cascade or warning.
//
// Not parallel: t.Chdir() and stderr capture.
func TestDoPushBranch_OuterContextCancelled_BailsQuietly(t *testing.T) {
	workDir, bareDir := setupBareRemoteWithCheckpointBranch(t)
	t.Chdir(workDir)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	restore := captureStderr(t)
	err := doPushBranch(ctx, bareDir, paths.MetadataBranchName)
	output := restore()

	require.NoError(t, err)
	assert.NotContains(t, output, "Syncing", "must not run sync after outer cancel")
	assert.NotContains(t, output, "Warning:", "must not print sync/retry warnings after outer cancel")
}

// Push attempts must reach INFO so doctor bundles capture push history at the
// default log level. Without this, a bundle from a stuck push reveals nothing
// about what was attempted.
//
// Not parallel: t.Chdir() and global logger state.
func TestDoPushBranch_LogsAttemptsAtInfo(t *testing.T) {
	workDir, bareDir := setupBareRemoteWithCheckpointBranch(t)
	t.Chdir(workDir)

	require.NoError(t, logging.Init(context.Background(), "2026-05-12-push-log-test"))
	t.Cleanup(logging.Close)

	restore := captureStderr(t)
	err := doPushBranch(context.Background(), bareDir, paths.MetadataBranchName)
	_ = restore()
	require.NoError(t, err)

	logging.Close() // flush

	logBytes, err := os.ReadFile(filepath.Join(workDir, ".entire", "logs", "entire.log"))
	require.NoError(t, err)
	logText := string(logBytes)

	assert.Contains(t, logText, "push attempt start", "log must record push start")
	assert.Contains(t, logText, "push attempt completed", "log must record push completion")
	assert.Contains(t, logText, `"branch":"`+paths.MetadataBranchName+`"`, "log must include branch")
	// Doctor bundles need the target on completion entries too, not just on
	// the start record, so a stuck or retried push can be correlated.
	assert.Contains(t, logText, `"target":"`+bareDir+`"`, "completion log must include target")
}

// reportPushFailure is the single chokepoint for the sync- and retry-stage
// failure tails of doPushBranch. If the outer context is done by the time
// it runs (Ctrl-C / hook deadline that arrived after the first push attempt
// completed), it must collapse to a silent bail — empty dot suffix, no log,
// no warning, no hint. Without this, late cancellation can still produce
// misleading "Warning: couldn't sync …" output.
//
// Safe under t.Parallel because reportPushFailureArgs.out lets us inject a
// per-test writer instead of mutating package-global pushStderr.
func TestReportPushFailure_BailsOnOuterContextCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var suffixes []string
	var buf strings.Builder

	reportPushFailure(ctx, reportPushFailureArgs{
		out:     &buf,
		stop:    func(s string) { suffixes = append(suffixes, s) },
		err:     errors.New("simulated sync failure"),
		logMsg:  "push sync failed",
		warnMsg: "[entire] Warning: couldn't sync foo: …\n",
		branch:  "entire/checkpoints/v1",
		target:  "origin",
		start:   time.Now(),
	})

	assert.Equal(t, []string{""}, suffixes, "stop must be called with empty suffix to clear the dot line")
	assert.Empty(t, buf.String(), "no warning should be emitted after outer cancel")
}

// Pins the errPushTimedOut sentinel so future refactors of doPushBranch's
// cascade logic can keep relying on errors.Is(err, errPushTimedOut).
//
// Not parallel: t.Chdir().
func TestTryPushSessionsCommon_InnerTimeout_ReturnsErrPushTimedOut(t *testing.T) {
	workDir, bareDir := setupBareRemoteWithCheckpointBranch(t)
	t.Chdir(workDir)

	// Zero timeout makes localCtx fire DeadlineExceeded synchronously, so the
	// remote.Push call sees an already-cancelled context and we exercise the
	// errPushTimedOut path without racing the network or local push.
	withShortPushTimeout(t, 0)

	_, err := tryPushSessionsCommon(context.Background(), bareDir, paths.MetadataBranchName)
	require.Error(t, err)
	assert.ErrorIs(t, err, errPushTimedOut,
		"err must wrap errPushTimedOut; got %v (class=%q)", err, classifyForLog(err))
}

// classifyForLog must keep logs free of raw push output (which can contain
// URLs and tokens). Anything we don't recognize collapses to "other".
func TestClassifyForLog(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"push timeout", errPushTimedOut, pushClassTimedOut},
		{"fetch timeout", errFetchTimedOut, pushClassTimedOut},
		{"non-fast-forward", errNonFastForward, pushClassNonFastForward},
		{"protected", &protectedRefError{output: "GH013"}, pushClassProtectedRef},
		{"other", errors.New("some random error"), pushClassOther},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, classifyForLog(tc.err))
		})
	}
}
