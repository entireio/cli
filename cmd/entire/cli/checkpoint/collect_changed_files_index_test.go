package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// TestCollectChangedFiles_DoesNotRewriteUserIndex is the regression test for
// issue #2111: a repository-deleting commit, caused by this hook-path helper
// replacing the user's .git/index behind their back.
//
// `git status` is not a read. It refreshes the index's stat cache and, when
// anything is stale, writes the result back — builtin/commit.c takes
// .git/index.lock for the whole worktree walk and renames a fresh index over
// .git/index (tempfile.c, rename(2)). The replacement gives .git/index a new
// inode, and on a filesystem where rename-over-existing is not atomic against a
// concurrent lookup (measured on a virtiofs bind mount: 9.9% of opens during
// continuous replacement, 0 on ext4) a concurrent reader gets ENOENT. Git
// silently treats ENOENT on the index — and only ENOENT — as an empty index, so
// a `git commit` landing in that window records the empty tree with exit code 0
// and no warning.
//
// Entire only ever wants the porcelain output, so the write is pure collateral
// and --no-optional-locks removes it with byte-identical output. This test
// asserts the file identity is preserved, which is the property that matters:
// no replacement means no window.
func TestCollectChangedFiles_DoesNotRewriteUserIndex(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		testutil.WriteFile(t, repoDir, name, "initial\n")
		testutil.GitAdd(t, repoDir, name)
	}
	testutil.GitCommit(t, repoDir, "init")

	repo, err := gitrepo.OpenPath(repoDir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()

	indexPath := filepath.Join(repoDir, ".git", "index")

	// Make the stat cache stale without changing content. This is the state git
	// writes the index back for, and it is the ordinary aftermath of an agent
	// turn, a formatter, or an editor save.
	stale := time.Now().Add(2 * time.Second)
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.Chtimes(filepath.Join(repoDir, name), stale, stale); err != nil {
			t.Fatalf("chtimes %s: %v", name, err)
		}
	}

	before, err := os.Stat(indexPath)
	if err != nil {
		t.Fatalf("stat index before: %v", err)
	}

	if _, err := collectChangedFiles(context.Background(), repo); err != nil {
		t.Fatalf("collectChangedFiles: %v", err)
	}

	after, err := os.Stat(indexPath)
	if err != nil {
		t.Fatalf("stat index after: %v", err)
	}

	if !os.SameFile(before, after) {
		t.Error("collectChangedFiles replaced the user's .git/index (new inode); " +
			"the git status subprocess must pass --no-optional-locks so it never " +
			"takes index.lock or renames a new index into place (issue #2111)")
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("collectChangedFiles rewrote the user's .git/index: mtime %v -> %v",
			before.ModTime(), after.ModTime())
	}
}

// TestCollectChangedFiles_IgnoresInheritedGitEnv proves the subprocess resolves
// the repository from cmd.Dir rather than from an inherited GIT_DIR.
//
// Git exports GIT_DIR (and sometimes GIT_WORK_TREE / GIT_INDEX_FILE) to its
// hooks, and those variables take precedence over the child's working
// directory — so a bare exec.Command inherits them and silently operates on the
// hook's repo instead of the one cmd.Dir names. Two other call sites in the
// codebase already strip them; this one did not.
func TestCollectChangedFiles_IgnoresInheritedGitEnv(t *testing.T) {
	// Cannot be parallel: t.Setenv is process-global.
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "tracked.txt", "initial\n")
	testutil.GitAdd(t, repoDir, "tracked.txt")
	testutil.GitCommit(t, repoDir, "init")
	testutil.WriteFile(t, repoDir, "untracked.txt", "new\n")

	// A decoy repo, as a git hook's exported GIT_DIR would name.
	decoyDir := t.TempDir()
	testutil.InitRepo(t, decoyDir)
	testutil.WriteFile(t, decoyDir, "decoy.txt", "decoy\n")

	t.Setenv("GIT_DIR", filepath.Join(decoyDir, ".git"))
	t.Setenv("GIT_WORK_TREE", decoyDir)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(decoyDir, ".git", "index"))

	repo, err := gitrepo.OpenPath(repoDir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()

	result, err := collectChangedFiles(context.Background(), repo)
	if err != nil {
		t.Fatalf("collectChangedFiles: %v", err)
	}

	var sawReal bool
	for _, f := range result.Changed {
		if f == "untracked.txt" {
			sawReal = true
		}
		if f == "decoy.txt" {
			t.Errorf("resolved the decoy repo from the inherited GIT_DIR instead of "+
				"cmd.Dir; got %v", result.Changed)
		}
	}
	if !sawReal {
		t.Errorf("expected untracked.txt from the real repo, got %v", result.Changed)
	}
}
