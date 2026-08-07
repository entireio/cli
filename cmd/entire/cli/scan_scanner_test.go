package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// scanRepoRoots reduces candidates to their roots for order-sensitive asserts.
func scanRepoRoots(candidates []scanCandidate) []string {
	roots := make([]string, len(candidates))
	for i, c := range candidates {
		roots[i] = c.Root
	}
	return roots
}

// resolvedTempDir returns a t.TempDir() with symlinks resolved, matching what
// `git rev-parse --show-toplevel` reports (macOS /var -> /private/var).
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	return resolved
}

func TestFindGitRepos_FindsSiblingRepos(t *testing.T) {
	t.Parallel()

	dev := resolvedTempDir(t)
	for _, name := range []string{"alpha", "beta", "gamma"} {
		repo := filepath.Join(dev, name)
		require.NoError(t, os.MkdirAll(repo, 0o755))
		testutil.InitRepo(t, repo)
	}
	// A plain directory next to them must not be reported.
	require.NoError(t, os.MkdirAll(filepath.Join(dev, "notes"), 0o755))

	got, err := findGitRepos(context.Background(), []string{dev}, 2)
	require.NoError(t, err)
	require.Equal(t, []string{
		filepath.Join(dev, "alpha"),
		filepath.Join(dev, "beta"),
		filepath.Join(dev, "gamma"),
	}, scanRepoRoots(got), "repos are returned sorted by path")
	for _, c := range got {
		require.False(t, c.LinkedWorktree)
	}
}

func TestFindGitRepos_ScanRootItselfIsARepo(t *testing.T) {
	t.Parallel()

	repo := resolvedTempDir(t)
	testutil.InitRepo(t, repo)

	got, err := findGitRepos(context.Background(), []string{repo}, 2)
	require.NoError(t, err)
	require.Equal(t, []string{repo}, scanRepoRoots(got))
}

func TestFindGitRepos_DoesNotDescendIntoRepos(t *testing.T) {
	t.Parallel()

	dev := resolvedTempDir(t)
	outer := filepath.Join(dev, "outer")
	require.NoError(t, os.MkdirAll(outer, 0o755))
	testutil.InitRepo(t, outer)

	// A vendored/nested repo inside a repo: never reported, because the outer
	// repo is a candidate and candidates are not descended into.
	nested := filepath.Join(outer, "vendor", "nested")
	require.NoError(t, os.MkdirAll(nested, 0o755))
	testutil.InitRepo(t, nested)

	got, err := findGitRepos(context.Background(), []string{dev}, 5)
	require.NoError(t, err)
	require.Equal(t, []string{outer}, scanRepoRoots(got))
}

func TestFindGitRepos_SkipsDotDirsAndNodeModules(t *testing.T) {
	t.Parallel()

	dev := resolvedTempDir(t)
	for _, parent := range []string{".hidden", "node_modules"} {
		repo := filepath.Join(dev, parent, "pkg")
		require.NoError(t, os.MkdirAll(repo, 0o755))
		testutil.InitRepo(t, repo)
	}
	visible := filepath.Join(dev, "visible")
	require.NoError(t, os.MkdirAll(visible, 0o755))
	testutil.InitRepo(t, visible)

	got, err := findGitRepos(context.Background(), []string{dev}, 5)
	require.NoError(t, err)
	require.Equal(t, []string{visible}, scanRepoRoots(got))
}

func TestFindGitRepos_SkipsBareRepos(t *testing.T) {
	t.Parallel()

	dev := resolvedTempDir(t)

	// A bare repo laid out as <dir>/.git looks like a candidate on disk but has
	// no working tree, so there is nothing to enable Entire in.
	holder := filepath.Join(dev, "holder")
	require.NoError(t, os.MkdirAll(holder, 0o755))
	runScanTestGit(t, dev, "init", "--bare", filepath.Join(holder, ".git"))

	// A conventional bare clone (mirror.git) is not even a candidate: it has no
	// .git entry of its own.
	runScanTestGit(t, dev, "init", "--bare", filepath.Join(dev, "mirror.git"))

	work := filepath.Join(dev, "work")
	require.NoError(t, os.MkdirAll(work, 0o755))
	testutil.InitRepo(t, work)

	got, err := findGitRepos(context.Background(), []string{dev}, 2)
	require.NoError(t, err)
	require.Equal(t, []string{work}, scanRepoRoots(got))
}

func TestFindGitRepos_FlagsLinkedWorktrees(t *testing.T) {
	t.Parallel()

	dev := resolvedTempDir(t)
	main := filepath.Join(dev, "main")
	require.NoError(t, os.MkdirAll(main, 0o755))
	testutil.InitRepo(t, main)
	testutil.WriteFile(t, main, "f.txt", "hello")
	testutil.GitAdd(t, main, "f.txt")
	testutil.GitCommit(t, main, "init")

	linked := filepath.Join(dev, "feature")
	runScanTestGit(t, main, "worktree", "add", "-q", linked, "-b", "feature")

	// The linked worktree is a .git *file*, not a directory.
	info, err := os.Lstat(filepath.Join(linked, ".git"))
	require.NoError(t, err)
	require.False(t, info.IsDir(), "precondition: linked worktree uses a gitfile")

	got, err := findGitRepos(context.Background(), []string{dev}, 2)
	require.NoError(t, err)
	require.Equal(t, []string{linked, main}, scanRepoRoots(got))
	require.True(t, got[0].LinkedWorktree, "feature is a linked worktree")
	require.False(t, got[1].LinkedWorktree, "main is not a linked worktree")
}

func TestFindGitRepos_DoesNotFollowDirectorySymlinks(t *testing.T) {
	t.Parallel()

	dev := resolvedTempDir(t)
	outside := resolvedTempDir(t)
	repo := filepath.Join(outside, "hidden-repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	testutil.InitRepo(t, repo)

	require.NoError(t, os.Symlink(outside, filepath.Join(dev, "link")))

	got, err := findGitRepos(context.Background(), []string{dev}, 5)
	require.NoError(t, err)
	require.Empty(t, got, "symlinked directories are not traversed")
}

func TestFindGitRepos_RespectsDepthCap(t *testing.T) {
	t.Parallel()

	dev := resolvedTempDir(t)
	shallow := filepath.Join(dev, "a", "shallow")
	require.NoError(t, os.MkdirAll(shallow, 0o755))
	testutil.InitRepo(t, shallow)

	deep := filepath.Join(dev, "a", "b", "deep")
	require.NoError(t, os.MkdirAll(deep, 0o755))
	testutil.InitRepo(t, deep)

	atDepth2, err := findGitRepos(context.Background(), []string{dev}, 2)
	require.NoError(t, err)
	require.Equal(t, []string{shallow}, scanRepoRoots(atDepth2))

	atDepth3, err := findGitRepos(context.Background(), []string{dev}, 3)
	require.NoError(t, err)
	require.Equal(t, []string{deep, shallow}, scanRepoRoots(atDepth3))

	atDepth1, err := findGitRepos(context.Background(), []string{dev}, 1)
	require.NoError(t, err)
	require.Empty(t, atDepth1, "depth 1 only inspects the root's direct children")
}

func TestFindGitRepos_DedupesRepeatedRoots(t *testing.T) {
	t.Parallel()

	dev := resolvedTempDir(t)
	repo := filepath.Join(dev, "solo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	testutil.InitRepo(t, repo)

	got, err := findGitRepos(context.Background(), []string{dev, dev, repo}, 2)
	require.NoError(t, err)
	require.Equal(t, []string{repo}, scanRepoRoots(got))
}

func TestFindGitRepos_SkipsUnreadableRoots(t *testing.T) {
	t.Parallel()

	dev := resolvedTempDir(t)
	repo := filepath.Join(dev, "solo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	testutil.InitRepo(t, repo)

	got, err := findGitRepos(context.Background(), []string{filepath.Join(dev, "does-not-exist"), dev}, 2)
	require.NoError(t, err, "a missing root is skipped, not fatal")
	require.Equal(t, []string{repo}, scanRepoRoots(got))
}

// runScanTestGit runs git with the repo-isolated environment the testutil
// helpers use, so a developer's global git config cannot change the outcome.
func runScanTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}
