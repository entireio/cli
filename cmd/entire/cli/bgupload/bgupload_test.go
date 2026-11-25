package bgupload

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testJob struct {
	Pending bool `json:"pending"`
}

func TestCoordinator_RunRecordsAndReport(t *testing.T) {
	t.Parallel()
	coord := New[testJob](t.TempDir(), "test", time.Hour)
	st, err := coord.State()
	require.NoError(t, err)
	assert.Nil(t, st.Last, "no file is the zero state")

	first := time.Now()
	require.NoError(t, coord.StartRun(first))
	require.NoError(t, coord.FinishRun(Result{StartedAt: first, FinishedAt: first, Announcement: "[entire] one"}))
	second := first.Add(time.Second)
	require.NoError(t, coord.StartRun(second))
	require.NoError(t, coord.FinishRun(Result{StartedAt: second, FinishedAt: second, Remaining: 3,
		Error: "budget exhausted", Detail: "why", Announcement: "[entire] two"}))

	var out bytes.Buffer
	require.NoError(t, coord.Report(&out, "test upload"))
	assert.Equal(t, "[entire] one\n[entire] two\n[entire] Last background test upload stopped: budget exhausted.\nwhy\n", out.String(),
		"an unshown announcement survives the next run")

	out.Reset()
	require.NoError(t, coord.Report(&out, "test upload"))
	assert.Empty(t, out.String(), "reported once")
	st, err = coord.State()
	require.NoError(t, err)
	assert.Equal(t, 3, st.Last.Remaining)
}

func TestResult_Running(t *testing.T) {
	t.Parallel()
	now := time.Now()
	assert.True(t, (&Result{StartedAt: now.Add(-time.Minute)}).Running(now, time.Hour))
	assert.False(t, (&Result{StartedAt: now.Add(-2 * time.Hour)}).Running(now, time.Hour), "a killed worker's record goes stale")
	assert.False(t, (&Result{StartedAt: now, FinishedAt: now}).Running(now, time.Hour))
	assert.False(t, (*Result)(nil).Running(now, time.Hour))
}

func TestCoordinator_TryLockPerKind(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	coord := New[testJob](dir, "a", time.Hour)
	release, ok, err := coord.TryLock()
	require.NoError(t, err)
	require.True(t, ok)

	_, ok, err = coord.TryLock()
	require.NoError(t, err)
	assert.False(t, ok, "a held lock is reported, not an error")

	other, ok, err := New[testJob](dir, "b", time.Hour).TryLock()
	require.NoError(t, err)
	assert.True(t, ok, "another kind has its own lock")
	other()

	release()
	release2, ok, err := coord.TryLock()
	require.NoError(t, err)
	assert.True(t, ok)
	release2()
}

func TestCoordinator_RunPasses(t *testing.T) {
	t.Parallel()
	coord := New[testJob](t.TempDir(), "test", time.Hour)
	require.NoError(t, coord.Update(func(st *State[testJob]) { st.Job.Pending = true }))
	pending := func(st State[testJob]) bool { return st.Job.Pending }

	passes := 0
	leftover := coord.RunPasses(t.Context(), 3, pending, func(_ context.Context) { passes++ })
	assert.Equal(t, 3, passes, "work that keeps arriving runs to the pass limit")
	assert.True(t, leftover)

	passes = 0
	leftover = coord.RunPasses(t.Context(), 3, pending, func(_ context.Context) {
		passes++
		require.NoError(t, coord.Update(func(st *State[testJob]) { st.Job.Pending = false }))
	})
	assert.Equal(t, 1, passes)
	assert.False(t, leftover)

	release, _, err := coord.TryLock()
	require.NoError(t, err)
	defer release()
	assert.False(t, coord.RunPasses(t.Context(), 3, pending, func(_ context.Context) { passes++ }), "a held lock ends the run")
}
