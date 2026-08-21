package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestState_CondensationAttemptLifecycle(t *testing.T) {
	t.Parallel()

	state := &State{}
	checkpointID := id.MustCheckpointID("111111111111")

	require.True(t, state.PendingCondensationID().IsEmpty())
	require.False(t, state.NeedsCondensationRecovery())

	state.BeginCondensationAttempt(checkpointID)
	require.Equal(t, checkpointID, state.PendingCondensationID())
	require.False(t, state.NeedsCondensationRecovery())

	state.RequireCondensationRecovery()
	require.True(t, state.NeedsCondensationRecovery())

	state.ClearCondensationAttempt()
	require.True(t, state.PendingCondensationID().IsEmpty())
	require.False(t, state.NeedsCondensationRecovery())
}

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

	t.Run("imported_is_never_stale", func(t *testing.T) {
		t.Parallel()
		// Imported sessions carry historical timestamps (always old) but must
		// never be auto-purged, or they'd vanish from `session list` on read.
		old := time.Now().Add(-30 * 24 * time.Hour)
		imported := &State{Kind: KindImported, StartedAt: old, LastInteractionTime: &old}
		assert.False(t, imported.IsStale(), "imported session should never be stale")

		// Control: a non-imported session of the same age IS stale (guards
		// against an over-broad exemption).
		normal := &State{StartedAt: old, LastInteractionTime: &old}
		assert.True(t, normal.IsStale())
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

// TestKind_IsInvestigate pins the umbrella-flag classifier for investigate
// kinds. Mirrors the pattern used for IsReview: a session's Kind is asked
// "do you count as an investigation?" without callers needing to know the
// specific Kind variant.
func TestKind_IsInvestigate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		k    Kind
		want bool
	}{
		{"investigate", KindAgentInvestigate, true},
		{"review_is_not_investigate", KindAgentReview, false},
		{"empty", Kind(""), false},
		{"unknown", Kind("something_else"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.k.IsInvestigate(); got != tc.want {
				t.Errorf("Kind(%q).IsInvestigate() = %v, want %v", tc.k, got, tc.want)
			}
		})
	}
}

// TestState_InvestigateRoundTrip pins the JSON wire format for the
// investigate fields on State so a future tag rename or migration can't
// silently drop persisted fields.
func TestState_InvestigateRoundTrip(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	s := State{
		SessionID:        "2026-04-20-uuid",
		BaseCommit:       "abc",
		StartedAt:        now,
		Kind:             KindAgentInvestigate,
		InvestigateRunID: "abcdef012345",
		InvestigateTopic: "Why is checkout flaky?",
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}

	// Inspect raw JSON to pin the on-disk keys.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if got, ok := raw["kind"].(string); !ok || got != "agent_investigate" {
		t.Errorf("kind = %v, want agent_investigate", raw["kind"])
	}
	if got, ok := raw["investigate_run_id"].(string); !ok || got != "abcdef012345" {
		t.Errorf("investigate_run_id = %v", raw["investigate_run_id"])
	}
	if got, ok := raw["investigate_topic"].(string); !ok || got != "Why is checkout flaky?" {
		t.Errorf("investigate_topic = %v", raw["investigate_topic"])
	}

	// Round-trip back into a State and verify field values survive.
	var got State
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindAgentInvestigate {
		t.Errorf("Kind = %q", got.Kind)
	}
	if got.InvestigateRunID != "abcdef012345" {
		t.Errorf("InvestigateRunID = %q", got.InvestigateRunID)
	}
	if got.InvestigateTopic != "Why is checkout flaky?" {
		t.Errorf("InvestigateTopic = %q", got.InvestigateTopic)
	}

	// Zero-value: omitempty must keep the keys out of marshalled output for a
	// non-investigate session.
	zero := State{SessionID: "x", BaseCommit: "y", StartedAt: now}
	zb, err := json.Marshal(zero)
	if err != nil {
		t.Fatal(err)
	}
	zs := string(zb)
	for _, key := range []string{"investigate_run_id", "investigate_topic"} {
		if strings.Contains(zs, `"`+key+`"`) {
			t.Errorf("expected zero-value State to omit %q, got %s", key, zs)
		}
	}
}

// TestState_TaskRecords_RoundTrip pins the JSON wire format for the durable
// task record ledger, including the fields added for #2058's pointer model
// (DeclaredTranscriptPath, Files, TokenUsage, CompletedAt) and the renamed
// "task_records" json key. Regression this guards: without a persisted record
// of a dispatched subagent, the launch-time post-task hook (which fires at
// the launch stub, seconds before any real work happens) has no way to defer
// capture to SubagentStop — the record is the only memory that background
// work is still outstanding, and it must also survive completion (unlike the
// prior claim-and-remove model) so a later condensation can materialize it.
func TestState_TaskRecords_RoundTrip(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	completedAt := now.Add(time.Minute)
	s := State{
		SessionID:  "2026-04-20-uuid",
		BaseCommit: "abc",
		StartedAt:  now,
		TaskRecords: []TaskRecord{
			{
				ToolUseID:              "toolu_01X",
				AgentID:                "a123",
				StartedAt:              now,
				SubagentType:           "code-reviewer",
				TaskDescription:        "Review the diff",
				DeclaredTranscriptPath: "/tmp/agent-a123.jsonl",
				Files:                  []string{"foo.go", "bar.go"},
				TokenUsage:             &agent.TokenUsage{InputTokens: 100, OutputTokens: 50},
				CompletedAt:            completedAt,
			},
		},
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"task_records"`) {
		t.Fatalf("expected json to use the task_records key, got %s", data)
	}
	if strings.Contains(string(data), `"in_flight_tasks"`) {
		t.Fatalf("expected the legacy in_flight_tasks key to be gone, got %s", data)
	}

	var got State
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	require.Len(t, got.TaskRecords, 1)
	record := got.TaskRecords[0]
	assert.Equal(t, "toolu_01X", record.ToolUseID)
	assert.Equal(t, "a123", record.AgentID)
	assert.True(t, now.Equal(record.StartedAt))
	assert.Equal(t, "code-reviewer", record.SubagentType)
	assert.Equal(t, "Review the diff", record.TaskDescription)
	assert.Equal(t, "/tmp/agent-a123.jsonl", record.DeclaredTranscriptPath)
	assert.Equal(t, []string{"foo.go", "bar.go"}, record.Files)
	require.NotNil(t, record.TokenUsage)
	assert.Equal(t, 100, record.TokenUsage.InputTokens)
	assert.Equal(t, 50, record.TokenUsage.OutputTokens)
	assert.True(t, completedAt.Equal(record.CompletedAt))

	// Zero-value: an empty task record list must be omitted entirely, not
	// serialized as "task_records":[] or ":null".
	zero := State{SessionID: "x", BaseCommit: "y", StartedAt: now}
	zb, err := json.Marshal(zero)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(zb), `"task_records"`) {
		t.Errorf("expected zero-value State to omit task_records, got %s", zb)
	}
}

// TestState_AddTaskRecord_DedupByToolUseID pins that a duplicate launch event
// for the same Task tool invocation replaces the existing record instead of
// accumulating a second one. Regression this guards: without dedup, a
// retried/duplicate PostToolUse for the same tool_use_id would leave two
// records, and RemoveTaskRecord (which removes only the first match) would
// leak a stale entry behind.
func TestState_AddTaskRecord_DedupByToolUseID(t *testing.T) {
	t.Parallel()
	s := &State{}
	first := time.Now().UTC()
	s.AddTaskRecord(TaskRecord{ToolUseID: "toolu_1", AgentID: "a1", StartedAt: first})
	require.Len(t, s.TaskRecords, 1)

	second := first.Add(time.Minute)
	s.AddTaskRecord(TaskRecord{ToolUseID: "toolu_1", AgentID: "a1-retry", StartedAt: second})
	require.Len(t, s.TaskRecords, 1, "duplicate ToolUseID must replace, not append")
	assert.Equal(t, "a1-retry", s.TaskRecords[0].AgentID)
	assert.True(t, second.Equal(s.TaskRecords[0].StartedAt))

	// A different ToolUseID does get appended.
	s.AddTaskRecord(TaskRecord{ToolUseID: "toolu_2", StartedAt: second})
	require.Len(t, s.TaskRecords, 2)
}

// TestState_TaskRecordAccessors pins Remove/Find record semantics: Remove
// clears only the matching record; Find returns the record by reference (the
// Final-path handler reads its launch-recorded label without copying). Both
// treat an unknown ToolUseID safely — no-op / nil, not a panic — the
// foreground-task / double-fire "no record" case the Final-path dedup relies
// on. RemoveTaskRecord itself is no longer on the ordinary completion path
// (CompleteTaskRecord keeps the record for the materializer instead) but
// stays covered since callers can still use it to discard a record outright.
func TestState_TaskRecordAccessors(t *testing.T) {
	t.Parallel()

	t.Run("find", func(t *testing.T) {
		t.Parallel()
		s := &State{TaskRecords: []TaskRecord{{ToolUseID: "toolu_1", SubagentType: "reviewer"}, {ToolUseID: "toolu_2", SubagentType: "dev"}}}

		got := s.FindTaskRecord("toolu_2")
		require.NotNil(t, got)
		assert.Equal(t, "dev", got.SubagentType)

		// Nil (not a panic) on a ToolUseID with no record.
		assert.Nil(t, s.FindTaskRecord("does-not-exist"))
	})

	t.Run("remove", func(t *testing.T) {
		t.Parallel()
		s := &State{TaskRecords: []TaskRecord{{ToolUseID: "toolu_1"}, {ToolUseID: "toolu_2"}}}

		s.RemoveTaskRecord("toolu_1")
		require.Len(t, s.TaskRecords, 1)
		assert.Equal(t, "toolu_2", s.TaskRecords[0].ToolUseID)

		// No-op when the ToolUseID has no record.
		s.RemoveTaskRecord("does-not-exist")
		require.Len(t, s.TaskRecords, 1)

		s.RemoveTaskRecord("toolu_2")
		assert.Empty(t, s.TaskRecords)
	})
}

// TestState_CompleteTaskRecord_ExactlyOnce pins CompleteTaskRecord's
// exactly-once completion guard — the replacement for the old
// claim-and-remove semantics, now that a completed record must persist for
// the future condensation materializer rather than being deleted.
// Regression this guards: two racing Final-path captures for the same
// ToolUseID (a late SubagentStop arriving just as SessionEnd sweeps, or a
// duplicate SubagentStop delivery) must capture exactly once. Note:
// CompleteTaskRecord only sets CompletedAt — DeclaredTranscriptPath/Files/
// TokenUsage are populated separately by the producer, via direct field
// mutation on the claimed record within the same MutateSessionState closure
// (see the type doc comment), so this test does not exercise those fields.
func TestState_CompleteTaskRecord_ExactlyOnce(t *testing.T) {
	t.Parallel()

	t.Run("absent_record_is_a_noop", func(t *testing.T) {
		t.Parallel()
		s := &State{}
		ok := s.CompleteTaskRecord("does-not-exist", time.Now())
		assert.False(t, ok)
	})

	t.Run("first_completion_succeeds", func(t *testing.T) {
		t.Parallel()
		s := &State{TaskRecords: []TaskRecord{{ToolUseID: "toolu_1", StartedAt: time.Now()}}}
		completedAt := time.Now().UTC().Truncate(time.Second)
		ok := s.CompleteTaskRecord("toolu_1", completedAt)
		require.True(t, ok)
		require.Len(t, s.TaskRecords, 1, "completing a record must not remove it — it must persist for the materializer")
		assert.True(t, completedAt.Equal(s.TaskRecords[0].CompletedAt))
	})

	t.Run("second_completion_is_rejected", func(t *testing.T) {
		t.Parallel()
		s := &State{TaskRecords: []TaskRecord{{ToolUseID: "toolu_1", StartedAt: time.Now()}}}
		firstCompletedAt := time.Now().UTC().Truncate(time.Second)
		require.True(t, s.CompleteTaskRecord("toolu_1", firstCompletedAt))

		// A second completion attempt (the racing duplicate) must be rejected
		// and must not move CompletedAt.
		second := s.CompleteTaskRecord("toolu_1", firstCompletedAt.Add(time.Hour))
		assert.False(t, second)
		assert.True(t, firstCompletedAt.Equal(s.TaskRecords[0].CompletedAt), "a rejected second completion must not move CompletedAt")
	})
}

// TestState_LiveTaskRecords pins that LiveTaskRecords filters to records with
// a zero CompletedAt. Regression this guards: since CompleteTaskRecord no
// longer removes a record, code that used to treat "any InFlightTasks
// present" as "any task still running" must filter for liveness explicitly,
// or a completed-but-not-yet-materialized record would look like live
// in-flight work forever.
func TestState_LiveTaskRecords(t *testing.T) {
	t.Parallel()
	s := &State{
		TaskRecords: []TaskRecord{
			{ToolUseID: "toolu_live", StartedAt: time.Now()},
			{ToolUseID: "toolu_done", StartedAt: time.Now(), CompletedAt: time.Now()},
		},
	}
	live := s.LiveTaskRecords()
	require.Len(t, live, 1)
	assert.Equal(t, "toolu_live", live[0].ToolUseID)

	assert.Empty(t, (&State{}).LiveTaskRecords())
}
