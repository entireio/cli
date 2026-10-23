package gitdir_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/gitdir"
)

func initRepo(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "init", "-q", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git init: %s", out)
}

// CommonDirForPath is handed a directory from configuration and must answer
// about THAT directory. Git exports GIT_DIR and GIT_WORK_TREE to every hook it runs and
// they outrank cmd.Dir, so an inherited environment makes it answer about the
// hook's repository instead. Callers compare the result to decide whether two
// paths belong to the same clone; being wrong there attributes one
// repository's state, or its settings, to another.
//
// CommonDir and CommonDirForWorktree deliberately do NOT scrub — they answer
// for the repository the command runs in, where an exported GIT_DIR is the
// right answer.
func TestCommonDirForPath_IgnoresAHooksExportedRepo(t *testing.T) {
	target := t.TempDir()
	initRepo(t, target)
	other := t.TempDir()
	initRepo(t, other)

	want, err := gitdir.CommonDirForPath(t.Context(), target)
	require.NoError(t, err)

	// Exactly what git hands a hook running in the other repository.
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)

	got, err := gitdir.CommonDirForPath(t.Context(), target)
	require.NoError(t, err)
	assert.Equal(t, want, got,
		"the directory asked about must win over a hook's exported repository")

	otherCommon, err := gitdir.CommonDirForPath(t.Context(), other)
	require.NoError(t, err)
	assert.NotEqual(t, otherCommon, got, "sanity: the two repositories are distinguishable")
}

// CommonDirForWorktree's callers take their root from the current repository,
// which honors exported repository selectors (a bare dotfiles repository with
// GIT_DIR and GIT_WORK_TREE set, say), so it must honor them too.
func TestCommonDirForWorktree_HonorsAnExportedRepo(t *testing.T) {
	target := t.TempDir()
	initRepo(t, target)
	other := t.TempDir()
	initRepo(t, other)

	otherCommon, err := gitdir.CommonDirForPath(t.Context(), other)
	require.NoError(t, err)

	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)

	got, err := gitdir.CommonDirForWorktree(t.Context(), target)
	require.NoError(t, err)
	assert.Equal(t, otherCommon, got, "the exported repository selectors decide")
}
