package checkpoint

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func TestV2ReadCommitted_ReturnsCheckpointSummary(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo)
	cpID := id.MustCheckpointID("a1a2a3a4a5a6")
	ctx := context.Background()

	writeV2TestCheckpoint(t, repo, v2TestCheckpointOptions{
		CheckpointID: cpID,
		SessionID:    "session-1",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"test": true}`)),
		Prompts:      []string{"hello"},
	})

	summary, err := store.ReadCommitted(ctx, cpID)
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Equal(t, cpID, summary.CheckpointID)
	assert.Len(t, summary.Sessions, 1)
}

func TestV2ReadCommitted_ReturnsNilForMissing(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo)
	cpID := id.MustCheckpointID("b1b2b3b4b5b6")
	ctx := context.Background()

	summary, err := store.ReadCommitted(ctx, cpID)
	require.NoError(t, err)
	assert.Nil(t, summary)
}

func TestV2ReadSessionContent_ReturnsMetadataAndTranscript(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo)
	cpID := id.MustCheckpointID("c1c2c3c4c5c6")
	ctx := context.Background()

	writeV2TestCheckpoint(t, repo, v2TestCheckpointOptions{
		CheckpointID: cpID,
		SessionID:    "session-1",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"message": "hello world"}`)),
		Prompts:      []string{"test prompt"},
	})

	content, err := store.ReadSessionContent(ctx, cpID, 0)
	require.NoError(t, err)
	require.NotNil(t, content)
	assert.Equal(t, "session-1", content.Metadata.SessionID)
	assert.NotEmpty(t, content.Transcript)
	assert.Contains(t, content.Prompts, "test prompt")
}

func TestV2ReadSessionContent_TranscriptFromArchivedGeneration(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo)
	ctx := context.Background()

	cpID := id.MustCheckpointID("d1d2d3d4d5d6")
	writeV2TestCheckpoint(t, repo, v2TestCheckpointOptions{
		CheckpointID: cpID,
		SessionID:    "session-archived",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"archived": true}`)),
	})

	archiveV2FullCurrentRef(t, repo, "0000000000001")
	resetV2FullCurrentRef(ctx, t, repo)

	content, err := store.ReadSessionContent(ctx, cpID, 0)
	require.NoError(t, err)
	require.NotNil(t, content)
	assert.Contains(t, string(content.Transcript), `"archived": true`)
}

func TestV2ReadSessionContent_FetchesRemoteArchivedGeneration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	remoteRepo := initTestRepo(t)
	remoteRoot := repoRootForTest(t, remoteRepo)
	cpID := id.MustCheckpointID("d2d3d4d5d6d7")
	writeV2TestCheckpoint(t, remoteRepo, v2TestCheckpointOptions{
		CheckpointID: cpID,
		SessionID:    "session-remote-archive",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"remoteArchived": true}` + "\n")),
	})
	archiveRefName := archiveV2FullCurrentRef(t, remoteRepo, "0000000000002")
	resetV2FullCurrentRef(ctx, t, remoteRepo)

	localRepo := initTestRepo(t)
	localRoot := repoRootForTest(t, localRepo)
	addOriginRemote(t, localRoot, remoteRoot)
	fetchRef(t, localRoot, paths.V2MainRefName)

	reopenedLocalRepo, err := git.PlainOpen(localRoot)
	require.NoError(t, err)
	localStore := NewV2GitStore(reopenedLocalRepo)

	content, err := localStore.ReadSessionContent(ctx, cpID, 0)
	require.NoError(t, err)
	require.NotNil(t, content)
	assert.Contains(t, string(content.Transcript), `"remoteArchived": true`)

	_, err = reopenedLocalRepo.Reference(archiveRefName, true)
	require.NoError(t, err, "remote archived full ref should be fetched locally")
}

func TestV2ReadSessionContent_MissingTranscript_ReturnsError(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo)
	cpID := id.MustCheckpointID("f1f2f3f4f5f6")
	ctx := context.Background()

	writeV2TestCheckpoint(t, repo, v2TestCheckpointOptions{
		CheckpointID: cpID,
		SessionID:    "session-1",
		Strategy:     "manual-commit",
		Prompts:      []string{"prompt"},
	})

	_, err := store.ReadSessionContent(ctx, cpID, 0)
	require.ErrorIs(t, err, ErrNoTranscript)
}

func TestV2FetchRemoteFullRefsUsesStoreRepository(t *testing.T) {
	// Cannot use t.Parallel because this test changes cwd to verify the store
	// repository controls remote resolution.
	ctx := context.Background()

	remoteRepo := initTestRepo(t)
	remoteRoot := repoRootForTest(t, remoteRepo)
	cpID := id.MustCheckpointID("a7a8a9aaabac")
	writeV2TestCheckpoint(t, remoteRepo, v2TestCheckpointOptions{
		CheckpointID: cpID,
		SessionID:    "session-remote",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"remote":true}` + "\n")),
	})

	localRepo := initTestRepo(t)
	localRoot := repoRootForTest(t, localRepo)
	addOriginRemote(t, localRoot, remoteRoot)

	cwdRemoteRepo := initTestRepo(t)
	cwdRemoteRoot := repoRootForTest(t, cwdRemoteRepo)
	cwdRepo := initTestRepo(t)
	cwdRoot := repoRootForTest(t, cwdRepo)
	addOriginRemote(t, cwdRoot, cwdRemoteRoot)
	t.Chdir(cwdRoot)

	localStore := NewV2GitStore(localRepo)
	require.NoError(t, localStore.fetchRemoteFullRefs(ctx))

	reopenedLocalRepo, err := git.PlainOpen(localRoot)
	require.NoError(t, err)
	_, err = reopenedLocalRepo.Reference(plumbing.ReferenceName(paths.V2FullCurrentRefName), true)
	require.NoError(t, err)

	reopenedCWDRepo, err := git.PlainOpen(cwdRoot)
	require.NoError(t, err)
	_, err = reopenedCWDRepo.Reference(plumbing.ReferenceName(paths.V2FullCurrentRefName), true)
	require.Error(t, err)
}

func archiveV2FullCurrentRef(t *testing.T, repo *git.Repository, generation string) plumbing.ReferenceName {
	t.Helper()

	fullRefName := plumbing.ReferenceName(paths.V2FullCurrentRefName)
	fullRef, err := repo.Reference(fullRefName, true)
	require.NoError(t, err)

	archiveRefName := plumbing.ReferenceName(v2FullRefPrefix() + generation)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(archiveRefName, fullRef.Hash())))
	return archiveRefName
}

func resetV2FullCurrentRef(ctx context.Context, t *testing.T, repo *git.Repository) {
	t.Helper()

	emptyTreeHash, err := BuildTreeFromEntries(ctx, repo, map[string]object.TreeEntry{})
	require.NoError(t, err)
	authorName, authorEmail := GetGitAuthorFromRepo(repo)
	emptyCommitHash, err := CreateCommit(ctx, repo, emptyTreeHash, plumbing.ZeroHash, "Reset v2 full current", authorName, authorEmail)
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(plumbing.ReferenceName(paths.V2FullCurrentRefName), emptyCommitHash)))
}

func TestV2ReadSessionMetadataAndPrompts_ReturnsWithoutTranscript(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo)
	cpID := id.MustCheckpointID("f1f2f3f4f5f7")
	ctx := context.Background()

	// Write a checkpoint with prompts but no transcript (the fixture skips
	// /full/current when Transcript is empty).
	writeV2TestCheckpoint(t, repo, v2TestCheckpointOptions{
		CheckpointID: cpID,
		SessionID:    "session-meta-only",
		Strategy:     "manual-commit",
		Prompts:      []string{"test prompt"},
	})

	// ReadSessionContent should fail (no transcript).
	_, err := store.ReadSessionContent(ctx, cpID, 0)
	require.ErrorIs(t, err, ErrNoTranscript)

	// ReadSessionMetadataAndPrompts should succeed.
	content, err := store.ReadSessionMetadataAndPrompts(ctx, cpID, 0)
	require.NoError(t, err)
	require.NotNil(t, content)
	assert.Equal(t, "session-meta-only", content.Metadata.SessionID)
	assert.Contains(t, content.Prompts, "test prompt")
	assert.Empty(t, content.Transcript)
}

func repoRootForTest(t *testing.T, repo *git.Repository) string {
	t.Helper()

	worktree, err := repo.Worktree()
	require.NoError(t, err)
	return worktree.Filesystem().Root()
}

func addOriginRemote(t *testing.T, repoRoot, remoteRoot string) {
	t.Helper()

	cmd := exec.CommandContext(context.Background(), "git", "remote", "add", "origin", remoteRoot)
	cmd.Dir = repoRoot
	cmd.Env = testutil.GitIsolatedEnv()
	output, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git remote add origin output:\n%s", output)
}

func fetchRef(t *testing.T, repoRoot, refName string) {
	t.Helper()

	cmd := exec.CommandContext(context.Background(), "git", "fetch", "origin", "+"+refName+":"+refName)
	cmd.Dir = repoRoot
	cmd.Env = testutil.GitIsolatedEnv()
	output, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git fetch %s output:\n%s", refName, output)
}

func TestV2ReadSessionMetadata_DoesNotRequireRawTranscript(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo)
	cpID := id.MustCheckpointID("f2f3f4f5f6f7")
	ctx := context.Background()

	writeV2TestCheckpoint(t, repo, v2TestCheckpointOptions{
		CheckpointID: cpID,
		SessionID:    "session-1",
		Strategy:     "manual-commit",
		Prompts:      []string{"prompt"},
		Agent:        "Claude Code",
	})

	metadata, err := store.ReadSessionMetadata(ctx, cpID, 0)
	require.NoError(t, err)
	require.NotNil(t, metadata)
	assert.Equal(t, "session-1", metadata.SessionID)
	assert.Equal(t, "Claude Code", string(metadata.Agent))
}

func TestV2ReadSessionMetadata_ContextCancellation(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo)
	cpID := id.MustCheckpointID("f2f3f4f5f6f8")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := store.ReadSessionMetadata(ctx, cpID, 0)
	require.ErrorIs(t, err, context.Canceled, "ReadSessionMetadata error = %v, want context.Canceled", err)

	_, err = store.ReadSessionMetadataAndPrompts(ctx, cpID, 0)
	require.ErrorIs(t, err, context.Canceled, "ReadSessionMetadataAndPrompts error = %v, want context.Canceled", err)
}

func TestV2ReadSessionMetadata_ReturnsMetadata(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo)
	cpID := id.MustCheckpointID("f1f2f3f4f5fa")
	ctx := context.Background()

	writeV2TestCheckpoint(t, repo, v2TestCheckpointOptions{
		CheckpointID: cpID,
		SessionID:    "session-metadata-only",
		Strategy:     "manual-commit",
		Prompts:      []string{"test prompt"},
	})

	meta, err := store.ReadSessionMetadata(ctx, cpID, 0)
	require.NoError(t, err)
	assert.Equal(t, "session-metadata-only", meta.SessionID)
}

func TestV2ReadSessionMetadata_FetchesMissingMetadataBlob(t *testing.T) {
	repo := initTestRepo(t)
	cpID := id.MustCheckpointID("f1f2f3f4f5fb")
	ctx := context.Background()

	writeV2TestCheckpoint(t, repo, v2TestCheckpointOptions{
		CheckpointID: cpID,
		SessionID:    "session-fetch-metadata",
		Strategy:     "manual-commit",
		Prompts:      []string{"test prompt"},
	})

	wt, err := repo.Worktree()
	require.NoError(t, err)
	repoRoot := wt.Filesystem().Root()
	t.Chdir(repoRoot)

	mainTree := v2MainTree(t, repo)
	sessionTree, err := mainTree.Tree(cpID.Path() + "/0")
	require.NoError(t, err)
	metadataEntry, err := sessionTree.FindEntry(paths.MetadataFileName)
	require.NoError(t, err)
	metadataContent := v2ReadFile(t, mainTree, cpID.Path()+"/0/"+paths.MetadataFileName)

	metadataObjectPath := filepath.Join(repoRoot, ".git", "objects", metadataEntry.Hash.String()[:2], metadataEntry.Hash.String()[2:])
	require.NoError(t, os.Remove(metadataObjectPath))

	reopenedRepo, err := git.PlainOpen(repoRoot)
	require.NoError(t, err)
	reopenedStore := NewV2GitStore(reopenedRepo)
	fetchCalled := false
	reopenedStore.SetBlobFetcher(func(_ context.Context, hashes []plumbing.Hash) error {
		fetchCalled = true
		require.Equal(t, []plumbing.Hash{metadataEntry.Hash}, hashes)
		_, createErr := CreateBlobFromContent(reopenedRepo, []byte(metadataContent))
		return createErr
	})

	meta, err := reopenedStore.ReadSessionMetadata(ctx, cpID, 0)
	require.NoError(t, err)
	assert.True(t, fetchCalled)
	assert.Equal(t, "session-fetch-metadata", meta.SessionID)
}

func TestV2ReadSessionMetadataAndPrompts_MissingCheckpoint(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo)
	cpID := id.MustCheckpointID("f1f2f3f4f5f8")
	ctx := context.Background()

	_, err := store.ReadSessionMetadataAndPrompts(ctx, cpID, 0)
	require.ErrorIs(t, err, ErrCheckpointNotFound)
}

func TestV2ReadSessionContent_ChunkedTranscript(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	cpID := id.MustCheckpointID("a0a1a2a3a4a5")
	ctx := context.Background()

	// Write metadata to /main so ReadSessionContent can find the checkpoint
	v2Store := NewV2GitStore(repo)
	writeV2TestCheckpoint(t, repo, v2TestCheckpointOptions{
		CheckpointID: cpID,
		SessionID:    "session-chunked",
		Strategy:     "manual-commit",
	})

	// Manually write chunked transcript to /full/current:
	// chunk 0 = raw_transcript (base file), chunk 1 = raw_transcript.001
	chunk0 := []byte(`{"line":"one"}` + "\n" + `{"line":"two"}`)
	chunk1 := []byte(`{"line":"three"}` + "\n" + `{"line":"four"}`)

	sessionPath := cpID.Path() + "/0/"
	writeV2TestFullSessionFiles(t, repo, map[string][]byte{
		sessionPath + paths.V2RawTranscriptFileName:          chunk0,
		sessionPath + paths.V2RawTranscriptFileName + ".001": chunk1,
	})

	// Read it back — should reassemble both chunks
	content, err := v2Store.ReadSessionContent(ctx, cpID, 0)
	require.NoError(t, err)
	require.NotNil(t, content)

	transcript := string(content.Transcript)
	assert.Contains(t, transcript, `{"line":"one"}`)
	assert.Contains(t, transcript, `{"line":"two"}`)
	assert.Contains(t, transcript, `{"line":"three"}`)
	assert.Contains(t, transcript, `{"line":"four"}`)
}

func TestV2ReadSessionCompactTranscript_ReturnsCompactData(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo)
	cpID := id.MustCheckpointID("b0b1b2b3b4b5")
	ctx := context.Background()

	compact := []byte(`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"user","content":[{"text":"hello compact"}]}` + "\n")
	writeV2TestCheckpoint(t, repo, v2TestCheckpointOptions{
		CheckpointID:      cpID,
		SessionID:         "session-compact",
		Strategy:          "manual-commit",
		Transcript:        redact.AlreadyRedacted([]byte(`{"raw":true}` + "\n")),
		CompactTranscript: compact,
	})

	content, err := store.ReadSessionCompactTranscript(ctx, cpID, 0)
	require.NoError(t, err)
	require.Equal(t, compact, content)
}

func TestV2ReadSessionCompactTranscript_MissingCompactTranscript(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo)
	cpID := id.MustCheckpointID("c0c1c2c3c4c5")
	ctx := context.Background()

	writeV2TestCheckpoint(t, repo, v2TestCheckpointOptions{
		CheckpointID: cpID,
		SessionID:    "session-no-compact",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"raw":true}` + "\n")),
	})

	_, err := store.ReadSessionCompactTranscript(ctx, cpID, 0)
	require.ErrorIs(t, err, ErrNoTranscript)
}

func TestV2ReadSessionCompactTranscript_MissingCheckpointOrSession(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	store := NewV2GitStore(repo)
	ctx := context.Background()

	_, err := store.ReadSessionCompactTranscript(ctx, id.MustCheckpointID("d0d1d2d3d4d5"), 0)
	require.ErrorIs(t, err, ErrCheckpointNotFound)

	cpID := id.MustCheckpointID("e0e1e2e3e4e5")
	writeV2TestCheckpoint(t, repo, v2TestCheckpointOptions{
		CheckpointID: cpID,
		SessionID:    "session-0",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"raw":true}` + "\n")),
	})

	_, err = store.ReadSessionCompactTranscript(ctx, cpID, 99)
	require.ErrorIs(t, err, ErrCheckpointNotFound)
}
