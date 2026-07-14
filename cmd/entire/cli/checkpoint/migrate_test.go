package checkpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/redact"
)

// legacyCheckpointVersion is the checkpoint_version stamp older git-branch
// checkpoints carry; the migration drops it for the refs layout.
const legacyCheckpointVersion = "branch-v1"

// sampleSession builds a checkpoint write request with deterministic content,
// shared so a checkpoint written to the git-branch and git-refs stores has
// byte-identical session contents.
func sampleSession(cid id.CheckpointID, sessionID string) Session {
	return Session{
		CheckpointID: cid,
		SessionID:    sessionID,
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("transcript for " + sessionID + "\n")),
		Prompts:      []string{"do the thing"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}
}

// seedBranchCheckpoint writes one checkpoint to the git-branch v1 store.
func seedBranchCheckpoint(t *testing.T, store *GitStore, cid id.CheckpointID, sessionID string) {
	t.Helper()
	require.NoError(t, store.Write(context.Background(), sampleSession(cid, sessionID)))
}

// mutateBranchCheckpointMetadata rewrites a checkpoint's root metadata.json on
// the v1 branch tip (simulates older-CLI metadata).
func mutateBranchCheckpointMetadata(t *testing.T, repo *git.Repository, cid id.CheckpointID, mutate func(map[string]any)) {
	t.Helper()
	ctx := context.Background()
	branchRef := DefaultV1Refs().Primary
	ref, err := repo.Reference(branchRef, true)
	require.NoError(t, err)
	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)

	metaPath := cid.Path() + "/" + paths.MetadataFileName
	file, err := tree.File(metaPath)
	require.NoError(t, err)
	raw, err := file.Contents()
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &doc))
	mutate(doc)
	edited, err := json.Marshal(doc)
	require.NoError(t, err)

	blobHash, err := CreateBlobFromContent(repo, edited)
	require.NoError(t, err)
	newTree, err := ApplyTreeChanges(ctx, repo, tree.Hash, []TreeChange{{
		Path:  metaPath,
		Entry: &object.TreeEntry{Name: metaPath, Mode: filemode.Regular, Hash: blobHash},
	}})
	require.NoError(t, err)
	commitHash, err := CreateCommit(ctx, repo, newTree, ref.Hash(), "test: legacy metadata", "Test", "test@test.com")
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(branchRef, commitHash)))
}

// migratedMetadataDoc reads a migrated ref tree's root metadata.json as a JSON object.
func migratedMetadataDoc(t *testing.T, repo *git.Repository, commitTree plumbing.Hash) map[string]any {
	t.Helper()
	tree, err := repo.TreeObject(commitTree)
	require.NoError(t, err)
	file, err := tree.File(paths.MetadataFileName)
	require.NoError(t, err)
	raw, err := file.Contents()
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &doc))
	return doc
}

// refHash returns the commit hash a checkpoint's ref points at (fatal if absent).
func refHash(t *testing.T, repo *git.Repository, cid id.CheckpointID) plumbing.Hash {
	t.Helper()
	refName, err := RefName(cid)
	require.NoError(t, err)
	ref, err := repo.Reference(refName, true)
	require.NoError(t, err)
	return ref.Hash()
}

func TestMigrateBranchToRefs(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	ctx := context.Background()
	branch := NewGitStore(repo, DefaultV1Refs())

	cid1 := id.MustCheckpointID("a1b2c3d4e5f6")
	cid2 := id.MustCheckpointID("b2c3d4e5f6a1")
	seedBranchCheckpoint(t, branch, cid1, "s1")
	seedBranchCheckpoint(t, branch, cid2, "s2")

	// cid1 carries legacy metadata: a checkpoint_version stamp plus an unmodeled field.
	mutateBranchCheckpointMetadata(t, repo, cid1, func(doc map[string]any) {
		doc["checkpoint_version"] = legacyCheckpointVersion
		doc["future_field"] = "keep-me"
	})

	result, err := MigrateBranchToRefs(ctx, repo, false)
	require.NoError(t, err)
	assert.Equal(t, 2, result.Total)
	assert.Len(t, result.Migrated, 2)
	assert.Equal(t, 0, result.Skipped)

	// Each ref carries the branch subtree with a normalized root metadata.json
	// and reads back through the git-refs store.
	branchTree, err := branch.getSessionsBranchTree()
	require.NoError(t, err)
	refsStore := newGitRefsStore(repo)
	for _, cid := range []id.CheckpointID{cid1, cid2} {
		commit, err := repo.CommitObject(refHash(t, repo, cid))
		require.NoError(t, err)

		// Session contents carry over byte-identical; only the root
		// metadata.json is rewritten.
		branchSession, err := refsStore.subtreeObjAt(branchTree.Hash, cid.Path()+"/0")
		require.NoError(t, err)
		require.NotNil(t, branchSession)
		refSession, err := refsStore.subtreeObjAt(commit.TreeHash, "0")
		require.NoError(t, err)
		require.NotNil(t, refSession)
		assert.Equal(t, branchSession.Hash, refSession.Hash,
			"session subtree must be the branch's, byte-identical")

		// checkpoint_version is dropped; sessions[] paths are rebased to the ref root.
		doc := migratedMetadataDoc(t, repo, commit.TreeHash)
		assert.NotContains(t, doc, "checkpoint_version", "legacy version stamp must be dropped")
		sessions, ok := doc["sessions"].([]any)
		require.True(t, ok, "sessions must be an array")
		require.Len(t, sessions, 1)
		session, ok := sessions[0].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "/0/metadata.json", session["metadata"])
		assert.Equal(t, "/0/full.jsonl", session["transcript"])
		assert.Equal(t, "/0/prompt.txt", session["prompt"])
		assert.Equal(t, "/0/content_hash.txt", session["content_hash"])

		// A migration commit wraps the tree with no parent (orphan).
		assert.Empty(t, commit.ParentHashes, "first migration commit is an orphan")

		summary, err := refsStore.Read(ctx, cid)
		require.NoError(t, err)
		require.NotNil(t, summary, "migrated checkpoint should read via git-refs")
		assert.Equal(t, cid, summary.CheckpointID)
	}

	// Fields this CLI doesn't model survive the rewrite untouched.
	commit1, err := repo.CommitObject(refHash(t, repo, cid1))
	require.NoError(t, err)
	doc1 := migratedMetadataDoc(t, repo, commit1.TreeHash)
	assert.Equal(t, "keep-me", doc1["future_field"], "unknown metadata fields must be preserved")

	// Migrated refs are enqueued for push (the doctor's "push now" depends on it).
	queue, err := PushQueueForRepo(ctx, repo)
	require.NoError(t, err)
	queued, err := queue.Drain()
	require.NoError(t, err)
	wantQueued := make([]plumbing.ReferenceName, 0, 2)
	for _, cid := range []id.CheckpointID{cid1, cid2} {
		refName, err := RefName(cid)
		require.NoError(t, err)
		wantQueued = append(wantQueued, refName)
	}
	assert.ElementsMatch(t, wantQueued, queued, "migrated refs must be queued for push")

	// Idempotent: a second run skips everything and leaves the refs untouched.
	before := map[string]plumbing.Hash{cid1.String(): refHash(t, repo, cid1), cid2.String(): refHash(t, repo, cid2)}
	result2, err := MigrateBranchToRefs(ctx, repo, false)
	require.NoError(t, err)
	assert.Equal(t, 2, result2.Total)
	assert.Empty(t, result2.Migrated, "nothing to migrate on a repeat run")
	assert.Equal(t, 2, result2.Skipped)
	assert.Equal(t, before[cid1.String()], refHash(t, repo, cid1), "idempotent re-run must not move refs")
	assert.Equal(t, before[cid2.String()], refHash(t, repo, cid2))
}

// TestMigrateBranchToRefs_MetadataMatchesNativeRefsLayout pins the migration's
// metadata rebasing to the git-refs writer it must mirror. The migration rebases
// session paths by string surgery rather than round-tripping the metadata model,
// so if the native layout ever drifts (a renamed session dir, a new path field,
// a non-prefix-relative field) this fails loudly instead of silently shipping
// checkpoints whose paths a native reader can't resolve.
func TestMigrateBranchToRefs_MetadataMatchesNativeRefsLayout(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	ctx := context.Background()

	// The same checkpoint content written two ways: natively by the git-refs
	// store, and migrated from the git-branch store.
	nativeCID := id.MustCheckpointID("aaaaaaaaaaaa")
	migratedCID := id.MustCheckpointID("bbbbbbbbbbbb")

	refsStore := newGitRefsStore(repo)
	require.NoError(t, refsStore.Write(ctx, sampleSession(nativeCID, "s1")))

	branch := NewGitStore(repo, DefaultV1Refs())
	seedBranchCheckpoint(t, branch, migratedCID, "s1")
	_, err := MigrateBranchToRefs(ctx, repo, false)
	require.NoError(t, err)

	nativeCommit, err := repo.CommitObject(refHash(t, repo, nativeCID))
	require.NoError(t, err)
	migratedCommit, err := repo.CommitObject(refHash(t, repo, migratedCID))
	require.NoError(t, err)

	native := sessionPathFields(t, migratedMetadataDoc(t, repo, nativeCommit.TreeHash))
	migrated := sessionPathFields(t, migratedMetadataDoc(t, repo, migratedCommit.TreeHash))
	require.NotEmpty(t, migrated, "sanity: migrated metadata carries session paths")
	assert.Equal(t, native, migrated,
		"migrated session paths must match the native git-refs layout")
}

// sessionPathFields returns the sorted "field=value" pairs of every path-shaped
// (leading "/") string value under sessions[] — the layout the migration must
// keep in lockstep with the writer.
func sessionPathFields(t *testing.T, doc map[string]any) []string {
	t.Helper()
	sessions, ok := doc["sessions"].([]any)
	require.True(t, ok, "sessions must be an array")
	var out []string
	for i, entry := range sessions {
		session, ok := entry.(map[string]any)
		require.True(t, ok)
		for field, v := range session {
			if s, ok := v.(string); ok && strings.HasPrefix(s, "/") {
				out = append(out, fmt.Sprintf("%d.%s=%s", i, field, s))
			}
		}
	}
	sort.Strings(out)
	return out
}

func TestMigrateBranchToRefs_AdvancesOnBranchChange(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	ctx := context.Background()
	branch := NewGitStore(repo, DefaultV1Refs())
	cid := id.MustCheckpointID("a1b2c3d4e5f6")

	seedBranchCheckpoint(t, branch, cid, "s1")
	_, err := MigrateBranchToRefs(ctx, repo, false)
	require.NoError(t, err)
	first := refHash(t, repo, cid)

	// The branch checkpoint gains a second session (its subtree changes).
	seedBranchCheckpoint(t, branch, cid, "s2")

	result, err := MigrateBranchToRefs(ctx, repo, false)
	require.NoError(t, err)
	assert.Len(t, result.Migrated, 1, "changed checkpoint is re-migrated")
	assert.Equal(t, 0, result.Skipped)

	second := refHash(t, repo, cid)
	assert.NotEqual(t, first, second, "ref advances to the new tree")

	// The advance is a fast-forward: the prior migration commit is the parent,
	// so no history is lost.
	commit, err := repo.CommitObject(second)
	require.NoError(t, err)
	require.Len(t, commit.ParentHashes, 1)
	assert.Equal(t, first, commit.ParentHashes[0])
}

func TestMigrateBranchToRefs_DryRunWritesNothing(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	ctx := context.Background()
	branch := NewGitStore(repo, DefaultV1Refs())
	cid := id.MustCheckpointID("a1b2c3d4e5f6")
	seedBranchCheckpoint(t, branch, cid, "s1")
	// Legacy metadata so normalization rewrites the tree — the case that used to
	// persist a blob + tree even under dry-run.
	mutateBranchCheckpointMetadata(t, repo, cid, func(doc map[string]any) {
		doc["checkpoint_version"] = legacyCheckpointVersion
	})

	before := countObjects(t, repo)
	result, err := MigrateBranchToRefs(ctx, repo, true)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Total)
	assert.Len(t, result.Migrated, 1, "dry-run reports what would migrate")
	assert.Equal(t, before, countObjects(t, repo), "dry-run must not write git objects")

	refName, err := RefName(cid)
	require.NoError(t, err)
	_, err = repo.Reference(refName, true)
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound, "dry-run must not write refs")

	queue, err := PushQueueForRepo(ctx, repo)
	require.NoError(t, err)
	queued, err := queue.Drain()
	require.NoError(t, err)
	assert.Empty(t, queued, "dry-run must not enqueue refs for push")
}

// countObjects returns the number of objects in the repo's object store.
func countObjects(t *testing.T, repo *git.Repository) int {
	t.Helper()
	iter, err := repo.Storer.IterEncodedObjects(plumbing.AnyObject)
	require.NoError(t, err)
	defer iter.Close()
	n := 0
	require.NoError(t, iter.ForEach(func(plumbing.EncodedObject) error {
		n++
		return nil
	}))
	return n
}

func TestMigrateBranchToRefs_DryRunRecognizesAlreadyMigrated(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	ctx := context.Background()
	branch := NewGitStore(repo, DefaultV1Refs())
	cid := id.MustCheckpointID("a1b2c3d4e5f6")
	seedBranchCheckpoint(t, branch, cid, "s1")
	mutateBranchCheckpointMetadata(t, repo, cid, func(doc map[string]any) {
		doc["checkpoint_version"] = legacyCheckpointVersion
	})

	// Real migration persists the normalized tree.
	res, err := MigrateBranchToRefs(ctx, repo, false)
	require.NoError(t, err)
	require.Len(t, res.Migrated, 1)

	// Dry-run must see it as already migrated — proving the non-persisting hash
	// computation matches the persisted tree hash byte-for-byte.
	dry, err := MigrateBranchToRefs(ctx, repo, true)
	require.NoError(t, err)
	assert.Empty(t, dry.Migrated, "already-migrated checkpoint is not a would-migrate")
	assert.Equal(t, 1, dry.Skipped)
}

func TestMigrateBranchToRefs_SkipsRefAdvancedPastBranchSnapshot(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	ctx := context.Background()
	branch := NewGitStore(repo, DefaultV1Refs())
	cid := id.MustCheckpointID("a1b2c3d4e5f6")
	seedBranchCheckpoint(t, branch, cid, "s1")

	_, err := MigrateBranchToRefs(ctx, repo, false)
	require.NoError(t, err)
	imported := refHash(t, repo, cid)

	// The ref advances past the migration snapshot, as a refs-store write
	// (e.g. a summary backfill) would: a new commit with a different tree,
	// parented on the imported commit.
	head, err := repo.Head()
	require.NoError(t, err)
	headCommit, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)
	advanced, err := CreateCommit(ctx, repo, headCommit.TreeHash, imported, "refs-store write", "Test", "test@test.com")
	require.NoError(t, err)
	refName, err := RefName(cid)
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, advanced)))

	// A re-run must recognize the already-imported snapshot in the ref's
	// history and skip — not regress the tip to the old branch tree.
	result, err := MigrateBranchToRefs(ctx, repo, false)
	require.NoError(t, err)
	assert.Empty(t, result.Migrated, "already-imported checkpoint must not be re-migrated")
	assert.Equal(t, 1, result.Skipped)
	assert.Equal(t, advanced, refHash(t, repo, cid), "ref tip must keep the newer refs-store write")
}

func TestMigrateBranchToRefs_UnreadableRefIsReplacedWithOrphan(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	branch := NewGitStore(repo, DefaultV1Refs())
	cid := id.MustCheckpointID("a1b2c3d4e5f6")
	seedBranchCheckpoint(t, branch, cid, "s1")

	// A ref at a nonexistent commit: the lookup succeeds but the commit read
	// fails. The migration treats it as absent rather than parenting on the
	// bad hash.
	refName, err := RefName(cid)
	require.NoError(t, err)
	bogus := plumbing.NewHash("0123456789abcdef0123456789abcdef01234567")
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, bogus)))

	result, err := MigrateBranchToRefs(context.Background(), repo, false)
	require.NoError(t, err)
	assert.Len(t, result.Migrated, 1)

	commit, err := repo.CommitObject(refHash(t, repo, cid))
	require.NoError(t, err)
	assert.Empty(t, commit.ParentHashes, "unreadable ref must not become the parent")
}

func TestMigrateBranchToRefs_EnqueueFailureIsError(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	ctx := context.Background()
	branch := NewGitStore(repo, DefaultV1Refs())
	cid := id.MustCheckpointID("a1b2c3d4e5f6")
	seedBranchCheckpoint(t, branch, cid, "s1")

	// Occupy the queue file path with a directory so appending fails: the
	// queued-for-push contract must surface this, not leave the migrated ref
	// silently unpushed.
	queue, err := PushQueueForRepo(ctx, repo)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(queue.queuePath(), 0o755))

	_, err = MigrateBranchToRefs(ctx, repo, false)
	require.Error(t, err, "a failed enqueue must fail the migration")
}

func TestMigrateBranchToRefs_ReenqueuesAlreadyImportedRef(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	ctx := context.Background()
	branch := NewGitStore(repo, DefaultV1Refs())
	cid := id.MustCheckpointID("a1b2c3d4e5f6")
	seedBranchCheckpoint(t, branch, cid, "s1")

	_, err := MigrateBranchToRefs(ctx, repo, false)
	require.NoError(t, err)
	first := refHash(t, repo, cid)

	// Simulate a prior run that wrote the ref but lost its push-queue entry
	// (a failed enqueue, or a crash between setRef and Enqueue): the ref exists
	// but nothing is queued for it.
	refName, err := RefName(cid)
	require.NoError(t, err)
	queue, err := PushQueueForRepo(ctx, repo)
	require.NoError(t, err)
	require.NoError(t, queue.Remove([]plumbing.ReferenceName{refName}))
	emptied, err := queue.Drain()
	require.NoError(t, err)
	require.Empty(t, emptied, "precondition: the ref is written but unqueued")

	// Re-running skips the already-imported snapshot but must re-enqueue it, so
	// the ref a partial earlier run left behind still reaches the remote.
	result, err := MigrateBranchToRefs(ctx, repo, false)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Skipped, "already-imported checkpoint is a skip")
	assert.Empty(t, result.Migrated)
	assert.Equal(t, first, refHash(t, repo, cid), "skip must not move the ref")

	requeued, err := queue.Drain()
	require.NoError(t, err)
	assert.ElementsMatch(t, []plumbing.ReferenceName{refName}, requeued,
		"an already-imported ref must be re-enqueued so a lost prior enqueue still pushes")
}

func TestMigrateBranchToRefs_NoBranchIsNoop(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t) // initial commit only; no v1 checkpoint branch yet
	result, err := MigrateBranchToRefs(context.Background(), repo, false)
	require.NoError(t, err)
	assert.Equal(t, 0, result.Total)
	assert.Empty(t, result.Migrated)
}
