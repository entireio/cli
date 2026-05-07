package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
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

	t.Run("leaves_CompactTranscriptStart_zero_when_missing", func(t *testing.T) {
		t.Parallel()
		state := &State{
			CheckpointTranscriptStart: 120,
		}
		state.NormalizeAfterLoad(context.Background())
		assert.Equal(t, 0, state.CompactTranscriptStart)
	})

	t.Run("preserves_existing_CompactTranscriptStart", func(t *testing.T) {
		t.Parallel()
		state := &State{
			CheckpointTranscriptStart: 120,
			CompactTranscriptStart:    45,
		}
		state.NormalizeAfterLoad(context.Background())
		assert.Equal(t, 45, state.CompactTranscriptStart)
	})

	t.Run("heals_stale_divergence_flag_when_attribution_aligned", func(t *testing.T) {
		t.Parallel()
		// DivergenceNoticeShown is only meaningful while attribution is diverged.
		// A state file carrying notice=true with base==attribution must self-heal on load —
		// otherwise a legitimate future divergence would be suppressed by the stale flag.
		state := &State{
			BaseCommit:            "aaaaaaa",
			AttributionBaseCommit: "aaaaaaa",
			DivergenceNoticeShown: true,
		}
		state.NormalizeAfterLoad(context.Background())
		assert.False(t, state.DivergenceNoticeShown,
			"DivergenceNoticeShown must be cleared when AttributionBaseCommit == BaseCommit")
	})

	t.Run("heals_stale_divergence_flag_when_attribution_empty", func(t *testing.T) {
		t.Parallel()
		// Empty AttributionBaseCommit gets backfilled to BaseCommit below; once aligned,
		// the flag is meaningless and must clear.
		state := &State{
			BaseCommit:            "bbbbbbb",
			AttributionBaseCommit: "",
			DivergenceNoticeShown: true,
		}
		state.NormalizeAfterLoad(context.Background())
		assert.False(t, state.DivergenceNoticeShown,
			"DivergenceNoticeShown must be cleared when AttributionBaseCommit is empty/backfilled")
	})

	t.Run("preserves_divergence_flag_when_actually_diverged", func(t *testing.T) {
		t.Parallel()
		state := &State{
			BaseCommit:            "cccccc1",
			AttributionBaseCommit: "cccccc0",
			DivergenceNoticeShown: true,
		}
		state.NormalizeAfterLoad(context.Background())
		assert.True(t, state.DivergenceNoticeShown,
			"DivergenceNoticeShown must be preserved when attribution is genuinely diverged")
	})
}

func TestState_RealignAttributionBase_ClearsDivergenceFlag(t *testing.T) {
	t.Parallel()

	state := &State{
		BaseCommit:            "ccccccc",
		AttributionBaseCommit: "aaaaaaa",
		DivergenceNoticeShown: true,
	}
	state.RealignAttributionBase("ccccccc")

	assert.Equal(t, "ccccccc", state.AttributionBaseCommit,
		"AttributionBaseCommit must be updated to the new base")
	assert.False(t, state.DivergenceNoticeShown,
		"DivergenceNoticeShown must be cleared whenever attribution is realigned")
}

func TestState_NormalizeAfterLoad_JSONRoundTrip(t *testing.T) {
	tests := []struct {
		name        string
		json        string
		wantCTS     int // CheckpointTranscriptStart
		wantCompact int // CompactTranscriptStart
		wantStep    int // StepCount
	}{
		{
			name:        "migrates old condensed_transcript_lines",
			json:        `{"session_id":"s1","condensed_transcript_lines":42,"checkpoint_count":5}`,
			wantCTS:     42,
			wantCompact: 0,
			wantStep:    5,
		},
		{
			name:        "migrates old transcript_lines_at_start",
			json:        `{"session_id":"s1","transcript_lines_at_start":75}`,
			wantCTS:     75,
			wantCompact: 0,
		},
		{
			name:        "preserves new field over old",
			json:        `{"session_id":"s1","condensed_transcript_lines":10,"checkpoint_transcript_start":50}`,
			wantCTS:     50,
			wantCompact: 0,
		},
		{
			name:        "handles clean new format",
			json:        `{"session_id":"s1","checkpoint_transcript_start":25,"checkpoint_count":3}`,
			wantCTS:     25,
			wantCompact: 0,
			wantStep:    3,
		},
		{
			name:        "preserves explicit compact_transcript_start",
			json:        `{"session_id":"s1","checkpoint_transcript_start":25,"compact_transcript_start":9}`,
			wantCTS:     25,
			wantCompact: 9,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var state State
			require.NoError(t, json.Unmarshal([]byte(tt.json), &state))
			state.NormalizeAfterLoad(context.Background())

			assert.Equal(t, tt.wantCTS, state.CheckpointTranscriptStart)
			assert.Equal(t, tt.wantCompact, state.CompactTranscriptStart)
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

func TestStateStore_Clear_RemovesAllSessionFiles(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "entire-sessions")
	require.NoError(t, os.MkdirAll(stateDir, 0o750))
	store := NewStateStoreWithDir(stateDir)
	ctx := context.Background()

	// Create a state file and a .model hint file
	state := &State{
		SessionID:  "hint-session",
		BaseCommit: "abc123",
		StartedAt:  time.Now(),
	}
	require.NoError(t, store.Save(ctx, state))

	hintFile := filepath.Join(stateDir, "hint-session.model")
	require.NoError(t, os.WriteFile(hintFile, []byte("claude-sonnet-4-20250514"), 0o600))

	// Clear should remove all files for this session
	require.NoError(t, store.Clear(ctx, "hint-session"))

	matches, err := filepath.Glob(filepath.Join(stateDir, "hint-session.*"))
	require.NoError(t, err)
	assert.Empty(t, matches, "all session files should be removed")
}

func TestStateStore_Clear_RemovesOrphanedHintFile(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "entire-sessions")
	require.NoError(t, os.MkdirAll(stateDir, 0o750))
	store := NewStateStoreWithDir(stateDir)
	ctx := context.Background()

	// Only a .model hint file exists (no .json state file)
	hintFile := filepath.Join(stateDir, "orphan-session.model")
	require.NoError(t, os.WriteFile(hintFile, []byte("claude-opus-4-6"), 0o600))

	// Clear should succeed and remove the hint file
	require.NoError(t, store.Clear(ctx, "orphan-session"))

	matches, err := filepath.Glob(filepath.Join(stateDir, "orphan-session.*"))
	require.NoError(t, err)
	assert.Empty(t, matches, "orphaned hint file should be removed")
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

func TestStateStore_Load_TraversalResistant(t *testing.T) {
	t.Parallel()

	// Create the state directory and a "secret" file outside it
	stateDir := filepath.Join(t.TempDir(), "entire-sessions")
	require.NoError(t, os.MkdirAll(stateDir, 0o750))

	outsideDir := filepath.Dir(stateDir)
	secretFile := filepath.Join(outsideDir, "secret.json")
	require.NoError(t, os.WriteFile(secretFile, []byte(`{"session_id":"secret","base_commit":"abc"}`), 0o600))

	store := NewStateStoreWithDir(stateDir)

	// Attempt to load with a traversal path should fail validation
	_, err := store.Load(context.Background(), "../secret")
	assert.Error(t, err, "loading with path traversal should fail validation")
}

func TestStateStore_Save_UsesOsRoot(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "entire-sessions")
	store := NewStateStoreWithDir(stateDir)
	ctx := context.Background()

	state := &State{
		SessionID:  "test-osroot-save",
		BaseCommit: "abc123",
		StartedAt:  time.Now(),
	}

	require.NoError(t, store.Save(ctx, state))

	// Verify the file was written
	data, err := os.ReadFile(filepath.Join(stateDir, "test-osroot-save.json"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "test-osroot-save")
}

func TestStateStore_Load_NonexistentDir(t *testing.T) {
	t.Parallel()

	// When the state directory doesn't exist, Load should return (nil, nil)
	store := NewStateStoreWithDir(filepath.Join(t.TempDir(), "nonexistent", "entire-sessions"))
	state, err := store.Load(context.Background(), "some-session")
	require.NoError(t, err)
	assert.Nil(t, state)
}

func TestStateStore_Clear_NonexistentDir(t *testing.T) {
	t.Parallel()

	// When the state directory doesn't exist, Clear returns nil because
	// filepath.Glob finds no matches and os.Root is never opened.
	stateDir := filepath.Join(t.TempDir(), "nonexistent-sessions")
	store := NewStateStoreWithDir(stateDir)
	err := store.Clear(context.Background(), "some-session")
	assert.NoError(t, err)
}

func TestStateStore_SaveLoadClear_SymlinkedDir(t *testing.T) {
	t.Parallel()

	// Simulate macOS-style symlinked temp paths: create the real dir,
	// then point a symlink at it, and use the symlink path as stateDir.
	realDir := filepath.Join(t.TempDir(), "real-sessions")
	require.NoError(t, os.MkdirAll(realDir, 0o750))

	linkParent := t.TempDir()
	symlinkedDir := filepath.Join(linkParent, "linked-sessions")
	require.NoError(t, os.Symlink(realDir, symlinkedDir))

	store := NewStateStoreWithDir(symlinkedDir)
	ctx := context.Background()

	// Save through the symlinked path
	state := &State{
		SessionID:  "symlink-test",
		BaseCommit: "abc123",
		StartedAt:  time.Now(),
	}
	require.NoError(t, store.Save(ctx, state))

	// Load should work through the symlink
	loaded, err := store.Load(ctx, "symlink-test")
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.Equal(t, "symlink-test", loaded.SessionID)

	// File should exist in the real directory
	_, err = os.Stat(filepath.Join(realDir, "symlink-test.json"))
	assert.NoError(t, err, "file should exist in the real directory behind the symlink")

	// Clear should work through the symlink
	require.NoError(t, store.Clear(ctx, "symlink-test"))
	_, err = os.Stat(filepath.Join(realDir, "symlink-test.json"))
	assert.True(t, os.IsNotExist(err), "file should be removed after Clear")
}

func TestStateStore_List_EmptyDir(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "entire-sessions")
	require.NoError(t, os.MkdirAll(stateDir, 0o750))
	store := NewStateStoreWithDir(stateDir)

	states, err := store.List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, states)
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
	testutil.InitRepo(t, dir)
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
	testutil.InitRepo(t, dir1)

	dir2 := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir2); err == nil {
		dir2 = resolved
	}
	testutil.InitRepo(t, dir2)

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

func TestState_KindRoundTrip(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	s := State{
		SessionID:    "2026-04-20-uuid",
		BaseCommit:   "abc",
		StartedAt:    now,
		Kind:         KindAgentReview,
		ReviewSkills: []string{"/review-pr"},
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var got State
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindAgentReview {
		t.Errorf("Kind = %q", got.Kind)
	}
	if len(got.ReviewSkills) != 1 || got.ReviewSkills[0] != "/review-pr" {
		t.Errorf("ReviewSkills = %v", got.ReviewSkills)
	}
}
