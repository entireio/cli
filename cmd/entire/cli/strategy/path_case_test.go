package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/worktreedir"

	"github.com/stretchr/testify/require"
)

// trackedCaseRepo returns a repository that tracks README.md and src/App.go,
// and its worktree root.
func trackedCaseRepo(t *testing.T) (string, *os.Root) {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "README.md", "readme\n")
	testutil.WriteFile(t, dir, "src/App.go", "package src\n")
	testutil.GitAdd(t, dir, "README.md", "src/App.go")
	testutil.GitCommit(t, dir, "Add files")
	worktree, err := worktreedir.OpenAt(dir)
	require.NoError(t, err)
	return dir, worktree
}

// TestTrackedSpellings_CaseInsensitiveFilesystemWindows checks the rewrite
// where it applies, on a case-insensitive filesystem: CI's Windows job runs
// it by its name. On a case-sensitive one it checks that nothing changes.
func TestTrackedSpellings_CaseInsensitiveFilesystemWindows(t *testing.T) {
	t.Parallel()
	dir, _ := trackedCaseRepo(t)
	files := []string{"readme.md", "SRC/app.go", "README.md", "new.txt"}
	got := TrackedSpellings(context.Background(), dir, files)
	if worktreedir.NewCaseProbe(dir).IgnoresCase(".") {
		require.Equal(t, []string{"README.md", "src/App.go", "new.txt"}, got,
			"spelled as git spells them, the duplicate dropped; an untracked name is kept")
	} else {
		require.Equal(t, files, got, "readme.md and SRC/app.go are other files here")
	}
}

// TestTrackedSpelling covers the rule itself, independent of the filesystem.
// A hard link stands in for a case-insensitive filesystem, where both
// spellings open one file, and a hard-linked pair of entries for a directory
// that finds its entries ignoring case.
func TestTrackedSpelling(t *testing.T) {
	t.Parallel()
	dir, worktree := trackedCaseRepo(t)
	cases := worktreedir.NewCaseProbe(dir)
	if cases.IgnoresCase(".") {
		t.Skip("needs a case-sensitive filesystem to tell the cases apart")
	}
	tracked := []string{"README.md"}

	require.Equal(t, "README.md", trackedSpelling(worktree, cases, dir, "README.md", tracked), "already git's spelling")
	require.Equal(t, "notes.txt", trackedSpelling(worktree, cases, dir, "notes.txt", nil), "not tracked in any case")
	require.Equal(t, "readme.md", trackedSpelling(worktree, cases, dir, "readme.md", tracked), "opens no file")

	testutil.WriteFile(t, dir, "readme.md", "another file\n")
	require.Equal(t, "readme.md", trackedSpelling(worktree, cases, dir, "readme.md", tracked), "another file")

	require.NoError(t, os.Remove(filepath.Join(dir, "readme.md")))
	require.NoError(t, os.Link(filepath.Join(dir, "README.md"), filepath.Join(dir, "readme.md")))
	require.Equal(t, "README.md", trackedSpelling(worktree, cases, dir, "readme.md", tracked), "opens the same file")

	// A deletion has nothing left to open, so the directory decides. The
	// root respects case: readme.md was another file.
	require.NoError(t, os.Remove(filepath.Join(dir, "readme.md")))
	require.NoError(t, os.Remove(filepath.Join(dir, "README.md")))
	require.Equal(t, "readme.md", trackedSpelling(worktree, cases, dir, "readme.md", tracked), "the root respects case")

	// src/ finds its entries ignoring case, so src/app.go names git's
	// deleted src/App.go.
	testutil.WriteFile(t, dir, "src/A.txt", "a\n")
	require.NoError(t, os.Link(filepath.Join(dir, "src", "A.txt"), filepath.Join(dir, "src", "a.TXT")))
	require.NoError(t, os.Remove(filepath.Join(dir, "src", "App.go")))
	cases = worktreedir.NewCaseProbe(dir)
	app := []string{"src/App.go"}
	require.Equal(t, "src/App.go", trackedSpelling(worktree, cases, dir, "src/app.go", app), "deleted under git's spelling")
	require.Equal(t, "src/app.go", trackedSpelling(worktree, cases, dir, "src/app.go", []string{"src/App.go", "src/APP.go"}),
		"ambiguous: the index has two spellings")
	require.Equal(t, "Src/app.go", trackedSpelling(worktree, cases, dir, "Src/app.go", app), "the root respects case")
	testutil.WriteFile(t, dir, "src/App.go", "back\n")
	require.Equal(t, "src/app.go", trackedSpelling(worktree, cases, dir, "src/app.go", app),
		"git's spelling still opens a file, so src/app.go is another one")
}

// On a case-sensitive filesystem FilterTrackableChanges keeps both
// spellings: they are two files.
func TestFilterTrackableChanges_KeepsSpellingsOnCaseSensitiveFilesystem(t *testing.T) {
	t.Parallel()
	dir, _ := trackedCaseRepo(t)
	if worktreedir.NewCaseProbe(dir).IgnoresCase(".") {
		t.Skip("TestTrackedSpellings_CaseInsensitiveFilesystemWindows covers a case-insensitive filesystem")
	}
	testutil.WriteFile(t, dir, "readme.md", "another file\n")
	modified, added, deleted := FilterTrackableChanges(context.Background(), dir, []string{"README.md"}, []string{"readme.md"}, nil)
	require.Equal(t, []string{"README.md"}, modified)
	require.Equal(t, []string{"readme.md"}, added, "a separate file on a case-sensitive filesystem")
	require.Empty(t, deleted)
}
