package strategy

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"strings"
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

// ignoresCaseIn stands in for a worktree where the directory it names finds
// its entries ignoring case and the others respect case.
type ignoresCaseIn string

func (dir ignoresCaseIn) IgnoresCase(d string) bool {
	return d == string(dir)
}

func (dir ignoresCaseIn) SameEntry(file, spelling string) bool {
	return path.Dir(file) == string(dir) && path.Dir(spelling) == string(dir) && strings.EqualFold(file, spelling)
}

// TestAnyDirectoryIgnoresCase checks that each directory on a path counts,
// not only the one holding the file: SRC/App.go in a root that ignores case
// opens src/App.go even when src/ respects case, as Windows allows.
func TestAnyDirectoryIgnoresCase(t *testing.T) {
	t.Parallel()
	require.True(t, anyDirectoryIgnoresCase(ignoresCaseIn("."), []string{"SRC/App.go"}), "the root ignores case")
	require.True(t, anyDirectoryIgnoresCase(ignoresCaseIn("src"), []string{"README.md", "src/a/b.go"}), "an ancestor of b.go ignores case")
	require.False(t, anyDirectoryIgnoresCase(ignoresCaseIn("src"), []string{"README.md", "docs/a.md"}), "no directory on these paths ignores case")
	require.False(t, anyDirectoryIgnoresCase(ignoresCaseIn("."), nil))
}

// TestTrackedSpelling covers the rule itself, independent of the filesystem.
// A hard link stands in for a case-insensitive filesystem, where both
// spellings open one file, and ignoresCaseIn for a directory that finds its
// entries ignoring case (worktreedir's tests cover the probe itself).
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
	require.Equal(t, "README.md", trackedSpelling(worktree, ignoresCaseIn("."), dir, "readme.md", tracked),
		"opens the same file where the root ignores case")
	require.Equal(t, "readme.md", trackedSpelling(worktree, cases, dir, "readme.md", tracked),
		"where the root respects case, a hard link is another entry, which git tracks apart")

	// A deletion has nothing left to open, so the directory decides. The
	// root respects case: readme.md was another file.
	require.NoError(t, os.Remove(filepath.Join(dir, "readme.md")))
	require.NoError(t, os.Remove(filepath.Join(dir, "README.md")))
	require.Equal(t, "readme.md", trackedSpelling(worktree, cases, dir, "readme.md", tracked), "the root respects case")

	// Where src/ finds its entries ignoring case, src/app.go names git's
	// deleted src/App.go.
	require.NoError(t, os.Remove(filepath.Join(dir, "src", "App.go")))
	app := []string{"src/App.go"}
	require.Equal(t, "src/app.go", trackedSpelling(worktree, cases, dir, "src/app.go", app), "src/ respects case here")
	folding := ignoresCaseIn("src")
	require.Equal(t, "src/App.go", trackedSpelling(worktree, folding, dir, "src/app.go", app), "deleted under git's spelling")
	require.Equal(t, "src/app.go", trackedSpelling(worktree, folding, dir, "src/app.go", []string{"src/App.go", "src/APP.go"}),
		"ambiguous: the index has two spellings")
	require.Equal(t, "Src/app.go", trackedSpelling(worktree, folding, dir, "Src/app.go", app), "the root respects case")
	testutil.WriteFile(t, dir, "src/App.go", "back\n")
	require.Equal(t, "src/app.go", trackedSpelling(worktree, folding, dir, "src/app.go", app),
		"git's spelling still opens a file, so src/app.go is another one")
}

// TestFilterTrackableFiles_KeepsGitsSpellingWindows checks the transcript
// route a commit hook takes before the first turn end: a path respelled as
// git spells it is kept in that spelling, not dropped for differing from the
// transcript's. CI's Windows job runs it by its name.
func TestFilterTrackableFiles_KeepsGitsSpellingWindows(t *testing.T) {
	t.Parallel()
	dir, _ := trackedCaseRepo(t)
	got := filterTrackableFiles(context.Background(), dir, []string{"readme.md", "notes.txt"})
	if worktreedir.NewCaseProbe(dir).IgnoresCase(".") {
		require.Equal(t, []string{"README.md", "notes.txt"}, got)
	} else {
		require.Equal(t, []string{"readme.md", "notes.txt"}, got, "readme.md is another file here")
	}
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
