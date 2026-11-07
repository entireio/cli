package gitrepo

import (
	"context"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIgnoresCase(t *testing.T) {
	// Not parallel: isolateGitConfig uses t.Setenv.
	isolateGitConfig(t)
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
		cmd.Env = EnvWithoutRepoOverrides()
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	git("init", "-q")
	ctx := context.Background()

	for _, value := range []string{"true", "false"} {
		git("config", "core.ignorecase", value)
		got, err := IgnoresCase(ctx, dir)
		require.NoError(t, err)
		require.Equal(t, value == "true", got, "core.ignorecase=%s", value)
	}

	git("config", "--unset", "core.ignorecase")
	got, err := IgnoresCase(ctx, dir)
	require.NoError(t, err)
	require.False(t, got, "unset is git's default, false")
}
