package checkpoint

import (
	"context"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/redact"
)

// seedBranchCheckpoint writes one checkpoint to the git-branch v1 store,
// authored by Test <test@test.com>.
func seedBranchCheckpoint(t *testing.T, store *GitStore, cid id.CheckpointID, sessionID string) {
	t.Helper()
	require.NoError(t, store.Write(context.Background(), Session{
		CheckpointID: cid,
		SessionID:    sessionID,
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("transcript for " + sessionID + "\n")),
		Prompts:      []string{"do the thing"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))
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

// rootTreeEntryNames returns the top-level entry names of a checkpoint ref's tree.
func rootTreeEntryNames(t *testing.T, repo *git.Repository, cid id.CheckpointID) map[string]bool {
	t.Helper()
	commit, err := repo.CommitObject(refHash(t, repo, cid))
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)
	names := make(map[string]bool, len(tree.Entries))
	for _, e := range tree.Entries {
		names[e.Name] = true
	}
	return names
}

func TestRewriteBranchToRefs(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	ctx := context.Background()
	branch := NewGitStore(repo, DefaultV1Refs())
	cid := id.MustCheckpointID("a1b2c3d4e5f6")
	seedBranchCheckpoint(t, branch, cid, "s1") // authored by Test <test@test.com>

	result, err := RewriteBranchToRefs(ctx, repo, false, false)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Total)
	assert.Len(t, result.Rewritten, 1)
	assert.Equal(t, 0, result.Skipped)

	// Reads back through the git-refs store.
	refsStore := newGitRefsStore(repo)
	summary, err := refsStore.Read(ctx, cid)
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Equal(t, cid, summary.CheckpointID)
	require.Len(t, summary.Sessions, 1)

	// Root-flat: checkpoint files sit at the tree root (no <shard>/<id> nesting).
	names := rootTreeEntryNames(t, repo, cid)
	assert.True(t, names["metadata.json"], "checkpoint metadata should be at the ref tree root")
	assert.True(t, names["0"], "session 0 should be at the ref tree root")
	assert.False(t, names["a1"], "there must be no shard folder in the ref tree")

	// The commit keeps the original author (read from the branch commit).
	commit, err := repo.CommitObject(refHash(t, repo, cid))
	require.NoError(t, err)
	assert.Equal(t, "Test", commit.Author.Name)
	assert.Equal(t, "test@test.com", commit.Author.Email)
}

func TestRewriteBranchToRefs_IdempotentAndForce(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	ctx := context.Background()
	branch := NewGitStore(repo, DefaultV1Refs())
	cid := id.MustCheckpointID("a1b2c3d4e5f6")
	seedBranchCheckpoint(t, branch, cid, "s1")

	_, err := RewriteBranchToRefs(ctx, repo, false, false)
	require.NoError(t, err)
	before := refHash(t, repo, cid)

	// Re-run without force: the existing ref is skipped and left untouched.
	again, err := RewriteBranchToRefs(ctx, repo, false, false)
	require.NoError(t, err)
	assert.Empty(t, again.Rewritten, "existing ref should be skipped")
	assert.Equal(t, 1, again.Skipped)
	assert.Equal(t, before, refHash(t, repo, cid), "skipped run must not move the ref")

	// Re-run with force: the checkpoint is re-materialized and still reads back.
	forced, err := RewriteBranchToRefs(ctx, repo, false, true)
	require.NoError(t, err)
	assert.Len(t, forced.Rewritten, 1, "force re-materializes the checkpoint")
	refsStore := newGitRefsStore(repo)
	summary, err := refsStore.Read(ctx, cid)
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Equal(t, cid, summary.CheckpointID)
}

func TestRewriteBranchToRefs_MultipleSessions(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	ctx := context.Background()
	branch := NewGitStore(repo, DefaultV1Refs())
	cid := id.MustCheckpointID("a1b2c3d4e5f6")
	seedBranchCheckpoint(t, branch, cid, "sess-1")
	seedBranchCheckpoint(t, branch, cid, "sess-2")

	result, err := RewriteBranchToRefs(ctx, repo, false, false)
	require.NoError(t, err)
	assert.Len(t, result.Rewritten, 1)

	refsStore := newGitRefsStore(repo)
	summary, err := refsStore.Read(ctx, cid)
	require.NoError(t, err)
	require.Len(t, summary.Sessions, 2, "both sessions replayed into the ref checkpoint")

	names := rootTreeEntryNames(t, repo, cid)
	assert.True(t, names["0"] && names["1"], "both session dirs at root")
}

func TestRewriteBranchToRefs_DryRunAndNoBranch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// No v1 branch → no-op.
	emptyRepo, _ := setupBranchTestRepo(t)
	res, err := RewriteBranchToRefs(ctx, emptyRepo, false, false)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Total)

	// Dry run reports without writing.
	repo, _ := setupBranchTestRepo(t)
	branch := NewGitStore(repo, DefaultV1Refs())
	cid := id.MustCheckpointID("a1b2c3d4e5f6")
	seedBranchCheckpoint(t, branch, cid, "s1")

	dry, err := RewriteBranchToRefs(ctx, repo, true, false)
	require.NoError(t, err)
	assert.Len(t, dry.Rewritten, 1)
	refName, err := RefName(cid)
	require.NoError(t, err)
	_, err = repo.Reference(refName, true)
	assert.Error(t, err, "dry-run must not write a ref")
}
