package strategy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// initHooksTestRepo creates a temporary git repository, changes to it, and clears
// the repo root cache. Returns the repo directory path and the hooks directory path.
func initHooksTestRepo(t *testing.T) (string, string) {
	t.Helper()
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "git", "init")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}
	paths.ClearRepoRootCache()

	return tmpDir, filepath.Join(tmpDir, ".git", "hooks")
}

func TestGetGitDirInPath_RegularRepo(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "git", "init")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	result, err := getGitDirInPath(tmpDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := filepath.Join(tmpDir, ".git")

	// Resolve symlinks for comparison (macOS /var -> /private/var)
	resultResolved, err := filepath.EvalSymlinks(result)
	if err != nil {
		t.Fatalf("failed to resolve symlinks for result: %v", err)
	}
	expectedResolved, err := filepath.EvalSymlinks(expected)
	if err != nil {
		t.Fatalf("failed to resolve symlinks for expected: %v", err)
	}

	if resultResolved != expectedResolved {
		t.Errorf("expected %s, got %s", expectedResolved, resultResolved)
	}
}

func TestGetGitDirInPath_Worktree(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	mainRepo := filepath.Join(tmpDir, "main")
	worktreeDir := filepath.Join(tmpDir, "worktree")

	// Initialize main repo
	if err := os.MkdirAll(mainRepo, 0o755); err != nil {
		t.Fatalf("failed to create main repo dir: %v", err)
	}

	ctx := context.Background()

	cmd := exec.CommandContext(ctx, "git", "init")
	cmd.Dir = mainRepo
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to init main repo: %v", err)
	}

	// Configure git user for the commit
	cmd = exec.CommandContext(ctx, "git", "config", "user.email", "test@test.com")
	cmd.Dir = mainRepo
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to configure git email: %v", err)
	}

	cmd = exec.CommandContext(ctx, "git", "config", "user.name", "Test User")
	cmd.Dir = mainRepo
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to configure git name: %v", err)
	}

	// Disable GPG signing for test commits
	cmd = exec.CommandContext(ctx, "git", "config", "commit.gpgsign", "false")
	cmd.Dir = mainRepo
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to configure commit.gpgsign: %v", err)
	}

	// Create an initial commit (required for worktree)
	testFile := filepath.Join(mainRepo, "test.txt")
	if err := os.WriteFile(testFile, []byte("test"), 0o644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	cmd = exec.CommandContext(ctx, "git", "add", ".")
	cmd.Dir = mainRepo
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to git add: %v", err)
	}

	cmd = exec.CommandContext(ctx, "git", "commit", "-m", "initial")
	cmd.Dir = mainRepo
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to git commit: %v", err)
	}

	// Create a worktree
	cmd = exec.CommandContext(ctx, "git", "worktree", "add", worktreeDir, "-b", "feature")
	cmd.Dir = mainRepo
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to create worktree: %v", err)
	}

	// Test that getGitDirInPath works in the worktree
	result, err := getGitDirInPath(worktreeDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Resolve symlinks for comparison (macOS /var -> /private/var)
	resultResolved, err := filepath.EvalSymlinks(result)
	if err != nil {
		t.Fatalf("failed to resolve symlinks for result: %v", err)
	}
	expectedPrefix, err := filepath.EvalSymlinks(filepath.Join(mainRepo, ".git", "worktrees"))
	if err != nil {
		t.Fatalf("failed to resolve symlinks for expected prefix: %v", err)
	}

	// The git dir for a worktree should be inside main repo's .git/worktrees/
	if !strings.HasPrefix(resultResolved, expectedPrefix) {
		t.Errorf("expected git dir to be under %s, got %s", expectedPrefix, resultResolved)
	}
}

func TestGetGitDirInPath_NotARepo(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	_, err := getGitDirInPath(tmpDir)
	if err == nil {
		t.Fatal("expected error for non-repo directory, got nil")
	}

	expectedMsg := "not a git repository"
	if err.Error() != expectedMsg {
		t.Errorf("expected error message %q, got %q", expectedMsg, err.Error())
	}
}

// isGitSequenceOperation tests use t.Chdir() so cannot call t.Parallel().

func TestIsGitSequenceOperation_NoOperation(t *testing.T) {
	initHooksTestRepo(t)

	if isGitSequenceOperation() {
		t.Error("isGitSequenceOperation() = true, want false for clean repo")
	}
}

func TestIsGitSequenceOperation_RebaseMerge(t *testing.T) {
	tmpDir, _ := initHooksTestRepo(t)

	if err := os.MkdirAll(filepath.Join(tmpDir, ".git", "rebase-merge"), 0o755); err != nil {
		t.Fatalf("failed to create rebase-merge dir: %v", err)
	}

	if !isGitSequenceOperation() {
		t.Error("isGitSequenceOperation() = false, want true during rebase-merge")
	}
}

func TestIsGitSequenceOperation_RebaseApply(t *testing.T) {
	tmpDir, _ := initHooksTestRepo(t)

	if err := os.MkdirAll(filepath.Join(tmpDir, ".git", "rebase-apply"), 0o755); err != nil {
		t.Fatalf("failed to create rebase-apply dir: %v", err)
	}

	if !isGitSequenceOperation() {
		t.Error("isGitSequenceOperation() = false, want true during rebase-apply")
	}
}

func TestIsGitSequenceOperation_CherryPick(t *testing.T) {
	tmpDir, _ := initHooksTestRepo(t)

	if err := os.WriteFile(filepath.Join(tmpDir, ".git", "CHERRY_PICK_HEAD"), []byte("abc123"), 0o644); err != nil {
		t.Fatalf("failed to create CHERRY_PICK_HEAD: %v", err)
	}

	if !isGitSequenceOperation() {
		t.Error("isGitSequenceOperation() = false, want true during cherry-pick")
	}
}

func TestIsGitSequenceOperation_Revert(t *testing.T) {
	tmpDir, _ := initHooksTestRepo(t)

	if err := os.WriteFile(filepath.Join(tmpDir, ".git", "REVERT_HEAD"), []byte("abc123"), 0o644); err != nil {
		t.Fatalf("failed to create REVERT_HEAD: %v", err)
	}

	if !isGitSequenceOperation() {
		t.Error("isGitSequenceOperation() = false, want true during revert")
	}
}

func TestIsGitSequenceOperation_Worktree(t *testing.T) {
	// Test that detection works in a worktree (git dir is different)
	tmpDir := t.TempDir()
	mainRepo := filepath.Join(tmpDir, "main")
	worktreeDir := filepath.Join(tmpDir, "worktree")

	if err := os.MkdirAll(mainRepo, 0o755); err != nil {
		t.Fatalf("failed to create main repo dir: %v", err)
	}

	ctx := context.Background()

	// Initialize main repo with a commit
	cmd := exec.CommandContext(ctx, "git", "init")
	cmd.Dir = mainRepo
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to init main repo: %v", err)
	}

	cmd = exec.CommandContext(ctx, "git", "config", "user.email", "test@test.com")
	cmd.Dir = mainRepo
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to configure git email: %v", err)
	}

	cmd = exec.CommandContext(ctx, "git", "config", "user.name", "Test User")
	cmd.Dir = mainRepo
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to configure git name: %v", err)
	}

	// Disable GPG signing for test commits
	cmd = exec.CommandContext(ctx, "git", "config", "commit.gpgsign", "false")
	cmd.Dir = mainRepo
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to configure commit.gpgsign: %v", err)
	}

	testFile := filepath.Join(mainRepo, "test.txt")
	if err := os.WriteFile(testFile, []byte("test"), 0o644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	cmd = exec.CommandContext(ctx, "git", "add", ".")
	cmd.Dir = mainRepo
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to git add: %v", err)
	}

	cmd = exec.CommandContext(ctx, "git", "commit", "-m", "initial")
	cmd.Dir = mainRepo
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to git commit: %v", err)
	}

	// Create a worktree
	cmd = exec.CommandContext(ctx, "git", "worktree", "add", worktreeDir, "-b", "feature")
	cmd.Dir = mainRepo
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to create worktree: %v", err)
	}

	// Change to worktree
	t.Chdir(worktreeDir)

	// Should not detect sequence operation in clean worktree
	if isGitSequenceOperation() {
		t.Error("isGitSequenceOperation() = true in clean worktree, want false")
	}

	// Get the worktree's git dir and simulate rebase state there
	cmd = exec.CommandContext(ctx, "git", "rev-parse", "--git-dir")
	cmd.Dir = worktreeDir
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("failed to get git dir: %v", err)
	}
	gitDir := strings.TrimSpace(string(output))

	rebaseMergeDir := filepath.Join(gitDir, "rebase-merge")
	if err := os.MkdirAll(rebaseMergeDir, 0o755); err != nil {
		t.Fatalf("failed to create rebase-merge dir in worktree: %v", err)
	}

	// Now should detect sequence operation
	if !isGitSequenceOperation() {
		t.Error("isGitSequenceOperation() = false in worktree during rebase, want true")
	}
}

func TestInstallGitHook_Idempotent(t *testing.T) {
	_, hooksDir := initHooksTestRepo(t)

	// First install should install hooks
	firstCount, err := InstallGitHook(true)
	if err != nil {
		t.Fatalf("First InstallGitHook() error = %v", err)
	}
	if firstCount == 0 {
		t.Error("First InstallGitHook() should install hooks (count > 0)")
	}

	// Capture hook contents after first install
	firstContents := make(map[string]string)
	for _, hook := range gitHookNames {
		data, err := os.ReadFile(filepath.Join(hooksDir, hook))
		if err != nil {
			t.Fatalf("hook %s should exist after install: %v", hook, err)
		}
		firstContents[hook] = string(data)
		if !strings.Contains(string(data), entireHookMarker) {
			t.Errorf("hook %s should contain Entire marker", hook)
		}
	}

	// Second install should return 0 (all hooks already up to date)
	secondCount, err := InstallGitHook(true)
	if err != nil {
		t.Fatalf("Second InstallGitHook() error = %v", err)
	}
	if secondCount != 0 {
		t.Errorf("Second InstallGitHook() returned %d, want 0 (hooks unchanged)", secondCount)
	}

	// Content should be identical after second install
	for _, hook := range gitHookNames {
		data, err := os.ReadFile(filepath.Join(hooksDir, hook))
		if err != nil {
			t.Fatalf("hook %s should exist: %v", hook, err)
		}
		if string(data) != firstContents[hook] {
			t.Errorf("hook %s content changed after idempotent reinstall", hook)
		}
	}
}

func TestRemoveGitHook_RemovesInstalledHooks(t *testing.T) {
	tmpDir, _ := initHooksTestRepo(t)

	// Install hooks first
	installCount, err := InstallGitHook(true)
	if err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	}
	if installCount == 0 {
		t.Fatal("InstallGitHook() should install hooks")
	}

	// Verify hooks are installed
	if !IsGitHookInstalled() {
		t.Fatal("hooks should be installed before removal test")
	}

	// Remove hooks
	removeCount, err := RemoveGitHook()
	if err != nil {
		t.Fatalf("RemoveGitHook() error = %v", err)
	}
	if removeCount != installCount {
		t.Errorf("RemoveGitHook() returned %d, want %d (same as installed)", removeCount, installCount)
	}

	// Verify hooks are removed
	if IsGitHookInstalled() {
		t.Error("hooks should not be installed after removal")
	}

	// Verify hook files no longer exist
	hooksDir := filepath.Join(tmpDir, ".git", "hooks")
	for _, hookName := range gitHookNames {
		hookPath := filepath.Join(hooksDir, hookName)
		if _, err := os.Stat(hookPath); !os.IsNotExist(err) {
			t.Errorf("hook file %s should not exist after removal", hookName)
		}
	}
}

func TestRemoveGitHook_NoHooksInstalled(t *testing.T) {
	initHooksTestRepo(t)

	// Remove hooks when none are installed - should handle gracefully
	removeCount, err := RemoveGitHook()
	if err != nil {
		t.Fatalf("RemoveGitHook() error = %v", err)
	}
	if removeCount != 0 {
		t.Errorf("RemoveGitHook() returned %d, want 0 (no hooks to remove)", removeCount)
	}
}

func TestRemoveGitHook_IgnoresNonEntireHooks(t *testing.T) {
	_, hooksDir := initHooksTestRepo(t)

	// Create a non-Entire hook manually
	customHookPath := filepath.Join(hooksDir, "pre-commit")
	customHookContent := "#!/bin/sh\necho 'custom hook'"
	if err := os.WriteFile(customHookPath, []byte(customHookContent), 0o755); err != nil {
		t.Fatalf("failed to create custom hook: %v", err)
	}

	// Remove hooks - should not remove the custom hook
	removeCount, err := RemoveGitHook()
	if err != nil {
		t.Fatalf("RemoveGitHook() error = %v", err)
	}
	if removeCount != 0 {
		t.Errorf("RemoveGitHook() returned %d, want 0 (custom hook should not be removed)", removeCount)
	}

	// Verify custom hook still exists
	if _, err := os.Stat(customHookPath); os.IsNotExist(err) {
		t.Error("custom hook should still exist after RemoveGitHook()")
	}
}

func TestRemoveGitHook_NotAGitRepo(t *testing.T) {
	// Create a temp directory without git init
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Clear cache so paths resolve correctly
	paths.ClearRepoRootCache()

	// Remove hooks in non-git directory - should return error
	_, err := RemoveGitHook()
	if err == nil {
		t.Fatal("RemoveGitHook() should return error for non-git directory")
	}
}

func TestInstallGitHook_BacksUpCustomHook(t *testing.T) {
	_, hooksDir := initHooksTestRepo(t)

	// Create a custom prepare-commit-msg hook
	customHookPath := filepath.Join(hooksDir, "prepare-commit-msg")
	customContent := "#!/bin/sh\necho 'my custom hook'\n"
	if err := os.WriteFile(customHookPath, []byte(customContent), 0o755); err != nil {
		t.Fatalf("failed to create custom hook: %v", err)
	}

	count, err := InstallGitHook(true)
	if err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	}
	if count == 0 {
		t.Error("InstallGitHook() should install hooks")
	}

	// Verify custom hook was backed up
	backupPath := customHookPath + backupSuffix
	backupData, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("backup file should exist at %s: %v", backupPath, err)
	}
	if string(backupData) != customContent {
		t.Errorf("backup content = %q, want %q", string(backupData), customContent)
	}

	// Verify installed hook has our marker and chain call
	hookData, err := os.ReadFile(customHookPath)
	if err != nil {
		t.Fatalf("hook file should exist: %v", err)
	}
	hookContent := string(hookData)
	if !strings.Contains(hookContent, entireHookMarker) {
		t.Error("installed hook should contain Entire marker")
	}
	if !strings.Contains(hookContent, chainComment) {
		t.Error("installed hook should contain chain call")
	}
	if !strings.Contains(hookContent, "prepare-commit-msg"+backupSuffix) {
		t.Error("chain call should reference the backup file")
	}
}

func TestInstallGitHook_DoesNotOverwriteExistingBackup(t *testing.T) {
	_, hooksDir := initHooksTestRepo(t)

	// Create a backup file manually (simulating a previous backup)
	firstBackupContent := "#!/bin/sh\necho 'first custom hook'\n"
	backupPath := filepath.Join(hooksDir, "prepare-commit-msg"+backupSuffix)
	if err := os.WriteFile(backupPath, []byte(firstBackupContent), 0o755); err != nil {
		t.Fatalf("failed to create backup: %v", err)
	}

	// Create a second custom hook at the standard path
	secondCustomContent := "#!/bin/sh\necho 'second custom hook'\n"
	hookPath := filepath.Join(hooksDir, "prepare-commit-msg")
	if err := os.WriteFile(hookPath, []byte(secondCustomContent), 0o755); err != nil {
		t.Fatalf("failed to create second custom hook: %v", err)
	}

	_, err := InstallGitHook(true)
	if err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	}

	// Verify the original backup was NOT overwritten
	backupData, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("backup should still exist: %v", err)
	}
	if string(backupData) != firstBackupContent {
		t.Errorf("backup content = %q, want original %q", string(backupData), firstBackupContent)
	}

	// Verify our hook was installed with chain call
	hookData, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("hook should exist: %v", err)
	}
	if !strings.Contains(string(hookData), entireHookMarker) {
		t.Error("hook should contain Entire marker")
	}
	if !strings.Contains(string(hookData), chainComment) {
		t.Error("hook should contain chain call since backup exists")
	}
}

func TestInstallGitHook_IdempotentWithChaining(t *testing.T) {
	_, hooksDir := initHooksTestRepo(t)

	// Create a custom hook, then install
	customHookPath := filepath.Join(hooksDir, "prepare-commit-msg")
	if err := os.WriteFile(customHookPath, []byte("#!/bin/sh\necho custom\n"), 0o755); err != nil {
		t.Fatalf("failed to create custom hook: %v", err)
	}

	firstCount, err := InstallGitHook(true)
	if err != nil {
		t.Fatalf("first InstallGitHook() error = %v", err)
	}
	if firstCount == 0 {
		t.Error("first install should install hooks")
	}

	// Re-install should return 0 (idempotent)
	secondCount, err := InstallGitHook(true)
	if err != nil {
		t.Fatalf("second InstallGitHook() error = %v", err)
	}
	if secondCount != 0 {
		t.Errorf("second InstallGitHook() = %d, want 0 (idempotent)", secondCount)
	}
}

func TestInstallGitHook_NoBackupWhenNoExistingHook(t *testing.T) {
	_, hooksDir := initHooksTestRepo(t)

	_, err := InstallGitHook(true)
	if err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	}

	// No .pre-entire files should exist
	for _, hook := range gitHookNames {
		backupPath := filepath.Join(hooksDir, hook+backupSuffix)
		if _, err := os.Stat(backupPath); !os.IsNotExist(err) {
			t.Errorf("backup %s should not exist for fresh install", hook+backupSuffix)
		}

		// Hook should not contain chain call
		data, err := os.ReadFile(filepath.Join(hooksDir, hook))
		if err != nil {
			t.Fatalf("hook %s should exist: %v", hook, err)
		}
		if strings.Contains(string(data), chainComment) {
			t.Errorf("hook %s should not contain chain call for fresh install", hook)
		}
	}
}

func TestInstallGitHook_MixedHooks(t *testing.T) {
	_, hooksDir := initHooksTestRepo(t)

	// Only create custom hooks for some hooks
	customHooks := map[string]string{
		"prepare-commit-msg": "#!/bin/sh\necho 'custom pcm'\n",
		"pre-push":           "#!/bin/sh\necho 'custom prepush'\n",
	}
	for name, content := range customHooks {
		hookPath := filepath.Join(hooksDir, name)
		if err := os.WriteFile(hookPath, []byte(content), 0o755); err != nil {
			t.Fatalf("failed to create %s: %v", name, err)
		}
	}

	_, err := InstallGitHook(true)
	if err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	}

	// Hooks with pre-existing content should have backups and chain calls
	for name := range customHooks {
		backupPath := filepath.Join(hooksDir, name+backupSuffix)
		if _, err := os.Stat(backupPath); os.IsNotExist(err) {
			t.Errorf("backup for %s should exist", name)
		}

		data, err := os.ReadFile(filepath.Join(hooksDir, name))
		if err != nil {
			t.Fatalf("hook %s should exist: %v", name, err)
		}
		if !strings.Contains(string(data), chainComment) {
			t.Errorf("hook %s should contain chain call", name)
		}
	}

	// Hooks without pre-existing content should NOT have backups or chain calls
	noCustom := []string{"commit-msg", "post-commit"}
	for _, name := range noCustom {
		backupPath := filepath.Join(hooksDir, name+backupSuffix)
		if _, err := os.Stat(backupPath); !os.IsNotExist(err) {
			t.Errorf("backup for %s should NOT exist", name)
		}

		data, err := os.ReadFile(filepath.Join(hooksDir, name))
		if err != nil {
			t.Fatalf("hook %s should exist: %v", name, err)
		}
		if strings.Contains(string(data), chainComment) {
			t.Errorf("hook %s should NOT contain chain call", name)
		}
	}
}

func TestRemoveGitHook_RestoresBackup(t *testing.T) {
	_, hooksDir := initHooksTestRepo(t)

	// Create a custom hook, install (backs it up), then remove
	customContent := "#!/bin/sh\necho 'my custom hook'\n"
	hookPath := filepath.Join(hooksDir, "prepare-commit-msg")
	if err := os.WriteFile(hookPath, []byte(customContent), 0o755); err != nil {
		t.Fatalf("failed to create custom hook: %v", err)
	}

	_, err := InstallGitHook(true)
	if err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	}

	removed, err := RemoveGitHook()
	if err != nil {
		t.Fatalf("RemoveGitHook() error = %v", err)
	}
	if removed == 0 {
		t.Error("RemoveGitHook() should remove hooks")
	}

	// Original custom hook should be restored
	data, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("hook should be restored: %v", err)
	}
	if string(data) != customContent {
		t.Errorf("restored hook content = %q, want %q", string(data), customContent)
	}

	// Backup should be gone
	backupPath := hookPath + backupSuffix
	if _, err := os.Stat(backupPath); !os.IsNotExist(err) {
		t.Error("backup should be removed after restore")
	}
}

func TestRemoveGitHook_RestoresBackupWhenHookAlreadyGone(t *testing.T) {
	_, hooksDir := initHooksTestRepo(t)

	// Create custom hook, install (creates backup), then delete the main hook
	customContent := "#!/bin/sh\necho 'original'\n"
	hookPath := filepath.Join(hooksDir, "prepare-commit-msg")
	if err := os.WriteFile(hookPath, []byte(customContent), 0o755); err != nil {
		t.Fatalf("failed to create custom hook: %v", err)
	}

	_, err := InstallGitHook(true)
	if err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	}

	// Simulate another tool deleting our hook
	if err := os.Remove(hookPath); err != nil {
		t.Fatalf("failed to remove hook: %v", err)
	}

	_, err = RemoveGitHook()
	if err != nil {
		t.Fatalf("RemoveGitHook() error = %v", err)
	}

	// Backup should be restored even though the main hook was already gone
	data, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal("backup should be restored to main hook path")
	}
	if string(data) != customContent {
		t.Errorf("restored hook content = %q, want %q", string(data), customContent)
	}

	// Backup file should be gone
	backupPath := hookPath + backupSuffix
	if _, err := os.Stat(backupPath); !os.IsNotExist(err) {
		t.Error("backup file should not exist after restore")
	}
}

func TestGenerateChainedContent(t *testing.T) {
	t.Parallel()

	base := "#!/bin/sh\n# Entire CLI hooks\nentire hooks git pre-push \"$1\" || true\n"
	result := generateChainedContent(base, "pre-push")

	// Should start with the base content
	if !strings.HasPrefix(result, base) {
		t.Error("chained content should start with base content")
	}

	// Should contain the chain comment
	if !strings.Contains(result, chainComment) {
		t.Error("chained content should contain chain comment")
	}

	// Should resolve hook directory from $0
	if !strings.Contains(result, `_entire_hook_dir="$(dirname "$0")"`) {
		t.Error("chained content should resolve hook directory from $0")
	}

	// Should check executable permission on backup
	expectedCheck := `[ -x "$_entire_hook_dir/pre-push` + backupSuffix + `" ]`
	if !strings.Contains(result, expectedCheck) {
		t.Errorf("chained content should check -x on backup, got:\n%s", result)
	}

	// Should forward all arguments with "$@"
	expectedExec := `"$_entire_hook_dir/pre-push` + backupSuffix + `" "$@"`
	if !strings.Contains(result, expectedExec) {
		t.Errorf("chained content should execute backup with $@, got:\n%s", result)
	}
}

func TestInstallGitHook_InstallRemoveReinstall(t *testing.T) {
	_, hooksDir := initHooksTestRepo(t)

	// Create a custom hook
	customContent := "#!/bin/sh\necho 'user hook'\n"
	hookPath := filepath.Join(hooksDir, "prepare-commit-msg")
	if err := os.WriteFile(hookPath, []byte(customContent), 0o755); err != nil {
		t.Fatalf("failed to create custom hook: %v", err)
	}

	// Install: should back up and chain
	count, err := InstallGitHook(true)
	if err != nil {
		t.Fatalf("first install error: %v", err)
	}
	if count == 0 {
		t.Error("first install should install hooks")
	}
	backupPath := hookPath + backupSuffix
	if !fileExists(backupPath) {
		t.Fatal("backup should exist after install")
	}

	// Remove: should restore backup
	_, err = RemoveGitHook()
	if err != nil {
		t.Fatalf("remove error: %v", err)
	}
	data, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal("hook should be restored after remove")
	}
	if string(data) != customContent {
		t.Errorf("restored hook = %q, want %q", string(data), customContent)
	}
	if fileExists(backupPath) {
		t.Error("backup should not exist after remove")
	}

	// Reinstall: should back up again and chain
	count, err = InstallGitHook(true)
	if err != nil {
		t.Fatalf("reinstall error: %v", err)
	}
	if count == 0 {
		t.Error("reinstall should install hooks")
	}
	if !fileExists(backupPath) {
		t.Fatal("backup should exist after reinstall")
	}
	data, err = os.ReadFile(hookPath)
	if err != nil {
		t.Fatal("hook should exist after reinstall")
	}
	if !strings.Contains(string(data), entireHookMarker) {
		t.Error("reinstalled hook should contain Entire marker")
	}
	if !strings.Contains(string(data), chainComment) {
		t.Error("reinstalled hook should contain chain call")
	}
}

func TestRemoveGitHook_DoesNotOverwriteReplacedHook(t *testing.T) {
	_, hooksDir := initHooksTestRepo(t)

	// User has custom hook A
	hookPath := filepath.Join(hooksDir, "prepare-commit-msg")
	hookAContent := "#!/bin/sh\necho 'hook A'\n"
	if err := os.WriteFile(hookPath, []byte(hookAContent), 0o755); err != nil {
		t.Fatalf("failed to create hook A: %v", err)
	}

	// entire enable: backs up A, installs our hook with chain
	_, err := InstallGitHook(true)
	if err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	}

	// User replaces our hook with their own hook B
	hookBContent := "#!/bin/sh\necho 'hook B'\n"
	if err := os.WriteFile(hookPath, []byte(hookBContent), 0o755); err != nil {
		t.Fatalf("failed to create hook B: %v", err)
	}

	// entire disable: should NOT overwrite hook B with backup A
	_, err = RemoveGitHook()
	if err != nil {
		t.Fatalf("RemoveGitHook() error = %v", err)
	}

	// Hook B should still be in place
	data, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal("hook should still exist")
	}
	if string(data) != hookBContent {
		t.Errorf("hook content = %q, want hook B %q (should not be overwritten by backup)", string(data), hookBContent)
	}

	// Backup should still exist (not consumed)
	backupPath := hookPath + backupSuffix
	if !fileExists(backupPath) {
		t.Error("backup should be left in place when hook was modified")
	}
}

func TestRemoveGitHook_PermissionDenied(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("Test cannot run as root (permission checks are bypassed)")
	}

	tmpDir, _ := initHooksTestRepo(t)

	// Install hooks first
	_, err := InstallGitHook(true)
	if err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	}

	// Remove write permissions from hooks directory to cause permission error
	hooksDir := filepath.Join(tmpDir, ".git", "hooks")
	if err := os.Chmod(hooksDir, 0o555); err != nil {
		t.Fatalf("failed to change hooks dir permissions: %v", err)
	}
	// Restore permissions on cleanup
	t.Cleanup(func() {
		_ = os.Chmod(hooksDir, 0o755) //nolint:errcheck // Cleanup, best-effort
	})

	// Remove hooks should now fail with permission error
	removed, err := RemoveGitHook()
	if err == nil {
		t.Fatal("RemoveGitHook() should return error when hooks cannot be deleted")
	}
	if removed != 0 {
		t.Errorf("RemoveGitHook() removed %d hooks, expected 0 when all fail", removed)
	}
	if !strings.Contains(err.Error(), "failed to remove hooks") {
		t.Errorf("error should mention 'failed to remove hooks', got: %v", err)
	}
}

// Self-heal: reinstalling hooks after a third-party tool overwrites or deletes them.

func TestInstallGitHook_SelfHealAfterThirdPartyOverwrite(t *testing.T) {
	_, hooksDir := initHooksTestRepo(t)

	// Install Entire hooks, then simulate a third-party tool overwriting them
	if _, err := InstallGitHook(true); err != nil {
		t.Fatalf("initial install error: %v", err)
	}

	thirdPartyContents := make(map[string]string)
	for _, hook := range gitHookNames {
		content := "#!/bin/sh\n# third_party_tool managed hook\nrun_third_party \"" + hook + "\" \"$@\"\n"
		thirdPartyContents[hook] = content
		if err := os.WriteFile(filepath.Join(hooksDir, hook), []byte(content), 0o755); err != nil {
			t.Fatalf("failed to overwrite %s: %v", hook, err)
		}
	}

	// Detection: hooks should be seen as missing
	if IsGitHookInstalled() {
		t.Fatal("IsGitHookInstalled() = true after overwrite, want false")
	}

	// Self-heal reinstall
	count, err := InstallGitHook(true)
	if err != nil {
		t.Fatalf("self-heal error: %v", err)
	}
	if count == 0 {
		t.Error("self-heal should install hooks (count > 0)")
	}
	if !IsGitHookInstalled() {
		t.Error("hooks should be detected after self-heal")
	}

	// Each hook should have Entire marker, chain to backup, and backup should contain third-party content
	for _, hook := range gitHookNames {
		data, err := os.ReadFile(filepath.Join(hooksDir, hook))
		if err != nil {
			t.Fatalf("hook %s missing: %v", hook, err)
		}
		if !strings.Contains(string(data), entireHookMarker) {
			t.Errorf("hook %s missing Entire marker", hook)
		}
		if !strings.Contains(string(data), chainComment) {
			t.Errorf("hook %s missing chain call", hook)
		}

		backupData, err := os.ReadFile(filepath.Join(hooksDir, hook+backupSuffix))
		if err != nil {
			t.Fatalf("backup for %s missing: %v", hook, err)
		}
		if string(backupData) != thirdPartyContents[hook] {
			t.Errorf("backup for %s = %q, want third-party content", hook, string(backupData))
		}
	}
}

func TestInstallGitHook_SelfHealAfterHooksDeleted(t *testing.T) {
	_, hooksDir := initHooksTestRepo(t)

	if _, err := InstallGitHook(true); err != nil {
		t.Fatalf("initial install error: %v", err)
	}

	// Simulate third-party tool deleting all hooks
	for _, hook := range gitHookNames {
		if err := os.Remove(filepath.Join(hooksDir, hook)); err != nil {
			t.Fatalf("failed to delete %s: %v", hook, err)
		}
	}

	if IsGitHookInstalled() {
		t.Fatal("IsGitHookInstalled() = true after deletion, want false")
	}

	count, err := InstallGitHook(true)
	if err != nil {
		t.Fatalf("self-heal error: %v", err)
	}
	if count != len(gitHookNames) {
		t.Errorf("self-heal installed %d hooks, want %d", count, len(gitHookNames))
	}
	if !IsGitHookInstalled() {
		t.Error("hooks should be detected after self-heal")
	}

	// No backups should exist (nothing to back up)
	for _, hook := range gitHookNames {
		if _, err := os.Stat(filepath.Join(hooksDir, hook+backupSuffix)); !os.IsNotExist(err) {
			t.Errorf("backup for %s should not exist when hook was deleted", hook)
		}
	}
}

func TestInstallGitHook_SelfHealPreservesOriginalBackup(t *testing.T) {
	_, hooksDir := initHooksTestRepo(t)

	// User has a custom hook before Entire is ever installed
	originalContent := "#!/bin/sh\necho 'user original hook'\n"
	hookPath := filepath.Join(hooksDir, "prepare-commit-msg")
	if err := os.WriteFile(hookPath, []byte(originalContent), 0o755); err != nil {
		t.Fatalf("failed to create custom hook: %v", err)
	}

	// First install backs up the user's hook
	if _, err := InstallGitHook(true); err != nil {
		t.Fatalf("initial install error: %v", err)
	}

	// Third-party tool overwrites our hook (but backup still exists)
	thirdPartyContent := "#!/bin/sh\n# third_party_tool managed\nrun_third_party prepare-commit-msg\n"
	if err := os.WriteFile(hookPath, []byte(thirdPartyContent), 0o755); err != nil {
		t.Fatalf("failed to overwrite: %v", err)
	}

	// Self-heal
	if _, err := InstallGitHook(true); err != nil {
		t.Fatalf("self-heal error: %v", err)
	}

	// The ORIGINAL user backup must be preserved, not overwritten with third-party content
	backupData, err := os.ReadFile(hookPath + backupSuffix)
	if err != nil {
		t.Fatalf("backup should exist: %v", err)
	}
	if string(backupData) != originalContent {
		t.Errorf("original backup overwritten: got %q, want %q", string(backupData), originalContent)
	}
}
