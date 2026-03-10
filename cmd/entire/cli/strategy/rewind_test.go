package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/claudecode" // Register agent for ResolveAgentForRewind tests
	_ "github.com/entireio/cli/cmd/entire/cli/agent/geminicli"  // Register agent for ResolveAgentForRewind tests
	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func TestShadowStrategy_PreviewRewind(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	// Create initial commit
	readmeFile := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readmeFile, []byte("# Test\n"), 0o644); err != nil {
		t.Fatalf("failed to write README: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	if _, err := worktree.Add("README.md"); err != nil {
		t.Fatalf("failed to add README: %v", err)
	}

	initialCommit, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create checkpoint with app.js file
	appFile := filepath.Join(dir, "app.js")
	if err := os.WriteFile(appFile, []byte("console.log('hello');\n"), 0o644); err != nil {
		t.Fatalf("failed to write app.js: %v", err)
	}

	if _, err := worktree.Add("app.js"); err != nil {
		t.Fatalf("failed to add app.js: %v", err)
	}

	// Create metadata directory structure first
	sessionID := "test-session-123"
	metadataDir := filepath.Join(dir, entireDir, "metadata", sessionID)
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}

	// Create session state to track untracked files at start
	s := &ManualCommitStrategy{}
	state := &SessionState{
		SessionID:             sessionID,
		BaseCommit:            initialCommit.String(),
		StartedAt:             time.Now(),
		UntrackedFilesAtStart: []string{"existing-untracked.txt"},
		StepCount:             1,
		WorktreePath:          dir,
	}
	if err := s.saveSessionState(context.Background(), state); err != nil {
		t.Fatalf("failed to save session state: %v", err)
	}

	// Create checkpoint commit with session trailer
	checkpointMsg := "Checkpoint\n\nEntire-Session: " + sessionID
	checkpointHash, err := worktree.Commit(checkpointMsg, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("failed to create checkpoint: %v", err)
	}

	// Reset to initial commit to simulate moving forward in time
	if err := worktree.Reset(&git.ResetOptions{
		Commit: initialCommit,
		Mode:   git.HardReset,
	}); err != nil {
		t.Fatalf("failed to reset to initial: %v", err)
	}

	// Create files that would be deleted on rewind:
	// 1. A new untracked file (created after checkpoint)
	extraFile := filepath.Join(dir, "extra.js")
	if err := os.WriteFile(extraFile, []byte("console.log('extra');\n"), 0o644); err != nil {
		t.Fatalf("failed to write extra.js: %v", err)
	}

	// 2. An untracked file that existed at session start (should NOT be deleted)
	existingFile := filepath.Join(dir, "existing-untracked.txt")
	if err := os.WriteFile(existingFile, []byte("I existed before session\n"), 0o644); err != nil {
		t.Fatalf("failed to write existing-untracked.txt: %v", err)
	}

	// Create rewind point
	point := RewindPoint{
		ID:          checkpointHash.String(),
		Message:     "Checkpoint",
		MetadataDir: metadataDir,
		Date:        time.Now(),
	}

	// Test PreviewRewind
	preview, err := s.PreviewRewind(context.Background(), point)
	if err != nil {
		t.Fatalf("PreviewRewind() error = %v", err)
	}

	if preview == nil {
		t.Fatal("PreviewRewind() returned nil preview")
	}

	// Should restore app.js
	foundApp := false
	for _, f := range preview.FilesToRestore {
		if f == "app.js" {
			foundApp = true
			break
		}
	}
	if !foundApp {
		t.Errorf("FilesToRestore missing app.js, got: %v", preview.FilesToRestore)
	}

	// Should delete extra.js
	foundExtra := false
	for _, f := range preview.FilesToDelete {
		if f == "extra.js" {
			foundExtra = true
			break
		}
	}
	if !foundExtra {
		t.Errorf("FilesToDelete missing extra.js, got: %v", preview.FilesToDelete)
	}

	// Should NOT delete existing-untracked.txt (existed at session start)
	for _, f := range preview.FilesToDelete {
		if f == "existing-untracked.txt" {
			t.Errorf("FilesToDelete incorrectly includes existing-untracked.txt, got: %v", preview.FilesToDelete)
		}
	}
}

func TestShadowStrategy_PreviewRewind_LogsOnly(t *testing.T) {
	dir := t.TempDir()
	_, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)

	s := &ManualCommitStrategy{}

	// Logs-only point should return empty preview
	point := RewindPoint{
		ID:           "abc123",
		Message:      "Committed",
		IsLogsOnly:   true,
		CheckpointID: "a1b2c3d4e5f6",
		Date:         time.Now(),
	}

	preview, err := s.PreviewRewind(context.Background(), point)
	if err != nil {
		t.Fatalf("PreviewRewind() error = %v", err)
	}

	if preview == nil {
		t.Fatal("PreviewRewind() returned nil preview")
	}

	if len(preview.FilesToDelete) > 0 {
		t.Errorf("Logs-only preview should have no files to delete, got: %v", preview.FilesToDelete)
	}

	if len(preview.FilesToRestore) > 0 {
		t.Errorf("Logs-only preview should have no files to restore, got: %v", preview.FilesToRestore)
	}
}

func TestResolveAgentForRewind(t *testing.T) {
	t.Parallel()

	t.Run("empty type returns error", func(t *testing.T) {
		t.Parallel()
		_, err := ResolveAgentForRewind("")
		if err == nil {
			t.Error("expected error for empty agent type")
		}
	})

	t.Run("Claude Code type resolves correctly", func(t *testing.T) {
		t.Parallel()
		ag, err := ResolveAgentForRewind("Claude Code")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ag.Name() != agent.AgentNameClaudeCode {
			t.Errorf("Name() = %q, want %q", ag.Name(), agent.AgentNameClaudeCode)
		}
	})

	t.Run("Gemini CLI type resolves correctly", func(t *testing.T) {
		t.Parallel()
		ag, err := ResolveAgentForRewind("Gemini CLI")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ag.Name() != agent.AgentNameGemini {
			t.Errorf("Name() = %q, want %q", ag.Name(), agent.AgentNameGemini)
		}
	})

	t.Run("unknown type returns error", func(t *testing.T) {
		t.Parallel()
		_, err := ResolveAgentForRewind("Nonexistent Agent")
		if err == nil {
			t.Error("expected error for unknown agent type")
		}
	})
}

// TestShadowStrategy_Rewind_FromSubdirectory verifies that Rewind() writes files
// to the correct repo-root-relative locations when CWD is a subdirectory.
// This is a regression test for the bug where f.Name (repo-relative) was used
// directly with os.WriteFile, causing files to be written relative to CWD instead
// of the repo root.
func TestShadowStrategy_Rewind_FromSubdirectory(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	author := &object.Signature{
		Name:  "Test",
		Email: "test@example.com",
		When:  time.Now(),
	}

	// Create initial commit with README.md
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0o644); err != nil {
		t.Fatalf("failed to write README: %v", err)
	}
	if _, err := worktree.Add("README.md"); err != nil {
		t.Fatalf("failed to add README: %v", err)
	}
	initialCommit, err := worktree.Commit("Initial commit", &git.CommitOptions{Author: author})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create files in nested directories for the checkpoint
	srcDir := filepath.Join(dir, "src")
	libDir := filepath.Join(dir, "src", "lib")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatalf("failed to create lib dir: %v", err)
	}

	appContent := "const app = 'hello';\n"
	utilsContent := "export function utils() {}\n"

	if err := os.WriteFile(filepath.Join(srcDir, "app.js"), []byte(appContent), 0o644); err != nil {
		t.Fatalf("failed to write app.js: %v", err)
	}
	if err := os.WriteFile(filepath.Join(libDir, "utils.js"), []byte(utilsContent), 0o644); err != nil {
		t.Fatalf("failed to write utils.js: %v", err)
	}

	if _, err := worktree.Add("src/app.js"); err != nil {
		t.Fatalf("failed to add src/app.js: %v", err)
	}
	if _, err := worktree.Add("src/lib/utils.js"); err != nil {
		t.Fatalf("failed to add src/lib/utils.js: %v", err)
	}

	// Create checkpoint commit
	checkpointHash, err := worktree.Commit("Checkpoint with nested files", &git.CommitOptions{Author: author})
	if err != nil {
		t.Fatalf("failed to create checkpoint: %v", err)
	}

	// Reset back to initial commit so the nested files are gone
	if err := worktree.Reset(&git.ResetOptions{
		Commit: initialCommit,
		Mode:   git.HardReset,
	}); err != nil {
		t.Fatalf("failed to reset to initial: %v", err)
	}

	// Verify src directory is gone after reset
	if _, err := os.Stat(filepath.Join(dir, "src")); !os.IsNotExist(err) {
		t.Fatalf("expected src/ to not exist after reset, but it does")
	}

	// Create a subdirectory and chdir into it (simulating agent running from subdirectory)
	frontendDir := filepath.Join(dir, "frontend")
	if err := os.MkdirAll(frontendDir, 0o755); err != nil {
		t.Fatalf("failed to create frontend dir: %v", err)
	}
	t.Chdir(frontendDir)
	paths.ClearWorktreeRootCache()

	// Call Rewind from the subdirectory
	s := NewManualCommitStrategy()
	point := RewindPoint{
		ID:      checkpointHash.String(),
		Message: "Checkpoint with nested files",
		Date:    time.Now(),
	}

	if err := s.Rewind(context.Background(), point); err != nil {
		t.Fatalf("Rewind() error = %v", err)
	}

	// Verify files are restored at the REPO ROOT (not relative to CWD)
	restoredApp := filepath.Join(dir, "src", "app.js")
	content, err := os.ReadFile(restoredApp)
	if err != nil {
		t.Fatalf("expected src/app.js to exist at repo root, but got error: %v", err)
	}
	if string(content) != appContent {
		t.Errorf("src/app.js content = %q, want %q", string(content), appContent)
	}

	restoredUtils := filepath.Join(dir, "src", "lib", "utils.js")
	content, err = os.ReadFile(restoredUtils)
	if err != nil {
		t.Fatalf("expected src/lib/utils.js to exist at repo root, but got error: %v", err)
	}
	if string(content) != utilsContent {
		t.Errorf("src/lib/utils.js content = %q, want %q", string(content), utilsContent)
	}

	// Verify README.md is also restored at repo root
	content, err = os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatalf("expected README.md to exist at repo root, but got error: %v", err)
	}
	if string(content) != "# Test\n" {
		t.Errorf("README.md content = %q, want %q", string(content), "# Test\n")
	}

	// Verify files are NOT written at CWD-relative paths (the bug behavior)
	wrongApp := filepath.Join(frontendDir, "src", "app.js")
	if _, err := os.Stat(wrongApp); !os.IsNotExist(err) {
		t.Errorf("src/app.js should NOT exist under frontend/ (CWD-relative), but it does at %s", wrongApp)
	}

	wrongUtils := filepath.Join(frontendDir, "src", "lib", "utils.js")
	if _, err := os.Stat(wrongUtils); !os.IsNotExist(err) {
		t.Errorf("src/lib/utils.js should NOT exist under frontend/ (CWD-relative), but it does at %s", wrongUtils)
	}
}

// TestShadowStrategy_Rewind_FromRepoRoot verifies the normal case where Rewind()
// restores files correctly when CWD is the repo root. This ensures the subdirectory
// fix did not break the happy path.
func TestShadowStrategy_Rewind_FromRepoRoot(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(dir)
	paths.ClearWorktreeRootCache()

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	author := &object.Signature{
		Name:  "Test",
		Email: "test@example.com",
		When:  time.Now(),
	}

	// Create initial commit with README.md
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0o644); err != nil {
		t.Fatalf("failed to write README: %v", err)
	}
	if _, err := worktree.Add("README.md"); err != nil {
		t.Fatalf("failed to add README: %v", err)
	}
	initialCommit, err := worktree.Commit("Initial commit", &git.CommitOptions{Author: author})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create files in nested directories for the checkpoint
	srcDir := filepath.Join(dir, "src")
	libDir := filepath.Join(dir, "src", "lib")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatalf("failed to create lib dir: %v", err)
	}

	appContent := "const app = 'hello';\n"
	utilsContent := "export function utils() {}\n"

	if err := os.WriteFile(filepath.Join(srcDir, "app.js"), []byte(appContent), 0o644); err != nil {
		t.Fatalf("failed to write app.js: %v", err)
	}
	if err := os.WriteFile(filepath.Join(libDir, "utils.js"), []byte(utilsContent), 0o644); err != nil {
		t.Fatalf("failed to write utils.js: %v", err)
	}

	if _, err := worktree.Add("src/app.js"); err != nil {
		t.Fatalf("failed to add src/app.js: %v", err)
	}
	if _, err := worktree.Add("src/lib/utils.js"); err != nil {
		t.Fatalf("failed to add src/lib/utils.js: %v", err)
	}

	// Create checkpoint commit
	checkpointHash, err := worktree.Commit("Checkpoint with nested files", &git.CommitOptions{Author: author})
	if err != nil {
		t.Fatalf("failed to create checkpoint: %v", err)
	}

	// Reset back to initial commit so the nested files are gone
	if err := worktree.Reset(&git.ResetOptions{
		Commit: initialCommit,
		Mode:   git.HardReset,
	}); err != nil {
		t.Fatalf("failed to reset to initial: %v", err)
	}

	// Verify src directory is gone after reset
	if _, err := os.Stat(filepath.Join(dir, "src")); !os.IsNotExist(err) {
		t.Fatalf("expected src/ to not exist after reset, but it does")
	}

	// Call Rewind from the repo root
	s := NewManualCommitStrategy()
	point := RewindPoint{
		ID:      checkpointHash.String(),
		Message: "Checkpoint with nested files",
		Date:    time.Now(),
	}

	if err := s.Rewind(context.Background(), point); err != nil {
		t.Fatalf("Rewind() error = %v", err)
	}

	// Verify files are restored correctly at repo root
	restoredApp := filepath.Join(dir, "src", "app.js")
	content, err := os.ReadFile(restoredApp)
	if err != nil {
		t.Fatalf("expected src/app.js to exist, but got error: %v", err)
	}
	if string(content) != appContent {
		t.Errorf("src/app.js content = %q, want %q", string(content), appContent)
	}

	restoredUtils := filepath.Join(dir, "src", "lib", "utils.js")
	content, err = os.ReadFile(restoredUtils)
	if err != nil {
		t.Fatalf("expected src/lib/utils.js to exist, but got error: %v", err)
	}
	if string(content) != utilsContent {
		t.Errorf("src/lib/utils.js content = %q, want %q", string(content), utilsContent)
	}

	// Verify README.md is also restored
	content, err = os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatalf("expected README.md to exist, but got error: %v", err)
	}
	if string(content) != "# Test\n" {
		t.Errorf("README.md content = %q, want %q", string(content), "# Test\n")
	}
}
