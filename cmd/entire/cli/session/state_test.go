package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestState_NormalizeAfterLoad(t *testing.T) {
	t.Parallel()

	t.Run("migrates_CondensedTranscriptLines", func(t *testing.T) {
		t.Parallel()
		state := &State{
			CondensedTranscriptLines: 150,
		}
		state.NormalizeAfterLoad()
		assert.Equal(t, 150, state.CheckpointTranscriptStart)
		assert.Equal(t, 0, state.CondensedTranscriptLines)
		assert.Equal(t, 0, state.TranscriptLinesAtStart)
	})

	t.Run("no_migration_when_CheckpointTranscriptStart_set", func(t *testing.T) {
		t.Parallel()
		state := &State{
			CheckpointTranscriptStart: 200,
			CondensedTranscriptLines:  150, // old value should be cleared but not override new
		}
		state.NormalizeAfterLoad()
		assert.Equal(t, 200, state.CheckpointTranscriptStart)
		assert.Equal(t, 0, state.CondensedTranscriptLines)
	})

	t.Run("no_migration_when_all_zero", func(t *testing.T) {
		t.Parallel()
		state := &State{}
		state.NormalizeAfterLoad()
		assert.Equal(t, 0, state.CheckpointTranscriptStart)
	})

	t.Run("migrates_TranscriptLinesAtStart", func(t *testing.T) {
		t.Parallel()
		state := &State{
			TranscriptLinesAtStart: 42,
		}
		state.NormalizeAfterLoad()
		assert.Equal(t, 42, state.CheckpointTranscriptStart)
		assert.Equal(t, 0, state.TranscriptLinesAtStart)
	})

	t.Run("CondensedTranscriptLines_takes_precedence_over_TranscriptLinesAtStart", func(t *testing.T) {
		t.Parallel()
		state := &State{
			CondensedTranscriptLines: 150,
			TranscriptLinesAtStart:   42,
		}
		state.NormalizeAfterLoad()
		assert.Equal(t, 150, state.CheckpointTranscriptStart)
		assert.Equal(t, 0, state.CondensedTranscriptLines)
		assert.Equal(t, 0, state.TranscriptLinesAtStart)
	})

	t.Run("CheckpointTranscriptStart_not_overridden_by_TranscriptLinesAtStart", func(t *testing.T) {
		t.Parallel()
		state := &State{
			CheckpointTranscriptStart: 200,
			TranscriptLinesAtStart:    42,
		}
		state.NormalizeAfterLoad()
		assert.Equal(t, 200, state.CheckpointTranscriptStart)
		assert.Equal(t, 0, state.TranscriptLinesAtStart)
	})
}

func TestState_NormalizeAfterLoad_JSONRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		json     string
		wantCTS  int // CheckpointTranscriptStart
		wantStep int // StepCount
	}{
		{
			name:     "migrates old condensed_transcript_lines",
			json:     `{"session_id":"s1","condensed_transcript_lines":42,"checkpoint_count":5}`,
			wantCTS:  42,
			wantStep: 5,
		},
		{
			name:    "migrates old transcript_lines_at_start",
			json:    `{"session_id":"s1","transcript_lines_at_start":75}`,
			wantCTS: 75,
		},
		{
			name:    "preserves new field over old",
			json:    `{"session_id":"s1","condensed_transcript_lines":10,"checkpoint_transcript_start":50}`,
			wantCTS: 50,
		},
		{
			name:     "handles clean new format",
			json:     `{"session_id":"s1","checkpoint_transcript_start":25,"checkpoint_count":3}`,
			wantCTS:  25,
			wantStep: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var state State
			require.NoError(t, json.Unmarshal([]byte(tt.json), &state))
			state.NormalizeAfterLoad()

			assert.Equal(t, tt.wantCTS, state.CheckpointTranscriptStart)
			assert.Equal(t, tt.wantStep, state.StepCount)
			assert.Equal(t, 0, state.CondensedTranscriptLines, "deprecated field should be cleared")
			assert.Equal(t, 0, state.TranscriptLinesAtStart, "deprecated field should be cleared")
		})
	}
}

func TestState_IsStale(t *testing.T) {
	t.Parallel()

	t.Run("nil_EndedAt_is_not_stale", func(t *testing.T) {
		t.Parallel()
		state := &State{EndedAt: nil}
		assert.False(t, state.IsStale())
	})

	t.Run("recently_ended_is_not_stale", func(t *testing.T) {
		t.Parallel()
		recent := time.Now().Add(-1 * time.Hour)
		state := &State{EndedAt: &recent}
		assert.False(t, state.IsStale())
	})

	t.Run("ended_over_24h_ago_is_stale", func(t *testing.T) {
		t.Parallel()
		old := time.Now().Add(-25 * time.Hour)
		state := &State{EndedAt: &old}
		assert.True(t, state.IsStale())
	})

	t.Run("ended_just_under_threshold_is_not_stale", func(t *testing.T) {
		t.Parallel()
		// A session that ended just under the staleness threshold should not be stale.
		// Use StaleSessionThreshold rather than a magic number so the test stays in sync
		// if the threshold changes.
		recent := time.Now().Add(-1 * (StaleSessionThreshold - time.Hour))
		state := &State{EndedAt: &recent}
		assert.False(t, state.IsStale())
	})
}

func TestStateStore_List_DeletesStaleSession(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "entire-sessions")
	require.NoError(t, os.MkdirAll(stateDir, 0o750))
	store := NewStateStoreWithDir(stateDir)
	ctx := context.Background()

	// Create an active session (no EndedAt)
	active := &State{
		SessionID:  "active-session",
		BaseCommit: "abc123",
		StartedAt:  time.Now(),
	}
	require.NoError(t, store.Save(ctx, active))

	// Create a stale session (ended >24h ago)
	staleEnded := time.Now().Add(-48 * time.Hour)
	stale := &State{
		SessionID:  "stale-session",
		BaseCommit: "def456",
		StartedAt:  time.Now().Add(-72 * time.Hour),
		EndedAt:    &staleEnded,
	}
	require.NoError(t, store.Save(ctx, stale))

	// List should return only the active session
	states, err := store.List(ctx)
	require.NoError(t, err)
	require.Len(t, states, 1)
	assert.Equal(t, "active-session", states[0].SessionID)

	// Stale session file should be deleted from disk
	_, err = os.Stat(filepath.Join(stateDir, "stale-session.json"))
	assert.True(t, os.IsNotExist(err), "stale session file should be deleted")

	// Active session file should still exist
	_, err = os.Stat(filepath.Join(stateDir, "active-session.json"))
	assert.NoError(t, err, "active session file should still exist")
}
