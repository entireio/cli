package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
)

// TestSeam_GitRefsPrimaryWithGitBranchMirror drives the branch->refs rollout
// topology through checkpoint.Open: a git-refs primary with a git-branch mirror.
// It writes all six WriteRequest variants and asserts reads resolve from the
// git-refs primary while the git-branch mirror (the v1 branch) independently
// received every write.
//
// Not parallel: uses t.Chdir so settings + ref resolution target the test repo.
func TestSeam_GitRefsPrimaryWithGitBranchMirror(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "README.md", "# test")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "init")

	body := `{"enabled": true, "checkpoints": {"primary": {"type": "git-refs"}, "mirrors": [{"type": "git-branch"}]}}`
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".entire", "settings.json"), []byte(body), 0o644))
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	stores, err := Open(context.Background(), repo, OpenOptions{})
	require.NoError(t, err)

	ctx := context.Background()
	cid := id.MustCheckpointID("a1b2c3d4e5f6")
	reservedCID := id.MustCheckpointID("01ARZ3NDEKTSV4RRFFQ69G5FAV")
	batchCID := id.MustCheckpointID("01ARZ3NDEKTSV4RRFFQ69G5FAW")
	const sessionID = "sess-1"

	require.NoError(t, stores.Persistent.Write(ctx, Session{
		CheckpointID: cid, SessionID: sessionID, Strategy: "manual-commit",
		Transcript: redact.AlreadyRedacted([]byte("initial transcript")),
		Prompts:    []string{"do the thing"}, FilesTouched: []string{"a.go"},
		AuthorName: "Test", AuthorEmail: "test@example.com",
	}))
	require.NoError(t, stores.Persistent.Write(ctx, ReservedSession{
		CheckpointID: reservedCID, SessionID: "reserved-session", Strategy: "manual-commit",
		Transcript: redact.AlreadyRedacted([]byte("reserved transcript")),
		AuthorName: "Test", AuthorEmail: "test@example.com",
	}))
	require.NoError(t, stores.Persistent.Write(ctx, BatchSessions{
		CheckpointID: batchCID,
		Sessions: []ReservedSession{
			{CheckpointID: batchCID, SessionID: "batch-b", Strategy: "manual-commit"},
			{CheckpointID: batchCID, SessionID: "batch-a", Strategy: "manual-commit"},
		},
		AuthorName: "Test", AuthorEmail: "test@example.com",
	}))
	require.NoError(t, stores.Persistent.Write(ctx, SessionTranscript{
		CheckpointID: cid, SessionID: sessionID,
		Transcript: redact.AlreadyRedacted([]byte("final transcript")),
		Prompts:    []string{"do the thing"},
	}))
	require.NoError(t, stores.Persistent.Write(ctx, SessionSummary{
		CheckpointID: cid, Summary: &Summary{Intent: "intent-x", Outcome: "outcome-y"},
	}))
	require.NoError(t, stores.Persistent.Write(ctx, CheckpointAttribution{
		CheckpointID: cid, Attribution: &Attribution{AgentLines: 7, AgentPercentage: 70},
	}))

	// Reads resolve from the git-refs primary.
	t.Run("git-refs primary", func(t *testing.T) {
		assertSeamVariants(t, stores.Persistent, cid, reservedCID, batchCID)
		// The primary is the per-checkpoint-ref store, not a fan-out of nothing.
		_, err := repo.Reference(mustRefName(t, cid), true)
		assert.NoError(t, err, "primary should have written the per-checkpoint ref")
	})

	// The git-branch mirror independently received every write on the v1 branch.
	t.Run("git-branch mirror", func(t *testing.T) {
		mirror := NewGitStore(repo, DefaultV1Refs())
		assertSeamVariants(t, mirror, cid, reservedCID, batchCID)
	})

	// Reads must be served by the git-refs primary, not the mirror: after the
	// mirror's v1 branch is deleted, the composed store still reads everything.
	t.Run("reads resolve from primary", func(t *testing.T) {
		require.NoError(t, repo.Storer.RemoveReference(v1BranchRef()))
		assertSeamVariants(t, stores.Persistent, cid, reservedCID, batchCID)
	})
}

func assertSeamVariants(t *testing.T, store PersistentStore, cid, reservedCID, batchCID id.CheckpointID) {
	t.Helper()
	ctx := context.Background()

	summary, err := store.Read(ctx, cid)
	require.NoError(t, err)
	require.NotNil(t, summary, "checkpoint should exist")
	require.Len(t, summary.Sessions, 1)
	require.NotNil(t, summary.CombinedAttribution)
	assert.Equal(t, 7, summary.CombinedAttribution.AgentLines)

	content, err := store.ReadSessionContent(ctx, cid, 0)
	require.NoError(t, err)
	assert.Equal(t, []byte("final transcript"), content.Transcript)

	meta, err := store.ReadSessionMetadata(ctx, cid, 0)
	require.NoError(t, err)
	require.NotNil(t, meta.Summary)
	assert.Equal(t, "intent-x", meta.Summary.Intent)

	reservedContent, err := store.ReadSessionContent(ctx, reservedCID, 0)
	require.NoError(t, err)
	assert.Equal(t, []byte("reserved transcript"), reservedContent.Transcript)

	batchSummary, err := store.Read(ctx, batchCID)
	require.NoError(t, err)
	require.Len(t, batchSummary.Sessions, 2)
	batchFirst, err := store.ReadSessionMetadata(ctx, batchCID, 0)
	require.NoError(t, err)
	assert.Equal(t, "batch-a", batchFirst.SessionID)
}
