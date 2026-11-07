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
	dir, worktree := trackedCaseRepo(t)
	files := []string{"readme.md", "SRC/app.go", "README.md", "new.txt"}
	got := TrackedSpellings(context.Background(), dir, files)
	if ignoresCase(worktree, dir, []string{"README.md"}) {
		require.Equal(t, []string{"README.md", "src/App.go", "new.txt"}, got,
			"spelled as git spells them, the duplicate dropped; an untracked name is kept")
	} else {
		require.Equal(t, files, got, "readme.md and SRC/app.go are other files here")
	}
}

// TestTrackedSpelling covers the rule itself, independent of the filesystem.
// A hard link stands in for a case-insensitive filesystem, where both
// spellings open one file.
func TestTrackedSpelling(t *testing.T) {
	t.Parallel()
	dir, worktree := trackedCaseRepo(t)
	if ignoresCase(worktree, dir, []string{"README.md"}) {
		t.Skip("needs a case-sensitive filesystem to tell the cases apart")
	}
	tracked := []string{"README.md"}

	require.Equal(t, "README.md", trackedSpelling(worktree, dir, "README.md", tracked), "already git's spelling")
	require.Equal(t, "notes.txt", trackedSpelling(worktree, dir, "notes.txt", nil), "not tracked in any case")
	require.Equal(t, "readme.md", trackedSpelling(worktree, dir, "readme.md", tracked), "opens no file")

	testutil.WriteFile(t, dir, "readme.md", "another file\n")
	require.Equal(t, "readme.md", trackedSpelling(worktree, dir, "readme.md", tracked), "another file")

	require.NoError(t, os.Remove(filepath.Join(dir, "readme.md")))
	require.NoError(t, os.Link(filepath.Join(dir, "README.md"), filepath.Join(dir, "readme.md")))
	require.Equal(t, "README.md", trackedSpelling(worktree, dir, "readme.md", tracked), "opens the same file")

	// A deletion: both spellings are gone, as on a case-insensitive
	// filesystem, so the transcript's spelling names git's deleted file.
	require.NoError(t, os.Remove(filepath.Join(dir, "readme.md")))
	require.NoError(t, os.Remove(filepath.Join(dir, "README.md")))
	require.Equal(t, "README.md", trackedSpelling(worktree, dir, "readme.md", tracked), "deleted under git's spelling")
	require.Equal(t, "readme.md", trackedSpelling(worktree, dir, "readme.md", []string{"README.md", "Readme.md"}),
		"ambiguous: the index has two spellings")
	testutil.WriteFile(t, dir, "README.md", "back\n")
	require.Equal(t, "readme.md", trackedSpelling(worktree, dir, "readme.md", tracked), "git's spelling still opens a file, so readme.md is another one")
}

// On a case-sensitive filesystem FilterTrackableChanges keeps both
// spellings: they are two files.
func TestFilterTrackableChanges_KeepsSpellingsOnCaseSensitiveFilesystem(t *testing.T) {
	t.Parallel()
	dir, worktree := trackedCaseRepo(t)
	if ignoresCase(worktree, dir, []string{"README.md"}) {
		t.Skip("TestTrackedSpellings_CaseInsensitiveFilesystemWindows covers a case-insensitive filesystem")
	}
	testutil.WriteFile(t, dir, "readme.md", "another file\n")
	modified, added, deleted := FilterTrackableChanges(context.Background(), dir, []string{"README.md"}, []string{"readme.md"}, nil)
	require.Equal(t, []string{"README.md"}, modified)
	require.Equal(t, []string{"readme.md"}, added, "a separate file on a case-sensitive filesystem")
	require.Empty(t, deleted)
}

// ignoresCase judges by the first file, or parent directory, that exists and
// has a letter; with none, git is asked.
func TestIgnoresCase(t *testing.T) {
	t.Parallel()
	dir, worktree := trackedCaseRepo(t)
	insensitive := caseInsensitiveProbe(t, dir)
	require.Equal(t, insensitive, ignoresCase(worktree, dir, []string{"README.md"}))
	require.Equal(t, insensitive, ignoresCase(worktree, dir, []string{"src/gone.go"}), "a deleted file's directory")
	require.Equal(t, insensitive, ignoresCase(worktree, dir, []string{"123", "gone.txt", "README.md"}), "skips what cannot tell")
	require.True(t, ignoresCase(worktree, dir, []string{"gone.txt"}), "nothing can tell")
	require.True(t, ignoresCase(worktree, dir, nil), "nothing can tell")
	require.Equal(t, "rEADME.MD", swapCase("Readme.md"))
}

// caseInsensitiveProbe reports whether dir's filesystem ignores case, from a
// probe file of its own.
func caseInsensitiveProbe(t *testing.T, dir string) bool {
	t.Helper()
	testutil.WriteFile(t, dir, "CaseProbe", "probe\n")
	defer func() { require.NoError(t, os.Remove(filepath.Join(dir, "CaseProbe"))) }()
	probe, err := os.Lstat(filepath.Join(dir, "CaseProbe"))
	require.NoError(t, err)
	folded, err := os.Lstat(filepath.Join(dir, "caseprobe"))
	return err == nil && os.SameFile(probe, folded)
}
