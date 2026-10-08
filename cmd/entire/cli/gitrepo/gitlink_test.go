package gitrepo

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gitIn runs git in dir with the test's (isolated) environment and fails the
// test on error.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = EnvWithoutRepoOverrides()
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

// initGitlinkRepo creates a repository with a local identity so commits work
// with the process's git config isolated.
func initGitlinkRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	gitIn(t, dir, "config", "user.name", "Test User")
	gitIn(t, dir, "config", "user.email", "test@example.com")
	gitIn(t, dir, "config", "commit.gpgsign", "false")
	return dir
}

func TestGitlinkPaths(t *testing.T) {
	// Not parallel: isolateGitConfig uses t.Setenv.
	isolateGitConfig(t)

	subSrc := initGitlinkRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(subSrc, "lib.txt"), []byte("v1\n"), 0o644))
	gitIn(t, subSrc, "add", "lib.txt")
	gitIn(t, subSrc, "commit", "-q", "-m", "lib v1")

	dir := initGitlinkRepo(t)

	// Unborn HEAD: a submodule that is only staged is found through the index.
	gitIn(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", subSrc, "staged-only")
	found, err := GitlinkPaths(context.Background(), dir, []string{"staged-only", ".gitmodules", "missing"})
	require.NoError(t, err)
	assert.Equal(t, map[string]struct{}{"staged-only": {}}, found, "unborn HEAD: the index is the only source")

	// A committed submodule in a nested directory, regular files, and a file
	// whose name is a glob that would match the submodule as a pattern.
	gitIn(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", subSrc, "vendor/committed")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "regular.txt"), []byte("x\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "vendor"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "vendor", "c*"), []byte("glob\n"), 0o644))
	gitIn(t, dir, "add", "regular.txt", "vendor/c*")
	gitIn(t, dir, "commit", "-q", "-m", "submodules and files")
	// Drop the committed submodule from the index only: HEAD still has it.
	gitIn(t, dir, "rm", "-q", "--cached", "vendor/committed")

	found, err = GitlinkPaths(context.Background(), dir,
		[]string{"vendor/committed", "staged-only", "regular.txt", "vendor/c*", "vendor", "missing"})
	require.NoError(t, err)
	assert.Equal(t, map[string]struct{}{
		"vendor/committed": {},
		"staged-only":      {},
	}, found, "HEAD-only and index gitlinks count; files, a glob-named file, and a directory do not")

	// Pathspecs are literal: "staged-*" names only itself, not a pattern that
	// would also match the staged-only gitlink in the index.
	globOnly, err := GitlinkPaths(context.Background(), dir, []string{"staged-*", "vendor/c*"})
	require.NoError(t, err)
	assert.Empty(t, globOnly)

	empty, err := GitlinkPaths(context.Background(), dir, nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

func TestPathsStagedAsNew(t *testing.T) {
	// Not parallel: isolateGitConfig uses t.Setenv.
	isolateGitConfig(t)
	dir := initGitlinkRepo(t)

	// Unborn HEAD: a staged file is staged as new, and so is one removed from
	// the worktree after staging; an untracked file, an intent-to-add entry
	// (no blob), and a glob-named pathspec are not.
	for _, name := range []string{"staged.go", "gone.go", "untracked.go", "intent.go"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o644))
	}
	gitIn(t, dir, "add", "staged.go", "gone.go")
	gitIn(t, dir, "add", "-N", "intent.go")
	require.NoError(t, os.Remove(filepath.Join(dir, "gone.go")))

	query := []string{"staged.go", "gone.go", "untracked.go", "intent.go", "missing", "*.go"}
	found, err := PathsStagedAsNew(context.Background(), dir, query)
	require.NoError(t, err)
	assert.Equal(t, map[string]struct{}{"staged.go": {}, "gone.go": {}}, found)

	// Once committed, a file is no longer new; a modified tracked file is not
	// either.
	gitIn(t, dir, "commit", "-q", "-m", "base")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "staged.go"), []byte("changed\n"), 0o644))
	gitIn(t, dir, "add", "staged.go")
	found, err = PathsStagedAsNew(context.Background(), dir, query)
	require.NoError(t, err)
	assert.Empty(t, found)

	empty, err := PathsStagedAsNew(context.Background(), dir, nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// A conflicting pathspec-magic variable in the caller's environment must not
// make the literal-pathspec classifiers fail: git refuses GIT_LITERAL_PATHSPECS
// combined with GIT_GLOB_PATHSPECS, GIT_NOGLOB_PATHSPECS or GIT_ICASE_PATHSPECS.
func TestLiteralPathspecCommand_IgnoresConflictingPathspecEnv(t *testing.T) {
	// Not parallel: t.Setenv.
	isolateGitConfig(t)
	dir := initGitlinkRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644))
	gitIn(t, dir, "add", "a.txt")
	gitIn(t, dir, "commit", "-q", "-m", "a")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b\n"), 0o644))
	gitIn(t, dir, "add", "b.txt")

	for _, name := range []string{"GIT_GLOB_PATHSPECS", "GIT_NOGLOB_PATHSPECS", "GIT_ICASE_PATHSPECS"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "1")
			inHead, err := PathsInHEAD(context.Background(), dir, []string{"a.txt"})
			require.NoError(t, err)
			assert.Contains(t, inHead, "a.txt")
			staged, err := PathsStagedAsNew(context.Background(), dir, []string{"b.txt"})
			require.NoError(t, err)
			assert.Contains(t, staged, "b.txt")
		})
	}
}

func TestPathStatuses(t *testing.T) {
	// Not parallel: isolateGitConfig uses t.Setenv.
	isolateGitConfig(t)
	dir := initGitlinkRepo(t)
	for _, name := range []string{"clean.txt", "modified.txt", "deleted.txt", "staged.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o644))
	}
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-q", "-m", "base")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "modified.txt"), []byte("changed\n"), 0o644))
	require.NoError(t, os.Remove(filepath.Join(dir, "deleted.txt")))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("staged\n"), 0o644))
	gitIn(t, dir, "add", "staged.txt")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "new file.txt"), []byte("new\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "*.txt"), []byte("glob\n"), 0o644))

	got, err := PathStatuses(context.Background(), dir,
		[]string{"clean.txt", "modified.txt", "deleted.txt", "staged.txt", "new file.txt", "missing.txt"})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"modified.txt": " M",
		"deleted.txt":  " D",
		"staged.txt":   "M ",
		"new file.txt": "??",
	}, got, "a clean or unknown path is absent; pathspecs are literal, so *.txt is not matched")

	empty, err := PathStatuses(context.Background(), dir, nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// A large committed directory named as an ancestor must not make the index
// query list its subtree: diff-index reports only index entries that differ
// from HEAD, so an unchanged directory produces no output at all.
func TestGitlinkPaths_IndexQueryIsBoundedByDifferences(t *testing.T) {
	// Not parallel: isolateGitConfig uses t.Setenv.
	isolateGitConfig(t)
	dir := initGitlinkRepo(t)
	for i := range 300 {
		sub := filepath.Join(dir, "packages", fmt.Sprintf("p%03d", i))
		require.NoError(t, os.MkdirAll(sub, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(sub, "f.txt"), []byte("x\n"), 0o644))
	}
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-q", "-m", "monorepo")

	query := []string{"packages", "packages/p007", "packages/p007/f.txt"}
	out, err := literalPathspecCommand(t.Context(), dir, query, "diff-index", "--cached", "--raw", "-z", "--no-renames", "--ignore-submodules=none", "HEAD").Output()
	require.NoError(t, err)
	assert.Empty(t, out, "an unchanged directory contributes nothing to the index query")

	found, err := GitlinkPaths(context.Background(), dir, query)
	require.NoError(t, err)
	assert.Empty(t, found)
}

// A staged, not yet committed submodule is a gitlink even when .gitmodules
// sets `ignore = all` for it; that setting must not hide the index record.
func TestGitlinkPaths_StagedSubmoduleWithIgnoreAll(t *testing.T) {
	// Not parallel: isolateGitConfig uses t.Setenv.
	isolateGitConfig(t)

	subSrc := initGitlinkRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(subSrc, "lib.txt"), []byte("v1\n"), 0o644))
	gitIn(t, subSrc, "add", "lib.txt")
	gitIn(t, subSrc, "commit", "-q", "-m", "lib v1")

	dir := initGitlinkRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644))
	gitIn(t, dir, "add", "a.txt")
	gitIn(t, dir, "commit", "-q", "-m", "init")

	gitIn(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", subSrc, "sub")
	gitIn(t, dir, "config", "-f", ".gitmodules", "submodule.sub.ignore", "all")
	gitIn(t, dir, "add", ".gitmodules")

	found, err := GitlinkPaths(context.Background(), dir, []string{"sub"})
	require.NoError(t, err)
	assert.Equal(t, map[string]struct{}{"sub": {}}, found,
		"diff-index honours submodule.<name>.ignore=all and drops the staged gitlink; pass --ignore-submodules=none")
}
