package strategy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestOpenRepository(t *testing.T) {
	// Create a temporary directory for the test repository
	tmpDir := t.TempDir()

	// Initialize a git repository
	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("failed to init repo: %v", err)
	}

	// Create a test file and commit it
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test content"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	if _, err := worktree.Add("test.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}

	if _, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
		},
	}); err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	// Change to the repository directory
	t.Chdir(tmpDir)

	// Test OpenRepository
	openedRepo, err := OpenRepository()
	if err != nil {
		t.Fatalf("OpenRepository() failed: %v", err)
	}

	if openedRepo == nil {
		t.Fatal("OpenRepository() returned nil repository")
	}

	// Verify we can perform basic operations
	head, err := openedRepo.Head()
	if err != nil {
		t.Fatalf("failed to get HEAD: %v", err)
	}

	if head == nil {
		t.Fatal("HEAD is nil")
	}

	// Verify we can get the commit
	commit, err := openedRepo.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("failed to get commit: %v", err)
	}

	if commit.Message != "Initial commit" {
		t.Errorf("expected commit message 'Initial commit', got '%s'", commit.Message)
	}
}

func TestOpenRepositoryError(t *testing.T) {
	// Create a temporary directory without git repository
	tmpDir := t.TempDir()

	// Change to the non-repository directory
	t.Chdir(tmpDir)

	// Test OpenRepository should fail
	_, err := OpenRepository()
	if err == nil {
		t.Fatal("OpenRepository() should have failed in non-repository directory")
	}
}

func TestIsInsideWorktree(t *testing.T) {
	t.Run("main repo", func(t *testing.T) {
		tmpDir := t.TempDir()
		initTestRepo(t, tmpDir)
		t.Chdir(tmpDir)

		if IsInsideWorktree() {
			t.Error("IsInsideWorktree() should return false in main repo")
		}
	})

	t.Run("worktree", func(t *testing.T) {
		tmpDir := t.TempDir()
		initTestRepo(t, tmpDir)

		// Create a worktree
		worktreeDir := filepath.Join(tmpDir, "worktree")
		if err := createWorktree(tmpDir, worktreeDir, "test-branch"); err != nil {
			t.Fatalf("failed to create worktree: %v", err)
		}
		t.Cleanup(func() {
			removeWorktree(tmpDir, worktreeDir)
		})

		t.Chdir(worktreeDir)

		if !IsInsideWorktree() {
			t.Error("IsInsideWorktree() should return true in worktree")
		}
	})

	t.Run("non-repo", func(t *testing.T) {
		tmpDir := t.TempDir()
		t.Chdir(tmpDir)

		if IsInsideWorktree() {
			t.Error("IsInsideWorktree() should return false in non-repo")
		}
	})
}

func TestGetMainRepoRoot(t *testing.T) {
	t.Run("main repo", func(t *testing.T) {
		tmpDir := t.TempDir()
		// Resolve symlinks (macOS /var -> /private/var)
		// git rev-parse --show-toplevel returns the resolved path
		resolved, err := filepath.EvalSymlinks(tmpDir)
		if err != nil {
			t.Fatalf("filepath.EvalSymlinks() failed: %v", err)
		}
		tmpDir = resolved

		initTestRepo(t, tmpDir)
		t.Chdir(tmpDir)

		root, err := GetMainRepoRoot()
		if err != nil {
			t.Fatalf("GetMainRepoRoot() failed: %v", err)
		}

		if root != tmpDir {
			t.Errorf("GetMainRepoRoot() = %q, want %q", root, tmpDir)
		}
	})

	t.Run("worktree", func(t *testing.T) {
		tmpDir := t.TempDir()
		// Resolve symlinks (macOS /var -> /private/var)
		resolved, err := filepath.EvalSymlinks(tmpDir)
		if err != nil {
			t.Fatalf("filepath.EvalSymlinks() failed: %v", err)
		}
		tmpDir = resolved

		initTestRepo(t, tmpDir)

		worktreeDir := filepath.Join(tmpDir, "worktree")
		if err := createWorktree(tmpDir, worktreeDir, "test-branch"); err != nil {
			t.Fatalf("failed to create worktree: %v", err)
		}
		t.Cleanup(func() {
			removeWorktree(tmpDir, worktreeDir)
		})

		t.Chdir(worktreeDir)

		root, err := GetMainRepoRoot()
		if err != nil {
			t.Fatalf("GetMainRepoRoot() failed: %v", err)
		}

		if root != tmpDir {
			t.Errorf("GetMainRepoRoot() = %q, want %q", root, tmpDir)
		}
	})
}

func TestGetCurrentBranchName(t *testing.T) {
	t.Run("on branch", func(t *testing.T) {
		tmpDir := t.TempDir()
		repo, err := git.PlainInit(tmpDir, false)
		if err != nil {
			t.Fatalf("failed to init repo: %v", err)
		}

		// Create initial commit
		testFile := filepath.Join(tmpDir, "test.txt")
		if err := os.WriteFile(testFile, []byte("test"), 0o644); err != nil {
			t.Fatalf("failed to write file: %v", err)
		}

		wt, err := repo.Worktree()
		if err != nil {
			t.Fatalf("failed to get worktree: %v", err)
		}

		if _, err := wt.Add("test.txt"); err != nil {
			t.Fatalf("failed to add file: %v", err)
		}

		if _, err := wt.Commit("Initial commit", &git.CommitOptions{
			Author: &object.Signature{Name: "Test", Email: "test@test.com"},
		}); err != nil {
			t.Fatalf("failed to commit: %v", err)
		}

		// Should be on default branch (master or main)
		branchName := GetCurrentBranchName(repo)
		if branchName == "" {
			t.Error("GetCurrentBranchName() returned empty string, expected branch name")
		}

		// Create and checkout a new branch
		head, err := repo.Head()
		if err != nil {
			t.Fatalf("failed to get HEAD: %v", err)
		}

		newBranch := "feature/test-branch"
		if err := wt.Checkout(&git.CheckoutOptions{
			Branch: plumbing.NewBranchReferenceName(newBranch),
			Create: true,
			Hash:   head.Hash(),
		}); err != nil {
			t.Fatalf("failed to checkout branch: %v", err)
		}

		// Should return the new branch name
		branchName = GetCurrentBranchName(repo)
		if branchName != newBranch {
			t.Errorf("GetCurrentBranchName() = %q, want %q", branchName, newBranch)
		}
	})

	t.Run("detached HEAD", func(t *testing.T) {
		tmpDir := t.TempDir()
		repo, err := git.PlainInit(tmpDir, false)
		if err != nil {
			t.Fatalf("failed to init repo: %v", err)
		}

		// Create initial commit
		testFile := filepath.Join(tmpDir, "test.txt")
		if err := os.WriteFile(testFile, []byte("test"), 0o644); err != nil {
			t.Fatalf("failed to write file: %v", err)
		}

		wt, err := repo.Worktree()
		if err != nil {
			t.Fatalf("failed to get worktree: %v", err)
		}

		if _, err := wt.Add("test.txt"); err != nil {
			t.Fatalf("failed to add file: %v", err)
		}

		commitHash, err := wt.Commit("Initial commit", &git.CommitOptions{
			Author: &object.Signature{Name: "Test", Email: "test@test.com"},
		})
		if err != nil {
			t.Fatalf("failed to commit: %v", err)
		}

		// Checkout the commit directly (detached HEAD)
		if err := wt.Checkout(&git.CheckoutOptions{Hash: commitHash}); err != nil {
			t.Fatalf("failed to checkout commit: %v", err)
		}

		// Should return empty string for detached HEAD
		branchName := GetCurrentBranchName(repo)
		if branchName != "" {
			t.Errorf("GetCurrentBranchName() = %q, want empty string for detached HEAD", branchName)
		}
	})
}

// initTestRepo creates a git repo with an initial commit
func initTestRepo(t *testing.T, dir string) {
	t.Helper()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("failed to init repo: %v", err)
	}

	testFile := filepath.Join(dir, "README.md")
	if err := os.WriteFile(testFile, []byte("# Test"), 0o600); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	if _, err := wt.Add("README.md"); err != nil {
		t.Fatalf("failed to add: %v", err)
	}

	if _, err := wt.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	}); err != nil {
		t.Fatalf("failed to commit: %v", err)
	}
}

// createWorktree creates a git worktree using native git command
func createWorktree(repoDir, worktreeDir, branch string) error {
	cmd := exec.CommandContext(context.Background(), "git", "worktree", "add", worktreeDir, "-b", branch)
	cmd.Dir = repoDir
	return cmd.Run()
}

// removeWorktree removes a git worktree using native git command
func removeWorktree(repoDir, worktreeDir string) {
	cmd := exec.CommandContext(context.Background(), "git", "worktree", "remove", worktreeDir, "--force")
	cmd.Dir = repoDir
	_ = cmd.Run() //nolint:errcheck // Best effort cleanup, ignore errors
}
