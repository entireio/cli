package checkpoint

import (
	"context"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// originV1Store returns a git-branch store whose ref is origin's remote-tracking
// entire/checkpoints/v1, for seeding checkpoints that live only on origin.
func originV1Store(repo *git.Repository) *GitStore {
	originRef := plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName)
	return NewGitStore(repo, PersistentRefs{Primary: originRef, Read: originRef})
}

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

func TestMigrateBranchHexToULIDRefs_UnionsLocalAndOriginV1(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, repo, _ := newTestRepo(t)

	// A checkpoint only on the local v1 and another only on origin's v1 (the two
	// can diverge). Both must be migrated — reading only one would leave the
	// other's commit trailers un-remapped.
	hexLocal := id.MustCheckpointID("a1b2c3d4e5f6")
	hexOrigin := id.MustCheckpointID("ffffffffeeee")
	writeRoutingCheckpoint(t, NewGitStore(repo, DefaultV1Refs()), hexLocal, "s-local")
	writeRoutingCheckpoint(t, originV1Store(repo), hexOrigin, "s-origin")

	result, err := MigrateBranchHexToULIDRefs(ctx, repo, false)
	require.NoError(t, err)
	assert.Equal(t, 2, result.Total, "should migrate checkpoints from BOTH local and origin v1")

	migrated := make(map[id.CheckpointID]bool)
	for _, m := range result.Mapping {
		migrated[m.OldID] = true
	}
	assert.True(t, migrated[hexLocal], "local-only checkpoint should be migrated")
	assert.True(t, migrated[hexOrigin], "origin-only checkpoint should be migrated")
}

func TestMigrateBranchHexToULIDRefs_DedupsAcrossV1Sources(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, repo, _ := newTestRepo(t)

	// The same checkpoint present on both local and origin v1 must migrate once.
	dup := id.MustCheckpointID("a1b2c3d4e5f6")
	writeRoutingCheckpoint(t, NewGitStore(repo, DefaultV1Refs()), dup, "s")
	writeRoutingCheckpoint(t, originV1Store(repo), dup, "s")

	result, err := MigrateBranchHexToULIDRefs(ctx, repo, false)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Total, "a checkpoint on both v1 sources should migrate once")
	assert.Len(t, result.Mapping, 1)
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
