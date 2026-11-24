package gitrepo

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gitOut runs git in dir with the test's isolated environment and returns its
// output.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = withoutPathspecMagicEnv(EnvWithoutRepoOverrides())
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return string(out)
}

func TestCommitPaths_CommitsOnlyThePathsInTheEnvironmentPathStatusesUses(t *testing.T) {
	// Not parallel: isolateGitConfig and the inherited selectors use t.Setenv.
	isolateGitConfig(t)

	dir := initGitlinkRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("x\n"), 0o644))
	gitIn(t, dir, "add", "README.md")
	gitIn(t, dir, "commit", "-q", "-m", "initial")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("other\n"), 0o644))
	gitIn(t, dir, "add", "staged.txt")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".entire", "settings.json"), []byte("{}\n"), 0o644))

	// A shell that exported pathspec magic or repository selectors: PathStatuses
	// ignores both, so the commit must too, or it fails (pathspec magic
	// conflicts with literal pathspecs) or lands in another repository.
	other := initGitlinkRepo(t)
	gitIn(t, other, "commit", "-q", "--allow-empty", "-m", "other")
	otherHead := gitOut(t, other, "rev-parse", "HEAD")
	t.Setenv("GIT_GLOB_PATHSPECS", "1")
	t.Setenv("GIT_ICASE_PATHSPECS", "1")
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(other, ".git", "index"))

	statuses, err := PathStatuses(t.Context(), dir, []string{".entire/settings.json"})
	require.NoError(t, err)
	require.Contains(t, statuses, ".entire/settings.json")

	var out bytes.Buffer
	require.NoError(t, CommitPaths(t.Context(), dir, []string{".entire/settings.json"}, "Enable Entire", &out), out.String())

	assert.Equal(t, "Enable Entire\n\n.entire/settings.json\n", gitOut(t, dir, "show", "--name-only", "--format=%s", "HEAD"))
	assert.Equal(t, "A  staged.txt\n", gitOut(t, dir, "status", "--porcelain"), "staged.txt should stay staged and out of the commit")
	assert.Equal(t, otherHead, gitOut(t, other, "rev-parse", "HEAD"), "the exported GIT_DIR's repository must not change")
}

func TestCommitPaths_CommitsAStagedDeletionWithTheOtherPaths(t *testing.T) {
	// Not parallel: isolateGitConfig uses t.Setenv.
	isolateGitConfig(t)

	dir := initGitlinkRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".claude"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), []byte("{}\n"), 0o644))
	gitIn(t, dir, "add", ".claude/settings.json")
	gitIn(t, dir, "commit", "-q", "-m", "initial")
	gitIn(t, dir, "rm", "-q", ".claude/settings.json")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".entire", "settings.json"), []byte("{}\n"), 0o644))

	paths := []string{".claude/settings.json", ".entire/settings.json"}
	var out bytes.Buffer
	require.NoError(t, CommitPaths(t.Context(), dir, paths, "Enable Entire", &out), out.String())

	assert.Equal(t, "D\t.claude/settings.json\nA\t.entire/settings.json\n", gitOut(t, dir, "show", "--name-status", "--no-renames", "--format=", "HEAD"))
	assert.Empty(t, gitOut(t, dir, "status", "--porcelain"))
}
