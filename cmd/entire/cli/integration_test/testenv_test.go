//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/strategy"
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
	RunForAllStrategiesWithBasicEnv(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Verify .entire directory exists
		entireDir := filepath.Join(env.RepoDir, ".entire")
		if _, err := os.Stat(entireDir); os.IsNotExist(err) {
			t.Error(".entire directory should exist")
		}

		// Verify settings file exists and contains strategy
		settingsPath := filepath.Join(entireDir, paths.SettingsFileName)
		data, err := os.ReadFile(settingsPath)
		if err != nil {
			t.Fatalf("failed to read %s: %v", paths.SettingsFileName, err)
		}

		settingsContent := string(data)
		expectedStrategy := `"strategy": "` + strategyName + `"`
		if !strings.Contains(settingsContent, expectedStrategy) {
			t.Errorf("settings.json should contain %s, got: %s", expectedStrategy, settingsContent)
		}

		// Verify tmp directory exists
		tmpDir := filepath.Join(entireDir, "tmp")
		if _, err := os.Stat(tmpDir); os.IsNotExist(err) {
			t.Error(".entire/tmp directory should exist")
		}
	})
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
	env := NewRepoEnv(t, strategy.StrategyNameManualCommit)

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
	env := NewRepoWithCommit(t, strategy.StrategyNameManualCommit)

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
	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

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

func TestAllStrategies(t *testing.T) {
	t.Parallel()
	strategies := AllStrategies()
	if len(strategies) != 2 {
		t.Errorf("AllStrategies() returned %d strategies, want 2", len(strategies))
	}

	// Verify expected strategies are present
	expected := []string{"auto-commit", "manual-commit"}
	for _, exp := range expected {
		found := false
		for _, s := range strategies {
			if s == exp {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("AllStrategies() missing %s", exp)
		}
	}
}

func TestRunForAllStrategies(t *testing.T) {
	t.Parallel()
	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Verify we're on feature branch
		branch := env.GetCurrentBranch()
		if branch != "feature/test-branch" {
			t.Errorf("GetCurrentBranch = %s, want feature/test-branch", branch)
		}

		// Verify strategy was passed correctly
		if strategyName == "" {
			t.Error("strategyName should not be empty")
		}
	})
}
