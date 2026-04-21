package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gitCheckout uses git CLI instead of go-git to work around go-git v5 bug
// where Checkout deletes untracked files (see https://github.com/go-git/go-git/issues/970).
func gitCheckout(t *testing.T, dir, ref string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "checkout", ref)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Failed to checkout %s: %v\nOutput: %s", ref, err, output)
	}
}

func initOpenedTestRepo(t *testing.T, dir string) *git.Repository {
	t.Helper()
	testutil.InitRepo(t, dir)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	return repo
}

func TestGetCurrentBranch(t *testing.T) {
	// Create temp directory for test repo
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize repo
	repo := initOpenedTestRepo(t, tmpDir)

	// Create initial commit
	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Failed to get worktree: %v", err)
	}
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test"), 0o644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("Failed to add test file: %v", err)
	}
	commit, err := w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
		},
	})
	if err != nil {
		t.Fatalf("Failed to create initial commit: %v", err)
	}

	// Create feature branch
	featureRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("feature"), commit)
	if err := repo.Storer.SetReference(featureRef); err != nil {
		t.Fatalf("Failed to create feature branch: %v", err)
	}

	// Checkout feature branch
	gitCheckout(t, tmpDir, "feature")

	// Test getting current branch
	branch, err := GetCurrentBranch(context.Background())
	if err != nil {
		t.Fatalf("GetCurrentBranch(context.Background()) error = %v", err)
	}
	if branch != "feature" {
		t.Errorf("GetCurrentBranch(context.Background()) = %v, want feature", branch)
	}
}

func TestGetCurrentBranchDetachedHead(t *testing.T) {
	// Create temp directory for test repo
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize repo
	repo := initOpenedTestRepo(t, tmpDir)

	// Create initial commit
	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Failed to get worktree: %v", err)
	}
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test"), 0o644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("Failed to add test file: %v", err)
	}
	commit, err := w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
		},
	})
	if err != nil {
		t.Fatalf("Failed to create initial commit: %v", err)
	}

	// Checkout to detached HEAD
	gitCheckout(t, tmpDir, commit.String())

	// Test should error on detached HEAD
	_, err = GetCurrentBranch(context.Background())
	if err == nil {
		t.Error("GetCurrentBranch(context.Background()) expected error for detached HEAD, got nil")
	}
}

func TestGetMergeBase(t *testing.T) {
	// Create temp directory for test repo
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize repo
	repo := initOpenedTestRepo(t, tmpDir)

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Failed to get worktree: %v", err)
	}

	// Create initial commit on main
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("Failed to add test file: %v", err)
	}
	baseCommit, err := w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
		},
	})
	if err != nil {
		t.Fatalf("Failed to create initial commit: %v", err)
	}

	// Create main branch reference
	mainRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), baseCommit)
	if err := repo.Storer.SetReference(mainRef); err != nil {
		t.Fatalf("Failed to create main branch: %v", err)
	}

	// Create feature branch from base
	featureRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("feature"), baseCommit)
	if err := repo.Storer.SetReference(featureRef); err != nil {
		t.Fatalf("Failed to create feature branch: %v", err)
	}

	// Checkout feature and make a commit
	gitCheckout(t, tmpDir, "feature")
	if err := os.WriteFile(testFile, []byte("feature change"), 0o644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("Failed to add test file: %v", err)
	}
	if _, err := w.Commit("feature commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
		},
	}); err != nil {
		t.Fatalf("Failed to commit: %v", err)
	}

	// Test getting merge base
	mergeBase, err := GetMergeBase(context.Background(), "feature", "main")
	if err != nil {
		t.Fatalf("GetMergeBase(context.Background(),) error = %v", err)
	}
	if mergeBase.String() != baseCommit.String() {
		t.Errorf("GetMergeBase(context.Background(),) = %v, want %v", mergeBase, baseCommit)
	}
}

func TestGetMergeBaseNonExistentBranch(t *testing.T) {
	// Create temp directory for test repo
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize repo with commit
	repo := initOpenedTestRepo(t, tmpDir)

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Failed to get worktree: %v", err)
	}
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test"), 0o644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("Failed to add test file: %v", err)
	}
	if _, err := w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
		},
	}); err != nil {
		t.Fatalf("Failed to commit: %v", err)
	}

	// Test with non-existent branch
	_, err = GetMergeBase(context.Background(), "feature", "nonexistent")
	if err == nil {
		t.Error("GetMergeBase(context.Background(),) expected error for nonexistent branch, got nil")
	}
}

func TestHasUncommittedChanges(t *testing.T) {
	// Create temp directory for test repo
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize repo
	repo := initOpenedTestRepo(t, tmpDir)

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Failed to get worktree: %v", err)
	}

	// Create initial commit
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test"), 0o644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("Failed to add test file: %v", err)
	}
	if _, err := w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
		},
	}); err != nil {
		t.Fatalf("Failed to commit: %v", err)
	}

	// Test clean working tree
	hasChanges, err := HasUncommittedChanges(context.Background())
	if err != nil {
		t.Fatalf("HasUncommittedChanges(context.Background()) error = %v", err)
	}
	if hasChanges {
		t.Error("HasUncommittedChanges(context.Background()) = true, want false for clean tree")
	}

	// Make unstaged change
	if err := os.WriteFile(testFile, []byte("modified"), 0o644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	// Test with unstaged changes
	hasChanges, err = HasUncommittedChanges(context.Background())
	if err != nil {
		t.Fatalf("HasUncommittedChanges(context.Background()) error = %v", err)
	}
	if !hasChanges {
		t.Error("HasUncommittedChanges(context.Background()) = false, want true for modified file")
	}

	// Stage the change
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("Failed to add test file: %v", err)
	}

	// Test with staged changes
	hasChanges, err = HasUncommittedChanges(context.Background())
	if err != nil {
		t.Fatalf("HasUncommittedChanges(context.Background()) error = %v", err)
	}
	if !hasChanges {
		t.Error("HasUncommittedChanges(context.Background()) = false, want true for staged file")
	}

	// Commit and add untracked file
	if _, err := w.Commit("second commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
		},
	}); err != nil {
		t.Fatalf("Failed to commit: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "untracked.txt"), []byte("new"), 0o644); err != nil {
		t.Fatalf("Failed to write untracked file: %v", err)
	}

	// Test with untracked file (should be true)
	hasChanges, err = HasUncommittedChanges(context.Background())
	if err != nil {
		t.Fatalf("HasUncommittedChanges(context.Background()) error = %v", err)
	}
	if !hasChanges {
		t.Error("HasUncommittedChanges(context.Background()) = false, want true for untracked file")
	}

	// Clean up untracked file for next test
	if err := os.Remove(filepath.Join(tmpDir, "untracked.txt")); err != nil {
		t.Fatalf("Failed to remove untracked file: %v", err)
	}

	// Test global gitignore (core.excludesfile) handling
	// go-git doesn't read global gitignore, so we use git CLI instead.
	// Simulate global gitignore by setting core.excludesfile in repo config.
	// The file must be outside the repo to avoid showing up as untracked itself.
	globalIgnoreDir := t.TempDir()
	globalIgnoreFile := filepath.Join(globalIgnoreDir, "global-gitignore")
	if err := os.WriteFile(globalIgnoreFile, []byte("*.globally-ignored\n"), 0o644); err != nil {
		t.Fatalf("Failed to write global gitignore: %v", err)
	}

	// Set core.excludesfile in repo config
	cmd := exec.CommandContext(context.Background(), "git", "config", "core.excludesfile", globalIgnoreFile)
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to set core.excludesfile: %v", err)
	}

	// Create a file that matches the global ignore pattern
	if err := os.WriteFile(filepath.Join(tmpDir, "secret.globally-ignored"), []byte("ignored"), 0o644); err != nil {
		t.Fatalf("Failed to write globally ignored file: %v", err)
	}

	// Test with globally gitignored file - should return false (clean)
	// This catches regressions if someone switches back to go-git's Status()
	// which doesn't read core.excludesfile (global gitignore)
	hasChanges, err = HasUncommittedChanges(context.Background())
	if err != nil {
		t.Fatalf("HasUncommittedChanges(context.Background()) error = %v", err)
	}
	if hasChanges {
		t.Error("HasUncommittedChanges(context.Background()) = true, want false for globally gitignored file (core.excludesfile)")
	}
}

func TestFindNewUntrackedFiles(t *testing.T) {
	tests := []struct {
		name        string
		current     []string
		preExisting []string
		expected    []string
	}{
		{
			name:        "finds new files not in pre-existing list",
			current:     []string{"file1.go", "file2.go", "file3.go"},
			preExisting: []string{"file1.go"},
			expected:    []string{"file2.go", "file3.go"},
		},
		{
			name:        "returns empty when all files pre-exist",
			current:     []string{"file1.go", "file2.go"},
			preExisting: []string{"file1.go", "file2.go"},
			expected:    nil,
		},
		{
			name:        "returns all files when pre-existing is empty",
			current:     []string{"file1.go", "file2.go"},
			preExisting: []string{},
			expected:    []string{"file1.go", "file2.go"},
		},
		{
			name:        "returns nil when current is empty",
			current:     []string{},
			preExisting: []string{"file1.go"},
			expected:    nil,
		},
		{
			name:        "handles nil current slice",
			current:     nil,
			preExisting: []string{"file1.go"},
			expected:    nil,
		},
		{
			name:        "handles nil pre-existing slice",
			current:     []string{"file1.go", "file2.go"},
			preExisting: nil,
			expected:    []string{"file1.go", "file2.go"},
		},
		{
			name:        "handles both nil slices",
			current:     nil,
			preExisting: nil,
			expected:    nil,
		},
		{
			name:        "handles files with paths",
			current:     []string{"src/main.go", "src/utils.go", "test/main_test.go"},
			preExisting: []string{"src/main.go"},
			expected:    []string{"src/utils.go", "test/main_test.go"},
		},
		{
			name:        "handles duplicate files in pre-existing",
			current:     []string{"file1.go", "file2.go"},
			preExisting: []string{"file1.go", "file1.go"},
			expected:    []string{"file2.go"},
		},
		{
			name:        "is case-sensitive",
			current:     []string{"File.go", "file.go"},
			preExisting: []string{"file.go"},
			expected:    []string{"File.go"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := findNewUntrackedFiles(tt.current, tt.preExisting)

			if len(result) != len(tt.expected) {
				t.Errorf("findNewUntrackedFiles() returned %d files, want %d", len(result), len(tt.expected))
				t.Errorf("got: %v, want: %v", result, tt.expected)
				return
			}

			// Create a map for easy lookup
			expectedMap := make(map[string]bool)
			for _, f := range tt.expected {
				expectedMap[f] = true
			}

			for _, f := range result {
				if !expectedMap[f] {
					t.Errorf("findNewUntrackedFiles() returned unexpected file %q", f)
				}
			}
		})
	}
}

func TestGetGitConfigValue(t *testing.T) {
	// Test that invalid keys return empty string
	invalid := getGitConfigValue(context.Background(), "nonexistent.key.that.does.not.exist")
	if invalid != "" {
		t.Errorf("expected empty string for invalid key, got %q", invalid)
	}

	// Test that it returns a value for user.name (assuming git is configured on test machine)
	// This is a basic sanity check - it may return empty on unconfigured systems
	name := getGitConfigValue(context.Background(), "user.name")
	t.Logf("git config user.name returned: %q", name)
}

func TestGetGitConfigValueTrimsWhitespace(t *testing.T) {
	// The git config command returns values with trailing newline
	// Verify that getGitConfigValue trims whitespace properly
	email := getGitConfigValue(context.Background(), "user.email")
	t.Logf("git config user.email returned: %q", email)

	// If email is set, verify no leading/trailing whitespace
	if email != "" {
		if email[0] == ' ' || email[0] == '\n' || email[0] == '\t' {
			t.Errorf("expected no leading whitespace, got %q", email)
		}
		if email[len(email)-1] == ' ' || email[len(email)-1] == '\n' || email[len(email)-1] == '\t' {
			t.Errorf("expected no trailing whitespace, got %q", email)
		}
	}
}

func TestGetGitAuthorReturnsAuthor(t *testing.T) {
	// Create temp directory for test repo
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize repo with user config
	repo := initOpenedTestRepo(t, tmpDir)

	// Set local user config
	cfg, err := repo.Config()
	if err != nil {
		t.Fatalf("Failed to get repo config: %v", err)
	}
	cfg.User.Name = "Test Author"
	cfg.User.Email = "test@example.com"
	if err := repo.SetConfig(cfg); err != nil {
		t.Fatalf("Failed to set repo config: %v", err)
	}

	// Test GetGitAuthor
	author, err := GetGitAuthor(context.Background())
	if err != nil {
		t.Fatalf("GetGitAuthor(context.Background()) error = %v", err)
	}

	if author.Name != "Test Author" {
		t.Errorf("GetGitAuthor(context.Background()).Name = %q, want %q", author.Name, "Test Author")
	}
	if author.Email != "test@example.com" {
		t.Errorf("GetGitAuthor(context.Background()).Email = %q, want %q", author.Email, "test@example.com")
	}
}

func TestGetGitAuthorFallsBackToGitCommand(t *testing.T) {
	// Create temp directory for test repo
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize repo WITHOUT setting user config in go-git
	// This simulates the case where go-git can't find the config
	_, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("Failed to init repo: %v", err)
	}

	// GetGitAuthor should NOT error - it falls back to git command or returns defaults
	author, err := GetGitAuthor(context.Background())
	if err != nil {
		t.Fatalf("GetGitAuthor(context.Background()) should not error, got: %v", err)
	}

	// Verify it's not nil first
	require.NotNil(t, author, "GetGitAuthor(context.Background()) returned nil author")

	// The author should have some value (either from global git config or defaults)
	t.Logf("GetGitAuthor(context.Background()) returned Name=%q, Email=%q", author.Name, author.Email)
}

func TestGetGitAuthorReturnsDefaultsWhenNoConfig(t *testing.T) {
	// Create temp directory for test repo
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize repo without user config
	_, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("Failed to init repo: %v", err)
	}

	// Even without config, GetGitAuthor should not error
	// It will return either values from global git config OR defaults
	author, err := GetGitAuthor(context.Background())
	if err != nil {
		t.Fatalf("GetGitAuthor(context.Background()) should not error even without config, got: %v", err)
	}

	// Just verify we got a non-nil result first
	require.NotNil(t, author, "GetGitAuthor(context.Background()) returned nil")

	// Name and Email should be non-empty (either from global config or defaults)
	if author.Name == "" {
		t.Error("GetGitAuthor(context.Background()).Name is empty, expected a value or default")
	}
	if author.Email == "" {
		t.Error("GetGitAuthor(context.Background()).Email is empty, expected a value or default")
	}
}

func TestBranchExistsOnRemote(t *testing.T) {
	// Create temp directory for test repo
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize repo
	repo := initOpenedTestRepo(t, tmpDir)

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Failed to get worktree: %v", err)
	}

	// Create initial commit
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test"), 0o644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("Failed to add test file: %v", err)
	}
	commit, err := w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
		},
	})
	if err != nil {
		t.Fatalf("Failed to commit: %v", err)
	}

	// Create remote reference (simulating a pushed branch)
	remoteRef := plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", "feature"), commit)
	if err := repo.Storer.SetReference(remoteRef); err != nil {
		t.Fatalf("Failed to create remote ref: %v", err)
	}

	t.Run("returns true when branch exists on remote", func(t *testing.T) {
		exists, err := BranchExistsOnRemote(context.Background(), "feature")
		if err != nil {
			t.Fatalf("BranchExistsOnRemote(context.Background(),) error = %v", err)
		}
		if !exists {
			t.Error("BranchExistsOnRemote(context.Background(),) = false, want true for existing remote branch")
		}
	})

	t.Run("returns false when branch does not exist on remote", func(t *testing.T) {
		exists, err := BranchExistsOnRemote(context.Background(), "nonexistent")
		if err != nil {
			t.Fatalf("BranchExistsOnRemote(context.Background(),) error = %v", err)
		}
		if exists {
			t.Error("BranchExistsOnRemote(context.Background(),) = true, want false for nonexistent remote branch")
		}
	})
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointFetchTarget_NoCheckpointRemote(t *testing.T) {
	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")

	// Add origin remote
	cmd := exec.CommandContext(context.Background(), "git", "remote", "add", "origin", "git@github.com:org/main-repo.git")
	cmd.Dir = localDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	// Settings with no checkpoint_remote
	entireDir := filepath.Join(localDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, "settings.json"),
		[]byte(`{"enabled": true}`),
		0o644,
	))

	t.Chdir(localDir)

	target := resolveCheckpointFetchTarget(context.Background())
	assert.Equal(t, "git@github.com:org/main-repo.git", target)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointFetchTarget_WithCheckpointRemote(t *testing.T) {
	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")

	// Add SSH origin remote — checkpoint URL derives protocol from origin
	cmd := exec.CommandContext(context.Background(), "git", "remote", "add", "origin", "git@github.com:org/main-repo.git")
	cmd.Dir = localDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	// Settings with checkpoint_remote configured
	entireDir := filepath.Join(localDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "org/checkpoints"}}}`),
		0o644,
	))

	t.Chdir(localDir)

	target := resolveCheckpointFetchTarget(context.Background())
	assert.Equal(t, "git@github.com:org/checkpoints.git", target)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointFetchTarget_FallsBackOnError(t *testing.T) {
	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")

	// No origin remote — FetchURL cannot resolve an effective fetch URL.

	// Settings with checkpoint_remote configured but no origin to derive URL from
	entireDir := filepath.Join(localDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(entireDir, "settings.json"),
		[]byte(`{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github", "repo": "org/checkpoints"}}}`),
		0o644,
	))

	t.Chdir(localDir)

	// Falls back to the origin remote name when URL resolution fails.
	target := resolveCheckpointFetchTarget(context.Background())
	assert.Equal(t, "origin", target)
}

// setupRepoWithBlobOnMetadataBranch creates a repo with a blob committed on
// entire/checkpoints/v1, checks out the default branch, and returns
// (repoDir, blobHash) for tests that need a reachable blob on the metadata branch.
func setupRepoWithBlobOnMetadataBranch(t *testing.T) (string, plumbing.Hash) {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")

	defaultBranch := gitDefaultBranch(t, dir)

	gitRun(t, dir, "checkout", "--orphan", "entire/checkpoints/v1")
	gitRun(t, dir, "rm", "-rf", ".")
	testutil.WriteFile(t, dir, "ab/cdef123456/metadata.json", `{"checkpoint_id": "abcdef123456"}`)
	testutil.GitAdd(t, dir, "ab/cdef123456/metadata.json")
	gitRun(t, dir, "-c", "commit.gpgsign=false", "commit", "-m", "Checkpoint: abcdef123456")

	blobHash := plumbing.NewHash(gitOutput(t, dir, "rev-parse", "HEAD:ab/cdef123456/metadata.json"))

	gitRun(t, dir, "checkout", defaultBranch)
	return dir, blobHash
}

// Not parallel: uses t.Chdir()
// Tests basic FetchBlobsByHash mechanics: when the resolved fetch target has
// the blob, the function brings it into the local object store.
// Target selection is tested separately in TestResolveCheckpointFetchTarget_*.
func TestFetchBlobsByHash_FetchesMissingBlob(t *testing.T) {
	ctx := context.Background()

	remoteDir, blobHash := setupRepoWithBlobOnMetadataBranch(t)

	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")

	// Origin is the remote that has the blob. With no checkpoint_remote
	// configured, resolveCheckpointFetchTarget returns "origin".
	gitRun(t, localDir, "remote", "add", "origin", remoteDir)

	t.Chdir(localDir)

	// Precondition: blob is not local
	localRepo, err := git.PlainOpen(localDir)
	require.NoError(t, err)
	require.Error(t, localRepo.Storer.HasEncodedObject(blobHash), "blob should not exist locally before fetch")

	// Fetch succeeds; blob lands in local store
	require.NoError(t, FetchBlobsByHash(ctx, []plumbing.Hash{blobHash}))

	freshRepo, err := git.PlainOpen(localDir)
	require.NoError(t, err)
	require.NoError(t, freshRepo.Storer.HasEncodedObject(blobHash), "blob should exist locally after fetch")
}

// Not parallel: uses t.Chdir()
// Tests that FetchBlobsByHash returns an error when the blob is unreachable
// from the resolved target and both fallback fetches fail.
func TestFetchBlobsByHash_FailsWhenBlobUnreachable(t *testing.T) {
	ctx := context.Background()

	// Origin has no metadata branch, no blobs
	originDir := t.TempDir()
	testutil.InitRepo(t, originDir)
	testutil.WriteFile(t, originDir, "f.txt", "init")
	testutil.GitAdd(t, originDir, "f.txt")
	testutil.GitCommit(t, originDir, "init")

	localDir := t.TempDir()
	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "f.txt", "init")
	testutil.GitAdd(t, localDir, "f.txt")
	testutil.GitCommit(t, localDir, "init")

	gitRun(t, localDir, "remote", "add", "origin", originDir)

	t.Chdir(localDir)

	// Arbitrary hash nobody has
	unreachable := plumbing.NewHash("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")

	err := FetchBlobsByHash(ctx, []plumbing.Hash{unreachable})
	require.Error(t, err, "FetchBlobsByHash should fail when blob is unreachable and no fallback succeeds")
}

// gitRun runs a git command in dir and fails the test on error.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s failed: %v\nOutput: %s", args[0], err, output)
	}
}

// gitOutput runs a git command and returns trimmed stdout.
func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
}

// gitDefaultBranch returns the current branch name in a repo.
func gitDefaultBranch(t *testing.T, dir string) string {
	t.Helper()
	return gitOutput(t, dir, "rev-parse", "--abbrev-ref", "HEAD")
}
