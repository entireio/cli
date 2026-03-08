//go:build integration

package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6/plumbing"
)

// TestWorktreeOpenRepository verifies that OpenRepository() works correctly
// in a worktree context by checking it can read HEAD and refs.
//
// NOTE: This test uses os.Chdir() so it cannot use t.Parallel().
func TestWorktreeOpenRepository(t *testing.T) {
	env := NewTestEnv(t)
	env.InitRepo()

	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	worktreeDir := filepath.Join(t.TempDir(), "worktree")
	if resolved, err := filepath.EvalSymlinks(filepath.Dir(worktreeDir)); err == nil {
		worktreeDir = filepath.Join(resolved, "worktree")
	}

	cmd := exec.Command("git", "worktree", "add", worktreeDir, "-b", "test-branch")
	cmd.Dir = env.RepoDir
	cmd.Env = testutil.GitIsolatedEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to create worktree: %v\nOutput: %s", err, output)
	}

	originalWd, _ := os.Getwd()
	if err := os.Chdir(worktreeDir); err != nil {
		t.Fatalf("failed to chdir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(originalWd)
	})

	repo, err := strategy.OpenRepository(context.Background())
	if err != nil {
		t.Fatalf("OpenRepository() failed in worktree: %v", err)
	}

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("repo.Head() failed: %v", err)
	}

	if head.Name().Short() != "test-branch" {
		t.Errorf("expected HEAD to be test-branch, got %s", head.Name().Short())
	}

	refs, err := repo.References()
	if err != nil {
		t.Fatalf("repo.References() failed: %v", err)
	}

	refCount := 0
	_ = refs.ForEach(func(ref *plumbing.Reference) error {
		refCount++
		return nil
	})

	if refCount == 0 {
		t.Error("expected to find refs, but found none")
	}

	t.Logf("Successfully opened worktree repo, HEAD=%s, found %d refs",
		head.Name().Short(), refCount)
}
