package gitrepo

import (
	"context"
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
