package fsstore

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cp "github.com/entireio/cli/api/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/redact"
)

func TestStore_WriteSessionRoundTrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := New(t.TempDir())
	cid := id.MustCheckpointID("a1b2c3d4e5f6")

	require.NoError(t, store.Write(ctx, cp.Session{
		CheckpointID:     cid,
		SessionID:        "sess-1",
		Strategy:         "manual-commit",
		Transcript:       redact.AlreadyRedacted([]byte("transcript-1")),
		Prompts:          []string{"hello"},
		FilesTouched:     []string{"a.go"},
		CheckpointsCount: 2,
	}))

	summary, err := store.Read(ctx, cid)
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Equal(t, cid, summary.CheckpointID)
	require.Len(t, summary.Sessions, 1)
	assert.Equal(t, 2, summary.CheckpointsCount)
	assert.Equal(t, []string{"a.go"}, summary.FilesTouched)

	content, err := store.ReadSessionContent(ctx, cid, 0)
	require.NoError(t, err)
	assert.Equal(t, []byte("transcript-1"), content.Transcript)
	assert.Contains(t, content.Prompts, "hello")
}

func TestStore_ReadUnknownCheckpoint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := New(t.TempDir())

	summary, err := store.Read(ctx, id.MustCheckpointID("ffffffffffff"))
	require.NoError(t, err)
	assert.Nil(t, summary, "absent checkpoint should read as nil summary")

	_, err = store.ReadSessionContent(ctx, id.MustCheckpointID("ffffffffffff"), 0)
	require.ErrorIs(t, err, cp.ErrCheckpointNotFound)
}

func TestStore_BackfillTranscriptReplacesWithoutClobbering(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := New(t.TempDir())
	cid := id.MustCheckpointID("a1b2c3d4e5f6")

	require.NoError(t, store.Write(ctx, cp.Session{
		CheckpointID: cid, SessionID: "sess-1", Strategy: "manual-commit",
		Transcript: redact.AlreadyRedacted([]byte("old")), FilesTouched: []string{"a.go"},
	}))
	require.NoError(t, store.Write(ctx, cp.SessionTranscript{
		CheckpointID: cid, SessionID: "sess-1",
		Transcript: redact.AlreadyRedacted([]byte("new")), Prompts: []string{"p"},
	}))

	content, err := store.ReadSessionContent(ctx, cid, 0)
	require.NoError(t, err)
	assert.Equal(t, []byte("new"), content.Transcript)
	// Sibling field (files touched, surfaced via the summary) must survive.
	summary, err := store.Read(ctx, cid)
	require.NoError(t, err)
	assert.Equal(t, []string{"a.go"}, summary.FilesTouched)
}

func TestStore_SessionSummaryAndAttribution(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := New(t.TempDir())
	cid := id.MustCheckpointID("a1b2c3d4e5f6")

	require.NoError(t, store.Write(ctx, cp.Session{
		CheckpointID: cid, SessionID: "sess-1", Strategy: "manual-commit",
		Transcript: redact.AlreadyRedacted([]byte("t")),
	}))
	require.NoError(t, store.Write(ctx, cp.SessionSummary{
		CheckpointID: cid, Summary: &cp.Summary{Intent: "do a thing", Outcome: "did it"},
	}))
	require.NoError(t, store.Write(ctx, cp.CheckpointAttribution{
		CheckpointID: cid, Attribution: &cp.Attribution{AgentLines: 10, AgentPercentage: 80},
	}))

	meta, err := store.ReadSessionMetadata(ctx, cid, 0)
	require.NoError(t, err)
	require.NotNil(t, meta.Summary)
	assert.Equal(t, "do a thing", meta.Summary.Intent)

	summary, err := store.Read(ctx, cid)
	require.NoError(t, err)
	require.NotNil(t, summary.CombinedAttribution)
	assert.Equal(t, 10, summary.CombinedAttribution.AgentLines)
}

func TestStore_ListReturnsCheckpoints(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := New(t.TempDir())

	require.NoError(t, store.Write(ctx, cp.Session{
		CheckpointID: id.MustCheckpointID("a1b2c3d4e5f6"), SessionID: "s1",
		CreatedAt: time.Unix(100, 0), Transcript: redact.AlreadyRedacted([]byte("t")),
	}))
	require.NoError(t, store.Write(ctx, cp.Session{
		CheckpointID: id.MustCheckpointID("b1b2c3d4e5f6"), SessionID: "s2",
		CreatedAt: time.Unix(200, 0), Transcript: redact.AlreadyRedacted([]byte("t")),
	}))

	infos, err := store.List(ctx)
	require.NoError(t, err)
	require.Len(t, infos, 2)
	// Sorted newest-first by CreatedAt.
	assert.Equal(t, id.MustCheckpointID("b1b2c3d4e5f6"), infos[0].CheckpointID)
}

func TestStore_DefaultsCreatedAtWhenZero(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := New(t.TempDir())
	cid := id.MustCheckpointID("a1b2c3d4e5f6")

	require.NoError(t, store.Write(ctx, cp.Session{
		CheckpointID: cid, SessionID: "s1", // CreatedAt left zero
		Transcript: redact.AlreadyRedacted([]byte("t")),
	}))

	meta, err := store.ReadSessionMetadata(ctx, cid, 0)
	require.NoError(t, err)
	assert.False(t, meta.CreatedAt.IsZero(), "zero CreatedAt should default to the current time")
}

func TestStore_PersistsReviewFlagAndCombinedAttribution(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := New(t.TempDir())
	cid := id.MustCheckpointID("a1b2c3d4e5f6")

	require.NoError(t, store.Write(ctx, cp.Session{
		CheckpointID: cid, SessionID: "s1", Transcript: redact.AlreadyRedacted([]byte("t")),
		HasReview:           true,
		CombinedAttribution: &cp.Attribution{AgentLines: 3},
	}))

	summary, err := store.Read(ctx, cid)
	require.NoError(t, err)
	assert.True(t, summary.HasReview)
	require.NotNil(t, summary.CombinedAttribution)
	assert.Equal(t, 3, summary.CombinedAttribution.AgentLines)
}

func TestStore_FactoryRequiresPath(t *testing.T) {
	t.Parallel()
	_, err := factory(context.Background(), checkpoint.OpenEnv{}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config.path is required")
}

// A CLI upgrade between two writes to the same checkpoint must not assert the
// delta scope over sessions whose token_usage predates it. token_usage_version
// is a summary-level flag for a per-session property, so it may only claim the
// delta scope once no legacy row is left.
func TestWriteSession_KeepsLegacyTokenUsageVersionWhenLegacyRowsRemain(t *testing.T) {
	t.Parallel()

	store := New(t.TempDir())
	checkpointID := id.MustCheckpointID("abc123abc123")

	require.NoError(t, store.Write(context.Background(), cp.Session{
		CheckpointID: checkpointID, SessionID: "legacy-session", Strategy: "manual-commit",
		AuthorName: "T", AuthorEmail: "t@example.com",
	}))

	// Simulate a checkpoint written before token_usage_version existed: the row
	// stays, the flag does not.
	sc, err := store.load(checkpointID)
	require.NoError(t, err)
	sc.Summary.TokenUsageVersion = 0
	require.NoError(t, store.save(sc))

	// A newer CLI attaches a second session (e.g. a review session).
	require.NoError(t, store.Write(context.Background(), cp.Session{
		CheckpointID: checkpointID, SessionID: "new-session", Strategy: "manual-commit",
		AuthorName: "T", AuthorEmail: "t@example.com",
	}))

	sc, err = store.load(checkpointID)
	require.NoError(t, err)
	require.Len(t, sc.Sessions, 2)
	assert.Equal(t, 0, sc.Summary.TokenUsageVersion,
		"the legacy row is still in the checkpoint, so readers must keep applying the legacy dedupe")
}

// The flag may move to the delta scope once the write leaves the checkpoint
// holding only the row it just wrote — including the ordinary new-checkpoint case.
func TestWriteSession_StampsDeltaVersionWhenNoLegacyRowRemains(t *testing.T) {
	t.Parallel()

	store := New(t.TempDir())
	checkpointID := id.MustCheckpointID("def456def456")

	require.NoError(t, store.Write(context.Background(), cp.Session{
		CheckpointID: checkpointID, SessionID: "only-session", Strategy: "manual-commit",
		AuthorName: "T", AuthorEmail: "t@example.com",
	}))

	sc, err := store.load(checkpointID)
	require.NoError(t, err)
	sc.Summary.TokenUsageVersion = 0
	require.NoError(t, store.save(sc))

	// Rewriting that same sole session replaces the legacy row outright.
	require.NoError(t, store.Write(context.Background(), cp.Session{
		CheckpointID: checkpointID, SessionID: "only-session", Strategy: "manual-commit",
		AuthorName: "T", AuthorEmail: "t@example.com",
	}))

	sc, err = store.load(checkpointID)
	require.NoError(t, err)
	require.Len(t, sc.Sessions, 1)
	assert.Equal(t, cp.TokenUsageVersionDelta, sc.Summary.TokenUsageVersion)
}
