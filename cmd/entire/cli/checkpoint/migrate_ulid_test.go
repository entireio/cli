package checkpoint

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

func TestMigrateBranchHexToULIDRefs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, repo, _ := newTestRepo(t)

	branch := NewGitStore(repo, DefaultV1Refs())
	hexA := id.MustCheckpointID("a1b2c3d4e5f6")
	hexB := id.MustCheckpointID("ffffffffeeee")
	writeRoutingCheckpoint(t, branch, hexA, "session-a")
	writeRoutingCheckpoint(t, branch, hexB, "session-b")

	result, err := MigrateBranchHexToULIDRefs(ctx, repo, false)
	require.NoError(t, err)
	assert.Equal(t, 2, result.Total)
	assert.Equal(t, 0, result.Skipped)
	require.Len(t, result.Mapping, 2)

	refsStore := newGitRefsStore(repo)
	byOld := make(map[id.CheckpointID]id.CheckpointID)
	for _, m := range result.Mapping {
		byOld[m.OldID] = m.NewID
		assert.Equal(t, id.KindLegacy, m.OldID.Kind(), "source id should be legacy hex")
		require.Equal(t, id.KindULID, m.NewID.Kind(), "minted id should be a ULID")

		// The ULID ref exists...
		refName, err := RefName(m.NewID)
		require.NoError(t, err)
		_, err = repo.Reference(refName, true)
		require.NoError(t, err, "ULID ref %s should exist after migration", refName)

		// ...and the migrated checkpoint's embedded ids are re-stamped to the ULID.
		summary, err := refsStore.Read(ctx, m.NewID)
		require.NoError(t, err)
		require.NotNil(t, summary, "migrated checkpoint should read from the refs store")
		assert.Equal(t, m.NewID, summary.CheckpointID, "root metadata.json checkpoint_id should be the ULID")

		meta, err := refsStore.ReadSessionMetadata(ctx, m.NewID, 0)
		require.NoError(t, err)
		require.NotNil(t, meta)
		assert.Equal(t, m.NewID, meta.CheckpointID, "session metadata.json checkpoint_id should be the ULID")
	}
	assert.Contains(t, byOld, hexA)
	assert.Contains(t, byOld, hexB)

	// The original hex checkpoints remain on the v1 branch (the command, not this
	// function, deletes the branch afterward).
	orig, err := branch.Read(ctx, hexA)
	require.NoError(t, err)
	assert.NotNil(t, orig, "hex checkpoint should remain on the branch")
}

func TestMigrateBranchHexToULIDRefs_DryRunWritesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, repo, _ := newTestRepo(t)

	branch := NewGitStore(repo, DefaultV1Refs())
	hexA := id.MustCheckpointID("a1b2c3d4e5f6")
	writeRoutingCheckpoint(t, branch, hexA, "session-a")

	result, err := MigrateBranchHexToULIDRefs(ctx, repo, true)
	require.NoError(t, err)
	require.Len(t, result.Mapping, 1)

	// A dry run mints the mapping but must not create any ref.
	refName, err := RefName(result.Mapping[0].NewID)
	require.NoError(t, err)
	_, err = repo.Reference(refName, true)
	assert.Error(t, err, "dry run must not write the ULID ref")
}

func TestMigrateBranchHexToULIDRefs_NoBranch(t *testing.T) {
	t.Parallel()
	_, repo, _ := newTestRepo(t)
	result, err := MigrateBranchHexToULIDRefs(context.Background(), repo, false)
	require.NoError(t, err)
	assert.Equal(t, 0, result.Total)
	assert.Empty(t, result.Mapping)
}
