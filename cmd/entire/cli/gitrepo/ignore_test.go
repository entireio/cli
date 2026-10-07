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

	ignored, err := IgnoredPaths(context.Background(), dir,
		[]string{"debug.log", "build/out.bin", "src/main.go", "tracked.log", "name with space.log"})
	require.NoError(t, err)
	assert.Equal(t, map[string]struct{}{
		"debug.log":           {},
		"build/out.bin":       {},
		"name with space.log": {},
	}, ignored, "a tracked file matching an ignore pattern is not reported")

	none, err := IgnoredPaths(context.Background(), dir, []string{"src/main.go"})
	require.NoError(t, err)
	assert.Empty(t, none)
}
