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

// isolateGitConfig keeps the developer's global and system git config — in
// particular a global excludes file — out of git subprocesses for the rest of
// the test, so ignore results depend only on the fixture's own .gitignore.
// gitrepo has no process-wide isolation (no TestMain), so this is done per
// test with t.Setenv, which is why these tests cannot run in parallel.
func isolateGitConfig(t *testing.T) {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "empty-gitconfig")
	require.NoError(t, os.WriteFile(empty, nil, 0o600))
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

func TestIgnoredPaths(t *testing.T) {
	// Not parallel: isolateGitConfig uses t.Setenv.
	isolateGitConfig(t)
	dir := t.TempDir()
	initCmd := exec.CommandContext(t.Context(), "git", "init", "-q", dir)
	initCmd.Env = EnvWithoutRepoOverrides()
	require.NoError(t, initCmd.Run())
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.log\nbuild/\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tracked.log"), []byte("x"), 0o644))
	add := exec.CommandContext(t.Context(), "git", "-C", dir, "add", "-f", "tracked.log")
	add.Env = EnvWithoutRepoOverrides()
	require.NoError(t, add.Run())

	ignored, refused, err := IgnoredPaths(context.Background(), dir,
		[]string{"debug.log", "build/out.bin", "src/main.go", "tracked.log", "name with space.log"})
	require.NoError(t, err)
	assert.Empty(t, refused)
	assert.Equal(t, map[string]struct{}{
		"debug.log":           {},
		"build/out.bin":       {},
		"name with space.log": {},
	}, ignored, "a tracked file matching an ignore pattern is not reported")

	none, _, err := IgnoredPaths(context.Background(), dir, []string{"src/main.go"})
	require.NoError(t, err)
	assert.Empty(t, none)
}

// git check-ignore exits 128 for the whole batch when one path is one it
// refuses (here a path inside a submodule), after printing some answers. The
// batch is retried per path: the refused path is reported as refused and every
// other answer is kept.
func TestIgnoredPaths_RetriesPerPathAndReportsRefused(t *testing.T) {
	// Not parallel: isolateGitConfig uses t.Setenv.
	isolateGitConfig(t)
	subSrc := initGitlinkRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(subSrc, "lib.txt"), []byte("v1\n"), 0o644))
	gitIn(t, subSrc, "add", "lib.txt")
	gitIn(t, subSrc, "commit", "-q", "-m", "lib")
	dir := initGitlinkRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.env\n"), 0o644))
	gitIn(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", subSrc, "sub")

	ignored, refused, err := IgnoredPaths(context.Background(), dir,
		[]string{"agent.txt", "ignored.env", "sub/lib.txt", "other.env"})
	require.NoError(t, err)
	assert.Equal(t, map[string]struct{}{"ignored.env": {}, "other.env": {}}, ignored)
	assert.Equal(t, map[string]struct{}{"sub/lib.txt": {}}, refused)
}

// When git cannot answer at all (not a repository), nothing is reported as
// refused: the error is returned so the caller keeps every path.
func TestIgnoredPaths_SystemicFailureIsAnError(t *testing.T) {
	// Not parallel: isolateGitConfig uses t.Setenv.
	isolateGitConfig(t)
	notARepo := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(notARepo))
	_, refused, err := IgnoredPaths(context.Background(), notARepo, []string{"a.txt", "b.txt"})
	require.Error(t, err)
	assert.Empty(t, refused)
}
