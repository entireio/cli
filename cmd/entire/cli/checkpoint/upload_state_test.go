package checkpoint

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUploadCoordinator_RequestMerge(t *testing.T) {
	t.Parallel()
	coord := NewUploadCoordinator(t.TempDir())

	require.NoError(t, coord.Request(UploadRequest{Remote: "origin", OPFDecision: UploadOPFRun, PendingCapture: "origin"}))
	require.NoError(t, coord.Request(UploadRequest{Remote: "origin", OPFDecision: UploadOPFUnset}))

	req, err := coord.TakeRequest()
	require.NoError(t, err)
	require.NotNil(t, req)
	assert.Equal(t, UploadOPFRun, req.OPFDecision, "a later push that resolved nothing must not drop an earlier run")
	assert.Equal(t, "origin", req.PendingCapture, "an unsatisfied capture for the same remote is kept")

	req, err = coord.TakeRequest()
	require.NoError(t, err)
	assert.Nil(t, req, "taking consumes the request")
}

func TestUploadCoordinator_RequestToOtherRemoteDropsCapture(t *testing.T) {
	t.Parallel()
	coord := NewUploadCoordinator(t.TempDir())
	require.NoError(t, coord.Request(UploadRequest{Remote: "origin", PendingCapture: "origin"}))
	require.NoError(t, coord.Request(UploadRequest{Remote: "fork"}))

	req, err := coord.TakeRequest()
	require.NoError(t, err)
	require.NotNil(t, req)
	assert.Equal(t, "fork", req.Remote)
	assert.Empty(t, req.PendingCapture)
}

func TestUploadCoordinator_RunRecords(t *testing.T) {
	t.Parallel()
	coord := NewUploadCoordinator(t.TempDir())
	st, err := coord.State()
	require.NoError(t, err)
	assert.Nil(t, st.Last, "no file is the zero state")

	first := time.Now()
	require.NoError(t, coord.StartRun(first))
	require.NoError(t, coord.FinishRun(UploadResult{StartedAt: first, FinishedAt: first, Announcement: "[entire] one"}))
	second := first.Add(time.Second)
	require.NoError(t, coord.StartRun(second))
	require.NoError(t, coord.FinishRun(UploadResult{StartedAt: second, FinishedAt: second, Remaining: 3,
		Error: "budget (10m0s) exhausted", Announcement: "[entire] two"}))
	st, err = coord.State()
	require.NoError(t, err)
	assert.Equal(t, "[entire] one\n[entire] two", st.Last.Announcement, "an unshown announcement survives the next run")

	require.NoError(t, coord.ClearReported(first))
	st, err = coord.State()
	require.NoError(t, err)
	assert.NotEmpty(t, st.Last.Error, "clearing an older run must not wipe a newer one")

	require.NoError(t, coord.ClearReported(second))
	st, err = coord.State()
	require.NoError(t, err)
	assert.Empty(t, st.Last.Error)
	assert.Empty(t, st.Last.Announcement)
	assert.Equal(t, 3, st.Last.Remaining)
}

func TestUploadResult_Running(t *testing.T) {
	t.Parallel()
	now := time.Now()
	assert.True(t, (&UploadResult{StartedAt: now.Add(-time.Minute)}).Running(now, time.Hour))
	assert.False(t, (&UploadResult{StartedAt: now.Add(-2 * time.Hour)}).Running(now, time.Hour), "a killed worker's record goes stale")
	assert.False(t, (&UploadResult{StartedAt: now, FinishedAt: now}).Running(now, time.Hour))
	assert.False(t, (*UploadResult)(nil).Running(now, time.Hour))
}

func TestUploadCoordinator_TryLockWorker(t *testing.T) {
	t.Parallel()
	coord := NewUploadCoordinator(t.TempDir())
	release, ok, err := coord.TryLockWorker(t.Context())
	require.NoError(t, err)
	require.True(t, ok)

	_, ok, err = coord.TryLockWorker(t.Context())
	require.NoError(t, err)
	assert.False(t, ok, "a held lock is reported, not an error")

	release()
	release2, ok, err := coord.TryLockWorker(t.Context())
	require.NoError(t, err)
	assert.True(t, ok)
	release2()
}
