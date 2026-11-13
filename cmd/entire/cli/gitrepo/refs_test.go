package gitrepo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func headOf(t *testing.T, dir string) plumbing.Hash {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "-C", dir, "rev-parse", "HEAD")
	cmd.Env = EnvWithoutRepoOverrides()
	out, err := cmd.Output()
	require.NoError(t, err)
	return plumbing.NewHash(strings.TrimSpace(string(out)))
}

func TestListRefs_ReturnsEachRefByItsExactName(t *testing.T) {
	// Not parallel: isolateGitConfig uses t.Setenv.
	isolateGitConfig(t)
	dir := initGitlinkRepo(t)
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "one")
	head := headOf(t, dir)
	gitIn(t, dir, "update-ref", "refs/entire/checkpoints/ab/0000000000ab", head.String())
	gitIn(t, dir, "update-ref", "refs/entire/checkpoints/cd/0000000000cd", head.String())

	refs, err := ListRefs(t.Context(), dir, "refs/entire/checkpoints/*/0000000000ab")
	require.NoError(t, err)

	assert.Equal(t, map[plumbing.ReferenceName]plumbing.Hash{
		"refs/entire/checkpoints/ab/0000000000ab": head,
	}, refs)
}

func TestPackRefs_PacksTheMatchingRefs(t *testing.T) {
	// Not parallel: isolateGitConfig uses t.Setenv.
	isolateGitConfig(t)
	dir := initGitlinkRepo(t)
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "one")
	gitIn(t, dir, "update-ref", "refs/entire/checkpoints/ab/0000000000ab", headOf(t, dir).String())

	require.NoError(t, PackRefs(t.Context(), dir, "refs/entire/checkpoints/*/0000000000ab"))

	_, err := os.Stat(filepath.Join(dir, ".git", "refs", "entire", "checkpoints", "ab", "0000000000ab"))
	require.ErrorIs(t, err, os.ErrNotExist, "the loose ref is packed")
	packed, err := os.ReadFile(filepath.Join(dir, ".git", "packed-refs"))
	require.NoError(t, err)
	assert.Contains(t, string(packed), "refs/entire/checkpoints/ab/0000000000ab")
}

// Both run on hook paths, so a cancelled context stops them.
func TestListRefsAndPackRefs_StopOnACancelledContext(t *testing.T) {
	// Not parallel: isolateGitConfig uses t.Setenv.
	isolateGitConfig(t)
	dir := initGitlinkRepo(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := ListRefs(ctx, dir, "refs/")
	require.Error(t, err)
	require.Error(t, PackRefs(ctx, dir, "refs/*"))
}

// git before 2.42 has no pack-refs --include; Apple's bundled git is older.
// A stand-in git that rejects the option, as those versions do, and passes
// everything else to the real git makes that case testable on any version.
func TestPackRefs_PacksOnAGitWithoutInclude(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in git is a POSIX shell script")
	}
	// Not parallel: isolateGitConfig and the PATH override use t.Setenv.
	isolateGitConfig(t)
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	shimDir := t.TempDir()
	shim := "#!/bin/sh\nfor arg in \"$@\"; do\n  if [ \"$arg\" = --include ]; then echo \"error: unknown option \\`include'\" >&2; exit 129; fi\ndone\nexec \"" + realGit + "\" \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o700))
	dir := initGitlinkRepo(t)
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "one")
	gitIn(t, dir, "update-ref", "refs/entire/checkpoints/ab/0000000000ab", headOf(t, dir).String())
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	require.NoError(t, PackRefs(t.Context(), dir, "refs/entire/checkpoints/*/0000000000ab"))

	_, err = os.Stat(filepath.Join(dir, ".git", "refs", "entire", "checkpoints", "ab", "0000000000ab"))
	require.ErrorIs(t, err, os.ErrNotExist, "the loose ref is packed")
}
