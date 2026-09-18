package strategy

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

func TestFetchAndRebase_RepairedRemoteWithRemoteOnlySuffixReplaysLocalSuffix(t *testing.T) {
	dir, repo, cleanRoot, _, sharedTip := bloatedV1Fixture(t)
	t.Chdir(dir)
	ctx := context.Background()

	localPath := "22/3333333333/0/metadata.json"
	localTip := appendCheckpointFiles(t, repo, sharedTip, "Checkpoint: 223333333333", map[string][]byte{
		localPath: []byte(`{"checkpoint_id":"223333333333","source":"local"}` + "\n"),
	})
	repairedSharedTip, err := newMetadataRewriter(ctx, repo, shrinkTestThreshold).rewriteHistory(sharedTip)
	require.NoError(t, err)
	repairedC3 := readCommit(t, repo, repairedSharedTip)
	repairedC2 := readCommit(t, repo, repairedC3.ParentHashes[0])
	independentC2 := makeOrphanCommit(t, repo, repairedC2.TreeHash, []plumbing.Hash{cleanRoot}, repairedC2.Message)
	independentC3 := makeOrphanCommit(t, repo, repairedC3.TreeHash, []plumbing.Hash{independentC2}, repairedC3.Message)
	require.NotEqual(t, repairedSharedTip, independentC3,
		"an independently signed repair has different commits with the same checkpoint trees")
	remotePath := "44/5555555555/0/metadata.json"
	remoteTip := appendCheckpointFiles(t, repo, independentC3, "Checkpoint: 445555555555", map[string][]byte{
		remotePath: []byte(`{"checkpoint_id":"445555555555","source":"remote"}` + "\n"),
	})

	bare := pushV1ToBare(t, dir, remoteTip)
	setV1Tip(t, repo, localTip)
	ref := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	delivered, err := pushRefIfNeededWithMetadataThreshold(
		ctx,
		"file://"+bare,
		ref,
		shrinkTestThreshold,
	)
	require.NoError(t, err)
	require.True(t, delivered)

	got := readTip(t, repo)
	assert.Equal(t, got.String(), bareV1(t, bare), "the retry must deliver the reconciled tip")
	gotCommit := readCommit(t, repo, got)
	require.Equal(t, []plumbing.Hash{remoteTip}, gotCommit.ParentHashes,
		"the local-only checkpoint must be replayed onto the actual remote tip")
	assert.Equal(t, []string{
		"Checkpoint: 223333333333",
		"Checkpoint: 445555555555",
		"Checkpoint: eeffffffffff",
		"Checkpoint: ccdddddddddd",
		"Checkpoint: aabbbbbbbbbb",
	}, commitMessages(t, repo, got), "shared repaired checkpoints must not be duplicated")
	assertCommitHasFile(t, repo, got, localPath)
	assertCommitHasFile(t, repo, got, remotePath)

	oversized, err := findOversizedMetadataBlobs(ctx, repo, got, shrinkTestThreshold)
	require.NoError(t, err)
	assert.Empty(t, oversized)
	assertNoFetchTmpRefsWithPurpose(t, repo, "push-recovery")

	again, handled, err := reconcileOversizedV1ForPush(ctx, repo, got, remoteTip, shrinkTestThreshold)
	require.NoError(t, err)
	assert.False(t, handled)
	assert.Equal(t, got, again, "already-clean history is idempotent")
	delivered, err = pushRefIfNeededWithMetadataThreshold(ctx, "file://"+bare, ref, shrinkTestThreshold)
	require.NoError(t, err)
	assert.True(t, delivered)
	assert.Equal(t, got.String(), bareV1(t, bare), "a repeated push is a no-op")
}

func TestPrePush_RepairedRemoteConvergesWithoutHistoricalBloat(t *testing.T) {
	dir, repo, _, _, sharedTip := bloatedV1Fixture(t)
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	ctx := context.Background()
	localTip := appendCheckpointFiles(t, repo, sharedTip, "Checkpoint: local-only", map[string][]byte{
		"22/3333333333/0/metadata.json": []byte(`{"checkpoint_id":"223333333333"}` + "\n"),
	})
	repairedSharedTip, err := newMetadataRewriter(ctx, repo, shrinkTestThreshold).rewriteHistory(sharedTip)
	require.NoError(t, err)
	remoteTip := appendCheckpointFiles(t, repo, repairedSharedTip, "Checkpoint: remote-only", map[string][]byte{
		"44/5555555555/0/metadata.json": []byte(`{"checkpoint_id":"445555555555"}` + "\n"),
	})
	bare := pushV1ToBare(t, dir, remoteTip)
	setV1Tip(t, repo, localTip)
	testutil.WriteFile(t, dir, ".entire/settings.json", `{"enabled":true}`+"\n")

	require.NoError(t, NewManualCommitStrategy().prePushWithMetadataThreshold(
		ctx, "origin", false, shrinkTestThreshold,
	))
	got := readTip(t, repo)
	assert.Equal(t, got.String(), bareV1(t, bare))
	oversized, err := findOversizedMetadataBlobs(ctx, repo, got, shrinkTestThreshold)
	require.NoError(t, err)
	assert.Empty(t, oversized)
	assert.Equal(t, []string{
		"Checkpoint: local-only",
		"Checkpoint: remote-only",
		"Checkpoint: eeffffffffff",
		"Checkpoint: ccdddddddddd",
		"Checkpoint: aabbbbbbbbbb",
	}, commitMessages(t, repo, got))
	require.NoError(t, NewManualCommitStrategy().prePushWithMetadataThreshold(
		ctx, "origin", false, shrinkTestThreshold,
	))
	assert.Equal(t, got, readTip(t, repo), "a repeated pre-push must not create another checkpoint commit")
}

func TestPrepareOversizedV1ForPush_RemoteBranchAbsent(t *testing.T) {
	for _, targetKind := range []string{"named", "url"} {
		t.Run(targetKind, func(t *testing.T) {
			dir, repo, _, _, oldTip := bloatedV1Fixture(t)
			t.Chdir(dir)
			bare := t.TempDir()
			testutil.RunGit(t, dir, "init", "--bare", bare)
			target := "file://" + bare
			if targetKind == "named" {
				testutil.RunGit(t, dir, "remote", "add", "origin", bare)
				target = "origin"
			}

			require.NoError(t, prepareOversizedV1ForPush(
				context.Background(), repo, target, shrinkTestThreshold,
			))
			newTip := readTip(t, repo)
			require.NotEqual(t, oldTip, newTip)
			oversized, err := findOversizedMetadataBlobs(context.Background(), repo, newTip, shrinkTestThreshold)
			require.NoError(t, err)
			assert.Empty(t, oversized)

			delivered, err := pushRefIfNeededWithMetadataThreshold(
				context.Background(), target, plumbing.NewBranchReferenceName(paths.MetadataBranchName), shrinkTestThreshold,
			)
			require.NoError(t, err)
			assert.True(t, delivered)
			assert.Equal(t, newTip.String(), bareV1(t, bare))
		})
	}
}

func TestNewFetchTmpRefIsUniqueAndValid(t *testing.T) {
	t.Parallel()

	seen := make(map[plumbing.ReferenceName]struct{})
	for range 100 {
		ref, err := newFetchTmpRef("metadata-cleanup-v1")
		require.NoError(t, err)
		require.NoError(t, ref.Validate())
		assert.Contains(t, ref.String(), FetchTmpRefPrefix+"metadata-cleanup-v1/")
		if _, exists := seen[ref]; exists {
			t.Fatalf("newFetchTmpRef() returned duplicate %s", ref)
		}
		seen[ref] = struct{}{}
	}
}

func TestFetchV1TipForMetadataCleanup_IsolatesConcurrentTargets(t *testing.T) {
	t.Parallel()
	dir, repo, firstTip := setupV1RepoInDir(t)
	secondTip := appendCheckpointFiles(t, repo, firstTip, "Checkpoint: second remote", map[string][]byte{
		"22/2222222222/0/metadata.json": []byte(`{"checkpoint_id":"222222222222"}` + "\n"),
	})
	targets := []struct {
		path string
		tip  plumbing.Hash
	}{
		{path: t.TempDir(), tip: firstTip},
		{path: t.TempDir(), tip: secondTip},
	}
	for _, target := range targets {
		testutil.RunGit(t, dir, "init", "--bare", target.path)
		testutil.RunGit(t, dir, "push", target.path, target.tip.String()+":refs/heads/"+paths.MetadataBranchName)
	}

	var wg sync.WaitGroup
	errs := make([]error, len(targets))
	got := make([]plumbing.Hash, len(targets))
	for i, target := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], errs[i] = fetchV1TipForMetadataCleanup(t.Context(), repo, "file://"+target.path)
		}()
	}
	wg.Wait()
	for i, target := range targets {
		require.NoError(t, errs[i])
		assert.Equal(t, target.tip, got[i])
	}
	assertNoFetchTmpRefsWithPurpose(t, repo, metadataCleanupFetchPurpose)
}

func TestPrepareOversizedV1ForPush_RemovesHistoricalBlobMissingFromTip(t *testing.T) {
	dir, repo, _, _, tipWithBloat := bloatedV1Fixture(t)
	t.Chdir(dir)
	ctx := context.Background()
	tipTree := readCommit(t, repo, tipWithBloat).TreeHash
	withoutBloatedMetadata, err := checkpoint.ApplyTreeChanges(ctx, repo, tipTree, []checkpoint.TreeChange{{
		Path: bloatedMetadataPath,
	}})
	require.NoError(t, err)
	oldTip := makeOrphanCommit(t, repo, withoutBloatedMetadata, []plumbing.Hash{tipWithBloat}, "metadata replaced")
	setV1Tip(t, repo, oldTip)
	_, err = readCommit(t, repo, oldTip).File(bloatedMetadataPath)
	require.Error(t, err, "the oversized blob must be historical rather than present in the tip tree")

	bare := t.TempDir()
	testutil.RunGit(t, dir, "init", "--bare", bare)
	testutil.RunGit(t, dir, "remote", "add", "origin", bare)
	require.NoError(t, prepareOversizedV1ForPush(ctx, repo, "origin", shrinkTestThreshold))

	newTip := readTip(t, repo)
	require.NotEqual(t, oldTip, newTip)
	oversized, err := findOversizedMetadataBlobs(ctx, repo, newTip, shrinkTestThreshold)
	require.NoError(t, err)
	assert.Empty(t, oversized, "the blob absent from the tip tree must also be removed from rewritten history")
	delivered, err := pushRefIfNeededWithMetadataThreshold(
		ctx, "origin", plumbing.NewBranchReferenceName(paths.MetadataBranchName), shrinkTestThreshold,
	)
	require.NoError(t, err)
	assert.True(t, delivered)
	assert.Equal(t, newTip.String(), bareV1(t, bare))
}

func TestPrepareOversizedV1ForPush_InitialHistoryBeyondReplayLimit(t *testing.T) {
	dir, repo, _, _, tip := bloatedV1Fixture(t)
	t.Chdir(dir)

	for range MaxCommitTraversalDepth {
		parent := readCommit(t, repo, tip)
		tip = makeOrphanCommit(t, repo, parent.TreeHash, []plumbing.Hash{tip}, "Checkpoint: clean suffix")
	}
	setV1Tip(t, repo, tip)

	bare := t.TempDir()
	testutil.RunGit(t, dir, "init", "--bare", bare)
	require.NoError(t, prepareOversizedV1ForPush(
		context.Background(), repo, "file://"+bare, shrinkTestThreshold,
	))

	newTip := readTip(t, repo)
	require.NotEqual(t, tip, newTip)
	oversized, err := findOversizedMetadataBlobs(context.Background(), repo, newTip, shrinkTestThreshold)
	require.NoError(t, err)
	assert.Empty(t, oversized)
}

func TestPrepareOversizedV1ForPush_BoundsOnlyLocalReplaySuffix(t *testing.T) {
	dir, repo, _, _, sharedTip := bloatedV1Fixture(t)
	t.Chdir(dir)

	localTip := sharedTip
	for i := range MaxCommitTraversalDepth + 1 {
		localTip = appendCheckpointFiles(t, repo, localTip, "Checkpoint: local suffix", map[string][]byte{
			"ff/ffffffffff/0/metadata.json": []byte(fmt.Sprintf(`{"checkpoint_id":"%012x"}`, i) + "\n"),
		})
	}
	setV1Tip(t, repo, localTip)
	repairedRemoteTip, err := newMetadataRewriter(
		context.Background(), repo, shrinkTestThreshold,
	).rewriteHistory(sharedTip)
	require.NoError(t, err)
	pushV1ToBare(t, dir, repairedRemoteTip)
	setV1Tip(t, repo, localTip)

	err = prepareOversizedV1ForPush(context.Background(), repo, "origin", shrinkTestThreshold)
	require.ErrorContains(t, err, fmt.Sprintf("commit chain exceeded %d commits", MaxCommitTraversalDepth))
	assert.Equal(t, localTip, readTip(t, repo), "an over-limit replay must leave the local ref unchanged")
}

func TestPrepareOversizedV1ForPush_RewritesAfterCleanShallowBoundary(t *testing.T) {
	dir, repo, a := setupV1RepoInDir(t)
	b := appendCheckpointFiles(t, repo, a, "Checkpoint: clean boundary", map[string][]byte{
		"bb/bbbbbbbbbb/0/metadata.json": []byte(`{"checkpoint_id":"bbbbbbbbbbbb"}` + "\n"),
	})
	c := appendCheckpointFiles(t, repo, b, "Checkpoint: oversized", map[string][]byte{
		bloatedMetadataPath: sessionMetadataJSON(t, "shallow-bloat", 200),
	})
	d := appendCheckpointFiles(t, repo, c, "Checkpoint: local suffix", map[string][]byte{
		"dd/dddddddddd/0/metadata.json": []byte(`{"checkpoint_id":"dddddddddddd"}` + "\n"),
	})
	baredir := pushV1ToBare(t, dir, d)

	cloneParent := t.TempDir()
	cloneDir := filepath.Join(cloneParent, "shallow")
	testutil.RunGit(t, cloneParent, "clone", "--depth=3", "--branch", paths.MetadataBranchName, "file://"+baredir, cloneDir)
	shallowRepo, err := git.PlainOpen(cloneDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = shallowRepo.Close() })
	_, err = shallowRepo.CommitObject(a)
	require.Error(t, err, "the pre-boundary parent must genuinely be absent")

	emptyRemote := t.TempDir()
	testutil.RunGit(t, cloneDir, "init", "--bare", emptyRemote)
	t.Chdir(cloneDir)
	require.NoError(t, prepareOversizedV1ForPush(
		context.Background(), shallowRepo, "file://"+emptyRemote, shrinkTestThreshold,
	))

	newTip := readTip(t, shallowRepo)
	require.NotEqual(t, d, newTip)
	rewrittenBoundary := commitAt(t, shallowRepo, newTip, 2)
	assert.Equal(t, b, rewrittenBoundary.Hash, "clean shallow boundary must remain unchanged")
	oversized, err := hasOversizedMetadataIntroducedSince(
		context.Background(), shallowRepo, newTip, plumbing.ZeroHash, shrinkTestThreshold,
	)
	require.NoError(t, err)
	assert.False(t, oversized)
}

func TestPrepareOversizedV1ForPush_ShallowInitialPushWhenRemoteHasBoundary(t *testing.T) {
	dir, repo, a := setupV1RepoInDir(t)
	b := appendCheckpointFiles(t, repo, a, "Checkpoint: clean boundary", map[string][]byte{
		"bb/bbbbbbbbbb/0/metadata.json": []byte(`{"checkpoint_id":"bbbbbbbbbbbb"}` + "\n"),
	})
	c := appendCheckpointFiles(t, repo, b, "Checkpoint: oversized", map[string][]byte{
		bloatedMetadataPath: sessionMetadataJSON(t, "shallow-bloat", 200),
	})
	tip := appendCheckpointFiles(t, repo, c, "Checkpoint: local suffix", map[string][]byte{
		"dd/dddddddddd/0/metadata.json": []byte(`{"checkpoint_id":"dddddddddddd"}` + "\n"),
	})
	sourceBare := pushV1ToBare(t, dir, tip)

	cloneDir := filepath.Join(t.TempDir(), "shallow")
	testutil.RunGit(t, t.TempDir(), "clone", "--depth=3", "--branch", paths.MetadataBranchName, "file://"+sourceBare, cloneDir)
	shallowRepo, err := git.PlainOpen(cloneDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = shallowRepo.Close() })

	targetBare := t.TempDir()
	testutil.RunGit(t, cloneDir, "init", "--bare", targetBare)
	testutil.RunGit(t, dir, "push", targetBare, b.String()+":refs/heads/checkpoint-boundary")
	t.Chdir(cloneDir)
	require.NoError(t, prepareOversizedV1ForPush(
		context.Background(), shallowRepo, "file://"+targetBare, shrinkTestThreshold,
	))

	delivered, err := pushRefIfNeededWithMetadataThreshold(
		context.Background(), "file://"+targetBare,
		plumbing.NewBranchReferenceName(paths.MetadataBranchName), shrinkTestThreshold,
	)
	require.NoError(t, err)
	assert.True(t, delivered)
	assert.Equal(t, readTip(t, shallowRepo).String(), bareV1(t, targetBare))
}

func TestPrepareOversizedV1ForPush_ShallowCloneConvergesOnRepairedRemote(t *testing.T) {
	dir, repo, a := setupV1RepoInDir(t)
	b := appendCheckpointFiles(t, repo, a, "Checkpoint: clean boundary", map[string][]byte{
		"bb/bbbbbbbbbb/0/metadata.json": []byte(`{"checkpoint_id":"bbbbbbbbbbbb"}` + "\n"),
	})
	c := appendCheckpointFiles(t, repo, b, "Checkpoint: oversized", map[string][]byte{
		bloatedMetadataPath: sessionMetadataJSON(t, "shallow-bloat", 200),
	})
	localTip := appendCheckpointFiles(t, repo, c, "Checkpoint: local suffix", map[string][]byte{
		"dd/dddddddddd/0/metadata.json": []byte(`{"checkpoint_id":"dddddddddddd"}` + "\n"),
	})
	sourceBare := pushV1ToBare(t, dir, localTip)

	cloneDir := filepath.Join(t.TempDir(), "shallow")
	testutil.RunGit(t, t.TempDir(), "clone", "--depth=3", "--branch", paths.MetadataBranchName, "file://"+sourceBare, cloneDir)
	shallowRepo, err := git.PlainOpen(cloneDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = shallowRepo.Close() })

	repairedTip, err := newMetadataRewriter(context.Background(), repo, shrinkTestThreshold).rewriteHistory(c)
	require.NoError(t, err)
	remoteTip := appendCheckpointFiles(t, repo, repairedTip, "Checkpoint: remote suffix", map[string][]byte{
		"ee/eeeeeeeeee/0/metadata.json": []byte(`{"checkpoint_id":"eeeeeeeeeeee"}` + "\n"),
	})
	targetBare := t.TempDir()
	testutil.RunGit(t, dir, "init", "--bare", targetBare)
	testutil.RunGit(t, dir, "push", targetBare, remoteTip.String()+":refs/heads/"+paths.MetadataBranchName)

	t.Chdir(cloneDir)
	require.NoError(t, prepareOversizedV1ForPush(
		context.Background(), shallowRepo, "file://"+targetBare, shrinkTestThreshold,
	))
	got := readTip(t, shallowRepo)
	assertCommitHasFile(t, shallowRepo, got, "dd/dddddddddd/0/metadata.json")
	assertCommitHasFile(t, shallowRepo, got, "ee/eeeeeeeeee/0/metadata.json")
	assert.Equal(t, []string{
		"Checkpoint: local suffix",
		"Checkpoint: remote suffix",
		"Checkpoint: oversized",
		"Checkpoint: clean boundary",
	}, []string{
		readCommit(t, shallowRepo, got).Message,
		commitAt(t, shallowRepo, got, 1).Message,
		commitAt(t, shallowRepo, got, 2).Message,
		commitAt(t, shallowRepo, got, 3).Message,
	})
}

func TestPrepareOversizedV1ForPush_RejectsOversizedShallowBoundary(t *testing.T) {
	dir, _, _, _, tip := bloatedV1Fixture(t)
	baredir := pushV1ToBare(t, dir, tip)

	cloneParent := t.TempDir()
	cloneDir := filepath.Join(cloneParent, "shallow")
	testutil.RunGit(t, cloneParent, "clone", "--depth=2", "--branch", paths.MetadataBranchName, "file://"+baredir, cloneDir)
	shallowRepo, err := git.PlainOpen(cloneDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = shallowRepo.Close() })

	emptyRemote := t.TempDir()
	testutil.RunGit(t, cloneDir, "init", "--bare", emptyRemote)
	t.Chdir(cloneDir)
	err = prepareOversizedV1ForPush(
		context.Background(), shallowRepo, "file://"+emptyRemote, shrinkTestThreshold,
	)
	var shallowErr *shallowMetadataRepairError
	require.ErrorAs(t, err, &shallowErr)
	assert.Equal(t, tip, readTip(t, shallowRepo), "failed shallow repair must not move the local ref")
}

func TestPrepareOversizedV1ForPush_RepairedRootWithoutHashMergeBase(t *testing.T) {
	dir, repo, _ := setupV1RepoInDir(t)
	t.Chdir(dir)
	ctx := context.Background()
	rootFiles := map[string][]byte{
		paths.MetadataFileName: []byte(`{"checkpoints":1}` + "\n"),
		bloatedMetadataPath:    sessionMetadataJSON(t, "root-bloat", 200),
		bloatedTranscript:      []byte(`{"role":"user"}` + "\n"),
	}
	oldRoot := testutil.CommitFiles(t, repo, nil, rootFiles, "Checkpoint: root-bloat")
	oldSharedTip := appendCheckpointFiles(t, repo, oldRoot, "Checkpoint: shared", map[string][]byte{
		"20/0000000000/0/metadata.json": []byte(`{"checkpoint_id":"200000000000"}` + "\n"),
	})
	localTip := appendCheckpointFiles(t, repo, oldSharedTip, "Checkpoint: local-only", map[string][]byte{
		"30/0000000000/0/metadata.json": []byte(`{"checkpoint_id":"300000000000"}` + "\n"),
	})

	fixedSharedTip, err := newMetadataRewriter(ctx, repo, shrinkTestThreshold).rewriteHistory(oldSharedTip)
	require.NoError(t, err)
	fixedShared := readCommit(t, repo, fixedSharedTip)
	fixedRoot := readCommit(t, repo, fixedShared.ParentHashes[0])
	independentRoot := makeOrphanCommit(t, repo, fixedRoot.TreeHash, nil, fixedRoot.Message)
	independentShared := makeOrphanCommit(t, repo, fixedShared.TreeHash, []plumbing.Hash{independentRoot}, fixedShared.Message)
	remoteTip := appendCheckpointFiles(t, repo, independentShared, "Checkpoint: remote-only", map[string][]byte{
		"40/0000000000/0/metadata.json": []byte(`{"checkpoint_id":"400000000000"}` + "\n"),
	})
	mergeBase, err := computeMergeBase(repo, localTip, remoteTip)
	require.NoError(t, err)
	require.True(t, mergeBase.IsZero(), "rewriting an affected root leaves no shared commit hash")

	bare := pushV1ToBare(t, dir, remoteTip)
	setV1Tip(t, repo, localTip)
	require.NoError(t, prepareOversizedV1ForPush(ctx, repo, "origin", shrinkTestThreshold))
	got := readTip(t, repo)
	require.Equal(t, []plumbing.Hash{remoteTip}, readCommit(t, repo, got).ParentHashes)
	assert.Equal(t, []string{
		"Checkpoint: local-only",
		"Checkpoint: remote-only",
		"Checkpoint: shared",
		"Checkpoint: root-bloat",
	}, commitMessages(t, repo, got))
	oversized, err := findOversizedMetadataBlobs(ctx, repo, got, shrinkTestThreshold)
	require.NoError(t, err)
	assert.Empty(t, oversized)
	assertNoFetchTmpRefsWithPurpose(t, repo, metadataCleanupFetchPurpose)
	assert.Equal(t, remoteTip.String(), bareV1(t, bare), "preflight must not mutate the remote")
}

func TestFetchAndRebase_StripsOversizedLocalOnlyHistory(t *testing.T) {
	dir, repo, remoteTip, _, localTip := bloatedV1Fixture(t)
	t.Chdir(dir)
	ctx := context.Background()

	pushV1ToBare(t, dir, remoteTip)
	setV1Tip(t, repo, localTip)
	require.NoError(t, fetchAndRebaseRefWithMetadataThreshold(
		ctx,
		"origin",
		plumbing.NewBranchReferenceName(paths.MetadataBranchName),
		shrinkTestThreshold,
	))

	got := readTip(t, repo)
	assert.Equal(t, []string{
		"Checkpoint: eeffffffffff",
		"Checkpoint: ccdddddddddd",
		"Checkpoint: aabbbbbbbbbb",
	}, commitMessages(t, repo, got))
	assert.Equal(t, remoteTip, commitAt(t, repo, got, 2).Hash,
		"history before the local-only oversized blob keeps the remote's exact commit")
	oversized, err := findOversizedMetadataBlobs(ctx, repo, got, shrinkTestThreshold)
	require.NoError(t, err)
	assert.Empty(t, oversized)
}

func TestFetchAndRebase_UnrepairedRemoteUsesExactMergeBase(t *testing.T) {
	dir, repo, _, _, sharedTip := bloatedV1Fixture(t)
	t.Chdir(dir)
	ctx := context.Background()

	localTip := appendCheckpointFiles(t, repo, sharedTip, "Checkpoint: local-only", map[string][]byte{
		"66/7777777777/0/metadata.json": []byte(`{"checkpoint_id":"667777777777"}` + "\n"),
	})
	remoteTip := appendCheckpointFiles(t, repo, sharedTip, "Checkpoint: remote-only", map[string][]byte{
		"88/9999999999/0/metadata.json": []byte(`{"checkpoint_id":"889999999999"}` + "\n"),
	})
	pushV1ToBare(t, dir, remoteTip)
	setV1Tip(t, repo, localTip)
	before, handled, err := reconcileOversizedV1ForPush(ctx, repo, localTip, remoteTip, shrinkTestThreshold)
	require.NoError(t, err)
	assert.False(t, handled)
	assert.Equal(t, localTip, before,
		"bloat introduced before the exact merge-base is already remote-owned and must not select an older tree boundary")

	require.NoError(t, fetchAndRebaseRefWithMetadataThreshold(
		ctx,
		"origin",
		plumbing.NewBranchReferenceName(paths.MetadataBranchName),
		shrinkTestThreshold,
	))

	got := readTip(t, repo)
	require.Equal(t, []plumbing.Hash{remoteTip}, readCommit(t, repo, got).ParentHashes)
	assert.Equal(t, []string{
		"Checkpoint: local-only",
		"Checkpoint: remote-only",
		"Checkpoint: eeffffffffff",
		"Checkpoint: ccdddddddddd",
		"Checkpoint: aabbbbbbbbbb",
	}, commitMessages(t, repo, got),
		"an older clean tree must not move the replay boundary behind the exact merge-base")
	oversized, err := findOversizedMetadataBlobs(ctx, repo, got, shrinkTestThreshold)
	require.NoError(t, err)
	require.Len(t, oversized, 1, "an unrepaired remote stays on the existing replay path")
}

func TestPrepareOversizedV1ForPush_DoesNotTrustCleanTipWithHistoricalRemoteBloat(t *testing.T) {
	dir, repo, _, _, sharedTip := bloatedV1Fixture(t)
	t.Chdir(dir)
	ctx := context.Background()
	localTip := appendCheckpointFiles(t, repo, sharedTip, "Checkpoint: local-only", map[string][]byte{
		"66/7777777777/0/metadata.json": []byte(`{"checkpoint_id":"667777777777"}` + "\n"),
	})
	repairedSharedTip, err := newMetadataRewriter(ctx, repo, shrinkTestThreshold).rewriteHistory(sharedTip)
	require.NoError(t, err)
	_, repairedMetadata := blobAt(t, repo, repairedSharedTip, bloatedMetadataPath)
	remoteTip := appendCheckpointFiles(t, repo, sharedTip, "Checkpoint: remote cleaned tip", map[string][]byte{
		bloatedMetadataPath: repairedMetadata,
	})
	require.Equal(t, readCommit(t, repo, repairedSharedTip).TreeHash, readCommit(t, repo, remoteTip).TreeHash)

	got, handled, err := reconcileOversizedV1ForPush(ctx, repo, localTip, remoteTip, shrinkTestThreshold)
	require.NoError(t, err)
	assert.False(t, handled)
	assert.Equal(t, localTip, got,
		"a clean cumulative tree must not prove repair while the remote still reaches the old blob")
}

func TestPrepareOversizedV1ForPush_FindsRepairedBoundaryOnRemoteMergeParent(t *testing.T) {
	_, repo, cleanRoot, _, sharedTip := bloatedV1Fixture(t)
	ctx := context.Background()
	localPath := "66/7777777777/0/metadata.json"
	remotePath := "88/9999999999/0/metadata.json"
	localTip := appendCheckpointFiles(t, repo, sharedTip, "Checkpoint: local-only", map[string][]byte{
		localPath: []byte(`{"checkpoint_id":"667777777777"}` + "\n"),
	})
	repairedSharedTip, err := newMetadataRewriter(ctx, repo, shrinkTestThreshold).rewriteHistory(sharedTip)
	require.NoError(t, err)
	remoteFirstParent := appendCheckpointFiles(t, repo, cleanRoot, "Checkpoint: remote first parent", map[string][]byte{
		"77/8888888888/0/metadata.json": []byte(`{"checkpoint_id":"778888888888"}` + "\n"),
	})
	remoteTreeCommit := appendCheckpointFiles(t, repo, repairedSharedTip, "remote merge tree", map[string][]byte{
		remotePath: []byte(`{"checkpoint_id":"889999999999"}` + "\n"),
	})
	remoteTip := makeOrphanCommit(
		t,
		repo,
		readCommit(t, repo, remoteTreeCommit).TreeHash,
		[]plumbing.Hash{remoteFirstParent, repairedSharedTip},
		"Checkpoint: remote merge",
	)
	setV1Tip(t, repo, localTip)

	got, handled, err := reconcileOversizedV1ForPush(ctx, repo, localTip, remoteTip, shrinkTestThreshold)
	require.NoError(t, err)
	assert.True(t, handled)
	assertCommitHasFile(t, repo, got, localPath)
	assertCommitHasFile(t, repo, got, remotePath)
	require.Equal(t, []plumbing.Hash{remoteTip}, readCommit(t, repo, got).ParentHashes)
}

func TestRewriteUnpushedV1WithOPF_ReconcilesRepairedRemoteBeforeDivergenceCheck(t *testing.T) {
	configureFakeOPF(t, &fakeOPFForRewrite{})
	dir, repo, _, _, sharedTip := bloatedV1Fixture(t)
	t.Chdir(dir)
	ctx := context.Background()

	localTip := appendCheckpointFiles(t, repo, sharedTip, "Checkpoint: local-after-repair", map[string][]byte{
		"12/3456789abc/0/full.jsonl": []byte(`{"role":"user","content":"PERSONABC"}` + "\n"),
	})
	repairedRemoteTip, err := newMetadataRewriter(ctx, repo, shrinkTestThreshold).rewriteHistory(sharedTip)
	require.NoError(t, err)
	pushV1ToBare(t, dir, repairedRemoteTip)
	setV1Tip(t, repo, localTip)

	got, err := rewriteUnpushedV1WithOPF(ctx, repo, "origin", shrinkTestThreshold)
	require.NoError(t, err)
	gotCommit := readCommit(t, repo, got)
	require.Equal(t, []plumbing.Hash{repairedRemoteTip}, gotCommit.ParentHashes)
	assert.True(t, trailers.HasOPFApplied(gotCommit.Message))
	assert.Len(t, commitMessages(t, repo, got), 4, "the shared repaired history appears once")
	oversized, err := findOversizedMetadataBlobs(ctx, repo, got, shrinkTestThreshold)
	require.NoError(t, err)
	assert.Empty(t, oversized)
}

func TestPrepareOversizedV1ForPush_PreservesMergeFinalTree(t *testing.T) {
	dir, repo, _, _, sharedTip := bloatedV1Fixture(t)
	t.Chdir(dir)
	ctx := context.Background()

	localPath := "11/1111111111/0/metadata.json"
	sidePath := "22/2222222222/0/metadata.json"
	resolutionPath := "33/3333333333/0/metadata.json"
	remotePath := "44/4444444444/0/metadata.json"
	localTip := appendCheckpointFiles(t, repo, sharedTip, "Checkpoint: local first parent", map[string][]byte{
		localPath: []byte(`{"checkpoint_id":"111111111111"}` + "\n"),
	})
	sideTip := appendCheckpointFiles(t, repo, sharedTip, "Checkpoint: local side", map[string][]byte{
		sidePath: []byte(`{"checkpoint_id":"222222222222"}` + "\n"),
	})
	sideFile, err := readCommit(t, repo, sideTip).File(sidePath)
	require.NoError(t, err)
	resolutionBlob, err := checkpoint.CreateBlobFromContent(repo, []byte(`{"checkpoint_id":"333333333333"}`+"\n"))
	require.NoError(t, err)
	mergeTree, err := checkpoint.ApplyTreeChanges(ctx, repo, readCommit(t, repo, localTip).TreeHash, []checkpoint.TreeChange{
		{Path: sidePath, Entry: &object.TreeEntry{Name: filepath.Base(sidePath), Mode: filemode.Regular, Hash: sideFile.Hash}},
		{Path: resolutionPath, Entry: &object.TreeEntry{Name: filepath.Base(resolutionPath), Mode: filemode.Regular, Hash: resolutionBlob}},
	})
	require.NoError(t, err)
	mergeTip := makeOrphanCommit(t, repo, mergeTree, []plumbing.Hash{localTip, sideTip}, "Checkpoint: merge resolution")

	repairedShared, err := newMetadataRewriter(ctx, repo, shrinkTestThreshold).rewriteHistory(sharedTip)
	require.NoError(t, err)
	remoteTip := appendCheckpointFiles(t, repo, repairedShared, "Checkpoint: remote-only", map[string][]byte{
		remotePath: []byte(`{"checkpoint_id":"444444444444"}` + "\n"),
	})
	pushV1ToBare(t, dir, remoteTip)
	setV1Tip(t, repo, mergeTip)

	require.NoError(t, prepareOversizedV1ForPush(ctx, repo, "origin", shrinkTestThreshold))
	got := readTip(t, repo)
	for _, filePath := range []string{localPath, sidePath, resolutionPath, remotePath} {
		assertCommitHasFile(t, repo, got, filePath)
	}
	assert.Equal(t, []string{
		"Checkpoint: merge resolution",
		"Checkpoint: local first parent",
		"Checkpoint: remote-only",
		"Checkpoint: eeffffffffff",
		"Checkpoint: ccdddddddddd",
		"Checkpoint: aabbbbbbbbbb",
	}, commitMessages(t, repo, got))
}

func TestCherryPickOnto_SkipsChangeAlreadyPresentAtTip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	sourceParent := testutil.CommitFiles(t, repo, nil, map[string][]byte{"metadata.json": []byte("old\n")}, "source parent")
	sourceTip := testutil.CommitFiles(t, repo, []plumbing.Hash{sourceParent}, map[string][]byte{"metadata.json": []byte("shared\n")}, "source change")
	base := testutil.CommitFiles(t, repo, nil, map[string][]byte{"metadata.json": []byte("shared\n")}, "remote already has change")
	sourceCommit := readCommit(t, repo, sourceTip)

	got, err := cherryPickOnto(context.Background(), repo, base, []*object.Commit{sourceCommit}, nil)
	require.NoError(t, err)
	assert.Equal(t, base, got, "an already-present tree change must not create a duplicate commit")
}

func appendCheckpointFiles(
	t *testing.T,
	repo *git.Repository,
	parent plumbing.Hash,
	message string,
	files map[string][]byte,
) plumbing.Hash {
	t.Helper()
	parentCommit := readCommit(t, repo, parent)
	changes := make([]checkpoint.TreeChange, 0, len(files))
	for path, content := range files {
		blob, err := checkpoint.CreateBlobFromContent(repo, content)
		require.NoError(t, err)
		changes = append(changes, checkpoint.TreeChange{
			Path: path,
			Entry: &object.TreeEntry{
				Name: filepath.Base(path),
				Mode: filemode.Regular,
				Hash: blob,
			},
		})
	}
	tree, err := checkpoint.ApplyTreeChanges(context.Background(), repo, parentCommit.TreeHash, changes)
	require.NoError(t, err)
	return makeOrphanCommit(t, repo, tree, []plumbing.Hash{parent}, message)
}

func setV1Tip(t *testing.T, repo *git.Repository, tip plumbing.Hash) {
	t.Helper()
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(
		plumbing.NewBranchReferenceName(paths.MetadataBranchName), tip,
	)))
}

func assertCommitHasFile(t *testing.T, repo *git.Repository, commit plumbing.Hash, path string) {
	t.Helper()
	_, err := readCommit(t, repo, commit).File(path)
	require.NoError(t, err)
}

func assertNoFetchTmpRefsWithPurpose(t *testing.T, repo *git.Repository, purpose string) {
	t.Helper()
	refs, err := repo.References()
	require.NoError(t, err)
	defer refs.Close()
	err = refs.ForEach(func(ref *plumbing.Reference) error {
		if strings.HasPrefix(ref.Name().String(), FetchTmpRefPrefix+purpose+"/") {
			t.Errorf("temporary fetch ref was not removed: %s", ref.Name())
		}
		return nil
	})
	require.NoError(t, err)
}

func TestFindSharedRewrittenTree_Cancellation(t *testing.T) {
	t.Parallel()
	_, repo, _, _, tip := bloatedV1Fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	repoPath, pathErr := getRepoPath(repo)
	require.NoError(t, pathErr)
	_, _, _, err := findSharedRewrittenTree(ctx, repo, repoPath, tip, tip, map[plumbing.Hash]plumbing.Hash{tip: tip})
	assert.ErrorIs(t, err, context.Canceled)
}

func TestComputeMergeBaseWithGit_RejectsMultipleBases(t *testing.T) {
	t.Parallel()
	dir, repo, root := setupV1RepoInDir(t)
	a := appendCheckpointFiles(t, repo, root, "a", map[string][]byte{
		"a/metadata.json": []byte(`{"checkpoint_id":"aaaaaaaaaaaa"}` + "\n"),
	})
	b := appendCheckpointFiles(t, repo, root, "b", map[string][]byte{
		"b/metadata.json": []byte(`{"checkpoint_id":"bbbbbbbbbbbb"}` + "\n"),
	})
	union := appendCheckpointFiles(t, repo, a, "union tree", map[string][]byte{
		"b/metadata.json": []byte(`{"checkpoint_id":"bbbbbbbbbbbb"}` + "\n"),
	})
	left := makeOrphanCommit(t, repo, readCommit(t, repo, union).TreeHash, []plumbing.Hash{a, b}, "left")
	right := makeOrphanCommit(t, repo, readCommit(t, repo, union).TreeHash, []plumbing.Hash{b, a}, "right")

	_, err := computeMergeBaseWithGit(context.Background(), dir, left, right)
	require.ErrorContains(t, err, "multiple merge bases")
}

func TestHasOversizedMetadataIntroducedSince_ScansMergeSideBranches(t *testing.T) {
	t.Parallel()
	_, repo, cleanRoot, bloatedSide, _ := bloatedV1Fixture(t)
	cleanSide := appendCheckpointFiles(t, repo, cleanRoot, "clean side", map[string][]byte{
		"10/0000000000/0/metadata.json": []byte(`{"checkpoint_id":"100000000000"}` + "\n"),
	})
	merge := makeOrphanCommit(
		t,
		repo,
		readCommit(t, repo, cleanSide).TreeHash,
		[]plumbing.Hash{cleanSide, bloatedSide},
		"merge",
	)

	found, err := hasOversizedMetadataIntroducedSince(
		context.Background(), repo, merge, cleanRoot, shrinkTestThreshold,
	)
	require.NoError(t, err)
	assert.True(t, found, "the boundary on the first-parent path must not hide the other parent's bloat")
}

func TestHasOversizedMetadataIntroducedSince_ScansMergeResolution(t *testing.T) {
	t.Parallel()
	_, repo, root := setupV1RepoInDir(t)
	firstParent := appendCheckpointFiles(t, repo, root, "first parent", map[string][]byte{
		"10/0000000000/0/metadata.json": []byte(`{"checkpoint_id":"100000000000"}` + "\n"),
	})
	secondParent := appendCheckpointFiles(t, repo, root, "second parent", map[string][]byte{
		"20/0000000000/0/metadata.json": []byte(`{"checkpoint_id":"200000000000"}` + "\n"),
	})
	mergeTree := appendCheckpointFiles(t, repo, firstParent, "merge tree", map[string][]byte{
		bloatedMetadataPath: sessionMetadataJSON(t, "merge-resolution", 200),
	})
	merge := makeOrphanCommit(
		t,
		repo,
		readCommit(t, repo, mergeTree).TreeHash,
		[]plumbing.Hash{firstParent, secondParent},
		"merge resolution",
	)

	found, err := hasOversizedMetadataIntroducedSince(
		context.Background(), repo, merge, root, shrinkTestThreshold,
	)
	require.NoError(t, err)
	assert.True(t, found)
}

func TestHasOversizedMetadataIntroducedSince_DetectsBlobAliasedByUnrelatedPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	repo, err := gitrepo.OpenPath(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })

	content := sessionMetadataJSON(t, "aliased", 200)
	tip := testutil.CommitFiles(t, repo, nil, map[string][]byte{
		"aaa-unrelated.bin": content,
		"metadata.json":     content,
	}, "aliased metadata")

	found, err := hasOversizedMetadataIntroducedSince(
		context.Background(), repo, tip, plumbing.ZeroHash, shrinkTestThreshold,
	)
	require.NoError(t, err)
	assert.True(t, found)
}

func BenchmarkHasOversizedMetadataIntroducedSince_CleanHistory(b *testing.B) {
	repo, tip := benchmarkCleanCheckpointHistory(b, 500)
	b.ResetTimer()
	for b.Loop() {
		found, err := hasOversizedMetadataIntroducedSince(context.Background(), repo, tip, plumbing.ZeroHash, shrinkTestThreshold)
		if err != nil {
			b.Fatal(err)
		}
		if found {
			b.Fatal("clean history reported oversized metadata")
		}
	}
}

func BenchmarkPerCommitDiffScanner_CleanHistory(b *testing.B) {
	repo, tip := benchmarkCleanCheckpointHistory(b, 500)
	b.ResetTimer()
	for b.Loop() {
		history, err := repo.Log(&git.LogOptions{From: tip})
		if err != nil {
			b.Fatal(err)
		}
		err = history.ForEach(func(commit *object.Commit) error {
			return forEachMetadataBlobIntroduced(context.Background(), commit, func(_ string, hash plumbing.Hash) error {
				blob, err := repo.BlobObject(hash)
				if err != nil {
					return err
				}
				if blob.Size > shrinkTestThreshold {
					b.Fatal("clean history reported oversized metadata")
				}
				return nil
			})
		})
		history.Close()
		if err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkCleanCheckpointHistory(b *testing.B, commits int) (*git.Repository, plumbing.Hash) {
	b.Helper()
	repo, err := git.PlainInit(b.TempDir(), false)
	if err != nil {
		b.Fatal(err)
	}
	tree := plumbing.ZeroHash
	parent := plumbing.ZeroHash
	for i := range commits {
		blob, err := checkpoint.CreateBlobFromContent(repo, []byte(fmt.Sprintf(`{"checkpoint_id":"%012d"}`+"\n", i)))
		if err != nil {
			b.Fatal(err)
		}
		tree, err = checkpoint.ApplyTreeChanges(context.Background(), repo, tree, []checkpoint.TreeChange{{
			Path: fmt.Sprintf("%02x/%010x/0/metadata.json", i%256, i),
			Entry: &object.TreeEntry{
				Name: "metadata.json",
				Mode: filemode.Regular,
				Hash: blob,
			},
		}})
		if err != nil {
			b.Fatal(err)
		}
		commit := &object.Commit{
			Author:       object.Signature{Name: "Test", Email: "test@example.com", When: time.Unix(int64(i), 0)},
			Committer:    object.Signature{Name: "Test", Email: "test@example.com", When: time.Unix(int64(i), 0)},
			Message:      fmt.Sprintf("Checkpoint: %012d", i),
			TreeHash:     tree,
			ParentHashes: nil,
		}
		if !parent.IsZero() {
			commit.ParentHashes = []plumbing.Hash{parent}
		}
		obj := repo.Storer.NewEncodedObject()
		if err := commit.Encode(obj); err != nil {
			b.Fatal(err)
		}
		parent, err = repo.Storer.SetEncodedObject(obj)
		if err != nil {
			b.Fatal(err)
		}
	}
	return repo, parent
}
