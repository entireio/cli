package strategy

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const testTrailerCheckpointID id.CheckpointID = "a1b2c3d4e5f6"

func TestShadowStrategy_Registration(t *testing.T) {
	s, err := Get(StrategyNameManualCommit)
	if err != nil {
		t.Fatalf("Get(%q) error = %v", StrategyNameManualCommit, err)
	}
	if s == nil {
		t.Fatal("Get() returned nil strategy")
	}
	if s.Name() != StrategyNameManualCommit {
		t.Errorf("Name() = %q, want %q", s.Name(), StrategyNameManualCommit)
	}
}

func TestShadowStrategy_DirectInstantiation(t *testing.T) {
	// NewShadowStrategy delegates to NewManualCommitStrategy, so returns manual-commit name.
	s := NewManualCommitStrategy()
	if s.Name() != StrategyNameManualCommit {
		t.Errorf("Name() = %q, want %q", s.Name(), StrategyNameManualCommit)
	}
}

func TestShadowStrategy_Description(t *testing.T) {
	s := NewManualCommitStrategy()
	desc := s.Description()
	if desc == "" {
		t.Error("Description() returned empty string")
	}
}

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
	err = s.saveSessionState(state)
	if err != nil {
		t.Fatalf("saveSessionState() error = %v", err)
	}

	// Verify file exists
	stateFile := filepath.Join(".git", "entire-sessions", "test-session-123.json")
	if _, err := os.Stat(stateFile); os.IsNotExist(err) {
		t.Error("session state file not created")
	}

	// Load state
	loaded, err := s.loadSessionState("test-session-123")
	if err != nil {
		t.Fatalf("loadSessionState() error = %v", err)
	}
	if loaded == nil {
		t.Fatal("loadSessionState() returned nil")
	}

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

	loaded, err := s.loadSessionState("nonexistent-session")
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
	dummyCommitHash, err := createCommit(repo, emptyTreeHash, plumbing.ZeroHash, "dummy commit", "test", "test@test.com")
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

	if err := s.saveSessionState(state1); err != nil {
		t.Fatalf("saveSessionState() error = %v", err)
	}
	if err := s.saveSessionState(state2); err != nil {
		t.Fatalf("saveSessionState() error = %v", err)
	}

	// List all states
	states, err := s.listAllSessionStates()
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
		if err := s.saveSessionState(state); err != nil {
			t.Fatalf("saveSessionState(%s) error = %v", state.SessionID, err)
		}
	}

	states, err := s.listAllSessionStates()
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
		loaded, err := LoadSessionState(staleID)
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
	dummyCommitHash, err := createCommit(repo, emptyTreeHash, plumbing.ZeroHash, "dummy commit", "test", "test@test.com")
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
		if err := s.saveSessionState(state); err != nil {
			t.Fatalf("saveSessionState() error = %v", err)
		}
	}

	// Find sessions for base commit "abc1234"
	matching, err := s.findSessionsForCommit("abc1234")
	if err != nil {
		t.Fatalf("findSessionsForCommit() error = %v", err)
	}

	if len(matching) != 2 {
		t.Errorf("findSessionsForCommit() returned %d sessions, want 2", len(matching))
	}

	// Find sessions for base commit "xyz7890"
	matching, err = s.findSessionsForCommit("xyz7890")
	if err != nil {
		t.Fatalf("findSessionsForCommit() error = %v", err)
	}

	if len(matching) != 1 {
		t.Errorf("findSessionsForCommit() returned %d sessions, want 1", len(matching))
	}

	// Find sessions for nonexistent base commit
	matching, err = s.findSessionsForCommit("nonexistent")
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
	if err := s.saveSessionState(state); err != nil {
		t.Fatalf("saveSessionState() error = %v", err)
	}

	// Verify it exists
	loaded, loadErr := s.loadSessionState("test-session")
	if loadErr != nil {
		t.Fatalf("loadSessionState() error = %v", loadErr)
	}
	if loaded == nil {
		t.Fatal("session state not created")
	}

	// Clear state
	if err := s.clearSessionState("test-session"); err != nil {
		t.Fatalf("clearSessionState() error = %v", err)
	}

	// Verify it's gone
	loaded, loadErr = s.loadSessionState("test-session")
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
	points, err := s.GetRewindPoints(10)
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

	sessions, err := ListSessions()
	if err != nil {
		t.Errorf("ListSessions() error = %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("ListSessions() returned %d sessions, want 0", len(sessions))
	}
}

func TestShadowStrategy_GetSession_NotFound(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	_, err = GetSession("nonexistent")
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
	_, err = s.GetSessionInfo()
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
	can, reason, err := s.CanRewind()
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
	can, reason, err := s.CanRewind()
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
	can, reason, err := s.CanRewind()
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

	_, err = s.GetTaskCheckpoint(point)
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

	_, err = s.GetTaskCheckpointTranscript(point)
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
	// NewManualCommitStrategy returns ManualCommitStrategy
	sv2, ok := s.(*ManualCommitStrategy)
	if !ok {
		t.Fatal("failed to cast to ManualCommitStrategy")
	}
	prepErr := sv2.PrepareCommitMsg(commitMsgFile, "")
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
	sv2, ok := s.(*ManualCommitStrategy)
	if !ok {
		t.Fatal("failed to cast to ManualCommitStrategy")
	}

	skipSources := []string{"merge", "squash", "commit"}
	for _, source := range skipSources {
		t.Run(source, func(t *testing.T) {
			if err := os.WriteFile(commitMsgFile, []byte(originalMsg), 0o644); err != nil {
				t.Fatalf("failed to write commit message file: %v", err)
			}

			prepErr := sv2.PrepareCommitMsg(commitMsgFile, source)
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

func TestCheckpointInfo_JSONRoundTrip(t *testing.T) {
	original := CheckpointInfo{
		CheckpointID:     "a1b2c3d4e5f6",
		SessionID:        "session-123",
		CreatedAt:        time.Date(2025, 12, 2, 10, 0, 0, 0, time.UTC),
		CheckpointsCount: 5,
		FilesTouched:     []string{"file1.go", "file2.go"},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	var loaded CheckpointInfo
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if loaded.CheckpointID != original.CheckpointID {
		t.Errorf("CheckpointID = %q, want %q", loaded.CheckpointID, original.CheckpointID)
	}
	if loaded.SessionID != original.SessionID {
		t.Errorf("SessionID = %q, want %q", loaded.SessionID, original.SessionID)
	}
}

func TestSessionState_JSONRoundTrip(t *testing.T) {
	original := SessionState{
		SessionID:  "session-123",
		BaseCommit: "abc123def456",
		StartedAt:  time.Date(2025, 12, 2, 10, 0, 0, 0, time.UTC),
		StepCount:  10,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	var loaded SessionState
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if loaded.SessionID != original.SessionID {
		t.Errorf("SessionID = %q, want %q", loaded.SessionID, original.SessionID)
	}
	if loaded.BaseCommit != original.BaseCommit {
		t.Errorf("BaseCommit = %q, want %q", loaded.BaseCommit, original.BaseCommit)
	}
	if loaded.StepCount != original.StepCount {
		t.Errorf("StepCount = %d, want %d", loaded.StepCount, original.StepCount)
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
	_, err = s.GetCheckpointLog(checkpoint)
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
	_, err = s.GetCheckpointLog(checkpoint)
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

	// First checkpoint using SaveChanges - captures ALL working directory files
	// (for rewind purposes), but tracks only modified files in FilesTouched
	err = s.SaveChanges(SaveContext{
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
		t.Fatalf("SaveChanges() error = %v", err)
	}

	// Now simulate a second checkpoint where ONLY existing1.txt is modified
	// (but NOT existing2.txt or existing3.txt)
	modifiedContent := []byte("MODIFIED content of existing1.txt")
	if err := os.WriteFile(filepath.Join(dir, "existing1.txt"), modifiedContent, 0o644); err != nil {
		t.Fatalf("failed to modify existing1.txt: %v", err)
	}

	// Second checkpoint using SaveChanges - only modified file should be tracked
	err = s.SaveChanges(SaveContext{
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
		t.Fatalf("SaveChanges() error = %v", err)
	}

	// Load session state to verify FilesTouched
	state, err := s.loadSessionState(sessionID)
	if err != nil {
		t.Fatalf("loadSessionState() error = %v", err)
	}

	// Now condense the session
	checkpointID := id.MustCheckpointID("a1b2c3d4e5f6")
	result, err := s.CondenseSession(repo, checkpointID, state)
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
	dummyCommitHash, err := createCommit(repo, emptyTreeHash, plumbing.ZeroHash, "dummy commit", "test", "test@test.com")
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
	err = deleteShadowBranch(repo, shadowBranchName)
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
	err = deleteShadowBranch(repo, "entire/nonexistent")
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
	err = s.saveSessionState(state)
	if err != nil {
		t.Fatalf("saveSessionState() error = %v", err)
	}

	// Load state and verify LastCheckpointID
	loaded, err := s.loadSessionState("test-session-123")
	if err != nil {
		t.Fatalf("loadSessionState() error = %v", err)
	}
	if loaded == nil {
		t.Fatal("loadSessionState() returned nil")
	}

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
	err = s.saveSessionState(state)
	if err != nil {
		t.Fatalf("saveSessionState() error = %v", err)
	}

	// Load state and verify token usage fields are persisted
	loaded, err := s.loadSessionState("test-session-token-usage")
	if err != nil {
		t.Fatalf("loadSessionState() error = %v", err)
	}
	if loaded == nil {
		t.Fatal("loadSessionState() returned nil")
	}

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
		LastCheckpointID:          "abc123def456",
	}
	if err := s.saveSessionState(state); err != nil {
		t.Fatalf("saveSessionState() error = %v", err)
	}

	// Note: We can't fully test PrepareCommitMsg without setting up a shadow branch
	// with transcript, but we can verify the session state has LastCheckpointID set
	// The actual behavior is tested through integration tests

	// Verify the state was saved correctly
	loaded, err := s.loadSessionState("test-session")
	if err != nil {
		t.Fatalf("loadSessionState() error = %v", err)
	}
	if loaded.LastCheckpointID != "abc123def456" {
		t.Errorf("LastCheckpointID = %q, want %q", loaded.LastCheckpointID, "abc123def456")
	}
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

	transcript := `{"type":"human","message":{"content":"test prompt"}}
{"type":"assistant","message":{"content":"test response"}}
`
	if err := os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(transcript), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Use SaveChanges to create a checkpoint (this creates the shadow branch)
	err = s.SaveChanges(SaveContext{
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
		t.Fatalf("SaveChanges() error = %v", err)
	}

	// Load session state
	state, err := s.loadSessionState(sessionID)
	if err != nil {
		t.Fatalf("loadSessionState() error = %v", err)
	}

	// Condense the session
	checkpointID := id.MustCheckpointID("a1b2c3d4e5f6")
	_, err = s.CondenseSession(repo, checkpointID, state)
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

// TestSaveChanges_EmptyBaseCommit_Recovery verifies that SaveChanges recovers gracefully
// when a session state exists with empty BaseCommit (can happen from concurrent warning state).
func TestSaveChanges_EmptyBaseCommit_Recovery(t *testing.T) {
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
	if err := s.saveSessionState(partialState); err != nil {
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

	// SaveChanges should recover by re-initializing the session state
	err = s.SaveChanges(SaveContext{
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
		t.Fatalf("SaveChanges() should recover from empty BaseCommit, got error: %v", err)
	}

	// Verify session state now has a valid BaseCommit
	loaded, err := s.loadSessionState(sessionID)
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

// TestSaveChanges_UsesCtxAgentType_WhenNoSessionState tests that SaveChanges uses
// ctx.AgentType instead of DefaultAgentType ("Agent") when no session state exists.
// This is the primary bug scenario for ENT-207.
func TestSaveChanges_UsesCtxAgentType_WhenNoSessionState(t *testing.T) {
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
	// SaveChanges should use ctx.AgentType, not DefaultAgentType

	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	if err := os.MkdirAll(metadataDirAbs, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}
	transcript := `{"type":"human","message":{"content":"test"}}` + "\n"
	if err := os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(transcript), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	err = s.SaveChanges(SaveContext{
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
		t.Fatalf("SaveChanges() error = %v", err)
	}

	loaded, err := s.loadSessionState(sessionID)
	if err != nil {
		t.Fatalf("failed to load session state: %v", err)
	}
	if loaded.AgentType != agent.AgentTypeClaudeCode {
		t.Errorf("AgentType = %q, want %q", loaded.AgentType, agent.AgentTypeClaudeCode)
	}
}

// TestSaveChanges_UsesCtxAgentType_WhenPartialState tests that SaveChanges uses
// ctx.AgentType when a partial session state exists (empty BaseCommit and AgentType).
func TestSaveChanges_UsesCtxAgentType_WhenPartialState(t *testing.T) {
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
	if err := s.saveSessionState(partialState); err != nil {
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

	err = s.SaveChanges(SaveContext{
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
		t.Fatalf("SaveChanges() error = %v", err)
	}

	loaded, err := s.loadSessionState(sessionID)
	if err != nil {
		t.Fatalf("failed to load session state: %v", err)
	}
	if loaded.AgentType != agent.AgentTypeClaudeCode {
		t.Errorf("AgentType = %q, want %q", loaded.AgentType, agent.AgentTypeClaudeCode)
	}
}

// TestInitializeSession_BackfillsUnknownAgentType tests that InitializeSession
// replaces the default "Agent" value with the correct agent type on subsequent calls.
func TestInitializeSession_BackfillsUnknownAgentType(t *testing.T) {
	dir := t.TempDir()
	initTestRepo(t, dir)

	t.Chdir(dir)

	s := &ManualCommitStrategy{}
	sessionID := "2026-02-06-backfill-agent-type"

	// First call: initialize with correct type
	if err := s.InitializeSession(sessionID, agent.AgentTypeClaudeCode, "", ""); err != nil {
		t.Fatalf("InitializeSession() error = %v", err)
	}

	// Simulate the bug: manually set AgentType to "Agent" (as if session was created with default)
	state, err := s.loadSessionState(sessionID)
	if err != nil {
		t.Fatalf("failed to load session state: %v", err)
	}
	state.AgentType = agent.AgentTypeUnknown
	if err := s.saveSessionState(state); err != nil {
		t.Fatalf("failed to save state: %v", err)
	}

	// Second call with correct agent type should fix the "Agent" value
	if err := s.InitializeSession(sessionID, agent.AgentTypeClaudeCode, "", ""); err != nil {
		t.Fatalf("InitializeSession() second call error = %v", err)
	}

	loaded, err := s.loadSessionState(sessionID)
	if err != nil {
		t.Fatalf("failed to load session state: %v", err)
	}
	if loaded.AgentType != agent.AgentTypeClaudeCode {
		t.Errorf("AgentType = %q, want %q (should have been backfilled from %q)",
			loaded.AgentType, agent.AgentTypeClaudeCode, agent.AgentTypeUnknown)
	}
}

// TestCountTranscriptItems tests counting lines/messages in different transcript formats.
func TestCountTranscriptItems(t *testing.T) {
	tests := []struct {
		name      string
		agentType agent.AgentType
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
		agentType agent.AgentType
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
	err = s.SaveChanges(SaveContext{
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
		t.Fatalf("SaveChanges() error = %v", err)
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
	state, err := s.loadSessionState(sessionID)
	if err != nil {
		t.Fatalf("loadSessionState() error = %v", err)
	}

	// Condense the session - this should calculate InitialAttribution
	checkpointID := id.MustCheckpointID("a1b2c3d4e5f6")
	result, err := s.CondenseSession(repo, checkpointID, state)
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
	if err := s.InitializeSession(sessionID, "Claude Code", "", ""); err != nil {
		t.Fatalf("InitializeSession() prompt 1 error = %v", err)
	}

	// === CHECKPOINT 1: Agent modifies agent.go (adds 4 lines) ===
	checkpoint1Content := "package main\n\nfunc agentFunc1() {\n\tprintln(\"agent1\")\n}\n"
	if err := os.WriteFile(agentFile, []byte(checkpoint1Content), 0o644); err != nil {
		t.Fatalf("failed to write agent changes 1: %v", err)
	}

	err = s.SaveChanges(SaveContext{
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
		t.Fatalf("SaveChanges() checkpoint 1 error = %v", err)
	}

	// Verify PromptAttribution was recorded for checkpoint 1
	state1, err := s.loadSessionState(sessionID)
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
	if err := s.InitializeSession(sessionID, "Claude Code", "", ""); err != nil {
		t.Fatalf("InitializeSession() prompt 2 error = %v", err)
	}

	// === CHECKPOINT 2: Agent modifies agent.go again (adds 4 more lines) ===
	checkpoint2Content := "package main\n\nfunc agentFunc1() {\n\tprintln(\"agent1\")\n}\n\nfunc agentFunc2() {\n\tprintln(\"agent2\")\n}\n"
	if err := os.WriteFile(agentFile, []byte(checkpoint2Content), 0o644); err != nil {
		t.Fatalf("failed to write agent changes 2: %v", err)
	}

	err = s.SaveChanges(SaveContext{
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
		t.Fatalf("SaveChanges() checkpoint 2 error = %v", err)
	}

	// Verify PromptAttribution was recorded for checkpoint 2
	state2, err := s.loadSessionState(sessionID)
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
	result, err := s.CondenseSession(repo, checkpointID, state2)
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
// This reproduces the bug where SaveChanges was skipped (no code changes) but the
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

	// SaveChanges to create shadow branch with the stale transcript
	err = s.SaveChanges(SaveContext{
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
		t.Fatalf("SaveChanges() error = %v", err)
	}

	// Now simulate the conversation continuing: write a LONGER live transcript file.
	// In the real bug, SaveChanges would be skipped because totalChanges == 0,
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
	state, err := s.loadSessionState(sessionID)
	if err != nil {
		t.Fatalf("loadSessionState() error = %v", err)
	}
	state.TranscriptPath = liveTranscriptFile
	if err := s.saveSessionState(state); err != nil {
		t.Fatalf("saveSessionState() error = %v", err)
	}

	// Condense — this should read the live transcript, not the shadow branch copy
	checkpointID := id.MustCheckpointID("b2c3d4e5f6a1")
	result, err := s.CondenseSession(repo, checkpointID, state)
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

	// Create modified file
	if err := os.WriteFile(testFile, []byte("modified by gemini"), 0o644); err != nil {
		t.Fatalf("failed to modify file: %v", err)
	}

	// Save checkpoint (creates shadow branch)
	err = s.SaveChanges(SaveContext{
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
		t.Fatalf("SaveChanges() error = %v", err)
	}

	// Load session state
	state, err := s.loadSessionState(sessionID)
	if err != nil {
		t.Fatalf("loadSessionState() error = %v", err)
	}
	if state.AgentType != agent.AgentTypeGemini {
		t.Errorf("AgentType = %q, want %q", state.AgentType, agent.AgentTypeGemini)
	}

	// Condense the session
	checkpointID := id.MustCheckpointID("aabbcc112233")
	result, err := s.CondenseSession(repo, checkpointID, state)
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

	// Modify file for checkpoint 1
	if err := os.WriteFile(testFile, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatalf("failed to modify file: %v", err)
	}

	// Save checkpoint 1
	err = s.SaveChanges(SaveContext{
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
		t.Fatalf("SaveChanges() checkpoint 1 error = %v", err)
	}

	// Load and verify state after checkpoint 1
	state, err := s.loadSessionState(sessionID)
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

	// Modify file for checkpoint 2
	if err := os.WriteFile(testFile, []byte("package main\n\nfunc main() {\n\tif err := run(); err != nil {\n\t\tpanic(err)\n\t}\n}\n"), 0o644); err != nil {
		t.Fatalf("failed to modify file: %v", err)
	}

	// Before checkpoint 2, manually update CheckpointTranscriptStart to simulate
	// what would happen after condensing checkpoint 1
	state.CheckpointTranscriptStart = 2 // Start from message index 2 (the second user prompt)
	state.StepCount = 1                 // Set to 1 (will be incremented to 2 by SaveChanges)
	if err := s.saveSessionState(state); err != nil {
		t.Fatalf("failed to update session state: %v", err)
	}

	// Save checkpoint 2
	err = s.SaveChanges(SaveContext{
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
		t.Fatalf("SaveChanges() checkpoint 2 error = %v", err)
	}

	// Reload state to get updated values
	state, err = s.loadSessionState(sessionID)
	if err != nil {
		t.Fatalf("loadSessionState() error = %v", err)
	}

	// Condense the session - this should calculate token usage ONLY from message index 2 onwards
	checkpointID := id.MustCheckpointID("ddeeff998877")
	result, err := s.CondenseSession(repo, checkpointID, state)
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

	// Verify both prompts are present (even though tokens only count from second prompt)
	if !strings.Contains(content.Prompts, "Add a main function") {
		t.Error("Prompts should contain first prompt")
	}
	if !strings.Contains(content.Prompts, "Now add error handling") {
		t.Error("Prompts should contain second prompt")
	}
}
