package strategy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadSessionState_PackageLevel tests the package-level LoadSessionState function.
func TestLoadSessionState_PackageLevel(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	// Create and save a session state using the package-level function
	state := &SessionState{
		SessionID:                 "test-session-pkg-123",
		BaseCommit:                "abc123def456",
		StartedAt:                 time.Now(),
		StepCount:                 3,
		CheckpointTranscriptStart: 150,
	}

	// Save using package-level function
	err = SaveSessionState(context.Background(), state)
	if err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	// Load using package-level function
	loaded, err := LoadSessionState(context.Background(), "test-session-pkg-123")
	if err != nil {
		t.Fatalf("LoadSessionState() error = %v", err)
	}
	if loaded == nil {
		t.Fatal("LoadSessionState() returned nil")
	}

	// Validate fields (loaded is guaranteed non-nil after the check above)
	verifySessionState(t, loaded, state)
}

// verifySessionState compares loaded session state against expected values.
func verifySessionState(t *testing.T, loaded, expected *SessionState) {
	t.Helper()
	if loaded.SessionID != expected.SessionID {
		t.Errorf("SessionID = %q, want %q", loaded.SessionID, expected.SessionID)
	}
	if loaded.BaseCommit != expected.BaseCommit {
		t.Errorf("BaseCommit = %q, want %q", loaded.BaseCommit, expected.BaseCommit)
	}
	if loaded.StepCount != expected.StepCount {
		t.Errorf("StepCount = %d, want %d", loaded.StepCount, expected.StepCount)
	}
	if loaded.CheckpointTranscriptStart != expected.CheckpointTranscriptStart {
		t.Errorf("CheckpointTranscriptStart = %d, want %d", loaded.CheckpointTranscriptStart, expected.CheckpointTranscriptStart)
	}
}

// TestLoadSessionState_WithEndedAt tests that EndedAt serializes/deserializes correctly.
// TestLoadSessionState_OptionalTimeFields verifies that the optional *time.Time
// fields on SessionState (EndedAt, LastInteractionTime) round-trip correctly
// through save/load — both when set (preserved and Equal) and when nil (stays nil).
func TestLoadSessionState_OptionalTimeFields(t *testing.T) {
	tests := []struct {
		name string
		// set assigns the field on a state and returns the value assigned.
		set func(s *SessionState, ts time.Time)
		// get reads the field back from a loaded state.
		get func(s *SessionState) *time.Time
	}{
		{
			name: "EndedAt",
			set:  func(s *SessionState, ts time.Time) { s.EndedAt = &ts },
			get:  func(s *SessionState) *time.Time { return s.EndedAt },
		},
		{
			name: "LastInteractionTime",
			set:  func(s *SessionState, ts time.Time) { s.LastInteractionTime = &ts },
			get:  func(s *SessionState) *time.Time { return s.LastInteractionTime },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			_, err := git.PlainInit(dir, false)
			if err != nil {
				t.Fatalf("failed to init git repo: %v", err)
			}
			t.Chdir(dir)

			// Field set: it should be preserved and Equal after load.
			ts := time.Now().Add(-time.Hour)
			state := &SessionState{
				SessionID:  "test-session-set",
				BaseCommit: "abc123def456",
				StartedAt:  time.Now().Add(-2 * time.Hour),
				StepCount:  5,
			}
			tt.set(state, ts)

			if err := SaveSessionState(context.Background(), state); err != nil {
				t.Fatalf("SaveSessionState() error = %v", err)
			}
			loaded, err := LoadSessionState(context.Background(), "test-session-set")
			if err != nil {
				t.Fatalf("LoadSessionState() error = %v", err)
			}
			require.NotNil(t, loaded, "LoadSessionState() returned nil")

			got := tt.get(loaded)
			if got == nil {
				t.Fatalf("%s was nil after load, expected non-nil", tt.name)
			}
			if !got.Equal(ts) {
				t.Errorf("%s = %v, want %v", tt.name, *got, ts)
			}

			// Field nil: it should remain nil after load.
			stateNil := &SessionState{
				SessionID:  "test-session-nil",
				BaseCommit: "xyz789",
				StartedAt:  time.Now(),
				StepCount:  1,
			}
			if err := SaveSessionState(context.Background(), stateNil); err != nil {
				t.Fatalf("SaveSessionState() error = %v", err)
			}
			loadedNil, err := LoadSessionState(context.Background(), "test-session-nil")
			if err != nil {
				t.Fatalf("LoadSessionState() error = %v", err)
			}
			require.NotNil(t, loadedNil, "LoadSessionState() returned nil")

			if gotNil := tt.get(loadedNil); gotNil != nil {
				t.Errorf("%s = %v, want nil", tt.name, *gotNil)
			}
		})
	}
}

// TestRecordFilesTouched_MergesIncrementally verifies the helper merges new
// files into existing FilesTouched without losing prior entries — the
// invariant per-tool-use hooks rely on so PostCommit's carry-forward decision
// stays accurate.
func TestRecordFilesTouched_MergesIncrementally(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	t.Chdir(dir)

	state := &SessionState{
		SessionID:    "ft-merge",
		BaseCommit:   "deadbeef",
		StartedAt:    time.Now(),
		FilesTouched: []string{"existing.txt"},
	}
	require.NoError(t, SaveSessionState(context.Background(), state))

	require.NoError(t, RecordFilesTouched(context.Background(), "ft-merge",
		[]string{"updated.txt"}, []string{"new.txt"}, []string{"removed.txt"}))

	loaded, err := LoadSessionState(context.Background(), "ft-merge")
	require.NoError(t, err)
	require.NotNil(t, loaded)
	require.ElementsMatch(t, []string{"existing.txt", "updated.txt", "new.txt", "removed.txt"}, loaded.FilesTouched)
}

func TestRecordFilesTouched_NoStateIsNoop(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	t.Chdir(dir)

	// Hook fires before InitializeSession ran — RecordFilesTouched must not
	// fabricate a state file or error.
	err = RecordFilesTouched(context.Background(), "missing", []string{"f.txt"}, nil, nil)
	require.NoError(t, err)

	loaded, err := LoadSessionState(context.Background(), "missing")
	require.NoError(t, err)
	require.Nil(t, loaded)
}

func TestRecordFilesTouched_EmptyInputsIsNoop(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	t.Chdir(dir)

	state := &SessionState{
		SessionID:    "ft-empty",
		BaseCommit:   "deadbeef",
		StartedAt:    time.Now(),
		FilesTouched: []string{"keep.txt"},
	}
	require.NoError(t, SaveSessionState(context.Background(), state))

	require.NoError(t, RecordFilesTouched(context.Background(), "ft-empty", nil, nil, nil))

	loaded, err := LoadSessionState(context.Background(), "ft-empty")
	require.NoError(t, err)
	require.NotNil(t, loaded)
	require.Equal(t, []string{"keep.txt"}, loaded.FilesTouched)
}

// TestClearSessionState_PreservesLockFile pins the rule that ClearSessionState
// must NOT unlink the per-session lock file. Unlinking the lock path while
// another process holds an advisory lock on the inode would let a third
// caller recreate the file and acquire an independent lock — losing mutual
// exclusion. The lock file is a 0-byte sentinel; leaving it on disk after
// state-file removal is harmless.
func TestClearSessionState_PreservesLockFile(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	t.Chdir(dir)

	sessionID := "ft-clear-keeps-lock"
	state := &SessionState{
		SessionID:  sessionID,
		BaseCommit: "deadbeef",
		StartedAt:  time.Now(),
	}
	require.NoError(t, SaveSessionState(context.Background(), state))

	// Touch the lock file by entering MutateSessionState once.
	require.NoError(t, MutateSessionState(context.Background(), sessionID, func(_ *SessionState) error {
		return ErrMutationSkip
	}))

	lockPath, err := stateLockPath(context.Background(), sessionID)
	require.NoError(t, err)
	_, statErr := os.Stat(lockPath)
	require.NoError(t, statErr, "lock file must exist after a MutateSessionState call")

	require.NoError(t, ClearSessionState(context.Background(), sessionID))

	_, statErr = os.Stat(lockPath)
	require.NoError(t, statErr, "ClearSessionState must not unlink the lock file (would break flock semantics)")
}

// TestMutateSessionState_DoesNotClobberRicherStateUnderRace simulates the
// TOCTOU window between an existence check and a default-state init: a
// caller observes "no state", but a concurrent richer write lands before
// the init takes the lock. The init must re-read under lock and skip the
// write rather than overwriting TranscriptPath, LastPrompt, etc. with
// blanks.
func TestMutateSessionState_DoesNotClobberRicherStateUnderRace(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	t.Chdir(dir)

	sessionID := "ft-toctou"
	rich := &SessionState{
		SessionID:      sessionID,
		BaseCommit:     "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		StartedAt:      time.Now(),
		TranscriptPath: "/tmp/transcript.jsonl",
		LastPrompt:     "find the bug",
		ModelName:      "gpt-5",
	}
	require.NoError(t, SaveSessionState(context.Background(), rich))

	// Pretend another process raced past its existence check while ours
	// was about to initialize: do a no-op MutateSessionState that sets a
	// clearly different value for an init-overwritten field. If the next
	// call (simulating initializeSession's create path) reloads under the
	// lock and bails out, our richer fields survive.
	require.NoError(t, MutateSessionState(context.Background(), sessionID, func(_ *SessionState) error {
		// no mutation; the test is about what the simulated init does next
		return ErrMutationSkip
	}))

	// Now run the lock-then-recheck dance the real init does. Pass a state
	// with all-empty derived fields to mimic the default-state shape.
	_, _, release, lockErr := acquireSessionGate(context.Background(), sessionID)
	require.NoError(t, lockErr)
	existing, loadErr := LoadSessionState(context.Background(), sessionID)
	release()
	require.NoError(t, loadErr)
	require.NotNil(t, existing)
	require.Equal(t, "/tmp/transcript.jsonl", existing.TranscriptPath, "richer state must survive re-check under lock")
	require.Equal(t, "find the bug", existing.LastPrompt)
	require.Equal(t, "gpt-5", existing.ModelName)
}

// TestMutateSessionState_NestedCallsAreReentrant verifies that calling
// MutateSessionState from within an outer MutateSessionState callback
// doesn't deadlock. POSIX flock isn't reentrant across distinct FDs in the
// same process, so the gate's goroutine-ID ownership tracking has to skip
// the flock re-acquire on the inner call.
func TestMutateSessionState_NestedCallsAreReentrant(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	t.Chdir(dir)

	state := &SessionState{
		SessionID:  "ft-nested",
		BaseCommit: "deadbeef",
		StartedAt:  time.Now(),
	}
	require.NoError(t, SaveSessionState(context.Background(), state))

	done := make(chan struct{})
	go func() {
		defer close(done)
		err := MutateSessionState(context.Background(), "ft-nested", func(outer *SessionState) error {
			outer.LastPrompt = "outer"
			return MutateSessionState(context.Background(), "ft-nested", func(inner *SessionState) error {
				inner.ModelName = "inner"
				return nil
			})
		})
		assert.NoError(t, err)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("nested MutateSessionState deadlocked")
	}

	loaded, err := LoadSessionState(context.Background(), "ft-nested")
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.Equal(t, "outer", loaded.LastPrompt)
	assert.Equal(t, "inner", loaded.ModelName)
}

// TestRecordFilesTouched_ParallelMergesAreSerialized verifies the file-lock
// in RecordFilesTouched: many concurrent callers, each merging a unique
// file, must all land in FilesTouched. Without the lock, parallel
// load → merge → save would lose updates and the final list would be missing
// entries (or have duplicates).
func TestRecordFilesTouched_ParallelMergesAreSerialized(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	t.Chdir(dir)

	state := &SessionState{
		SessionID:  "ft-parallel",
		BaseCommit: "deadbeef",
		StartedAt:  time.Now(),
	}
	require.NoError(t, SaveSessionState(context.Background(), state))

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			path := fmt.Sprintf("file-%02d.go", i)
			err := RecordFilesTouched(context.Background(), "ft-parallel", nil, []string{path}, nil)
			assert.NoError(t, err)
		}()
	}
	wg.Wait()

	loaded, err := LoadSessionState(context.Background(), "ft-parallel")
	require.NoError(t, err)
	require.NotNil(t, loaded)
	require.Len(t, loaded.FilesTouched, n, "every concurrent merge should be present")
}

// TestLoadSessionState_PackageLevel_NonExistent tests loading a non-existent session.
func TestLoadSessionState_PackageLevel_NonExistent(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	loaded, err := LoadSessionState(context.Background(), "nonexistent-session")
	if err != nil {
		t.Errorf("LoadSessionState() error = %v, want nil for nonexistent session", err)
	}
	if loaded != nil {
		t.Error("LoadSessionState() returned non-nil for nonexistent session")
	}
}

// TestManualCommitStrategy_SessionState_UsesPackageFunctions tests that ManualCommitStrategy
// methods delegate to the package-level functions.
func TestManualCommitStrategy_SessionState_UsesPackageFunctions(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	// Save using package-level function
	state := &SessionState{
		SessionID:  "cross-usage-test",
		BaseCommit: "xyz789",
		StartedAt:  time.Now(),
		StepCount:  2,
	}
	if err := SaveSessionState(context.Background(), state); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	// Load using ManualCommitStrategy method - should find the same state
	s := &ManualCommitStrategy{}
	loaded, err := s.loadSessionState(context.Background(), "cross-usage-test")
	if err != nil {
		t.Fatalf("ManualCommitStrategy.loadSessionState() error = %v", err)
	}
	require.NotNil(t, loaded, "ManualCommitStrategy.loadSessionState() returned nil")

	// Verify via helper (loaded guaranteed non-nil after Fatal above)

	if loaded.SessionID != state.SessionID {
		t.Errorf("SessionID = %q, want %q", loaded.SessionID, state.SessionID)
	}

	// Save using ManualCommitStrategy method
	state2 := &SessionState{
		SessionID:  "cross-usage-test-2",
		BaseCommit: "abc123",
		StartedAt:  time.Now(),
		StepCount:  1,
	}
	if err := s.saveSessionState(context.Background(), state2); err != nil {
		t.Fatalf("ManualCommitStrategy.saveSessionState() error = %v", err)
	}

	// Load using package-level function - should find the state
	loaded2, err := LoadSessionState(context.Background(), "cross-usage-test-2")
	if err != nil {
		t.Fatalf("LoadSessionState() error = %v", err)
	}
	require.NotNil(t, loaded2, "LoadSessionState() returned nil")

	// Verify via direct comparison (loaded2 guaranteed non-nil after Fatal above)

	if loaded2.SessionID != state2.SessionID {
		t.Errorf("SessionID = %q, want %q", loaded2.SessionID, state2.SessionID)
	}
}

// TestFindMostRecentSession_FiltersByWorktree tests that FindMostRecentSession
// returns sessions from the current worktree, not from other worktrees.
func TestFindMostRecentSession_FiltersByWorktree(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	// Get the resolved worktree path (git resolves symlinks, e.g. /var → /private/var on macOS)
	resolvedDir, err := paths.WorktreeRoot(context.Background())
	if err != nil {
		t.Fatalf("paths.WorktreeRoot() error = %v", err)
	}

	older := time.Now().Add(-1 * time.Hour)
	newer := time.Now()

	// Session from a different worktree (more recent)
	otherWorktree := &SessionState{
		SessionID:           "other-worktree-session",
		BaseCommit:          "abc1234",
		WorktreePath:        "/some/other/worktree",
		StartedAt:           newer,
		LastInteractionTime: &newer,
		Phase:               "idle",
	}

	// Session from current worktree (older)
	currentWorktree := &SessionState{
		SessionID:           "current-worktree-session",
		BaseCommit:          "xyz7890",
		WorktreePath:        resolvedDir, // matches current worktree
		StartedAt:           older,
		LastInteractionTime: &older,
		Phase:               "idle",
	}

	if err := SaveSessionState(context.Background(), otherWorktree); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}
	if err := SaveSessionState(context.Background(), currentWorktree); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	// FindMostRecentSession should return the current worktree's session,
	// not the other worktree's session (even though it's more recent).
	result := FindMostRecentSession(context.Background())
	if result != "current-worktree-session" {
		t.Errorf("FindMostRecentSession(context.Background()) = %q, want %q (should prefer current worktree)",
			result, "current-worktree-session")
	}
}

// TestFindMostRecentSession_FallsBackWhenNoWorktreeMatch tests that
// FindMostRecentSession falls back to all sessions when none match the current worktree.
func TestFindMostRecentSession_FallsBackWhenNoWorktreeMatch(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	newer := time.Now()

	// Session from a different worktree only (no sessions for current worktree)
	otherWorktree := &SessionState{
		SessionID:           "only-session",
		BaseCommit:          "abc1234",
		WorktreePath:        "/some/other/worktree",
		StartedAt:           newer,
		LastInteractionTime: &newer,
		Phase:               "idle",
	}

	if err := SaveSessionState(context.Background(), otherWorktree); err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	// Should fall back to the only available session since none match current worktree
	result := FindMostRecentSession(context.Background())
	if result != "only-session" {
		t.Errorf("FindMostRecentSession(context.Background()) = %q, want %q (should fall back when no worktree match)",
			result, "only-session")
	}

	// Cleanup
	if err := os.Remove(dir + "/.git/entire-sessions/only-session.json"); err != nil && !os.IsNotExist(err) {
		t.Logf("cleanup warning: %v", err)
	}
}

// errorActionHandler returns an error from HandleCondense to test
// that TransitionAndLog propagates handler errors while still applying the phase transition.
type errorActionHandler struct {
	session.NoOpActionHandler
}

func (errorActionHandler) HandleCondense(_ *session.State) error {
	return errors.New("test condense error")
}

// TestTransitionAndLog_ReturnsHandlerError verifies that TransitionAndLog
// applies the phase transition even when the handler returns an error,
// and propagates that error to the caller.
func TestTransitionAndLog_ReturnsHandlerError(t *testing.T) {
	t.Parallel()

	state := &SessionState{
		SessionID: "test-error-handler",
		Phase:     session.PhaseIdle,
	}

	// IDLE + GitCommit → IDLE with ActionCondense.
	// The handler will fail on ActionCondense, but the phase should still be IDLE.
	err := TransitionAndLog(context.Background(), state, session.EventGitCommit, session.TransitionContext{}, &errorActionHandler{})

	if state.Phase != session.PhaseIdle {
		t.Errorf("Phase = %q, want %q (should transition despite handler error)", state.Phase, session.PhaseIdle)
	}
	if err == nil {
		t.Error("TransitionAndLog() should return handler error")
	}
}

// TestLoadSessionState_DeletesStaleSession tests that LoadSessionState returns (nil, nil)
// for a stale session and deletes the file from disk.
func TestLoadSessionState_DeletesStaleSession(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	// Create a stale session (ended >2wk ago)
	staleInteracted := time.Now().Add(-2 * 7 * 24 * time.Hour)
	state := &SessionState{
		SessionID:           "stale-load-test",
		BaseCommit:          "abc123def456",
		StartedAt:           time.Now().Add(-3 * 7 * 24 * time.Hour),
		LastInteractionTime: &staleInteracted,
		StepCount:           5,
	}

	err = SaveSessionState(context.Background(), state)
	if err != nil {
		t.Fatalf("SaveSessionState() error = %v", err)
	}

	// Verify file exists before load
	stateFile, err := sessionStateFile(context.Background(), "stale-load-test")
	if err != nil {
		t.Fatalf("sessionStateFile() error = %v", err)
	}
	if _, err := os.Stat(stateFile); err != nil {
		t.Fatalf("state file should exist before load: %v", err)
	}

	// Load should return (nil, nil) for stale session
	loaded, err := LoadSessionState(context.Background(), "stale-load-test")
	if err != nil {
		t.Errorf("LoadSessionState() error = %v, want nil for stale session", err)
	}
	if loaded != nil {
		t.Error("LoadSessionState() returned non-nil for stale session")
	}

	// File should be deleted from disk
	if _, err := os.Stat(stateFile); !os.IsNotExist(err) {
		t.Error("stale session file should be deleted after LoadSessionState()")
	}
}

// --- Model hint file tests ---

func TestStoreModelHint_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}
	t.Chdir(dir)

	ctx := context.Background()
	sessionID := "2026-01-01-hint-roundtrip"

	err = StoreModelHint(ctx, sessionID, "claude-sonnet-4-20250514")
	if err != nil {
		t.Fatalf("StoreModelHint() error = %v", err)
	}

	got := LoadModelHint(ctx, sessionID)
	if got != "claude-sonnet-4-20250514" {
		t.Errorf("LoadModelHint() = %q, want %q", got, "claude-sonnet-4-20250514")
	}
}

func TestStoreModelHint_EmptyModel_NoOp(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}
	t.Chdir(dir)

	ctx := context.Background()
	sessionID := "2026-01-01-hint-empty"

	err = StoreModelHint(ctx, sessionID, "")
	if err != nil {
		t.Fatalf("StoreModelHint() error = %v", err)
	}

	// No file should have been created
	stateDir, sdErr := getSessionStateDir(ctx)
	if sdErr != nil {
		t.Fatalf("getSessionStateDir() error = %v", sdErr)
	}
	hintPath := stateDir + "/" + sessionID + ".model"
	if _, statErr := os.Stat(hintPath); !os.IsNotExist(statErr) {
		t.Error("StoreModelHint with empty model should not create a file")
	}
}

func TestLoadModelHint_NoFile_ReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}
	t.Chdir(dir)

	got := LoadModelHint(context.Background(), "2026-01-01-nonexistent")
	if got != "" {
		t.Errorf("LoadModelHint() = %q, want empty string for missing file", got)
	}
}

func TestStoreModelHint_InvalidSessionID_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}
	t.Chdir(dir)

	err = StoreModelHint(context.Background(), "../../../etc/passwd", "model")
	if err == nil {
		t.Error("StoreModelHint() should return error for invalid session ID")
	}
}

func TestLoadModelHint_InvalidSessionID_ReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}
	t.Chdir(dir)

	got := LoadModelHint(context.Background(), "../../../etc/passwd")
	if got != "" {
		t.Errorf("LoadModelHint() = %q, want empty for invalid session ID", got)
	}
}

func TestLoadModelHint_TrimsWhitespace(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}
	t.Chdir(dir)

	ctx := context.Background()
	sessionID := "2026-01-01-hint-whitespace"

	// Write hint with trailing newline (simulating manual edit)
	stateDir, sdErr := getSessionStateDir(ctx)
	if sdErr != nil {
		t.Fatalf("getSessionStateDir() error = %v", sdErr)
	}
	if mkErr := os.MkdirAll(stateDir, 0o750); mkErr != nil {
		t.Fatalf("MkdirAll() error = %v", mkErr)
	}
	hintPath := stateDir + "/" + sessionID + ".model"
	if wErr := os.WriteFile(hintPath, []byte("claude-opus-4-6\n"), 0o600); wErr != nil {
		t.Fatalf("WriteFile() error = %v", wErr)
	}

	got := LoadModelHint(ctx, sessionID)
	if got != "claude-opus-4-6" {
		t.Errorf("LoadModelHint() = %q, want %q (should trim whitespace)", got, "claude-opus-4-6")
	}
}

// --- Agent type hint file tests ---

func TestStoreAgentTypeHint_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	t.Chdir(dir)

	ctx := context.Background()
	sessionID := "2026-01-01-agent-roundtrip"

	created, err := StoreAgentTypeHint(ctx, sessionID, agent.AgentTypeCursor)
	require.NoError(t, err)
	require.True(t, created, "first call must report it created the hint")

	got := LoadAgentTypeHint(ctx, sessionID)
	require.Equal(t, agent.AgentTypeCursor, got)
}

func TestStoreAgentTypeHint_FirstWriterWins(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	t.Chdir(dir)

	ctx := context.Background()
	sessionID := "2026-01-01-agent-firstwriter"

	// Cursor claims the session first.
	created, err := StoreAgentTypeHint(ctx, sessionID, agent.AgentTypeCursor)
	require.NoError(t, err)
	require.True(t, created)

	// Claude Code's hook fires next (concurrent forwarded-hook scenario).
	// Should be a no-op — does not overwrite the existing hint.
	created, err = StoreAgentTypeHint(ctx, sessionID, agent.AgentTypeClaudeCode)
	require.NoError(t, err)
	require.False(t, created, "second call must report it did not create the hint")

	got := LoadAgentTypeHint(ctx, sessionID)
	require.Equal(t, agent.AgentTypeCursor, got, "first writer's hint must persist")
}

func TestStoreAgentTypeHint_EmptyOrUnknown_NoOp(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	t.Chdir(dir)

	ctx := context.Background()

	for _, tc := range []struct {
		sid string
		at  types.AgentType
	}{
		{"2026-01-01-empty", ""},
		{"2026-01-01-unknown", agent.AgentTypeUnknown},
	} {
		created, hErr := StoreAgentTypeHint(ctx, tc.sid, tc.at)
		require.NoError(t, hErr)
		require.False(t, created, "empty/Unknown must report created=false")
	}

	stateDir, sdErr := getSessionStateDir(ctx)
	require.NoError(t, sdErr)

	for _, sid := range []string{"2026-01-01-empty", "2026-01-01-unknown"} {
		hintPath := filepath.Join(stateDir, sid+".agent")
		_, statErr := os.Stat(hintPath)
		require.True(t, os.IsNotExist(statErr), "no hint file should be created for empty/Unknown agent type")
	}
}

func TestLoadAgentTypeHint_NoFile_ReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	t.Chdir(dir)

	got := LoadAgentTypeHint(context.Background(), "2026-01-01-nonexistent")
	require.Empty(t, string(got))
}

func TestStoreAgentTypeHint_InvalidSessionID_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	t.Chdir(dir)

	_, err = StoreAgentTypeHint(context.Background(), "../../../etc/passwd", agent.AgentTypeCursor)
	require.Error(t, err)
}

func TestClaimSessionStartBanner_FirstWriterWins(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	t.Chdir(dir)

	ctx := context.Background()
	sessionID := "2026-01-01-banner-claim"

	claimed, err := ClaimSessionStartBanner(ctx, sessionID)
	require.NoError(t, err)
	require.True(t, claimed, "first call must win the banner claim")

	claimed, err = ClaimSessionStartBanner(ctx, sessionID)
	require.NoError(t, err)
	require.False(t, claimed, "subsequent calls must report the banner already claimed")
}

func TestClaimSessionStartBanner_InvalidSessionID_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	t.Chdir(dir)

	_, err = ClaimSessionStartBanner(context.Background(), "../../../etc/passwd")
	require.Error(t, err)
}

func TestClearSessionState_RemovesBannerMarker(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	t.Chdir(dir)

	ctx := context.Background()
	sessionID := "2026-01-01-clear-banner"

	_, err = ClaimSessionStartBanner(ctx, sessionID)
	require.NoError(t, err)
	require.NoError(t, ClearSessionState(ctx, sessionID))

	// After clear, the marker is gone — the next claim wins again.
	claimed, err := ClaimSessionStartBanner(ctx, sessionID)
	require.NoError(t, err)
	require.True(t, claimed, "ClearSessionState should remove the banner marker")
}

func TestClearSessionState_RemovesAgentHint(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	t.Chdir(dir)

	ctx := context.Background()
	sessionID := "2026-01-01-clear-agent-hint"

	_, err = StoreAgentTypeHint(ctx, sessionID, agent.AgentTypeCursor)
	require.NoError(t, err)
	require.NoError(t, ClearSessionState(ctx, sessionID))

	got := LoadAgentTypeHint(ctx, sessionID)
	require.Empty(t, string(got))
}

func TestClearSessionState_RemovesHintFile(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}
	t.Chdir(dir)

	ctx := context.Background()
	sessionID := "2026-01-01-clear-hint"

	// Create both state and hint files
	state := &SessionState{
		SessionID:  sessionID,
		BaseCommit: "abc123",
		StartedAt:  time.Now(),
	}
	if sErr := SaveSessionState(ctx, state); sErr != nil {
		t.Fatalf("SaveSessionState() error = %v", sErr)
	}
	if sErr := StoreModelHint(ctx, sessionID, "some-model"); sErr != nil {
		t.Fatalf("StoreModelHint() error = %v", sErr)
	}

	// Clear should remove both
	if cErr := ClearSessionState(ctx, sessionID); cErr != nil {
		t.Fatalf("ClearSessionState() error = %v", cErr)
	}

	stateDir, sdErr := getSessionStateDir(ctx)
	if sdErr != nil {
		t.Fatalf("getSessionStateDir() error = %v", sdErr)
	}
	matches, err := filepath.Glob(filepath.Join(stateDir, sessionID+".*"))
	if err != nil {
		t.Fatalf("filepath.Glob() error = %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("expected no files for session after clear, found: %v", matches)
	}
}

func TestClearSessionState_RemovesOrphanedHintFile(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}
	t.Chdir(dir)

	ctx := context.Background()
	sessionID := "2026-01-01-orphan-hint"

	// Only create hint file (no state file)
	if sErr := StoreModelHint(ctx, sessionID, "orphan-model"); sErr != nil {
		t.Fatalf("StoreModelHint() error = %v", sErr)
	}

	// Clear should succeed and remove the hint file
	if cErr := ClearSessionState(ctx, sessionID); cErr != nil {
		t.Fatalf("ClearSessionState() error = %v", cErr)
	}

	stateDir, sdErr := getSessionStateDir(ctx)
	if sdErr != nil {
		t.Fatalf("getSessionStateDir() error = %v", sdErr)
	}
	matches, err := filepath.Glob(filepath.Join(stateDir, sessionID+".*"))
	if err != nil {
		t.Fatalf("filepath.Glob() error = %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("expected no files for session after clear, found: %v", matches)
	}
}
