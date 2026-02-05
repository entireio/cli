package gitutil

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

// gitCheckoutInDir uses git CLI to checkout a ref in a specific directory.
func gitCheckoutInDir(t *testing.T, dir, ref string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "checkout", ref)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Failed to checkout %s: %v\nOutput: %s", ref, err, output)
	}
}

func TestGetCurrentBranch(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("Failed to init repo: %v", err)
	}

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

	featureRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("feature"), commit)
	if err := repo.Storer.SetReference(featureRef); err != nil {
		t.Fatalf("Failed to create feature branch: %v", err)
	}

	gitCheckoutInDir(t, tmpDir, "feature")

	branch, err := GetCurrentBranch()
	if err != nil {
		t.Fatalf("GetCurrentBranch() error = %v", err)
	}
	if branch != "feature" {
		t.Errorf("GetCurrentBranch() = %v, want feature", branch)
	}
}

func TestGetCurrentBranchDetachedHead(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("Failed to init repo: %v", err)
	}

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

	gitCheckoutInDir(t, tmpDir, commit.String())

	_, err = GetCurrentBranch()
	if err == nil {
		t.Error("GetCurrentBranch() expected error for detached HEAD, got nil")
	}
}

func TestGetMergeBase(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("Failed to init repo: %v", err)
	}

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Failed to get worktree: %v", err)
	}

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

	mainRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), baseCommit)
	if err := repo.Storer.SetReference(mainRef); err != nil {
		t.Fatalf("Failed to create main branch: %v", err)
	}

	featureRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("feature"), baseCommit)
	if err := repo.Storer.SetReference(featureRef); err != nil {
		t.Fatalf("Failed to create feature branch: %v", err)
	}

	gitCheckoutInDir(t, tmpDir, "feature")
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

	mergeBase, err := GetMergeBase("feature", "main")
	if err != nil {
		t.Fatalf("GetMergeBase() error = %v", err)
	}
	if mergeBase.String() != baseCommit.String() {
		t.Errorf("GetMergeBase() = %v, want %v", mergeBase, baseCommit)
	}
}

func TestGetMergeBaseNonExistentBranch(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("Failed to init repo: %v", err)
	}

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

	_, err = GetMergeBase("feature", "nonexistent")
	if err == nil {
		t.Error("GetMergeBase() expected error for nonexistent branch, got nil")
	}
}

func TestGetGitAuthorReturnsAuthor(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("Failed to init repo: %v", err)
	}

	cfg, err := repo.Config()
	if err != nil {
		t.Fatalf("Failed to get repo config: %v", err)
	}
	cfg.User.Name = "Test Author"
	cfg.User.Email = "test@example.com"
	if err := repo.SetConfig(cfg); err != nil {
		t.Fatalf("Failed to set repo config: %v", err)
	}

	author, err := GetGitAuthor()
	if err != nil {
		t.Fatalf("GetGitAuthor() error = %v", err)
	}

	if author.Name != "Test Author" {
		t.Errorf("GetGitAuthor().Name = %q, want %q", author.Name, "Test Author")
	}
	if author.Email != "test@example.com" {
		t.Errorf("GetGitAuthor().Email = %q, want %q", author.Email, "test@example.com")
	}
}

func TestGetGitAuthorFallsBackToGitCommand(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	_, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("Failed to init repo: %v", err)
	}

	author, err := GetGitAuthor()
	if err != nil {
		t.Fatalf("GetGitAuthor() should not error, got: %v", err)
	}

	if author == nil {
		t.Fatal("GetGitAuthor() returned nil author")
	}

	t.Logf("GetGitAuthor() returned Name=%q, Email=%q", author.Name, author.Email)
}

func TestGetGitAuthorReturnsDefaultsWhenNoConfig(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	_, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("Failed to init repo: %v", err)
	}

	author, err := GetGitAuthor()
	if err != nil {
		t.Fatalf("GetGitAuthor() should not error even without config, got: %v", err)
	}

	if author == nil {
		t.Fatal("GetGitAuthor() returned nil")
	}

	if author.Name == "" {
		t.Error("GetGitAuthor().Name is empty, expected a value or default")
	}
	if author.Email == "" {
		t.Error("GetGitAuthor().Email is empty, expected a value or default")
	}
}

func TestBranchExistsOnRemote(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("Failed to init repo: %v", err)
	}

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
		t.Fatalf("Failed to commit: %v", err)
	}

	remoteRef := plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", "feature"), commit)
	if err := repo.Storer.SetReference(remoteRef); err != nil {
		t.Fatalf("Failed to create remote ref: %v", err)
	}

	t.Run("returns true when branch exists on remote", func(t *testing.T) {
		exists, err := BranchExistsOnRemote("feature")
		if err != nil {
			t.Fatalf("BranchExistsOnRemote() error = %v", err)
		}
		if !exists {
			t.Error("BranchExistsOnRemote() = false, want true for existing remote branch")
		}
	})

	t.Run("returns false when branch does not exist on remote", func(t *testing.T) {
		exists, err := BranchExistsOnRemote("nonexistent")
		if err != nil {
			t.Fatalf("BranchExistsOnRemote() error = %v", err)
		}
		if exists {
			t.Error("BranchExistsOnRemote() = true, want false for nonexistent remote branch")
		}
	})
}
