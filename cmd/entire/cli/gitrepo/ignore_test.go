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

func TestIgnoredPaths(t *testing.T) {
	t.Parallel()
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
