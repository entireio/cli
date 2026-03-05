package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
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
		state.NormalizeAfterLoad(context.Background())
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
		state.NormalizeAfterLoad(context.Background())
		assert.Equal(t, 200, state.CheckpointTranscriptStart)
		assert.Equal(t, 0, state.CondensedTranscriptLines)
	})

	t.Run("no_migration_when_all_zero", func(t *testing.T) {
		t.Parallel()
		state := &State{}
		state.NormalizeAfterLoad(context.Background())
		assert.Equal(t, 0, state.CheckpointTranscriptStart)
	})

	t.Run("migrates_TranscriptLinesAtStart", func(t *testing.T) {
		t.Parallel()
		state := &State{
			TranscriptLinesAtStart: 42,
		}
		state.NormalizeAfterLoad(context.Background())
		assert.Equal(t, 42, state.CheckpointTranscriptStart)
		assert.Equal(t, 0, state.TranscriptLinesAtStart)
	})

	t.Run("CondensedTranscriptLines_takes_precedence_over_TranscriptLinesAtStart", func(t *testing.T) {
		t.Parallel()
		state := &State{
			CondensedTranscriptLines: 150,
			TranscriptLinesAtStart:   42,
		}
		state.NormalizeAfterLoad(context.Background())
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
		state.NormalizeAfterLoad(context.Background())
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
			state.NormalizeAfterLoad(context.Background())

			assert.Equal(t, tt.wantCTS, state.CheckpointTranscriptStart)
			assert.Equal(t, tt.wantStep, state.StepCount)
			assert.Equal(t, 0, state.CondensedTranscriptLines, "deprecated field should be cleared")
			assert.Equal(t, 0, state.TranscriptLinesAtStart, "deprecated field should be cleared")
		})
	}
}

func TestState_IsStale(t *testing.T) {
	t.Parallel()

	t.Run("nil_LastInteractionTime_falls_back_to_StartedAt", func(t *testing.T) {
		t.Parallel()
		// Started 48 days ago, no interaction time — should be stale
		state := &State{
			StartedAt:           time.Now().Add(-48 * 24 * time.Hour),
			LastInteractionTime: nil,
		}
		assert.True(t, state.IsStale())
	})

	t.Run("nil_LastInteractionTime_recent_start_is_not_stale", func(t *testing.T) {
		t.Parallel()
		// Started 1 hour ago, no interaction time — not stale
		state := &State{
			StartedAt:           time.Now().Add(-1 * time.Hour),
			LastInteractionTime: nil,
		}
		assert.False(t, state.IsStale())
	})

	t.Run("recently_interacted_is_not_stale", func(t *testing.T) {
		t.Parallel()
		recent := time.Now().Add(-1 * time.Hour)
		state := &State{LastInteractionTime: &recent}
		assert.False(t, state.IsStale())
	})

	t.Run("old_interaction_is_stale", func(t *testing.T) {
		t.Parallel()
		old := time.Now().Add(-14 * 24 * time.Hour)
		state := &State{LastInteractionTime: &old}
		assert.True(t, state.IsStale())
	})

	t.Run("just_under_threshold_is_not_stale", func(t *testing.T) {
		t.Parallel()
		recent := time.Now().Add(-1 * (StaleSessionThreshold - time.Hour))
		state := &State{LastInteractionTime: &recent}
		assert.False(t, state.IsStale())
	})

	t.Run("nil_LastInteractionTime_just_under_threshold_is_not_stale", func(t *testing.T) {
		t.Parallel()
		state := &State{
			StartedAt:           time.Now().Add(-1 * (StaleSessionThreshold - time.Hour)),
			LastInteractionTime: nil,
		}
		assert.False(t, state.IsStale())
	})
}

func TestStateStore_Load_DeletesStaleSession(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "entire-sessions")
	require.NoError(t, os.MkdirAll(stateDir, 0o750))
	store := NewStateStoreWithDir(stateDir)
	ctx := context.Background()

	// Create a stale session (ended >1wk ago)
	staleInteracted := time.Now().Add(-2 * 7 * 24 * time.Hour)
	stale := &State{
		SessionID:           "stale-session",
		BaseCommit:          "def456",
		StartedAt:           time.Now().Add(-3 * 7 * 24 * time.Hour),
		LastInteractionTime: &staleInteracted,
	}
	require.NoError(t, store.Save(ctx, stale))

	// Verify file exists before load
	stateFile := filepath.Join(stateDir, "stale-session.json")
	_, err := os.Stat(stateFile)
	require.NoError(t, err, "state file should exist before load")

	// Load should return (nil, nil) for stale session
	loaded, err := store.Load(ctx, "stale-session")
	require.NoError(t, err, "Load should not return error for stale session")
	assert.Nil(t, loaded, "Load should return nil for stale session")

	// File should be deleted from disk
	_, err = os.Stat(stateFile)
	assert.True(t, os.IsNotExist(err), "stale session file should be deleted after Load")

	// Create an active session (no LastInteractionTime) to verify non-stale sessions still work
	active := &State{
		SessionID:  "active-session",
		BaseCommit: "abc123",
		StartedAt:  time.Now(),
	}
	require.NoError(t, store.Save(ctx, active))

	loaded, err = store.Load(ctx, "active-session")
	require.NoError(t, err)
	assert.NotNil(t, loaded, "Load should return state for active session")
	assert.Equal(t, "active-session", loaded.SessionID)
}

func TestStateStore_Load_DeletesStaleSession_NilLastInteraction(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "entire-sessions")
	require.NoError(t, os.MkdirAll(stateDir, 0o750))
	store := NewStateStoreWithDir(stateDir)
	ctx := context.Background()

	// Exact production scenario: session created before interaction tracking,
	// so LastInteractionTime is nil and StartedAt is old.
	immortal := &State{
		SessionID:           "immortal-session",
		BaseCommit:          "abc123",
		StartedAt:           time.Now().Add(-48 * 24 * time.Hour),
		LastInteractionTime: nil,
	}
	require.NoError(t, store.Save(ctx, immortal))

	stateFile := filepath.Join(stateDir, "immortal-session.json")
	_, err := os.Stat(stateFile)
	require.NoError(t, err, "state file should exist before load")

	loaded, err := store.Load(ctx, "immortal-session")
	require.NoError(t, err)
	assert.Nil(t, loaded, "Load should return nil for session with nil LastInteractionTime and old StartedAt")

	_, err = os.Stat(stateFile)
	assert.True(t, os.IsNotExist(err), "immortal session file should be deleted after Load")
}

func TestStateStore_List_DeletesStaleSession(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "entire-sessions")
	require.NoError(t, os.MkdirAll(stateDir, 0o750))
	store := NewStateStoreWithDir(stateDir)
	ctx := context.Background()

	// Create an active session (no LastInteractionTime)
	active := &State{
		SessionID:  "active-session",
		BaseCommit: "abc123",
		StartedAt:  time.Now(),
	}
	require.NoError(t, store.Save(ctx, active))

	// Create a stale session (ended >2wk ago)
	staleInteracted := time.Now().Add(-2 * 7 * 24 * time.Hour)
	stale := &State{
		SessionID:           "stale-session",
		BaseCommit:          "def456",
		StartedAt:           time.Now().Add(-3 * 7 * 24 * time.Hour),
		LastInteractionTime: &staleInteracted,
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

// initTestRepo creates a temp dir with a git repo and chdirs into it.
// Cannot use t.Parallel() because of t.Chdir.
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// Resolve symlinks (macOS /var -> /private/var)
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	_, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	t.Chdir(dir)
	ClearGitCommonDirCache()
	return dir
}

func TestGetGitCommonDir_ReturnsValidPath(t *testing.T) {
	dir := initTestRepo(t)

	commonDir, err := getGitCommonDir(context.Background())
	require.NoError(t, err)

	// getGitCommonDir returns a relative path from cwd; resolve it to absolute for comparison
	absCommonDir, err := filepath.Abs(commonDir)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, ".git"), absCommonDir)

	// The path should actually exist
	info, err := os.Stat(commonDir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func TestGetGitCommonDir_CachesResult(t *testing.T) {
	initTestRepo(t)

	// First call populates cache
	first, err := getGitCommonDir(context.Background())
	require.NoError(t, err)

	// Second call should return the same result (from cache)
	second, err := getGitCommonDir(context.Background())
	require.NoError(t, err)

	assert.Equal(t, first, second)
}

func TestGetGitCommonDir_ClearCache(t *testing.T) {
	initTestRepo(t)

	// Populate cache
	_, err := getGitCommonDir(context.Background())
	require.NoError(t, err)

	// Verify cache is populated
	gitCommonDirMu.RLock()
	assert.NotEmpty(t, gitCommonDirCache)
	gitCommonDirMu.RUnlock()

	// Clear and verify
	ClearGitCommonDirCache()

	gitCommonDirMu.RLock()
	assert.Empty(t, gitCommonDirCache)
	assert.Empty(t, gitCommonDirCacheDir)
	gitCommonDirMu.RUnlock()
}

func TestGetGitCommonDir_InvalidatesOnCwdChange(t *testing.T) {
	// Create two separate repos
	dir1 := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir1); err == nil {
		dir1 = resolved
	}
	_, err := git.PlainInit(dir1, false)
	require.NoError(t, err)

	dir2 := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir2); err == nil {
		dir2 = resolved
	}
	_, err = git.PlainInit(dir2, false)
	require.NoError(t, err)

	ClearGitCommonDirCache()

	// Populate cache from dir1
	t.Chdir(dir1)
	first, err := getGitCommonDir(context.Background())
	require.NoError(t, err)
	absFirst, err := filepath.Abs(first)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir1, ".git"), absFirst)

	// Change to dir2 — cache should miss and resolve to dir2's .git
	t.Chdir(dir2)
	second, err := getGitCommonDir(context.Background())
	require.NoError(t, err)
	absSecond, err := filepath.Abs(second)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir2, ".git"), absSecond)

	assert.NotEqual(t, absFirst, absSecond)
}

func TestGetGitCommonDir_ErrorOutsideRepo(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ClearGitCommonDirCache()

	_, err := getGitCommonDir(context.Background())
	assert.Error(t, err)
}
