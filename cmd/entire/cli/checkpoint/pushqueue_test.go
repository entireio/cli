package checkpoint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPushQueue_EnqueueDrainRemove(t *testing.T) {
	t.Parallel()
	q := NewPushQueue(t.TempDir())

	a := mustRefName(t, "a1b2c3d4e5f6")
	b := mustRefName(t, "b2c3d4e5f6a1")

	// Empty queue drains to nothing.
	refs, err := q.Drain()
	require.NoError(t, err)
	assert.Empty(t, refs)

	require.NoError(t, q.Enqueue(a))
	require.NoError(t, q.Enqueue(b))

	refs, err = q.Drain()
	require.NoError(t, err)
	assert.Equal(t, []plumbing.ReferenceName{a, b}, refs, "drain preserves first-seen order")

	// Drain does not clear: refs survive until Remove.
	refs, err = q.Drain()
	require.NoError(t, err)
	assert.Equal(t, []plumbing.ReferenceName{a, b}, refs)

	require.NoError(t, q.Remove([]plumbing.ReferenceName{a}))
	refs, err = q.Drain()
	require.NoError(t, err)
	assert.Equal(t, []plumbing.ReferenceName{b}, refs)

	// Removing the last ref deletes the file entirely.
	require.NoError(t, q.Remove([]plumbing.ReferenceName{b}))
	refs, err = q.Drain()
	require.NoError(t, err)
	assert.Empty(t, refs)
	_, statErr := os.Stat(filepath.Join(q.dir, pushQueueFileName))
	assert.True(t, os.IsNotExist(statErr), "empty queue file should be removed")
}

func TestPushQueue_DrainDedupes(t *testing.T) {
	t.Parallel()
	q := NewPushQueue(t.TempDir())
	a := mustRefName(t, "a1b2c3d4e5f6")

	require.NoError(t, q.Enqueue(a))
	require.NoError(t, q.Enqueue(a))
	require.NoError(t, q.Enqueue(a))

	refs, err := q.Drain()
	require.NoError(t, err)
	assert.Equal(t, []plumbing.ReferenceName{a}, refs, "duplicates collapse to one")
}

// nonEmptyLineCount returns how many non-blank lines the queue file holds.
func nonEmptyLineCount(t *testing.T, q *PushQueue) int {
	t.Helper()
	data, err := os.ReadFile(q.queuePath())
	if os.IsNotExist(err) {
		return 0
	}
	require.NoError(t, err)
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}

func TestPushQueue_DrainCompactsRedundantEntries(t *testing.T) {
	t.Parallel()
	q := NewPushQueue(t.TempDir())
	a := mustRefName(t, "a1b2c3d4e5f6")
	b := mustRefName(t, "b2c3d4e5f6a1")

	require.NoError(t, q.Enqueue(a))
	require.NoError(t, q.Enqueue(a))
	require.NoError(t, q.Enqueue(b))
	require.NoError(t, q.Enqueue(a))
	require.Equal(t, 4, nonEmptyLineCount(t, q), "enqueue only appends")

	refs, err := q.Drain()
	require.NoError(t, err)
	assert.Equal(t, []plumbing.ReferenceName{a, b}, refs)
	assert.Equal(t, 2, nonEmptyLineCount(t, q), "Drain compacts the file to the de-duplicated set")

	// The refs still survive until Remove, and a re-drain does not rewrite again.
	refs, err = q.Drain()
	require.NoError(t, err)
	assert.Equal(t, []plumbing.ReferenceName{a, b}, refs, "compaction preserves queued refs")
	assert.Equal(t, 2, nonEmptyLineCount(t, q))
}

func TestPushQueue_DrainCompactsMalformedLines(t *testing.T) {
	t.Parallel()
	q := NewPushQueue(t.TempDir())
	a := mustRefName(t, "a1b2c3d4e5f6")
	require.NoError(t, q.Enqueue(a))

	f, err := os.OpenFile(q.queuePath(), os.O_WRONLY|os.O_APPEND, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString("not json\n\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	refs, err := q.Drain()
	require.NoError(t, err)
	assert.Equal(t, []plumbing.ReferenceName{a}, refs)
	assert.Equal(t, 1, nonEmptyLineCount(t, q), "Drain drops malformed lines from disk")
}

func TestPushQueue_RemovePreservesLaterEntries(t *testing.T) {
	t.Parallel()
	q := NewPushQueue(t.TempDir())
	a := mustRefName(t, "a1b2c3d4e5f6")
	b := mustRefName(t, "b2c3d4e5f6a1")

	// Simulate: drain sees [a], then b is enqueued during the push, then we
	// Remove(a). b must survive for the next pre-push.
	require.NoError(t, q.Enqueue(a))
	drained, err := q.Drain()
	require.NoError(t, err)
	require.Equal(t, []plumbing.ReferenceName{a}, drained)

	require.NoError(t, q.Enqueue(b))
	require.NoError(t, q.Remove(drained))

	refs, err := q.Drain()
	require.NoError(t, err)
	assert.Equal(t, []plumbing.ReferenceName{b}, refs)
}

func TestPushQueue_SkipsMalformedLines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	q := NewPushQueue(dir)
	a := mustRefName(t, "a1b2c3d4e5f6")
	require.NoError(t, q.Enqueue(a))

	// Append a garbage line + a blank line directly.
	f, err := os.OpenFile(q.queuePath(), os.O_WRONLY|os.O_APPEND, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString("not json\n\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	refs, err := q.Drain()
	require.NoError(t, err)
	assert.Equal(t, []plumbing.ReferenceName{a}, refs, "malformed lines are skipped, valid refs survive")
}

func TestPushQueue_RemoveEmptyIsNoop(t *testing.T) {
	t.Parallel()
	q := NewPushQueue(t.TempDir())
	require.NoError(t, q.Remove(nil))
}
