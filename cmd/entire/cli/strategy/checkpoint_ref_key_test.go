package strategy

import (
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// foldedShardPair returns one checkpoint's canonical ref name and the
// case-folded shard spelling the same ref can be stored under.
func foldedShardPair(t *testing.T) (plumbing.ReferenceName, plumbing.ReferenceName) {
	t.Helper()
	cid := id.MustCheckpointID("01M2DCHJCHTR9T9MZTSB7WV76B")
	canonical, err := checkpoint.RefName(cid)
	require.NoError(t, err)
	folded, ok := checkpoint.FoldedRefName(cid)
	require.True(t, ok, "a 6B shard must have a folded spelling")
	require.NotEqual(t, canonical, folded)
	return canonical, folded
}

func TestCanonicalCheckpointRefKey(t *testing.T) {
	t.Parallel()
	canonical, folded := foldedShardPair(t)

	assert.Equal(t, canonical, canonicalCheckpointRefKey(canonical))
	assert.Equal(t, canonical, canonicalCheckpointRefKey(folded),
		"both shard spellings of one checkpoint must share a key")

	const notACheckpoint = plumbing.ReferenceName("refs/heads/main")
	assert.Equal(t, notACheckpoint, canonicalCheckpointRefKey(notACheckpoint),
		"a ref that does not name a checkpoint is its own key")
}

// A checkpoint stored under its folded shard is queued and reported under that
// spelling, while the user names it by its canonical one. Comparing the two
// spellings directly made a targeted `entire checkpoint push <id>` skip the
// checkpoint as "not waiting" even though the queue held it.
func TestWaitingTargetSelection_MatchesFoldedShardSpelling(t *testing.T) {
	t.Parallel()
	canonical, folded := foldedShardPair(t)
	pin := plumbing.NewHash("1111111111111111111111111111111111111111")

	selection, err := waitingTargetSelection(
		[]CheckpointPushRequest{{Ref: folded, Commit: pin}},
		map[plumbing.ReferenceName]struct{}{canonical: {}},
	)
	require.NoError(t, err)

	got, ok := selection.pin(canonical)
	assert.True(t, ok, "a target named by either shard spelling selects the queued ref")
	assert.Equal(t, pin, got)
}

// The flush reports the spelling it pushed, which is the stored one. Keying the
// report by that name alone left a folded-shard checkpoint reported as never
// sent even though it reached the remote.
func TestCheckpointPushOutcomes_FoldedShardRefReportsUploaded(t *testing.T) {
	t.Parallel()
	canonical, folded := foldedShardPair(t)
	tip := plumbing.NewHash("2222222222222222222222222222222222222222")

	outcomes := checkpointPushOutcomes(
		[]CheckpointPushRequest{{Ref: canonical}},
		map[plumbing.ReferenceName]struct{}{canonical: {}},
		nil,
		checkpointFlushResult{pushed: []checkpointRefPush{{name: folded, hash: tip}}},
		nil,
	)
	require.Len(t, outcomes, 1)
	assert.Equal(t, CheckpointPushUploaded, outcomes[0].Status)
	assert.Equal(t, tip, outcomes[0].Commit)
}
