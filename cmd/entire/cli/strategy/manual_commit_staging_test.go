package strategy

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"entire.io/cli/cmd/entire/cli/paths"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const (
	testTranscript = `{"type":"human","message":{"content":"add function"}}
{"type":"assistant","message":{"content":"adding function"}}
`
	testCheckpoint1Content = "package main\n\nfunc agentFunc() {\n\tprintln(\"agent\")\n}\n"
)

// TestPromptAttribution_UsesWorktreeNotStagingArea tests that attribution calculation
// reads from the worktree (not staging area) to match what WriteTemporary captures.
// This ensures that if a user has both staged and unstaged changes, ALL changes are
// counted in PromptAttribution to match what's in the checkpoint tree.
//
// IMPORTANT: If we read from staging area but checkpoints capture worktree, unstaged
// changes would appear in the checkpoint but not in PromptAttribution, causing them
// to be incorrectly attributed to the agent later.
func TestPromptAttribution_UsesWorktreeNotStagingArea(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create initial commit with a file
	testFile := filepath.Join(dir, "test.go")
	if err := os.WriteFile(testFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("failed to write initial file: %v", err)
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
	sessionID := "2026-01-23-staging-test"

	// Create metadata directory
	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	if err := os.MkdirAll(metadataDirAbs, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(testTranscript), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// === PROMPT 1 START: Initialize session ===
	if err := s.InitializeSession(sessionID, "Claude Code", ""); err != nil {
		t.Fatalf("InitializeSession() prompt 1 error = %v", err)
	}

	// === CHECKPOINT 1: Agent adds 4 lines ===
	if err := os.WriteFile(testFile, []byte(testCheckpoint1Content), 0o644); err != nil {
		t.Fatalf("failed to write agent changes: %v", err)
	}

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
		t.Fatalf("SaveChanges() checkpoint 1 error = %v", err)
	}

	// === USER PARTIAL STAGING SCENARIO ===
	// Step 1: User adds 5 lines to the file
	partialContent := testCheckpoint1Content + "// User added line 1\n// User added line 2\n// User added line 3\n// User added line 4\n// User added line 5\n"
	if err := os.WriteFile(testFile, []byte(partialContent), 0o644); err != nil {
		t.Fatalf("failed to write partial user content: %v", err)
	}

	// Step 2: User stages these 5 lines
	if _, err := worktree.Add("test.go"); err != nil {
		t.Fatalf("failed to stage partial changes: %v", err)
	}

	// Step 3: User adds 5 MORE lines to worktree (unstaged)
	// Now: staging area has 5 user lines, worktree has 10 user lines
	fullContent := partialContent + "// User added line 6\n// User added line 7\n// User added line 8\n// User added line 9\n// User added line 10\n"
	if err := os.WriteFile(testFile, []byte(fullContent), 0o644); err != nil {
		t.Fatalf("failed to write full user content: %v", err)
	}

	// === PROMPT 2 START: Initialize session again ===
	// This should capture ALL 10 worktree lines to match what WriteTemporary will capture
	if err := s.InitializeSession(sessionID, "Claude Code", ""); err != nil {
		t.Fatalf("InitializeSession() prompt 2 error = %v", err)
	}

	// Verify PendingPromptAttribution shows worktree changes (10 lines), not just staged (5 lines)
	state, err := s.loadSessionState(sessionID)
	if err != nil {
		t.Fatalf("loadSessionState() after prompt 2 error = %v", err)
	}

	if state.PendingPromptAttribution == nil {
		t.Fatal("PendingPromptAttribution is nil after prompt 2")
	}

	// Should count ALL 10 worktree lines, not just the 5 staged lines
	// This matches what WriteTemporary will capture in the checkpoint
	if state.PendingPromptAttribution.UserLinesAdded != 10 {
		t.Errorf("PendingPromptAttribution.UserLinesAdded = %d, want 10 (worktree lines, not just staged)",
			state.PendingPromptAttribution.UserLinesAdded)
	}

	if state.PendingPromptAttribution.CheckpointNumber != 2 {
		t.Errorf("PendingPromptAttribution.CheckpointNumber = %d, want 2",
			state.PendingPromptAttribution.CheckpointNumber)
	}

	// === Verify checkpoint captures the same content ===
	// This demonstrates why we need to read from worktree: the checkpoint will capture
	// the full worktree content (10 lines), so PromptAttribution must also count 10 lines
	err = s.SaveChanges(SaveContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{"test.go"},
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

	// Reload state to see final PromptAttributions
	state, err = s.loadSessionState(sessionID)
	if err != nil {
		t.Fatalf("loadSessionState() after checkpoint 2 error = %v", err)
	}

	// PromptAttributions should now contain the entry with 10 user lines
	if len(state.PromptAttributions) != 2 {
		t.Fatalf("expected 2 PromptAttributions, got %d", len(state.PromptAttributions))
	}

	// Second attribution (from prompt 2) should show 10 user lines
	attr2 := state.PromptAttributions[1]
	if attr2.UserLinesAdded != 10 {
		t.Errorf("PromptAttributions[1].UserLinesAdded = %d, want 10 (worktree content)",
			attr2.UserLinesAdded)
	}
}

// TestPromptAttribution_UnstagedChanges tests that unstaged changes are
// still read from the worktree (not the staging area).
func TestPromptAttribution_UnstagedChanges(t *testing.T) {
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
	testFile := filepath.Join(dir, "test.go")
	if err := os.WriteFile(testFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("failed to write initial file: %v", err)
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
	sessionID := "2026-01-23-unstaged-test"

	// Create metadata directory
	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	if err := os.MkdirAll(metadataDirAbs, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(testTranscript), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Initialize and create checkpoint 1
	if err := s.InitializeSession(sessionID, "Claude Code", ""); err != nil {
		t.Fatalf("InitializeSession() prompt 1 error = %v", err)
	}

	if err := os.WriteFile(testFile, []byte(testCheckpoint1Content), 0o644); err != nil {
		t.Fatalf("failed to write agent changes: %v", err)
	}

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
		t.Fatalf("SaveChanges() checkpoint 1 error = %v", err)
	}

	// === USER UNSTAGED CHANGES ===
	// User adds 3 lines to worktree but does NOT stage them
	userContent := testCheckpoint1Content + "// User added line 1\n// User added line 2\n// User added line 3\n"
	if err := os.WriteFile(testFile, []byte(userContent), 0o644); err != nil {
		t.Fatalf("failed to write user content: %v", err)
	}

	// No staging - changes remain in worktree only

	// === PROMPT 2 START ===
	// Should read from worktree (since nothing is staged)
	if err := s.InitializeSession(sessionID, "Claude Code", ""); err != nil {
		t.Fatalf("InitializeSession() prompt 2 error = %v", err)
	}

	state, err := s.loadSessionState(sessionID)
	if err != nil {
		t.Fatalf("loadSessionState() error = %v", err)
	}

	if state.PendingPromptAttribution == nil {
		t.Fatal("PendingPromptAttribution is nil")
	}

	// Should read worktree changes (3 lines)
	if state.PendingPromptAttribution.UserLinesAdded != 3 {
		t.Errorf("PendingPromptAttribution.UserLinesAdded = %d, want 3 (worktree changes)",
			state.PendingPromptAttribution.UserLinesAdded)
	}
}

// TestPromptAttribution_AlwaysStored tests that PromptAttribution is always
// stored (even when zero) to maintain a complete history.
func TestPromptAttribution_AlwaysStored(t *testing.T) {
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
	testFile := filepath.Join(dir, "test.go")
	if err := os.WriteFile(testFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("failed to write initial file: %v", err)
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
	sessionID := "2026-01-23-always-stored-test"

	// Create metadata directory
	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	if err := os.MkdirAll(metadataDirAbs, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(testTranscript), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Initialize and create checkpoint 1
	if err := s.InitializeSession(sessionID, "Claude Code", ""); err != nil {
		t.Fatalf("InitializeSession() prompt 1 error = %v", err)
	}

	if err := os.WriteFile(testFile, []byte(testCheckpoint1Content), 0o644); err != nil {
		t.Fatalf("failed to write agent changes: %v", err)
	}

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
		t.Fatalf("SaveChanges() checkpoint 1 error = %v", err)
	}

	// === USER MAKES NO CHANGES ===
	// Don't modify the file at all

	// === PROMPT 2 START ===
	// Even though user made no changes, PendingPromptAttribution should be stored
	if err := s.InitializeSession(sessionID, "Claude Code", ""); err != nil {
		t.Fatalf("InitializeSession() prompt 2 error = %v", err)
	}

	state, err := s.loadSessionState(sessionID)
	if err != nil {
		t.Fatalf("loadSessionState() error = %v", err)
	}

	// Should have PendingPromptAttribution even though user made no changes
	if state.PendingPromptAttribution == nil {
		t.Fatal("PendingPromptAttribution should be stored even when user made no changes (for complete history)")
	}

	// All values should be zero
	if state.PendingPromptAttribution.UserLinesAdded != 0 {
		t.Errorf("PendingPromptAttribution.UserLinesAdded = %d, want 0",
			state.PendingPromptAttribution.UserLinesAdded)
	}
	if state.PendingPromptAttribution.UserLinesRemoved != 0 {
		t.Errorf("PendingPromptAttribution.UserLinesRemoved = %d, want 0",
			state.PendingPromptAttribution.UserLinesRemoved)
	}
	if state.PendingPromptAttribution.CheckpointNumber != 2 {
		t.Errorf("PendingPromptAttribution.CheckpointNumber = %d, want 2",
			state.PendingPromptAttribution.CheckpointNumber)
	}

	// === CHECKPOINT 2: Agent makes more changes ===
	checkpoint2Content := testCheckpoint1Content + "\nfunc agentFunc2() {\n\tprintln(\"agent2\")\n}\n"
	if err := os.WriteFile(testFile, []byte(checkpoint2Content), 0o644); err != nil {
		t.Fatalf("failed to write agent changes 2: %v", err)
	}

	err = s.SaveChanges(SaveContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{"test.go"},
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

	// Verify PromptAttributions array now contains the zero-value entry
	state, err = s.loadSessionState(sessionID)
	if err != nil {
		t.Fatalf("loadSessionState() after checkpoint 2 error = %v", err)
	}

	if len(state.PromptAttributions) != 2 {
		t.Fatalf("expected 2 PromptAttributions, got %d", len(state.PromptAttributions))
	}

	// Second attribution should have zero user changes (from prompt 2)
	attr2 := state.PromptAttributions[1]
	if attr2.UserLinesAdded != 0 || attr2.UserLinesRemoved != 0 {
		t.Errorf("PromptAttributions[1]: UserLinesAdded=%d, UserLinesRemoved=%d, want both 0",
			attr2.UserLinesAdded, attr2.UserLinesRemoved)
	}
	if attr2.CheckpointNumber != 2 {
		t.Errorf("PromptAttributions[1].CheckpointNumber = %d, want 2", attr2.CheckpointNumber)
	}
}

// TestPromptAttribution_CapturesPrePromptEdits tests that user edits made BEFORE
// the first prompt are correctly attributed to the user, not the agent.
// This verifies the fix for the bug where CheckpointCount > 0 guard and early
// return on missing shadow branch prevented attribution of pre-prompt edits.
func TestPromptAttribution_CapturesPrePromptEdits(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create initial commit with a file (base state: "A")
	testFile := filepath.Join(dir, "test.go")
	baseContent := "package main\n"
	if err := os.WriteFile(testFile, []byte(baseContent), 0o644); err != nil {
		t.Fatalf("failed to write initial file: %v", err)
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

	// === USER EDITS BEFORE FIRST PROMPT ===
	// User manually edits the file BEFORE starting the session
	prePromptContent := baseContent + "// User added line 1\n// User added line 2\n"
	if err := os.WriteFile(testFile, []byte(prePromptContent), 0o644); err != nil {
		t.Fatalf("failed to write pre-prompt user edits: %v", err)
	}

	s := &ManualCommitStrategy{}
	sessionID := "2026-01-24-preprompt-test"

	// Create metadata directory
	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	if err := os.MkdirAll(metadataDirAbs, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(testTranscript), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// === PROMPT 1 START ===
	// This should capture the user's pre-prompt edits (2 lines)
	if err := s.InitializeSession(sessionID, "Claude Code", ""); err != nil {
		t.Fatalf("InitializeSession() error = %v", err)
	}

	// Verify PendingPromptAttribution captured pre-prompt edits
	state, err := s.loadSessionState(sessionID)
	if err != nil {
		t.Fatalf("loadSessionState() error = %v", err)
	}

	if state.PendingPromptAttribution == nil {
		t.Fatal("PendingPromptAttribution is nil - pre-prompt edits not captured!")
	}

	// Should capture the 2 lines the user added before the first prompt
	if state.PendingPromptAttribution.UserLinesAdded != 2 {
		t.Errorf("PendingPromptAttribution.UserLinesAdded = %d, want 2 (pre-prompt user edits)",
			state.PendingPromptAttribution.UserLinesAdded)
	}

	if state.PendingPromptAttribution.CheckpointNumber != 1 {
		t.Errorf("PendingPromptAttribution.CheckpointNumber = %d, want 1",
			state.PendingPromptAttribution.CheckpointNumber)
	}

	// === CHECKPOINT 1: Agent adds 1 line ===
	agentContent := prePromptContent + "func agentFunc() {}\n"
	if err := os.WriteFile(testFile, []byte(agentContent), 0o644); err != nil {
		t.Fatalf("failed to write agent changes: %v", err)
	}

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

	// Reload state to verify PromptAttributions
	state, err = s.loadSessionState(sessionID)
	if err != nil {
		t.Fatalf("loadSessionState() after checkpoint error = %v", err)
	}

	// PromptAttributions should now contain the pre-prompt edits
	if len(state.PromptAttributions) != 1 {
		t.Fatalf("expected 1 PromptAttribution, got %d", len(state.PromptAttributions))
	}

	attr1 := state.PromptAttributions[0]
	if attr1.UserLinesAdded != 2 {
		t.Errorf("PromptAttributions[0].UserLinesAdded = %d, want 2 (pre-prompt user edits)",
			attr1.UserLinesAdded)
	}

	// Agent work should be 0 for the first attribution (only user edits captured)
	if attr1.AgentLinesAdded != 0 {
		t.Errorf("PromptAttributions[0].AgentLinesAdded = %d, want 0 (no prior checkpoints)",
			attr1.AgentLinesAdded)
	}
}
