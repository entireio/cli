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

func TestStore_TaskRecordsRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := New(t.TempDir())
	cid := id.MustCheckpointID("a1b2c3d4e5f7")
	started := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

	require.NoError(t, store.Write(ctx, cp.Session{
		CheckpointID: cid,
		SessionID:    "sess-tasks",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("parent")),
		Tasks: []cp.TaskPayload{
			{ToolUseID: "toolu_b", AgentID: "agentb", StartedAt: started.Add(time.Minute), Transcript: redact.AlreadyRedacted([]byte("child-b"))},
			{ToolUseID: "toolu_a", AgentID: "agenta", StartedAt: started, TranscriptUnavailableReason: "transcript empty"},
		},
	}))

	entries, err := store.ListTasks(ctx, cid)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, "toolu_a", entries[0].ToolUseID)
	assert.False(t, entries[0].TranscriptStored)
	assert.Equal(t, "toolu_b", entries[1].ToolUseID)
	assert.True(t, entries[1].TranscriptStored)
	assert.Equal(t, "agentb", entries[1].Record.AgentID)

	transcript, err := store.ReadTaskTranscript(ctx, cid, "toolu_b")
	require.NoError(t, err)
	assert.Equal(t, "child-b", string(transcript))

	_, err = store.ReadTaskTranscript(ctx, cid, "toolu_a")
	require.ErrorIs(t, err, cp.ErrNoTranscript)
	_, err = store.ReadTaskTranscript(ctx, cid, "toolu_c")
	require.ErrorIs(t, err, cp.ErrTaskNotFound)
	_, err = store.ListTasks(ctx, id.MustCheckpointID("ffffffffffff"))
	require.ErrorIs(t, err, cp.ErrCheckpointNotFound)
}
