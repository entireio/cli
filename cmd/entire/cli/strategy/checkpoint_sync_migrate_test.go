package strategy

import (
	"context"
	"fmt"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// syncFixture is a work repo with checkpoint refs, an "old" bare remote the refs
// were pushed to, and an empty "new" bare remote.
type syncFixture struct {
	workDir string
	oldBare string
	newBare string
	repo    *git.Repository
	refs    []plumbing.ReferenceName
	head    plumbing.Hash
}

// newSyncFixture builds the fixture. The work repo has remotes "old" and "new"
// pointing at the two bares; refs are pushed to "old" only.
func newSyncFixture(t *testing.T) *syncFixture {
	t.Helper()
	workDir, oldBare, refs := setupRepoWithCheckpointRefs(t)
	newBare := t.TempDir()
	testutil.RunGit(t, newBare, "init", "--bare")
	testutil.AddRemote(t, workDir, "old", oldBare)
	testutil.AddRemote(t, workDir, "new", newBare)

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)

	f := &syncFixture{workDir: workDir, oldBare: oldBare, newBare: newBare, repo: repo, refs: refs, head: head.Hash()}
	f.pushTo(t, "old", refs...)
	return f
}

// pushTo pushes refs from the work repo to remoteName.
func (f *syncFixture) pushTo(t *testing.T, remoteName string, refs ...plumbing.ReferenceName) {
	t.Helper()
	for _, ref := range refs {
		testutil.RunGit(t, f.workDir, "push", "-q", remoteName, ref.String()+":"+ref.String())
	}
}

func (f *syncFixture) setLocal(t *testing.T, ref plumbing.ReferenceName, h plumbing.Hash) {
	t.Helper()
	require.NoError(t, f.repo.Storer.SetReference(plumbing.NewHashReference(ref, h)))
}

func (f *syncFixture) localHash(t *testing.T, ref plumbing.ReferenceName) plumbing.Hash {
	t.Helper()
	r, err := f.repo.Reference(ref, true)
	require.NoError(t, err)
	return r.Hash()
}

// commit adds a file and commits it, returning the new HEAD.
func (f *syncFixture) commit(t *testing.T, name string) plumbing.Hash {
	t.Helper()
	testutil.WriteFile(t, f.workDir, name, name)
	testutil.GitAdd(t, f.workDir, name)
	testutil.GitCommit(t, f.workDir, "add "+name)
	h, err := f.repo.Head()
	require.NoError(t, err)
	return h.Hash()
}

func lsRemoteRefs(t *testing.T, bareDir string) string {
	t.Helper()
	return testutil.RunGit(t, bareDir, "for-each-ref", "--format=%(refname) %(objectname)")
}

func TestInventoryCheckpointArtifacts(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	ctx := context.Background()

	// A v1 branch on the old remote too.
	testutil.RunGit(t, f.workDir, "push", "-q", "old", "HEAD:"+v1BranchRef.String())

	inv, err := inventoryCheckpointArtifactsIn(ctx, f.workDir, "old")
	require.NoError(t, err)
	assert.Equal(t, "old", inv.Remote)
	assert.Len(t, inv.Refs, 2)
	for _, ref := range f.refs {
		assert.Equal(t, f.head, inv.Refs[ref])
	}
	assert.Equal(t, f.head, inv.V1Branch)
	assert.False(t, inv.Empty())
	assert.Equal(t, []plumbing.ReferenceName{f.refs[1], f.refs[0]}, inv.RefNames(), "names are sorted (a1/… before f6/…)")

	empty, err := inventoryCheckpointArtifactsIn(ctx, f.workDir, "new")
	require.NoError(t, err)
	assert.True(t, empty.Empty())
	assert.True(t, empty.V1Branch.IsZero())
}

// TestInventoryCheckpointArtifacts_ByName exercises the exported entry point,
// which resolves the worktree root from the process cwd.
func TestInventoryCheckpointArtifacts_ByName(t *testing.T) {
	f := newSyncFixture(t)
	t.Chdir(f.workDir)

	inv, err := InventoryCheckpointArtifacts(context.Background(), "old")
	require.NoError(t, err)
	assert.Len(t, inv.Refs, 2)

	_, err = InventoryCheckpointArtifacts(context.Background(), "nonexistent-remote")
	require.Error(t, err, "an unknown remote is an error, not an empty inventory")
}

func TestHydrateCheckpointArtifacts_MissingLocalRefIsFetched(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	ctx := context.Background()

	// Drop the local ref; the old remote still has it.
	require.NoError(t, f.repo.Storer.RemoveReference(f.refs[0]))
	_, err := f.repo.Reference(f.refs[0], true)
	require.Error(t, err)

	inv, err := inventoryCheckpointArtifactsIn(ctx, f.workDir, "old")
	require.NoError(t, err)
	res, err := HydrateCheckpointArtifacts(ctx, f.repo, "old", inv, nil)
	require.NoError(t, err)

	assert.Equal(t, 1, res.RefsFetched, "only the missing ref is a candidate")
	assert.Equal(t, 1, res.RefsAdvanced)
	assert.Equal(t, 0, res.RefsReplayed)
	assert.False(t, res.V1Advanced)
	assert.Equal(t, f.head, f.localHash(t, f.refs[0]))
	assertNoSyncTmpRefs(t, f.workDir)
}

func TestHydrateCheckpointArtifacts_AheadRemoteAdvancesLocal(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	ctx := context.Background()

	c2 := f.commit(t, "two.txt")
	f.setLocal(t, f.refs[0], c2)
	f.pushTo(t, "old", f.refs[0])    // remote = C2
	f.setLocal(t, f.refs[0], f.head) // local back to C1: behind

	inv, err := inventoryCheckpointArtifactsIn(ctx, f.workDir, "old")
	require.NoError(t, err)
	res, err := HydrateCheckpointArtifacts(ctx, f.repo, "old", inv, nil)
	require.NoError(t, err)

	assert.Equal(t, 1, res.RefsFetched)
	assert.Equal(t, 1, res.RefsAdvanced)
	assert.Equal(t, c2, f.localHash(t, f.refs[0]), "behind local ref fast-forwards to the remote tip")
	assertNoSyncTmpRefs(t, f.workDir)
}

func TestHydrateCheckpointArtifacts_AheadLocalUntouched(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	ctx := context.Background()

	c2 := f.commit(t, "two.txt")
	f.setLocal(t, f.refs[0], c2) // local ahead of the remote's C1

	inv, err := inventoryCheckpointArtifactsIn(ctx, f.workDir, "old")
	require.NoError(t, err)
	res, err := HydrateCheckpointArtifacts(ctx, f.repo, "old", inv, nil)
	require.NoError(t, err)

	assert.Equal(t, 1, res.RefsFetched, "a differing hash is fetched so divergence can be judged")
	assert.Equal(t, 0, res.RefsAdvanced, "an ahead local ref is never moved backwards")
	assert.Equal(t, c2, f.localHash(t, f.refs[0]))
	assertNoSyncTmpRefs(t, f.workDir)
}

func TestHydrateCheckpointArtifacts_DivergedIsReplayed(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	ctx := context.Background()

	// Remote side: C2 = C1 + b.txt.
	c2 := f.commit(t, "b.txt")
	f.setLocal(t, f.refs[0], c2)
	f.pushTo(t, "old", f.refs[0])

	// Local side: back to C1, then C3 = C1 + a.txt. Merge base is C1.
	testutil.RunGit(t, f.workDir, "reset", "-q", "--hard", f.head.String())
	c3 := f.commit(t, "a.txt")
	f.setLocal(t, f.refs[0], c3)

	inv, err := inventoryCheckpointArtifactsIn(ctx, f.workDir, "old")
	require.NoError(t, err)
	res, err := HydrateCheckpointArtifacts(ctx, f.repo, "old", inv, nil)
	require.NoError(t, err)

	assert.Equal(t, 1, res.RefsReplayed)
	assert.Equal(t, 0, res.RefsAdvanced)
	after := f.localHash(t, f.refs[0])
	assert.NotEqual(t, c2, after)
	assert.NotEqual(t, c3, after)
	// The replayed tip descends from the remote tip, so a later push is a fast-forward.
	testutil.RunGit(t, f.workDir, "merge-base", "--is-ancestor", c2.String(), after.String())
	assertNoSyncTmpRefs(t, f.workDir)
}

func TestHydrateCheckpointArtifacts_V1Advanced(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	ctx := context.Background()

	testutil.RunGit(t, f.workDir, "push", "-q", "old", "HEAD:"+v1BranchRef.String())
	_, err := f.repo.Reference(v1BranchRef, true)
	require.Error(t, err, "no local v1 branch before hydration")

	inv, err := inventoryCheckpointArtifactsIn(ctx, f.workDir, "old")
	require.NoError(t, err)
	res, err := HydrateCheckpointArtifacts(ctx, f.repo, "old", inv, nil)
	require.NoError(t, err)

	assert.True(t, res.V1Advanced)
	assert.Equal(t, 0, res.RefsFetched, "refs already at the remote hash are not re-fetched")
	assert.Equal(t, f.head, f.localHash(t, checkpoint.ResolveRefs(ctx).Primary))
	assertNoSyncTmpRefs(t, f.workDir)
}

func TestHydrateCheckpointArtifacts_ReportsProgress(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	ctx := context.Background()
	require.NoError(t, f.repo.Storer.RemoveReference(f.refs[0]))

	var lines []string
	progress := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	inv, err := inventoryCheckpointArtifactsIn(ctx, f.workDir, "old")
	require.NoError(t, err)
	_, err = HydrateCheckpointArtifacts(ctx, f.repo, "old", inv, progress)
	require.NoError(t, err)
	require.Len(t, lines, 1)
	assert.Contains(t, lines[0], "old (1/1)")
}

func assertNoSyncTmpRefs(t *testing.T, workDir string) {
	t.Helper()
	out := testutil.RunGit(t, workDir, "for-each-ref", syncTmpRefPrefix)
	assert.Empty(t, strings.TrimSpace(out), "temporary sync refs must be cleaned up")
}

func TestRequeueAllCheckpointRefs_EnqueuesEveryLocalRef_DedupOnDrain(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	ctx := context.Background()

	// A malformed ref under the namespace is not a checkpoint ref and is skipped.
	f.setLocal(t, plumbing.ReferenceName(checkpoint.CheckpointRefPrefix+"zz/not-a-matching-shard"), f.head)

	queue, err := checkpoint.PushQueueForRepo(ctx, f.repo)
	require.NoError(t, err)
	require.NoError(t, queue.Enqueue(f.refs[1])) // already queued once

	n, err := RequeueAllCheckpointRefs(ctx, f.repo)
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	queued, err := queue.Drain()
	require.NoError(t, err)
	assert.ElementsMatch(t, f.refs, queued, "drain collapses the duplicate and excludes the malformed ref")
}

func TestRequeueAllCheckpointRefs_NoRefsIsNoop(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	testutil.InitRepo(t, workDir)
	testutil.WriteFile(t, workDir, "f.txt", "x")
	testutil.GitAdd(t, workDir, "f.txt")
	testutil.GitCommit(t, workDir, "init")
	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)

	n, err := RequeueAllCheckpointRefs(context.Background(), repo)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestVerifyCheckpointArtifacts_MissingAndHashMismatchNotVerified(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	ctx := context.Background()

	// refs[0] lands on "new" at the local hash; refs[1] lands at a different
	// hash; a third ref exists locally only; a fourth is expected but has no
	// local ref at all.
	f.pushTo(t, "new", f.refs[0])
	c2 := f.commit(t, "two.txt")
	f.setLocal(t, f.refs[1], c2)
	f.pushTo(t, "new", f.refs[1])
	f.setLocal(t, f.refs[1], f.head)
	localOnly := mustRefName(t, id.MustCheckpointID("c3d4e5f6a1b2"))
	f.setLocal(t, localOnly, f.head)
	noLocal := mustRefName(t, id.MustCheckpointID("d4e5f6a1b2c3"))

	verified, missing, err := VerifyCheckpointArtifacts(ctx, f.repo, "new",
		[]plumbing.ReferenceName{f.refs[0], f.refs[1], localOnly, noLocal})
	require.NoError(t, err)
	assert.Equal(t, []plumbing.ReferenceName{f.refs[0]}, verified)
	assert.Equal(t, []plumbing.ReferenceName{f.refs[1], localOnly, noLocal}, missing)
}

func TestRemoveCheckpointArtifacts_DeletesVerifiedRefsInChunks(t *testing.T) {
	f := newSyncFixture(t)
	t.Chdir(f.workDir)
	ctx := context.Background()

	// 250 refs: more than one chunk of refChunkSize.
	refs := make([]plumbing.ReferenceName, 0, 250)
	for i := range 250 {
		cid := id.MustCheckpointID(fmt.Sprintf("%012x", 0x100000+i))
		ref := mustRefName(t, cid)
		f.setLocal(t, ref, f.head)
		refs = append(refs, ref)
	}
	require.NoError(t, batchPushRefs(ctx, f.oldBare, refs))
	require.Len(t, strings.Split(strings.TrimSpace(lsRemoteRefs(t, f.oldBare)), "\n"), 252, "250 refs plus the fixture's two")

	var progress []string
	res, err := RemoveCheckpointArtifacts(ctx, "old", refs, false, func(format string, args ...any) {
		progress = append(progress, fmt.Sprintf(format, args...))
	})
	require.NoError(t, err)
	assert.Equal(t, 250, res.RefsDeleted)
	assert.Equal(t, 0, res.AlreadyAbsent)
	assert.False(t, res.V1Deleted)
	assert.Len(t, progress, 2, "one progress line per chunk")

	remaining := lsRemoteRefs(t, f.oldBare)
	for _, ref := range refs {
		assert.NotContains(t, remaining, ref.String())
	}
	for _, ref := range f.refs {
		assert.Contains(t, remaining, ref.String(), "refs not passed in are left alone")
	}
}

func TestRemoveCheckpointArtifacts_AlreadyAbsentIsSuccess(t *testing.T) {
	f := newSyncFixture(t)
	t.Chdir(f.workDir)

	testutil.RunGit(t, f.oldBare, "update-ref", "-d", f.refs[0].String())

	res, err := RemoveCheckpointArtifacts(context.Background(), "old", f.refs, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.RefsDeleted)
	assert.Equal(t, 1, res.AlreadyAbsent)
	assert.Empty(t, strings.TrimSpace(lsRemoteRefs(t, f.oldBare)))
}

func TestRemoveCheckpointArtifacts_V1DeletedAndTrackingPruned(t *testing.T) {
	f := newSyncFixture(t)
	t.Chdir(f.workDir)

	testutil.RunGit(t, f.workDir, "push", "-q", "old", "HEAD:"+v1BranchRef.String())
	tracking := plumbing.NewRemoteReferenceName("old", paths.MetadataBranchName)
	f.setLocal(t, tracking, f.head)

	res, err := RemoveCheckpointArtifacts(context.Background(), "old", nil, true, nil)
	require.NoError(t, err)
	assert.True(t, res.V1Deleted)
	assert.Equal(t, 0, res.RefsDeleted)
	assert.NotContains(t, lsRemoteRefs(t, f.oldBare), v1BranchRef.String())
	_, err = f.repo.Reference(tracking, true)
	require.Error(t, err, "stale remote-tracking ref is pruned")

	// A second run finds the branch already gone and reports it as such.
	res, err = RemoveCheckpointArtifacts(context.Background(), "old", nil, true, nil)
	require.NoError(t, err)
	assert.False(t, res.V1Deleted)
}

func TestRemoveCheckpointArtifacts_RefusesFanOutRemote(t *testing.T) {
	f := newSyncFixture(t)
	t.Chdir(f.workDir)

	testutil.RunGit(t, f.workDir, "remote", "set-url", "--add", "--push", "old", f.newBare)
	testutil.RunGit(t, f.workDir, "remote", "set-url", "--add", "--push", "old", f.oldBare)

	_, err := RemoveCheckpointArtifacts(context.Background(), "old", f.refs, false, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pushes to 2 URLs")
	assert.Contains(t, lsRemoteRefs(t, f.oldBare), f.refs[0].String(), "nothing deleted")
}

func TestRemoveCheckpointArtifacts_UnknownRemote(t *testing.T) {
	f := newSyncFixture(t)
	t.Chdir(f.workDir)

	_, err := RemoveCheckpointArtifacts(context.Background(), "nope", f.refs, false, nil)
	require.Error(t, err)
}
