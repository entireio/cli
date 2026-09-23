package strategy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// shrinkTestThreshold keeps fixtures small: a "bloated" metadata.json in these
// tests is a few kilobytes, not 100 MiB.
const shrinkTestThreshold int64 = 1024

const (
	bloatedMetadataPath = "cc/dddddddddd/0/" + paths.MetadataFileName
	bloatedTranscript   = "cc/dddddddddd/0/" + paths.TranscriptFileName
)

// sessionMetadataJSON renders a per-session metadata.json. perFile > 0 adds a
// prompt_attributions record whose user_added_per_file map has that many
// entries — the shape the pre-v0.10.1 nested-checkout bug produced.
func sessionMetadataJSON(t *testing.T, sessionID string, perFile int) []byte {
	t.Helper()
	doc := map[string]any{
		"checkpoint_id": "ccdddddddddd",
		"session_id":    sessionID,
		"agent":         "claude-code",
		"created_at":    "2026-08-01T12:00:00Z",
		"attribution":   map[string]any{"agent_lines": 10, "human_added": 2},
	}
	if perFile > 0 {
		added := make(map[string]int, perFile)
		for i := range perFile {
			added[fmt.Sprintf(".claude/worktrees/agent/src/file%d.go", i)] = 3
		}
		doc["prompt_attributions"] = []map[string]any{{
			"checkpoint_number":     1,
			"user_lines_added":      3 * perFile,
			"user_added_per_file":   added,
			"user_removed_per_file": map[string]int{},
		}}
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	require.NoError(t, err)
	return append(data, '\n')
}

// bloatedV1Fixture builds entire/checkpoints/v1 as three cumulative commits:
// c1 holds one healthy checkpoint, c2 adds a checkpoint whose metadata.json is
// over shrinkTestThreshold, c3 adds another healthy checkpoint (and, being
// cumulative, still references the oversized blob). Returns the three hashes.
func bloatedV1Fixture(t *testing.T) (dir string, repo *git.Repository, c1, c2, c3 plumbing.Hash) {
	t.Helper()
	dir, r, _ := setupV1RepoInDir(t)
	files := map[string][]byte{
		paths.MetadataFileName:                        []byte(`{"checkpoints":1}` + "\n"),
		"aa/bbbbbbbbbb/0/" + paths.MetadataFileName:   sessionMetadataJSON(t, "s1", 0),
		"aa/bbbbbbbbbb/0/" + paths.TranscriptFileName: []byte(`{"role":"user"}` + "\n"),
	}
	c1 = testutil.CommitFiles(t, r, nil, files, "Checkpoint: aabbbbbbbbbb")
	files[bloatedMetadataPath] = sessionMetadataJSON(t, "s2", 200)
	files[bloatedTranscript] = []byte(`{"role":"user","content":"big session"}` + "\n")
	c2 = testutil.CommitFiles(t, r, []plumbing.Hash{c1}, files, "Checkpoint: ccdddddddddd")
	files["ee/ffffffffff/0/"+paths.MetadataFileName] = sessionMetadataJSON(t, "s3", 0)
	files["ee/ffffffffff/0/"+paths.TranscriptFileName] = []byte(`{"role":"user"}` + "\n")
	c3 = testutil.CommitFiles(t, r, []plumbing.Hash{c2}, files, "Checkpoint: eeffffffffff")
	require.NoError(t, r.Storer.SetReference(plumbing.NewHashReference(
		plumbing.NewBranchReferenceName(paths.MetadataBranchName), c3)))
	require.Greater(t, int64(len(files[bloatedMetadataPath])), shrinkTestThreshold, "fixture must exceed the test threshold")
	return dir, r, c1, c2, c3
}

func blobAt(t *testing.T, repo *git.Repository, commit plumbing.Hash, filePath string) (plumbing.Hash, []byte) {
	t.Helper()
	c, err := repo.CommitObject(commit)
	require.NoError(t, err)
	f, err := c.File(filePath)
	require.NoError(t, err, filePath)
	content, err := f.Contents()
	require.NoError(t, err)
	return f.Hash, []byte(content)
}

func TestFindOversizedMetadataBlobs_ReportsBlobsOverThreshold(t *testing.T) {
	t.Parallel()
	_, repo, _, c2, c3 := bloatedV1Fixture(t)

	found, err := findOversizedMetadataBlobs(context.Background(), repo, c3, shrinkTestThreshold)
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, bloatedMetadataPath, found[0].Path)
	assert.Equal(t, c2, found[0].Commit, "attributed to the commit that introduced it")
	assert.Greater(t, found[0].Size, shrinkTestThreshold)

	none, err := findOversizedMetadataBlobs(context.Background(), repo, plumbing.ZeroHash, shrinkTestThreshold)
	require.NoError(t, err)
	assert.Empty(t, none, "an absent branch has nothing oversized")
}

func TestShrinkOversizedCheckpointMetadata_RewritesOnlyAffectedHistory(t *testing.T) {
	t.Parallel()
	_, repo, c1, _, c3 := bloatedV1Fixture(t)
	ctx := context.Background()
	v1 := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	transcriptBefore, _ := blobAt(t, repo, c3, bloatedTranscript)
	healthyBefore, _ := blobAt(t, repo, c3, "aa/bbbbbbbbbb/0/"+paths.MetadataFileName)

	scan, err := ScanOversizedCheckpointMetadata(ctx, repo, "", shrinkTestThreshold)
	require.NoError(t, err)
	require.Len(t, scan.Local, 1)
	assert.Empty(t, scan.Remote)

	res, err := ShrinkOversizedCheckpointMetadata(ctx, repo, scan, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, c3, res.OldLocalTip)
	assert.NotEqual(t, c3, res.NewLocalTip)
	assert.Equal(t, 2, res.CommitsRewritten, "c2 introduced the blob and c3 still referenced it; c1 is untouched")
	assert.Equal(t, 1, res.BlobsShrunk)
	assert.Empty(t, res.StillOversized)
	assert.False(t, res.Pushed)
	assert.Contains(t, res.PushSkippedReason, "no checkpoint sync remote")

	ref, err := repo.Reference(v1, true)
	require.NoError(t, err)
	assert.Equal(t, res.NewLocalTip, ref.Hash(), "local branch moved to the rewritten tip")

	newC3, err := repo.CommitObject(res.NewLocalTip)
	require.NoError(t, err)
	assert.Equal(t, "Checkpoint: eeffffffffff", newC3.Message, "messages are preserved")
	newC2, err := newC3.Parent(0)
	require.NoError(t, err)
	assert.Equal(t, "Checkpoint: ccdddddddddd", newC2.Message)
	assert.Equal(t, []plumbing.Hash{c1}, newC2.ParentHashes, "history before the first oversized blob keeps its hashes")

	_, shrunk := blobAt(t, repo, res.NewLocalTip, bloatedMetadataPath)
	var doc map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(shrunk, &doc))
	assert.NotContains(t, doc, "prompt_attributions")
	assert.Contains(t, doc, "attribution", "the computed summary stays")
	assert.Contains(t, doc, "session_id")
	assert.LessOrEqual(t, int64(len(shrunk)), shrinkTestThreshold)

	transcriptAfter, _ := blobAt(t, repo, res.NewLocalTip, bloatedTranscript)
	assert.Equal(t, transcriptBefore, transcriptAfter, "the transcript blob is byte-identical")
	healthyAfter, _ := blobAt(t, repo, res.NewLocalTip, "aa/bbbbbbbbbb/0/"+paths.MetadataFileName)
	assert.Equal(t, healthyBefore, healthyAfter, "healthy metadata is untouched")

	// Idempotent: a second scan is clean and a second shrink is a no-op.
	again, err := ScanOversizedCheckpointMetadata(ctx, repo, "", shrinkTestThreshold)
	require.NoError(t, err)
	assert.True(t, again.Empty())
	res2, err := ShrinkOversizedCheckpointMetadata(ctx, repo, again, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, 0, res2.CommitsRewritten)
	assert.Equal(t, res.NewLocalTip, res2.NewLocalTip)
}

func TestShrinkOversizedCheckpointMetadata_ForcePushesElectedRemoteAndRecoversRemoteOnlyBloat(t *testing.T) {
	// Not parallel: remote.Fetch/Push and settings.Load resolve the repository
	// from the process working directory.
	dir, repo, _, _, c3 := bloatedV1Fixture(t)
	t.Chdir(dir)
	ctx := context.Background()
	v1 := "refs/heads/" + paths.MetadataBranchName

	bare := t.TempDir()
	testutil.RunGit(t, dir, "init", "--bare", bare)
	testutil.RunGit(t, dir, "remote", "add", "origin", bare)
	testutil.RunGit(t, dir, "push", "origin", v1)
	require.Equal(t, c3.String(), strings.TrimSpace(testutil.RunGit(t, bare, "rev-parse", v1)))

	scan, err := ScanOversizedCheckpointMetadata(ctx, repo, "origin", shrinkTestThreshold)
	require.NoError(t, err)
	require.NoError(t, scan.RemoteErr)
	assert.Equal(t, c3, scan.RemoteTip)
	assert.False(t, scan.RemoteAhead)
	require.Len(t, scan.Local, 1)
	assert.Empty(t, scan.Remote, "the remote holds the same blob; it is not reported twice")

	res, err := ShrinkOversizedCheckpointMetadata(ctx, repo, scan, io.Discard)
	require.NoError(t, err)
	assert.True(t, res.Pushed)
	assert.Equal(t, res.NewLocalTip.String(), strings.TrimSpace(testutil.RunGit(t, bare, "rev-parse", v1)),
		"the remote branch now points at the rewritten history")
	fixed := res.NewLocalTip

	// Simulate an earlier repair whose push never landed: the remote and the
	// tracking ref still sit on the bloated history while local is clean.
	testutil.RunGit(t, bare, "update-ref", v1, c3.String())
	testutil.RunGit(t, dir, "update-ref", "refs/remotes/origin/"+paths.MetadataBranchName, c3.String())

	scan2, err := ScanOversizedCheckpointMetadata(ctx, repo, "origin", shrinkTestThreshold)
	require.NoError(t, err)
	assert.Empty(t, scan2.Local, "local is already clean")
	require.Len(t, scan2.Remote, 1, "the bloat that only the remote still carries is reported")
	assert.True(t, scan2.RemoteAhead, "the remote history is not an ancestor of the rewritten local one")

	res2, err := ShrinkOversizedCheckpointMetadata(ctx, repo, scan2, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, fixed, res2.NewLocalTip, "rewriting the remote history reproduces the same fixed commits, so nothing is replayed twice")
	assert.True(t, res2.Pushed)
	assert.Equal(t, fixed.String(), strings.TrimSpace(testutil.RunGit(t, bare, "rev-parse", v1)))
}

func TestStripPromptAttributions(t *testing.T) {
	t.Parallel()
	withField := []byte(`{"session_id":"s","prompt_attributions":[{"x":1}],"attribution":{"agent_lines":1}}`)
	out, changed, err := stripPromptAttributions(withField)
	require.NoError(t, err)
	assert.True(t, changed)
	var doc map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &doc))
	assert.Equal(t, []string{"attribution", "session_id"}, slices.Sorted(maps.Keys(doc)))
	assert.JSONEq(t, `{"agent_lines":1}`, string(doc["attribution"]))

	without := []byte(`{"session_id":"s"}`)
	same, changed, err := stripPromptAttributions(without)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, without, same)

	_, _, err = stripPromptAttributions([]byte("not json"))
	require.Error(t, err)
}

// commitMessages returns the messages reachable from tip, newest first.
func commitMessages(t *testing.T, repo *git.Repository, tip plumbing.Hash) []string {
	t.Helper()
	iter, err := repo.Log(&git.LogOptions{From: tip})
	require.NoError(t, err)
	defer iter.Close()
	var msgs []string
	require.NoError(t, iter.ForEach(func(c *object.Commit) error {
		msgs = append(msgs, c.Message)
		return nil
	}))
	return msgs
}

// pushV1ToBare wires dir to a fresh bare remote named origin holding tip on
// the checkpoint branch, and returns the bare path.
func pushV1ToBare(t *testing.T, dir string, tip plumbing.Hash) string {
	t.Helper()
	bare := t.TempDir()
	testutil.RunGit(t, dir, "init", "--bare", bare)
	testutil.RunGit(t, dir, "remote", "add", "origin", bare)
	testutil.RunGit(t, dir, "push", "origin", tip.String()+":refs/heads/"+paths.MetadataBranchName)
	return bare
}

func bareV1(t *testing.T, bare string) string {
	t.Helper()
	return strings.TrimSpace(testutil.RunGit(t, bare, "rev-parse", "refs/heads/"+paths.MetadataBranchName))
}

// The remote is ahead by a checkpoint someone else pushed, and both sides
// share the oversized blob. The reconciled history must contain each
// checkpoint exactly once: reconciling the un-rewritten local branch against
// the rewritten remote would replay every commit since the bloat as a
// duplicate.
func TestShrinkOversizedCheckpointMetadata_RemoteAheadByNewCheckpoint_NoDuplicates(t *testing.T) {
	// Not parallel: remote.Fetch/Push and settings.Load use the working directory.
	dir, repo, c1, _, c3 := bloatedV1Fixture(t)
	t.Chdir(dir)
	ctx := context.Background()

	// Someone else's checkpoint on top of c3, present only on the remote.
	files := map[string][]byte{
		paths.MetadataFileName:                        []byte(`{"checkpoints":1}` + "\n"),
		"aa/bbbbbbbbbb/0/" + paths.MetadataFileName:   sessionMetadataJSON(t, "s1", 0),
		"aa/bbbbbbbbbb/0/" + paths.TranscriptFileName: []byte(`{"role":"user"}` + "\n"),
		bloatedMetadataPath:                           sessionMetadataJSON(t, "s2", 200),
		bloatedTranscript:                             []byte(`{"role":"user","content":"big session"}` + "\n"),
		"ee/ffffffffff/0/" + paths.MetadataFileName:   sessionMetadataJSON(t, "s3", 0),
		"ee/ffffffffff/0/" + paths.TranscriptFileName: []byte(`{"role":"user"}` + "\n"),
		"11/2222222222/0/" + paths.MetadataFileName:   sessionMetadataJSON(t, "s4", 0),
		"11/2222222222/0/" + paths.TranscriptFileName: []byte(`{"role":"user"}` + "\n"),
	}
	c4 := testutil.CommitFiles(t, repo, []plumbing.Hash{c3}, files, "Checkpoint: 112222222222")
	bare := pushV1ToBare(t, dir, c4)
	require.Equal(t, c3, readTip(t, repo), "local stays at c3")

	scan, err := ScanOversizedCheckpointMetadata(ctx, repo, "origin", shrinkTestThreshold)
	require.NoError(t, err)
	assert.Equal(t, c4, scan.RemoteTip)
	assert.True(t, scan.RemoteAhead)
	require.Len(t, scan.Local, 1)
	assert.Empty(t, scan.Remote, "the shared blob is reported once, on the local side")

	res, err := ShrinkOversizedCheckpointMetadata(ctx, repo, scan, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, 2, res.CommitsRewritten, "c2 and c3 locally")
	assert.Equal(t, 1, res.RemoteCommitsRewritten, "only c4 is new on the remote side; c2 and c3 were already rewritten")
	assert.True(t, res.Pushed)

	assert.Equal(t, []string{
		"Checkpoint: 112222222222",
		"Checkpoint: eeffffffffff",
		"Checkpoint: ccdddddddddd",
		"Checkpoint: aabbbbbbbbbb",
	}, commitMessages(t, repo, res.NewLocalTip), "every checkpoint exactly once, in order")
	assert.Equal(t, c1, commitAt(t, repo, res.NewLocalTip, 3).Hash, "the pre-bloat commit keeps its hash")
	assert.Equal(t, res.NewLocalTip.String(), bareV1(t, bare))

	none, err := findOversizedMetadataBlobs(ctx, repo, res.NewLocalTip, shrinkTestThreshold)
	require.NoError(t, err)
	assert.Empty(t, none)
}

// A second clone repaired the same history independently. With commit signing
// on, its rewritten commits carry different hashes but the same trees; the
// repair must recognise the content as already present and adopt it rather
// than replaying no-op duplicates.
func TestShrinkOversizedCheckpointMetadata_IndependentRewriteOfSameContentIsAdopted(t *testing.T) {
	// Not parallel: remote.Fetch/Push and settings.Load use the working directory.
	dir, repo, c1, _, c3 := bloatedV1Fixture(t)
	t.Chdir(dir)
	ctx := context.Background()
	bare := pushV1ToBare(t, dir, c3)

	// What this run's rewrite of the remote will produce, to borrow its trees.
	fixedTip, err := newMetadataRewriter(ctx, repo, shrinkTestThreshold).rewriteHistory(c3)
	require.NoError(t, err)
	fixedC3 := readCommit(t, repo, fixedTip)
	fixedC2 := readCommit(t, repo, fixedC3.ParentHashes[0])

	// The "other clone's" repair: same trees, different committer identity, so
	// different hashes — the shape commit signing produces.
	x2 := makeOrphanCommit(t, repo, fixedC2.TreeHash, []plumbing.Hash{c1}, "Checkpoint: ccdddddddddd")
	x3 := makeOrphanCommit(t, repo, fixedC3.TreeHash, []plumbing.Hash{x2}, "Checkpoint: eeffffffffff")
	require.NotEqual(t, fixedTip, x3)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(
		plumbing.NewBranchReferenceName(paths.MetadataBranchName), x3)))

	scan, err := ScanOversizedCheckpointMetadata(ctx, repo, "origin", shrinkTestThreshold)
	require.NoError(t, err)
	assert.Empty(t, scan.Local, "the local rewrite is already clean")
	require.Len(t, scan.Remote, 1)
	assert.True(t, scan.RemoteAhead, "hashes differ, so by hash the remote looks ahead")

	res, err := ShrinkOversizedCheckpointMetadata(ctx, repo, scan, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, 0, res.CommitsRewritten)
	assert.Equal(t, 2, res.RemoteCommitsRewritten)
	assert.Equal(t, fixedTip, res.NewLocalTip, "the remote's rewrite is adopted wholesale")
	assert.Len(t, commitMessages(t, repo, res.NewLocalTip), 3, "no duplicates")
	assert.True(t, res.Pushed)
	assert.Equal(t, fixedTip.String(), bareV1(t, bare))
}

// go-git's filemode.Deprecated (0100664) is a regular file; a metadata.json
// carrying it must be found and rewritten like any other.
func TestOversizedMetadata_DeprecatedFileModeIsHandled(t *testing.T) {
	t.Parallel()
	_, repo, _ := setupV1RepoInDir(t)
	ctx := context.Background()

	big, err := checkpoint.CreateBlobFromContent(repo, sessionMetadataJSON(t, "s9", 200))
	require.NoError(t, err)
	leaf := encodeTree(t, repo, []object.TreeEntry{{Name: paths.MetadataFileName, Mode: filemode.Deprecated, Hash: big}})
	mid := encodeTree(t, repo, []object.TreeEntry{{Name: "0", Mode: filemode.Dir, Hash: leaf}})
	shard := encodeTree(t, repo, []object.TreeEntry{{Name: "9999999999", Mode: filemode.Dir, Hash: mid}})
	root := encodeTree(t, repo, []object.TreeEntry{{Name: "99", Mode: filemode.Dir, Hash: shard}})
	tip := makeOrphanCommit(t, repo, root, nil, "Checkpoint: 999999999999")
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(
		plumbing.NewBranchReferenceName(paths.MetadataBranchName), tip)))

	found, err := findOversizedMetadataBlobs(ctx, repo, tip, shrinkTestThreshold)
	require.NoError(t, err)
	require.Len(t, found, 1)

	scan, err := ScanOversizedCheckpointMetadata(ctx, repo, "", shrinkTestThreshold)
	require.NoError(t, err)
	res, err := ShrinkOversizedCheckpointMetadata(ctx, repo, scan, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, 1, res.BlobsShrunk)
	none, err := findOversizedMetadataBlobs(ctx, repo, res.NewLocalTip, shrinkTestThreshold)
	require.NoError(t, err)
	assert.Empty(t, none)
}

func encodeTree(t *testing.T, repo *git.Repository, entries []object.TreeEntry) plumbing.Hash {
	t.Helper()
	obj := repo.Storer.NewEncodedObject()
	require.NoError(t, (&object.Tree{Entries: entries}).Encode(obj))
	hash, err := repo.Storer.SetEncodedObject(obj)
	require.NoError(t, err)
	return hash
}

func readTip(t *testing.T, repo *git.Repository) plumbing.Hash {
	t.Helper()
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)
	return ref.Hash()
}

func readCommit(t *testing.T, repo *git.Repository, h plumbing.Hash) *object.Commit {
	t.Helper()
	c, err := repo.CommitObject(h)
	require.NoError(t, err)
	return c
}

// commitAt walks n first-parents back from tip.
func commitAt(t *testing.T, repo *git.Repository, tip plumbing.Hash, n int) *object.Commit {
	t.Helper()
	c := readCommit(t, repo, tip)
	for range n {
		require.NotEmpty(t, c.ParentHashes)
		c = readCommit(t, repo, c.ParentHashes[0])
	}
	return c
}
