package checkpoint

import (
	"context"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// Under git-refs nothing advances the local v1 branch, while a plain git fetch
// still advances its remote-tracking copy. A copy from v1 must take whichever
// side last changed the checkpoint: a stale copy, once pushed as the ref, hides
// newer sessions from every clone that reads refs first.

// v1History builds the local v1 branch and origin's remote-tracking v1 from a
// shared base commit holding the fixture's checkpoint.
type v1History struct {
	f    legacyCopyFixture
	base plumbing.Hash
}

func newV1History(t *testing.T) v1History {
	t.Helper()
	f := newLegacyCopyFixture(t)
	return v1History{f: f, base: f.v1Tip(t)}
}

// commitOnTop writes sessionID to cid on top of from and returns the new v1
// commit, leaving local v1 there.
func (h v1History) commitOnTop(t *testing.T, from plumbing.Hash, cid id.CheckpointID, sessionID string) plumbing.Hash {
	t.Helper()
	h.f.setV1(t, from)
	writeRoutingCheckpoint(t, h.f.branch, cid, sessionID)
	return h.f.v1Tip(t)
}

// place points local v1 at local and origin's remote-tracking v1 at tracking.
func (h v1History) place(t *testing.T, local, tracking plumbing.Hash) {
	t.Helper()
	h.f.setV1(t, local)
	require.NoError(t, h.f.repo.Storer.SetReference(plumbing.NewHashReference(
		plumbing.NewRemoteReferenceName("origin", DefaultV1Refs().Primary.Short()), tracking)))
}

func (f legacyCopyFixture) v1Tip(t *testing.T) plumbing.Hash {
	t.Helper()
	ref, err := f.repo.Reference(DefaultV1Refs().Primary, true)
	require.NoError(t, err)
	return ref.Hash()
}

func (f legacyCopyFixture) setV1(t *testing.T, commit plumbing.Hash) {
	t.Helper()
	require.NoError(t, f.repo.Storer.SetReference(plumbing.NewHashReference(DefaultV1Refs().Primary, commit)))
}

func (f legacyCopyFixture) refSessionIDs(t *testing.T) []string {
	t.Helper()
	ctx := context.Background()
	summary, err := f.refs.Read(ctx, f.cid)
	require.NoError(t, err)
	require.NotNil(t, summary, "the checkpoint must have its own ref")
	ids := make([]string, 0, len(summary.Sessions))
	for i := range summary.Sessions {
		meta, err := f.refs.ReadSessionMetadata(ctx, f.cid, i)
		require.NoError(t, err)
		ids = append(ids, meta.SessionID)
	}
	return ids
}

func TestKindRoutingStore_CopyTakesTheRemoteTrackingV1WhenLocalV1IsBehind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newV1History(t)
	ahead := h.commitOnTop(t, h.base, h.f.cid, "teammate-on-v1")
	h.place(t, h.base, ahead)

	require.NoError(t, h.f.router.Write(ctx, afterSwitchSession(h.f.cid)))

	assert.Equal(t, []string{"before-switch", "teammate-on-v1", "after-switch"}, h.f.refSessionIDs(t))
}

func TestKindRoutingStore_CopyKeepsLocalV1WhenItIsAhead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newV1History(t)
	ahead := h.commitOnTop(t, h.base, h.f.cid, "unpushed-on-v1")
	h.place(t, ahead, h.base)

	require.NoError(t, h.f.router.Write(ctx, afterSwitchSession(h.f.cid)))

	assert.Equal(t, []string{"before-switch", "unpushed-on-v1", "after-switch"}, h.f.refSessionIDs(t))
}

// Local v1 has an unpushed commit for another checkpoint, so the branches have
// diverged, but only the remote side changed this checkpoint.
func TestKindRoutingStore_CopyTakesTheOnlySideThatChangedTheCheckpoint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newV1History(t)
	remote := h.commitOnTop(t, h.base, h.f.cid, "teammate-on-v1")
	local := h.commitOnTop(t, h.base, id.MustCheckpointID("0123456789ab"), "other-checkpoint")
	h.place(t, local, remote)

	require.NoError(t, h.f.router.Write(ctx, afterSwitchSession(h.f.cid)))

	assert.Equal(t, []string{"before-switch", "teammate-on-v1", "after-switch"}, h.f.refSessionIDs(t))
}

// Both sides added a session to the checkpoint since they split. Either copy
// would hide the other's session once pushed, so nothing is copied or written.
func TestKindRoutingStore_CopyRefusesWhenBothV1CopiesChangedTheCheckpoint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newV1History(t)
	remote := h.commitOnTop(t, h.base, h.f.cid, "teammate-on-v1")
	local := h.commitOnTop(t, h.base, h.f.cid, "unpushed-on-v1")
	h.place(t, local, remote)

	err := h.f.router.Write(ctx, afterSwitchSession(h.f.cid))

	require.ErrorIs(t, err, ErrLegacyCheckpointDiverged)
	h.f.requireNoRef(t)
	assert.Equal(t, local, h.f.v1Tip(t), "the refused write must not touch v1 either")
}

// A backfill hits the same refusal instead of falling through to v1.
func TestKindRoutingStore_BackfillRefusesWhenBothV1CopiesChangedTheCheckpoint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newV1History(t)
	remote := h.commitOnTop(t, h.base, h.f.cid, "teammate-on-v1")
	local := h.commitOnTop(t, h.base, h.f.cid, "unpushed-on-v1")
	h.place(t, local, remote)

	err := h.f.router.Write(ctx, SessionSummary{CheckpointID: h.f.cid, Summary: &Summary{Intent: "backfilled"}})

	require.ErrorIs(t, err, ErrLegacyCheckpointDiverged)
	h.f.requireNoRef(t)
	assert.Equal(t, local, h.f.v1Tip(t))
}

// Diverged v1 copies matter only when the copy needs one of them. A ref a
// teammate already pushed is the checkpoint's current state, so the write
// builds on it instead of refusing.
func TestKindRoutingStore_DivergedV1DoesNotBlockAWriteToAPushedRef(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newV1History(t)
	pushed := h.f.remoteCopy(t)
	remote := h.commitOnTop(t, h.base, h.f.cid, "teammate-on-v1")
	local := h.commitOnTop(t, h.base, h.f.cid, "unpushed-on-v1")
	h.place(t, local, remote)
	h.f = h.f.withLegacyRefFetcher(func(_ context.Context, ref plumbing.ReferenceName) error {
		return h.f.repo.Storer.SetReference(plumbing.NewHashReference(ref, pushed))
	})

	require.NoError(t, h.f.router.Write(ctx, afterSwitchSession(h.f.cid)))

	assert.Equal(t, []string{"before-switch", "after-switch"}, h.f.refSessionIDs(t))
}
