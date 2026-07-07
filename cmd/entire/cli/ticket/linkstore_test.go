package ticket

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLinkStore_RoundTrip(t *testing.T) {
	t.Parallel()

	store := newLinkStoreWithDir(t.TempDir())

	_, ok, err := store.get("main")
	require.NoError(t, err)
	assert.False(t, ok, "no link before any is set")

	require.NoError(t, store.set("feature/x", Link{Platform: "linear", ID: "ENG-1"}))
	got, ok, err := store.get("feature/x")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "ENG-1", got.ID)
	assert.Equal(t, "linear", got.Platform)

	// A second branch does not disturb the first.
	require.NoError(t, store.set("feature/y", Link{Platform: "linear", ID: "ENG-2"}))
	got, ok, err = store.get("feature/x")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "ENG-1", got.ID)

	removed, err := store.del("feature/x")
	require.NoError(t, err)
	assert.True(t, removed)

	_, ok, err = store.get("feature/x")
	require.NoError(t, err)
	assert.False(t, ok)

	// Deleting an absent branch reports false, not an error.
	removed, err = store.del("feature/x")
	require.NoError(t, err)
	assert.False(t, removed)
}
