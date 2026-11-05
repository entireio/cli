package checkpoint

import (
	"context"
	"errors"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/redact"
)

// When this clone's v1 and a remote-tracking v1 both changed a checkpoint,
// neither copy can become its ref without hiding the other's sessions. Only a
// person may choose which copy to keep; migration must never pick one silently.

// divergedV1 returns a history where local v1 and origin's v1 each added a
// session to the fixture's checkpoint since they split.
func divergedV1(t *testing.T) v1History {
	t.Helper()
	h := newV1History(t)
	remote := h.commitOnTop(t, h.base, h.f.cid, "teammate-on-v1")
	local := h.commitOnTop(t, h.base, h.f.cid, "unpushed-on-v1")
	h.place(t, local, remote)
	return h
}

var (
	localV1  = DefaultV1Refs().Primary
	originV1 = plumbing.NewRemoteReferenceName("origin", DefaultV1Refs().Primary.Short())
)

// chooser records what it was asked and answers with keep (empty skips).
type chooser struct {
	keep  plumbing.ReferenceName
	asked []DivergedLegacyCheckpoint
}

func (c *chooser) choose(_ context.Context, d DivergedLegacyCheckpoint) (plumbing.ReferenceName, error) {
	c.asked = append(c.asked, d)
	return c.keep, nil
}

func TestMigrateBranchToRefs_TakesTheNewerV1Copy(t *testing.T) {
	t.Parallel()
	h := newV1History(t)
	ahead := h.commitOnTop(t, h.base, h.f.cid, "teammate-on-v1")
	h.place(t, h.base, ahead)

	result, err := MigrateBranchToRefs(t.Context(), h.f.repo, MigrateOptions{})

	require.NoError(t, err)
	assert.Equal(t, []id.CheckpointID{h.f.cid}, result.Migrated)
	assert.Equal(t, []string{"before-switch", "teammate-on-v1"}, h.f.refSessionIDs(t),
		"local v1 is behind, so the ref must hold origin's copy")
}

func TestMigrateBranchToRefs_ListsADivergedCheckpointWithoutAChooser(t *testing.T) {
	t.Parallel()
	h := divergedV1(t)
	other := id.MustCheckpointID("0123456789ab")
	writeRoutingCheckpoint(t, h.f.branch, other, "untouched")
	h.place(t, h.f.v1Tip(t), h.trackingTip(t))

	result, err := MigrateBranchToRefs(t.Context(), h.f.repo, MigrateOptions{})

	require.NoError(t, err)
	assert.Equal(t, []id.CheckpointID{h.f.cid}, divergedIDs(result))
	assert.Equal(t, []id.CheckpointID{other}, result.Migrated, "other checkpoints still migrate")
	h.f.requireNoRef(t)
}

func TestMigrateBranchToRefs_KeepsTheChosenCopy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		keep plumbing.ReferenceName
		want string
	}{
		{keep: localV1, want: "unpushed-on-v1"},
		{keep: originV1, want: "teammate-on-v1"},
	} {
		t.Run(tc.keep.String(), func(t *testing.T) {
			t.Parallel()
			h := divergedV1(t)
			c := &chooser{keep: tc.keep}

			result, err := MigrateBranchToRefs(t.Context(), h.f.repo, MigrateOptions{Choose: c.choose})

			require.NoError(t, err)
			assert.Equal(t, []id.CheckpointID{h.f.cid}, result.Migrated)
			assert.Empty(t, result.Diverged)
			assert.Equal(t, []string{"before-switch", tc.want}, h.f.refSessionIDs(t))
			h.f.requireQueued(t)
		})
	}
}

func TestMigrateBranchToRefs_ShowsBothCopiesWhenAsking(t *testing.T) {
	t.Parallel()
	h := divergedV1(t)
	c := &chooser{}

	_, err := MigrateBranchToRefs(t.Context(), h.f.repo, MigrateOptions{Choose: c.choose})

	require.NoError(t, err)
	require.Len(t, c.asked, 1)
	asked := c.asked[0]
	assert.Equal(t, h.f.cid, asked.ID)
	assert.Equal(t, localV1, asked.Local.Ref)
	assert.Equal(t, []string{"before-switch", "unpushed-on-v1"}, asked.Local.SessionIDs)
	assert.Equal(t, originV1, asked.Remote.Ref)
	assert.Equal(t, []string{"before-switch", "teammate-on-v1"}, asked.Remote.SessionIDs)
	assert.Empty(t, asked.DifferingSessions(), "the shared session is unchanged on both sides")
}

// A session both copies hold can still differ, when each clone saved more of
// it. That must show, or a copy that looks like a superset hides newer work.
func TestMigrateBranchToRefs_ShowsASessionBothCopiesChanged(t *testing.T) {
	t.Parallel()
	h := newV1History(t)
	remote := h.commitOnTop(t, h.base, h.f.cid, "teammate-on-v1")
	h.f.setV1(t, h.base)
	require.NoError(t, h.f.branch.Write(t.Context(), Session{
		CheckpointID: h.f.cid,
		SessionID:    "before-switch",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("more of before-switch, saved only on this clone")),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	}))
	h.place(t, h.f.v1Tip(t), remote)
	c := &chooser{}

	_, err := MigrateBranchToRefs(t.Context(), h.f.repo, MigrateOptions{Choose: c.choose})

	require.NoError(t, err)
	require.Len(t, c.asked, 1)
	assert.Equal(t, []string{"before-switch"}, c.asked[0].Local.SessionIDs)
	assert.Equal(t, []string{"before-switch"}, c.asked[0].DifferingSessions())
}

func TestMigrateBranchToRefs_SkipWritesNothing(t *testing.T) {
	t.Parallel()
	h := divergedV1(t)
	c := &chooser{}

	result, err := MigrateBranchToRefs(t.Context(), h.f.repo, MigrateOptions{Choose: c.choose})

	require.NoError(t, err)
	assert.Equal(t, []id.CheckpointID{h.f.cid}, divergedIDs(result))
	h.f.requireNoRef(t)
}

func TestMigrateBranchToRefs_DryRunDoesNotAsk(t *testing.T) {
	t.Parallel()
	h := divergedV1(t)
	c := &chooser{keep: localV1}

	result, err := MigrateBranchToRefs(t.Context(), h.f.repo, MigrateOptions{DryRun: true, Choose: c.choose})

	require.NoError(t, err)
	assert.Empty(t, c.asked)
	assert.Equal(t, []id.CheckpointID{h.f.cid}, divergedIDs(result))
	h.f.requireNoRef(t)
}

func TestResolveDivergedLegacyCheckpoints_WritesTheChosenCopyIntoAnAbsentRef(t *testing.T) {
	t.Parallel()
	h := divergedV1(t)
	c := &chooser{keep: originV1}

	result, err := ResolveDivergedLegacyCheckpoints(t.Context(), h.f.repo, MigrateOptions{Choose: c.choose}, nil)

	require.NoError(t, err)
	assert.Equal(t, []id.CheckpointID{h.f.cid}, result.Migrated)
	assert.Equal(t, []string{"before-switch", "teammate-on-v1"}, h.f.refSessionIDs(t))
	h.f.requireQueued(t)
}

// After the switch a checkpoint's ref is its current state and may hold
// sessions neither v1 copy has, so it is never revisited.
func TestResolveDivergedLegacyCheckpoints_LeavesAnExistingRefAlone(t *testing.T) {
	t.Parallel()
	h := divergedV1(t)
	writeRoutingCheckpoint(t, h.f.refs, h.f.cid, "written-after-switch")
	before := h.f.refTip(t)
	c := &chooser{keep: localV1}

	result, err := ResolveDivergedLegacyCheckpoints(t.Context(), h.f.repo, MigrateOptions{Choose: c.choose}, nil)

	require.NoError(t, err)
	assert.Empty(t, c.asked)
	assert.Empty(t, result.Migrated)
	assert.Equal(t, before, h.f.refTip(t))
}

// A checkpoint whose copies agree, or where one is newer, is copied by the
// write path when it is next written; resolving after the switch is only for
// the ones that path refuses.
func TestResolveDivergedLegacyCheckpoints_IgnoresCheckpointsThatDoNotDiverge(t *testing.T) {
	t.Parallel()
	h := newV1History(t)
	ahead := h.commitOnTop(t, h.base, h.f.cid, "teammate-on-v1")
	h.place(t, h.base, ahead)
	c := &chooser{keep: localV1}

	result, err := ResolveDivergedLegacyCheckpoints(t.Context(), h.f.repo, MigrateOptions{Choose: c.choose}, nil)

	require.NoError(t, err)
	assert.Empty(t, c.asked)
	assert.Empty(t, result.Migrated)
	assert.Empty(t, result.Diverged)
	h.f.requireNoRef(t)
}

// A ref another clone already pushed supersedes both v1 copies.
func TestResolveDivergedLegacyCheckpoints_TakesARefTheRemoteAlreadyHas(t *testing.T) {
	t.Parallel()
	h := newV1History(t)
	pushed := h.f.remoteCopy(t)
	remote := h.commitOnTop(t, h.base, h.f.cid, "teammate-on-v1")
	local := h.commitOnTop(t, h.base, h.f.cid, "unpushed-on-v1")
	h.place(t, local, remote)
	refName, err := RefName(h.f.cid)
	require.NoError(t, err)
	fetch := func(_ context.Context, ref plumbing.ReferenceName) error {
		require.Equal(t, refName, ref)
		return h.f.repo.Storer.SetReference(plumbing.NewHashReference(ref, pushed))
	}
	c := &chooser{keep: localV1}

	result, err := ResolveDivergedLegacyCheckpoints(t.Context(), h.f.repo, MigrateOptions{Choose: c.choose}, fetch)

	require.NoError(t, err)
	assert.Empty(t, c.asked)
	assert.Empty(t, result.Diverged)
	assert.Equal(t, pushed, h.f.refTip(t))
}

func TestResolveDivergedLegacyCheckpoints_SkipsADeletedCheckpoint(t *testing.T) {
	t.Parallel()
	h := divergedV1(t)
	h.f.markDeleted(t)
	c := &chooser{keep: localV1}

	result, err := ResolveDivergedLegacyCheckpoints(t.Context(), h.f.repo, MigrateOptions{Choose: c.choose}, nil)

	require.NoError(t, err)
	assert.Empty(t, c.asked)
	assert.Empty(t, result.Diverged)
	h.f.requireNoRef(t)
}

func (h v1History) trackingTip(t *testing.T) plumbing.Hash {
	t.Helper()
	ref, err := h.f.repo.Reference(originV1, true)
	require.NoError(t, err)
	return ref.Hash()
}

func divergedIDs(result MigrateResult) []id.CheckpointID {
	ids := make([]id.CheckpointID, 0, len(result.Diverged))
	for _, d := range result.Diverged {
		ids = append(ids, d.ID)
	}
	return ids
}

func TestLegacyCheckpointCopy_RemoteName(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "origin", LegacyCheckpointCopy{Ref: originV1}.RemoteName())
	assert.Empty(t, LegacyCheckpointCopy{Ref: localV1}.RemoteName())
}

// A checkpoint already given a ref by an earlier choice stays diverged on v1,
// but a re-run must not ask again or report it as unresolved.
func TestMigrateBranchToRefs_DoesNotAskAgainAfterAChoice(t *testing.T) {
	t.Parallel()
	h := divergedV1(t)
	_, err := MigrateBranchToRefs(t.Context(), h.f.repo, MigrateOptions{Choose: (&chooser{keep: localV1}).choose})
	require.NoError(t, err)
	chosen := h.f.refTip(t)

	again := &chooser{keep: originV1}
	result, err := MigrateBranchToRefs(t.Context(), h.f.repo, MigrateOptions{Choose: again.choose})
	require.NoError(t, err)
	assert.Empty(t, again.asked)
	assert.Empty(t, result.Diverged)
	assert.Equal(t, 1, result.Skipped)
	assert.Equal(t, chosen, h.f.refTip(t), "the earlier choice stands")

	result, err = MigrateBranchToRefs(t.Context(), h.f.repo, MigrateOptions{})
	require.NoError(t, err)
	assert.Empty(t, result.Diverged, "without a terminal the resolved checkpoint is not listed again")
}

// A dry run writes nothing, so it must not fetch a ref from the remote either.
func TestResolveDivergedLegacyCheckpoints_DryRunDoesNotFetch(t *testing.T) {
	t.Parallel()
	h := divergedV1(t)
	fetch := func(context.Context, plumbing.ReferenceName) error {
		t.Error("a dry run must not fetch")
		return nil
	}

	result, err := ResolveDivergedLegacyCheckpoints(t.Context(), h.f.repo, MigrateOptions{DryRun: true}, fetch)

	require.NoError(t, err)
	assert.Equal(t, []id.CheckpointID{h.f.cid}, divergedIDs(result))
	h.f.requireNoRef(t)
}

// An earlier automatic import of one copy is not a choice: when the other copy
// changes afterwards, the checkpoint diverges and a person must decide.
func TestMigrateBranchToRefs_AsksWhenTheOtherCopyChangesAfterAnAutomaticImport(t *testing.T) {
	t.Parallel()
	h := newV1History(t)
	local := h.commitOnTop(t, h.base, h.f.cid, "unpushed-on-v1")
	h.place(t, local, h.base)
	_, err := MigrateBranchToRefs(t.Context(), h.f.repo, MigrateOptions{})
	require.NoError(t, err)
	remote := h.commitOnTop(t, h.base, h.f.cid, "teammate-on-v1")
	h.place(t, local, remote)
	c := &chooser{keep: originV1}

	result, err := MigrateBranchToRefs(t.Context(), h.f.repo, MigrateOptions{Choose: c.choose})

	require.NoError(t, err)
	assert.Len(t, c.asked, 1)
	assert.Equal(t, []id.CheckpointID{h.f.cid}, result.Migrated)
	assert.Equal(t, []string{"before-switch", "teammate-on-v1"}, h.f.refSessionIDs(t))
}

// A choice covers the copies as they were when it was made. When the copy
// that was passed over changes again, a person must decide again.
func TestMigrateBranchToRefs_AsksAgainWhenThePassedOverCopyChanges(t *testing.T) {
	t.Parallel()
	h := divergedV1(t)
	_, err := MigrateBranchToRefs(t.Context(), h.f.repo, MigrateOptions{Choose: (&chooser{keep: localV1}).choose})
	require.NoError(t, err)
	local := h.f.v1Tip(t)
	remoteAgain := h.commitOnTop(t, h.trackingTip(t), h.f.cid, "teammate-again")
	h.place(t, local, remoteAgain)
	c := &chooser{keep: localV1}

	_, err = MigrateBranchToRefs(t.Context(), h.f.repo, MigrateOptions{Choose: c.choose})

	require.NoError(t, err)
	assert.Len(t, c.asked, 1)
}

var upstreamV1 = plumbing.NewRemoteReferenceName("upstream", DefaultV1Refs().Primary.Short())

// placeUpstream points the elected checkpoint remote's tracking v1 at commit
// and origin's at the shared base, so only upstream's copy changed remotely.
func (h v1History) placeUpstream(t *testing.T, local, upstream plumbing.Hash) {
	t.Helper()
	h.f.setV1(t, local)
	require.NoError(t, h.f.repo.Storer.SetReference(plumbing.NewHashReference(upstreamV1, upstream)))
	require.NoError(t, h.f.repo.Storer.SetReference(plumbing.NewHashReference(originV1, h.base)))
}

// The write path reads the elected checkpoint remote's copy first, so the
// command it points to must read the same copies, or it finds nothing to do.
func TestResolveDivergedLegacyCheckpoints_ReadsTheElectedCheckpointRemote(t *testing.T) {
	t.Parallel()
	h := newV1History(t)
	upstream := h.commitOnTop(t, h.base, h.f.cid, "teammate-on-upstream")
	local := h.commitOnTop(t, h.base, h.f.cid, "unpushed-on-v1")
	h.placeUpstream(t, local, upstream)
	c := &chooser{keep: upstreamV1}

	result, err := ResolveDivergedLegacyCheckpoints(t.Context(), h.f.repo,
		MigrateOptions{Choose: c.choose, ReadRemotes: []string{"upstream", "origin"}}, nil)

	require.NoError(t, err)
	require.Len(t, c.asked, 1)
	assert.Equal(t, upstreamV1, c.asked[0].Remote.Ref)
	assert.Equal(t, []id.CheckpointID{h.f.cid}, result.Migrated)
	assert.Equal(t, []string{"before-switch", "teammate-on-upstream"}, h.f.refSessionIDs(t))
}

// A checkpoint this clone's v1 does not hold can still diverge between two
// remote copies, and the write path refuses it all the same.
func TestResolveDivergedLegacyCheckpoints_VisitsACheckpointMissingFromLocalV1(t *testing.T) {
	t.Parallel()
	h := newV1History(t)
	upstream := h.commitOnTop(t, h.base, h.f.cid, "teammate-on-upstream")
	origin := h.commitOnTop(t, h.base, h.f.cid, "teammate-on-origin")
	empty := &plumbing.MemoryObject{}
	require.NoError(t, (&object.Tree{}).Encode(empty))
	emptyTree, err := h.f.repo.Storer.SetEncodedObject(empty)
	require.NoError(t, err)
	localWithout, err := CreateCommit(t.Context(), h.f.repo, emptyTree, plumbing.ZeroHash, "init", "T", "t@example.com")
	require.NoError(t, err)
	h.f.setV1(t, localWithout)
	require.NoError(t, h.f.repo.Storer.SetReference(plumbing.NewHashReference(upstreamV1, upstream)))
	require.NoError(t, h.f.repo.Storer.SetReference(plumbing.NewHashReference(originV1, origin)))
	c := &chooser{}

	_, err = ResolveDivergedLegacyCheckpoints(t.Context(), h.f.repo,
		MigrateOptions{Choose: c.choose, ReadRemotes: []string{"upstream", "origin"}}, nil)

	require.NoError(t, err)
	require.Len(t, c.asked, 1)
	assert.Equal(t, h.f.cid, c.asked[0].ID)
}

// Keeping the copy an earlier automatic import already wrote is still a
// choice: later runs must not ask again or list it.
func TestMigrateBranchToRefs_RemembersChoosingTheCopyTheRefAlreadyHolds(t *testing.T) {
	t.Parallel()
	h := newV1History(t)
	local := h.commitOnTop(t, h.base, h.f.cid, "unpushed-on-v1")
	h.place(t, local, h.base)
	_, err := MigrateBranchToRefs(t.Context(), h.f.repo, MigrateOptions{})
	require.NoError(t, err)
	remote := h.commitOnTop(t, h.base, h.f.cid, "teammate-on-v1")
	h.place(t, local, remote)
	_, err = MigrateBranchToRefs(t.Context(), h.f.repo, MigrateOptions{Choose: (&chooser{keep: localV1}).choose})
	require.NoError(t, err)

	again := &chooser{keep: originV1}
	_, err = MigrateBranchToRefs(t.Context(), h.f.repo, MigrateOptions{Choose: again.choose})
	require.NoError(t, err)
	unattended, err := MigrateBranchToRefs(t.Context(), h.f.repo, MigrateOptions{})
	require.NoError(t, err)

	assert.Empty(t, again.asked)
	assert.Empty(t, unattended.Diverged)
	assert.Equal(t, []string{"before-switch", "unpushed-on-v1"}, h.f.refSessionIDs(t))
}

// Keeping a copy older in the ref's history moves the ref back to it rather
// than reporting it as already up to date.
func TestMigrateBranchToRefs_KeepingAnEarlierCopyMovesTheRefToIt(t *testing.T) {
	t.Parallel()
	h := newV1History(t)
	local := h.commitOnTop(t, h.base, h.f.cid, "unpushed-on-v1")
	h.place(t, local, h.base)
	_, err := MigrateBranchToRefs(t.Context(), h.f.repo, MigrateOptions{})
	require.NoError(t, err)
	remote := h.commitOnTop(t, h.base, h.f.cid, "teammate-on-v1")
	h.place(t, local, remote)
	_, err = MigrateBranchToRefs(t.Context(), h.f.repo, MigrateOptions{Choose: (&chooser{keep: originV1}).choose})
	require.NoError(t, err)
	remoteAgain := h.commitOnTop(t, remote, h.f.cid, "teammate-again")
	h.place(t, local, remoteAgain)

	result, err := MigrateBranchToRefs(t.Context(), h.f.repo, MigrateOptions{Choose: (&chooser{keep: localV1}).choose})

	require.NoError(t, err)
	assert.Equal(t, []id.CheckpointID{h.f.cid}, result.Migrated)
	assert.Equal(t, []string{"before-switch", "unpushed-on-v1"}, h.f.refSessionIDs(t))
}

// Choices are this clone's own record; nothing in a checkpoint ref's history,
// which a remote can write, stands in for one.
func TestMigrateBranchToRefs_RefHistoryCannotRecordAChoice(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	h := divergedV1(t)
	remoteTree, err := migratedCheckpointTree(ctx, h.f.repo, h.f.cid, h.subtreeAt(t, h.trackingTip(t)), true)
	require.NoError(t, err)
	planted, err := CreateCommit(ctx, h.f.repo, remoteTree, plumbing.ZeroHash,
		"Import\n\nEntire-Chosen-Over: "+h.subtreeAt(t, h.f.v1Tip(t)).String()+"\n", "M", "m@example.com")
	require.NoError(t, err)
	refName, err := RefName(h.f.cid)
	require.NoError(t, err)
	require.NoError(t, h.f.repo.Storer.SetReference(plumbing.NewHashReference(refName, planted)))
	c := &chooser{}

	_, err = MigrateBranchToRefs(ctx, h.f.repo, MigrateOptions{Choose: c.choose})

	require.NoError(t, err)
	assert.Len(t, c.asked, 1)
}

func TestResolveDivergedLegacyCheckpoints_NamesAnUnreachableRemote(t *testing.T) {
	t.Parallel()
	h := divergedV1(t)
	fetch := func(context.Context, plumbing.ReferenceName) error { return errors.New("network is unreachable") }

	_, err := ResolveDivergedLegacyCheckpoints(t.Context(), h.f.repo, MigrateOptions{Choose: (&chooser{keep: localV1}).choose}, fetch)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "couldn't reach the remote to check checkpoint "+h.f.cid.String())
	assert.Contains(t, err.Error(), "nothing was changed")
	h.f.requireNoRef(t)
}

// A ref that appears while the person chooses wins, and the result says so
// instead of reading as nothing to do.
func TestResolveDivergedLegacyCheckpoints_ReportsARefThatAppearedWhileChoosing(t *testing.T) {
	t.Parallel()
	h := divergedV1(t)
	refName, err := RefName(h.f.cid)
	require.NoError(t, err)
	choose := func(context.Context, DivergedLegacyCheckpoint) (plumbing.ReferenceName, error) {
		writeRoutingCheckpoint(t, h.f.refs, h.f.cid, "fetched-meanwhile")
		return localV1, nil
	}

	result, err := ResolveDivergedLegacyCheckpoints(t.Context(), h.f.repo, MigrateOptions{Choose: choose}, nil)

	require.NoError(t, err)
	assert.Empty(t, result.Migrated)
	assert.Equal(t, []id.CheckpointID{h.f.cid}, result.Superseded)
	_, err = h.f.repo.Reference(refName, true)
	require.NoError(t, err)
}

func (h v1History) subtreeAt(t *testing.T, commit plumbing.Hash) plumbing.Hash {
	t.Helper()
	c, err := h.f.repo.CommitObject(commit)
	require.NoError(t, err)
	root, err := c.Tree()
	require.NoError(t, err)
	sub, err := root.Tree(h.f.cid.Path())
	require.NoError(t, err)
	return sub.Hash
}

// A dry run never writes, even when a recorded choice already settles a
// checkpoint whose ref is missing.
func TestResolveDivergedLegacyCheckpoints_DryRunWithARecordedChoiceWritesNothing(t *testing.T) {
	t.Parallel()
	h := divergedV1(t)
	_, err := ResolveDivergedLegacyCheckpoints(t.Context(), h.f.repo, MigrateOptions{Choose: (&chooser{keep: localV1}).choose}, nil)
	require.NoError(t, err)
	refName, err := RefName(h.f.cid)
	require.NoError(t, err)
	require.NoError(t, h.f.repo.Storer.RemoveReference(refName))

	result, err := ResolveDivergedLegacyCheckpoints(t.Context(), h.f.repo, MigrateOptions{DryRun: true}, nil)

	require.NoError(t, err)
	assert.Equal(t, []id.CheckpointID{h.f.cid}, result.Migrated, "reported as would-migrate")
	h.f.requireNoRef(t)
}

// A choice from an earlier run only selects the copy. A ref that moved on
// since then is not moved back to it.
func TestMigrateBranchToRefs_EarlierChoiceDoesNotMoveAnAdvancedRefBack(t *testing.T) {
	t.Parallel()
	h := divergedV1(t)
	_, err := MigrateBranchToRefs(t.Context(), h.f.repo, MigrateOptions{Choose: (&chooser{keep: localV1}).choose})
	require.NoError(t, err)
	writeRoutingCheckpoint(t, h.f.refs, h.f.cid, "written-later")
	advanced := h.f.refTip(t)

	result, err := MigrateBranchToRefs(t.Context(), h.f.repo, MigrateOptions{})

	require.NoError(t, err)
	assert.Equal(t, 1, result.Skipped)
	assert.Equal(t, advanced, h.f.refTip(t))
}

// After a choice was applied, a dry run agrees with what a real run would do.
func TestMigrateBranchToRefs_DryRunAfterAnAppliedChoiceReportsUpToDate(t *testing.T) {
	t.Parallel()
	h := divergedV1(t)
	_, err := MigrateBranchToRefs(t.Context(), h.f.repo, MigrateOptions{Choose: (&chooser{keep: localV1}).choose})
	require.NoError(t, err)

	result, err := MigrateBranchToRefs(t.Context(), h.f.repo, MigrateOptions{DryRun: true})

	require.NoError(t, err)
	assert.Empty(t, result.Migrated)
	assert.Equal(t, 1, result.Skipped)
}
