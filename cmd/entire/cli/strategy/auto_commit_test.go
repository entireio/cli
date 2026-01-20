package strategy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"entire.io/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestAutoCommitStrategy_Registration(t *testing.T) {
	s, err := Get(StrategyNameAutoCommit)
	if err != nil {
		t.Fatalf("Get(%q) error = %v", StrategyNameAutoCommit, err)
	}
	if s == nil {
		t.Fatal("Get() returned nil strategy")
	}
	if s.Name() != StrategyNameAutoCommit {
		t.Errorf("Name() = %q, want %q", s.Name(), StrategyNameAutoCommit)
	}
}

func TestAutoCommitStrategy_SaveChanges_CommitHasMetadataRef(t *testing.T) {
	// Setup temp git repo
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
	readmeFile := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readmeFile, []byte("# Test"), 0o644); err != nil {
		t.Fatalf("failed to write README: %v", err)
	}
	if _, err := worktree.Add("README.md"); err != nil {
		t.Fatalf("failed to add README: %v", err)
	}
	_, err = worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	// Setup strategy and ensure entire/sessions branch exists
	s := NewAutoCommitStrategy()
	if err := s.EnsureSetup(); err != nil {
		t.Fatalf("EnsureSetup() error = %v", err)
	}

	// Create a modified file
	testFile := filepath.Join(dir, "test.go")
	if err := os.WriteFile(testFile, []byte("package main"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	// Create metadata directory with session log
	sessionID := "2025-12-04-test-session-123"
	metadataDir := filepath.Join(dir, paths.EntireMetadataDir, sessionID)
	if err := os.MkdirAll(metadataDir, 0o750); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}
	logFile := filepath.Join(metadataDir, paths.TranscriptFileName)
	if err := os.WriteFile(logFile, []byte("test session log"), 0o644); err != nil {
		t.Fatalf("failed to write log file: %v", err)
	}

	metadataDirAbs, err := paths.AbsPath(metadataDir)
	if err != nil {
		metadataDirAbs = metadataDir
	}

	// Call SaveChanges
	ctx := SaveContext{
		CommitMessage:  "Test session commit",
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		NewFiles:       []string{"test.go"},
		ModifiedFiles:  []string{},
		DeletedFiles:   []string{},
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	}

	if err := s.SaveChanges(ctx); err != nil {
		t.Fatalf("SaveChanges() error = %v", err)
	}

	// Verify the code commit on active branch has NO trailers (clean history)
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("failed to get HEAD: %v", err)
	}

	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("failed to get HEAD commit: %v", err)
	}

	// Active branch commits should be clean - no Entire-* trailers
	if strings.Contains(commit.Message, paths.StrategyTrailerKey) {
		t.Errorf("code commit should NOT have strategy trailer, got message:\n%s", commit.Message)
	}
	if strings.Contains(commit.Message, paths.SourceRefTrailerKey) {
		t.Errorf("code commit should NOT have source-ref trailer, got message:\n%s", commit.Message)
	}
	if strings.Contains(commit.Message, paths.SessionTrailerKey) {
		t.Errorf("code commit should NOT have session trailer, got message:\n%s", commit.Message)
	}

	// Verify metadata was stored on entire/sessions branch
	sessionsRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("entire/sessions branch not found: %v", err)
	}
	sessionsCommit, err := repo.CommitObject(sessionsRef.Hash())
	if err != nil {
		t.Fatalf("failed to get sessions branch commit: %v", err)
	}

	// Metadata commit should have the checkpoint format with session ID and strategy
	if !strings.Contains(sessionsCommit.Message, paths.SessionTrailerKey) {
		t.Errorf("sessions branch commit should have session trailer, got message:\n%s", sessionsCommit.Message)
	}
	if !strings.Contains(sessionsCommit.Message, paths.StrategyTrailerKey) {
		t.Errorf("sessions branch commit should have strategy trailer, got message:\n%s", sessionsCommit.Message)
	}
}

func TestAutoCommitStrategy_SaveChanges_MetadataRefPointsToValidCommit(t *testing.T) {
	// Setup temp git repo
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
	readmeFile := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readmeFile, []byte("# Test"), 0o644); err != nil {
		t.Fatalf("failed to write README: %v", err)
	}
	if _, err := worktree.Add("README.md"); err != nil {
		t.Fatalf("failed to add README: %v", err)
	}
	_, err = worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	// Setup strategy
	s := NewAutoCommitStrategy()
	if err := s.EnsureSetup(); err != nil {
		t.Fatalf("EnsureSetup() error = %v", err)
	}

	// Create a modified file
	testFile := filepath.Join(dir, "test.go")
	if err := os.WriteFile(testFile, []byte("package main"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	// Create metadata directory
	sessionID := "2025-12-04-test-session-456"
	metadataDir := filepath.Join(dir, paths.EntireMetadataDir, sessionID)
	if err := os.MkdirAll(metadataDir, 0o750); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}
	logFile := filepath.Join(metadataDir, paths.TranscriptFileName)
	if err := os.WriteFile(logFile, []byte("test session log"), 0o644); err != nil {
		t.Fatalf("failed to write log file: %v", err)
	}

	metadataDirAbs, err := paths.AbsPath(metadataDir)
	if err != nil {
		metadataDirAbs = metadataDir
	}

	// Call SaveChanges
	ctx := SaveContext{
		CommitMessage:  "Test session commit",
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		NewFiles:       []string{"test.go"},
		ModifiedFiles:  []string{},
		DeletedFiles:   []string{},
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	}

	if err := s.SaveChanges(ctx); err != nil {
		t.Fatalf("SaveChanges() error = %v", err)
	}

	// Get the code commit
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("failed to get HEAD: %v", err)
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("failed to get HEAD commit: %v", err)
	}

	// Code commit should be clean - no Entire-* trailers
	if strings.Contains(commit.Message, paths.SourceRefTrailerKey) {
		t.Errorf("code commit should NOT have source-ref trailer, got:\n%s", commit.Message)
	}

	// Get the entire/sessions branch
	metadataBranchRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("failed to get entire/sessions branch: %v", err)
	}

	metadataCommit, err := repo.CommitObject(metadataBranchRef.Hash())
	if err != nil {
		t.Fatalf("failed to get metadata branch commit: %v", err)
	}

	// Verify the metadata commit has the checkpoint format
	if !strings.HasPrefix(metadataCommit.Message, "Checkpoint: ") {
		t.Errorf("metadata commit missing checkpoint format, got:\n%s", metadataCommit.Message)
	}

	// Verify it contains the session ID
	if !strings.Contains(metadataCommit.Message, paths.SessionTrailerKey+": "+sessionID) {
		t.Errorf("metadata commit missing %s trailer for %s", paths.SessionTrailerKey, sessionID)
	}

	// Verify it contains the strategy (auto-commit)
	if !strings.Contains(metadataCommit.Message, paths.StrategyTrailerKey+": "+StrategyNameAutoCommit) {
		t.Errorf("metadata commit missing %s trailer for %s", paths.StrategyTrailerKey, StrategyNameAutoCommit)
	}
}

func TestAutoCommitStrategy_SaveTaskCheckpoint_CommitHasMetadataRef(t *testing.T) {
	// Setup temp git repo
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
	readmeFile := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readmeFile, []byte("# Test"), 0o644); err != nil {
		t.Fatalf("failed to write README: %v", err)
	}
	if _, err := worktree.Add("README.md"); err != nil {
		t.Fatalf("failed to add README: %v", err)
	}
	_, err = worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	// Setup strategy
	s := NewAutoCommitStrategy()
	if err := s.EnsureSetup(); err != nil {
		t.Fatalf("EnsureSetup() error = %v", err)
	}

	// Create a file (simulating task output)
	testFile := filepath.Join(dir, "task_output.txt")
	if err := os.WriteFile(testFile, []byte("task result"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	// Create transcript file
	transcriptDir := t.TempDir()
	transcriptPath := filepath.Join(transcriptDir, "session.jsonl")
	if err := os.WriteFile(transcriptPath, []byte(`{"type":"test"}`), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Call SaveTaskCheckpoint
	ctx := TaskCheckpointContext{
		SessionID:      "test-session-789",
		ToolUseID:      "toolu_abc123",
		CheckpointUUID: "checkpoint-uuid-456",
		AgentID:        "agent-xyz",
		TranscriptPath: transcriptPath,
		NewFiles:       []string{"task_output.txt"},
		ModifiedFiles:  []string{},
		DeletedFiles:   []string{},
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	}

	if err := s.SaveTaskCheckpoint(ctx); err != nil {
		t.Fatalf("SaveTaskCheckpoint() error = %v", err)
	}

	// Verify the code commit is clean (no trailers)
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("failed to get HEAD: %v", err)
	}

	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("failed to get HEAD commit: %v", err)
	}

	// Task checkpoint commit should be clean - no Entire-* trailers
	if strings.Contains(commit.Message, paths.SourceRefTrailerKey) {
		t.Errorf("task checkpoint commit should NOT have source-ref trailer, got message:\n%s", commit.Message)
	}
	if strings.Contains(commit.Message, paths.StrategyTrailerKey) {
		t.Errorf("task checkpoint commit should NOT have strategy trailer, got message:\n%s", commit.Message)
	}

	// Verify metadata was stored on entire/sessions branch
	sessionsRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("entire/sessions branch not found: %v", err)
	}
	sessionsCommit, err := repo.CommitObject(sessionsRef.Hash())
	if err != nil {
		t.Fatalf("failed to get sessions branch commit: %v", err)
	}

	// Metadata commit should reference the checkpoint
	if !strings.Contains(sessionsCommit.Message, "Checkpoint: ") {
		t.Errorf("sessions branch commit missing checkpoint format, got:\n%s", sessionsCommit.Message)
	}
}

func TestAutoCommitStrategy_SaveTaskCheckpoint_TaskStartCreatesEmptyCommit(t *testing.T) {
	// Setup temp git repo
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Create initial commit (needed for a valid repo state)
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	readmeFile := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readmeFile, []byte("# Test"), 0o644); err != nil {
		t.Fatalf("failed to write README: %v", err)
	}
	if _, err := worktree.Add("README.md"); err != nil {
		t.Fatalf("failed to add README: %v", err)
	}
	initialCommit, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	// Setup strategy
	s := NewAutoCommitStrategy()
	if err := s.EnsureSetup(); err != nil {
		t.Fatalf("EnsureSetup() error = %v", err)
	}

	// Create a TaskStart checkpoint with NO file changes
	ctx := TaskCheckpointContext{
		SessionID:           "test-session-taskstart",
		ToolUseID:           "toolu_taskstart123",
		IsIncremental:       true,
		IncrementalType:     IncrementalTypeTaskStart,
		IncrementalSequence: 1,
		SubagentType:        "dev",
		TaskDescription:     "Implement feature",
		// No file changes
		ModifiedFiles: []string{},
		NewFiles:      []string{},
		DeletedFiles:  []string{},
		AuthorName:    "Test",
		AuthorEmail:   "test@test.com",
	}

	if err := s.SaveTaskCheckpoint(ctx); err != nil {
		t.Fatalf("SaveTaskCheckpoint() error = %v", err)
	}

	// Verify a NEW commit was created (not just returning HEAD)
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("failed to get HEAD: %v", err)
	}

	if head.Hash() == initialCommit {
		t.Error("TaskStart should create a new commit even without file changes, but HEAD is still the initial commit")
	}

	// Verify the commit message contains the expected content
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("failed to get HEAD commit: %v", err)
	}

	// TaskStart commits should have "Starting" in the message
	expectedSubstring := "Starting 'dev' agent: Implement feature"
	if !strings.Contains(commit.Message, expectedSubstring) {
		t.Errorf("commit message should contain %q, got:\n%s", expectedSubstring, commit.Message)
	}
}

func TestAutoCommitStrategy_SaveTaskCheckpoint_NonTaskStartNoChangesAmendedForMetadata(t *testing.T) {
	// Setup temp git repo
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
	readmeFile := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readmeFile, []byte("# Test"), 0o644); err != nil {
		t.Fatalf("failed to write README: %v", err)
	}
	if _, err := worktree.Add("README.md"); err != nil {
		t.Fatalf("failed to add README: %v", err)
	}
	initialCommit, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	// Setup strategy
	s := NewAutoCommitStrategy()
	if err := s.EnsureSetup(); err != nil {
		t.Fatalf("EnsureSetup() error = %v", err)
	}

	// Create a regular incremental checkpoint (not TaskStart) with NO file changes
	ctx := TaskCheckpointContext{
		SessionID:           "test-session-nontaskstart",
		ToolUseID:           "toolu_nontaskstart456",
		IsIncremental:       true,
		IncrementalType:     "TodoWrite", // NOT TaskStart
		IncrementalSequence: 2,
		TodoContent:         "Write some code",
		// No file changes
		ModifiedFiles: []string{},
		NewFiles:      []string{},
		DeletedFiles:  []string{},
		AuthorName:    "Test",
		AuthorEmail:   "test@test.com",
	}

	if err := s.SaveTaskCheckpoint(ctx); err != nil {
		t.Fatalf("SaveTaskCheckpoint() error = %v", err)
	}

	// Get HEAD after the operation
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("failed to get HEAD: %v", err)
	}

	// The auto-commit strategy amends the HEAD commit to add source ref trailer,
	// so HEAD will be different from the initial commit even without file changes.
	// However, the commit tree should be the same as the initial commit.
	newCommit, err := repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("failed to get HEAD commit: %v", err)
	}

	oldCommit, err := repo.CommitObject(initialCommit)
	if err != nil {
		t.Fatalf("failed to get initial commit: %v", err)
	}

	// The tree hash should be the same (no file changes)
	if newCommit.TreeHash != oldCommit.TreeHash {
		t.Error("non-TaskStart checkpoint without file changes should have the same tree hash")
	}

	// Metadata should still be stored on entire/sessions branch
	metadataBranch, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("failed to get entire/sessions branch: %v", err)
	}

	metadataCommit, err := repo.CommitObject(metadataBranch.Hash())
	if err != nil {
		t.Fatalf("failed to get metadata commit: %v", err)
	}

	// Verify metadata was committed to the branch
	if !strings.Contains(metadataCommit.Message, paths.MetadataTaskTrailerKey) {
		t.Error("metadata should still be committed to entire/sessions branch")
	}
}

func TestAutoCommitStrategy_GetSessionContext(t *testing.T) {
	// Setup temp git repo
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
	readmeFile := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readmeFile, []byte("# Test"), 0o644); err != nil {
		t.Fatalf("failed to write README: %v", err)
	}
	if _, err := worktree.Add("README.md"); err != nil {
		t.Fatalf("failed to add README: %v", err)
	}
	_, err = worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	// Setup strategy
	s := NewAutoCommitStrategy()
	if err := s.EnsureSetup(); err != nil {
		t.Fatalf("EnsureSetup() error = %v", err)
	}

	// Create a modified file
	testFile := filepath.Join(dir, "test.go")
	if err := os.WriteFile(testFile, []byte("package main"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	// Create metadata directory with session log and context.md
	sessionID := "2025-12-10-test-session-context"
	metadataDir := filepath.Join(dir, paths.EntireMetadataDir, sessionID)
	if err := os.MkdirAll(metadataDir, 0o750); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}
	logFile := filepath.Join(metadataDir, paths.TranscriptFileName)
	if err := os.WriteFile(logFile, []byte("test session log"), 0o644); err != nil {
		t.Fatalf("failed to write log file: %v", err)
	}
	contextContent := "# Session Context\n\nThis is a test context.\n\n## Details\n\n- Item 1\n- Item 2"
	contextFile := filepath.Join(metadataDir, paths.ContextFileName)
	if err := os.WriteFile(contextFile, []byte(contextContent), 0o644); err != nil {
		t.Fatalf("failed to write context file: %v", err)
	}

	metadataDirAbs, err := paths.AbsPath(metadataDir)
	if err != nil {
		metadataDirAbs = metadataDir
	}

	// Save changes - this creates a checkpoint on entire/sessions
	ctx := SaveContext{
		CommitMessage:  "Test checkpoint",
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		NewFiles:       []string{"test.go"},
		ModifiedFiles:  []string{},
		DeletedFiles:   []string{},
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	}
	if err := s.SaveChanges(ctx); err != nil {
		t.Fatalf("SaveChanges() error = %v", err)
	}

	// Now retrieve the context using GetSessionContext
	result := s.GetSessionContext(sessionID)
	if result == "" {
		t.Error("GetSessionContext() returned empty string")
	}
	if result != contextContent {
		t.Errorf("GetSessionContext() = %q, want %q", result, contextContent)
	}
}

func TestAutoCommitStrategy_ListSessions_HasDescription(t *testing.T) {
	// Setup temp git repo
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
	readmeFile := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readmeFile, []byte("# Test"), 0o644); err != nil {
		t.Fatalf("failed to write README: %v", err)
	}
	if _, err := worktree.Add("README.md"); err != nil {
		t.Fatalf("failed to add README: %v", err)
	}
	_, err = worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	// Setup strategy
	s := NewAutoCommitStrategy()
	if err := s.EnsureSetup(); err != nil {
		t.Fatalf("EnsureSetup() error = %v", err)
	}

	// Create a modified file
	testFile := filepath.Join(dir, "test.go")
	if err := os.WriteFile(testFile, []byte("package main"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	// Create metadata directory with session log and prompt.txt
	sessionID := "2025-12-10-test-session-description"
	metadataDir := filepath.Join(dir, paths.EntireMetadataDir, sessionID)
	if err := os.MkdirAll(metadataDir, 0o750); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}
	logFile := filepath.Join(metadataDir, paths.TranscriptFileName)
	if err := os.WriteFile(logFile, []byte("test session log"), 0o644); err != nil {
		t.Fatalf("failed to write log file: %v", err)
	}

	// Write prompt.txt with description
	expectedDescription := "Fix the authentication bug in login.go"
	promptFile := filepath.Join(metadataDir, paths.PromptFileName)
	if err := os.WriteFile(promptFile, []byte(expectedDescription+"\n\nMore details here..."), 0o644); err != nil {
		t.Fatalf("failed to write prompt file: %v", err)
	}

	metadataDirAbs, err := paths.AbsPath(metadataDir)
	if err != nil {
		metadataDirAbs = metadataDir
	}

	// Save changes - this creates a checkpoint on entire/sessions
	ctx := SaveContext{
		CommitMessage:  "Test checkpoint",
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		NewFiles:       []string{"test.go"},
		ModifiedFiles:  []string{},
		DeletedFiles:   []string{},
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
		SessionID:      sessionID,
	}
	if err := s.SaveChanges(ctx); err != nil {
		t.Fatalf("SaveChanges() error = %v", err)
	}

	sessions, err := ListSessions()
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}

	if len(sessions) == 0 {
		t.Fatal("ListSessions() returned no sessions")
	}

	// Find our session
	var found *Session
	for i := range sessions {
		if sessions[i].ID == sessionID {
			found = &sessions[i]
			break
		}
	}

	if found == nil {
		t.Fatalf("Session %q not found in ListSessions() result", sessionID)
	}

	// Verify description is populated (not "No description")
	if found.Description == NoDescription {
		t.Errorf("ListSessions() returned session with Description = %q, want %q", found.Description, expectedDescription)
	}
	if found.Description != expectedDescription {
		t.Errorf("ListSessions() returned session with Description = %q, want %q", found.Description, expectedDescription)
	}
}

// TestAutoCommitStrategy_ImplementsSessionInitializer verifies that AutoCommitStrategy
// implements the SessionInitializer interface for session state management.
func TestAutoCommitStrategy_ImplementsSessionInitializer(t *testing.T) {
	s := NewAutoCommitStrategy()

	// Verify it implements SessionInitializer
	_, ok := s.(SessionInitializer)
	if !ok {
		t.Fatal("AutoCommitStrategy should implement SessionInitializer interface")
	}
}

// TestAutoCommitStrategy_InitializeSession_CreatesSessionState verifies that
// InitializeSession creates a SessionState file for auto-commit strategy.
func TestAutoCommitStrategy_InitializeSession_CreatesSessionState(t *testing.T) {
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
	readmeFile := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readmeFile, []byte("# Test"), 0o644); err != nil {
		t.Fatalf("failed to write README: %v", err)
	}
	if _, err := worktree.Add("README.md"); err != nil {
		t.Fatalf("failed to add README: %v", err)
	}
	_, err = worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	s := NewAutoCommitStrategy()
	initializer, ok := s.(SessionInitializer)
	if !ok {
		t.Fatal("AutoCommitStrategy should implement SessionInitializer")
	}

	sessionID := "2025-12-22-test-session-init"
	if err := initializer.InitializeSession(sessionID, "Claude Code"); err != nil {
		t.Fatalf("InitializeSession() error = %v", err)
	}

	// Verify session state was created
	state, err := LoadSessionState(sessionID)
	if err != nil {
		t.Fatalf("LoadSessionState() error = %v", err)
	}
	if state == nil {
		t.Fatal("SessionState not created")
	}

	if state.SessionID != sessionID {
		t.Errorf("SessionID = %q, want %q", state.SessionID, sessionID)
	}
	if state.CheckpointCount != 0 {
		t.Errorf("CheckpointCount = %d, want 0", state.CheckpointCount)
	}
	if state.CondensedTranscriptLines != 0 {
		t.Errorf("CondensedTranscriptLines = %d, want 0", state.CondensedTranscriptLines)
	}
}

func TestAutoCommitStrategy_GetCheckpointLog_ReadsFullJsonl(t *testing.T) {
	// Setup temp git repo
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
	readmeFile := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readmeFile, []byte("# Test"), 0o644); err != nil {
		t.Fatalf("failed to write README: %v", err)
	}
	if _, err := worktree.Add("README.md"); err != nil {
		t.Fatalf("failed to add README: %v", err)
	}
	_, err = worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	// Setup strategy
	s := NewAutoCommitStrategy()
	if err := s.EnsureSetup(); err != nil {
		t.Fatalf("EnsureSetup() error = %v", err)
	}

	// Create a file for the task checkpoint
	testFile := filepath.Join(dir, "task_output.txt")
	if err := os.WriteFile(testFile, []byte("task result"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	// Create transcript file with expected content
	transcriptDir := t.TempDir()
	transcriptPath := filepath.Join(transcriptDir, "session.jsonl")
	expectedContent := `{"type":"assistant","content":"test response"}`
	if err := os.WriteFile(transcriptPath, []byte(expectedContent), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	sessionID := "2025-12-12-test-checkpoint-jsonl"

	// Call SaveTaskCheckpoint (final, not incremental - this includes full.jsonl)
	ctx := TaskCheckpointContext{
		SessionID:      sessionID,
		ToolUseID:      "toolu_jsonl_test",
		CheckpointUUID: "checkpoint-uuid-jsonl",
		AgentID:        "agent-jsonl",
		TranscriptPath: transcriptPath,
		NewFiles:       []string{"task_output.txt"},
		ModifiedFiles:  []string{},
		DeletedFiles:   []string{},
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	}

	if err := s.SaveTaskCheckpoint(ctx); err != nil {
		t.Fatalf("SaveTaskCheckpoint() error = %v", err)
	}

	sessions, err := ListSessions()
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}

	var session *Session
	for i := range sessions {
		if sessions[i].ID == sessionID {
			session = &sessions[i]
			break
		}
	}
	if session == nil {
		t.Fatalf("Session %q not found", sessionID)
	}
	if len(session.Checkpoints) == 0 {
		t.Fatal("No checkpoints found for session")
	}

	// Get checkpoint log - should read full.jsonl
	checkpoint := session.Checkpoints[0]
	content, err := s.GetCheckpointLog(checkpoint)
	if err != nil {
		t.Fatalf("GetCheckpointLog() error = %v", err)
	}

	if string(content) != expectedContent {
		t.Errorf("GetCheckpointLog() content = %q, want %q", string(content), expectedContent)
	}
}

// TestAutoCommitStrategy_SaveChanges_FilesAlreadyCommitted verifies that SaveChanges
// skips creating metadata when files are listed but already committed by the user.
// This handles the case where git.ErrEmptyCommit occurs during commit.
func TestAutoCommitStrategy_SaveChanges_FilesAlreadyCommitted(t *testing.T) {
	// Setup temp git repo
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
	readmeFile := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readmeFile, []byte("# Test"), 0o644); err != nil {
		t.Fatalf("failed to write README: %v", err)
	}
	if _, err := worktree.Add("README.md"); err != nil {
		t.Fatalf("failed to add README: %v", err)
	}
	_, err = worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	// Setup strategy
	s := NewAutoCommitStrategy()
	if err := s.EnsureSetup(); err != nil {
		t.Fatalf("EnsureSetup() error = %v", err)
	}

	// Create a test file and commit it manually (simulating user committing before hook runs)
	testFile := filepath.Join(dir, "test.go")
	if err := os.WriteFile(testFile, []byte("package main"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := worktree.Add("test.go"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	userCommit, err := worktree.Commit("User committed the file first", &git.CommitOptions{
		Author: &object.Signature{Name: "User", Email: "user@test.com"},
	})
	if err != nil {
		t.Fatalf("failed to commit test file: %v", err)
	}

	// Get count of commits on entire/sessions before the call
	sessionsRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("entire/sessions branch not found: %v", err)
	}
	sessionsCommitBefore := sessionsRef.Hash()

	// Create metadata directory
	sessionID := "2025-12-22-already-committed-test"
	metadataDir := filepath.Join(dir, paths.EntireMetadataDir, sessionID)
	if err := os.MkdirAll(metadataDir, 0o750); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}
	logFile := filepath.Join(metadataDir, paths.TranscriptFileName)
	if err := os.WriteFile(logFile, []byte("test session log"), 0o644); err != nil {
		t.Fatalf("failed to write log file: %v", err)
	}

	metadataDirAbs, err := paths.AbsPath(metadataDir)
	if err != nil {
		metadataDirAbs = metadataDir
	}

	// Call SaveChanges with the file that was already committed
	// This simulates the hook running after the user already committed the changes
	ctx := SaveContext{
		CommitMessage:  "Should be skipped - file already committed",
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		NewFiles:       []string{"test.go"}, // File exists but already committed
		ModifiedFiles:  []string{},
		DeletedFiles:   []string{},
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	}

	// SaveChanges should succeed without error (skip is not an error)
	if err := s.SaveChanges(ctx); err != nil {
		t.Fatalf("SaveChanges() error = %v", err)
	}

	// Verify HEAD is still the user's commit (no new code commit created)
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("failed to get HEAD: %v", err)
	}
	if head.Hash() != userCommit {
		t.Errorf("HEAD should still be user's commit %s, got %s", userCommit, head.Hash())
	}

	// Verify entire/sessions branch has no new commits (metadata not created)
	sessionsRefAfter, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("entire/sessions branch not found after SaveChanges: %v", err)
	}
	if sessionsRefAfter.Hash() != sessionsCommitBefore {
		t.Errorf("entire/sessions should not have new commits when files already committed, before=%s after=%s",
			sessionsCommitBefore, sessionsRefAfter.Hash())
	}
}

// TestAutoCommitStrategy_SaveChanges_NoChangesSkipped verifies that SaveChanges
// skips creating metadata when there are no code changes to commit.
// This ensures 1:1 mapping between code commits and metadata commits.
func TestAutoCommitStrategy_SaveChanges_NoChangesSkipped(t *testing.T) {
	// Setup temp git repo
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
	readmeFile := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readmeFile, []byte("# Test"), 0o644); err != nil {
		t.Fatalf("failed to write README: %v", err)
	}
	if _, err := worktree.Add("README.md"); err != nil {
		t.Fatalf("failed to add README: %v", err)
	}
	initialCommit, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	t.Chdir(dir)

	// Setup strategy
	s := NewAutoCommitStrategy()
	if err := s.EnsureSetup(); err != nil {
		t.Fatalf("EnsureSetup() error = %v", err)
	}

	// Get count of commits on entire/sessions before the call
	sessionsRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("entire/sessions branch not found: %v", err)
	}
	sessionsCommitBefore := sessionsRef.Hash()

	// Create metadata directory (without any file changes to commit)
	sessionID := "2025-12-22-no-changes-test"
	metadataDir := filepath.Join(dir, paths.EntireMetadataDir, sessionID)
	if err := os.MkdirAll(metadataDir, 0o750); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}
	logFile := filepath.Join(metadataDir, paths.TranscriptFileName)
	if err := os.WriteFile(logFile, []byte("test session log"), 0o644); err != nil {
		t.Fatalf("failed to write log file: %v", err)
	}

	metadataDirAbs, err := paths.AbsPath(metadataDir)
	if err != nil {
		metadataDirAbs = metadataDir
	}

	// Call SaveChanges with NO file changes (empty lists)
	ctx := SaveContext{
		CommitMessage:  "Should be skipped",
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		NewFiles:       []string{}, // Empty - no changes
		ModifiedFiles:  []string{}, // Empty - no changes
		DeletedFiles:   []string{}, // Empty - no changes
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	}

	// SaveChanges should succeed without error (skip is not an error)
	if err := s.SaveChanges(ctx); err != nil {
		t.Fatalf("SaveChanges() error = %v", err)
	}

	// Verify HEAD is still the initial commit (no new code commit)
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("failed to get HEAD: %v", err)
	}
	if head.Hash() != initialCommit {
		t.Errorf("HEAD should still be initial commit %s, got %s", initialCommit, head.Hash())
	}

	// Verify entire/sessions branch has no new commits (metadata not created)
	sessionsRefAfter, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("entire/sessions branch not found after SaveChanges: %v", err)
	}
	if sessionsRefAfter.Hash() != sessionsCommitBefore {
		t.Errorf("entire/sessions should not have new commits when no code changes, before=%s after=%s",
			sessionsCommitBefore, sessionsRefAfter.Hash())
	}
}
