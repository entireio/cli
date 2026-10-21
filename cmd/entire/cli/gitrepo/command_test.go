package gitrepo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A hung clean filter must not outlive the caller's context: hashing runs on
// agent hook paths with a time budget. The filter here sleeps far longer than
// the deadline; HashWorktreeFiles must return promptly with an error instead
// of waiting for it.
func TestHashWorktreeFiles_HungCleanFilterDiesOnCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture's clean filter is a POSIX sleep")
	}
	// Not parallel: isolateGitConfig uses t.Setenv.
	isolateGitConfig(t)
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
		cmd.Env = EnvWithoutRepoOverrides()
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	run("init", "-q")
	run("config", "filter.slow.clean", "sleep 60")
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("*.txt filter=slow\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x\n"), 0o644))

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	hashes, err := HashWorktreeFiles(ctx, dir, []string{"a.txt"})
	require.Error(t, err)
	require.Empty(t, hashes, "a killed hash-object yields no hash")
	require.Less(t, time.Since(start), 5*time.Second, "the filter's process group must be killed on cancellation")
}
