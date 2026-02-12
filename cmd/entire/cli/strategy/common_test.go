package strategy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
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

func TestGetDefaultBranchName(t *testing.T) {
	t.Run("returns main when main branch exists", func(t *testing.T) {
		tmpDir := t.TempDir()
		repo, err := git.PlainInit(tmpDir, false)
		if err != nil {
			t.Fatalf("failed to init repo: %v", err)
		}

		// Create initial commit (go-git creates master by default)
		testFile := filepath.Join(tmpDir, "test.txt")
		if err := os.WriteFile(testFile, []byte("test"), 0o644); err != nil {
			t.Fatalf("failed to write test file: %v", err)
		}

		wt, err := repo.Worktree()
		if err != nil {
			t.Fatalf("failed to get worktree: %v", err)
		}

		if _, err := wt.Add("test.txt"); err != nil {
			t.Fatalf("failed to add file: %v", err)
		}

		commitHash, err := wt.Commit("Initial commit", &git.CommitOptions{
			Author: &object.Signature{Name: "Test", Email: "test@example.com"},
		})
		if err != nil {
			t.Fatalf("failed to commit: %v", err)
		}

		// Create main branch
		ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), commitHash)
		if err := repo.Storer.SetReference(ref); err != nil {
			t.Fatalf("failed to create main branch: %v", err)
		}

		result := GetDefaultBranchName(repo)

		if result != "main" {
			t.Errorf("GetDefaultBranchName() = %q, want %q", result, "main")
		}
	})

	t.Run("returns master when only master exists", func(t *testing.T) {
		tmpDir := t.TempDir()
		repo, err := git.PlainInit(tmpDir, false)
		if err != nil {
			t.Fatalf("failed to init repo: %v", err)
		}

		// Create initial commit (go-git creates master by default)
		testFile := filepath.Join(tmpDir, "test.txt")
		if err := os.WriteFile(testFile, []byte("test"), 0o644); err != nil {
			t.Fatalf("failed to write test file: %v", err)
		}

		wt, err := repo.Worktree()
		if err != nil {
			t.Fatalf("failed to get worktree: %v", err)
		}

		if _, err := wt.Add("test.txt"); err != nil {
			t.Fatalf("failed to add file: %v", err)
		}

		if _, err := wt.Commit("Initial commit", &git.CommitOptions{
			Author: &object.Signature{Name: "Test", Email: "test@example.com"},
		}); err != nil {
			t.Fatalf("failed to commit: %v", err)
		}

		result := GetDefaultBranchName(repo)

		if result != "master" {
			t.Errorf("GetDefaultBranchName() = %q, want %q", result, "master")
		}
	})

	t.Run("returns empty when no main or master", func(t *testing.T) {
		tmpDir := t.TempDir()
		repo, err := git.PlainInit(tmpDir, false)
		if err != nil {
			t.Fatalf("failed to init repo: %v", err)
		}

		// Create initial commit
		testFile := filepath.Join(tmpDir, "test.txt")
		if err := os.WriteFile(testFile, []byte("test"), 0o644); err != nil {
			t.Fatalf("failed to write test file: %v", err)
		}

		wt, err := repo.Worktree()
		if err != nil {
			t.Fatalf("failed to get worktree: %v", err)
		}

		if _, err := wt.Add("test.txt"); err != nil {
			t.Fatalf("failed to add file: %v", err)
		}

		commitHash, err := wt.Commit("Initial commit", &git.CommitOptions{
			Author: &object.Signature{Name: "Test", Email: "test@example.com"},
		})
		if err != nil {
			t.Fatalf("failed to commit: %v", err)
		}

		// Create a different branch and delete master
		if err := wt.Checkout(&git.CheckoutOptions{
			Hash:   commitHash,
			Branch: plumbing.NewBranchReferenceName("develop"),
			Create: true,
		}); err != nil {
			t.Fatalf("failed to create develop branch: %v", err)
		}

		// Delete master branch
		if err := repo.Storer.RemoveReference(plumbing.NewBranchReferenceName("master")); err != nil {
			t.Fatalf("failed to delete master branch: %v", err)
		}

		result := GetDefaultBranchName(repo)
		if result != "" {
			t.Errorf("GetDefaultBranchName() = %q, want empty string", result)
		}
	})

	t.Run("returns origin/HEAD target when set", func(t *testing.T) {
		tmpDir := t.TempDir()
		repo, err := git.PlainInit(tmpDir, false)
		if err != nil {
			t.Fatalf("failed to init repo: %v", err)
		}

		// Create initial commit
		testFile := filepath.Join(tmpDir, "test.txt")
		if err := os.WriteFile(testFile, []byte("test"), 0o644); err != nil {
			t.Fatalf("failed to write test file: %v", err)
		}

		wt, err := repo.Worktree()
		if err != nil {
			t.Fatalf("failed to get worktree: %v", err)
		}

		if _, err := wt.Add("test.txt"); err != nil {
			t.Fatalf("failed to add file: %v", err)
		}

		commitHash, err := wt.Commit("Initial commit", &git.CommitOptions{
			Author: &object.Signature{Name: "Test", Email: "test@example.com"},
		})
		if err != nil {
			t.Fatalf("failed to commit: %v", err)
		}

		// Create trunk branch (simulate non-standard default branch)
		trunkRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("trunk"), commitHash)
		if err := repo.Storer.SetReference(trunkRef); err != nil {
			t.Fatalf("failed to create trunk branch: %v", err)
		}

		// Create origin/trunk remote ref
		originTrunkRef := plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", "trunk"), commitHash)
		if err := repo.Storer.SetReference(originTrunkRef); err != nil {
			t.Fatalf("failed to create origin/trunk ref: %v", err)
		}

		// Set origin/HEAD to point to origin/trunk (symbolic ref)
		originHeadRef := plumbing.NewSymbolicReference(plumbing.ReferenceName("refs/remotes/origin/HEAD"), plumbing.ReferenceName("refs/remotes/origin/trunk"))
		if err := repo.Storer.SetReference(originHeadRef); err != nil {
			t.Fatalf("failed to set origin/HEAD: %v", err)
		}

		// Delete master branch so it doesn't take precedence
		_ = repo.Storer.RemoveReference(plumbing.NewBranchReferenceName("master")) //nolint:errcheck // best-effort cleanup for test

		result := GetDefaultBranchName(repo)

		if result != "trunk" {
			t.Errorf("GetDefaultBranchName() = %q, want %q", result, "trunk")
		}
	})
}

func TestIsOnDefaultBranch(t *testing.T) {
	t.Run("returns true when on main", func(t *testing.T) {
		tmpDir := t.TempDir()
		repo, err := git.PlainInit(tmpDir, false)
		if err != nil {
			t.Fatalf("failed to init repo: %v", err)
		}

		// Create initial commit
		testFile := filepath.Join(tmpDir, "test.txt")
		if err := os.WriteFile(testFile, []byte("test"), 0o644); err != nil {
			t.Fatalf("failed to write test file: %v", err)
		}

		wt, err := repo.Worktree()
		if err != nil {
			t.Fatalf("failed to get worktree: %v", err)
		}

		if _, err := wt.Add("test.txt"); err != nil {
			t.Fatalf("failed to add file: %v", err)
		}

		commitHash, err := wt.Commit("Initial commit", &git.CommitOptions{
			Author: &object.Signature{Name: "Test", Email: "test@example.com"},
		})
		if err != nil {
			t.Fatalf("failed to commit: %v", err)
		}

		// Create and checkout main branch
		ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), commitHash)
		if err := repo.Storer.SetReference(ref); err != nil {
			t.Fatalf("failed to create main branch: %v", err)
		}
		if err := wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("main")}); err != nil {
			t.Fatalf("failed to checkout main: %v", err)
		}

		isDefault, branchName := IsOnDefaultBranch(repo)
		if !isDefault {
			t.Error("IsOnDefaultBranch() = false, want true when on main")
		}
		if branchName != "main" {
			t.Errorf("branchName = %q, want %q", branchName, "main")
		}
	})

	t.Run("returns true when on master", func(t *testing.T) {
		tmpDir := t.TempDir()
		repo, err := git.PlainInit(tmpDir, false)
		if err != nil {
			t.Fatalf("failed to init repo: %v", err)
		}

		// Create initial commit (go-git creates master by default)
		testFile := filepath.Join(tmpDir, "test.txt")
		if err := os.WriteFile(testFile, []byte("test"), 0o644); err != nil {
			t.Fatalf("failed to write test file: %v", err)
		}

		wt, err := repo.Worktree()
		if err != nil {
			t.Fatalf("failed to get worktree: %v", err)
		}

		if _, err := wt.Add("test.txt"); err != nil {
			t.Fatalf("failed to add file: %v", err)
		}

		if _, err := wt.Commit("Initial commit", &git.CommitOptions{
			Author: &object.Signature{Name: "Test", Email: "test@example.com"},
		}); err != nil {
			t.Fatalf("failed to commit: %v", err)
		}

		isDefault, branchName := IsOnDefaultBranch(repo)
		if !isDefault {
			t.Error("IsOnDefaultBranch() = false, want true when on master")
		}
		if branchName != "master" {
			t.Errorf("branchName = %q, want %q", branchName, "master")
		}
	})

	t.Run("returns false when on feature branch", func(t *testing.T) {
		tmpDir := t.TempDir()
		repo, err := git.PlainInit(tmpDir, false)
		if err != nil {
			t.Fatalf("failed to init repo: %v", err)
		}

		// Create initial commit
		testFile := filepath.Join(tmpDir, "test.txt")
		if err := os.WriteFile(testFile, []byte("test"), 0o644); err != nil {
			t.Fatalf("failed to write test file: %v", err)
		}

		wt, err := repo.Worktree()
		if err != nil {
			t.Fatalf("failed to get worktree: %v", err)
		}

		if _, err := wt.Add("test.txt"); err != nil {
			t.Fatalf("failed to add file: %v", err)
		}

		commitHash, err := wt.Commit("Initial commit", &git.CommitOptions{
			Author: &object.Signature{Name: "Test", Email: "test@example.com"},
		})
		if err != nil {
			t.Fatalf("failed to commit: %v", err)
		}

		// Create and checkout feature branch
		if err := wt.Checkout(&git.CheckoutOptions{
			Hash:   commitHash,
			Branch: plumbing.NewBranchReferenceName("feature/test"),
			Create: true,
		}); err != nil {
			t.Fatalf("failed to create feature branch: %v", err)
		}

		isDefault, branchName := IsOnDefaultBranch(repo)
		if isDefault {
			t.Error("IsOnDefaultBranch() = true, want false when on feature branch")
		}
		if branchName != "feature/test" {
			t.Errorf("branchName = %q, want %q", branchName, "feature/test")
		}
	})

	t.Run("returns false for detached HEAD", func(t *testing.T) {
		tmpDir := t.TempDir()
		repo, err := git.PlainInit(tmpDir, false)
		if err != nil {
			t.Fatalf("failed to init repo: %v", err)
		}

		// Create initial commit
		testFile := filepath.Join(tmpDir, "test.txt")
		if err := os.WriteFile(testFile, []byte("test"), 0o644); err != nil {
			t.Fatalf("failed to write test file: %v", err)
		}

		wt, err := repo.Worktree()
		if err != nil {
			t.Fatalf("failed to get worktree: %v", err)
		}

		if _, err := wt.Add("test.txt"); err != nil {
			t.Fatalf("failed to add file: %v", err)
		}

		commitHash, err := wt.Commit("Initial commit", &git.CommitOptions{
			Author: &object.Signature{Name: "Test", Email: "test@example.com"},
		})
		if err != nil {
			t.Fatalf("failed to commit: %v", err)
		}

		// Checkout to detached HEAD state
		if err := wt.Checkout(&git.CheckoutOptions{Hash: commitHash}); err != nil {
			t.Fatalf("failed to checkout detached HEAD: %v", err)
		}

		isDefault, branchName := IsOnDefaultBranch(repo)
		if isDefault {
			t.Error("IsOnDefaultBranch() = true, want false for detached HEAD")
		}
		if branchName != "" {
			t.Errorf("branchName = %q, want empty string for detached HEAD", branchName)
		}
	})
}

// resetProtectedDirsForTest resets the cached protected dirs so tests that
// manipulate the agent registry can get fresh results. Call this in any test
// that registers/unregisters agents and then checks isProtectedPath behavior.
//
//nolint:unused // Intentionally kept as a test utility for future tests that mutate the agent registry.
func resetProtectedDirsForTest() {
	protectedDirsOnce = sync.Once{}
	protectedDirsCache = nil
}

func TestGetGitAuthorFromRepo(t *testing.T) {
	// Cannot use t.Parallel() because subtests use t.Setenv to isolate global git config.

	tests := []struct {
		name        string
		localName   string
		localEmail  string
		globalName  string
		globalEmail string
		wantName    string
		wantEmail   string
	}{
		{
			name:       "both set locally",
			localName:  "Local User",
			localEmail: "local@example.com",
			wantName:   "Local User",
			wantEmail:  "local@example.com",
		},
		{
			name:        "only name set locally falls back to global for email",
			localName:   "Local User",
			globalEmail: "global@example.com",
			wantName:    "Local User",
			wantEmail:   "global@example.com",
		},
		{
			name:       "only email set locally falls back to global for name",
			localEmail: "local@example.com",
			globalName: "Global User",
			wantName:   "Global User",
			wantEmail:  "local@example.com",
		},
		{
			name:        "nothing set locally falls back to global for both",
			globalName:  "Global User",
			globalEmail: "global@example.com",
			wantName:    "Global User",
			wantEmail:   "global@example.com",
		},
		{
			name:      "nothing set anywhere returns defaults",
			wantName:  "Unknown",
			wantEmail: "unknown@local",
		},
		{
			name:        "local takes precedence over global",
			localName:   "Local User",
			localEmail:  "local@example.com",
			globalName:  "Global User",
			globalEmail: "global@example.com",
			wantName:    "Local User",
			wantEmail:   "local@example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Isolate global git config by pointing HOME to a temp dir
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", "")

			// Write global .gitconfig if needed
			if tt.globalName != "" || tt.globalEmail != "" {
				globalCfg := "[user]\n"
				if tt.globalName != "" {
					globalCfg += "\tname = " + tt.globalName + "\n"
				}
				if tt.globalEmail != "" {
					globalCfg += "\temail = " + tt.globalEmail + "\n"
				}
				if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(globalCfg), 0o644); err != nil {
					t.Fatalf("failed to write global gitconfig: %v", err)
				}
			}

			// Create a repo for config resolution
			dir := t.TempDir()
			repo, err := git.PlainInit(dir, false)
			if err != nil {
				t.Fatalf("failed to init repo: %v", err)
			}

			// Set local config if needed
			if tt.localName != "" || tt.localEmail != "" {
				cfg, err := repo.Config()
				if err != nil {
					t.Fatalf("failed to get repo config: %v", err)
				}
				cfg.User.Name = tt.localName
				cfg.User.Email = tt.localEmail
				if err := repo.SetConfig(cfg); err != nil {
					t.Fatalf("failed to set repo config: %v", err)
				}
			}

			gotName, gotEmail := GetGitAuthorFromRepo(repo)
			if gotName != tt.wantName {
				t.Errorf("name = %q, want %q", gotName, tt.wantName)
			}
			if gotEmail != tt.wantEmail {
				t.Errorf("email = %q, want %q", gotEmail, tt.wantEmail)
			}
		})
	}
}

func TestIsProtectedPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path      string
		protected bool
	}{
		{".git", true},
		{".git/objects", true},
		{".entire", true},
		{".entire/metadata/session.json", true},
		{".claude", true},
		{".claude/settings.json", true},
		{".gemini", true},
		{".gemini/settings.json", true},
		{"src/main.go", false},
		{"README.md", false},
		{".gitignore", false},
		{".github/workflows/ci.yml", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			if got := isProtectedPath(tt.path); got != tt.protected {
				t.Errorf("isProtectedPath(%q) = %v, want %v", tt.path, got, tt.protected)
			}
		})
	}
}

func TestIsEmptyRepository(t *testing.T) {
	t.Parallel()
	t.Run("empty repo returns true", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		repo, err := git.PlainInit(dir, false)
		if err != nil {
			t.Fatalf("failed to init repo: %v", err)
		}
		if !IsEmptyRepository(repo) {
			t.Error("IsEmptyRepository() = false, want true for empty repo")
		}
	})

	t.Run("repo with commit returns false", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		repo, err := git.PlainInit(dir, false)
		if err != nil {
			t.Fatalf("failed to init repo: %v", err)
		}

		// Create a commit
		testFile := filepath.Join(dir, "test.txt")
		if err := os.WriteFile(testFile, []byte("content"), 0o644); err != nil {
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

		if IsEmptyRepository(repo) {
			t.Error("IsEmptyRepository() = true, want false for repo with commit")
		}
	})
}
