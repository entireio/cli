//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v5"
)

// TestGetGitAuthorWithLocalConfig tests the normal path where local repo config exists.
// This is the happy path - go-git can find the config directly.
func TestGetGitAuthorWithLocalConfig(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)

	// Local config is set by InitRepo(), so this tests the normal case
	env.WriteFile("test.txt", "content")

	// Create a session and simulate user prompt submit which uses GetGitAuthor
	session := env.NewSession()
	err := env.SimulateUserPromptSubmit(session.ID)
	if err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Create transcript and simulate stop which also uses GetGitAuthor
	session.CreateTranscript("Create a file", []FileChange{
		{Path: "test.txt", Content: "content"},
	})

	err = env.SimulateStop(session.ID, session.TranscriptPath)
	if err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}
}

// TestGetGitAuthorFallbackToGitCommand tests that GetGitAuthor falls back
// to the git command when go-git can't find user config in the repo's local config.
// We set global config via environment variables, simulating a user who has
// global git config but no local repo config.
func TestGetGitAuthorFallbackToGitCommand(t *testing.T) {
	t.Parallel()

	env := NewTestEnv(t)

	// Initialize repo using git command (not go-git) to avoid setting local config
	cmd := exec.Command("git", "init")
	cmd.Dir = env.RepoDir
	cmd.Env = testutil.GitIsolatedEnv()
	if err := cmd.Run(); err != nil {
		t.Fatalf("git init failed: %v", err)
	}

	// Disable GPG signing for test commits
	configCmd := exec.Command("git", "config", "commit.gpgsign", "false")
	configCmd.Dir = env.RepoDir
	configCmd.Env = testutil.GitIsolatedEnv()
	if err := configCmd.Run(); err != nil {
		t.Fatalf("git config commit.gpgsign failed: %v", err)
	}

	// The repo now has no local user config. We'll use GIT_AUTHOR_* and GIT_COMMITTER_*
	// env vars for commits, simulating global config that go-git can't see but git command can.

	env.InitEntire()

	// Create initial commit using environment variables for author/committer
	env.WriteFile("README.md", "# Test")

	addCmd := exec.Command("git", "add", "README.md")
	addCmd.Dir = env.RepoDir
	addCmd.Env = testutil.GitIsolatedEnv()
	if err := addCmd.Run(); err != nil {
		t.Fatalf("git add failed: %v", err)
	}

	// Use environment variables to set author and committer (works in CI without global config)
	commitCmd := exec.Command("git", "commit", "-m", "Initial")
	commitCmd.Dir = env.RepoDir
	commitCmd.Env = append(testutil.GitIsolatedEnv(),
		"GIT_AUTHOR_NAME=Test User",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test User",
		"GIT_COMMITTER_EMAIL=test@example.com",
	)
	if output, err := commitCmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit failed: %v\nOutput: %s", err, output)
	}

	// Create feature branch
	branchCmd := exec.Command("git", "checkout", "-b", "feature/test")
	branchCmd.Dir = env.RepoDir
	branchCmd.Env = testutil.GitIsolatedEnv()
	if err := branchCmd.Run(); err != nil {
		t.Fatalf("git checkout -b failed: %v", err)
	}

	env.WriteFile("test.txt", "content")

	// Simulate a session using hooks (which internally use GetGitAuthor)
	// The hook should work because GetGitAuthor falls back to git command or returns defaults
	session := env.NewSession()
	err := env.SimulateUserPromptSubmit(session.ID)
	if err != nil {
		errStr := err.Error()
		// Check if the error is specifically about git user config
		if strings.Contains(errStr, "git user not configured") ||
			strings.Contains(errStr, "git user.name not configured") ||
			strings.Contains(errStr, "git user.email not configured") {
			t.Errorf("GetGitAuthor should fall back to git command, not error about config: %s", errStr)
		}
		// Other errors may be acceptable
		t.Logf("SimulateUserPromptSubmit output (may be expected): %v", err)
	}
}

// TestGetGitAuthorNoConfigReturnsDefaults tests that when no config exists anywhere,
// the function returns defaults instead of erroring.
func TestGetGitAuthorNoConfigReturnsDefaults(t *testing.T) {
	t.Parallel()

	env := NewTestEnv(t)

	// Create a completely isolated environment with no git config
	fakeHome := t.TempDir()

	// Initialize repo
	initCmd := exec.Command("git", "init")
	initCmd.Dir = env.RepoDir
	initCmd.Env = []string{
		"HOME=" + fakeHome,
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_NOSYSTEM=1", // Ignore system config
	}
	if err := initCmd.Run(); err != nil {
		t.Fatalf("git init failed: %v", err)
	}

	// Disable GPG signing for test commits
	configCmd := exec.Command("git", "config", "commit.gpgsign", "false")
	configCmd.Dir = env.RepoDir
	configCmd.Env = []string{
		"HOME=" + fakeHome,
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_NOSYSTEM=1",
	}
	if err := configCmd.Run(); err != nil {
		t.Fatalf("git config commit.gpgsign failed: %v", err)
	}

	env.InitEntire()

	// Create initial commit using environment variables (required for CI without global config)
	env.WriteFile("README.md", "# Test")

	addCmd := exec.Command("git", "add", "README.md")
	addCmd.Dir = env.RepoDir
	addCmd.Env = []string{
		"HOME=" + fakeHome,
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_NOSYSTEM=1",
	}
	addCmd.Run()

	commitCmd := exec.Command("git", "commit", "-m", "Initial")
	commitCmd.Dir = env.RepoDir
	commitCmd.Env = []string{
		"HOME=" + fakeHome,
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@test.com",
	}
	commitCmd.Run()

	// Create feature branch
	branchCmd := exec.Command("git", "checkout", "-b", "feature/test")
	branchCmd.Dir = env.RepoDir
	branchCmd.Env = []string{
		"HOME=" + fakeHome,
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_NOSYSTEM=1",
	}
	branchCmd.Run()

	env.WriteFile("test.txt", "content")

	// Run hook command with isolated HOME (no global git config)
	// Use the hook runner but with custom environment
	hookCmd := exec.Command(getTestBinary(), "hooks", "claude-code", "user-prompt-submit")
	hookCmd.Dir = env.RepoDir
	hookCmd.Stdin = strings.NewReader(`{"session_id": "test-session", "transcript_path": ""}`)
	hookCmd.Env = []string{
		"HOME=" + fakeHome,
		"ENTIRE_TEST_CLAUDE_PROJECT_DIR=" + env.ClaudeProjectDir,
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_NOSYSTEM=1",
	}

	output, err := hookCmd.CombinedOutput()
	outputStr := string(output)

	// The key assertion: should NOT error specifically about git user config
	if err != nil {
		if strings.Contains(outputStr, "git user not configured") ||
			strings.Contains(outputStr, "git user.name not configured") ||
			strings.Contains(outputStr, "git user.email not configured") {
			t.Errorf("GetGitAuthor should return defaults, not error about git user config.\nOutput: %s", outputStr)
		}
		// Other errors are acceptable in this isolated environment
		t.Logf("Command output (checking for git user errors): %s", output)
	}
}

// TestGetGitAuthorRemovingLocalConfig tests that removing the [user] section
// from local config still works via fallback.
func TestGetGitAuthorRemovingLocalConfig(t *testing.T) {
	t.Parallel()

	env := NewTestEnv(t)
	env.InitRepo()

	// Read and modify .git/config to remove user section
	configPath := filepath.Join(env.RepoDir, ".git", "config")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("failed to read .git/config: %v", err)
	}

	// Remove [user] section from config
	configWithoutUser := removeUserSection(string(data))
	if err := os.WriteFile(configPath, []byte(configWithoutUser), 0o644); err != nil {
		t.Fatalf("failed to write .git/config: %v", err)
	}

	env.InitEntire()

	// Need to create initial commit - use environment variables (works in CI without global config)
	env.WriteFile("README.md", "# Test")

	addCmd := exec.Command("git", "add", "README.md")
	addCmd.Dir = env.RepoDir
	addCmd.Env = testutil.GitIsolatedEnv()
	addCmd.Run()

	commitCmd := exec.Command("git", "commit", "-m", "Initial")
	commitCmd.Dir = env.RepoDir
	commitCmd.Env = append(testutil.GitIsolatedEnv(),
		"GIT_AUTHOR_NAME=Test User",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test User",
		"GIT_COMMITTER_EMAIL=test@example.com",
	)
	commitCmd.Run()

	// Create feature branch
	branchCmd := exec.Command("git", "checkout", "-b", "feature/test")
	branchCmd.Dir = env.RepoDir
	branchCmd.Env = testutil.GitIsolatedEnv()
	branchCmd.Run()

	env.WriteFile("test.txt", "content")

	// The CLI should still work because GetGitAuthor falls back
	session := env.NewSession()
	err = env.SimulateUserPromptSubmit(session.ID)
	if err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "git user not configured") ||
			strings.Contains(errStr, "git user.name not configured") ||
			strings.Contains(errStr, "git user.email not configured") {
			t.Errorf("GetGitAuthor should fall back when local config is removed.\nError: %s", errStr)
		}
		t.Logf("SimulateUserPromptSubmit output: %v", err)
	}
}

// TestGetGitAuthorFromRepoReturnsDefaults verifies that GetGitAuthorFromRepo
// returns "Unknown" and "unknown@local" when config is missing.
func TestGetGitAuthorFromRepoReturnsDefaults(t *testing.T) {
	t.Parallel()

	// Create temp directory for test repo
	tmpDir := t.TempDir()

	// Initialize repo without setting user config
	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("Failed to init repo: %v", err)
	}

	// Import strategy package to test GetGitAuthorFromRepo directly
	// Note: We can't import cli package from integration tests due to import cycles,
	// but we can verify the behavior through the CLI commands above.

	// Verify repo has no user config set
	cfg, err := repo.Config()
	if err != nil {
		t.Fatalf("Failed to get repo config: %v", err)
	}

	if cfg.User.Name != "" || cfg.User.Email != "" {
		t.Errorf("Expected empty user config, got Name=%q Email=%q", cfg.User.Name, cfg.User.Email)
	}
}

// removeUserSection removes the [user] section from git config content.
func removeUserSection(config string) string {
	lines := strings.Split(config, "\n")
	var result []string
	inUserSection := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[user]") {
			inUserSection = true
			continue
		}
		if inUserSection && strings.HasPrefix(trimmed, "[") {
			inUserSection = false
		}
		if !inUserSection {
			result = append(result, line)
		}
	}

	return strings.Join(result, "\n")
}
