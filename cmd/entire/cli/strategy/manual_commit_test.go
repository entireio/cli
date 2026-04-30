package strategy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/redact"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

const testTrailerCheckpointID id.CheckpointID = "a1b2c3d4e5f6"

const testCheckpointsV2SettingsJSON = `{"enabled": true, "strategy": "manual-commit", "strategy_options": {"checkpoints_v2": true}}`

// testTranscriptPromptResponse is a minimal transcript used across strategy tests.
const testTranscriptPromptResponse = "{\"type\":\"human\",\"message\":{\"content\":\"test prompt\"}}\n{\"type\":\"assistant\",\"message\":{\"content\":\"test response\"}}\n"

func TestShadowStrategy_ValidateRepository(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	s := NewManualCommitStrategy()
	err = s.ValidateRepository()
	if err != nil {
		t.Errorf("ValidateRepository() error = %v, want nil", err)
	}
}

func TestShadowStrategy_ValidateRepository_NotGitRepo(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	s := NewManualCommitStrategy()
	err := s.ValidateRepository()
	if err == nil {
		t.Error("ValidateRepository() error = nil, want error for non-git directory")
	}
}

func TestShadowStrategy_SessionState_SaveLoad(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	state := &SessionState{
		SessionID:  "test-session-123",
		BaseCommit: "abc123def456",
		StartedAt:  time.Now(),
		StepCount:  5,
	}

	// Save state
	err = s.saveSessionState(context.Background(), state)
	if err != nil {
		t.Fatalf("saveSessionState() error = %v", err)
	}

	// Verify file exists
	stateFile := filepath.Join(".git", "entire-sessions", "test-session-123.json")
	if _, err := os.Stat(stateFile); os.IsNotExist(err) {
		t.Error("session state file not created")
	}

	// Load state
	loaded, err := s.loadSessionState(context.Background(), "test-session-123")
	if err != nil {
		t.Fatalf("loadSessionState() error = %v", err)
	}
	require.NotNil(t, loaded, "loadSessionState() returned nil")

	if loaded.SessionID != state.SessionID {
		t.Errorf("SessionID = %q, want %q", loaded.SessionID, state.SessionID)
	}
	if loaded.BaseCommit != state.BaseCommit {
		t.Errorf("BaseCommit = %q, want %q", loaded.BaseCommit, state.BaseCommit)
	}
	if loaded.StepCount != state.StepCount {
		t.Errorf("StepCount = %d, want %d", loaded.StepCount, state.StepCount)
	}
}

func TestShadowStrategy_SessionState_LoadNonExistent(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	loaded, err := s.loadSessionState(context.Background(), "nonexistent-session")
	if err != nil {
		t.Errorf("loadSessionState() error = %v, want nil for nonexistent session", err)
	}
	if loaded != nil {
		t.Error("loadSessionState() returned non-nil for nonexistent session")
	}
}

func TestShadowStrategy_ListAllSessionStates(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	// Create a dummy commit to use as a base for the shadow branch
	emptyTreeHash := plumbing.NewHash("4b825dc642cb6eb9a060e54bf8d69288fbee4904")
	dummyCommitHash, err := checkpoint.CreateCommit(context.Background(), repo, emptyTreeHash, plumbing.ZeroHash, "dummy commit", "test", "test@test.com")
	if err != nil {
		t.Fatalf("failed to create dummy commit: %v", err)
	}

	// Create shadow branch for base commit "abc1234" (needs 7 chars for prefix)
	// Use empty worktreeID since this is simulating the main worktree
	shadowBranch := getShadowBranchNameForCommit("abc1234", "")
	refName := plumbing.NewBranchReferenceName(shadowBranch)
	ref := plumbing.NewHashReference(refName, dummyCommitHash)
	if err := repo.Storer.SetReference(ref); err != nil {
		t.Fatalf("failed to create shadow branch: %v", err)
	}

	s := &ManualCommitStrategy{}

	// Save multiple session states (both with same base commit)
	state1 := &SessionState{
		SessionID:  "session-1",
		BaseCommit: "abc1234",
		StartedAt:  time.Now(),
		StepCount:  1,
	}
	state2 := &SessionState{
		SessionID:  "session-2",
		BaseCommit: "abc1234",
		StartedAt:  time.Now(),
		StepCount:  2,
	}

	if err := s.saveSessionState(context.Background(), state1); err != nil {
		t.Fatalf("saveSessionState() error = %v", err)
	}
	if err := s.saveSessionState(context.Background(), state2); err != nil {
		t.Fatalf("saveSessionState() error = %v", err)
	}

	// List all states
	states, err := s.listAllSessionStates(context.Background())
	if err != nil {
		t.Fatalf("listAllSessionStates() error = %v", err)
	}

	if len(states) != 2 {
		t.Errorf("listAllSessionStates() returned %d states, want 2", len(states))
	}
}

// TestShadowStrategy_ListAllSessionStates_CleansUpStaleSessions tests that
// listAllSessionStates cleans up stale sessions whose shadow branch no longer exists.
// Stale sessions include: pre-state-machine sessions (empty phase), IDLE/ENDED sessions
// that were never condensed. Active sessions and sessions with LastCheckpointID are kept.
func TestShadowStrategy_ListAllSessionStates_CleansUpStaleSessions(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	s := &ManualCommitStrategy{}
	now := time.Now()

	// None of these sessions have shadow branches → cleanup logic applies.

	// Session 1: Pre-state-machine session (empty phase, no checkpoint ID)
	// Should be cleaned up.
	staleEmpty := &SessionState{
		SessionID:  "stale-empty-phase",
		BaseCommit: "aaa1111",
		StartedAt:  now.Add(-24 * time.Hour),
		StepCount:  0,
	}

	// Session 2: IDLE session with no checkpoint ID
	// Should be cleaned up.
	staleIdle := &SessionState{
		SessionID:  "stale-idle",
		BaseCommit: "bbb2222",
		StartedAt:  now.Add(-12 * time.Hour),
		StepCount:  3,
		Phase:      "idle",
	}

	// Session 3: ENDED session with no checkpoint ID
	// Should be cleaned up.
	staleEnded := &SessionState{
		SessionID:  "stale-ended",
		BaseCommit: "ccc3333",
		StartedAt:  now.Add(-6 * time.Hour),
		StepCount:  1,
		Phase:      "ended",
	}

	// Session 4: ACTIVE session with no shadow branch (branch not yet created)
	// Should be KEPT (session is still running).
	activeNoShadow := &SessionState{
		SessionID:  "active-no-shadow",
		BaseCommit: "ddd4444",
		StartedAt:  now,
		StepCount:  0,
		Phase:      "active",
	}

	// Session 5: IDLE session with LastCheckpointID set (already condensed)
	// Should be KEPT (for checkpoint ID reuse).
	condensedIdle := &SessionState{
		SessionID:        "condensed-idle",
		BaseCommit:       "eee5555",
		StartedAt:        now.Add(-1 * time.Hour),
		StepCount:        0,
		Phase:            "idle",
		LastCheckpointID: "a1b2c3d4e5f6",
	}

	for _, state := range []*SessionState{staleEmpty, staleIdle, staleEnded, activeNoShadow, condensedIdle} {
		if err := s.saveSessionState(context.Background(), state); err != nil {
			t.Fatalf("saveSessionState(%s) error = %v", state.SessionID, err)
		}
	}

	states, err := s.listAllSessionStates(context.Background())
	if err != nil {
		t.Fatalf("listAllSessionStates() error = %v", err)
	}

	// Only active-no-shadow and condensed-idle should survive
	if len(states) != 2 {
		var ids []string
		for _, st := range states {
			ids = append(ids, st.SessionID)
		}
		t.Fatalf("listAllSessionStates() returned %d states %v, want 2 [active-no-shadow, condensed-idle]", len(states), ids)
	}

	kept := make(map[string]bool)
	for _, st := range states {
		kept[st.SessionID] = true
	}
	if !kept["active-no-shadow"] {
		t.Error("active session without shadow branch should be kept")
	}
	if !kept["condensed-idle"] {
		t.Error("session with LastCheckpointID should be kept")
	}

	// Verify stale sessions were actually cleared from disk
	for _, staleID := range []string{"stale-empty-phase", "stale-idle", "stale-ended"} {
		loaded, err := LoadSessionState(context.Background(), staleID)
		if err != nil {
			t.Errorf("LoadSessionState(%s) error = %v", staleID, err)
		}
		if loaded != nil {
			t.Errorf("stale session %s should have been cleared from disk", staleID)
		}
	}
}

func TestShadowStrategy_FindSessionsForCommit(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	// Create a dummy commit to use as a base for the shadow branches
	emptyTreeHash := plumbing.NewHash("4b825dc642cb6eb9a060e54bf8d69288fbee4904")
	dummyCommitHash, err := checkpoint.CreateCommit(context.Background(), repo, emptyTreeHash, plumbing.ZeroHash, "dummy commit", "test", "test@test.com")
	if err != nil {
		t.Fatalf("failed to create dummy commit: %v", err)
	}

	// Create shadow branches for base commits "abc1234" and "xyz7890" (7 chars)
	// Use empty worktreeID since this is simulating the main worktree
	for _, baseCommit := range []string{"abc1234", "xyz7890"} {
		shadowBranch := getShadowBranchNameForCommit(baseCommit, "")
		refName := plumbing.NewBranchReferenceName(shadowBranch)
		ref := plumbing.NewHashReference(refName, dummyCommitHash)
		if err := repo.Storer.SetReference(ref); err != nil {
			t.Fatalf("failed to create shadow branch for %s: %v", baseCommit, err)
		}
	}

	s := &ManualCommitStrategy{}

	// Save session states with different base commits
	state1 := &SessionState{
		SessionID:  "session-1",
		BaseCommit: "abc1234",
		StartedAt:  time.Now(),
		StepCount:  1,
	}
	state2 := &SessionState{
		SessionID:  "session-2",
		BaseCommit: "abc1234",
		StartedAt:  time.Now(),
		StepCount:  2,
	}
	state3 := &SessionState{
		SessionID:  "session-3",
		BaseCommit: "xyz7890",
		StartedAt:  time.Now(),
		StepCount:  3,
	}

	for _, state := range []*SessionState{state1, state2, state3} {
		if err := s.saveSessionState(context.Background(), state); err != nil {
			t.Fatalf("saveSessionState() error = %v", err)
		}
	}

	// Find sessions for base commit "abc1234"
	matching, err := s.findSessionsForCommit(context.Background(), "abc1234")
	if err != nil {
		t.Fatalf("findSessionsForCommit() error = %v", err)
	}

	if len(matching) != 2 {
		t.Errorf("findSessionsForCommit() returned %d sessions, want 2", len(matching))
	}

	// Find sessions for base commit "xyz7890"
	matching, err = s.findSessionsForCommit(context.Background(), "xyz7890")
	if err != nil {
		t.Fatalf("findSessionsForCommit() error = %v", err)
	}

	if len(matching) != 1 {
		t.Errorf("findSessionsForCommit() returned %d sessions, want 1", len(matching))
	}

	// Find sessions for nonexistent base commit
	matching, err = s.findSessionsForCommit(context.Background(), "nonexistent")
	if err != nil {
		t.Fatalf("findSessionsForCommit() error = %v", err)
	}

	if len(matching) != 0 {
		t.Errorf("findSessionsForCommit() returned %d sessions, want 0", len(matching))
	}
}

func TestShadowStrategy_ClearSessionState(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	state := &SessionState{
		SessionID:  "test-session",
		BaseCommit: "abc123",
		StartedAt:  time.Now(),
		StepCount:  1,
	}

	// Save state
	if err := s.saveSessionState(context.Background(), state); err != nil {
		t.Fatalf("saveSessionState() error = %v", err)
	}

	// Verify it exists
	loaded, loadErr := s.loadSessionState(context.Background(), "test-session")
	if loadErr != nil {
		t.Fatalf("loadSessionState() error = %v", loadErr)
	}
	if loaded == nil {
		t.Fatal("session state not created")
	}

	// Clear state
	if err := s.clearSessionState(context.Background(), "test-session"); err != nil {
		t.Fatalf("clearSessionState() error = %v", err)
	}

	// Verify it's gone
	loaded, loadErr = s.loadSessionState(context.Background(), "test-session")
	if loadErr != nil {
		t.Fatalf("loadSessionState() error = %v", loadErr)
	}
	if loaded != nil {
		t.Error("session state not cleared")
	}
}

func TestShadowStrategy_GetRewindPoints_NoShadowBranch(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Create initial commit
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := worktree.Add("test.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}
	_, err = worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	s := NewManualCommitStrategy()
	points, err := s.GetRewindPoints(context.Background(), 10)
	if err != nil {
		t.Errorf("GetRewindPoints() error = %v", err)
	}
	if len(points) != 0 {
		t.Errorf("GetRewindPoints() returned %d points, want 0", len(points))
	}
}

func TestShadowStrategy_ListSessions_Empty(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	sessions, err := ListSessions(context.Background())
	if err != nil {
		t.Errorf("ListSessions(context.Background()) error = %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("ListSessions(context.Background()) returned %d sessions, want 0", len(sessions))
	}
}

func TestShadowStrategy_GetSession_NotFound(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	_, err = GetSession(context.Background(), "nonexistent")
	if !errors.Is(err, ErrNoSession) {
		t.Errorf("GetSession() error = %v, want ErrNoSession", err)
	}
}

func TestShadowStrategy_GetSessionInfo_NoShadowBranch(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Create initial commit
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := worktree.Add("test.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}
	_, err = worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	s := NewManualCommitStrategy()
	_, err = s.GetSessionInfo(context.Background())
	if !errors.Is(err, ErrNoSession) {
		t.Errorf("GetSessionInfo() error = %v, want ErrNoSession", err)
	}
}

func TestShadowStrategy_CanRewind_CleanRepo(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Create initial commit
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := worktree.Add("test.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}
	_, err = worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	s := NewManualCommitStrategy()
	can, reason, err := s.CanRewind(context.Background())
	if err != nil {
		t.Errorf("CanRewind() error = %v", err)
	}
	if !can {
		t.Errorf("CanRewind() = false, want true (clean repo)")
	}
	if reason != "" {
		t.Errorf("CanRewind() reason = %q, want empty", reason)
	}
}

func TestShadowStrategy_CanRewind_DirtyRepo(t *testing.T) {
	// For shadow, CanRewind always returns true because rewinding
	// replaces local changes with checkpoint contents - that's the expected behavior.
	// Users rewind to undo Claude's changes, which are uncommitted by definition.
	// However, it now returns a warning message with diff stats.
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Create initial commit
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("line1\nline2\nline3\n"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := worktree.Add("test.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}
	_, err = worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	// Make the repo dirty by modifying the file
	if err := os.WriteFile(testFile, []byte("line1\nmodified line2\nline3\nnew line4\n"), 0o644); err != nil {
		t.Fatalf("failed to modify test file: %v", err)
	}

	t.Chdir(dir)

	s := NewManualCommitStrategy()
	can, reason, err := s.CanRewind(context.Background())
	if err != nil {
		t.Errorf("CanRewind() error = %v", err)
	}
	if !can {
		t.Error("CanRewind() = false, want true (shadow always allows rewind)")
	}
	// Now we expect a warning message with diff stats
	if reason == "" {
		t.Error("CanRewind() reason is empty, want warning about uncommitted changes")
	}
	if !strings.Contains(reason, "uncommitted changes will be reverted") {
		t.Errorf("CanRewind() reason = %q, want to contain 'uncommitted changes will be reverted'", reason)
	}
	if !strings.Contains(reason, "test.txt") {
		t.Errorf("CanRewind() reason = %q, want to contain filename 'test.txt'", reason)
	}
}

func TestShadowStrategy_CanRewind_NoRepo(t *testing.T) {
	// Test that CanRewind still returns true even when not in a git repo
	dir := t.TempDir()
	t.Chdir(dir)

	s := NewManualCommitStrategy()
	can, reason, err := s.CanRewind(context.Background())
	if err != nil {
		t.Errorf("CanRewind() error = %v", err)
	}
	if !can {
		t.Error("CanRewind() = false, want true (shadow always allows rewind)")
	}
	if reason != "" {
		t.Errorf("CanRewind() reason = %q, want empty string (no repo, no stats)", reason)
	}
}

func TestShadowStrategy_GetTaskCheckpoint_NotTaskCheckpoint(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	s := NewManualCommitStrategy()

	point := RewindPoint{
		ID:               "abc123",
		IsTaskCheckpoint: false,
	}

	_, err = s.GetTaskCheckpoint(context.Background(), point)
	if !errors.Is(err, ErrNotTaskCheckpoint) {
		t.Errorf("GetTaskCheckpoint() error = %v, want ErrNotTaskCheckpoint", err)
	}
}

func TestShadowStrategy_GetTaskCheckpointTranscript_NotTaskCheckpoint(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	s := NewManualCommitStrategy()

	point := RewindPoint{
		ID:               "abc123",
		IsTaskCheckpoint: false,
	}

	_, err = s.GetTaskCheckpointTranscript(context.Background(), point)
	if !errors.Is(err, ErrNotTaskCheckpoint) {
		t.Errorf("GetTaskCheckpointTranscript() error = %v, want ErrNotTaskCheckpoint", err)
	}
}

func TestGetShadowBranchNameForCommit(t *testing.T) {
	// Hash of empty worktreeID (main worktree) is "e3b0c4"
	mainWorktreeHash := "e3b0c4"

	tests := []struct {
		name       string
		baseCommit string
		worktreeID string
		want       string
	}{
		{
			name:       "short commit main worktree",
			baseCommit: "abc",
			worktreeID: "",
			want:       "entire/abc-" + mainWorktreeHash,
		},
		{
			name:       "7 char commit main worktree",
			baseCommit: "abc1234",
			worktreeID: "",
			want:       "entire/abc1234-" + mainWorktreeHash,
		},
		{
			name:       "long commit main worktree",
			baseCommit: "abc1234567890",
			worktreeID: "",
			want:       "entire/abc1234-" + mainWorktreeHash,
		},
		{
			name:       "with linked worktree",
			baseCommit: "abc1234",
			worktreeID: "feature-branch",
			want:       "entire/abc1234-" + checkpoint.HashWorktreeID("feature-branch"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getShadowBranchNameForCommit(tt.baseCommit, tt.worktreeID)
			if got != tt.want {
				t.Errorf("getShadowBranchNameForCommit(%q, %q) = %q, want %q", tt.baseCommit, tt.worktreeID, got, tt.want)
			}
		})
	}
}

func TestShadowStrategy_PrepareCommitMsg_NoActiveSession(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Create initial commit
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := worktree.Add("test.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}
	_, err = worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	// Create commit message file
	commitMsgFile := filepath.Join(dir, "COMMIT_MSG")
	if err := os.WriteFile(commitMsgFile, []byte("Test commit\n"), 0o644); err != nil {
		t.Fatalf("failed to write commit message file: %v", err)
	}

	s := NewManualCommitStrategy()
	prepErr := s.PrepareCommitMsg(context.Background(), commitMsgFile, "")
	if prepErr != nil {
		t.Errorf("PrepareCommitMsg() error = %v", prepErr)
	}

	// Message should be unchanged (no session)
	content, err := os.ReadFile(commitMsgFile)
	if err != nil {
		t.Fatalf("failed to read commit message file: %v", err)
	}
	if string(content) != "Test commit\n" {
		t.Errorf("PrepareCommitMsg() modified message when no session active: %q", content)
	}
}

func TestShadowStrategy_PrepareCommitMsg_SkipSources(t *testing.T) {
	// Tests that merge, squash, and commit sources are skipped
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	commitMsgFile := filepath.Join(dir, "COMMIT_MSG")
	originalMsg := "Merge branch 'feature'\n"

	s := NewManualCommitStrategy()

	skipSources := []string{"merge", "squash", "commit"}
	for _, source := range skipSources {
		t.Run(source, func(t *testing.T) {
			if err := os.WriteFile(commitMsgFile, []byte(originalMsg), 0o644); err != nil {
				t.Fatalf("failed to write commit message file: %v", err)
			}

			prepErr := s.PrepareCommitMsg(context.Background(), commitMsgFile, source)
			if prepErr != nil {
				t.Errorf("PrepareCommitMsg() error = %v", prepErr)
			}

			// Message should be unchanged for these sources
			content, readErr := os.ReadFile(commitMsgFile)
			if readErr != nil {
				t.Fatalf("failed to read commit message file: %v", readErr)
			}
			if string(content) != originalMsg {
				t.Errorf("PrepareCommitMsg(source=%q) modified message: got %q, want %q",
					source, content, originalMsg)
			}
		})
	}
}

func TestShadowStrategy_PrepareCommitMsg_SkipsSessionWhenContentCheckFails(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	t.Setenv("ENTIRE_TEST_TTY", "1")

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}

	err = s.InitializeSession(context.Background(), "test-session-corrupt-shadow", agent.AgentTypeClaudeCode, "", "", "")
	require.NoError(t, err)

	state, err := s.loadSessionState(context.Background(), "test-session-corrupt-shadow")
	require.NoError(t, err)
	require.NotNil(t, state)

	shadowBranch := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	corruptRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(shadowBranch), plumbing.ZeroHash)
	require.NoError(t, repo.Storer.SetReference(corruptRef))

	commitMsgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	originalMsg := "Test commit\n"
	require.NoError(t, os.WriteFile(commitMsgFile, []byte(originalMsg), 0o644))

	err = s.PrepareCommitMsg(context.Background(), commitMsgFile, "")
	require.NoError(t, err)

	content, err := os.ReadFile(commitMsgFile)
	require.NoError(t, err)

	_, found := trailers.ParseCheckpoint(string(content))
	require.False(t, found, "corrupt session state should not add a dangling checkpoint trailer")
	require.Equal(t, originalMsg, string(content))
}

func TestAddCheckpointTrailer_NoComment(t *testing.T) {
	// Test that addCheckpointTrailer adds trailer without any comment lines
	message := "Test commit message\n" //nolint:goconst // already present in codebase

	result := addCheckpointTrailer(message, testTrailerCheckpointID)

	// Should contain the trailer
	if !strings.Contains(result, trailers.CheckpointTrailerKey+": "+testTrailerCheckpointID.String()) {
		t.Errorf("addCheckpointTrailer() missing trailer, got: %q", result)
	}

	// Should NOT contain comment lines
	if strings.Contains(result, "# Remove the Entire-Checkpoint") {
		t.Errorf("addCheckpointTrailer() should not contain comment, got: %q", result)
	}
}

func TestAddCheckpointTrailerWithComment_HasComment(t *testing.T) {
	// Test that addCheckpointTrailerWithComment includes the explanatory comment
	message := "Test commit message\n"

	result := addCheckpointTrailerWithComment(message, testTrailerCheckpointID, "Claude Code", "add password hashing")

	// Should contain the trailer
	if !strings.Contains(result, trailers.CheckpointTrailerKey+": "+testTrailerCheckpointID.String()) {
		t.Errorf("addCheckpointTrailerWithComment() missing trailer, got: %q", result)
	}

	// Should contain comment lines with agent name (before prompt)
	if !strings.Contains(result, "# Remove the Entire-Checkpoint") {
		t.Errorf("addCheckpointTrailerWithComment() should contain comment, got: %q", result)
	}
	if !strings.Contains(result, "Claude Code session context") {
		t.Errorf("addCheckpointTrailerWithComment() should contain agent name in comment, got: %q", result)
	}

	// Should contain prompt line (after removal comment)
	if !strings.Contains(result, "# Last Prompt: add password hashing") {
		t.Errorf("addCheckpointTrailerWithComment() should contain prompt, got: %q", result)
	}

	// Verify order: Remove comment should come before Last Prompt
	removeIdx := strings.Index(result, "# Remove the Entire-Checkpoint")
	promptIdx := strings.Index(result, "# Last Prompt:")
	if promptIdx < removeIdx {
		t.Errorf("addCheckpointTrailerWithComment() prompt should come after remove comment, got: %q", result)
	}
}

func TestAddCheckpointTrailerWithComment_NoPrompt(t *testing.T) {
	// Test that addCheckpointTrailerWithComment works without a prompt
	message := "Test commit message\n"

	result := addCheckpointTrailerWithComment(message, testTrailerCheckpointID, "Claude Code", "")

	// Should contain the trailer
	if !strings.Contains(result, trailers.CheckpointTrailerKey+": "+testTrailerCheckpointID.String()) {
		t.Errorf("addCheckpointTrailerWithComment() missing trailer, got: %q", result)
	}

	// Should NOT contain prompt line when prompt is empty
	if strings.Contains(result, "# Last Prompt:") {
		t.Errorf("addCheckpointTrailerWithComment() should not contain prompt line when empty, got: %q", result)
	}

	// Should still contain the removal comment
	if !strings.Contains(result, "# Remove the Entire-Checkpoint") {
		t.Errorf("addCheckpointTrailerWithComment() should contain comment, got: %q", result)
	}
}

func TestAddCheckpointTrailer_ConventionalCommitSubject(t *testing.T) {
	t.Parallel()

	// Regression: single-line conventional commit subjects like "docs: Add foo"
	// contain ": " which falsely triggered the "already has trailers" detection,
	// causing the trailer to be appended without a blank line separator.
	tests := []struct {
		name    string
		message string
	}{
		{
			name:    "conventional commit docs",
			message: "docs: Add red.md with information about the color red\n",
		},
		{
			name:    "conventional commit feat",
			message: "feat: Add new login flow\n",
		},
		{
			name:    "conventional commit fix with scope",
			message: "fix(auth): Resolve token expiry issue\n",
		},
		{
			name:    "single line no newline",
			message: "docs: Add something",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := addCheckpointTrailer(tt.message, testTrailerCheckpointID)

			// The trailer must be separated from the subject by a blank line
			if !strings.Contains(result, "\n\n"+trailers.CheckpointTrailerKey+":") {
				t.Errorf("addCheckpointTrailer() trailer not separated by blank line from subject.\ngot: %q", result)
			}
		})
	}
}

func TestAddCheckpointTrailer_ExistingTrailers(t *testing.T) {
	t.Parallel()

	// When a message already has trailers (in a separate paragraph), the
	// new trailer should be appended directly (no extra blank line).
	message := "feat: Add login\n\nSigned-off-by: Test User <test@example.com>\n"
	result := addCheckpointTrailer(message, testTrailerCheckpointID)

	// Should NOT add a double blank line before our trailer
	if strings.Contains(result, "\n\n"+trailers.CheckpointTrailerKey) {
		t.Errorf("addCheckpointTrailer() added extra blank line before existing trailer block.\ngot: %q", result)
	}

	// Should contain both trailers
	if !strings.Contains(result, "Signed-off-by:") {
		t.Errorf("addCheckpointTrailer() lost existing trailer.\ngot: %q", result)
	}
	if !strings.Contains(result, trailers.CheckpointTrailerKey+":") {
		t.Errorf("addCheckpointTrailer() missing our trailer.\ngot: %q", result)
	}
}

func TestShadowStrategy_GetCheckpointLog_WithCheckpointID(t *testing.T) {
	// This test verifies that GetCheckpointLog correctly uses the checkpoint ID
	// to look up the log. Since getCheckpointLog requires a full git setup
	// with entire/checkpoints/v1 branch, we test the lookup logic by checking error behavior.

	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	s := NewManualCommitStrategy()

	// Checkpoint with checkpoint ID (12 hex chars)
	checkpoint := Checkpoint{
		CheckpointID: "a1b2c3d4e5f6",
		Message:      "Checkpoint: a1b2c3d4e5f6",
		Timestamp:    time.Now(),
	}

	// This should attempt to call getCheckpointLog (which will fail because
	// there's no entire/checkpoints/v1 branch), but the important thing is it uses
	// the checkpoint ID to look up metadata
	_, err = s.GetCheckpointLog(context.Background(), checkpoint)
	if err == nil {
		t.Error("GetCheckpointLog() expected error (no sessions branch), got nil")
	}
	// The error should be about sessions branch, not about parsing
	if err != nil && err.Error() != "sessions branch not found" {
		t.Logf("GetCheckpointLog() error = %v (expected sessions branch error)", err)
	}
}

func TestShadowStrategy_GetCheckpointLog_NoCheckpointID(t *testing.T) {
	// Test that checkpoints without checkpoint ID return ErrNoMetadata
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	s := NewManualCommitStrategy()

	// Checkpoint without checkpoint ID
	checkpoint := Checkpoint{
		CheckpointID: "",
		Message:      "Some other message",
		Timestamp:    time.Now(),
	}

	// This should return ErrNoMetadata since there's no checkpoint ID
	_, err = s.GetCheckpointLog(context.Background(), checkpoint)
	if err == nil {
		t.Error("GetCheckpointLog() expected error for missing checkpoint ID, got nil")
	}
	if !errors.Is(err, ErrNoMetadata) {
		t.Errorf("GetCheckpointLog() expected ErrNoMetadata, got %v", err)
	}
}

func TestShadowStrategy_FilesTouched_OnlyModifiedFiles(t *testing.T) {
	// This test verifies that files_touched only contains files that were actually
	// modified during the session, not ALL files in the repository.
	//
	// The fix tracks files in SessionState.FilesTouched as they are modified,
	// rather than collecting all files from the shadow branch tree.

	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Create initial commit with multiple pre-existing files
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create 3 pre-existing files that should NOT be in files_touched
	preExistingFiles := []string{"existing1.txt", "existing2.txt", "existing3.txt"}
	for _, f := range preExistingFiles {
		filePath := filepath.Join(dir, f)
		if err := os.WriteFile(filePath, []byte("original content of "+f), 0o644); err != nil {
			t.Fatalf("failed to write file %s: %v", f, err)
		}
		if _, err := worktree.Add(f); err != nil {
			t.Fatalf("failed to add file %s: %v", f, err)
		}
	}

	_, err = worktree.Commit("Initial commit with pre-existing files", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	s := &ManualCommitStrategy{}
	sessionID := "2025-01-15-test-session-123"

	// Create metadata directory with a transcript
	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	if err := os.MkdirAll(metadataDirAbs, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}

	// Write transcript file (minimal valid JSONL)
	transcript := `{"type":"human","message":{"content":"modify existing1.txt"}}
{"type":"assistant","message":{"content":"I'll modify existing1.txt for you."}}
`
	if err := os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(transcript), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// First checkpoint using SaveStep - captures ALL working directory files
	// (for rewind purposes), but tracks only modified files in FilesTouched
	err = s.SaveStep(context.Background(), StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{}, // No files modified yet
		NewFiles:       []string{},
		DeletedFiles:   []string{},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Checkpoint 1",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	})
	if err != nil {
		t.Fatalf("SaveStep() error = %v", err)
	}

	// Now simulate a second checkpoint where ONLY existing1.txt is modified
	// (but NOT existing2.txt or existing3.txt)
	modifiedContent := []byte("MODIFIED content of existing1.txt")
	if err := os.WriteFile(filepath.Join(dir, "existing1.txt"), modifiedContent, 0o644); err != nil {
		t.Fatalf("failed to modify existing1.txt: %v", err)
	}

	// Second checkpoint using SaveStep - only modified file should be tracked
	err = s.SaveStep(context.Background(), StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{"existing1.txt"}, // Only this file was modified
		NewFiles:       []string{},
		DeletedFiles:   []string{},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Checkpoint 2",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	})
	if err != nil {
		t.Fatalf("SaveStep() error = %v", err)
	}

	// Load session state to verify FilesTouched
	state, err := s.loadSessionState(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("loadSessionState() error = %v", err)
	}

	// Now condense the session
	checkpointID := id.MustCheckpointID("a1b2c3d4e5f6")
	result, err := s.CondenseSession(context.Background(), repo, checkpointID, state, nil)
	if err != nil {
		t.Fatalf("CondenseSession() error = %v", err)
	}

	// Verify that files_touched only contains the file that was actually modified
	expectedFilesTouched := []string{"existing1.txt"}

	// Check what we actually got
	if len(result.FilesTouched) != len(expectedFilesTouched) {
		t.Errorf("FilesTouched contains %d files, want %d.\nGot: %v\nWant: %v",
			len(result.FilesTouched), len(expectedFilesTouched),
			result.FilesTouched, expectedFilesTouched)
	}

	// Verify the exact content
	filesTouchedMap := make(map[string]bool)
	for _, f := range result.FilesTouched {
		filesTouchedMap[f] = true
	}

	// Check that ONLY the modified file is in files_touched
	for _, expected := range expectedFilesTouched {
		if !filesTouchedMap[expected] {
			t.Errorf("Expected file %q to be in files_touched, but it was not. Got: %v", expected, result.FilesTouched)
		}
	}

	// Check that pre-existing unmodified files are NOT in files_touched
	unmodifiedFiles := []string{"existing2.txt", "existing3.txt"}
	for _, unmodified := range unmodifiedFiles {
		if filesTouchedMap[unmodified] {
			t.Errorf("File %q should NOT be in files_touched (it was not modified during the session), but it was included. Got: %v",
				unmodified, result.FilesTouched)
		}
	}
}

// TestDeleteShadowBranch verifies that deleteShadowBranch correctly deletes a shadow branch.
func TestDeleteShadowBranch(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init repo: %v", err)
	}

	t.Chdir(dir)

	// Create a dummy commit to use as branch target
	emptyTreeHash := plumbing.NewHash("4b825dc642cb6eb9a060e54bf8d69288fbee4904")
	dummyCommitHash, err := checkpoint.CreateCommit(context.Background(), repo, emptyTreeHash, plumbing.ZeroHash, "dummy commit", "test", "test@test.com")
	if err != nil {
		t.Fatalf("failed to create dummy commit: %v", err)
	}

	// Create a shadow branch
	shadowBranchName := "entire/abc1234"
	refName := plumbing.NewBranchReferenceName(shadowBranchName)
	ref := plumbing.NewHashReference(refName, dummyCommitHash)
	if err := repo.Storer.SetReference(ref); err != nil {
		t.Fatalf("failed to create shadow branch: %v", err)
	}

	// Verify branch exists
	_, err = repo.Reference(refName, true)
	if err != nil {
		t.Fatalf("shadow branch should exist: %v", err)
	}

	// Delete the shadow branch
	err = deleteShadowBranch(context.Background(), repo, shadowBranchName)
	if err != nil {
		t.Fatalf("deleteShadowBranch() error = %v", err)
	}

	// Verify branch is deleted
	_, err = repo.Reference(refName, true)
	if err == nil {
		t.Error("shadow branch should be deleted, but still exists")
	}
}

// TestDeleteShadowBranch_NonExistent verifies that deleting a non-existent branch is idempotent.
func TestDeleteShadowBranch_NonExistent(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init repo: %v", err)
	}

	t.Chdir(dir)

	// Try to delete a branch that doesn't exist - should not error
	err = deleteShadowBranch(context.Background(), repo, "entire/nonexistent")
	if err != nil {
		t.Errorf("deleteShadowBranch() for non-existent branch should not error, got: %v", err)
	}
}

// TestSessionState_LastCheckpointID verifies that LastCheckpointID is persisted correctly.
func TestSessionState_LastCheckpointID(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	// Create session state with LastCheckpointID
	state := &SessionState{
		SessionID:        "test-session-123",
		BaseCommit:       "abc123def456",
		StartedAt:        time.Now(),
		StepCount:        5,
		LastCheckpointID: "a1b2c3d4e5f6",
	}

	// Save state
	err = s.saveSessionState(context.Background(), state)
	if err != nil {
		t.Fatalf("saveSessionState() error = %v", err)
	}

	// Load state and verify LastCheckpointID
	loaded, err := s.loadSessionState(context.Background(), "test-session-123")
	if err != nil {
		t.Fatalf("loadSessionState() error = %v", err)
	}
	require.NotNil(t, loaded, "loadSessionState() returned nil")

	if loaded.LastCheckpointID != state.LastCheckpointID {
		t.Errorf("LastCheckpointID = %q, want %q", loaded.LastCheckpointID, state.LastCheckpointID)
	}
}

// TestSessionState_TokenUsagePersistence verifies that token usage fields are persisted correctly
// across session state save/load cycles. This is critical for tracking token usage in the
// manual-commit strategy where session state is persisted to disk between checkpoints.
func TestSessionState_TokenUsagePersistence(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	// Create session state with token usage fields
	state := &SessionState{
		SessionID:                   "test-session-token-usage",
		BaseCommit:                  "abc123def456",
		StartedAt:                   time.Now(),
		StepCount:                   5,
		CheckpointTranscriptStart:   42,
		TranscriptIdentifierAtStart: "test-uuid-abc123",
		TokenUsage: &agent.TokenUsage{
			InputTokens:         1000,
			CacheCreationTokens: 200,
			CacheReadTokens:     300,
			OutputTokens:        500,
			APICallCount:        5,
		},
	}

	// Save state
	err = s.saveSessionState(context.Background(), state)
	if err != nil {
		t.Fatalf("saveSessionState() error = %v", err)
	}

	// Load state and verify token usage fields are persisted
	loaded, err := s.loadSessionState(context.Background(), "test-session-token-usage")
	if err != nil {
		t.Fatalf("loadSessionState() error = %v", err)
	}
	require.NotNil(t, loaded, "loadSessionState() returned nil")

	// Verify CheckpointTranscriptStart
	if loaded.CheckpointTranscriptStart != state.CheckpointTranscriptStart {
		t.Errorf("CheckpointTranscriptStart = %d, want %d", loaded.CheckpointTranscriptStart, state.CheckpointTranscriptStart)
	}

	// Verify TranscriptIdentifierAtStart
	if loaded.TranscriptIdentifierAtStart != state.TranscriptIdentifierAtStart {
		t.Errorf("TranscriptIdentifierAtStart = %q, want %q", loaded.TranscriptIdentifierAtStart, state.TranscriptIdentifierAtStart)
	}

	// Verify TokenUsage
	if loaded.TokenUsage == nil {
		t.Fatal("TokenUsage should be persisted, got nil")
	}
	if loaded.TokenUsage.InputTokens != state.TokenUsage.InputTokens {
		t.Errorf("TokenUsage.InputTokens = %d, want %d", loaded.TokenUsage.InputTokens, state.TokenUsage.InputTokens)
	}
	if loaded.TokenUsage.CacheCreationTokens != state.TokenUsage.CacheCreationTokens {
		t.Errorf("TokenUsage.CacheCreationTokens = %d, want %d", loaded.TokenUsage.CacheCreationTokens, state.TokenUsage.CacheCreationTokens)
	}
	if loaded.TokenUsage.CacheReadTokens != state.TokenUsage.CacheReadTokens {
		t.Errorf("TokenUsage.CacheReadTokens = %d, want %d", loaded.TokenUsage.CacheReadTokens, state.TokenUsage.CacheReadTokens)
	}
	if loaded.TokenUsage.OutputTokens != state.TokenUsage.OutputTokens {
		t.Errorf("TokenUsage.OutputTokens = %d, want %d", loaded.TokenUsage.OutputTokens, state.TokenUsage.OutputTokens)
	}
	if loaded.TokenUsage.APICallCount != state.TokenUsage.APICallCount {
		t.Errorf("TokenUsage.APICallCount = %d, want %d", loaded.TokenUsage.APICallCount, state.TokenUsage.APICallCount)
	}
}

// TestShadowStrategy_PrepareCommitMsg_ReusesLastCheckpointID verifies that PrepareCommitMsg
// reuses the LastCheckpointID when there's no new content to condense.
func TestShadowStrategy_PrepareCommitMsg_ReusesLastCheckpointID(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Create initial commit
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := worktree.Add("test.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}
	initialCommit, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	// Create session state with LastCheckpointID but no new content
	// (simulating state after first commit with condensation)
	state := &SessionState{
		SessionID:                 "test-session",
		BaseCommit:                initialCommit.String(),
		WorktreePath:              dir,
		StartedAt:                 time.Now(),
		StepCount:                 1,
		CheckpointTranscriptStart: 10, // Already condensed
		LastCheckpointID:          testTrailerCheckpointID,
	}
	if err := s.saveSessionState(context.Background(), state); err != nil {
		t.Fatalf("saveSessionState() error = %v", err)
	}

	// Note: We can't fully test PrepareCommitMsg without setting up a shadow branch
	// with transcript, but we can verify the session state has LastCheckpointID set
	// The actual behavior is tested through integration tests

	// Verify the state was saved correctly
	loaded, err := s.loadSessionState(context.Background(), "test-session")
	if err != nil {
		t.Fatalf("loadSessionState() error = %v", err)
	}
	if loaded.LastCheckpointID != testTrailerCheckpointID {
		t.Errorf("LastCheckpointID = %q, want %q", loaded.LastCheckpointID, testTrailerCheckpointID)
	}
}

func TestParsePostRewritePairs(t *testing.T) {
	pairs, err := parsePostRewritePairs(strings.NewReader("oldsha newsha\n\nold2 new2\n"))
	if err != nil {
		t.Fatalf("parsePostRewritePairs() error = %v", err)
	}
	if len(pairs) != 2 {
		t.Fatalf("len(pairs) = %d, want 2", len(pairs))
	}
	if pairs[0].OldSHA != "oldsha" || pairs[0].NewSHA != "newsha" {
		t.Fatalf("pairs[0] = %+v, want oldsha->newsha", pairs[0])
	}
	if pairs[1].OldSHA != "old2" || pairs[1].NewSHA != "new2" {
		t.Fatalf("pairs[1] = %+v, want old2->new2", pairs[1])
	}
}

func TestParsePostRewritePairs_AllowsOptionalExtraField(t *testing.T) {
	pairs, err := parsePostRewritePairs(strings.NewReader("oldsha newsha extra-info\n"))
	if err != nil {
		t.Fatalf("parsePostRewritePairs() error = %v", err)
	}
	if len(pairs) != 1 {
		t.Fatalf("len(pairs) = %d, want 1", len(pairs))
	}
	if pairs[0].OldSHA != "oldsha" || pairs[0].NewSHA != "newsha" {
		t.Fatalf("pairs[0] = %+v, want oldsha->newsha", pairs[0])
	}
}

func TestParsePostRewritePairs_InvalidLine(t *testing.T) {
	_, err := parsePostRewritePairs(strings.NewReader("missing-second-column\n"))
	if err == nil {
		t.Fatal("parsePostRewritePairs() error = nil, want error")
	}
}

func TestShadowStrategy_PostRewrite_RemapsMatchingSessionInWorktree(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	oldSHA := strings.Repeat("a", 40)
	newSHA := strings.Repeat("b", 40)
	worktreePath, err := paths.WorktreeRoot(context.Background())
	if err != nil {
		t.Fatalf("WorktreeRoot() error = %v", err)
	}

	s := &ManualCommitStrategy{}
	state := &SessionState{
		SessionID:             "session-1",
		BaseCommit:            oldSHA,
		AttributionBaseCommit: oldSHA,
		WorktreePath:          worktreePath,
		StartedAt:             time.Now(),
		LastCheckpointID:      testTrailerCheckpointID,
	}
	if err := s.saveSessionState(context.Background(), state); err != nil {
		t.Fatalf("saveSessionState() error = %v", err)
	}

	if err := s.PostRewrite(context.Background(), "amend", strings.NewReader(oldSHA+" "+newSHA+"\n")); err != nil {
		t.Fatalf("PostRewrite() error = %v", err)
	}

	loaded, err := s.loadSessionState(context.Background(), state.SessionID)
	if err != nil {
		t.Fatalf("loadSessionState() error = %v", err)
	}
	if loaded.BaseCommit != newSHA {
		t.Fatalf("BaseCommit = %q, want %q", loaded.BaseCommit, newSHA)
	}
	if loaded.AttributionBaseCommit != newSHA {
		t.Fatalf("AttributionBaseCommit = %q, want %q", loaded.AttributionBaseCommit, newSHA)
	}
	if loaded.LastCheckpointID != testTrailerCheckpointID {
		t.Fatalf("LastCheckpointID = %q, want %q", loaded.LastCheckpointID, testTrailerCheckpointID)
	}
}

func TestShadowStrategy_PostRewrite_MigratesExistingShadowBranch(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "tracked.txt", "one\n")
	testutil.GitAdd(t, dir, "tracked.txt")
	testutil.GitCommit(t, dir, "initial")
	t.Chdir(dir)

	repo, err := OpenRepository(context.Background())
	if err != nil {
		t.Fatalf("OpenRepository() error = %v", err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("Head() error = %v", err)
	}
	oldBaseCommit := head.Hash().String()

	testutil.WriteFile(t, dir, "tracked.txt", "two\n")
	testutil.GitAdd(t, dir, "tracked.txt")
	testutil.GitCommit(t, dir, "second")
	head, err = repo.Head()
	if err != nil {
		t.Fatalf("Head() after second commit error = %v", err)
	}
	newBaseCommit := head.Hash().String()

	worktreePath, err := paths.WorktreeRoot(context.Background())
	if err != nil {
		t.Fatalf("WorktreeRoot() error = %v", err)
	}
	worktreeID, err := paths.GetWorktreeID(worktreePath)
	if err != nil {
		t.Fatalf("GetWorktreeID() error = %v", err)
	}

	oldShadowBranch := checkpoint.ShadowBranchNameForCommit(oldBaseCommit, worktreeID)
	newShadowBranch := checkpoint.ShadowBranchNameForCommit(newBaseCommit, worktreeID)
	oldShadowRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(oldShadowBranch), plumbing.NewHash(oldBaseCommit))
	if err := repo.Storer.SetReference(oldShadowRef); err != nil {
		t.Fatalf("SetReference(old shadow) error = %v", err)
	}

	s := &ManualCommitStrategy{}
	state := &SessionState{
		SessionID:             "session-1",
		BaseCommit:            oldBaseCommit,
		AttributionBaseCommit: oldBaseCommit,
		WorktreePath:          worktreePath,
		WorktreeID:            worktreeID,
		StartedAt:             time.Now(),
		LastCheckpointID:      testTrailerCheckpointID,
	}
	if err := s.saveSessionState(context.Background(), state); err != nil {
		t.Fatalf("saveSessionState() error = %v", err)
	}

	if err := s.PostRewrite(context.Background(), "amend", strings.NewReader(oldBaseCommit+" "+newBaseCommit+" extra\n")); err != nil {
		t.Fatalf("PostRewrite() error = %v", err)
	}

	loaded, err := s.loadSessionState(context.Background(), state.SessionID)
	if err != nil {
		t.Fatalf("loadSessionState() error = %v", err)
	}
	if loaded.BaseCommit != newBaseCommit {
		t.Fatalf("BaseCommit = %q, want %q", loaded.BaseCommit, newBaseCommit)
	}
	if loaded.AttributionBaseCommit != oldBaseCommit {
		t.Fatalf("AttributionBaseCommit = %q, want original %q when shadow branch migrates", loaded.AttributionBaseCommit, oldBaseCommit)
	}
	if !referenceExists(t, repo, plumbing.NewBranchReferenceName(newShadowBranch)) {
		t.Fatalf("expected migrated shadow branch %q to exist", newShadowBranch)
	}
	if referenceExists(t, repo, plumbing.NewBranchReferenceName(oldShadowBranch)) {
		t.Fatalf("expected old shadow branch %q to be removed", oldShadowBranch)
	}
}

func TestShadowStrategy_MigrateAndPersistIfNeeded_PersistsBaseCommitWithoutShadowBranch(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "tracked.txt", "one\n")
	testutil.GitAdd(t, dir, "tracked.txt")
	testutil.GitCommit(t, dir, "initial")
	t.Chdir(dir)

	repo, err := OpenRepository(context.Background())
	if err != nil {
		t.Fatalf("OpenRepository() error = %v", err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("Head() error = %v", err)
	}
	oldBaseCommit := head.Hash().String()

	testutil.WriteFile(t, dir, "tracked.txt", "two\n")
	testutil.GitAdd(t, dir, "tracked.txt")
	testutil.GitCommit(t, dir, "second")
	head, err = repo.Head()
	if err != nil {
		t.Fatalf("Head() after second commit error = %v", err)
	}
	newBaseCommit := head.Hash().String()

	worktreePath, err := paths.WorktreeRoot(context.Background())
	if err != nil {
		t.Fatalf("WorktreeRoot() error = %v", err)
	}

	s := &ManualCommitStrategy{}
	state := &SessionState{
		SessionID:             "session-1",
		BaseCommit:            oldBaseCommit,
		AttributionBaseCommit: oldBaseCommit,
		WorktreePath:          worktreePath,
		StartedAt:             time.Now(),
		LastCheckpointID:      testTrailerCheckpointID,
	}
	if err := s.saveSessionState(context.Background(), state); err != nil {
		t.Fatalf("saveSessionState() error = %v", err)
	}

	if err := s.migrateAndPersistIfNeeded(context.Background(), repo, state); err != nil {
		t.Fatalf("migrateAndPersistIfNeeded() error = %v", err)
	}

	loaded, err := s.loadSessionState(context.Background(), state.SessionID)
	if err != nil {
		t.Fatalf("loadSessionState() error = %v", err)
	}
	if loaded.BaseCommit != newBaseCommit {
		t.Fatalf("BaseCommit = %q, want %q", loaded.BaseCommit, newBaseCommit)
	}
}

func TestShadowStrategy_PostRewrite_DoesNotTouchOtherWorktrees(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	oldSHA := strings.Repeat("a", 40)
	newSHA := strings.Repeat("b", 40)

	s := &ManualCommitStrategy{}
	other := &SessionState{
		SessionID:             "other-worktree",
		BaseCommit:            oldSHA,
		AttributionBaseCommit: oldSHA,
		WorktreePath:          filepath.Join(dir, "other"),
		StartedAt:             time.Now(),
		LastCheckpointID:      testTrailerCheckpointID,
	}
	if err := s.saveSessionState(context.Background(), other); err != nil {
		t.Fatalf("saveSessionState() error = %v", err)
	}

	if err := s.PostRewrite(context.Background(), "amend", strings.NewReader(oldSHA+" "+newSHA+"\n")); err != nil {
		t.Fatalf("PostRewrite() error = %v", err)
	}

	loaded, err := s.loadSessionState(context.Background(), other.SessionID)
	if err != nil {
		t.Fatalf("loadSessionState() error = %v", err)
	}
	if loaded.BaseCommit != oldSHA {
		t.Fatalf("BaseCommit = %q, want %q", loaded.BaseCommit, oldSHA)
	}
	if loaded.AttributionBaseCommit != oldSHA {
		t.Fatalf("AttributionBaseCommit = %q, want %q", loaded.AttributionBaseCommit, oldSHA)
	}
	if loaded.LastCheckpointID != testTrailerCheckpointID {
		t.Fatalf("LastCheckpointID = %q, want %q", loaded.LastCheckpointID, testTrailerCheckpointID)
	}
}

func referenceExists(t *testing.T, repo *git.Repository, refName plumbing.ReferenceName) bool {
	t.Helper()

	_, err := repo.Reference(refName, true)
	return err == nil
}

// TestShadowStrategy_CondenseSession_EphemeralBranchTrailer verifies that checkpoint commits
// on the entire/checkpoints/v1 branch include the Ephemeral-branch trailer indicating which shadow
// branch the checkpoint originated from.
func TestShadowStrategy_CondenseSession_EphemeralBranchTrailer(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init repo: %v", err)
	}

	// Create initial commit with a file
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	initialFile := filepath.Join(dir, "initial.txt")
	if err := os.WriteFile(initialFile, []byte("initial content"), 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	if _, err := worktree.Add("initial.txt"); err != nil {
		t.Fatalf("failed to stage file: %v", err)
	}

	_, err = worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	s := &ManualCommitStrategy{}
	sessionID := "2025-01-15-test-session-ephemeral"

	// Create metadata directory with transcript
	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	if err := os.MkdirAll(metadataDirAbs, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(testTranscriptPromptResponse), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Use SaveStep to create a checkpoint (this creates the shadow branch)
	err = s.SaveStep(context.Background(), StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{},
		NewFiles:       []string{},
		DeletedFiles:   []string{},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Checkpoint 1",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	})
	if err != nil {
		t.Fatalf("SaveStep() error = %v", err)
	}

	// Load session state
	state, err := s.loadSessionState(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("loadSessionState() error = %v", err)
	}

	// Condense the session
	checkpointID := id.MustCheckpointID("a1b2c3d4e5f6")
	_, err = s.CondenseSession(context.Background(), repo, checkpointID, state, nil)
	if err != nil {
		t.Fatalf("CondenseSession() error = %v", err)
	}

	// Get the sessions branch commit and verify the Ephemeral-branch trailer
	sessionsRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("failed to get sessions branch reference: %v", err)
	}

	sessionsCommit, err := repo.CommitObject(sessionsRef.Hash())
	if err != nil {
		t.Fatalf("failed to get sessions commit: %v", err)
	}

	// Verify the commit message contains the Ephemeral-branch trailer
	shadowBranchName := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	expectedTrailer := "Ephemeral-branch: " + shadowBranchName
	if !strings.Contains(sessionsCommit.Message, expectedTrailer) {
		t.Errorf("sessions branch commit should contain %q trailer, got message:\n%s", expectedTrailer, sessionsCommit.Message)
	}
}

// TestSaveStep_EmptyBaseCommit_Recovery verifies that SaveStep recovers gracefully
// when a session state exists with empty BaseCommit (can happen from concurrent warning state).
func TestSaveStep_EmptyBaseCommit_Recovery(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Create initial commit
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test content"), 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	if _, err := worktree.Add("test.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}
	_, err = worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	s := &ManualCommitStrategy{}
	sessionID := "2025-01-15-empty-basecommit-test"

	// Create a partial session state with empty BaseCommit
	// (simulates a partial session state with empty BaseCommit)
	partialState := &SessionState{
		SessionID:  sessionID,
		BaseCommit: "", // Empty! This is the bug scenario
		StartedAt:  time.Now(),
	}
	if err := s.saveSessionState(context.Background(), partialState); err != nil {
		t.Fatalf("failed to save partial state: %v", err)
	}

	// Create metadata directory
	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	if err := os.MkdirAll(metadataDirAbs, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}
	transcript := `{"type":"human","message":{"content":"test"}}` + "\n"
	if err := os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(transcript), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// SaveStep should recover by re-initializing the session state
	err = s.SaveStep(context.Background(), StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{},
		NewFiles:       []string{},
		DeletedFiles:   []string{},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Test checkpoint",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	})
	if err != nil {
		t.Fatalf("SaveStep() should recover from empty BaseCommit, got error: %v", err)
	}

	// Verify session state now has a valid BaseCommit
	loaded, err := s.loadSessionState(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("failed to load session state: %v", err)
	}
	if loaded.BaseCommit == "" {
		t.Error("BaseCommit should be populated after recovery")
	}
	if loaded.StepCount != 1 {
		t.Errorf("StepCount = %d, want 1", loaded.StepCount)
	}
}

// TestSaveStep_UsesCtxAgentType_WhenNoSessionState tests that SaveStep uses
// ctx.AgentType when no session state exists.
func TestSaveStep_UsesCtxAgentType_WhenNoSessionState(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test content"), 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	if _, err := worktree.Add("test.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}
	if _, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	}); err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	s := &ManualCommitStrategy{}
	sessionID := "2026-02-06-agent-type-test"

	// NO session state exists (simulates InitializeSession failure)
	// SaveStep should use ctx.AgentType

	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	if err := os.MkdirAll(metadataDirAbs, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}
	transcript := `{"type":"human","message":{"content":"test"}}` + "\n"
	if err := os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(transcript), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	err = s.SaveStep(context.Background(), StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{},
		NewFiles:       []string{},
		DeletedFiles:   []string{},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Test checkpoint",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
		AgentType:      agent.AgentTypeClaudeCode,
	})
	if err != nil {
		t.Fatalf("SaveStep() error = %v", err)
	}

	loaded, err := s.loadSessionState(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("failed to load session state: %v", err)
	}
	if loaded.AgentType != agent.AgentTypeClaudeCode {
		t.Errorf("AgentType = %q, want %q", loaded.AgentType, agent.AgentTypeClaudeCode)
	}
}

// TestSaveStep_UsesCtxAgentType_WhenPartialState tests that SaveStep uses
// ctx.AgentType when a partial session state exists (empty BaseCommit and AgentType).
func TestSaveStep_UsesCtxAgentType_WhenPartialState(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test content"), 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	if _, err := worktree.Add("test.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}
	if _, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	}); err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	s := &ManualCommitStrategy{}
	sessionID := "2026-02-06-partial-state-agent-test"

	// Create partial session state with empty BaseCommit and no AgentType
	partialState := &SessionState{
		SessionID:  sessionID,
		BaseCommit: "",
		StartedAt:  time.Now(),
	}
	if err := s.saveSessionState(context.Background(), partialState); err != nil {
		t.Fatalf("failed to save partial state: %v", err)
	}

	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	if err := os.MkdirAll(metadataDirAbs, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}
	transcript := `{"type":"human","message":{"content":"test"}}` + "\n"
	if err := os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(transcript), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	err = s.SaveStep(context.Background(), StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{},
		NewFiles:       []string{},
		DeletedFiles:   []string{},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Test checkpoint",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
		AgentType:      agent.AgentTypeClaudeCode,
	})
	if err != nil {
		t.Fatalf("SaveStep() error = %v", err)
	}

	loaded, err := s.loadSessionState(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("failed to load session state: %v", err)
	}
	if loaded.AgentType != agent.AgentTypeClaudeCode {
		t.Errorf("AgentType = %q, want %q", loaded.AgentType, agent.AgentTypeClaudeCode)
	}
}

// TestCountTranscriptItems tests counting lines/messages in different transcript formats.
func TestCountTranscriptItems(t *testing.T) {
	tests := []struct {
		name      string
		agentType types.AgentType
		content   string
		expected  int
	}{
		{
			name:      "Gemini JSON with messages",
			agentType: agent.AgentTypeGemini,
			content: `{
				"messages": [
					{"type": "user", "content": "Hello"},
					{"type": "gemini", "content": "Hi there!"}
				]
			}`,
			expected: 2,
		},
		{
			name:      "Gemini empty messages array",
			agentType: agent.AgentTypeGemini,
			content:   `{"messages": []}`,
			expected:  0,
		},
		{
			name:      "Claude Code JSONL",
			agentType: agent.AgentTypeClaudeCode,
			content: `{"type":"human","message":{"content":"Hello"}}
{"type":"assistant","message":{"content":"Hi"}}`,
			expected: 2,
		},
		{
			name:      "Claude Code JSONL with trailing newline",
			agentType: agent.AgentTypeClaudeCode,
			content: `{"type":"human","message":{"content":"Hello"}}
{"type":"assistant","message":{"content":"Hi"}}
`,
			expected: 2,
		},
		{
			name:      "empty string",
			agentType: agent.AgentTypeClaudeCode,
			content:   "",
			expected:  0,
		},
		{
			name:      "Gemini JSON with array content (real format)",
			agentType: agent.AgentTypeGemini,
			content: `{
				"messages": [
					{"type": "user", "content": [{"text": "Hello"}]},
					{"type": "gemini", "content": "Hi there!"},
					{"type": "user", "content": [{"text": "Do something"}]},
					{"type": "gemini", "content": "Done!"}
				]
			}`,
			expected: 4,
		},
		{
			name:      "OpenCode export JSON with messages",
			agentType: agent.AgentTypeOpenCode,
			content: `{
				"info": {"id": "session-1"},
				"messages": [
					{"info": {"role": "user"}, "parts": [{"type": "text", "text": "Hello"}]},
					{"info": {"role": "assistant"}, "parts": [{"type": "text", "text": "Hi there!"}]}
				]
			}`,
			expected: 2,
		},
		{
			name:      "OpenCode export JSON empty messages",
			agentType: agent.AgentTypeOpenCode,
			content:   `{"info": {"id": "session-1"}, "messages": []}`,
			expected:  0,
		},
		{
			name:      "OpenCode invalid JSON",
			agentType: agent.AgentTypeOpenCode,
			content:   `not valid json`,
			expected:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := countTranscriptItems(tt.agentType, tt.content)
			if result != tt.expected {
				t.Errorf("countTranscriptItems() = %v, want %v", result, tt.expected)
			}
		})
	}
}

// TestExtractUserPrompts tests extraction of user prompts from different transcript formats.
func TestExtractUserPrompts(t *testing.T) {
	tests := []struct {
		name      string
		agentType types.AgentType
		content   string
		expected  []string
	}{
		{
			name:      "Gemini single user prompt",
			agentType: agent.AgentTypeGemini,
			content: `{
				"messages": [
					{"type": "user", "content": "Create a file called test.txt"}
				]
			}`,
			expected: []string{"Create a file called test.txt"},
		},
		{
			name:      "Gemini multiple user prompts",
			agentType: agent.AgentTypeGemini,
			content: `{
				"messages": [
					{"type": "user", "content": "First prompt"},
					{"type": "gemini", "content": "Response 1"},
					{"type": "user", "content": "Second prompt"},
					{"type": "gemini", "content": "Response 2"}
				]
			}`,
			expected: []string{"First prompt", "Second prompt"},
		},
		{
			name:      "Gemini no user messages",
			agentType: agent.AgentTypeGemini,
			content: `{
				"messages": [
					{"type": "gemini", "content": "Hello!"}
				]
			}`,
			expected: nil,
		},
		{
			name:      "Claude Code JSONL with user messages",
			agentType: agent.AgentTypeClaudeCode,
			content: `{"type":"user","message":{"content":"Hello"}}
{"type":"assistant","message":{"content":"Hi"}}
{"type":"user","message":{"content":"Goodbye"}}`,
			expected: []string{"Hello", "Goodbye"},
		},
		{
			name:      "empty string",
			agentType: agent.AgentTypeClaudeCode,
			content:   "",
			expected:  nil,
		},
		{
			name:      "Gemini array content (real format)",
			agentType: agent.AgentTypeGemini,
			content: `{
				"messages": [
					{"type": "user", "content": [{"text": "Create a file"}]},
					{"type": "gemini", "content": "Done!"},
					{"type": "user", "content": [{"text": "Edit the file"}]},
					{"type": "gemini", "content": "Updated!"}
				]
			}`,
			expected: []string{"Create a file", "Edit the file"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractUserPrompts(tt.agentType, tt.content)
			if len(result) != len(tt.expected) {
				t.Errorf("extractUserPrompts() returned %d prompts, want %d", len(result), len(tt.expected))
				return
			}
			for i, prompt := range result {
				if prompt != tt.expected[i] {
					t.Errorf("prompt[%d] = %q, want %q", i, prompt, tt.expected[i])
				}
			}
		})
	}
}

// TestCondenseSession_IncludesInitialAttribution verifies that when manual-commit
// condenses a session, it calculates InitialAttribution by comparing the shadow branch
// (agent work) to HEAD (what was committed).
func TestCondenseSession_IncludesInitialAttribution(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init repo: %v", err)
	}

	// Create initial commit with a file
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create a file with some content
	testFile := filepath.Join(dir, "test.go")
	originalContent := "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n"
	if err := os.WriteFile(testFile, []byte(originalContent), 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	if _, err := worktree.Add("test.go"); err != nil {
		t.Fatalf("failed to stage file: %v", err)
	}

	_, err = worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	s := &ManualCommitStrategy{}
	sessionID := "2025-01-15-test-attribution"

	// Create metadata directory with transcript
	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	if err := os.MkdirAll(metadataDirAbs, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}

	transcript := `{"type":"human","message":{"content":"modify test.go"}}
{"type":"assistant","message":{"content":"I'll modify test.go"}}
`
	if err := os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(transcript), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Agent modifies the file (adds a new function)
	agentContent := "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n\nfunc newFunc() {\n\tprintln(\"agent added this\")\n}\n"
	if err := os.WriteFile(testFile, []byte(agentContent), 0o644); err != nil {
		t.Fatalf("failed to write agent changes: %v", err)
	}

	// First checkpoint - captures agent's work on shadow branch
	err = s.SaveStep(context.Background(), StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{"test.go"},
		NewFiles:       []string{},
		DeletedFiles:   []string{},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Checkpoint 1",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	})
	if err != nil {
		t.Fatalf("SaveStep() error = %v", err)
	}

	// Human edits the file (adds a comment)
	humanEditedContent := "package main\n\n// Human added this comment\nfunc main() {\n\tprintln(\"hello\")\n}\n\nfunc newFunc() {\n\tprintln(\"agent added this\")\n}\n"
	if err := os.WriteFile(testFile, []byte(humanEditedContent), 0o644); err != nil {
		t.Fatalf("failed to write human edits: %v", err)
	}

	// Stage and commit the human-edited file (this is what the user does)
	if _, err := worktree.Add("test.go"); err != nil {
		t.Fatalf("failed to stage human edits: %v", err)
	}
	_, err = worktree.Commit("Add new function with human comment", &git.CommitOptions{
		Author: &object.Signature{Name: "Human", Email: "human@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to commit human edits: %v", err)
	}

	// Load session state
	state, err := s.loadSessionState(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("loadSessionState() error = %v", err)
	}

	// Condense the session - this should calculate InitialAttribution
	checkpointID := id.MustCheckpointID("a1b2c3d4e5f6")
	result, err := s.CondenseSession(context.Background(), repo, checkpointID, state, nil)
	if err != nil {
		t.Fatalf("CondenseSession() error = %v", err)
	}

	// Verify CondenseResult
	if result.CheckpointID != checkpointID {
		t.Errorf("CheckpointID = %q, want %q", result.CheckpointID, checkpointID)
	}

	// Read metadata from entire/checkpoints/v1 branch and verify InitialAttribution
	sessionsRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("failed to get sessions branch: %v", err)
	}

	sessionsCommit, err := repo.CommitObject(sessionsRef.Hash())
	if err != nil {
		t.Fatalf("failed to get sessions commit: %v", err)
	}

	tree, err := sessionsCommit.Tree()
	if err != nil {
		t.Fatalf("failed to get tree: %v", err)
	}

	// InitialAttribution is stored in session-level metadata (0/metadata.json), not root (0-based indexing)
	sessionMetadataPath := checkpointID.Path() + "/0/" + paths.MetadataFileName
	metadataFile, err := tree.File(sessionMetadataPath)
	if err != nil {
		t.Fatalf("failed to find session metadata.json at %s: %v", sessionMetadataPath, err)
	}

	content, err := metadataFile.Contents()
	if err != nil {
		t.Fatalf("failed to read metadata.json: %v", err)
	}

	// Parse and verify InitialAttribution is present
	var metadata struct {
		InitialAttribution *struct {
			AgentLines      int     `json:"agent_lines"`
			HumanAdded      int     `json:"human_added"`
			HumanModified   int     `json:"human_modified"`
			HumanRemoved    int     `json:"human_removed"`
			TotalCommitted  int     `json:"total_committed"`
			AgentPercentage float64 `json:"agent_percentage"`
		} `json:"initial_attribution"`
	}
	if err := json.Unmarshal([]byte(content), &metadata); err != nil {
		t.Fatalf("failed to parse metadata.json: %v", err)
	}

	if metadata.InitialAttribution == nil {
		t.Fatal("InitialAttribution should be present in session metadata.json for manual-commit")
	}

	// Verify the attribution values are reasonable
	// Agent added new function, human added a comment line
	// The exact line counts depend on how the diff algorithm interprets the changes
	// (insertion vs modification), but we should have non-zero totals and reasonable percentages.
	if metadata.InitialAttribution.TotalCommitted == 0 {
		t.Error("TotalCommitted should be > 0")
	}
	if metadata.InitialAttribution.AgentLines == 0 {
		t.Error("AgentLines should be > 0 (agent wrote code)")
	}

	// Human contribution should be captured in either HumanAdded or HumanModified
	// When inserting lines in the middle of existing code, the diff algorithm may
	// interpret it as a modification rather than a pure addition.
	humanContribution := metadata.InitialAttribution.HumanAdded + metadata.InitialAttribution.HumanModified
	if humanContribution == 0 {
		t.Error("Human contribution (HumanAdded + HumanModified) should be > 0")
	}

	if metadata.InitialAttribution.AgentPercentage <= 0 || metadata.InitialAttribution.AgentPercentage > 100 {
		t.Errorf("AgentPercentage should be between 0-100, got %f", metadata.InitialAttribution.AgentPercentage)
	}

	t.Logf("Attribution: agent=%d, human_added=%d, human_modified=%d, human_removed=%d, total=%d, percentage=%.1f%%",
		metadata.InitialAttribution.AgentLines,
		metadata.InitialAttribution.HumanAdded,
		metadata.InitialAttribution.HumanModified,
		metadata.InitialAttribution.HumanRemoved,
		metadata.InitialAttribution.TotalCommitted,
		metadata.InitialAttribution.AgentPercentage)
}

// TestCondenseSession_AttributionWithoutShadowBranch verifies that when an agent
// commits mid-turn (before SaveStep), attribution is still calculated using HEAD
// as the shadow tree. This reproduces the bug where agent_lines=0 for mid-turn commits.
func TestCondenseSession_AttributionWithoutShadowBranch(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create initial empty commit
	initialHash, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author:            &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
		AllowEmptyCommits: true,
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Agent creates files in nested directories and commits (mid-turn, no SaveStep)
	srcDir := filepath.Join(dir, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatalf("failed to create src dir: %v", err)
	}
	agentFile := filepath.Join(srcDir, "main.go")
	agentContent := "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n"
	if err := os.WriteFile(agentFile, []byte(agentContent), 0o644); err != nil {
		t.Fatalf("failed to write agent file: %v", err)
	}
	agentFile2 := filepath.Join(dir, "README.md")
	agentContent2 := "# My Project\n\nA test project.\n"
	if err := os.WriteFile(agentFile2, []byte(agentContent2), 0o644); err != nil {
		t.Fatalf("failed to write agent file 2: %v", err)
	}
	if _, err := worktree.Add("src/main.go"); err != nil {
		t.Fatalf("failed to stage file: %v", err)
	}
	if _, err := worktree.Add("README.md"); err != nil {
		t.Fatalf("failed to stage file 2: %v", err)
	}
	_, err = worktree.Commit("Add project files", &git.CommitOptions{
		Author: &object.Signature{Name: "Agent", Email: "agent@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	// Create a live transcript file (required when no shadow branch)
	transcriptDir := filepath.Join(dir, ".claude", "projects", "test")
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		t.Fatalf("failed to create transcript dir: %v", err)
	}
	transcriptFile := filepath.Join(transcriptDir, "session.jsonl")
	transcriptContent := `{"type":"human","message":{"content":"create project files"}}
{"type":"assistant","message":{"content":"I'll create src/main.go and README.md"}}
`
	if err := os.WriteFile(transcriptFile, []byte(transcriptContent), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Construct session state manually (no SaveStep was called, so no shadow branch)
	state := &SessionState{
		SessionID:             "test-no-shadow",
		BaseCommit:            initialHash.String(),
		AttributionBaseCommit: initialHash.String(),
		FilesTouched:          []string{"src/main.go", "README.md"},
		TranscriptPath:        transcriptFile,
		AgentType:             "Claude Code",
	}

	s := &ManualCommitStrategy{}
	checkpointID := id.MustCheckpointID("c3d4e5f6a7b8")

	// Condense — no shadow branch exists, but attribution should still work
	committedFiles := map[string]struct{}{"src/main.go": {}, "README.md": {}}
	result, err := s.CondenseSession(context.Background(), repo, checkpointID, state, committedFiles)
	if err != nil {
		t.Fatalf("CondenseSession() error = %v", err)
	}
	if result.CheckpointID != checkpointID {
		t.Errorf("CheckpointID = %q, want %q", result.CheckpointID, checkpointID)
	}

	// Read metadata from entire/checkpoints/v1 branch
	sessionsRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("failed to get sessions branch: %v", err)
	}
	sessionsCommit, err := repo.CommitObject(sessionsRef.Hash())
	if err != nil {
		t.Fatalf("failed to get sessions commit: %v", err)
	}
	tree, err := sessionsCommit.Tree()
	if err != nil {
		t.Fatalf("failed to get tree: %v", err)
	}

	sessionMetadataPath := checkpointID.Path() + "/0/" + paths.MetadataFileName
	metadataFile, err := tree.File(sessionMetadataPath)
	if err != nil {
		t.Fatalf("failed to find session metadata at %s: %v", sessionMetadataPath, err)
	}
	content, err := metadataFile.Contents()
	if err != nil {
		t.Fatalf("failed to read metadata: %v", err)
	}

	var metadata struct {
		InitialAttribution *struct {
			AgentLines      int     `json:"agent_lines"`
			HumanAdded      int     `json:"human_added"`
			TotalCommitted  int     `json:"total_committed"`
			AgentPercentage float64 `json:"agent_percentage"`
		} `json:"initial_attribution"`
	}
	if err := json.Unmarshal([]byte(content), &metadata); err != nil {
		t.Fatalf("failed to parse metadata: %v", err)
	}

	if metadata.InitialAttribution == nil {
		t.Fatal("InitialAttribution should be present even without shadow branch")
	}

	// Agent created all content (10 lines across 2 files), no human edits
	if metadata.InitialAttribution.AgentLines == 0 {
		t.Error("AgentLines should be > 0 (agent created the file)")
	}
	if metadata.InitialAttribution.TotalCommitted == 0 {
		t.Error("TotalCommitted should be > 0")
	}
	if metadata.InitialAttribution.AgentPercentage <= 50 {
		t.Errorf("AgentPercentage should be > 50%% (agent wrote all content), got %.1f%%",
			metadata.InitialAttribution.AgentPercentage)
	}

	t.Logf("Attribution (no shadow branch): agent=%d, human_added=%d, total=%d, percentage=%.1f%%",
		metadata.InitialAttribution.AgentLines,
		metadata.InitialAttribution.HumanAdded,
		metadata.InitialAttribution.TotalCommitted,
		metadata.InitialAttribution.AgentPercentage)
}

// TestCondenseSession_AttributionWithoutShadowBranch_MixedHumanAgent verifies attribution
// when an agent commits mid-turn (no shadow branch) and the commit includes both human
// pre-session changes and agent-created files. Human changes are captured in PromptAttributions
// and should be subtracted from the total to isolate agent contribution.
func TestCondenseSession_AttributionWithoutShadowBranch_MixedHumanAgent(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init repo: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create initial commit with one file
	existingFile := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(existingFile, []byte("key: value\n"), 0o644); err != nil {
		t.Fatalf("failed to write initial file: %v", err)
	}
	if _, err := wt.Add("config.yaml"); err != nil {
		t.Fatalf("failed to stage: %v", err)
	}
	initialHash, err := wt.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	// Human adds a new file (before the agent session starts).
	// This is captured by calculatePromptAttributionAtStart.
	humanFile := filepath.Join(dir, "docs", "notes.md")
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatalf("failed to mkdir: %v", err)
	}
	humanContent := "# Notes\n\nSome human notes.\nAnother line.\n"
	if err := os.WriteFile(humanFile, []byte(humanContent), 0o644); err != nil {
		t.Fatalf("failed to write human file: %v", err)
	}

	// Agent creates its own file in a nested directory
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatalf("failed to mkdir: %v", err)
	}
	agentFile := filepath.Join(dir, "src", "app.go")
	agentContent := "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"app\")\n}\n"
	if err := os.WriteFile(agentFile, []byte(agentContent), 0o644); err != nil {
		t.Fatalf("failed to write agent file: %v", err)
	}

	// Agent stages everything and commits (mid-turn, no SaveStep)
	if _, err := wt.Add("docs/notes.md"); err != nil {
		t.Fatalf("failed to stage: %v", err)
	}
	if _, err := wt.Add("src/app.go"); err != nil {
		t.Fatalf("failed to stage: %v", err)
	}
	_, err = wt.Commit("Add app and notes", &git.CommitOptions{
		Author: &object.Signature{Name: "Agent", Email: "agent@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	// Create live transcript
	transcriptDir := filepath.Join(dir, ".claude", "projects", "test")
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		t.Fatalf("failed to create transcript dir: %v", err)
	}
	transcriptFile := filepath.Join(transcriptDir, "session.jsonl")
	if err := os.WriteFile(transcriptFile, []byte(`{"type":"human","message":{"content":"create src/app.go"}}
{"type":"assistant","message":{"content":"Done"}}
`), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Session state with PromptAttributions capturing human's pre-session file (4 lines)
	state := &SessionState{
		SessionID:             "test-mixed-no-shadow",
		BaseCommit:            initialHash.String(),
		AttributionBaseCommit: initialHash.String(),
		FilesTouched:          []string{"src/app.go"},
		TranscriptPath:        transcriptFile,
		AgentType:             "Claude Code",
		PromptAttributions: []PromptAttribution{{
			CheckpointNumber: 1,
			UserLinesAdded:   4,
			UserAddedPerFile: map[string]int{"docs/notes.md": 4},
		}},
	}

	s := &ManualCommitStrategy{}
	checkpointID := id.MustCheckpointID("d4e5f6a7b8c9")

	committedFiles := map[string]struct{}{"src/app.go": {}, "docs/notes.md": {}}
	result, err := s.CondenseSession(context.Background(), repo, checkpointID, state, committedFiles)
	if err != nil {
		t.Fatalf("CondenseSession() error = %v", err)
	}
	if result.CheckpointID != checkpointID {
		t.Errorf("CheckpointID = %q, want %q", result.CheckpointID, checkpointID)
	}

	// Read metadata
	sessionsRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("failed to get sessions branch: %v", err)
	}
	sessionsCommit, err := repo.CommitObject(sessionsRef.Hash())
	if err != nil {
		t.Fatalf("failed to get sessions commit: %v", err)
	}
	tree, err := sessionsCommit.Tree()
	if err != nil {
		t.Fatalf("failed to get tree: %v", err)
	}

	sessionMetadataPath := checkpointID.Path() + "/0/" + paths.MetadataFileName
	metadataFile, err := tree.File(sessionMetadataPath)
	if err != nil {
		t.Fatalf("failed to find session metadata at %s: %v", sessionMetadataPath, err)
	}
	content, err := metadataFile.Contents()
	if err != nil {
		t.Fatalf("failed to read metadata: %v", err)
	}

	var metadata struct {
		InitialAttribution *struct {
			AgentLines      int     `json:"agent_lines"`
			HumanAdded      int     `json:"human_added"`
			TotalCommitted  int     `json:"total_committed"`
			AgentPercentage float64 `json:"agent_percentage"`
		} `json:"initial_attribution"`
	}
	if err := json.Unmarshal([]byte(content), &metadata); err != nil {
		t.Fatalf("failed to parse metadata: %v", err)
	}

	if metadata.InitialAttribution == nil {
		t.Fatal("InitialAttribution should be present")
	}

	attr := metadata.InitialAttribution
	t.Logf("Attribution (mixed, no shadow): agent=%d, human_added=%d, total=%d, percentage=%.1f%%",
		attr.AgentLines, attr.HumanAdded, attr.TotalCommitted, attr.AgentPercentage)

	// src/app.go has 7 lines (agent). docs/notes.md was added before the session
	// (captured by PA1) so it's pre-session baseline — excluded from human count.
	if attr.AgentLines != 7 {
		t.Errorf("AgentLines = %d, want 7 (src/app.go has 7 lines)", attr.AgentLines)
	}
	if attr.HumanAdded != 0 {
		t.Errorf("HumanAdded = %d, want 0 (docs/notes.md is pre-session baseline, excluded)", attr.HumanAdded)
	}
	if attr.TotalCommitted != 7 {
		t.Errorf("TotalCommitted = %d, want 7 (agent-only, pre-session excluded)", attr.TotalCommitted)
	}
	// Agent wrote 7/7 = 100%
	if attr.AgentPercentage < 99.0 {
		t.Errorf("AgentPercentage = %.1f%%, want ~100%% (pre-session human file excluded)", attr.AgentPercentage)
	}
}

// TestExtractUserPromptsFromLines tests extraction of user prompts from JSONL format.
func TestExtractUserPromptsFromLines(t *testing.T) {
	tests := []struct {
		name     string
		lines    []string
		expected []string
	}{
		{
			name: "human type message",
			lines: []string{
				`{"type":"human","message":{"content":"Hello world"}}`,
			},
			expected: []string{"Hello world"},
		},
		{
			name: "user type message",
			lines: []string{
				`{"type":"user","message":{"content":"Test prompt"}}`,
			},
			expected: []string{"Test prompt"},
		},
		{
			name: "mixed human and assistant",
			lines: []string{
				`{"type":"human","message":{"content":"First"}}`,
				`{"type":"assistant","message":{"content":"Response"}}`,
				`{"type":"human","message":{"content":"Second"}}`,
			},
			expected: []string{"First", "Second"},
		},
		{
			name: "array content",
			lines: []string{
				`{"type":"human","message":{"content":[{"type":"text","text":"Part 1"},{"type":"text","text":"Part 2"}]}}`,
			},
			expected: []string{"Part 1\n\nPart 2"},
		},
		{
			name: "empty lines ignored",
			lines: []string{
				`{"type":"human","message":{"content":"Valid"}}`,
				"",
				"  ",
			},
			expected: []string{"Valid"},
		},
		{
			name: "invalid JSON ignored",
			lines: []string{
				`{"type":"human","message":{"content":"Valid"}}`,
				"not json",
			},
			expected: []string{"Valid"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractUserPromptsFromLines(tt.lines)
			if len(result) != len(tt.expected) {
				t.Errorf("extractUserPromptsFromLines() returned %d prompts, want %d", len(result), len(tt.expected))
				return
			}
			for i, prompt := range result {
				if prompt != tt.expected[i] {
					t.Errorf("prompt[%d] = %q, want %q", i, prompt, tt.expected[i])
				}
			}
		})
	}
}

// TestMultiCheckpoint_UserEditsBetweenCheckpoints tests that user edits made between
// agent checkpoints are correctly attributed to the user, not the agent.
//
// This tests two scenarios:
// 1. User edits a DIFFERENT file than agent - detected at checkpoint save time
// 2. User edits the SAME file as agent - detected at commit time (shadow → head diff)
//
//nolint:maintidx // Integration test with multiple steps is inherently complex
func TestMultiCheckpoint_UserEditsBetweenCheckpoints(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create initial commit with two files
	agentFile := filepath.Join(dir, "agent.go")
	userFile := filepath.Join(dir, "user.go")
	if err := os.WriteFile(agentFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("failed to write agent file: %v", err)
	}
	if err := os.WriteFile(userFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("failed to write user file: %v", err)
	}
	if _, err := worktree.Add("agent.go"); err != nil {
		t.Fatalf("failed to stage file: %v", err)
	}
	if _, err := worktree.Add("user.go"); err != nil {
		t.Fatalf("failed to stage file: %v", err)
	}
	_, err = worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	s := &ManualCommitStrategy{}
	sessionID := "2025-01-15-multi-checkpoint-test"

	// Create metadata directory
	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	if err := os.MkdirAll(metadataDirAbs, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}

	transcript := `{"type":"human","message":{"content":"add function"}}
{"type":"assistant","message":{"content":"adding function"}}
`
	if err := os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(transcript), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// === PROMPT 1 START: Initialize session (simulates UserPromptSubmit) ===
	// This must happen BEFORE agent makes any changes
	if err := s.InitializeSession(context.Background(), sessionID, "Claude Code", "", "", ""); err != nil {
		t.Fatalf("InitializeSession() prompt 1 error = %v", err)
	}

	// === CHECKPOINT 1: Agent modifies agent.go (adds 4 lines) ===
	checkpoint1Content := "package main\n\nfunc agentFunc1() {\n\tprintln(\"agent1\")\n}\n"
	if err := os.WriteFile(agentFile, []byte(checkpoint1Content), 0o644); err != nil {
		t.Fatalf("failed to write agent changes 1: %v", err)
	}

	err = s.SaveStep(context.Background(), StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{"agent.go"},
		NewFiles:       []string{},
		DeletedFiles:   []string{},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Checkpoint 1",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	})
	if err != nil {
		t.Fatalf("SaveStep() checkpoint 1 error = %v", err)
	}

	// Verify PromptAttribution was recorded for checkpoint 1
	state1, err := s.loadSessionState(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("loadSessionState() after checkpoint 1 error = %v", err)
	}
	if len(state1.PromptAttributions) != 1 {
		t.Fatalf("expected 1 PromptAttribution after checkpoint 1, got %d", len(state1.PromptAttributions))
	}
	// First checkpoint: no user edits yet (user.go hasn't changed)
	if state1.PromptAttributions[0].UserLinesAdded != 0 {
		t.Errorf("checkpoint 1: expected 0 user lines added, got %d", state1.PromptAttributions[0].UserLinesAdded)
	}

	// === USER EDITS A DIFFERENT FILE (user.go) BETWEEN CHECKPOINTS ===
	userEditContent := "package main\n\n// User added this function\nfunc userFunc() {\n\tprintln(\"user\")\n}\n"
	if err := os.WriteFile(userFile, []byte(userEditContent), 0o644); err != nil {
		t.Fatalf("failed to write user edits: %v", err)
	}

	// === PROMPT 2 START: Initialize session again (simulates UserPromptSubmit) ===
	// This captures the user's edits to user.go BEFORE the agent runs
	if err := s.InitializeSession(context.Background(), sessionID, "Claude Code", "", "", ""); err != nil {
		t.Fatalf("InitializeSession() prompt 2 error = %v", err)
	}

	// === CHECKPOINT 2: Agent modifies agent.go again (adds 4 more lines) ===
	checkpoint2Content := "package main\n\nfunc agentFunc1() {\n\tprintln(\"agent1\")\n}\n\nfunc agentFunc2() {\n\tprintln(\"agent2\")\n}\n"
	if err := os.WriteFile(agentFile, []byte(checkpoint2Content), 0o644); err != nil {
		t.Fatalf("failed to write agent changes 2: %v", err)
	}

	err = s.SaveStep(context.Background(), StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{"agent.go"},
		NewFiles:       []string{},
		DeletedFiles:   []string{},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Checkpoint 2",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	})
	if err != nil {
		t.Fatalf("SaveStep() checkpoint 2 error = %v", err)
	}

	// Verify PromptAttribution was recorded for checkpoint 2
	state2, err := s.loadSessionState(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("loadSessionState() after checkpoint 2 error = %v", err)
	}
	if len(state2.PromptAttributions) != 2 {
		t.Fatalf("expected 2 PromptAttributions after checkpoint 2, got %d", len(state2.PromptAttributions))
	}

	t.Logf("Checkpoint 2 PromptAttribution: user_added=%d, user_removed=%d, agent_added=%d, agent_removed=%d",
		state2.PromptAttributions[1].UserLinesAdded,
		state2.PromptAttributions[1].UserLinesRemoved,
		state2.PromptAttributions[1].AgentLinesAdded,
		state2.PromptAttributions[1].AgentLinesRemoved)

	// Second checkpoint should detect user's edits to user.go (different file than agent)
	// User added 5 lines to user.go
	if state2.PromptAttributions[1].UserLinesAdded == 0 {
		t.Error("checkpoint 2: expected user lines added > 0 because user edited user.go")
	}

	// === USER COMMITS ===
	if _, err := worktree.Add("agent.go"); err != nil {
		t.Fatalf("failed to stage agent.go: %v", err)
	}
	if _, err := worktree.Add("user.go"); err != nil {
		t.Fatalf("failed to stage user.go: %v", err)
	}
	_, err = worktree.Commit("Final commit with agent and user changes", &git.CommitOptions{
		Author: &object.Signature{Name: "Human", Email: "human@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	// === CONDENSE AND VERIFY ATTRIBUTION ===
	checkpointID := id.MustCheckpointID("b2c3d4e5f6a7")
	result, err := s.CondenseSession(context.Background(), repo, checkpointID, state2, nil)
	if err != nil {
		t.Fatalf("CondenseSession() error = %v", err)
	}

	if result.CheckpointID != checkpointID {
		t.Errorf("CheckpointID = %q, want %q", result.CheckpointID, checkpointID)
	}

	// Read metadata and verify attribution
	sessionsRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("failed to get sessions branch: %v", err)
	}

	sessionsCommit, err := repo.CommitObject(sessionsRef.Hash())
	if err != nil {
		t.Fatalf("failed to get sessions commit: %v", err)
	}

	tree, err := sessionsCommit.Tree()
	if err != nil {
		t.Fatalf("failed to get tree: %v", err)
	}

	// InitialAttribution is stored in session-level metadata (0/metadata.json), not root (0-based indexing)
	sessionMetadataPath := checkpointID.Path() + "/0/" + paths.MetadataFileName
	metadataFile, err := tree.File(sessionMetadataPath)
	if err != nil {
		t.Fatalf("failed to find session metadata.json at %s: %v", sessionMetadataPath, err)
	}

	content, err := metadataFile.Contents()
	if err != nil {
		t.Fatalf("failed to read metadata.json: %v", err)
	}

	var metadata struct {
		InitialAttribution *struct {
			AgentLines      int     `json:"agent_lines"`
			HumanAdded      int     `json:"human_added"`
			HumanModified   int     `json:"human_modified"`
			HumanRemoved    int     `json:"human_removed"`
			TotalCommitted  int     `json:"total_committed"`
			AgentPercentage float64 `json:"agent_percentage"`
		} `json:"initial_attribution"`
	}
	if err := json.Unmarshal([]byte(content), &metadata); err != nil {
		t.Fatalf("failed to parse metadata.json: %v", err)
	}

	if metadata.InitialAttribution == nil {
		t.Fatal("InitialAttribution should be present in session metadata")
	}

	t.Logf("Final Attribution: agent=%d, human_added=%d, human_modified=%d, human_removed=%d, total=%d, percentage=%.1f%%",
		metadata.InitialAttribution.AgentLines,
		metadata.InitialAttribution.HumanAdded,
		metadata.InitialAttribution.HumanModified,
		metadata.InitialAttribution.HumanRemoved,
		metadata.InitialAttribution.TotalCommitted,
		metadata.InitialAttribution.AgentPercentage)

	// Verify the attribution makes sense:
	// - Agent modified agent.go: added ~8 lines total
	// - User modified user.go: added ~5 lines
	// - So agent percentage should be around 50-70%
	if metadata.InitialAttribution.AgentLines == 0 {
		t.Error("AgentLines should be > 0")
	}
	if metadata.InitialAttribution.TotalCommitted == 0 {
		t.Error("TotalCommitted should be > 0")
	}

	// The key test: user's lines should be captured in HumanAdded
	if metadata.InitialAttribution.HumanAdded == 0 {
		t.Error("HumanAdded should be > 0 because user added lines to user.go")
	}

	// Agent percentage should not be 100% since user contributed
	if metadata.InitialAttribution.AgentPercentage >= 100 {
		t.Errorf("AgentPercentage should be < 100%% since user contributed, got %.1f%%",
			metadata.InitialAttribution.AgentPercentage)
	}
}

// TestCondenseSession_PrefersLiveTranscript verifies that CondenseSession reads the
// live transcript file when available, rather than the potentially stale shadow branch copy.
// This reproduces the bug where SaveStep was skipped (no code changes) but the
// transcript continued growing — deferred condensation would read stale data.
func TestCondenseSession_PrefersLiveTranscript(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init repo: %v", err)
	}

	// Create initial commit
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("content"), 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	if _, err := wt.Add("file.txt"); err != nil {
		t.Fatalf("failed to stage: %v", err)
	}
	_, err = wt.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	s := &ManualCommitStrategy{}
	sessionID := "2025-01-15-test-live-transcript"

	// Create metadata dir with an initial (short) transcript
	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	if err := os.MkdirAll(metadataDirAbs, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}

	staleTranscript := `{"type":"human","message":{"content":"first prompt"}}
{"type":"assistant","message":{"content":"first response"}}
`
	if err := os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(staleTranscript), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// SaveStep to create shadow branch with the stale transcript
	err = s.SaveStep(context.Background(), StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{},
		NewFiles:       []string{},
		DeletedFiles:   []string{},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Checkpoint 1",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	})
	if err != nil {
		t.Fatalf("SaveStep() error = %v", err)
	}

	// Now simulate the conversation continuing: write a LONGER live transcript file.
	// In the real bug, SaveStep would be skipped because totalChanges == 0,
	// so the shadow branch still has the stale version.
	liveTranscriptFile := filepath.Join(dir, "live-transcript.jsonl")
	liveTranscript := `{"type":"human","message":{"content":"first prompt"}}
{"type":"assistant","message":{"content":"first response"}}
{"type":"human","message":{"content":"second prompt"}}
{"type":"assistant","message":{"content":"second response"}}
`
	if err := os.WriteFile(liveTranscriptFile, []byte(liveTranscript), 0o644); err != nil {
		t.Fatalf("failed to write live transcript: %v", err)
	}

	// Load session state and set TranscriptPath to the live file
	state, err := s.loadSessionState(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("loadSessionState() error = %v", err)
	}
	state.TranscriptPath = liveTranscriptFile
	if err := s.saveSessionState(context.Background(), state); err != nil {
		t.Fatalf("saveSessionState() error = %v", err)
	}

	// Condense — this should read the live transcript, not the shadow branch copy
	checkpointID := id.MustCheckpointID("b2c3d4e5f6a1")
	result, err := s.CondenseSession(context.Background(), repo, checkpointID, state, nil)
	if err != nil {
		t.Fatalf("CondenseSession() error = %v", err)
	}

	// The live transcript has 4 lines; the shadow branch copy has 2.
	// If we read the stale shadow copy, we'd only see 2 lines.
	if result.TotalTranscriptLines != 4 {
		t.Errorf("TotalTranscriptLines = %d, want 4 (live transcript has 4 lines, shadow has 2)", result.TotalTranscriptLines)
	}

	// Verify the condensed content includes the second prompt
	store := checkpoint.NewGitStore(repo)
	content, err := store.ReadLatestSessionContent(t.Context(), checkpointID)
	if err != nil {
		t.Fatalf("ReadLatestSessionContent() error = %v", err)
	}
	if !strings.Contains(string(content.Transcript), "second prompt") {
		t.Error("condensed transcript should contain 'second prompt' from live file, but it doesn't")
	}
}

// TestCondenseSession_TranscriptRelocatedMidSession verifies that CondenseSession
// succeeds when the agent relocates its transcript mid-session (e.g., Cursor CLI
// switching from flat <dir>/<id>.jsonl to nested <dir>/<id>/<id>.jsonl layout).
// This is a regression test for a Cursor CLI 2026.03.11 change that broke mid-turn
// commits because the stored TranscriptPath became stale.
func TestCondenseSession_TranscriptRelocatedMidSession(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init repo: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("content"), 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	if _, err := wt.Add("file.txt"); err != nil {
		t.Fatalf("failed to stage: %v", err)
	}
	_, err = wt.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	s := &ManualCommitStrategy{}
	sessionID := "87874108-eff2-47a0-b260-183961dd6cb0"

	// Create the session state with a flat TranscriptPath (what before-submit-prompt reports)
	agentTranscriptsDir := filepath.Join(dir, "agent-transcripts")
	if err := os.MkdirAll(agentTranscriptsDir, 0o755); err != nil {
		t.Fatalf("failed to create agent-transcripts dir: %v", err)
	}
	flatPath := filepath.Join(agentTranscriptsDir, sessionID+".jsonl")

	// But the file actually lives at the nested path (Cursor relocated it)
	nestedDir := filepath.Join(agentTranscriptsDir, sessionID)
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatalf("failed to create nested dir: %v", err)
	}
	nestedPath := filepath.Join(nestedDir, sessionID+".jsonl")
	transcript := `{"type":"human","message":{"content":"create a file"}}
{"type":"assistant","message":{"content":"done"}}
`
	if err := os.WriteFile(nestedPath, []byte(transcript), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Create session state pointing to the FLAT (stale) path
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("failed to get HEAD: %v", err)
	}
	state := &SessionState{
		SessionID:      sessionID,
		BaseCommit:     head.Hash().String(),
		WorktreePath:   dir,
		AgentType:      agent.AgentTypeCursor,
		TranscriptPath: flatPath, // stale: file was relocated to nested path
	}
	if err := s.saveSessionState(context.Background(), state); err != nil {
		t.Fatalf("saveSessionState() error = %v", err)
	}

	// CondenseSession should succeed by re-resolving the transcript path
	checkpointID := id.MustCheckpointID("c1d2e3f4a5b6")
	result, err := s.CondenseSession(context.Background(), repo, checkpointID, state, nil)
	if err != nil {
		t.Fatalf("CondenseSession() error = %v, want nil (should re-resolve stale transcript path)", err)
	}

	if result.TotalTranscriptLines != 2 {
		t.Errorf("TotalTranscriptLines = %d, want 2", result.TotalTranscriptLines)
	}

	// State should have been updated to the resolved path
	if state.TranscriptPath != nestedPath {
		t.Errorf("state.TranscriptPath = %q, want %q (should be updated after re-resolution)", state.TranscriptPath, nestedPath)
	}
}

// TestCondenseSession_GeminiTranscript verifies that CondenseSession works correctly
// with Gemini JSON format transcripts, including prompt extraction and format detection.
func TestCondenseSession_GeminiTranscript(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create initial commit
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial content"), 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	if _, err := worktree.Add("test.txt"); err != nil {
		t.Fatalf("failed to stage file: %v", err)
	}
	_, err = worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	s := &ManualCommitStrategy{}
	sessionID := "2026-02-09-gemini-test"

	// Create metadata directory with Gemini JSON transcript
	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	if err := os.MkdirAll(metadataDirAbs, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}

	// Gemini JSON format with IDE tags to test stripping
	geminiTranscript := `{
		"sessionId": "test-session",
		"messages": [
			{
				"type": "user",
				"content": "<ide_opened_file>test.txt</ide_opened_file>Create a new file"
			},
			{
				"type": "gemini",
				"content": "I'll create the file for you",
				"tokens": {
					"input": 50,
					"output": 20,
					"cached": 10
				}
			}
		]
	}`

	if err := os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(geminiTranscript), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Write prompt.txt (simulating what lifecycle does at turn start / turn end)
	if err := os.WriteFile(filepath.Join(metadataDirAbs, paths.PromptFileName), []byte("Create a new file"), 0o644); err != nil {
		t.Fatalf("failed to write prompt file: %v", err)
	}

	// Create modified file
	if err := os.WriteFile(testFile, []byte("modified by gemini"), 0o644); err != nil {
		t.Fatalf("failed to modify file: %v", err)
	}

	// Save checkpoint (creates shadow branch)
	err = s.SaveStep(context.Background(), StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{"test.txt"},
		NewFiles:       []string{},
		DeletedFiles:   []string{},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Checkpoint 1",
		AuthorName:     "Gemini CLI",
		AuthorEmail:    "gemini@test.com",
		AgentType:      agent.AgentTypeGemini,
	})
	if err != nil {
		t.Fatalf("SaveStep() error = %v", err)
	}

	// Load session state
	state, err := s.loadSessionState(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("loadSessionState() error = %v", err)
	}
	if state.AgentType != agent.AgentTypeGemini {
		t.Errorf("AgentType = %q, want %q", state.AgentType, agent.AgentTypeGemini)
	}

	// Condense the session
	checkpointID := id.MustCheckpointID("aabbcc112233")
	result, err := s.CondenseSession(context.Background(), repo, checkpointID, state, nil)
	if err != nil {
		t.Fatalf("CondenseSession() error = %v", err)
	}

	// Verify result
	if result.CheckpointID != checkpointID {
		t.Errorf("CheckpointID = %v, want %v", result.CheckpointID, checkpointID)
	}
	if result.SessionID != sessionID {
		t.Errorf("SessionID = %q, want %q", result.SessionID, sessionID)
	}
	if len(result.FilesTouched) != 1 || result.FilesTouched[0] != "test.txt" {
		t.Errorf("FilesTouched = %v, want [test.txt]", result.FilesTouched)
	}

	// Verify condensed data on entire/checkpoints/v1 branch
	store := checkpoint.NewGitStore(repo)
	content, err := store.ReadLatestSessionContent(t.Context(), checkpointID)
	if err != nil {
		t.Fatalf("ReadLatestSessionContent() error = %v", err)
	}

	// Verify transcript was stored
	if len(content.Transcript) == 0 {
		t.Error("Transcript should not be empty")
	}

	// Verify prompts were extracted and IDE tags were stripped
	if !strings.Contains(content.Prompts, "Create a new file") {
		t.Errorf("Prompts = %q, should contain %q (IDE tags should be stripped)", content.Prompts, "Create a new file")
	}
	if strings.Contains(content.Prompts, "<ide_opened_file>") {
		t.Error("Prompts should not contain IDE tags")
	}

	// Verify token usage was calculated
	if content.Metadata.TokenUsage == nil {
		t.Fatal("TokenUsage should not be nil for Gemini transcript")
	}
	if content.Metadata.TokenUsage.InputTokens != 50 {
		t.Errorf("InputTokens = %d, want 50", content.Metadata.TokenUsage.InputTokens)
	}
	if content.Metadata.TokenUsage.OutputTokens != 20 {
		t.Errorf("OutputTokens = %d, want 20", content.Metadata.TokenUsage.OutputTokens)
	}
	if content.Metadata.TokenUsage.CacheReadTokens != 10 {
		t.Errorf("CacheReadTokens = %d, want 10", content.Metadata.TokenUsage.CacheReadTokens)
	}
}

// TestCondenseSession_GeminiMultiCheckpoint verifies that multi-checkpoint Gemini sessions
// correctly scope token usage to only the checkpoint portion (not the entire transcript).
// This is the core bug fix - ensuring CheckpointTranscriptStart is properly used.
//
//nolint:maintidx // Integration test with comprehensive verification steps
func TestCondenseSession_GeminiMultiCheckpoint(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create initial commit
	testFile := filepath.Join(dir, "code.go")
	if err := os.WriteFile(testFile, []byte("package main"), 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	if _, err := worktree.Add("code.go"); err != nil {
		t.Fatalf("failed to stage file: %v", err)
	}
	_, err = worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	s := &ManualCommitStrategy{}
	sessionID := "2026-02-09-multi-checkpoint"

	// Create metadata directory
	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	if err := os.MkdirAll(metadataDirAbs, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}

	transcriptPath := filepath.Join(metadataDirAbs, paths.TranscriptFileName)

	// CHECKPOINT 1: Initial work with 2 messages (1 gemini message with tokens)
	checkpoint1Transcript := `{
		"sessionId": "multi-test",
		"messages": [
			{
				"type": "user",
				"content": "Add a main function"
			},
			{
				"type": "gemini",
				"content": "I'll add a main function",
				"tokens": {
					"input": 100,
					"output": 50,
					"cached": 20
				}
			}
		]
	}`

	if err := os.WriteFile(transcriptPath, []byte(checkpoint1Transcript), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Write prompt.txt for checkpoint 1 (simulating what lifecycle does)
	if err := os.WriteFile(filepath.Join(metadataDirAbs, paths.PromptFileName), []byte("Add a main function"), 0o644); err != nil {
		t.Fatalf("failed to write prompt file: %v", err)
	}

	// Modify file for checkpoint 1
	if err := os.WriteFile(testFile, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatalf("failed to modify file: %v", err)
	}

	// Save checkpoint 1
	err = s.SaveStep(context.Background(), StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{"code.go"},
		NewFiles:       []string{},
		DeletedFiles:   []string{},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Checkpoint 1",
		AuthorName:     "Gemini CLI",
		AuthorEmail:    "gemini@test.com",
		AgentType:      agent.AgentTypeGemini,
	})
	if err != nil {
		t.Fatalf("SaveStep() checkpoint 1 error = %v", err)
	}

	// Load and verify state after checkpoint 1
	state, err := s.loadSessionState(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("loadSessionState() error = %v", err)
	}
	if state.CheckpointTranscriptStart != 0 {
		t.Errorf("CheckpointTranscriptStart after checkpoint 1 = %d, want 0", state.CheckpointTranscriptStart)
	}

	// CHECKPOINT 2: Add more messages to transcript (simulating continued session)
	// This adds 2 more messages (indices 2 and 3), with new token counts
	checkpoint2Transcript := `{
		"sessionId": "multi-test",
		"messages": [
			{
				"type": "user",
				"content": "Add a main function"
			},
			{
				"type": "gemini",
				"content": "I'll add a main function",
				"tokens": {
					"input": 100,
					"output": 50,
					"cached": 20
				}
			},
			{
				"type": "user",
				"content": "Now add error handling"
			},
			{
				"type": "gemini",
				"content": "I'll add error handling",
				"tokens": {
					"input": 200,
					"output": 75,
					"cached": 30
				}
			}
		]
	}`

	if err := os.WriteFile(transcriptPath, []byte(checkpoint2Transcript), 0o644); err != nil {
		t.Fatalf("failed to update transcript: %v", err)
	}

	// Simulate condensation clearing prompt.txt (condenseAndUpdateState does this),
	// then lifecycle appending the new prompt at turn start.
	if err := os.WriteFile(filepath.Join(metadataDirAbs, paths.PromptFileName), []byte("Now add error handling"), 0o644); err != nil {
		t.Fatalf("failed to write prompt file: %v", err)
	}

	// Modify file for checkpoint 2
	if err := os.WriteFile(testFile, []byte("package main\n\nfunc main() {\n\tif err := run(); err != nil {\n\t\tpanic(err)\n\t}\n}\n"), 0o644); err != nil {
		t.Fatalf("failed to modify file: %v", err)
	}

	// Before checkpoint 2, manually update CheckpointTranscriptStart to simulate
	// what would happen after condensing checkpoint 1
	state.CheckpointTranscriptStart = 2 // Start from message index 2 (the second user prompt)
	state.StepCount = 1                 // Set to 1 (will be incremented to 2 by SaveStep)
	if err := s.saveSessionState(context.Background(), state); err != nil {
		t.Fatalf("failed to update session state: %v", err)
	}

	// Save checkpoint 2
	err = s.SaveStep(context.Background(), StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{"code.go"},
		NewFiles:       []string{},
		DeletedFiles:   []string{},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Checkpoint 2",
		AuthorName:     "Gemini CLI",
		AuthorEmail:    "gemini@test.com",
		AgentType:      agent.AgentTypeGemini,
	})
	if err != nil {
		t.Fatalf("SaveStep() checkpoint 2 error = %v", err)
	}

	// Reload state to get updated values
	state, err = s.loadSessionState(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("loadSessionState() error = %v", err)
	}

	// Condense the session - this should calculate token usage ONLY from message index 2 onwards
	checkpointID := id.MustCheckpointID("ddeeff998877")
	result, err := s.CondenseSession(context.Background(), repo, checkpointID, state, nil)
	if err != nil {
		t.Fatalf("CondenseSession() error = %v", err)
	}

	// Verify result
	if result.CheckpointsCount != 2 {
		t.Errorf("CheckpointsCount = %d, want 2", result.CheckpointsCount)
	}
	if result.TotalTranscriptLines != 4 {
		t.Errorf("TotalTranscriptLines = %d, want 4 (4 messages in Gemini format)", result.TotalTranscriptLines)
	}

	// Read condensed metadata
	store := checkpoint.NewGitStore(repo)
	content, err := store.ReadLatestSessionContent(t.Context(), checkpointID)
	if err != nil {
		t.Fatalf("ReadLatestSessionContent() error = %v", err)
	}

	// CRITICAL VERIFICATION: Token usage should ONLY count from message index 2 onwards
	// This means ONLY the second gemini message (indices 2-3), NOT the first one (indices 0-1)
	if content.Metadata.TokenUsage == nil {
		t.Fatal("TokenUsage should not be nil")
	}

	// Expected: Only the second gemini message tokens (input=200, output=75, cached=30)
	// NOT the first gemini message tokens (input=100, output=50, cached=20)
	if content.Metadata.TokenUsage.InputTokens != 200 {
		t.Errorf("InputTokens = %d, want 200 (should only count from checkpoint start, not entire transcript)",
			content.Metadata.TokenUsage.InputTokens)
	}
	if content.Metadata.TokenUsage.OutputTokens != 75 {
		t.Errorf("OutputTokens = %d, want 75 (should only count from checkpoint start, not entire transcript)",
			content.Metadata.TokenUsage.OutputTokens)
	}
	if content.Metadata.TokenUsage.CacheReadTokens != 30 {
		t.Errorf("CacheReadTokens = %d, want 30 (should only count from checkpoint start, not entire transcript)",
			content.Metadata.TokenUsage.CacheReadTokens)
	}
	if content.Metadata.TokenUsage.APICallCount != 1 {
		t.Errorf("APICallCount = %d, want 1 (only one gemini message after checkpoint start)",
			content.Metadata.TokenUsage.APICallCount)
	}

	// Verify the full transcript is stored (all 4 messages)
	if len(content.Transcript) == 0 {
		t.Error("Full transcript should be stored")
	}

	// Verify only checkpoint-scoped prompts are present (from CheckpointTranscriptStart onwards)
	if strings.Contains(content.Prompts, "Add a main function") {
		t.Error("Prompts should NOT contain first prompt (before checkpoint start)")
	}
	if !strings.Contains(content.Prompts, "Now add error handling") {
		t.Error("Prompts should contain second prompt (checkpoint-scoped)")
	}
}

func TestCondenseSession_CopilotScopedCheckpointMetadataAndSessionBackfill(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	initialHash, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author:            &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
		AllowEmptyCommits: true,
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	t.Chdir(dir)

	sessionID := "2026-03-17-copilot-token-scope"
	transcriptDir := filepath.Join(dir, ".copilot", "session-state", sessionID)
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		t.Fatalf("failed to create transcript dir: %v", err)
	}
	transcriptPath := filepath.Join(transcriptDir, "events.jsonl")

	transcript := strings.Join([]string{
		`{"type":"session.start","data":{"sessionId":"2026-03-17-copilot-token-scope"},"id":"1","timestamp":"2026-03-17T00:00:00Z","parentId":""}`,
		`{"type":"session.model_change","data":{"newModel":"claude-sonnet-4.6"},"id":"2","timestamp":"2026-03-17T00:00:01Z","parentId":"1"}`,
		`{"type":"user.message","data":{"content":"Create alpha.txt"},"id":"3","timestamp":"2026-03-17T00:00:02Z","parentId":""}`,
		`{"type":"assistant.message","data":{"content":"Created alpha.txt","outputTokens":10},"id":"4","timestamp":"2026-03-17T00:00:03Z","parentId":"3"}`,
		`{"type":"tool.execution_complete","data":{"toolCallId":"tool-1","model":"claude-sonnet-4.6","toolTelemetry":{"properties":{"filePaths":"[\"alpha.txt\"]"},"metrics":{"linesAdded":1,"linesRemoved":0}}},"id":"5","timestamp":"2026-03-17T00:00:04Z","parentId":"4"}`,
		`{"type":"user.message","data":{"content":"Create beta.txt"},"id":"6","timestamp":"2026-03-17T00:00:05Z","parentId":""}`,
		`{"type":"assistant.message","data":{"content":"Created beta.txt","outputTokens":25},"id":"7","timestamp":"2026-03-17T00:00:06Z","parentId":"6"}`,
		`{"type":"tool.execution_complete","data":{"toolCallId":"tool-2","model":"claude-sonnet-4.6","toolTelemetry":{"properties":{"filePaths":"[\"beta.txt\"]"},"metrics":{"linesAdded":1,"linesRemoved":0}}},"id":"8","timestamp":"2026-03-17T00:00:07Z","parentId":"7"}`,
		`{"type":"session.shutdown","data":{"modelMetrics":{"claude-sonnet-4.6":{"requests":{"count":2},"usage":{"inputTokens":0,"outputTokens":35,"cacheReadTokens":20,"cacheWriteTokens":10}}}},"id":"9","timestamp":"2026-03-17T00:00:08Z","parentId":""}`,
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(transcript), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	state := &SessionState{
		SessionID:                 sessionID,
		BaseCommit:                initialHash.String(),
		StartedAt:                 time.Now(),
		FilesTouched:              []string{"beta.txt"},
		WorktreePath:              dir,
		TranscriptPath:            transcriptPath,
		AgentType:                 agent.AgentTypeCopilotCLI,
		ModelName:                 "claude-sonnet-4.6",
		CheckpointTranscriptStart: 5,
	}

	s := &ManualCommitStrategy{}
	checkpointID := id.MustCheckpointID("cc11aa22bb33")
	result, err := s.CondenseSession(context.Background(), repo, checkpointID, state, nil)
	if err != nil {
		t.Fatalf("CondenseSession() error = %v", err)
	}

	if result.CheckpointID != checkpointID {
		t.Errorf("CheckpointID = %v, want %v", result.CheckpointID, checkpointID)
	}
	if len(result.FilesTouched) != 1 || result.FilesTouched[0] != "beta.txt" {
		t.Errorf("FilesTouched = %v, want [beta.txt]", result.FilesTouched)
	}

	store := checkpoint.NewGitStore(repo)
	content, err := store.ReadLatestSessionContent(t.Context(), checkpointID)
	if err != nil {
		t.Fatalf("ReadLatestSessionContent() error = %v", err)
	}

	if content.Metadata.TokenUsage == nil {
		t.Fatal("TokenUsage should not be nil")
	}
	if content.Metadata.TokenUsage.InputTokens != 0 {
		t.Errorf("metadata InputTokens = %d, want 0 for scoped Copilot checkpoint usage", content.Metadata.TokenUsage.InputTokens)
	}
	if content.Metadata.TokenUsage.OutputTokens != 25 {
		t.Errorf("metadata OutputTokens = %d, want 25 for second checkpoint assistant output", content.Metadata.TokenUsage.OutputTokens)
	}
	if content.Metadata.TokenUsage.CacheReadTokens != 0 {
		t.Errorf("metadata CacheReadTokens = %d, want 0 for scoped fallback path", content.Metadata.TokenUsage.CacheReadTokens)
	}
	if content.Metadata.TokenUsage.CacheCreationTokens != 0 {
		t.Errorf("metadata CacheCreationTokens = %d, want 0 for scoped fallback path", content.Metadata.TokenUsage.CacheCreationTokens)
	}
	if content.Metadata.TokenUsage.APICallCount != 1 {
		t.Errorf("metadata APICallCount = %d, want 1", content.Metadata.TokenUsage.APICallCount)
	}

	if state.TokenUsage == nil {
		t.Fatal("state.TokenUsage should not be nil after Copilot session backfill")
	}
	if state.TokenUsage.InputTokens != 0 {
		t.Errorf("state InputTokens = %d, want 0 from session.shutdown", state.TokenUsage.InputTokens)
	}
	if state.TokenUsage.OutputTokens != 35 {
		t.Errorf("state OutputTokens = %d, want 35 from session.shutdown", state.TokenUsage.OutputTokens)
	}
	if state.TokenUsage.CacheReadTokens != 20 {
		t.Errorf("state CacheReadTokens = %d, want 20 from session.shutdown", state.TokenUsage.CacheReadTokens)
	}
	if state.TokenUsage.CacheCreationTokens != 10 {
		t.Errorf("state CacheCreationTokens = %d, want 10 from session.shutdown", state.TokenUsage.CacheCreationTokens)
	}
	if state.TokenUsage.APICallCount != 2 {
		t.Errorf("state APICallCount = %d, want 2 from session.shutdown", state.TokenUsage.APICallCount)
	}
}

// TestCondenseSession_FilesTouchedFallback_EmptyState verifies that when state.FilesTouched
// is empty (mid-session commit before SaveStep), the fallback to committedFiles works.
// This is the legitimate use case for the fallback.
func TestCondenseSession_FilesTouchedFallback_EmptyState(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create initial commit
	initialHash, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author:            &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
		AllowEmptyCommits: true,
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create a file and commit it (simulating agent mid-turn commit)
	agentFile := filepath.Join(dir, "agent.go")
	if err := os.WriteFile(agentFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	if _, err := worktree.Add("agent.go"); err != nil {
		t.Fatalf("failed to stage file: %v", err)
	}
	if _, err = worktree.Commit("Add agent.go", &git.CommitOptions{
		Author: &object.Signature{Name: "Agent", Email: "agent@test.com", When: time.Now()},
	}); err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	// Create live transcript (required when no shadow branch)
	transcriptDir := filepath.Join(dir, ".claude", "projects", "test")
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		t.Fatalf("failed to create transcript dir: %v", err)
	}
	transcriptFile := filepath.Join(transcriptDir, "session.jsonl")
	if err := os.WriteFile(transcriptFile, []byte(`{"type":"human","message":{"content":"create agent.go"}}
{"type":"assistant","message":{"content":"Done"}}
`), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Session state with EMPTY FilesTouched (mid-session commit scenario)
	state := &SessionState{
		SessionID:      "test-empty-files",
		BaseCommit:     initialHash.String(),
		FilesTouched:   []string{}, // Empty - no SaveStep called yet
		TranscriptPath: transcriptFile,
		AgentType:      "Claude Code",
	}

	s := &ManualCommitStrategy{}
	checkpointID := id.MustCheckpointID("fa11bac00001")

	// Condense with committedFiles - should fallback since FilesTouched is empty
	committedFiles := map[string]struct{}{"agent.go": {}}
	result, err := s.CondenseSession(context.Background(), repo, checkpointID, state, committedFiles)
	if err != nil {
		t.Fatalf("CondenseSession() error = %v", err)
	}

	// Read metadata and verify files_touched contains the committed file (fallback worked)
	sessionsRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("failed to get sessions branch: %v", err)
	}
	sessionsCommit, err := repo.CommitObject(sessionsRef.Hash())
	if err != nil {
		t.Fatalf("failed to get sessions commit: %v", err)
	}
	tree, err := sessionsCommit.Tree()
	if err != nil {
		t.Fatalf("failed to get tree: %v", err)
	}

	metadataPath := checkpointID.Path() + "/0/" + paths.MetadataFileName
	metadataFile, err := tree.File(metadataPath)
	if err != nil {
		t.Fatalf("failed to find metadata: %v", err)
	}
	content, err := metadataFile.Contents()
	if err != nil {
		t.Fatalf("failed to read metadata: %v", err)
	}

	var metadata struct {
		FilesTouched []string `json:"files_touched"`
	}
	if err := json.Unmarshal([]byte(content), &metadata); err != nil {
		t.Fatalf("failed to parse metadata: %v", err)
	}

	// Verify fallback worked - files_touched should contain agent.go
	if len(metadata.FilesTouched) != 1 || metadata.FilesTouched[0] != "agent.go" {
		t.Errorf("files_touched = %v, want [agent.go] (fallback should apply when FilesTouched is empty)",
			metadata.FilesTouched)
	}

	t.Logf("Fallback worked: files_touched = %v, result = %+v", metadata.FilesTouched, result)
}

// TestCondenseSession_FilesTouchedNoFallback_NoOverlap verifies that when state.FilesTouched
// has files but none overlap with committedFiles, we do NOT fallback to committedFiles.
// This prevents the bug where unrelated sessions get incorrect files_touched.
func TestCondenseSession_FilesTouchedNoFallback_NoOverlap(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create initial commit
	initialHash, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author:            &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
		AllowEmptyCommits: true,
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create files for both the session's work and the committed file
	sessionFile := filepath.Join(dir, "session_file.go")
	if err := os.WriteFile(sessionFile, []byte("package session\n"), 0o644); err != nil {
		t.Fatalf("failed to write session file: %v", err)
	}
	committedFile := filepath.Join(dir, "other_file.go")
	if err := os.WriteFile(committedFile, []byte("package other\n"), 0o644); err != nil {
		t.Fatalf("failed to write committed file: %v", err)
	}

	// Only commit the "other" file (not the session's file)
	if _, err := worktree.Add("other_file.go"); err != nil {
		t.Fatalf("failed to stage file: %v", err)
	}
	if _, err = worktree.Commit("Add other_file.go", &git.CommitOptions{
		Author: &object.Signature{Name: "Human", Email: "human@test.com", When: time.Now()},
	}); err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	// Create live transcript
	transcriptDir := filepath.Join(dir, ".claude", "projects", "test")
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		t.Fatalf("failed to create transcript dir: %v", err)
	}
	transcriptFile := filepath.Join(transcriptDir, "session.jsonl")
	if err := os.WriteFile(transcriptFile, []byte(`{"type":"human","message":{"content":"work on session_file.go"}}
{"type":"assistant","message":{"content":"Done"}}
`), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Session state with FilesTouched that does NOT overlap with committedFiles
	state := &SessionState{
		SessionID:      "test-no-overlap",
		BaseCommit:     initialHash.String(),
		FilesTouched:   []string{"session_file.go"}, // Does NOT overlap with other_file.go
		TranscriptPath: transcriptFile,
		AgentType:      "Claude Code",
	}

	s := &ManualCommitStrategy{}
	checkpointID := id.MustCheckpointID("00001a000001")

	// Condense with committedFiles that don't overlap
	committedFiles := map[string]struct{}{"other_file.go": {}}
	result, err := s.CondenseSession(context.Background(), repo, checkpointID, state, committedFiles)
	if err != nil {
		t.Fatalf("CondenseSession() error = %v", err)
	}

	// Read metadata and verify files_touched is EMPTY (no fallback applied)
	sessionsRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("failed to get sessions branch: %v", err)
	}
	sessionsCommit, err := repo.CommitObject(sessionsRef.Hash())
	if err != nil {
		t.Fatalf("failed to get sessions commit: %v", err)
	}
	tree, err := sessionsCommit.Tree()
	if err != nil {
		t.Fatalf("failed to get tree: %v", err)
	}

	metadataPath := checkpointID.Path() + "/0/" + paths.MetadataFileName
	metadataFile, err := tree.File(metadataPath)
	if err != nil {
		t.Fatalf("failed to find metadata: %v", err)
	}
	content, err := metadataFile.Contents()
	if err != nil {
		t.Fatalf("failed to read metadata: %v", err)
	}

	var metadata struct {
		FilesTouched []string `json:"files_touched"`
	}
	if err := json.Unmarshal([]byte(content), &metadata); err != nil {
		t.Fatalf("failed to parse metadata: %v", err)
	}

	// Verify NO fallback - files_touched should be EMPTY, NOT contain other_file.go
	// This is the key fix: session had files (session_file.go) but none overlapped,
	// so we should NOT fallback to committedFiles (other_file.go)
	if len(metadata.FilesTouched) != 0 {
		t.Errorf("files_touched = %v, want [] (should NOT fallback when session had files but no overlap)",
			metadata.FilesTouched)
	}

	t.Logf("No fallback applied: files_touched = %v (correctly empty), result = %+v", metadata.FilesTouched, result)
}

// TestExtractFilesFromLiveTranscript_RespectsOffset verifies that after condensation
// sets CheckpointTranscriptStart = N, resolveFilesTouched only returns
// files from messages at index N and beyond, not from the beginning.
//
// This is a regression test for a bug where compaction events (pre-compress hooks)
// unconditionally reset CheckpointTranscriptStart to 0, causing already-condensed
// files to re-appear in carry-forward and break sequential commit scenarios.
func TestExtractFilesFromLiveTranscript_RespectsOffset(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	// Create a Gemini-format transcript with 3 file writes at different message indices:
	//   msg 0: user prompt
	//   msg 1: gemini writes red.md      (already condensed)
	//   msg 2: user prompt
	//   msg 3: gemini writes blue.md     (already condensed)
	//   msg 4: user prompt
	//   msg 5: gemini writes green.md    (new, should be extracted)
	transcript := `{
  "messages": [
    {"type": "user", "content": [{"text": "create red.md"}]},
    {"type": "gemini", "content": "", "toolCalls": [{"name": "write_file", "args": {"file_path": "docs/red.md"}}]},
    {"type": "user", "content": [{"text": "create blue.md"}]},
    {"type": "gemini", "content": "", "toolCalls": [{"name": "write_file", "args": {"file_path": "docs/blue.md"}}]},
    {"type": "user", "content": [{"text": "create green.md"}]},
    {"type": "gemini", "content": "", "toolCalls": [{"name": "write_file", "args": {"file_path": "docs/green.md"}}]}
  ]
}`

	transcriptPath := filepath.Join(dir, "transcript.json")
	if err := os.WriteFile(transcriptPath, []byte(transcript), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Simulate state after 2 condensations: offset points past blue.md's message
	state := &SessionState{
		SessionID:                 "test-offset-session",
		TranscriptPath:            transcriptPath,
		AgentType:                 agent.AgentTypeGemini,
		WorktreePath:              dir,
		CheckpointTranscriptStart: 4, // Past red.md (msg 1) and blue.md (msg 3)
	}

	// With correct offset (4): should only find green.md
	files := s.resolveFilesTouched(context.Background(), state)
	if len(files) != 1 || files[0] != "docs/green.md" {
		t.Errorf("resolveFilesTouched(offset=4) = %v, want [docs/green.md]", files)
	}

	// With reset offset (0): would incorrectly find all 3 files (the bug)
	state.CheckpointTranscriptStart = 0
	allFiles := s.resolveFilesTouched(context.Background(), state)
	if len(allFiles) != 3 {
		t.Errorf("resolveFilesTouched(offset=0) got %d files, want 3: %v", len(allFiles), allFiles)
	}
}

// TestResolveFilesTouched_PrefersStateFallsBackToTranscript verifies the two-tier
// resolution in resolveFilesTouched: state.FilesTouched is preferred (returns a copy),
// and transcript extraction is only used as a fallback when FilesTouched is empty.
func TestResolveFilesTouched_PrefersStateFallsBackToTranscript(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	// Gemini transcript containing a file write
	transcript := `{
  "messages": [
    {"type": "user", "content": [{"text": "create file"}]},
    {"type": "gemini", "content": "", "toolCalls": [{"name": "write_file", "args": {"file_path": "from-transcript.txt"}}]}
  ]
}`
	transcriptPath := filepath.Join(dir, "transcript.json")
	if err := os.WriteFile(transcriptPath, []byte(transcript), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	t.Run("prefers FilesTouched over transcript", func(t *testing.T) {
		state := &SessionState{
			SessionID:      "test-prefers-state",
			TranscriptPath: transcriptPath,
			AgentType:      agent.AgentTypeGemini,
			WorktreePath:   dir,
			FilesTouched:   []string{"from-hook.txt"},
		}
		files := s.resolveFilesTouched(context.Background(), state)
		if len(files) != 1 || files[0] != "from-hook.txt" {
			t.Errorf("resolveFilesTouched with FilesTouched = %v, want [from-hook.txt]", files)
		}
	})

	t.Run("returns copy of FilesTouched", func(t *testing.T) {
		state := &SessionState{
			SessionID:    "test-copy",
			FilesTouched: []string{"a.txt", "b.txt"},
		}
		files := s.resolveFilesTouched(context.Background(), state)
		// Mutating returned slice should not affect state
		files[0] = "mutated.txt"
		if state.FilesTouched[0] != "a.txt" {
			t.Errorf("resolveFilesTouched did not return a copy; state.FilesTouched[0] = %q", state.FilesTouched[0])
		}
	})

	t.Run("falls back to transcript when FilesTouched is empty", func(t *testing.T) {
		state := &SessionState{
			SessionID:      "test-fallback",
			TranscriptPath: transcriptPath,
			AgentType:      agent.AgentTypeGemini,
			WorktreePath:   dir,
			FilesTouched:   nil,
		}
		files := s.resolveFilesTouched(context.Background(), state)
		if len(files) != 1 || files[0] != "from-transcript.txt" {
			t.Errorf("resolveFilesTouched with empty FilesTouched = %v, want [from-transcript.txt]", files)
		}
	})

	t.Run("returns nil when both sources are empty", func(t *testing.T) {
		state := &SessionState{
			SessionID:    "test-empty",
			FilesTouched: nil,
			// No transcript path — extraction will return nil
		}
		files := s.resolveFilesTouched(context.Background(), state)
		if files != nil {
			t.Errorf("resolveFilesTouched with no sources = %v, want nil", files)
		}
	})
}

// TestCondenseSession_V2DualWrite verifies that when checkpoints_v2 is enabled,
// CondenseSession writes to both v1 (entire/checkpoints/v1) and v2 refs
// (refs/entire/checkpoints/v2/main and refs/entire/checkpoints/v2/full/current).
func TestCondenseSession_V2DualWrite(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	require.NoError(t, err)

	worktree, err := repo.Worktree()
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644))
	_, err = worktree.Add("main.go")
	require.NoError(t, err)
	commitHash, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	t.Chdir(dir)

	// Enable checkpoints_v2 via settings
	entireDir := filepath.Join(dir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(testCheckpointsV2SettingsJSON), 0o644))

	s := &ManualCommitStrategy{}
	sessionID := "2025-01-15-test-v2-dual-write"

	// Create metadata directory with transcript
	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	require.NoError(t, os.MkdirAll(metadataDirAbs, 0o755))

	secret := "q9Xv2Lm8Rt1Yp4Kd7Wz0Hs6Nc3Bf5Jg"
	transcript := `{"type":"human","message":{"content":"hello secret: ` + secret + `"}}
{"type":"assistant","message":{"content":"hi there"}}
`
	require.NoError(t, os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(transcript), 0o644))

	// SaveStep to create shadow branch
	err = s.SaveStep(context.Background(), StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{"main.go"},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Checkpoint 1",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	})
	require.NoError(t, err)

	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	state.TranscriptPath = filepath.Join(metadataDirAbs, paths.TranscriptFileName)
	state.BaseCommit = commitHash.String()[:7]
	state.AgentType = agent.AgentTypeClaudeCode

	checkpointID := id.MustCheckpointID("dd11ee22ff33")
	result, err := s.CondenseSession(context.Background(), repo, checkpointID, state, nil)
	require.NoError(t, err)
	require.NotNil(t, result)

	// v1 branch should exist (as before)
	v1Ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err, "v1 metadata branch should exist")
	require.NotEqual(t, plumbing.ZeroHash, v1Ref.Hash())

	// v2 /main ref should exist
	v2MainRef, err := repo.Reference(plumbing.ReferenceName(paths.V2MainRefName), true)
	require.NoError(t, err, "v2 /main ref should exist")
	require.NotEqual(t, plumbing.ZeroHash, v2MainRef.Hash())

	// v2 /full/current ref should exist (transcript was non-empty)
	v2FullRef, err := repo.Reference(plumbing.ReferenceName(paths.V2FullCurrentRefName), true)
	require.NoError(t, err, "v2 /full/current ref should exist")
	require.NotEqual(t, plumbing.ZeroHash, v2FullRef.Hash())

	// Verify /main has metadata and redacted compact transcript
	v2MainCommit, err := repo.CommitObject(v2MainRef.Hash())
	require.NoError(t, err)
	v2MainTree, err := v2MainCommit.Tree()
	require.NoError(t, err)

	cpPath := checkpointID.Path()
	mainCpTree, err := v2MainTree.Tree(cpPath)
	require.NoError(t, err)

	// Root metadata.json should exist
	_, err = mainCpTree.File(paths.MetadataFileName)
	require.NoError(t, err, "root metadata.json should exist on /main")

	mainSessionTree, err := mainCpTree.Tree("0")
	require.NoError(t, err)
	compactFile, err := mainSessionTree.File(paths.CompactTranscriptFileName)
	require.NoError(t, err, "transcript.jsonl should exist on /main")
	compactContent, err := compactFile.Contents()
	require.NoError(t, err)
	require.NotContains(t, compactContent, secret, "compact transcript on /main must be redacted")

	// Verify /full/current has transcript
	v2FullCommit, err := repo.CommitObject(v2FullRef.Hash())
	require.NoError(t, err)
	v2FullTree, err := v2FullCommit.Tree()
	require.NoError(t, err)

	fullCpTree, err := v2FullTree.Tree(cpPath)
	require.NoError(t, err)
	fullSessionTree, err := fullCpTree.Tree("0")
	require.NoError(t, err)
	_, err = fullSessionTree.File(paths.V2RawTranscriptFileName)
	require.NoError(t, err, "raw_transcript should exist on /full/current")
}

func TestCondenseSession_V2DualWrite_CopiesTaskMetadataToFullCurrent(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "main.go", "package main")
	testutil.GitAdd(t, dir, "main.go")
	testutil.GitCommit(t, dir, "Initial commit")

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	commitHash := testutil.GetHeadHash(t, dir)

	t.Chdir(dir)

	entireDir := filepath.Join(dir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(testCheckpointsV2SettingsJSON), 0o644))

	s := &ManualCommitStrategy{}
	sessionID := "2025-01-15-test-v2-task-dual-write"

	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	require.NoError(t, os.MkdirAll(metadataDirAbs, 0o755))

	transcript := `{"type":"human","message":{"content":"hello"}}
{"type":"assistant","message":{"content":"hi there"}}
`
	transcriptPath := filepath.Join(metadataDirAbs, paths.TranscriptFileName)
	require.NoError(t, os.WriteFile(transcriptPath, []byte(transcript), 0o644))

	// Create shadow branch/session checkpoint data.
	err = s.SaveStep(context.Background(), StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{"main.go"},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Checkpoint 1",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	})
	require.NoError(t, err)

	subagentTranscriptPath := filepath.Join(metadataDirAbs, "subagent.jsonl")
	require.NoError(t, os.WriteFile(subagentTranscriptPath, []byte("{\"type\":\"event\",\"message\":\"done\"}\n"), 0o644))

	err = s.SaveTaskStep(context.Background(), TaskStepContext{
		SessionID:              sessionID,
		ToolUseID:              "toolu_01TASK",
		AgentID:                "agent-01",
		ModifiedFiles:          []string{"main.go"},
		TranscriptPath:         transcriptPath,
		SubagentTranscriptPath: subagentTranscriptPath,
		CheckpointUUID:         "uuid-task-001",
		AuthorName:             "Test",
		AuthorEmail:            "test@test.com",
		SubagentType:           "general",
		TaskDescription:        "Implement task",
		AgentType:              agent.AgentTypeClaudeCode,
	})
	require.NoError(t, err)

	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	state.TranscriptPath = transcriptPath
	state.BaseCommit = commitHash[:7]
	state.AgentType = agent.AgentTypeClaudeCode

	checkpointID := id.MustCheckpointID("ab11cd22ef33")
	result, err := s.CondenseSession(context.Background(), repo, checkpointID, state, nil)
	require.NoError(t, err)
	require.NotNil(t, result)

	v2FullRef, err := repo.Reference(plumbing.ReferenceName(paths.V2FullCurrentRefName), true)
	require.NoError(t, err, "v2 /full/current ref should exist")

	v2FullCommit, err := repo.CommitObject(v2FullRef.Hash())
	require.NoError(t, err)
	v2FullTree, err := v2FullCommit.Tree()
	require.NoError(t, err)

	taskCheckpointPath := checkpointID.Path() + "/0/tasks/toolu_01TASK/checkpoint.json"
	_, err = v2FullTree.File(taskCheckpointPath)
	require.NoError(t, err, "task checkpoint metadata should be copied to v2 /full/current")
}

// TestCondenseSession_V2CompactTranscriptStart verifies v2 /main writes
// checkpoint_transcript_start from compact transcript offset, not full.jsonl offset.
func TestCondenseSession_V2CompactTranscriptStart(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "main.go", "package main")
	testutil.GitAdd(t, dir, "main.go")
	testutil.GitCommit(t, dir, "Initial commit")

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	commitHash := testutil.GetHeadHash(t, dir)

	t.Chdir(dir)

	// Enable checkpoints_v2 via settings
	entireDir := filepath.Join(dir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(testCheckpointsV2SettingsJSON), 0o644))

	s := &ManualCommitStrategy{}
	sessionID := "2025-01-15-test-v2-compact-start"

	// Create metadata directory with transcript
	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	require.NoError(t, os.MkdirAll(metadataDirAbs, 0o755))

	transcript := `{"type":"human","message":{"content":"hello"}}
{"type":"assistant","message":{"content":"hi there"}}
`
	require.NoError(t, os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(transcript), 0o644))

	// SaveStep to create shadow branch
	err = s.SaveStep(context.Background(), StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{"main.go"},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Checkpoint 1",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	})
	require.NoError(t, err)

	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	state.TranscriptPath = filepath.Join(metadataDirAbs, paths.TranscriptFileName)
	state.BaseCommit = commitHash[:7]
	state.AgentType = agent.AgentTypeClaudeCode

	// First condensation starts at compact offset 0.
	checkpointID := id.MustCheckpointID("cc11dd22ee33")
	result, err := s.CondenseSession(context.Background(), repo, checkpointID, state, nil)
	require.NoError(t, err)
	require.NotNil(t, result)

	// v2 /main should have checkpoint_transcript_start = 0 for first checkpoint.
	v2MainRef, err := repo.Reference(plumbing.ReferenceName(paths.V2MainRefName), true)
	require.NoError(t, err)
	v2MainCommit, err := repo.CommitObject(v2MainRef.Hash())
	require.NoError(t, err)
	v2MainTree, err := v2MainCommit.Tree()
	require.NoError(t, err)

	cpPath := checkpointID.Path()
	sessionTree, err := v2MainTree.Tree(cpPath + "/0")
	require.NoError(t, err)
	metadataFile, err := sessionTree.File(paths.MetadataFileName)
	require.NoError(t, err)
	metadataContent, err := metadataFile.Contents()
	require.NoError(t, err)

	var v2Metadata checkpoint.CommittedMetadata
	require.NoError(t, json.Unmarshal([]byte(metadataContent), &v2Metadata))
	require.Equal(t, 0, v2Metadata.CheckpointTranscriptStart,
		"first checkpoint v2 metadata should have checkpoint_transcript_start=0")

	// Read v1 metadata for comparison.
	v1Ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)
	v1Commit, err := repo.CommitObject(v1Ref.Hash())
	require.NoError(t, err)
	v1Tree, err := v1Commit.Tree()
	require.NoError(t, err)
	v1SessionTree, err := v1Tree.Tree(cpPath + "/0")
	require.NoError(t, err)
	v1MetadataFile, err := v1SessionTree.File(paths.MetadataFileName)
	require.NoError(t, err)
	v1MetadataContent, err := v1MetadataFile.Contents()
	require.NoError(t, err)

	var v1Metadata checkpoint.CommittedMetadata
	require.NoError(t, json.Unmarshal([]byte(v1MetadataContent), &v1Metadata))
	require.Equal(t, 0, v1Metadata.CheckpointTranscriptStart,
		"first checkpoint v1 metadata should also have checkpoint_transcript_start=0")

	// Verify compact transcript lines were counted in the result
	require.Positive(t, result.CompactTranscriptLines,
		"CondenseResult should report compact transcript lines")

	// Read compact transcript.jsonl from v2 /main for the first checkpoint.
	compactFile1, err := sessionTree.File(paths.CompactTranscriptFileName)
	require.NoError(t, err, "transcript.jsonl should exist on v2 /main")
	compactContent1, err := compactFile1.Contents()
	require.NoError(t, err)
	firstCompactLines := bytes.Count([]byte(compactContent1), []byte{'\n'})
	require.Positive(t, firstCompactLines, "first checkpoint compact transcript should have lines")

	// --- Second condensation: add more transcript content ---
	transcript2 := transcript + `{"type":"human","message":{"content":"next question"}}
{"type":"assistant","message":{"content":"next answer"}}
`
	require.NoError(t, os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(transcript2), 0o644))

	// Update state after first condensation (mimic what CondenseSessionByID does)
	state.StepCount = 0
	state.CheckpointTranscriptStart = result.TotalTranscriptLines
	state.CompactTranscriptStart += result.CompactTranscriptLines

	// SaveStep for second checkpoint
	testutil.WriteFile(t, dir, "main.go", "package main\n// v2")
	err = s.SaveStep(context.Background(), StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{"main.go"},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Checkpoint 2",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	})
	require.NoError(t, err)

	state2, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	state2.TranscriptPath = filepath.Join(metadataDirAbs, paths.TranscriptFileName)
	state2.BaseCommit = commitHash[:7]
	state2.AgentType = agent.AgentTypeClaudeCode
	state2.CheckpointTranscriptStart = state.CheckpointTranscriptStart
	state2.CompactTranscriptStart = state.CompactTranscriptStart

	checkpointID2 := id.MustCheckpointID("dd22ee33ff44")
	result2, err := s.CondenseSession(context.Background(), repo, checkpointID2, state2, nil)
	require.NoError(t, err)
	require.NotNil(t, result2)

	// v2 /main metadata for second checkpoint should have compact start = firstCompactLines.
	v2MainRef2, err := repo.Reference(plumbing.ReferenceName(paths.V2MainRefName), true)
	require.NoError(t, err)
	v2MainCommit2, err := repo.CommitObject(v2MainRef2.Hash())
	require.NoError(t, err)
	v2MainTree2, err := v2MainCommit2.Tree()
	require.NoError(t, err)

	cpPath2 := checkpointID2.Path()
	sessionTree2, err := v2MainTree2.Tree(cpPath2 + "/0")
	require.NoError(t, err)
	metadataFile2, err := sessionTree2.File(paths.MetadataFileName)
	require.NoError(t, err)
	metadataContent2, err := metadataFile2.Contents()
	require.NoError(t, err)

	var v2Metadata2 checkpoint.CommittedMetadata
	require.NoError(t, json.Unmarshal([]byte(metadataContent2), &v2Metadata2))
	require.Equal(t, firstCompactLines, v2Metadata2.CheckpointTranscriptStart,
		"second checkpoint v2 metadata should have checkpoint_transcript_start = first checkpoint's compact line count")

	// The compact transcript.jsonl for checkpoint 2 should be CUMULATIVE:
	// it should contain both checkpoint 1's and checkpoint 2's compact lines.
	compactFile2, err := sessionTree2.File(paths.CompactTranscriptFileName)
	require.NoError(t, err, "transcript.jsonl should exist for second checkpoint")
	compactContent2, err := compactFile2.Contents()
	require.NoError(t, err)
	secondCompactTotalLines := bytes.Count([]byte(compactContent2), []byte{'\n'})
	require.Greater(t, secondCompactTotalLines, firstCompactLines,
		"second checkpoint compact transcript should include all prior content plus new content")

	// The first checkpoint's content should be a prefix of the second checkpoint's content.
	require.True(t, strings.HasPrefix(compactContent2, compactContent1),
		"second checkpoint compact transcript should start with first checkpoint's content")
}

// TestCondenseSession_V2Disabled_NoV2Refs verifies that when checkpoints_v2 is
// not enabled, CondenseSession only writes to v1 and does not create v2 refs.
func TestCondenseSession_V2Disabled_NoV2Refs(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	require.NoError(t, err)

	worktree, err := repo.Worktree()
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644))
	_, err = worktree.Add("main.go")
	require.NoError(t, err)
	commitHash, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	t.Chdir(dir)

	// No checkpoints_v2 setting — default is disabled
	entireDir := filepath.Join(dir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	settingsJSON := `{"enabled": true, "strategy": "manual-commit"}`
	require.NoError(t, os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(settingsJSON), 0o644))

	s := &ManualCommitStrategy{}
	sessionID := "2025-01-15-test-v2-disabled"

	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	require.NoError(t, os.MkdirAll(metadataDirAbs, 0o755))

	transcript := `{"type":"human","message":{"content":"hello"}}
{"type":"assistant","message":{"content":"hi"}}
`
	require.NoError(t, os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(transcript), 0o644))

	err = s.SaveStep(context.Background(), StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{"main.go"},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Checkpoint 1",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	})
	require.NoError(t, err)

	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	state.TranscriptPath = filepath.Join(metadataDirAbs, paths.TranscriptFileName)
	state.BaseCommit = commitHash.String()[:7]

	checkpointID := id.MustCheckpointID("ee22ff33aa44")
	result, err := s.CondenseSession(context.Background(), repo, checkpointID, state, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 0, result.CompactTranscriptLines, "v2-disabled condensation should not report compact transcript line deltas")

	// v1 should exist
	_, err = repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err, "v1 metadata branch should exist")

	// v2 refs should NOT exist
	_, err = repo.Reference(plumbing.ReferenceName(paths.V2MainRefName), true)
	require.Error(t, err, "v2 /main ref should not exist when v2 is disabled")

	_, err = repo.Reference(plumbing.ReferenceName(paths.V2FullCurrentRefName), true)
	require.Error(t, err, "v2 /full/current ref should not exist when v2 is disabled")
}

func TestCondenseSession_RedactionFailure_DropsTranscriptButWritesMetadata(t *testing.T) {
	originalRedact := redactSessionJSONLBytes
	redactSessionJSONLBytes = func([]byte) (redact.RedactedBytes, error) {
		return redact.RedactedBytes{}, errors.New("forced redaction failure")
	}
	t.Cleanup(func() {
		redactSessionJSONLBytes = originalRedact
	})

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "main.go", "package main")
	testutil.GitAdd(t, dir, "main.go")
	testutil.GitCommit(t, dir, "Initial commit")

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	headRef, err := repo.Head()
	require.NoError(t, err)

	t.Chdir(dir)

	s := &ManualCommitStrategy{}
	sessionID := "2026-04-10-test-redaction-failure"

	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	require.NoError(t, os.MkdirAll(metadataDirAbs, 0o755))

	transcript := "{\"type\":\"human\",\"message\":{\"content\":\"hello\"}}\n"
	require.NoError(t, os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(transcript), 0o644))

	err = s.SaveStep(context.Background(), StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{"main.go"},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Checkpoint 1",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	})
	require.NoError(t, err)

	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	state.TranscriptPath = filepath.Join(metadataDirAbs, paths.TranscriptFileName)
	state.BaseCommit = headRef.Hash().String()[:7]
	state.AgentType = agent.AgentTypeClaudeCode
	state.FilesTouched = []string{"main.go"}

	checkpointID := id.MustCheckpointID("aa11bb22cc33")
	result, err := s.CondenseSession(context.Background(), repo, checkpointID, state, nil)
	require.NoError(t, err, "redaction failure should not abort condensation")
	require.NotNil(t, result)

	store, err := s.getCheckpointStore()
	require.NoError(t, err)

	committed, err := store.ListCommitted(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, committed)

	found := false
	for _, c := range committed {
		if c.CheckpointID == checkpointID {
			found = true
			break
		}
	}
	require.True(t, found, "checkpoint metadata should be written even when transcript redaction fails")

	_, err = store.ReadLatestSessionContent(context.Background(), checkpointID)
	require.ErrorIs(t, err, checkpoint.ErrNoTranscript, "transcript should be dropped when redaction fails")
}

func TestCommittedFilesExcludingMetadata(t *testing.T) {
	t.Parallel()

	input := map[string]struct{}{
		"docs/blue.md":          {},
		"docs/red.md":           {},
		".entire/settings.json": {},
		".entire/.gitignore":    {},
		".claude/settings.json": {},
	}

	result := committedFilesExcludingMetadata(input)

	// .entire/ files should be excluded, everything else kept
	resultSet := make(map[string]struct{}, len(result))
	for _, f := range result {
		resultSet[f] = struct{}{}
	}

	require.Contains(t, resultSet, "docs/blue.md")
	require.Contains(t, resultSet, "docs/red.md")
	require.Contains(t, resultSet, ".claude/settings.json")
	require.NotContains(t, resultSet, ".entire/settings.json", ".entire/ should be excluded")
	require.NotContains(t, resultSet, ".entire/.gitignore", ".entire/ should be excluded")
	require.Len(t, result, 3)
}

func TestMarshalPromptAttributionsIncludingPending_IncludesPending(t *testing.T) {
	t.Parallel()

	state := &SessionState{
		PromptAttributions: []PromptAttribution{
			{CheckpointNumber: 1, UserLinesAdded: 3},
		},
		PendingPromptAttribution: &PromptAttribution{
			CheckpointNumber: 2, UserLinesAdded: 5,
		},
	}

	raw := marshalPromptAttributionsIncludingPending(state)
	require.NotNil(t, raw)

	var result []PromptAttribution
	require.NoError(t, json.Unmarshal(raw, &result))
	require.Len(t, result, 2, "should include both committed and pending attributions")
	require.Equal(t, 1, result[0].CheckpointNumber)
	require.Equal(t, 3, result[0].UserLinesAdded)
	require.Equal(t, 2, result[1].CheckpointNumber)
	require.Equal(t, 5, result[1].UserLinesAdded)
}

func TestMarshalPromptAttributionsIncludingPending_NoPending(t *testing.T) {
	t.Parallel()

	state := &SessionState{
		PromptAttributions: []PromptAttribution{
			{CheckpointNumber: 1, UserLinesAdded: 3},
		},
	}

	raw := marshalPromptAttributionsIncludingPending(state)
	require.NotNil(t, raw)

	var result []PromptAttribution
	require.NoError(t, json.Unmarshal(raw, &result))
	require.Len(t, result, 1)
}

func TestMarshalPromptAttributionsIncludingPending_Empty(t *testing.T) {
	t.Parallel()

	state := &SessionState{}
	raw := marshalPromptAttributionsIncludingPending(state)
	require.Nil(t, raw, "empty state should return nil")
}

func TestMarshalPromptAttributionsIncludingPending_OnlyPending(t *testing.T) {
	t.Parallel()

	state := &SessionState{
		PendingPromptAttribution: &PromptAttribution{
			CheckpointNumber: 1, UserLinesAdded: 7,
		},
	}

	raw := marshalPromptAttributionsIncludingPending(state)
	require.NotNil(t, raw, "pending-only should still produce output")

	var result []PromptAttribution
	require.NoError(t, json.Unmarshal(raw, &result))
	require.Len(t, result, 1)
	require.Equal(t, 7, result[0].UserLinesAdded)
}

func TestCommittedFilesExcludingMetadata_AllMetadata(t *testing.T) {
	t.Parallel()

	result := committedFilesExcludingMetadata(map[string]struct{}{
		".entire/settings.json": {},
		".entire/.gitignore":    {},
	})
	require.Empty(t, result, "all metadata files should be excluded")
}
