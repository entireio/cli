package paths_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// initSeparateGitDirRepo creates <base>/repo with `git init --separate-git-dir
// <base>/gitdir` plus one commit, returning the repo and git dir paths. This
// is the layout whose .git file matches no lexical linked-worktree marker.
func initSeparateGitDirRepo(t *testing.T) (repoDir, gitDir string) {
	t.Helper()
	base := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(base); err == nil {
		base = resolved
	}
	repoDir = filepath.Join(base, "repo")
	gitDir = filepath.Join(base, "gitdir")
	runGit(t, base, "init", "--separate-git-dir", gitDir, repoDir)
	runGit(t, repoDir, "config", "user.email", "test@entire.io")
	runGit(t, repoDir, "config", "user.name", "Test")
	runGit(t, repoDir, "config", "commit.gpgsign", "false")
	runGit(t, repoDir, "commit", "--allow-empty", "-m", "init")
	return repoDir, gitDir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestGetWorktreeID_SeparateGitDir pins the classification fix for
// `git init --separate-git-dir`: the MAIN worktree of such a repo has a .git
// FILE whose gitdir matches no lexical linked-worktree marker, and its
// correct worktree ID is "" — this must not be an error (it previously was,
// which made invisible routing fall open into the worktree).
func TestGetWorktreeID_SeparateGitDir(t *testing.T) {
	t.Parallel()
	repoDir, _ := initSeparateGitDirRepo(t)

	id, err := paths.GetWorktreeID(repoDir)
	if err != nil {
		t.Fatalf("GetWorktreeID(main worktree with separate git dir) error: %v", err)
	}
	if id != "" {
		t.Errorf("GetWorktreeID = %q, want \"\" (main worktree)", id)
	}
}

// TestGetWorktreeID_SeparateGitDir_LinkedWorktree covers the linked worktree
// of a separate-git-dir repo: its admin dir is <sepdir>/worktrees/<id>, which
// also matches no lexical marker (.git/worktrees/ or .bare/worktrees/), so
// the on-disk classification must recover the ID.
func TestGetWorktreeID_SeparateGitDir_LinkedWorktree(t *testing.T) {
	t.Parallel()
	repoDir, gitDir := initSeparateGitDirRepo(t)
	linked := filepath.Join(filepath.Dir(repoDir), "wt-linked")
	runGit(t, repoDir, "worktree", "add", linked)

	id, err := paths.GetWorktreeID(linked)
	if err != nil {
		t.Fatalf("GetWorktreeID(linked worktree of separate git dir): %v", err)
	}
	if id != "wt-linked" {
		t.Errorf("GetWorktreeID = %q, want %q (admin dir %s/worktrees/wt-linked)", id, "wt-linked", gitDir)
	}
}
