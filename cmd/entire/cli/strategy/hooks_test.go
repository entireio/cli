package strategy

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

const goosWindows = "windows"

// Shared base body for chained-content execution tests.
const chainedHookBaseTrue = "#!/bin/sh\n# Entire CLI hooks\ntrue\n"

// clearGlobalHooksPath overrides any global core.hooksPath setting so that
// test repos use their default .git/hooks directory. Setting the local value
// takes precedence over the global one.
func clearGlobalHooksPath(t *testing.T, repoDir string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "config", "--local", "core.hooksPath", filepath.Join(repoDir, ".git", "hooks"))
	cmd.Dir = repoDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to set local core.hooksPath: %v", err)
	}
}

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
	clearGlobalHooksPath(t, tmpDir)
	paths.ClearWorktreeRootCache()

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

	result, err := getGitDirInPath(context.Background(), tmpDir)
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
	clearGlobalHooksPath(t, mainRepo)

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
	result, err := getGitDirInPath(context.Background(), worktreeDir)
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

	_, err := getGitDirInPath(context.Background(), tmpDir)
	if err == nil {
		t.Fatal("expected error for non-repo directory, got nil")
	}

	expectedMsg := "not a git repository"
	if err.Error() != expectedMsg {
		t.Errorf("expected error message %q, got %q", expectedMsg, err.Error())
	}
}

func TestGetHooksDirInPath_RegularRepo(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "git", "init")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}
	clearGlobalHooksPath(t, tmpDir)

	result, err := getHooksDirInPath(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := filepath.Join(tmpDir, ".git", "hooks")

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

func TestGetHooksDirInPath_Worktree(t *testing.T) {
	t.Parallel()

	mainRepo, worktreeDir := initHooksWorktreeRepo(t)

	result, err := getHooksDirInPath(context.Background(), worktreeDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := filepath.Join(mainRepo, ".git", "hooks")

	resultResolved, err := filepath.EvalSymlinks(result)
	if err != nil {
		t.Fatalf("failed to resolve symlinks for result: %v", err)
	}
	expectedResolved, err := filepath.EvalSymlinks(expected)
	if err != nil {
		t.Fatalf("failed to resolve symlinks for expected: %v", err)
	}

	// In a linked worktree, hooks should resolve to the common hooks dir.
	if resultResolved != expectedResolved {
		t.Errorf("expected hooks dir %s, got %s", expectedResolved, resultResolved)
	}
}

func TestGetHooksDirInPath_CoreHooksPath(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	ctx := context.Background()

	cmd := exec.CommandContext(ctx, "git", "init")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Relative core.hooksPath should resolve relative to repo root.
	cmd = exec.CommandContext(ctx, "git", "config", "core.hooksPath", ".githooks")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to set relative core.hooksPath: %v", err)
	}
	relativeResult, err := getHooksDirInPath(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("unexpected error for relative hooks path: %v", err)
	}
	relativeExpected := filepath.Join(tmpDir, ".githooks")
	if filepath.Clean(relativeResult) != filepath.Clean(relativeExpected) {
		t.Errorf("relative core.hooksPath expected %s, got %s", relativeExpected, relativeResult)
	}

	// Absolute core.hooksPath should be returned unchanged.
	absHooksPath := filepath.Join(tmpDir, "abs-hooks")
	cmd = exec.CommandContext(ctx, "git", "config", "core.hooksPath", absHooksPath)
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to set absolute core.hooksPath: %v", err)
	}
	absoluteResult, err := getHooksDirInPath(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("unexpected error for absolute hooks path: %v", err)
	}
	if filepath.Clean(absoluteResult) != filepath.Clean(absHooksPath) {
		t.Errorf("absolute core.hooksPath expected %s, got %s", absHooksPath, absoluteResult)
	}
}

func TestInstallGitHook_HooksPathNotADirectory(t *testing.T) {
	// core.hooksPath pointing at a non-directory (commonly /dev/null, the
	// "disable git hooks globally" idiom) must fail with guidance naming
	// core.hooksPath, not a raw mkdir error.
	tmpDir := t.TempDir()
	ctx := context.Background()

	cmd := exec.CommandContext(ctx, "git", "init")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	hooksPath := "/dev/null"
	if runtime.GOOS == goosWindows {
		hooksPath = filepath.Join(tmpDir, "not-a-dir")
		if err := os.WriteFile(hooksPath, []byte("x"), 0o600); err != nil {
			t.Fatalf("failed to create non-directory hooks path: %v", err)
		}
	}
	cmd = exec.CommandContext(ctx, "git", "config", "core.hooksPath", hooksPath)
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to set core.hooksPath: %v", err)
	}

	t.Chdir(tmpDir)
	ClearHooksDirCache()
	paths.ClearWorktreeRootCache()

	// Assert against the resolved hooks dir (what the error prints), not the
	// configured value — git may normalize separators on Windows.
	resolvedHooksDir, resolveErr := GetHooksDir(ctx)
	if resolveErr != nil {
		t.Fatalf("GetHooksDir() failed: %v", resolveErr)
	}

	_, _, err := InstallGitHook(ctx, true, false)
	if err == nil {
		t.Fatal("InstallGitHook() should fail when hooks path is not a directory")
	}
	msg := err.Error()
	if !strings.Contains(msg, "core.hooksPath") {
		t.Errorf("error should name core.hooksPath, got: %s", msg)
	}
	if !strings.Contains(msg, resolvedHooksDir) {
		t.Errorf("error should include the resolved hooks path %s, got: %s", resolvedHooksDir, msg)
	}
	if !strings.Contains(msg, "git config") {
		t.Errorf("error should tell the user how to inspect/fix the setting, got: %s", msg)
	}
}

func TestInstallGitHook_HooksPathUnderNonDirectory(t *testing.T) {
	// core.hooksPath pointing below a non-directory (e.g. /dev/null/hooks)
	// makes os.Stat fail with ENOTDIR instead of succeeding on a non-dir;
	// the guidance must fire for this variant too.
	if runtime.GOOS == goosWindows {
		t.Skip("ENOTDIR detection is POSIX-specific; Windows falls back to the raw mkdir error")
	}
	tmpDir := t.TempDir()
	ctx := context.Background()

	cmd := exec.CommandContext(ctx, "git", "init")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}
	cmd = exec.CommandContext(ctx, "git", "config", "core.hooksPath", "/dev/null/hooks")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to set core.hooksPath: %v", err)
	}

	t.Chdir(tmpDir)
	ClearHooksDirCache()
	paths.ClearWorktreeRootCache()

	_, _, err := InstallGitHook(ctx, true, false)
	if err == nil {
		t.Fatal("InstallGitHook() should fail when hooks path is under a non-directory")
	}
	if !strings.Contains(err.Error(), "core.hooksPath") {
		t.Errorf("error should name core.hooksPath, got: %s", err)
	}
}

func TestInstallGitHook_HooksPathNonexistentIsCreated(t *testing.T) {
	// A configured-but-missing core.hooksPath is legitimate: the guard must
	// not fire, and MkdirAll must create the directory and install hooks.
	tmpDir := t.TempDir()
	ctx := context.Background()

	cmd := exec.CommandContext(ctx, "git", "init")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}
	hooksPath := filepath.Join(tmpDir, "githooks-not-yet-created")
	cmd = exec.CommandContext(ctx, "git", "config", "core.hooksPath", hooksPath)
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to set core.hooksPath: %v", err)
	}

	t.Chdir(tmpDir)
	ClearHooksDirCache()
	paths.ClearWorktreeRootCache()

	count, _, err := InstallGitHook(ctx, true, false)
	if err != nil {
		t.Fatalf("InstallGitHook() should create a nonexistent hooks path: %v", err)
	}
	if count == 0 {
		t.Fatal("InstallGitHook() should install hooks into the created directory")
	}
	for _, hook := range gitHookNames {
		data, readErr := os.ReadFile(filepath.Join(hooksPath, hook))
		if readErr != nil {
			t.Fatalf("expected hook %s in created hooks dir: %v", hook, readErr)
		}
		if !strings.Contains(string(data), entireHookMarker) {
			t.Errorf("hook %s should contain Entire marker", hook)
		}
	}
}

func TestInstallGitHook_WorktreeInstallsInCommonHooks(t *testing.T) {
	mainRepo, worktreeDir := initHooksWorktreeRepo(t)
	t.Chdir(worktreeDir)
	paths.ClearWorktreeRootCache()

	count, _, err := InstallGitHook(context.Background(), true, false)
	if err != nil {
		t.Fatalf("InstallGitHook() in worktree failed: %v", err)
	}
	if count == 0 {
		t.Fatal("InstallGitHook() should install hooks in worktree")
	}

	// Hooks should be installed in common .git/hooks, not in .git/worktrees/<name>/hooks.
	commonHooksDir := filepath.Join(mainRepo, ".git", "hooks")
	for _, hook := range gitHookNames {
		data, readErr := os.ReadFile(filepath.Join(commonHooksDir, hook))
		if readErr != nil {
			t.Fatalf("expected common hook %s to exist: %v", hook, readErr)
		}
		if !strings.Contains(string(data), entireHookMarker) {
			t.Errorf("common hook %s should contain Entire marker", hook)
		}
	}

	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-dir")
	cmd.Dir = worktreeDir
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("failed to get worktree git dir: %v", err)
	}
	worktreeGitDir := strings.TrimSpace(string(output))
	if !filepath.IsAbs(worktreeGitDir) {
		worktreeGitDir = filepath.Join(worktreeDir, worktreeGitDir)
	}
	for _, hook := range gitHookNames {
		wtHookPath := filepath.Join(worktreeGitDir, "hooks", hook)
		if data, readErr := os.ReadFile(wtHookPath); readErr == nil && strings.Contains(string(data), entireHookMarker) {
			t.Errorf("worktree-local hook %s should not contain Entire marker (should install in common hooks dir)", hook)
		}
	}

	if !IsGitHookInstalledInDir(context.Background(), worktreeDir) {
		t.Error("IsGitHookInstalledInDir(worktree) should be true after install")
	}
}

func initHooksWorktreeRepo(t *testing.T) (string, string) {
	t.Helper()

	tmpDir := t.TempDir()
	mainRepo := filepath.Join(tmpDir, "main")
	worktreeDir := filepath.Join(tmpDir, "worktree")

	if err := os.MkdirAll(mainRepo, 0o755); err != nil {
		t.Fatalf("failed to create main repo dir: %v", err)
	}

	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "git", "init")
	cmd.Dir = mainRepo
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to init main repo: %v", err)
	}
	clearGlobalHooksPath(t, mainRepo)

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

	cmd = exec.CommandContext(ctx, "git", "worktree", "add", worktreeDir, "-b", "feature")
	cmd.Dir = mainRepo
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to create worktree: %v", err)
	}

	return mainRepo, worktreeDir
}

// isGitSequenceOperation tests use t.Chdir() so cannot call t.Parallel().

func TestIsGitSequenceOperation_NoOperation(t *testing.T) {
	initHooksTestRepo(t)

	if isGitSequenceOperation(context.Background()) {
		t.Error("isGitSequenceOperation(context.Background()) = true, want false for clean repo")
	}
}

func TestIsGitSequenceOperation_RebaseMerge(t *testing.T) {
	tmpDir, _ := initHooksTestRepo(t)

	if err := os.MkdirAll(filepath.Join(tmpDir, ".git", "rebase-merge"), 0o755); err != nil {
		t.Fatalf("failed to create rebase-merge dir: %v", err)
	}

	if !isGitSequenceOperation(context.Background()) {
		t.Error("isGitSequenceOperation(context.Background()) = false, want true during rebase-merge")
	}
}

func TestIsGitSequenceOperation_RebaseApply(t *testing.T) {
	tmpDir, _ := initHooksTestRepo(t)

	if err := os.MkdirAll(filepath.Join(tmpDir, ".git", "rebase-apply"), 0o755); err != nil {
		t.Fatalf("failed to create rebase-apply dir: %v", err)
	}

	if !isGitSequenceOperation(context.Background()) {
		t.Error("isGitSequenceOperation(context.Background()) = false, want true during rebase-apply")
	}
}

func TestIsGitSequenceOperation_CherryPick(t *testing.T) {
	tmpDir, _ := initHooksTestRepo(t)

	if err := os.WriteFile(filepath.Join(tmpDir, ".git", "CHERRY_PICK_HEAD"), []byte("abc123"), 0o644); err != nil {
		t.Fatalf("failed to create CHERRY_PICK_HEAD: %v", err)
	}

	if !isGitSequenceOperation(context.Background()) {
		t.Error("isGitSequenceOperation(context.Background()) = false, want true during cherry-pick")
	}
}

func TestIsGitSequenceOperation_Revert(t *testing.T) {
	tmpDir, _ := initHooksTestRepo(t)

	if err := os.WriteFile(filepath.Join(tmpDir, ".git", "REVERT_HEAD"), []byte("abc123"), 0o644); err != nil {
		t.Fatalf("failed to create REVERT_HEAD: %v", err)
	}

	if !isGitSequenceOperation(context.Background()) {
		t.Error("isGitSequenceOperation(context.Background()) = false, want true during revert")
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
	if isGitSequenceOperation(context.Background()) {
		t.Error("isGitSequenceOperation(context.Background()) = true in clean worktree, want false")
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
	if !isGitSequenceOperation(context.Background()) {
		t.Error("isGitSequenceOperation(context.Background()) = false in worktree during rebase, want true")
	}
}

func TestInstallGitHook_Idempotent(t *testing.T) {
	_, hooksDir := initHooksTestRepo(t)

	// First install should install hooks
	firstCount, _, err := InstallGitHook(context.Background(), true, false)
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
	secondCount, _, err := InstallGitHook(context.Background(), true, false)
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

// TestIsGitHookInstalled_LegacyLocalDevCountsAsNotInstalled pins the trigger that
// makes the migration actually happen. EnsureSetup (every turn-start) reinstalls
// only when IsGitHookInstalled reports false, so a legacy local-dev hook — which
// still carries entireHookMarker — would otherwise read as installed forever
// while invoking a launcher script that no longer exists. Because a repo-relative
// prefix gets no availability guard and pre-push propagates exit codes, that
// state rejects `git push`.
// TestCheckGitHookState covers the three states, and specifically that a legacy
// hook is Outdated rather than Absent. The distinction is load-bearing: uninstall
// asks "is there anything of ours to remove" and would skip a stale hook if it
// read as Absent, while EnsureSetup asks "are these current" and would leave a
// stale hook forever if it read as Current.
func TestCheckGitHookState(t *testing.T) {
	t.Parallel()

	t.Run("no hooks at all is Absent", func(t *testing.T) {
		t.Parallel()
		if got := gitHookStateInHooksDir(context.Background(), t.TempDir()); got != GitHooksAbsent {
			t.Errorf("empty hooks dir = %v, want GitHooksAbsent", got)
		}
	})

	t.Run("a foreign hook at our path is Absent", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeCurrentManagedHooks(t, dir)
		// Overwrite one with somebody else's hook.
		if err := os.WriteFile(filepath.Join(dir, "pre-push"), []byte("#!/bin/sh\necho lefthook\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if got := gitHookStateInHooksDir(context.Background(), dir); got != GitHooksAbsent {
			t.Errorf("foreign hook = %v, want GitHooksAbsent", got)
		}
	})

	t.Run("an incomplete set is Absent", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeCurrentManagedHooks(t, dir)
		if err := os.Remove(filepath.Join(dir, "post-commit")); err != nil {
			t.Fatal(err)
		}
		if got := gitHookStateInHooksDir(context.Background(), dir); got != GitHooksAbsent {
			t.Errorf("incomplete set = %v, want GitHooksAbsent", got)
		}
	})

	// Kept filesystem-only rather than installing for real: initHooksTestRepo
	// chdirs, which cannot combine with t.Parallel. A real install is already
	// asserted to read Current by
	// TestIsGitHookInstalled_LegacyLocalDevCountsAsNotInstalled.
	t.Run("hooks in the current shape are Current", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeCurrentManagedHooks(t, dir)
		if got := gitHookStateInHooksDir(context.Background(), dir); got != GitHooksCurrent {
			t.Errorf("current-shape hooks = %v, want GitHooksCurrent", got)
		}
	})

	t.Run("a single legacy hook makes the set Outdated", func(t *testing.T) {
		t.Parallel()
		for _, legacy := range []string{
			"./scripts/entire-dev hooks git pre-push",
			`go run ./cmd/entire/main.go hooks git pre-push "$1" || true`,
		} {
			dir := t.TempDir()
			writeCurrentManagedHooks(t, dir)
			legacyContent := "#!/bin/sh\n# " + entireHookMarker + "\n" + legacy + "\n"
			if err := os.WriteFile(filepath.Join(dir, "pre-push"), []byte(legacyContent), 0o755); err != nil {
				t.Fatal(err)
			}
			if got := gitHookStateInHooksDir(context.Background(), dir); got != GitHooksOutdated {
				t.Errorf("legacy hook %q = %v, want GitHooksOutdated", legacy, got)
			}
		}
	})
}

// TestCheckGitHookState_UserAdditionsAreNotDrift pins that a hook Entire
// installed and the user then hand-edited is not classified as stale just because
// one of their own lines mentions `go run` or the old launcher path.
//
// The classification decides whether EnsureSetup rewrites the file, and
// InstallGitHook only backs up a hook that does NOT carry entireHookMarker — so a
// hand-edited hook is overwritten with no backup. A whole-file substring match
// would therefore silently discard the user's additions.
func TestCheckGitHookState_UserAdditionsAreNotDrift(t *testing.T) {
	t.Parallel()

	for _, userLine := range []string{
		"go run ./tools/mylint.go",
		"sh ./scripts/entire-dev-notes.sh",
	} {
		dir := t.TempDir()
		for _, hook := range gitHookNames {
			content := "#!/bin/sh\n# " + entireHookMarker + "\n" +
				"if command -v entire >/dev/null 2>&1; then entire hooks git " + hook + "; else :; fi\n" +
				userLine + "\n"
			if err := os.WriteFile(filepath.Join(dir, hook), []byte(content), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if got := gitHookStateInHooksDir(context.Background(), dir); got != GitHooksCurrent {
			t.Errorf("a hook whose own Entire line is current must read Current despite the user line %q, got %v", userLine, got)
		}
	}
}

// TestCheckGitHookState_LegacyEntireLineIsStillDrift is the other half: when the
// legacy launcher is on Entire's OWN invocation line, that is genuinely our stale
// hook and must be replaced, user additions around it notwithstanding.
func TestCheckGitHookState_LegacyEntireLineIsStillDrift(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for _, hook := range gitHookNames {
		content := "#!/bin/sh\n# " + entireHookMarker + "\n" +
			"./scripts/entire-dev hooks git " + hook + "\n" +
			"echo my own step\n"
		if err := os.WriteFile(filepath.Join(dir, hook), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := gitHookStateInHooksDir(context.Background(), dir); got != GitHooksOutdated {
		t.Errorf("a legacy launcher on Entire's own invocation line must read Outdated, got %v", got)
	}
}

// writeCurrentManagedHooks writes every managed hook in the shape this version
// installs. Tests that need a variant overwrite an individual hook afterwards.
func writeCurrentManagedHooks(t *testing.T, dir string) {
	t.Helper()
	for _, hook := range gitHookNames {
		content := "#!/bin/sh\n# " + entireHookMarker + "\nentire hooks git " + hook + "\n"
		if err := os.WriteFile(filepath.Join(dir, hook), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIsGitHookInstalled_LegacyLocalDevCountsAsNotInstalled(t *testing.T) {
	_, hooksDir := initHooksTestRepo(t)

	// A current install must read as installed.
	if _, _, err := InstallGitHook(context.Background(), true, false); err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	}
	if got := gitHookStateInHooksDir(context.Background(), hooksDir); got != GitHooksCurrent {
		t.Fatalf("a freshly installed hook set should read as current, got %v", got)
	}

	// Rewriting a single hook into either legacy shape must flip that to false,
	// so the next EnsureSetup replaces it. Both forms were written by local-dev
	// mode over time; a real clone was found still carrying the `go run` one.
	for _, legacyCmd := range []string{
		"./scripts/entire-dev hooks git pre-push",
		`go run ./cmd/entire/main.go hooks git pre-push "$1" || true`,
	} {
		if _, _, err := InstallGitHook(context.Background(), true, false); err != nil {
			t.Fatalf("reinstall before seeding: %v", err)
		}
		legacy := "#!/bin/sh\n# " + entireHookMarker + "\n" + legacyCmd + "\n"
		if err := os.WriteFile(filepath.Join(hooksDir, "pre-push"), []byte(legacy), 0o755); err != nil {
			t.Fatal(err)
		}
		if got := gitHookStateInHooksDir(context.Background(), hooksDir); got != GitHooksOutdated {
			t.Errorf("a hook running Entire from the working tree must read as outdated, got %v: %s", got, legacyCmd)
		}
	}
}

func TestInstallGitHook_RewritesLegacyLocalDevHooks(t *testing.T) {
	_, hooksDir := initHooksTestRepo(t)

	// Seed hooks in the shape the removed local-dev mode wrote: a repo-relative
	// launcher script. They carry entireHookMarker, so they are ours to replace.
	const legacyPrefix = "./scripts/entire-dev"
	for _, hook := range gitHookNames {
		content := "#!/bin/sh\n# " + entireHookMarker + "\n" + legacyPrefix + " hooks git " + hook + "\n"
		if err := os.WriteFile(filepath.Join(hooksDir, hook), []byte(content), 0o755); err != nil {
			t.Fatalf("seed %s: %v", hook, err)
		}
	}

	// Reinstall — hooks should update to use "entire" prefix
	count, _, err := InstallGitHook(context.Background(), true, false)
	if err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	}
	if count == 0 {
		t.Fatal("InstallGitHook() should rewrite legacy local-dev hooks (content changed)")
	}

	for _, hook := range gitHookNames {
		data, err := os.ReadFile(filepath.Join(hooksDir, hook))
		if err != nil {
			t.Fatalf("hook %s should exist: %v", hook, err)
		}
		content := string(data)
		if strings.Contains(content, "scripts/entire-dev") {
			t.Errorf("hook %s still runs a script inside the working tree, got:\n%s", hook, content)
		}
		if !strings.Contains(content, "entire hooks git") {
			t.Errorf("hook %s should invoke the entire binary, got:\n%s", hook, content)
		}
	}
}

func TestGitHookCommand_GuardsEveryInstallablePrefix(t *testing.T) {
	t.Parallel()

	const args = `prepare-commit-msg "$1" "$2" 2>/dev/null || true`

	cases := []struct {
		name      string
		prefix    string
		wantGuard string
	}{
		{"bare binary on PATH", "entire", "command -v entire >/dev/null 2>&1"},
		{"absolute unix path", "'/opt/homebrew/bin/entire'", "[ -x '/opt/homebrew/bin/entire' ]"},
		{"windows absolute path", `'C:\Users\me\entire.exe'`, `[ -f 'C:\Users\me\entire.exe' ]`},
		// An unclassifiable absolute form (UNC / network share) must still be
		// guarded rather than falling through unguarded.
		{"windows UNC path", `'\\server\share\entire.exe'`, `[ -x '\\server\share\entire.exe' ]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			command := gitHookCommand(tc.prefix, args, false)

			if !strings.Contains(command, tc.wantGuard) {
				t.Errorf("command should guard on %q, got: %s", tc.wantGuard, command)
			}
			if !strings.HasPrefix(command, "if ") {
				t.Errorf("every hook command must be guarded by an existence test, got: %s", command)
			}
			if !strings.Contains(command, tc.prefix+" hooks git "+args) {
				t.Errorf("command should invoke the prefix verbatim, got: %s", command)
			}
			// No build-probe or fallback logic belongs in a hook command.
			if strings.Contains(command, "go build") || strings.Contains(command, "go run") {
				t.Errorf("hook command must not build or run from source: %s", command)
			}
		})
	}
}

// TestHookCmdPrefix_NeverNamesRepoContent enforces the invariant the local-dev
// removal was about: the prefix embedded in a git hook must name a binary outside
// the repository, never a path that resolves inside the working tree. A
// repo-relative prefix would run whatever the checked-out branch contains on every
// git operation.
func TestHookCmdPrefix_NeverNamesRepoContent(t *testing.T) {
	t.Parallel()

	for _, absolutePath := range []bool{false, true} {
		prefix, err := hookCmdPrefix(absolutePath)
		if err != nil {
			t.Fatalf("hookCmdPrefix(%v) error = %v", absolutePath, err)
		}
		if prefix == "entire" {
			continue // resolved through PATH, not the repo
		}
		unquoted := strings.Trim(prefix, "'")
		if !filepath.IsAbs(unquoted) {
			t.Errorf("hookCmdPrefix(%v) = %q, which is not absolute — a relative prefix resolves inside the repo", absolutePath, prefix)
		}
		if strings.HasPrefix(unquoted, ".") || strings.Contains(unquoted, "$(") {
			t.Errorf("hookCmdPrefix(%v) = %q must not reference repository content or resolve a path at hook runtime", absolutePath, prefix)
		}
	}
}

func TestInstallGitHook_AbsoluteGitHookPath(t *testing.T) {
	_, hooksDir := initHooksTestRepo(t)

	// Install with absolutePath=true
	count, _, err := InstallGitHook(context.Background(), true, true)
	if err != nil {
		t.Fatalf("InstallGitHook(absolutePath=true) error = %v", err)
	}
	if count == 0 {
		t.Fatal("InstallGitHook(absolutePath=true) should install hooks")
	}

	// Get the expected absolute path (shell-quoted)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		t.Fatalf("filepath.EvalSymlinks() error = %v", err)
	}
	quoted := shellQuote(resolved)

	for _, hook := range gitHookNames {
		data, err := os.ReadFile(filepath.Join(hooksDir, hook))
		if err != nil {
			t.Fatalf("hook %s should exist: %v", hook, err)
		}
		content := string(data)
		if !strings.Contains(content, quoted) {
			t.Errorf("hook %s should contain shell-quoted absolute path %q, got:\n%s", hook, quoted, content)
		}
		if strings.Contains(content, "\nentire ") {
			t.Errorf("hook %s should not use bare 'entire' prefix when absolutePath=true", hook)
		}
	}
}

func TestShellQuote(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"/usr/local/bin/entire", "'/usr/local/bin/entire'"},
		{"/Users/John O'Brien/bin/entire", "'/Users/John O'\\''Brien/bin/entire'"},
		{"/path with spaces/entire", "'/path with spaces/entire'"},
		{"/simple", "'/simple'"},
	}

	for _, tt := range tests {
		got := shellQuote(tt.input)
		if got != tt.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestGitHookCommand_MissingWarningIsNonFatal(t *testing.T) {
	t.Parallel()

	command := gitHookCommand("entire", `commit-msg "$1" || true`, true)
	if !strings.Contains(command, ">&2 || :") {
		t.Fatalf("missing-entire warning should be explicitly non-fatal, got:\n%s", command)
	}
}

func TestGitHookCommandAvailableTest_WindowsAbsolutePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		cmdPrefix string
		want      string
	}{
		{
			name:      "backslash path",
			cmdPrefix: shellQuote(`C:\Program Files\Entire\entire.exe`),
			want:      `[ -f 'C:\Program Files\Entire\entire.exe' ]`,
		},
		{
			name:      "slash path",
			cmdPrefix: shellQuote(`z:/tools/entire.exe`),
			want:      `[ -f 'z:/tools/entire.exe' ]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := gitHookCommandAvailableTest(tt.cmdPrefix)
			if got != tt.want {
				t.Fatalf("gitHookCommandAvailableTest(%q) = %q, want %q", tt.cmdPrefix, got, tt.want)
			}
		})
	}
}

func TestInstallGitHook_CoreHooksPathRelative(t *testing.T) {
	tmpDir, _ := initHooksTestRepo(t)
	ctx := context.Background()

	// Simulate Husky-style override: git invokes stubs in .husky/_, but Entire
	// must install into the parent .husky/ user-hook directory so husky prepare
	// cannot clobber the wrappers (issue #784).
	cmd := exec.CommandContext(ctx, "git", "config", "core.hooksPath", ".husky/_")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to set core.hooksPath: %v", err)
	}
	// Seed husky-owned stubs + dispatcher the way `husky` would.
	huskyOwnedDir := filepath.Join(tmpDir, ".husky", "_")
	seedHuskyOwnedDir(t, huskyOwnedDir)

	count, _, err := InstallGitHook(context.Background(), true, false)
	if err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	}
	if count == 0 {
		t.Fatal("InstallGitHook() should install hooks when core.hooksPath is set")
	}

	userHooksDir := filepath.Join(tmpDir, ".husky")
	for _, hook := range gitHookNames {
		hookPath := filepath.Join(userHooksDir, hook)
		data, readErr := os.ReadFile(hookPath)
		if readErr != nil {
			t.Fatalf("expected hook %s in husky user-hook dir: %v", hook, readErr)
		}
		if !strings.Contains(string(data), entireHookMarker) {
			t.Errorf("hook %s in .husky/ should contain Entire marker", hook)
		}
	}

	// Must not replace husky-owned stubs in .husky/_.
	for _, hook := range gitHookNames {
		hookPath := filepath.Join(huskyOwnedDir, hook)
		data, readErr := os.ReadFile(hookPath)
		if readErr != nil {
			t.Fatalf("expected husky stub %s to remain: %v", hook, readErr)
		}
		if strings.Contains(string(data), entireHookMarker) {
			t.Errorf("husky-owned stub %s must not contain Entire marker", hook)
		}
	}

	// Ensure we did not incorrectly write Entire hooks into .git/hooks.
	defaultHooksDir := filepath.Join(tmpDir, ".git", "hooks")
	for _, hook := range gitHookNames {
		defaultHookPath := filepath.Join(defaultHooksDir, hook)
		if data, readErr := os.ReadFile(defaultHookPath); readErr == nil && strings.Contains(string(data), entireHookMarker) {
			t.Errorf("default hook %s should not contain Entire marker when core.hooksPath is set", hook)
		}
	}

	if !IsGitHookInstalledInDir(context.Background(), tmpDir) {
		t.Error("IsGitHookInstalledInDir() should detect hooks installed in .husky/")
	}
}

func TestInstallGitHook_HuskyPrepareDoesNotClobberUserHooks(t *testing.T) {
	tmpDir, _ := initHooksTestRepo(t)
	ctx := context.Background()

	cmd := exec.CommandContext(ctx, "git", "config", "core.hooksPath", ".husky/_")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to set core.hooksPath: %v", err)
	}
	huskyOwnedDir := filepath.Join(tmpDir, ".husky", "_")
	seedHuskyOwnedDir(t, huskyOwnedDir)

	if _, _, err := InstallGitHook(context.Background(), true, false); err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	}
	if !IsGitHookInstalledInDir(context.Background(), tmpDir) {
		t.Fatal("hooks should be installed before simulated husky prepare")
	}

	// Simulate `npx husky` / npm prepare regenerating .husky/_.
	seedHuskyOwnedDir(t, huskyOwnedDir)

	if !IsGitHookInstalledInDir(context.Background(), tmpDir) {
		t.Fatal("Entire hooks in .husky/ must survive husky prepare regenerating .husky/_")
	}
	for _, hook := range gitHookNames {
		data, err := os.ReadFile(filepath.Join(tmpDir, ".husky", hook))
		if err != nil {
			t.Fatalf("read user hook %s: %v", hook, err)
		}
		if !strings.Contains(string(data), entireHookMarker) {
			t.Errorf("user hook %s lost Entire marker after husky prepare", hook)
		}
	}
}

func TestInstallGitHook_MigratesLegacyEntireHooksFromHuskyOwnedDir(t *testing.T) {
	tmpDir, _ := initHooksTestRepo(t)
	ctx := context.Background()

	cmd := exec.CommandContext(ctx, "git", "config", "core.hooksPath", ".husky/_")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to set core.hooksPath: %v", err)
	}

	huskyOwnedDir := filepath.Join(tmpDir, ".husky", "_")
	if err := os.MkdirAll(huskyOwnedDir, 0o755); err != nil {
		t.Fatalf("mkdir .husky/_: %v", err)
	}
	// Legacy layout: Entire wrappers live in `_`, husky stubs backed up beside them.
	for _, hook := range gitHookNames {
		stub := huskyForwardingStub
		if err := os.WriteFile(filepath.Join(huskyOwnedDir, hook+backupSuffix), []byte(stub), 0o755); err != nil {
			t.Fatalf("write backup stub %s: %v", hook, err)
		}
		legacy := "#!/bin/sh\n# " + entireHookMarker + "\nentire hooks git " + hook + "\n"
		if err := os.WriteFile(filepath.Join(huskyOwnedDir, hook), []byte(legacy), 0o755); err != nil {
			t.Fatalf("write legacy Entire hook %s: %v", hook, err)
		}
	}
	// Dispatcher present so hookInstallDir redirects to parent.
	if err := os.WriteFile(filepath.Join(huskyOwnedDir, "h"), []byte("#!/usr/bin/env sh\necho husky-dispatcher\n"), 0o755); err != nil {
		t.Fatalf("write husky dispatcher: %v", err)
	}

	if _, _, err := InstallGitHook(context.Background(), true, false); err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	}

	for _, hook := range gitHookNames {
		userData, err := os.ReadFile(filepath.Join(tmpDir, ".husky", hook))
		if err != nil {
			t.Fatalf("expected migrated hook in .husky/%s: %v", hook, err)
		}
		if !strings.Contains(string(userData), entireHookMarker) {
			t.Errorf(".husky/%s should contain Entire marker after migrate", hook)
		}
		ownedData, err := os.ReadFile(filepath.Join(huskyOwnedDir, hook))
		if err != nil {
			t.Fatalf("expected restored husky stub %s: %v", hook, err)
		}
		if strings.Contains(string(ownedData), entireHookMarker) {
			t.Errorf("legacy Entire wrapper should be removed from .husky/_/%s", hook)
		}
		if !strings.Contains(string(ownedData), `dirname "$0"`) {
			t.Errorf("husky stub should be restored for %s", hook)
		}
	}
}

func TestInstallGitHook_MigratesLegacyEntireHooksWithoutBackupWritesStub(t *testing.T) {
	tmpDir, _ := initHooksTestRepo(t)
	ctx := context.Background()

	cmd := exec.CommandContext(ctx, "git", "config", "core.hooksPath", ".husky/_")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to set core.hooksPath: %v", err)
	}

	huskyOwnedDir := filepath.Join(tmpDir, ".husky", "_")
	if err := os.MkdirAll(huskyOwnedDir, 0o755); err != nil {
		t.Fatalf("mkdir .husky/_: %v", err)
	}
	if err := os.WriteFile(filepath.Join(huskyOwnedDir, "h"), []byte("#!/usr/bin/env sh\necho husky-dispatcher\n"), 0o755); err != nil {
		t.Fatalf("write husky dispatcher: %v", err)
	}
	// Legacy Entire wrappers with no .pre-entire backup.
	for _, hook := range gitHookNames {
		legacy := "#!/bin/sh\n# " + entireHookMarker + "\nentire hooks git " + hook + "\n"
		if err := os.WriteFile(filepath.Join(huskyOwnedDir, hook), []byte(legacy), 0o755); err != nil {
			t.Fatalf("write legacy Entire hook %s: %v", hook, err)
		}
	}

	if _, _, err := InstallGitHook(context.Background(), true, false); err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	}
	if !IsGitHookInstalledInDir(context.Background(), tmpDir) {
		t.Fatal("hooks should be installed after migrate-without-backup")
	}
	for _, hook := range gitHookNames {
		ownedData, err := os.ReadFile(filepath.Join(huskyOwnedDir, hook))
		if err != nil {
			t.Fatalf("expected husky stub written for %s: %v", hook, err)
		}
		if string(ownedData) != huskyForwardingStub {
			t.Errorf("stub for %s = %q, want huskyForwardingStub", hook, ownedData)
		}
	}
}

// seedHuskyOwnedDir writes husky v9 stubs + the `_/h` dispatcher into ownedDir.
func seedHuskyOwnedDir(t *testing.T, ownedDir string) {
	t.Helper()
	if err := os.MkdirAll(ownedDir, 0o755); err != nil {
		t.Fatalf("mkdir husky owned dir: %v", err)
	}
	dispatcher := "#!/usr/bin/env sh\nn=$(basename \"$0\")\ns=$(dirname \"$(dirname \"$0\")\")/$n\n[ ! -f \"$s\" ] && exit 0\nsh -e \"$s\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(ownedDir, "h"), []byte(dispatcher), 0o755); err != nil {
		t.Fatalf("write husky dispatcher: %v", err)
	}
	stub := "#!/usr/bin/env sh\n. \"$(dirname \"$0\")/h\"\n"
	for _, hook := range gitHookNames {
		if err := os.WriteFile(filepath.Join(ownedDir, hook), []byte(stub), 0o755); err != nil {
			t.Fatalf("write husky stub %s: %v", hook, err)
		}
	}
}

func TestRemoveGitHook_CoreHooksPathRelative(t *testing.T) {
	tmpDir, _ := initHooksTestRepo(t)
	ctx := context.Background()

	cmd := exec.CommandContext(ctx, "git", "config", "core.hooksPath", ".husky/_")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to set core.hooksPath: %v", err)
	}
	seedHuskyOwnedDir(t, filepath.Join(tmpDir, ".husky", "_"))

	installCount, _, err := InstallGitHook(context.Background(), true, false)
	if err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	}
	if installCount == 0 {
		t.Fatal("InstallGitHook() should install hooks before removal test")
	}

	// Hooks must be installed in .husky/ user-hook dir (not regenerable .husky/_).
	userHooksDir := filepath.Join(tmpDir, ".husky")
	for _, hook := range gitHookNames {
		hookPath := filepath.Join(userHooksDir, hook)
		if _, statErr := os.Stat(hookPath); statErr != nil {
			t.Fatalf("expected hook %s in .husky/ before removal: %v", hook, statErr)
		}
	}

	removeCount, err := RemoveGitHook(context.Background())
	if err != nil {
		t.Fatalf("RemoveGitHook(context.Background()) error = %v", err)
	}
	if removeCount != installCount {
		t.Errorf("RemoveGitHook(context.Background()) returned %d, want %d", removeCount, installCount)
	}

	for _, hook := range gitHookNames {
		hookPath := filepath.Join(userHooksDir, hook)
		if _, statErr := os.Stat(hookPath); !os.IsNotExist(statErr) {
			t.Errorf("hook file %s should not exist in .husky/ after removal", hook)
		}
	}

	if IsGitHookInstalledInDir(context.Background(), tmpDir) {
		t.Error("IsGitHookInstalledInDir() should be false after removing hooks in .husky/")
	}
}

func TestHuskyUserHooksDir(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	huskyOwned := filepath.Join(tmp, ".husky", "_")
	if err := os.MkdirAll(huskyOwned, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Without dispatcher, do not redirect (git would not forward to parent).
	if got := huskyUserHooksDir(huskyOwned); got != "" {
		t.Errorf("huskyUserHooksDir without dispatcher = %q, want \"\"", got)
	}
	if got := hookInstallDir(huskyOwned); got != huskyOwned {
		t.Errorf("hookInstallDir without dispatcher = %q, want %q", got, huskyOwned)
	}

	if err := os.WriteFile(filepath.Join(huskyOwned, "h"), []byte("#!/usr/bin/env sh\necho husky-dispatcher\n"), 0o755); err != nil {
		t.Fatalf("write dispatcher: %v", err)
	}
	wantParent := filepath.Join(tmp, ".husky")
	if got := huskyUserHooksDir(huskyOwned); got != wantParent {
		t.Errorf("huskyUserHooksDir with dispatcher = %q, want %q", got, wantParent)
	}
	if got := hookInstallDir(huskyOwned); got != wantParent {
		t.Errorf("hookInstallDir with dispatcher = %q, want %q", got, wantParent)
	}

	// Shebang-only / empty dispatcher must not redirect.
	if err := os.WriteFile(filepath.Join(huskyOwned, "h"), []byte("#!/usr/bin/env sh\n"), 0o755); err != nil {
		t.Fatalf("write shebang-only dispatcher: %v", err)
	}
	if got := huskyUserHooksDir(huskyOwned); got != "" {
		t.Errorf("huskyUserHooksDir shebang-only = %q, want \"\"", got)
	}
	if err := os.WriteFile(filepath.Join(huskyOwned, "h"), []byte("#!/usr/bin/env sh\necho husky-dispatcher\n"), 0o755); err != nil {
		t.Fatalf("restore dispatcher: %v", err)
	}

	// Non-husky shapes never redirect (no `_`+`h` dispatcher layout).
	for _, hooksDir := range []string{
		filepath.Join(tmp, ".git", "hooks"),
		filepath.Join(tmp, ".husky"),
	} {
		if got := huskyUserHooksDir(hooksDir); got != "" {
			t.Errorf("huskyUserHooksDir(%q) = %q, want \"\"", hooksDir, got)
		}
		if got := hookInstallDir(hooksDir); got != hooksDir {
			t.Errorf("hookInstallDir(%q) = %q, want %q", hooksDir, got, hooksDir)
		}
	}

	// Custom husky dir (e.g. husky frontend/.husky) still redirects when `_/h` exists.
	customOwned := filepath.Join(tmp, "frontend", ".husky", "_")
	if err := os.MkdirAll(customOwned, 0o755); err != nil {
		t.Fatalf("mkdir custom husky: %v", err)
	}
	if err := os.WriteFile(filepath.Join(customOwned, "h"), []byte("#!/usr/bin/env sh\necho husky-dispatcher\n"), 0o755); err != nil {
		t.Fatalf("write custom dispatcher: %v", err)
	}
	wantCustomParent := filepath.Join(tmp, "frontend", ".husky")
	if got := huskyUserHooksDir(customOwned); got != wantCustomParent {
		t.Errorf("huskyUserHooksDir(custom) = %q, want %q", got, wantCustomParent)
	}
}

func TestRemoveGitHook_RemovesInstalledHooks(t *testing.T) {
	tmpDir, _ := initHooksTestRepo(t)

	// Install hooks first
	installCount, _, err := InstallGitHook(context.Background(), true, false)
	if err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	}
	if installCount == 0 {
		t.Fatal("InstallGitHook() should install hooks")
	}

	// Verify hooks are installed
	if !IsGitHookInstalled(context.Background()) {
		t.Fatal("hooks should be installed before removal test")
	}

	// Remove hooks
	removeCount, err := RemoveGitHook(context.Background())
	if err != nil {
		t.Fatalf("RemoveGitHook(context.Background()) error = %v", err)
	}
	if removeCount != installCount {
		t.Errorf("RemoveGitHook(context.Background()) returned %d, want %d (same as installed)", removeCount, installCount)
	}

	// Verify hooks are removed
	if IsGitHookInstalled(context.Background()) {
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
	removeCount, err := RemoveGitHook(context.Background())
	if err != nil {
		t.Fatalf("RemoveGitHook(context.Background()) error = %v", err)
	}
	if removeCount != 0 {
		t.Errorf("RemoveGitHook(context.Background()) returned %d, want 0 (no hooks to remove)", removeCount)
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
	removeCount, err := RemoveGitHook(context.Background())
	if err != nil {
		t.Fatalf("RemoveGitHook(context.Background()) error = %v", err)
	}
	if removeCount != 0 {
		t.Errorf("RemoveGitHook(context.Background()) returned %d, want 0 (custom hook should not be removed)", removeCount)
	}

	// Verify custom hook still exists
	if _, err := os.Stat(customHookPath); os.IsNotExist(err) {
		t.Error("custom hook should still exist after RemoveGitHook(context.Background())")
	}
}

func TestRemoveGitHook_NotAGitRepo(t *testing.T) {
	// Create a temp directory without git init
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Clear cache so paths resolve correctly
	paths.ClearWorktreeRootCache()

	// Remove hooks in non-git directory - should return error
	_, err := RemoveGitHook(context.Background())
	if err == nil {
		t.Fatal("RemoveGitHook(context.Background()) should return error for non-git directory")
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

	count, _, err := InstallGitHook(context.Background(), true, false)
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

func TestManagedGitHookNames_IncludesPostRewrite(t *testing.T) {
	t.Parallel()

	names := ManagedGitHookNames()
	if !slices.Contains(names, "post-rewrite") {
		t.Fatalf("ManagedGitHookNames() = %v, want post-rewrite included", names)
	}
}

func TestInstallGitHook_InstallsPostRewrite(t *testing.T) {
	_, hooksDir := initHooksTestRepo(t)

	count, _, err := InstallGitHook(context.Background(), true, false)
	if err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	}
	if count == 0 {
		t.Fatal("InstallGitHook() should install hooks")
	}

	hookPath := filepath.Join(hooksDir, "post-rewrite")
	hookData, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("post-rewrite hook should exist: %v", err)
	}

	hookContent := string(hookData)
	if !strings.Contains(hookContent, entireHookMarker) {
		t.Error("installed post-rewrite hook should contain Entire marker")
	}
	if !strings.Contains(hookContent, `entire hooks git post-rewrite "$1" 2>/dev/null || true`) {
		t.Errorf("installed post-rewrite hook content missing expected command:\n%s", hookContent)
	}
}

func TestGitHookCommitMsg_MissingEntireWarnsAndAllowsCommit(t *testing.T) {
	t.Parallel()

	shPath := requireShell(t)
	tempDir := t.TempDir()
	msgFile := filepath.Join(tempDir, "COMMIT_EDITMSG")
	if err := os.WriteFile(msgFile, []byte("commit message\n"), 0o600); err != nil {
		t.Fatalf("failed to write commit message: %v", err)
	}

	hook := findHookSpec(t, buildHookSpecs("entire"), "commit-msg")
	hookPath := filepath.Join(tempDir, "commit-msg")
	if err := os.WriteFile(hookPath, []byte(hook.content), 0o755); err != nil {
		t.Fatalf("failed to write hook: %v", err)
	}

	cmd := exec.CommandContext(context.Background(), shPath, hookPath, msgFile)
	cmd.Env = envWithPath(t.TempDir())
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("commit-msg hook should allow commit when entire is missing: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), missingEntireGitHookWarning) {
		t.Fatalf("missing entire warning not printed, got:\n%s", output)
	}
}

func TestGitHookPrePush_MissingEntireSkipsSilentlyAndAllowsPush(t *testing.T) {
	t.Parallel()

	shPath := requireShell(t)
	tempDir := t.TempDir()

	hook := findHookSpec(t, buildHookSpecs("entire"), "pre-push")
	hookPath := filepath.Join(tempDir, "pre-push")
	if err := os.WriteFile(hookPath, []byte(hook.content), 0o755); err != nil {
		t.Fatalf("failed to write hook: %v", err)
	}

	cmd := exec.CommandContext(context.Background(), shPath, hookPath, "origin")
	cmd.Env = envWithPath(t.TempDir())
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pre-push hook should allow push when entire is missing: %v\n%s", err, output)
	}
	if strings.Contains(string(output), missingEntireGitHookWarning) {
		t.Fatalf("pre-push hook should skip missing entire silently, got:\n%s", output)
	}
}

func TestGitHookCommitMsg_EntireFailureAllowsCommit(t *testing.T) {
	t.Parallel()

	shPath := requireShell(t)
	tempDir := t.TempDir()
	binDir := t.TempDir()
	msgFile := filepath.Join(tempDir, "COMMIT_EDITMSG")
	if err := os.WriteFile(msgFile, []byte("commit message\n"), 0o600); err != nil {
		t.Fatalf("failed to write commit message: %v", err)
	}

	// The hook only needs an `entire` that exists and fails: it emits
	// `if command -v entire ...; then entire hooks git commit-msg "$1" || true;
	// else <warn>; fi`, so the name has to resolve (or the warning this test
	// forbids would print) and `|| true` discards whatever it exits with. The
	// script this replaces exited 42, but no code path read that value, so
	// linking `false` preserves the behaviour under test and — being a link,
	// not a written file — cannot hit the ETXTBSY race described above.
	linkExecutable(t, filepath.Join(binDir, "entire"), "false")

	hook := findHookSpec(t, buildHookSpecs("entire"), "commit-msg")
	hookPath := filepath.Join(tempDir, "commit-msg")
	if err := os.WriteFile(hookPath, []byte(hook.content), 0o755); err != nil {
		t.Fatalf("failed to write hook: %v", err)
	}

	cmd := exec.CommandContext(context.Background(), shPath, hookPath, msgFile)
	cmd.Env = envWithPath(binDir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("commit-msg hook should allow commit when entire handler fails: %v\n%s", err, output)
	}
	if strings.Contains(string(output), missingEntireGitHookWarning) {
		t.Fatalf("missing-entire warning should not print when entire exists, got:\n%s", output)
	}
}

func TestGitHookCommitMsg_MissingEntireStillRunsChainedHook(t *testing.T) {
	t.Parallel()

	shPath := requireShell(t)
	tempDir := t.TempDir()
	binDir := t.TempDir()
	msgFile := filepath.Join(tempDir, "COMMIT_EDITMSG")
	markerFile := msgFile + ".backup-ran"
	if err := os.WriteFile(msgFile, []byte("commit message\n"), 0o600); err != nil {
		t.Fatalf("failed to write commit message: %v", err)
	}
	// The generated hook calls dirname (hooks.go: _entire_hook_dir=...), and
	// envWithPath makes binDir the entire PATH, so binDir has to provide it.
	// The stand-in it replaces only reimplemented ${1%/*}, so linking the real
	// one is behaviour-identical.
	linkExecutable(t, filepath.Join(binDir, "dirname"), "dirname")

	hook := findHookSpec(t, buildHookSpecs("entire"), "commit-msg")
	hookPath := filepath.Join(tempDir, "commit-msg")
	content := generateChainedContent(hook.content, "commit-msg", false)
	if err := os.WriteFile(hookPath, []byte(content), 0o755); err != nil {
		t.Fatalf("failed to write hook: %v", err)
	}
	backupPath := hookPath + backupSuffix
	backupContent := "#!/bin/sh\nprintf 'backup ran\\n' > \"$1.backup-ran\"\n"
	if err := os.WriteFile(backupPath, []byte(backupContent), 0o755); err != nil {
		t.Fatalf("failed to write backup hook: %v", err)
	}

	cmd := exec.CommandContext(context.Background(), shPath, hookPath, msgFile)
	// Keep real `sh` on PATH: chained backups run via `sh -e` (husky semantics).
	// Fake dirname still wins so $_entire_hook_dir resolves inside tempDir.
	cmd.Env = envWithPath(binDir + string(os.PathListSeparator) + filepath.Dir(shPath))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("chained commit-msg hook should allow commit when entire is missing: %v\n%s", err, output)
	}
	if _, err := os.Stat(markerFile); err != nil {
		t.Fatalf("backup hook did not run: %v\n%s", err, output)
	}
}

// linkExecutable places an ETXTBSY-immune stand-in for the real `name` binary
// at dst, for tests that hand a fabricated PATH to a shell subprocess.
//
// Writing the stand-in as a script is the shape to avoid: a written-then-exec'd
// file is an ETXTBSY target (golang/go#22315). Between os.WriteFile's
// open-for-write and its close, any of this package's parallel tests can fork,
// the child inherits the write fd, and exec of that file then fails with "Text
// file busy" until the child closes it. Go's os/exec retries ETXTBSY for
// commands it starts, but these execs happen inside the sh subprocess, so
// nothing retries: the hook dies mid-script and the test reports whichever
// later assertion noticed, which points nowhere near the cause. A link is never
// a write target, so the race has no purchase.
//
// Symlink first, hard link second: Windows without Developer Mode or admin
// refuses symlinks, while a hard link is the same inode (equally immune) but
// cannot cross filesystems, which is the common case here since t.TempDir and
// the system binary usually differ. Skip if neither works — this is a test
// prerequisite, like requireShell's missing sh, not a failure of the code under
// test.
func linkExecutable(t *testing.T, dst, name string) {
	t.Helper()

	realPath, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not available on PATH", name)
	}
	if symlinkErr := os.Symlink(realPath, dst); symlinkErr == nil {
		return
	}
	if linkErr := os.Link(realPath, dst); linkErr != nil {
		t.Skipf("cannot link %s into the fake PATH: %v", name, linkErr)
	}
}

func requireShell(t *testing.T) string {
	t.Helper()

	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	return shPath
}

func findHookSpec(t *testing.T, specs []hookSpec, name string) hookSpec {
	t.Helper()

	for _, spec := range specs {
		if spec.name == name {
			return spec
		}
	}
	t.Fatalf("hook spec %q not found", name)
	return hookSpec{}
}

func envWithPath(path string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "PATH=") {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "PATH="+path)
}

func TestInstallGitHook_DoesNotOverwriteExistingBackup(t *testing.T) {
	_, hooksDir := initHooksTestRepo(t)

	// Create a backup file manually (simulating a previous backup)
	firstBackupContent := "#!/bin/sh\necho 'first custom hook'\n"
	backupPath := filepath.Join(hooksDir, "prepare-commit-msg"+backupSuffix)
	if err := os.WriteFile(backupPath, []byte(firstBackupContent), 0o755); err != nil {
		t.Fatalf("failed to create backup: %v", err)
	}

	// Create a second custom hook at the standard path (newer than the backup)
	secondCustomContent := "#!/bin/sh\necho 'second custom hook'\n"
	hookPath := filepath.Join(hooksDir, "prepare-commit-msg")
	if err := os.WriteFile(hookPath, []byte(secondCustomContent), 0o755); err != nil {
		t.Fatalf("failed to create second custom hook: %v", err)
	}

	_, _, err := InstallGitHook(context.Background(), true, false)
	if err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	}

	// Newer current hook must win: backup is rotated, then replaced with current.
	backupData, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("backup should still exist: %v", err)
	}
	if string(backupData) != secondCustomContent {
		t.Errorf("backup content = %q, want newer current %q", string(backupData), secondCustomContent)
	}
	staleData, err := os.ReadFile(backupPath + ".stale")
	if err != nil {
		t.Fatalf("stale backup should be preserved: %v", err)
	}
	if string(staleData) != firstBackupContent {
		t.Errorf("stale backup = %q, want original %q", string(staleData), firstBackupContent)
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

	firstCount, _, err := InstallGitHook(context.Background(), true, false)
	if err != nil {
		t.Fatalf("first InstallGitHook() error = %v", err)
	}
	if firstCount == 0 {
		t.Error("first install should install hooks")
	}

	// Re-install should return 0 (idempotent)
	secondCount, _, err := InstallGitHook(context.Background(), true, false)
	if err != nil {
		t.Fatalf("second InstallGitHook() error = %v", err)
	}
	if secondCount != 0 {
		t.Errorf("second InstallGitHook() = %d, want 0 (idempotent)", secondCount)
	}
}

func TestInstallGitHook_NoBackupWhenNoExistingHook(t *testing.T) {
	_, hooksDir := initHooksTestRepo(t)

	_, _, err := InstallGitHook(context.Background(), true, false)
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

	_, _, err := InstallGitHook(context.Background(), true, false)
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

	_, _, err := InstallGitHook(context.Background(), true, false)
	if err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	}

	removed, err := RemoveGitHook(context.Background())
	if err != nil {
		t.Fatalf("RemoveGitHook(context.Background()) error = %v", err)
	}
	if removed == 0 {
		t.Error("RemoveGitHook(context.Background()) should remove hooks")
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

	_, _, err := InstallGitHook(context.Background(), true, false)
	if err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	}

	// Simulate another tool deleting our hook
	if err := os.Remove(hookPath); err != nil {
		t.Fatalf("failed to remove hook: %v", err)
	}

	_, err = RemoveGitHook(context.Background())
	if err != nil {
		t.Fatalf("RemoveGitHook(context.Background()) error = %v", err)
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
	result := generateChainedContent(base, "pre-push", true)

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

	// Executable backups run directly (preserve shebang); non-exec fall back to sh -e.
	expectedExecCheck := `[ -x "$_entire_hook_dir/pre-push` + backupSuffix + `" ]`
	if !strings.Contains(result, expectedExecCheck) {
		t.Errorf("chained content should check -x on backup, got:\n%s", result)
	}
	expectedDirect := `"$_entire_hook_dir/pre-push` + backupSuffix + `" "$@" || exit $?`
	if !strings.Contains(result, expectedDirect) {
		t.Errorf("chained content should direct-exec executable backup, got:\n%s", result)
	}
	expectedFileCheck := `elif [ -f "$_entire_hook_dir/pre-push` + backupSuffix + `" ]`
	if !strings.Contains(result, expectedFileCheck) {
		t.Errorf("chained content should fall back to -f for non-exec backups, got:\n%s", result)
	}
	expectedSh := `sh -e "$_entire_hook_dir/pre-push` + backupSuffix + `" "$@" || exit $?`
	if !strings.Contains(result, expectedSh) {
		t.Errorf("chained content should sh -e non-exec backup with $@, got:\n%s", result)
	}
	if !strings.Contains(result, "_entire_status=$?") || !strings.Contains(result, "exit $_entire_status") {
		t.Errorf("chained content must preserve Entire exit status, got:\n%s", result)
	}
}

func TestGenerateChainedContent_PostRewritePreservesStdinForBackup(t *testing.T) {
	t.Parallel()

	base := "#!/bin/sh\n# Entire CLI hooks\n# Post-rewrite hook: remap session linkage after amend/rebase rewrites\nentire hooks git post-rewrite \"$1\" 2>/dev/null || true\n"
	result := generateChainedContent(base, "post-rewrite", true)

	if !strings.Contains(result, `_entire_stdin="$(mktemp "${TMPDIR:-/tmp}/entire-post-rewrite.XXXXXX")"`) {
		t.Fatalf("post-rewrite chained content should create temp stdin copy, got:\n%s", result)
	}
	if !strings.Contains(result, `cat > "$_entire_stdin"`) {
		t.Fatalf("post-rewrite chained content should capture stdin once, got:\n%s", result)
	}
	if !strings.Contains(result, `entire hooks git post-rewrite "$1" < "$_entire_stdin" 2>/dev/null || true`) {
		t.Fatalf("post-rewrite chained content should replay stdin into Entire handler, got:\n%s", result)
	}
	if !strings.Contains(result, `[ -x "$_entire_hook_dir/post-rewrite`+backupSuffix+`" ]`) {
		t.Fatalf("post-rewrite chained content should check -x on backup, got:\n%s", result)
	}
	if !strings.Contains(result, `"$_entire_hook_dir/post-rewrite`+backupSuffix+`" "$@" < "$_entire_stdin" || exit $?`) {
		t.Fatalf("post-rewrite chained content should direct-exec executable backup with stdin, got:\n%s", result)
	}
	if !strings.Contains(result, `sh -e "$_entire_hook_dir/post-rewrite`+backupSuffix+`" "$@" < "$_entire_stdin" || exit $?`) {
		t.Fatalf("post-rewrite chained content should sh -e replay stdin into non-exec backup, got:\n%s", result)
	}
	if !strings.Contains(result, "exit $_entire_status") {
		t.Fatalf("post-rewrite chained content must preserve Entire exit status, got:\n%s", result)
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
	count, _, err := InstallGitHook(context.Background(), true, false)
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
	_, err = RemoveGitHook(context.Background())
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
	count, _, err = InstallGitHook(context.Background(), true, false)
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
	_, _, err := InstallGitHook(context.Background(), true, false)
	if err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	}

	// User replaces our hook with their own hook B
	hookBContent := "#!/bin/sh\necho 'hook B'\n"
	if err := os.WriteFile(hookPath, []byte(hookBContent), 0o755); err != nil {
		t.Fatalf("failed to create hook B: %v", err)
	}

	// entire disable: should NOT overwrite hook B with backup A
	_, err = RemoveGitHook(context.Background())
	if err != nil {
		t.Fatalf("RemoveGitHook(context.Background()) error = %v", err)
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
	_, _, err := InstallGitHook(context.Background(), true, false)
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
	removed, err := RemoveGitHook(context.Background())
	if err == nil {
		t.Fatal("RemoveGitHook(context.Background()) should return error when hooks cannot be deleted")
	}
	if removed != 0 {
		t.Errorf("RemoveGitHook(context.Background()) removed %d hooks, expected 0 when all fail", removed)
	}
	if !strings.Contains(err.Error(), "failed to remove hooks") {
		t.Errorf("error should mention 'failed to remove hooks', got: %v", err)
	}
}

// TestResolveHookExePath covers the absolute-git-hook-path symlink resolution,
// including the Windows fallback for NTFS junctions that EvalSymlinks cannot
// resolve (e.g. Scoop's `…\current\` junction — issue #1424). GOOS and the
// symlink resolver are injected so every branch runs on any host.
func TestResolveHookExePath(t *testing.T) {
	t.Parallel()

	const exe = `C:\Users\admin\scoop\apps\cli\current\entire.exe`
	// Stand-in for the Windows junction error ("The system cannot find the path
	// specified") that filepath.EvalSymlinks returns on Scoop's `current\`.
	junctionErr := errors.New("cannot find the path specified")

	t.Run("resolves normally when EvalSymlinks succeeds", func(t *testing.T) {
		t.Parallel()
		got, err := resolveHookExePath("/tmp/linkto", func(string) (string, error) {
			return "/opt/entire/entire", nil
		}, "linux")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/opt/entire/entire" {
			t.Errorf("got %q, want resolved target", got)
		}
	})

	t.Run("windows falls back to unresolved path on EvalSymlinks failure", func(t *testing.T) {
		t.Parallel()
		got, err := resolveHookExePath(exe, func(string) (string, error) {
			return "", junctionErr
		}, goosWindows)
		if err != nil {
			t.Fatalf("windows should fall back, got error: %v", err)
		}
		if got != exe {
			t.Errorf("got %q, want unresolved exe %q", got, exe)
		}
	})

	t.Run("non-windows surfaces EvalSymlinks failure", func(t *testing.T) {
		t.Parallel()
		_, err := resolveHookExePath("/usr/local/bin/entire", func(string) (string, error) {
			return "", junctionErr
		}, "linux")
		if err == nil {
			t.Fatal("expected error on non-windows EvalSymlinks failure")
		}
		if !strings.Contains(err.Error(), "failed to resolve symlinks") {
			t.Errorf("error should mention symlink resolution, got: %v", err)
		}
	})
}

func TestGenerateChainedContent_PreservesEntireExitStatus(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	backupName := "pre-push" + backupSuffix
	backupPath := filepath.Join(tmp, backupName)
	// Successful backup must not mask Entire's failure (OPF abort).
	if err := os.WriteFile(backupPath, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatalf("write backup: %v", err)
	}

	base := "#!/bin/sh\n# Entire CLI hooks\nfalse\n"
	script := generateChainedContent(base, "pre-push", true)
	scriptPath := filepath.Join(tmp, "pre-push")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write chained hook: %v", err)
	}

	cmd := exec.CommandContext(context.Background(), "sh", scriptPath)
	cmd.Dir = tmp
	err := cmd.Run()
	if err == nil {
		t.Fatal("chained pre-push must exit non-zero when Entire fails even if backup succeeds")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() == 0 {
		t.Fatalf("want non-zero exit, got %v", err)
	}
}

func TestGenerateChainedContent_RunsMode0644Backup(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	backupName := "pre-push" + backupSuffix
	backupPath := filepath.Join(tmp, backupName)
	sentinel := filepath.Join(tmp, "ran")
	backupBody := "#!/bin/sh\ntouch \"" + sentinel + "\"\n"
	if err := os.WriteFile(backupPath, []byte(backupBody), 0o644); err != nil {
		t.Fatalf("write backup: %v", err)
	}

	base := chainedHookBaseTrue
	script := generateChainedContent(base, "pre-push", true)
	scriptPath := filepath.Join(tmp, "pre-push")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write chained hook: %v", err)
	}

	cmd := exec.CommandContext(context.Background(), "sh", scriptPath)
	cmd.Dir = tmp
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("chained hook failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("mode-0644 backup should have run via sh -e: %v", err)
	}
}

func TestGenerateChainedContent_NonHuskySkipsMode0644Backup(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	backupName := "pre-push" + backupSuffix
	backupPath := filepath.Join(tmp, backupName)
	sentinel := filepath.Join(tmp, "ran")
	backupBody := "#!/bin/sh\ntouch \"" + sentinel + "\"\n"
	if err := os.WriteFile(backupPath, []byte(backupBody), 0o644); err != nil {
		t.Fatalf("write backup: %v", err)
	}

	base := chainedHookBaseTrue
	script := generateChainedContent(base, "pre-push", false)
	if strings.Contains(script, "sh -e") {
		t.Fatalf("non-husky chain must not use sh -e fallback, got:\n%s", script)
	}
	scriptPath := filepath.Join(tmp, "pre-push")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write chained hook: %v", err)
	}

	cmd := exec.CommandContext(context.Background(), "sh", scriptPath)
	cmd.Dir = tmp
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("chained hook failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("non-executable backup must stay inert outside husky")
	}
}

func TestGenerateChainedContent_RunsExecutableShebangBackup(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	backupName := "pre-push" + backupSuffix
	backupPath := filepath.Join(tmp, backupName)
	sentinel := filepath.Join(tmp, "ran")
	// Non-shell shebang: must be direct-exec'd, not forced through `sh -e`.
	backupBody := "#!/usr/bin/env python3\nopen(r'" + sentinel + "', 'w').close()\n"
	if err := os.WriteFile(backupPath, []byte(backupBody), 0o755); err != nil {
		t.Fatalf("write backup: %v", err)
	}

	base := chainedHookBaseTrue
	script := generateChainedContent(base, "pre-push", false)
	scriptPath := filepath.Join(tmp, "pre-push")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write chained hook: %v", err)
	}

	cmd := exec.CommandContext(context.Background(), "sh", scriptPath)
	cmd.Dir = tmp
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("chained hook failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("executable shebang backup should have run via direct exec: %v", err)
	}
}

func TestMigrateEntireHooksFromHuskyOwnedDir_ReadErrorIsLoud(t *testing.T) {
	t.Parallel()

	owned := t.TempDir()
	// Make prepare-commit-msg a directory so ReadFile fails with a non-ErrNotExist error.
	blocked := filepath.Join(owned, "prepare-commit-msg")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatalf("mkdir blocked hook path: %v", err)
	}
	err := migrateEntireHooksFromHuskyOwnedDir(owned)
	if err == nil {
		t.Fatal("expected loud error when husky-owned hook path is unreadable")
	}
	if !strings.Contains(err.Error(), blocked) {
		t.Errorf("error should include path %q, got: %v", blocked, err)
	}
}

func TestHuskyForwardingStubsPresent_RequiresDispatchSource(t *testing.T) {
	tmpDir, _ := initHooksTestRepo(t)
	ctx := context.Background()

	cmd := exec.CommandContext(ctx, "git", "config", "core.hooksPath", ".husky/_")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to set core.hooksPath: %v", err)
	}
	huskyOwnedDir := filepath.Join(tmpDir, ".husky", "_")
	seedHuskyOwnedDir(t, huskyOwnedDir)

	if _, _, err := InstallGitHook(context.Background(), true, false); err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	}
	if !IsGitHookInstalledInDir(context.Background(), tmpDir) {
		t.Fatal("hooks should be installed")
	}

	// Replace one stub with a non-Entire file that does not source `_/h`.
	badStub := filepath.Join(huskyOwnedDir, "pre-push")
	if err := os.WriteFile(badStub, []byte("#!/bin/sh\necho noop\n"), 0o755); err != nil {
		t.Fatalf("write bad stub: %v", err)
	}
	if IsGitHookInstalledInDir(context.Background(), tmpDir) {
		t.Fatal("missing husky dispatch source should report hooks not installed so EnsureSetup can heal")
	}

	// Reinstall must heal the non-forwarding stub so IsGitHookInstalled recovers.
	if _, _, err := InstallGitHook(context.Background(), true, false); err != nil {
		t.Fatalf("reinstall to heal stub: %v", err)
	}
	if !IsGitHookInstalledInDir(context.Background(), tmpDir) {
		t.Fatal("reinstall should replace non-forwarding husky stubs and report installed")
	}
	healed, err := os.ReadFile(badStub)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(healed), huskyStubDispatchMarker) {
		t.Fatalf("healed stub should source _/h, got:\n%s", healed)
	}
}

func TestRemoveGitHook_DoesNotBackfillMissingHuskyStubs(t *testing.T) {
	tmpDir, _ := initHooksTestRepo(t)
	ctx := context.Background()

	cmd := exec.CommandContext(ctx, "git", "config", "core.hooksPath", ".husky/_")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to set core.hooksPath: %v", err)
	}
	huskyOwnedDir := filepath.Join(tmpDir, ".husky", "_")
	seedHuskyOwnedDir(t, huskyOwnedDir)

	if _, _, err := InstallGitHook(context.Background(), true, false); err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	}

	missing := filepath.Join(huskyOwnedDir, "pre-push")
	if err := os.Remove(missing); err != nil {
		t.Fatalf("remove stub: %v", err)
	}

	removed, err := RemoveGitHook(context.Background())
	if err != nil {
		t.Fatalf("RemoveGitHook() error = %v", err)
	}
	if removed == 0 {
		t.Fatal("RemoveGitHook() should count removed Entire user hooks")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("disable must not backfill missing husky stubs under `_`")
	}
}

func TestInstallGitHook_HealsMissingHuskyStub(t *testing.T) {
	tmpDir, _ := initHooksTestRepo(t)
	ctx := context.Background()

	cmd := exec.CommandContext(ctx, "git", "config", "core.hooksPath", ".husky/_")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to set core.hooksPath: %v", err)
	}
	huskyOwnedDir := filepath.Join(tmpDir, ".husky", "_")
	seedHuskyOwnedDir(t, huskyOwnedDir)

	if _, _, err := InstallGitHook(context.Background(), true, false); err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	}
	missing := filepath.Join(huskyOwnedDir, "pre-push")
	if err := os.Remove(missing); err != nil {
		t.Fatalf("remove stub: %v", err)
	}
	if IsGitHookInstalledInDir(context.Background(), tmpDir) {
		t.Fatal("missing stub should make IsGitHookInstalledInDir false")
	}
	if _, _, err := InstallGitHook(context.Background(), true, false); err != nil {
		t.Fatalf("reinstall error: %v", err)
	}
	data, err := os.ReadFile(missing)
	if err != nil {
		t.Fatalf("stub should be healed: %v", err)
	}
	if string(data) != huskyForwardingStub {
		t.Errorf("healed stub = %q, want huskyForwardingStub", data)
	}
	if !IsGitHookInstalledInDir(context.Background(), tmpDir) {
		t.Fatal("hooks should be installed after stub heal")
	}
}

func TestRemoveGitHook_DispatcherDeletedScrubsParentUserHooks(t *testing.T) {
	tmpDir, _ := initHooksTestRepo(t)
	ctx := context.Background()

	cmd := exec.CommandContext(ctx, "git", "config", "core.hooksPath", ".husky/_")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to set core.hooksPath: %v", err)
	}
	huskyOwnedDir := filepath.Join(tmpDir, ".husky", "_")
	seedHuskyOwnedDir(t, huskyOwnedDir)

	if _, _, err := InstallGitHook(context.Background(), true, false); err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	}
	userHook := filepath.Join(tmpDir, ".husky", "prepare-commit-msg")
	if data, err := os.ReadFile(userHook); err != nil || !strings.Contains(string(data), entireHookMarker) {
		t.Fatalf("expected Entire user hook before dispatcher delete: %v", err)
	}

	// Delete dispatcher so hookInstallDir falls back to `_` and removal
	// scrubs orphan Entire wrappers from the parent user-hook directory.
	if err := os.Remove(filepath.Join(huskyOwnedDir, "h")); err != nil {
		t.Fatalf("remove dispatcher: %v", err)
	}

	removed, err := RemoveGitHook(context.Background())
	if err != nil {
		t.Fatalf("RemoveGitHook() error = %v", err)
	}
	if removed == 0 {
		t.Fatal("should scrub orphan Entire wrappers from parent .husky/")
	}
	if data, err := os.ReadFile(userHook); err == nil && strings.Contains(string(data), entireHookMarker) {
		t.Fatal("orphan Entire user hook must be removed when dispatcher is gone")
	}
}

func TestMigrateEntireHooksFromHuskyOwnedDir_MixedLegacyState(t *testing.T) {
	t.Parallel()

	owned := t.TempDir()
	withBackup := "prepare-commit-msg"
	withoutBackup := "commit-msg"
	alreadyStub := "post-commit"
	missing := "pre-push"

	legacy := "#!/bin/sh\n# " + entireHookMarker + "\nentire hooks git x\n"
	if err := os.WriteFile(filepath.Join(owned, withBackup), []byte(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owned, withBackup+backupSuffix), []byte(huskyForwardingStub), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owned, withoutBackup), []byte(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owned, alreadyStub), []byte(huskyForwardingStub), 0o755); err != nil {
		t.Fatal(err)
	}
	// post-rewrite left as another missing hook besides pre-push
	_ = missing

	if err := migrateEntireHooksFromHuskyOwnedDir(owned); err != nil {
		t.Fatalf("migrate mixed state: %v", err)
	}

	restored, err := os.ReadFile(filepath.Join(owned, withBackup))
	if err != nil || string(restored) != huskyForwardingStub {
		t.Fatalf("with-backup should restore stub, got %q err=%v", restored, err)
	}
	replaced, err := os.ReadFile(filepath.Join(owned, withoutBackup))
	if err != nil || string(replaced) != huskyForwardingStub {
		t.Fatalf("without-backup should become stub, got %q err=%v", replaced, err)
	}
	unchanged, err := os.ReadFile(filepath.Join(owned, alreadyStub))
	if err != nil || string(unchanged) != huskyForwardingStub {
		t.Fatalf("already-stub should remain, got %q err=%v", unchanged, err)
	}
	for _, hook := range []string{missing, "post-rewrite"} {
		healed, err := os.ReadFile(filepath.Join(owned, hook))
		if err != nil || string(healed) != huskyForwardingStub {
			t.Fatalf("missing %s should be backfilled, got %q err=%v", hook, healed, err)
		}
	}
}

func TestInstallGitHook_ChainsPreExistingHuskyUserHook(t *testing.T) {
	tmpDir, _ := initHooksTestRepo(t)
	ctx := context.Background()

	cmd := exec.CommandContext(ctx, "git", "config", "core.hooksPath", ".husky/_")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to set core.hooksPath: %v", err)
	}
	huskyOwnedDir := filepath.Join(tmpDir, ".husky", "_")
	seedHuskyOwnedDir(t, huskyOwnedDir)

	userHook := filepath.Join(tmpDir, ".husky", "prepare-commit-msg")
	custom := "#!/bin/sh\necho 'preexisting user hook'\n"
	if err := os.MkdirAll(filepath.Dir(userHook), 0o755); err != nil {
		t.Fatalf("mkdir .husky: %v", err)
	}
	if err := os.WriteFile(userHook, []byte(custom), 0o644); err != nil {
		t.Fatalf("write preexisting user hook: %v", err)
	}

	if _, huskySafe, err := InstallGitHook(context.Background(), true, false); err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	} else if !huskySafe {
		t.Fatal("expected husky-safe install")
	}

	backupPath := userHook + backupSuffix
	if _, err := os.Stat(backupPath); !os.IsNotExist(err) {
		t.Fatalf("husky-safe inject must not leave a .pre-entire companion, stat err=%v", err)
	}
	installed, err := os.ReadFile(userHook)
	if err != nil {
		t.Fatalf("read installed user hook: %v", err)
	}
	content := string(installed)
	if !strings.Contains(content, entireHookMarker) {
		t.Error("installed hook should contain Entire marker")
	}
	if !strings.Contains(content, entireManagedBegin) || !strings.Contains(content, entireManagedEnd) {
		t.Errorf("installed hook should wrap Entire in managed markers, got:\n%s", content)
	}
	if !strings.Contains(content, "preexisting user hook") {
		t.Errorf("installed hook must keep original user logic for clones, got:\n%s", content)
	}
	if strings.Contains(content, `sh -e`) {
		t.Errorf("injected husky hook should not chain via sh -e backup, got:\n%s", content)
	}

	removed, err := RemoveGitHook(context.Background())
	if err != nil {
		t.Fatalf("RemoveGitHook() error = %v", err)
	}
	if removed == 0 {
		t.Fatal("RemoveGitHook() should remove Entire wrappers")
	}
	restored, err := os.ReadFile(userHook)
	if err != nil {
		t.Fatalf("expected restored user hook: %v", err)
	}
	if string(restored) != custom {
		t.Errorf("restored hook = %q, want %q", restored, custom)
	}

	excludePath := filepath.Join(tmpDir, ".git", "info", "exclude")
	if data, err := os.ReadFile(excludePath); err == nil {
		if strings.Contains(string(data), preEntireExcludePattern) {
			t.Errorf("exclude should be cleared when no backups remain, got %q", data)
		}
	}
}

func TestEnsurePreEntireExcluded_LinkedWorktreeUsesCommonExclude(t *testing.T) {
	mainRepo, worktreeDir := initHooksWorktreeRepo(t)
	t.Chdir(worktreeDir)
	paths.ClearWorktreeRootCache()

	// Create a backup path that check-ignore will evaluate from the worktree.
	huskyParent := filepath.Join(mainRepo, ".husky")
	if err := os.MkdirAll(huskyParent, 0o755); err != nil {
		t.Fatal(err)
	}
	backupRel := filepath.ToSlash(filepath.Join(".husky", "prepare-commit-msg"+backupSuffix))
	backupAbs := filepath.Join(mainRepo, ".husky", "prepare-commit-msg"+backupSuffix)
	if err := os.WriteFile(backupAbs, []byte("#!/bin/sh\necho backup\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ensurePreEntireExcluded(context.Background()); err != nil {
		t.Fatalf("ensurePreEntireExcluded from linked worktree: %v", err)
	}

	commonExclude := filepath.Join(mainRepo, ".git", "info", "exclude")
	data, err := os.ReadFile(commonExclude)
	if err != nil {
		t.Fatalf("common-dir exclude missing: %v", err)
	}
	if !strings.Contains(string(data), preEntireExcludePattern) {
		t.Fatalf("common exclude should contain %q, got %q", preEntireExcludePattern, data)
	}

	resolved, err := gitInfoExcludePath(context.Background())
	if err != nil {
		t.Fatalf("gitInfoExcludePath: %v", err)
	}
	commonResolved, err := filepath.EvalSymlinks(commonExclude)
	if err != nil {
		commonResolved = filepath.Clean(commonExclude)
	}
	resolvedEval, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		resolvedEval = filepath.Clean(resolved)
	}
	if resolvedEval != commonResolved {
		t.Fatalf("gitInfoExcludePath from worktree = %q, want common exclude %q", resolvedEval, commonResolved)
	}

	check := exec.CommandContext(context.Background(), "git", "check-ignore", "-q", backupRel)
	check.Dir = worktreeDir
	if err := check.Run(); err != nil {
		t.Fatalf("git check-ignore from linked worktree should match %s: %v", backupRel, err)
	}
}

func TestPreserveCurrentHookOverStaleBackup_KeepsHookUntilOverwrite(t *testing.T) {
	dir := t.TempDir()
	hookPath := filepath.Join(dir, "prepare-commit-msg")
	backupPath := hookPath + backupSuffix
	oldBackup := "#!/bin/sh\necho old-backup\n"
	current := "#!/bin/sh\necho current-hook\n"
	if err := os.WriteFile(backupPath, []byte(oldBackup), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hookPath, []byte(current), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := preserveCurrentHookOverStaleBackup(hookPath, backupPath, []byte(current)); err != nil {
		t.Fatalf("preserveCurrentHookOverStaleBackup: %v", err)
	}

	// Live hook must still exist so a later install failure cannot erase it.
	gotHook, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("hookPath should remain until overwrite: %v", err)
	}
	if string(gotHook) != current {
		t.Errorf("hookPath = %q, want %q", gotHook, current)
	}
	gotBackup, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if string(gotBackup) != current {
		t.Errorf("backup = %q, want %q", gotBackup, current)
	}
	gotStale, err := os.ReadFile(backupPath + ".stale")
	if err != nil {
		t.Fatalf("stale: %v", err)
	}
	if string(gotStale) != oldBackup {
		t.Errorf("stale = %q, want %q", gotStale, oldBackup)
	}
}

func TestStripDelimitedBlock_IgnoresEndMarkerInsideManagedBody(t *testing.T) {
	body := "#!/bin/sh\n" +
		entireManagedBegin + "\n" +
		"echo 'mention " + entireManagedEnd + " in a comment'\n" +
		"entire-real\n" +
		entireManagedEnd + "\n" +
		"echo user-hook\n"
	got := stripEntireManagedBlock(body)
	if strings.Contains(got, "entire-real") {
		t.Fatalf("managed body leaked into stripped output:\n%s", got)
	}
	if !strings.Contains(got, "echo user-hook") {
		t.Fatalf("user hook lost:\n%s", got)
	}
	if strings.Contains(got, entireManagedBegin) || strings.Contains(got, "entire-real") {
		t.Fatalf("managed markers/body remain:\n%s", got)
	}
	// Mid-body mention must not truncate: user line stays, fake end text only in stripped-away region.
	wantPrefix := "#!/bin/sh\necho user-hook\n"
	if got != wantPrefix {
		t.Fatalf("got:\n%q\nwant:\n%q", got, wantPrefix)
	}
}

func TestStripEntireManagedBlock_RemovesPrePushExitWrapper(t *testing.T) {
	user := "#!/bin/sh\necho lint\n"
	injected := injectEntireManagedBlock("pre-push", user, "# entire body\n")
	got := stripEntireManagedBlock(injected)
	if got != user {
		t.Fatalf("round-trip pre-push managed inject:\ngot:\n%q\nwant:\n%q", got, user)
	}
	if strings.Contains(got, "_entire_status") || strings.Contains(got, "exit $_entire_status") {
		t.Fatalf("exit wrapper leaked into restored hook:\n%s", got)
	}
}

func TestStripDelimitedBlock_IgnoresEndMarkerEmbeddedMidLine(t *testing.T) {
	content := "before\n" + entireManagedBegin + "\nmid " + entireManagedEnd + " mid\nkeep\n" + entireManagedEnd + "\nafter\n"
	got := stripDelimitedBlock(content, entireManagedBegin, entireManagedEnd)
	if got != "before\nafter\n" {
		t.Fatalf("got %q", got)
	}
}

func TestHasActiveHuskyStubDispatch_RequiresExactLine(t *testing.T) {
	t.Parallel()
	if !hasActiveHuskyStubDispatch(huskyForwardingStub) {
		t.Fatal("canonical stub must pass")
	}
	for _, bad := range []string{
		`echo ` + huskyStubDispatchMarker + "\n",
		`false && ` + huskyStubDispatchMarker + "\n",
		"# " + huskyStubDispatchMarker + "\n",
	} {
		if hasActiveHuskyStubDispatch(bad) {
			t.Errorf("should reject non-exact dispatch:\n%s", bad)
		}
	}
}

func TestEntireHookUsesForeignAbsoluteLauncher(t *testing.T) {
	t.Parallel()
	content := `#!/bin/sh
# Entire CLI hooks
if [ -x "/other/clone/entire" ]; then /other/clone/entire hooks git prepare-commit-msg "$1" "$2" 2>/dev/null || true; else :; fi
`
	if !entireHookUsesForeignAbsoluteLauncher(content, bareEntireHookCmd) {
		t.Fatal("foreign absolute should be outdated vs bare entire")
	}
	if !entireHookUsesForeignAbsoluteLauncher(content, `"/local/entire"`) && !entireHookUsesForeignAbsoluteLauncher(content, "/local/entire") {
		t.Fatal("foreign absolute should be outdated vs different expected absolute")
	}
	local := `#!/bin/sh
# Entire CLI hooks
if [ -x "/local/entire" ]; then /local/entire hooks git prepare-commit-msg "$1" "$2" 2>/dev/null || true; else :; fi
`
	if entireHookUsesForeignAbsoluteLauncher(local, "/local/entire") {
		t.Fatal("matching absolute should not be foreign")
	}
}

func TestInjectPostRewriteManagedBlock_ReplaysStdin(t *testing.T) {
	t.Parallel()
	entire := `#!/bin/sh
# Entire CLI hooks
if command -v entire >/dev/null 2>&1; then entire hooks git post-rewrite "$1" 2>/dev/null || true; else :; fi
`
	user := "#!/bin/sh\ncat > /tmp/should-see-mapping\n"
	got := injectEntireManagedBlock("post-rewrite", user, entire)
	if !strings.Contains(got, `_entire_stdin=`) {
		t.Fatalf("missing stdin capture:\n%s", got)
	}
	if !strings.Contains(got, `hooks git post-rewrite "$1" < "$_entire_stdin"`) {
		t.Fatalf("Entire must read replayed stdin:\n%s", got)
	}
	if !strings.Contains(got, `) < "$_entire_stdin"`) {
		t.Fatalf("user body must receive replayed stdin:\n%s", got)
	}
}

func TestInstallGitHook_RejectsSymlinkedHook(t *testing.T) {
	_, hooksDir := initHooksTestRepo(t)
	hookPath := filepath.Join(hooksDir, "prepare-commit-msg")
	target := filepath.Join(hooksDir, "target-config")
	if err := os.WriteFile(target, []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, hookPath); err != nil {
		t.Fatal(err)
	}
	_, _, err := InstallGitHook(context.Background(), true, false)
	if err == nil {
		t.Fatal("expected symlink rejection")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error should mention symlink, got: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "secret\n" {
		t.Fatalf("symlink target corrupted: %q", got)
	}
}

func TestRewriteHuskyOwnedHooks_RotatesStaleBackupBeforeHeal(t *testing.T) {
	tmpDir, _ := initHooksTestRepo(t)
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "git", "config", "core.hooksPath", ".husky/_")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	owned := filepath.Join(tmpDir, ".husky", "_")
	seedHuskyOwnedDir(t, owned)

	hookPath := filepath.Join(owned, "prepare-commit-msg")
	backupPath := hookPath + backupSuffix
	oldBackup := "#!/bin/sh\necho old-stub-backup\n"
	current := "#!/bin/sh\necho newer-unknown-stub\n"
	if err := os.WriteFile(backupPath, []byte(oldBackup), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hookPath, []byte(current), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := rewriteHuskyOwnedHooks(owned, true); err != nil {
		t.Fatalf("rewriteHuskyOwnedHooks: %v", err)
	}
	gotBackup, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotBackup) != current {
		t.Fatalf("backup = %q, want current %q", gotBackup, current)
	}
	gotStale, err := os.ReadFile(backupPath + ".stale")
	if err != nil {
		t.Fatal(err)
	}
	if string(gotStale) != oldBackup {
		t.Fatalf("stale = %q, want %q", gotStale, oldBackup)
	}
	gotHook, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal(err)
	}
	if !hasActiveHuskyStubDispatch(string(gotHook)) {
		t.Fatalf("healed stub should forward, got:\n%s", gotHook)
	}
}

func TestPreserveCurrentHookOverStaleBackup_PreservesMode0644(t *testing.T) {
	dir := t.TempDir()
	hookPath := filepath.Join(dir, "prepare-commit-msg")
	backupPath := hookPath + backupSuffix
	oldBackup := "#!/bin/sh\necho old-backup\n"
	current := "#!/bin/sh\necho current-0644\n"
	if err := os.WriteFile(backupPath, []byte(oldBackup), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hookPath, []byte(current), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := preserveCurrentHookOverStaleBackup(hookPath, backupPath, []byte(current)); err != nil {
		t.Fatalf("preserveCurrentHookOverStaleBackup: %v", err)
	}

	info, err := os.Lstat(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 != 0 {
		t.Fatalf("rotated backup mode = %o, want non-executable (0644)", info.Mode().Perm())
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("rotated backup mode = %o, want 0644", info.Mode().Perm())
	}
}
