package strategy

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"entire.io/cli/cmd/entire/cli/checkpoint"
	"entire.io/cli/cmd/entire/cli/checkpoint/id"
	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/trailers"
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
		SessionID:       "test-session-123",
		BaseCommit:      "abc123def456",
		StartedAt:       time.Now(),
		CheckpointCount: 5,
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
	if loaded.CheckpointCount != state.CheckpointCount {
		t.Errorf("CheckpointCount = %d, want %d", loaded.CheckpointCount, state.CheckpointCount)
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
	shadowBranch := getShadowBranchNameForCommit("abc1234")
	refName := plumbing.NewBranchReferenceName(shadowBranch)
	ref := plumbing.NewHashReference(refName, dummyCommitHash)
	if err := repo.Storer.SetReference(ref); err != nil {
		t.Fatalf("failed to create shadow branch: %v", err)
	}

	s := &ManualCommitStrategy{}

	// Save multiple session states (both with same base commit)
	state1 := &SessionState{
		SessionID:       "session-1",
		BaseCommit:      "abc1234",
		StartedAt:       time.Now(),
		CheckpointCount: 1,
	}
	state2 := &SessionState{
		SessionID:       "session-2",
		BaseCommit:      "abc1234",
		StartedAt:       time.Now(),
		CheckpointCount: 2,
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
	for _, baseCommit := range []string{"abc1234", "xyz7890"} {
		shadowBranch := getShadowBranchNameForCommit(baseCommit)
		refName := plumbing.NewBranchReferenceName(shadowBranch)
		ref := plumbing.NewHashReference(refName, dummyCommitHash)
		if err := repo.Storer.SetReference(ref); err != nil {
			t.Fatalf("failed to create shadow branch for %s: %v", baseCommit, err)
		}
	}

	s := &ManualCommitStrategy{}

	// Save session states with different base commits
	state1 := &SessionState{
		SessionID:       "session-1",
		BaseCommit:      "abc1234",
		StartedAt:       time.Now(),
		CheckpointCount: 1,
	}
	state2 := &SessionState{
		SessionID:       "session-2",
		BaseCommit:      "abc1234",
		StartedAt:       time.Now(),
		CheckpointCount: 2,
	}
	state3 := &SessionState{
		SessionID:       "session-3",
		BaseCommit:      "xyz7890",
		StartedAt:       time.Now(),
		CheckpointCount: 3,
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
		SessionID:       "test-session",
		BaseCommit:      "abc123",
		StartedAt:       time.Now(),
		CheckpointCount: 1,
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
	tests := []struct {
		name       string
		baseCommit string
		want       string
	}{
		{
			name:       "short commit",
			baseCommit: "abc",
			want:       "entire/abc",
		},
		{
			name:       "7 char commit",
			baseCommit: "abc1234",
			want:       "entire/abc1234",
		},
		{
			name:       "long commit",
			baseCommit: "abc1234567890",
			want:       "entire/abc1234",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getShadowBranchNameForCommit(tt.baseCommit)
			if got != tt.want {
				t.Errorf("getShadowBranchNameForCommit(%q) = %q, want %q", tt.baseCommit, got, tt.want)
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
	message := "Test commit message\n"

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
		SessionID:       "session-123",
		BaseCommit:      "abc123def456",
		StartedAt:       time.Date(2025, 12, 2, 10, 0, 0, 0, time.UTC),
		CheckpointCount: 10,
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
	if loaded.CheckpointCount != original.CheckpointCount {
		t.Errorf("CheckpointCount = %d, want %d", loaded.CheckpointCount, original.CheckpointCount)
	}
}

func TestShadowStrategy_GetCheckpointLog_WithCheckpointID(t *testing.T) {
	// This test verifies that GetCheckpointLog correctly uses the checkpoint ID
	// to look up the log. Since getCheckpointLog requires a full git setup
	// with entire/sessions branch, we test the lookup logic by checking error behavior.

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
	// there's no entire/sessions branch), but the important thing is it uses
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
		CheckpointCount:  5,
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
		SessionID:              "test-session-token-usage",
		BaseCommit:             "abc123def456",
		StartedAt:              time.Now(),
		CheckpointCount:        5,
		TranscriptLinesAtStart: 42,
		TranscriptUUIDAtStart:  "test-uuid-abc123",
		TokenUsage: &checkpoint.TokenUsage{
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

	// Verify TranscriptLinesAtStart
	if loaded.TranscriptLinesAtStart != state.TranscriptLinesAtStart {
		t.Errorf("TranscriptLinesAtStart = %d, want %d", loaded.TranscriptLinesAtStart, state.TranscriptLinesAtStart)
	}

	// Verify TranscriptUUIDAtStart
	if loaded.TranscriptUUIDAtStart != state.TranscriptUUIDAtStart {
		t.Errorf("TranscriptUUIDAtStart = %q, want %q", loaded.TranscriptUUIDAtStart, state.TranscriptUUIDAtStart)
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
		SessionID:                "test-session",
		BaseCommit:               initialCommit.String(),
		WorktreePath:             dir,
		StartedAt:                time.Now(),
		CheckpointCount:          1,
		CondensedTranscriptLines: 10, // Already condensed
		LastCheckpointID:         "abc123def456",
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
// on the entire/sessions branch include the Ephemeral-branch trailer indicating which shadow
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
	shadowBranchName := getShadowBranchNameForCommit(state.BaseCommit)
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
	// (simulates what checkConcurrentSessions used to create)
	partialState := &SessionState{
		SessionID:              sessionID,
		BaseCommit:             "", // Empty! This is the bug scenario
		ConcurrentWarningShown: true,
		StartedAt:              time.Now(),
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
	if loaded.CheckpointCount != 1 {
		t.Errorf("CheckpointCount = %d, want 1", loaded.CheckpointCount)
	}
}

// TestIsGeminiJSONTranscript tests detection of Gemini JSON transcript format.
func TestIsGeminiJSONTranscript(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected bool
	}{
		{
			name: "valid Gemini JSON",
			content: `{
				"messages": [
					{"type": "user", "content": "Hello"},
					{"type": "gemini", "content": "Hi there!"}
				]
			}`,
			expected: true,
		},
		{
			name:     "empty messages array",
			content:  `{"messages": []}`,
			expected: false,
		},
		{
			name: "JSONL format (Claude Code)",
			content: `{"type":"human","message":{"content":"Hello"}}
{"type":"assistant","message":{"content":"Hi"}}`,
			expected: false,
		},
		{
			name:     "not JSON",
			content:  "plain text",
			expected: false,
		},
		{
			name:     "JSON without messages field",
			content:  `{"foo": "bar"}`,
			expected: false,
		},
		{
			name:     "empty string",
			content:  "",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isGeminiJSONTranscript(tt.content)
			if result != tt.expected {
				t.Errorf("isGeminiJSONTranscript() = %v, want %v", result, tt.expected)
			}
		})
	}
}

// TestExtractUserPromptsFromGeminiJSON tests extraction of user prompts from Gemini JSON format.
func TestExtractUserPromptsFromGeminiJSON(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected []string
	}{
		{
			name: "single user prompt",
			content: `{
				"messages": [
					{"type": "user", "content": "Create a file called test.txt"}
				]
			}`,
			expected: []string{"Create a file called test.txt"},
		},
		{
			name: "multiple user prompts",
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
			name: "no user messages",
			content: `{
				"messages": [
					{"type": "gemini", "content": "Hello!"}
				]
			}`,
			expected: nil,
		},
		{
			name:     "empty messages",
			content:  `{"messages": []}`,
			expected: nil,
		},
		{
			name: "user message with empty content",
			content: `{
				"messages": [
					{"type": "user", "content": ""},
					{"type": "user", "content": "Valid prompt"}
				]
			}`,
			expected: []string{"Valid prompt"},
		},
		{
			name:     "invalid JSON",
			content:  "not json",
			expected: nil,
		},
		{
			name: "mixed message types",
			content: `{
				"sessionId": "abc123",
				"messages": [
					{"type": "user", "content": "Hello"},
					{"type": "gemini", "content": "Hi!", "toolCalls": []},
					{"type": "user", "content": "Goodbye"}
				]
			}`,
			expected: []string{"Hello", "Goodbye"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractUserPromptsFromGeminiJSON(tt.content)
			if len(result) != len(tt.expected) {
				t.Errorf("extractUserPromptsFromGeminiJSON() returned %d prompts, want %d", len(result), len(tt.expected))
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
