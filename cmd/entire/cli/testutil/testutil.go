// Package testutil provides shared test utilities for both integration and e2e tests.
// This package has no build tags, making it usable by all test packages.
package testutil

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/config"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// RewindPoint mirrors the rewind --list JSON output.
type RewindPoint struct {
	ID               string    `json:"id"`
	Message          string    `json:"message"`
	MetadataDir      string    `json:"metadata_dir"`
	Date             time.Time `json:"date"`
	IsTaskCheckpoint bool      `json:"is_task_checkpoint"`
	ToolUseID        string    `json:"tool_use_id"`
	IsLogsOnly       bool      `json:"is_logs_only"`
	CondensationID   string    `json:"condensation_id"`
}

// InitRepo initializes a git repository in the given directory with test user config.
func InitRepo(t *testing.T, repoDir string) {
	t.Helper()

	repo, err := git.PlainInit(repoDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Configure git user for commits
	cfg, err := repo.Config()
	if err != nil {
		t.Fatalf("failed to get repo config: %v", err)
	}
	cfg.User.Name = "Test User"
	cfg.User.Email = "test@example.com"

	// Disable GPG signing for test commits
	if cfg.Raw == nil {
		cfg.Raw = config.New()
	}
	cfg.Raw.Section("commit").SetOption("gpgsign", "false")

	if err := repo.SetConfig(cfg); err != nil {
		t.Fatalf("failed to set repo config: %v", err)
	}
}

// WriteFile creates a file with the given content in the repo directory.
// It creates parent directories as needed.
func WriteFile(t *testing.T, repoDir, path, content string) {
	t.Helper()

	fullPath := filepath.Join(repoDir, path)

	// Create parent directories
	dir := filepath.Dir(fullPath)
	//nolint:gosec // test code, permissions are intentionally standard
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("failed to create directory %s: %v", dir, err)
	}

	//nolint:gosec // test code, permissions are intentionally standard
	if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write file %s: %v", path, err)
	}
}

// ReadFile reads a file from the repo directory.
func ReadFile(t *testing.T, repoDir, path string) string {
	t.Helper()

	fullPath := filepath.Join(repoDir, path)
	//nolint:gosec // test code, path is from test setup
	data, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("failed to read file %s: %v", path, err)
	}
	return string(data)
}

// TryReadFile reads a file from the repo directory, returning empty string if not found.
func TryReadFile(t *testing.T, repoDir, path string) string {
	t.Helper()

	fullPath := filepath.Join(repoDir, path)
	//nolint:gosec // test code, path is from test setup
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return ""
	}
	return string(data)
}

// FileExists checks if a file exists in the repo directory.
func FileExists(repoDir, path string) bool {
	fullPath := filepath.Join(repoDir, path)
	_, err := os.Stat(fullPath)
	return err == nil
}

// GitAdd stages files for commit.
func GitAdd(t *testing.T, repoDir string, paths ...string) {
	t.Helper()

	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatalf("failed to open git repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	for _, path := range paths {
		if _, err := worktree.Add(path); err != nil {
			t.Fatalf("failed to add file %s: %v", path, err)
		}
	}
}

// GitCommit creates a commit with all staged files.
func GitCommit(t *testing.T, repoDir, message string) {
	t.Helper()

	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatalf("failed to open git repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	_, err = worktree.Commit(message, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}
}

// GitCheckoutNewBranch creates and checks out a new branch.
// Uses git CLI to work around go-git v5 bug with checkout deleting untracked files.
func GitCheckoutNewBranch(t *testing.T, repoDir, branchName string) {
	t.Helper()

	//nolint:noctx // test code, no context needed for git checkout
	cmd := exec.Command("git", "checkout", "-b", branchName)
	cmd.Dir = repoDir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to checkout new branch %s: %v\nOutput: %s", branchName, err, output)
	}
}

// GetHeadHash returns the current HEAD commit hash.
func GetHeadHash(t *testing.T, repoDir string) string {
	t.Helper()

	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatalf("failed to open git repo: %v", err)
	}

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("failed to get HEAD: %v", err)
	}

	return head.Hash().String()
}

// BranchExists checks if a branch exists in the repository.
func BranchExists(t *testing.T, repoDir, branchName string) bool {
	t.Helper()

	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatalf("failed to open git repo: %v", err)
	}

	refs, err := repo.References()
	if err != nil {
		t.Fatalf("failed to get references: %v", err)
	}

	found := false
	//nolint:errcheck,gosec // ForEach callback doesn't return errors we need to handle
	refs.ForEach(func(ref *plumbing.Reference) error {
		if ref.Name().Short() == branchName {
			found = true
		}
		return nil
	})

	return found
}

// GetCommitMessage returns the commit message for the given commit hash.
func GetCommitMessage(t *testing.T, repoDir, hash string) string {
	t.Helper()

	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatalf("failed to open git repo: %v", err)
	}

	commitHash := plumbing.NewHash(hash)
	commit, err := repo.CommitObject(commitHash)
	if err != nil {
		t.Fatalf("failed to get commit %s: %v", hash, err)
	}

	return commit.Message
}

// GetLatestCheckpointIDFromHistory walks backwards from HEAD and returns
// the checkpoint ID from the first commit with an Entire-Checkpoint trailer.
// Returns an error if no checkpoint trailer is found in any commit.
func GetLatestCheckpointIDFromHistory(t *testing.T, repoDir string) (string, error) {
	t.Helper()

	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatalf("failed to open git repo: %v", err)
	}

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("failed to get HEAD: %v", err)
	}

	commitIter, err := repo.Log(&git.LogOptions{From: head.Hash()})
	if err != nil {
		t.Fatalf("failed to iterate commits: %v", err)
	}

	var checkpointID string
	//nolint:errcheck,gosec // ForEach callback returns error to stop iteration
	commitIter.ForEach(func(c *object.Commit) error {
		// Look for Entire-Checkpoint trailer
		for line := range strings.SplitSeq(c.Message, "\n") {
			line = strings.TrimSpace(line)
			if value, found := strings.CutPrefix(line, "Entire-Checkpoint:"); found {
				checkpointID = strings.TrimSpace(value)
				return errors.New("stop iteration")
			}
		}
		return nil
	})

	if checkpointID == "" {
		return "", errors.New("no commit with Entire-Checkpoint trailer found in history")
	}

	return checkpointID, nil
}

// SafeIDPrefix returns first 12 chars of ID or the full ID if shorter.
// Use this when logging checkpoint IDs to avoid index out of bounds panic.
func SafeIDPrefix(id string) string {
	if len(id) >= 12 {
		return id[:12]
	}
	return id
}
