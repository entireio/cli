package checkpoint

import (
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPushQueue_EnqueueAll(t *testing.T) {
	t.Parallel()
	q := NewPushQueue(t.TempDir())

	a := mustRefName(t, "a1b2c3d4e5f6")
	b := mustRefName(t, "b2c3d4e5f6a1")
	c := mustRefName(t, "c3d4e5f6a1b2")

	require.NoError(t, q.EnqueueAll(nil), "empty input is a no-op")
	refs, err := q.Drain()
	require.NoError(t, err)
	assert.Empty(t, refs)

	require.NoError(t, q.Enqueue(b))
	require.NoError(t, q.EnqueueAll([]plumbing.ReferenceName{a, b, c}))

	refs, err = q.Drain()
	require.NoError(t, err)
	assert.Equal(t, []plumbing.ReferenceName{b, a, c}, refs, "first-seen order, duplicates collapsed")

	require.NoError(t, q.Remove([]plumbing.ReferenceName{a, b, c}))
	refs, err = q.Drain()
	require.NoError(t, err)
	assert.Empty(t, refs)
}
