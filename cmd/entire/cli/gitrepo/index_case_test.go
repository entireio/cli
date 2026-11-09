package gitrepo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIndexPathsIgnoringCase(t *testing.T) {
	// Not parallel: isolateGitConfig and the inherited-variable check use t.Setenv.
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
	for _, name := range []string{"README.md", "src/App.go", "notes[1].txt"} {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x\n"), 0o644))
	}
	git("add", ".")

	// Inherited pathspec settings must not change the answer.
	t.Setenv("GIT_LITERAL_PATHSPECS", "1")
	t.Setenv("GIT_GLOB_PATHSPECS", "1")

	found, err := IndexPathsIgnoringCase(context.Background(), dir,
		[]string{"readme.md", "SRC/app.go", "NOTES[1].TXT", "src/App.go", "missing.txt"})
	require.NoError(t, err)
	require.Equal(t, map[string][]string{
		"readme.md":    {"README.md"},
		"src/app.go":   {"src/App.go"},
		"notes[1].txt": {"notes[1].txt"},
	}, found, "brackets are matched literally; an untracked name matches nothing")

	none, err := IndexPathsIgnoringCase(context.Background(), dir, nil)
	require.NoError(t, err)
	require.Empty(t, none)
}

// TestCommitIndexPathsIgnoringCase covers a commit hook's temporary index:
// README.md is staged as renamed to readme.md, and a path-limited commit is
// prepared in an index that still has README.md. Only the commit variant
// reads that index.
func TestCommitIndexPathsIgnoringCase(t *testing.T) {
	// Not parallel: isolateGitConfig and GIT_INDEX_FILE use t.Setenv.
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
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("x\n"), 0o644))
	git("add", "README.md")
	commitIndex := filepath.Join(t.TempDir(), "next-index.lock")
	index, err := os.ReadFile(filepath.Join(dir, ".git", "index"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(commitIndex, index, 0o644))
	git("mv", "README.md", "readme.md")
	t.Setenv("GIT_INDEX_FILE", commitIndex)

	worktree, err := IndexPathsIgnoringCase(context.Background(), dir, []string{"README.md"})
	require.NoError(t, err)
	require.Equal(t, map[string][]string{"readme.md": {"readme.md"}}, worktree, "the worktree's index has the rename")

	commit, err := CommitIndexPathsIgnoringCase(context.Background(), dir, []string{"README.md"})
	require.NoError(t, err)
	require.Equal(t, map[string][]string{"readme.md": {"README.md"}}, commit, "the commit's index keeps README.md")
}
