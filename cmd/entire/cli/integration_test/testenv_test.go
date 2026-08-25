//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

func TestNewTestEnv(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)

	// Verify RepoDir exists
	if _, err := os.Stat(env.RepoDir); os.IsNotExist(err) {
		t.Error("RepoDir should exist")
	}

	// Verify ClaudeProjectDir exists
	if _, err := os.Stat(env.ClaudeProjectDir); os.IsNotExist(err) {
		t.Error("ClaudeProjectDir should exist")
	}

	// Verify ClaudeProjectDir is set in struct (no longer uses env var for parallel test compatibility)
	if env.ClaudeProjectDir == "" {
		t.Error("ClaudeProjectDir should not be empty")
	}

	// Note: NewTestEnv no longer changes working directory or uses t.Setenv
	// to allow parallel execution. CLI commands receive env vars via cmd.Env.
}

func TestTestEnv_InitRepo(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	env.InitRepo()

	// Verify .git directory exists
	gitDir := filepath.Join(env.RepoDir, ".git")
	if _, err := os.Stat(gitDir); os.IsNotExist(err) {
		t.Error(".git directory should exist after InitRepo")
	}
}

func TestTestEnv_InitEntire(t *testing.T) {
	t.Parallel()
	env := NewRepoEnv(t)
	// Verify .entire directory exists
	entireDir := filepath.Join(env.RepoDir, ".entire")
	if _, err := os.Stat(entireDir); os.IsNotExist(err) {
		t.Error(".entire directory should exist")
	}

	// Verify settings file exists and contains enabled
	settingsPath := filepath.Join(entireDir, paths.SettingsFileName)
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("failed to read %s: %v", paths.SettingsFileName, err)
	}

	settingsContent := string(data)
	if !strings.Contains(settingsContent, `"enabled"`) {
		t.Errorf("settings.json should contain enabled field, got: %s", settingsContent)
	}

	// Verify tmp directory exists
	tmpDir := filepath.Join(entireDir, "tmp")
	if _, err := os.Stat(tmpDir); os.IsNotExist(err) {
		t.Error(".entire/tmp directory should exist")
	}
}

func TestTestEnv_WriteAndReadFile(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	env.InitRepo()

	// Write a simple file
	env.WriteFile("test.txt", "hello world")

	// Read it back
	content := env.ReadFile("test.txt")
	if content != "hello world" {
		t.Errorf("ReadFile = %q, want %q", content, "hello world")
	}

	// Write a file in a subdirectory
	env.WriteFile("src/main.go", "package main")

	content = env.ReadFile("src/main.go")
	if content != "package main" {
		t.Errorf("ReadFile = %q, want %q", content, "package main")
	}
}

func TestTestEnv_FileExists(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	env.InitRepo()

	// File doesn't exist yet
	if env.FileExists("test.txt") {
		t.Error("FileExists should return false for non-existent file")
	}

	// Create file
	env.WriteFile("test.txt", "content")

	// Now it exists
	if !env.FileExists("test.txt") {
		t.Error("FileExists should return true for existing file")
	}
}

func TestTestEnv_GitAddAndCommit(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	env.InitRepo()

	// Create and commit a file
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	// Verify we can get the HEAD hash
	hash := env.GetHeadHash()
	if len(hash) != 40 {
		t.Errorf("GetHeadHash returned invalid hash: %s", hash)
	}
}

func TestTestEnv_MultipleCommits(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	env.InitRepo()

	// First commit
	env.WriteFile("file.txt", "v1")
	env.GitAdd("file.txt")
	env.GitCommit("Commit 1")
	hash1 := env.GetHeadHash()

	// Second commit
	env.WriteFile("file.txt", "v2")
	env.GitAdd("file.txt")
	env.GitCommit("Commit 2")
	hash2 := env.GetHeadHash()

	// Hashes should be different
	if hash1 == hash2 {
		t.Error("different commits should have different hashes")
	}
}

func TestNewRepoEnv(t *testing.T) {
	t.Parallel()
	env := NewRepoEnv(t)

	// Verify .git directory exists
	gitDir := filepath.Join(env.RepoDir, ".git")
	if _, err := os.Stat(gitDir); os.IsNotExist(err) {
		t.Error(".git directory should exist")
	}

	// Verify .entire directory exists
	entireDir := filepath.Join(env.RepoDir, ".entire")
	if _, err := os.Stat(entireDir); os.IsNotExist(err) {
		t.Error(".entire directory should exist")
	}
}

func TestNewRepoWithCommit(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)

	// Verify README exists
	if !env.FileExists("README.md") {
		t.Error("README.md should exist")
	}

	// Verify we have a commit
	hash := env.GetHeadHash()
	if len(hash) != 40 {
		t.Errorf("GetHeadHash returned invalid hash: %s", hash)
	}
}

func TestNewFeatureBranchEnv(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	// Verify we're on feature branch
	branch := env.GetCurrentBranch()
	if branch != "feature/test-branch" {
		t.Errorf("GetCurrentBranch = %s, want feature/test-branch", branch)
	}

	// Verify README exists
	if !env.FileExists("README.md") {
		t.Error("README.md should exist")
	}
}

func TestNormalizeGitConfigForGuard_IgnoresTransportPromisorRemote(t *testing.T) {
	t.Parallel()

	baseline := `[core]
	repositoryformatversion = 0
`
	withURLPromisor := `[core]
	repositoryformatversion = 1
[remote "https://github.com/entireio/cli.git"]
	promisor = true
	partialclonefilter = blob:none
`

	if got, want := normalizeGitConfigForGuard(withURLPromisor), normalizeGitConfigForGuard(baseline); got != want {
		t.Fatalf("normalizeGitConfigForGuard should ignore transport-keyed promisor remotes\nGot:\n%s\nWant:\n%s", got, want)
	}
}

func TestNormalizeGitConfigForGuard_PreservesNamedRemotePromisor(t *testing.T) {
	t.Parallel()

	baseline := `[core]
	repositoryformatversion = 0
`
	withOriginPromisor := `[core]
	repositoryformatversion = 1
[remote "origin"]
	promisor = true
	partialclonefilter = blob:none
`

	if normalizeGitConfigForGuard(withOriginPromisor) == normalizeGitConfigForGuard(baseline) {
		t.Fatal("normalizeGitConfigForGuard should preserve named remote promisor changes")
	}
}
