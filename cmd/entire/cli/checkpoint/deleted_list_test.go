package checkpoint

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

func TestDeletedCheckpoints_RecordAndContains(t *testing.T) {
	t.Parallel()
	list := NewDeletedCheckpoints(t.TempDir())
	hex := id.MustCheckpointID("a1b2c3d4e5f6")
	ulid := id.MustCheckpointID("01K6ZQ2M8E3V7R5T9Y4X6W2A1B")

	deleted, err := list.Load()
	require.NoError(t, err)
	assert.Empty(t, deleted, "a missing list file is an empty list")

	require.NoError(t, list.Record(hex))
	require.NoError(t, list.Record(ulid))
	require.NoError(t, list.Record(hex), "recording twice is a no-op")

	deleted, err = list.Load()
	require.NoError(t, err)
	assert.Len(t, deleted, 2)
	assert.True(t, deleted.Contains(hex))
	assert.True(t, deleted.Contains(ulid))
	assert.False(t, deleted.Contains(id.MustCheckpointID("ffffffffffff")))
}

func TestDeletedCheckpoints_RejectsInvalidID(t *testing.T) {
	t.Parallel()
	list := NewDeletedCheckpoints(t.TempDir())
	require.Error(t, list.Record(id.CheckpointID("../escape")))
}

func TestDeletedCheckpoints_IgnoresInvalidEntriesOnDisk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, deletedCheckpointsFileName),
		[]byte(`{"checkpoint_ids":["a1b2c3d4e5f6","not-an-id"]}`), 0o600))

	deleted, err := NewDeletedCheckpoints(dir).Load()
	require.NoError(t, err)
	assert.Len(t, deleted, 1)
	assert.True(t, deleted.Contains(id.MustCheckpointID("a1b2c3d4e5f6")))
}

func TestDeletedCheckpoints_CorruptFileIsAnError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, deletedCheckpointsFileName), []byte(`{not json`), 0o600))

	_, err := NewDeletedCheckpoints(dir).Load()
	require.Error(t, err, "a corrupt list must not read as empty, or deleted IDs come back")
	assert.Contains(t, err.Error(), filepath.Join(dir, deletedCheckpointsFileName), "the error names the file to fix")
}

func TestDeletedCheckpoints_Remove(t *testing.T) {
	t.Parallel()
	list := NewDeletedCheckpoints(t.TempDir())
	kept := id.MustCheckpointID("a1b2c3d4e5f6")
	removed := id.MustCheckpointID("01K6ZQ2M8E3V7R5T9Y4X6W2A1B")
	require.NoError(t, list.Record(kept))
	require.NoError(t, list.Record(removed))

	require.NoError(t, list.Remove(removed))
	require.NoError(t, list.Remove(removed), "removing an absent ID is a no-op")

	deleted, err := list.Load()
	require.NoError(t, err)
	assert.True(t, deleted.Contains(kept))
	assert.False(t, deleted.Contains(removed))
}

func TestDeletedCheckpoints_AddReportsWhetherItAdded(t *testing.T) {
	t.Parallel()
	list := NewDeletedCheckpoints(t.TempDir())
	cid := id.MustCheckpointID("a1b2c3d4e5f6")

	added, err := list.Add(cid)
	require.NoError(t, err)
	assert.True(t, added)
	added, err = list.Add(cid)
	require.NoError(t, err)
	assert.False(t, added, "an ID an earlier run recorded is not this run's to remove")
}
