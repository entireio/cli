package cli

import (
	"context"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// newDirtyTrackedRepo returns a repository with a.txt, b.txt and c.txt
// committed, then a.txt modified, b.txt deleted and u.txt untracked, and
// changes into it.
func newDirtyTrackedRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	releaseSharedRoots(t)
	t.Chdir(dir)
	testutil.InitRepo(t, dir)
	for _, f := range []string{"a.txt", "b.txt", "c.txt"} {
		testutil.WriteFile(t, dir, f, f+"\n")
	}
	testutil.GitAdd(t, dir, "a.txt", "b.txt", "c.txt")
	testutil.GitCommit(t, dir, "Add files")
	testutil.WriteFile(t, dir, "a.txt", "a, edited\n")
	require.NoError(t, os.Remove(filepath.Join(dir, "b.txt")))
	testutil.WriteFile(t, dir, "u.txt", "untracked\n")
	return dir
}

func TestStatusFilesForState_ListsDirtyTrackedFiles(t *testing.T) {
	// Not parallel: newDirtyTrackedRepo uses t.Chdir.
	newDirtyTrackedRepo(t)

	untracked, dirty, err := statusFilesForState(context.Background())
	require.NoError(t, err)
	slices.Sort(dirty)
	require.Equal(t, []string{"u.txt"}, untracked)
	require.Equal(t, []string{"a.txt", "b.txt"}, dirty)
}

// TestStatusClaim_LeavesOutUnchangedDirtyFiles pins direction 1 of #3011: a
// tracked file already dirty at prompt start is claimed only once its stat
// changes.
func TestStatusClaim_LeavesOutUnchangedDirtyFiles(t *testing.T) {
	// Not parallel: newDirtyTrackedRepo uses t.Chdir.
	dir := newDirtyTrackedRepo(t)
	ctx := context.Background()
	claim := statusClaim{repoRoot: dir, dirtyTracked: statDirtyTrackedFiles(ctx, []string{"a.txt", "b.txt"})}
	require.Equal(t, UntrackedFileStat{}, claim.dirtyTracked["b.txt"], "a deleted file gets the zero stat")

	modified, created, deleted := claim.narrow(ctx, []string{"a.txt"}, []string{"u.txt"}, []string{"b.txt"})
	require.Empty(t, modified, "a.txt was dirty before and is unchanged")
	require.Empty(t, deleted, "b.txt was deleted before and is still deleted")
	require.Equal(t, []string{"u.txt"}, created, "new files are not judged by the tracked baseline")

	testutil.WriteFile(t, dir, "a.txt", "a, edited again\n")
	testutil.WriteFile(t, dir, "b.txt", "b, restored with other content\n")
	testutil.WriteFile(t, dir, "c.txt", "c, newly dirty\n")
	modified, _, _ = claim.narrow(ctx, []string{"a.txt", "b.txt", "c.txt"}, nil, nil)
	require.Equal(t, []string{"a.txt", "b.txt", "c.txt"}, modified)
}

// TestStatusClaim_NoBaselineNarrowsNothing pins the fallback for state
// written before the baseline existed, or with no state at all: every file git
// status lists is kept, as before.
func TestStatusClaim_NoBaselineNarrowsNothing(t *testing.T) {
	t.Parallel()
	var noState *PrePromptState
	oldState := &PrePromptState{Timestamp: "not a time"}
	for _, claim := range []statusClaim{noState.statusClaim("", "s", nil), oldState.statusClaim("", "s", nil)} {
		modified, created, deleted := claim.narrow(context.Background(), []string{"a.txt"}, []string{"n.txt"}, []string{"d.txt"})
		require.Equal(t, []string{"a.txt"}, modified)
		require.Equal(t, []string{"n.txt"}, created)
		require.Equal(t, []string{"d.txt"}, deleted)
	}
}

// releaseSharedRoots closes every shared root before the test's temporary
// directories are removed, as Windows cannot remove a directory while a
// handle to it is open. t.Cleanup runs in reverse order, so call it after
// t.TempDir. It closes them all, not only the test's directory, because git
// can report the worktree under a longer spelling than t.TempDir's (a short
// TEMP path such as C:\Users\RUNNER~1), which opens a second root on it. That
// is only safe in a test that is not parallel.
func releaseSharedRoots(t *testing.T) {
	t.Helper()
	t.Cleanup(osroot.ResetShared)
}

// forgetSharedRoot closes the shared root opened on dir, a t.TempDir, before
// the directory is removed, for a parallel test that names the worktree only
// by dir.
func forgetSharedRoot(t *testing.T, dir string) {
	t.Helper()
	t.Cleanup(func() { osroot.Forget(dir) })
}

// caseInsensitiveDir reports whether dir's filesystem ignores case, the
// macOS and Windows default.
func caseInsensitiveDir(t *testing.T, dir string) bool {
	t.Helper()
	testutil.WriteFile(t, dir, "CaseProbe", "probe\n")
	defer func() { require.NoError(t, os.Remove(filepath.Join(dir, "CaseProbe"))) }()
	probe, err := os.Lstat(filepath.Join(dir, "CaseProbe"))
	require.NoError(t, err)
	folded, err := os.Lstat(filepath.Join(dir, "caseprobe"))
	return err == nil && os.SameFile(probe, folded)
}

// TestNamedFiles_OtherCaseIsTheSameFileWindows pins the case rule for
// transcript names (#3011) where it matters, on a case-insensitive filesystem:
// a name in another case than git's opens the same file, so it counts as
// naming it. CI's Windows job runs it by its name. On a case-sensitive
// filesystem it checks that a name in another case that opens no file names
// nothing.
func TestNamedFiles_OtherCaseIsTheSameFileWindows(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	forgetSharedRoot(t, dir)
	testutil.WriteFile(t, dir, "README.md", "readme\n")
	testutil.WriteFile(t, dir, "src/App.go", "package src\n")
	modified := []string{"README.md", "src/App.go"}
	named := newNamedFiles(dir, []string{"readme.md", "SRC/app.go"}, modified)
	if caseInsensitiveDir(t, dir) {
		require.Equal(t, modified, named.only(modified), "kept in git's spelling")
	} else {
		require.Empty(t, named.only(modified), "readme.md and SRC/app.go are other files, absent here")
	}
}

// TestNamedFiles_DeletedInOtherCaseWindows covers a deletion git lists as
// README.md that the transcript names readme.md. With the file gone there is
// nothing to compare, so the directory decides: one that finds its other
// entries under another case would have found README.md as readme.md, and
// one that is gone cannot tell. git's
// core.ignorecase is set to the opposite of what the filesystem does, to show
// that the filesystem decides: a linked worktree, a moved repository or a
// Windows directory can disagree with the repository's setting. CI's Windows
// job runs it by its name.
func TestNamedFiles_DeletedInOtherCaseWindows(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	forgetSharedRoot(t, dir)
	testutil.InitRepo(t, dir)
	insensitive := caseInsensitiveDir(t, dir)
	testutil.RunGit(t, dir, "config", "core.ignorecase", strconv.FormatBool(!insensitive))
	testutil.WriteFile(t, dir, "docs/Other.md", "keeps docs/\n")

	// docs/Guide.md is asked of docs/; Gone/File.md, whose directory went
	// with it, of the worktree root, which holds the directory's name.
	deleted := []string{"README.md", "docs/Guide.md", "Gone/File.md"}
	transcript := []string{"readme.md", "Docs/guide.md", "gone/File.md"}
	got := newNamedFiles(dir, transcript, deleted).only(deleted)
	if insensitive {
		require.Equal(t, deleted, got, "kept in git's spelling")
	} else {
		require.Empty(t, got, "on a case-sensitive filesystem the transcript names other files")
	}

	// A directory that is gone cannot tell how it found its entries, so a
	// file name spelled in another case inside it does not match.
	lost := []string{"Lost/File.md"}
	require.Empty(t, newNamedFiles(dir, []string{"Lost/file.md"}, lost).only(lost))

	// A transcript spelling that still opens a file is that file, not the
	// deleted one, unless the filesystem says they are the same.
	testutil.WriteFile(t, dir, "readme.md", "another file\n")
	other := []string{"README.md"}
	got = newNamedFiles(dir, []string{"readme.md"}, other).only(other)
	if insensitive {
		require.Equal(t, other, got, "README.md is present after all: it is readme.md")
	} else {
		require.Empty(t, got, "readme.md is another file")
	}
}

// TestNamedFiles_OtherCaseIsAnotherFile pins the case-sensitive side: two
// files that differ only in case are two files, whether git status lists both
// or only the one the transcript does not name (the session restored its own).
// Only a spelling that opens the same file matches.
func TestNamedFiles_OtherCaseIsAnotherFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	forgetSharedRoot(t, dir)
	if caseInsensitiveDir(t, dir) {
		t.Skip("needs a case-sensitive filesystem")
	}
	testutil.WriteFile(t, dir, "README.md", "edited by another session\n")
	testutil.WriteFile(t, dir, "readme.md", "restored\n")

	both := []string{"README.md", "readme.md"}
	require.Equal(t, []string{"readme.md"}, newNamedFiles(dir, []string{"readme.md"}, both).only(both))
	other := []string{"README.md"}
	require.Empty(t, newNamedFiles(dir, []string{"readme.md"}, other).only(other))
	require.Empty(t, newNamedFiles(dir, nil, both).only(both))

	// A spelling that opens the same file does count. A hard link stands in
	// for a case-insensitive filesystem, where both spellings open one file.
	require.NoError(t, os.Remove(filepath.Join(dir, "readme.md")))
	require.NoError(t, os.Link(filepath.Join(dir, "README.md"), filepath.Join(dir, "readme.md")))
	require.Equal(t, other, newNamedFiles(dir, []string{"readme.md"}, other).only(other))
}

// TestNamedFiles_UnicodeCaseFold covers a name in another case outside
// ASCII: ς folds with Σ, as filesystems that ignore case treat it, but
// lowercases to itself rather than to σ, so a lower-case key never brought the
// two together. Where the filesystem does not fold the pair, a hard link
// stands in for one file under both names.
func TestNamedFiles_UnicodeCaseFold(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	forgetSharedRoot(t, dir)
	testutil.WriteFile(t, dir, "Σ.txt", "sigma\n")
	if _, err := os.Lstat(filepath.Join(dir, "ς.txt")); err != nil {
		require.NoError(t, os.Link(filepath.Join(dir, "Σ.txt"), filepath.Join(dir, "ς.txt")))
	}
	listed := []string{"Σ.txt"}
	require.Equal(t, listed, newNamedFiles(dir, []string{"ς.txt"}, listed).only(listed))
}

// TestNamedFiles_ThroughDirectoryLink covers a transcript that names a file
// through a symlink to a directory in the worktree, alias -> real: git status
// lists the file under real/, which the transcript never spells. A link
// written as an absolute path matches too, as does one to the worktree root
// itself, and one to another directory names nothing there.
func TestNamedFiles_ThroughDirectoryLink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	forgetSharedRoot(t, dir)
	testutil.WriteFile(t, dir, "real/data.txt", "edited through alias\n")
	testutil.WriteFile(t, dir, "other/data.txt", "untouched\n")
	if err := os.Symlink("real", filepath.Join(dir, "alias")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	require.NoError(t, os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "absolute")))
	require.NoError(t, os.Symlink("other", filepath.Join(dir, "elsewhere")))
	require.NoError(t, os.Symlink(".", filepath.Join(dir, "top")))
	testutil.WriteFile(t, dir, "notes.txt", "edited through top\n")

	modified := []string{"real/data.txt"}
	deleted := []string{"real/gone.txt"}
	named := newNamedFiles(dir, []string{"alias/data.txt", "alias/gone.txt"}, modified, deleted)
	require.Equal(t, modified, named.only(modified))
	require.Equal(t, deleted, named.only(deleted), "a deletion through the link")
	require.Equal(t, modified, newNamedFiles(dir, []string{"absolute/data.txt"}, modified).only(modified))
	require.Empty(t, newNamedFiles(dir, []string{"elsewhere/data.txt"}, modified).only(modified))
	atRoot := []string{"notes.txt"}
	require.Equal(t, atRoot, newNamedFiles(dir, []string{"top/notes.txt"}, atRoot).only(atRoot), "a link to the root")
}

// TestNamedFiles_ThroughFileLink covers a transcript that names a symlink to
// a file, AGENTS.md -> CLAUDE.md: git status lists the file the write
// changed, CLAUDE.md, and the link itself not at all. A link to a file git
// status does not list names nothing there.
func TestNamedFiles_ThroughFileLink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	forgetSharedRoot(t, dir)
	testutil.WriteFile(t, dir, "CLAUDE.md", "edited through AGENTS.md\n")
	testutil.WriteFile(t, dir, "docs/other.md", "untouched\n")
	if err := os.Symlink("CLAUDE.md", filepath.Join(dir, "AGENTS.md")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	require.NoError(t, os.Symlink(filepath.Join("..", "CLAUDE.md"), filepath.Join(dir, "docs", "CLAUDE.md")))
	require.NoError(t, os.Symlink("other.md", filepath.Join(dir, "docs", "link.md")))

	modified := []string{"CLAUDE.md"}
	require.Equal(t, modified, newNamedFiles(dir, []string{"AGENTS.md"}, modified).only(modified))
	require.Equal(t, modified, newNamedFiles(dir, []string{"docs/CLAUDE.md"}, modified).only(modified), "a link in another directory")
	require.Empty(t, newNamedFiles(dir, []string{"docs/link.md"}, modified).only(modified), "a link to a file git status does not list")
}

// TestNamedFiles_ThroughFileLinkInOtherCase: on a case-insensitive
// filesystem, a write through AGENTS.md -> claude.md changes git's CLAUDE.md.
// A hard link stands in for such a filesystem here; without one, claude.md
// is another file and must not match.
func TestNamedFiles_ThroughFileLinkInOtherCase(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	forgetSharedRoot(t, dir)
	testutil.WriteFile(t, dir, "CLAUDE.md", "edited through AGENTS.md\n")
	if err := os.Symlink("claude.md", filepath.Join(dir, "AGENTS.md")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	modified := []string{"CLAUDE.md"}
	listed, err := os.Lstat(filepath.Join(dir, "CLAUDE.md"))
	require.NoError(t, err)
	if other, err := os.Lstat(filepath.Join(dir, "claude.md")); err != nil || !os.SameFile(listed, other) {
		testutil.WriteFile(t, dir, "claude.md", "another file\n")
		require.Empty(t, newNamedFiles(dir, []string{"AGENTS.md"}, modified).only(modified), "claude.md is another file")
		require.NoError(t, os.Remove(filepath.Join(dir, "claude.md")))
		if err := os.Link(filepath.Join(dir, "CLAUDE.md"), filepath.Join(dir, "claude.md")); err != nil {
			t.Skipf("cannot create a hard link here: %v", err)
		}
	}
	require.Equal(t, modified, newNamedFiles(dir, []string{"AGENTS.md"}, modified).only(modified))
}

// TestNamedFiles_ListedLinkClaimsOnlyItself: git status lists a new or
// retargeted AGENTS.md -> CLAUDE.md beside CLAUDE.md. A session whose
// transcript names AGENTS.md changed the link; it may have written CLAUDE.md
// through it, or another session may have, so it claims only the link.
func TestNamedFiles_ListedLinkClaimsOnlyItself(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	forgetSharedRoot(t, dir)
	testutil.WriteFile(t, dir, "CLAUDE.md", "edited by another session\n")
	if err := os.Symlink("CLAUDE.md", filepath.Join(dir, "AGENTS.md")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	modified := []string{"AGENTS.md", "CLAUDE.md"}
	require.Equal(t, []string{"AGENTS.md"}, newNamedFiles(dir, []string{"AGENTS.md"}, modified).only(modified))
}

// TestNamedFiles_ListedLinkThroughDirectoryLink: the transcript names
// alias/AGENTS.md for alias -> real, and git status lists the link it
// resolves to, real/AGENTS.md -> CLAUDE.md, beside real/CLAUDE.md. The link
// is listed as its directory resolves, so it claims only itself.
func TestNamedFiles_ListedLinkThroughDirectoryLink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	forgetSharedRoot(t, dir)
	testutil.WriteFile(t, dir, "real/CLAUDE.md", "edited by another session\n")
	if err := os.Symlink("CLAUDE.md", filepath.Join(dir, "real", "AGENTS.md")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	require.NoError(t, os.Symlink("real", filepath.Join(dir, "alias")))
	modified := []string{"real/AGENTS.md", "real/CLAUDE.md"}
	require.Equal(t, []string{"real/AGENTS.md"}, newNamedFiles(dir, []string{"alias/AGENTS.md"}, modified).only(modified))
}

// TestNamedFiles_ListedLinkInOtherCase: on a case-insensitive filesystem
// the transcript's agents.md is git's listed AGENTS.md -> CLAUDE.md, so it
// claims only the link. A hard link to the symlink stands in for such a
// filesystem here.
func TestNamedFiles_ListedLinkInOtherCase(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	forgetSharedRoot(t, dir)
	testutil.WriteFile(t, dir, "CLAUDE.md", "edited by another session\n")
	if err := os.Symlink("CLAUDE.md", filepath.Join(dir, "AGENTS.md")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "agents.md")); err != nil {
		if err := os.Link(filepath.Join(dir, "AGENTS.md"), filepath.Join(dir, "agents.md")); err != nil {
			t.Skipf("cannot create a hard link here: %v", err)
		}
	}
	if info, err := os.Lstat(filepath.Join(dir, "agents.md")); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Skip("a hard link here does not keep the symlink")
	}
	modified := []string{"AGENTS.md", "CLAUDE.md"}
	require.Equal(t, []string{"AGENTS.md"}, newNamedFiles(dir, []string{"agents.md"}, modified).only(modified))
}

// TestNamedFiles_DeletedInOtherNormalizationForm: a deletion named
// decomposed (cafe\u0301.txt) matches git's composed caf\u00e9.txt only
// where the filesystem ignores normalization, as APFS and HFS+ do; nothing is
// left to compare.
func TestNamedFiles_DeletedInOtherNormalizationForm(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	forgetSharedRoot(t, dir)
	deleted := []string{"caf\u00e9.txt"}
	n := newNamedFiles(dir, []string{"cafe\u0301.txt"}, deleted)
	if runtime.GOOS != "darwin" {
		require.False(t, n.ignoresNormalization(deleted[0]), "only APFS and HFS+ are known to ignore normalization")
	}
	n.ignoresNormalization = func(string) bool { return false }
	require.Empty(t, n.only(deleted), "two names for two files")
	n.ignoresNormalization = func(string) bool { return true }
	require.Equal(t, deleted, n.only(deleted))

	// A worktree on APFS can hold a mount that keeps the forms apart: each
	// name's own directory is asked.
	deleted = []string{"mnt/caf\u00e9.txt"}
	n = newNamedFiles(dir, []string{"mnt/cafe\u0301.txt"}, deleted)
	n.ignoresNormalization = func(name string) bool { return path.Dir(name) != "mnt" }
	require.Empty(t, n.only(deleted), "a directory that keeps the forms apart")

	// The filesystem asked is the one holding the component that differs,
	// not the leaf's: here café, in the worktree's root, above the mount.
	deleted = []string{"caf\u00e9/mnt/x.txt"}
	n = newNamedFiles(dir, []string{"cafe\u0301/mnt/x.txt"}, deleted)
	n.ignoresNormalization = func(name string) bool { return path.Dir(name) == "." }
	require.Equal(t, deleted, n.only(deleted), "the root ignores normalization, whatever the mount does")
	n.ignoresNormalization = func(name string) bool { return path.Dir(name) != "." }
	require.Empty(t, n.only(deleted), "the root keeps the forms apart, whatever the mount does")
}

// TestNamedFiles_OtherNormalizationForm covers a transcript that spells a
// name decomposed (cafe\u0301.txt) where git status lists it composed
// (caf\u00e9.txt), as git does on macOS, whose filesystem opens both as one
// file. A hard link stands in for such a filesystem here; without one, the
// two spellings are two files and must not match.
func TestNamedFiles_OtherNormalizationForm(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	forgetSharedRoot(t, dir)
	composed, decomposed := "caf\u00e9.txt", "cafe\u0301.txt"
	testutil.WriteFile(t, dir, composed, "edited\n")
	modified := []string{composed}
	listed, err := os.Lstat(filepath.Join(dir, composed))
	require.NoError(t, err)
	if named, err := os.Lstat(filepath.Join(dir, decomposed)); err == nil && os.SameFile(listed, named) {
		require.Equal(t, modified, newNamedFiles(dir, []string{decomposed}, modified).only(modified), "this filesystem ignores normalization")
		return
	}
	require.Empty(t, newNamedFiles(dir, []string{decomposed}, modified).only(modified), "the decomposed spelling opens no file")
	if err := os.Link(filepath.Join(dir, composed), filepath.Join(dir, decomposed)); err != nil {
		t.Skipf("cannot create a hard link here: %v", err)
	}
	require.Equal(t, modified, newNamedFiles(dir, []string{decomposed}, modified).only(modified), "both spellings open one file")
	testutil.WriteFile(t, dir, "other\u00e9.txt", "another file\n")
	testutil.WriteFile(t, dir, "othere\u0301.txt", "another file\n")
	other := []string{"other\u00e9.txt"}
	require.Empty(t, newNamedFiles(dir, []string{"othere\u0301.txt"}, other).only(other), "two files")
}

// TestNamedFiles_LexicalSpelling covers a transcript that names a file with
// components git status never shows, such as ./notes.txt. A ".." is taken as
// the filesystem takes it: after a symlink to a directory, it leaves the
// link's target, not the link's own directory.
func TestNamedFiles_LexicalSpelling(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	forgetSharedRoot(t, dir)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "src", "lib"), 0o755))
	modified := []string{"notes.txt", "src/app.go"}
	named := newNamedFiles(dir, []string{"./notes.txt", "src/./lib/../app.go"}, modified)
	require.Equal(t, modified, named.only(modified))

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "real", "sub"), 0o755))
	if err := os.Symlink(filepath.Join("real", "sub"), filepath.Join(dir, "alias")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	both := []string{"file.txt", "real/file.txt"}
	want := "real/file.txt" // alias/.. is real/, not the root
	if runtime.GOOS == "windows" {
		want = "file.txt" // Windows takes .. before the link
	}
	require.Equal(t, []string{want}, newNamedFiles(dir, []string{"alias/../file.txt"}, both).only(both))
}

func TestCaseFoldKey(t *testing.T) {
	t.Parallel()
	same := [][]string{
		{"Σ.txt", "σ.txt", "ς.txt"},
		{"README.md", "readme.md", "ReadMe.MD"},
		{"k.go", "K.go", "\u212a.go"}, // the Kelvin sign folds with k
		// composed and decomposed
		{"caf\u00e9.txt", "cafe\u0301.txt", "CAFE\u0301.TXT"},
	}
	for _, names := range same {
		for _, name := range names[1:] {
			require.Equal(t, caseFoldKey(names[0]), caseFoldKey(name), "%s and %s fold together", names[0], name)
		}
	}
	require.NotEqual(t, caseFoldKey("a.txt"), caseFoldKey("b.txt"))
	require.NotEqual(t, caseFoldKey("ss.txt"), caseFoldKey("ß.txt"), "simple folding does not expand ß into ss")
}

func TestParseCaptureTime(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	require.True(t, parseCaptureTime(now.Format(time.RFC3339Nano)).Equal(now))
	require.True(t, parseCaptureTime(now.Format(time.RFC3339)).Equal(now.Truncate(time.Second)), "state written before nanoseconds were recorded")
	require.True(t, parseCaptureTime("").IsZero())
}

// TestStatusClaim_DirtyTrackedBaselineWindows is selected by CI's Windows job
// through its name. There a file's stat is only its size and modification
// time, so the dirty-file baseline must still read the nested paths git status
// reports with forward slashes, leave unchanged files out, and catch a
// same-size edit by its modification time alone.
func TestStatusClaim_DirtyTrackedBaselineWindows(t *testing.T) {
	// Not parallel: uses t.Chdir.
	dir := t.TempDir()
	releaseSharedRoots(t)
	t.Chdir(dir)
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "src/app/main.txt", "v1\n")
	testutil.WriteFile(t, dir, "gone.txt", "gone\n")
	testutil.GitAdd(t, dir, "src/app/main.txt", "gone.txt")
	testutil.GitCommit(t, dir, "Add files")
	testutil.WriteFile(t, dir, "src/app/main.txt", "v2\n")
	require.NoError(t, os.Remove(filepath.Join(dir, "gone.txt")))

	ctx := context.Background()
	_, dirty, err := statusFilesForState(ctx)
	require.NoError(t, err)
	slices.Sort(dirty)
	require.Equal(t, []string{"gone.txt", "src/app/main.txt"}, dirty)

	claim := statusClaim{repoRoot: dir, dirtyTracked: statDirtyTrackedFiles(ctx, dirty)}
	before, ok := claim.dirtyTracked["src/app/main.txt"]
	require.True(t, ok, "the nested file was statted")
	require.NotZero(t, before.ModTime)
	modified, _, deleted := claim.narrow(ctx, []string{"src/app/main.txt"}, nil, []string{"gone.txt"})
	require.Empty(t, modified, "src/app/main.txt is unchanged since the baseline")
	require.Empty(t, deleted, "gone.txt is still deleted")

	// The same size as before: only the modification time can tell. An agent's
	// edit lands well after its prompt starts, so move it past any clock tick.
	testutil.WriteFile(t, dir, "src/app/main.txt", "v3\n")
	later := time.Unix(0, before.ModTime).Add(2 * time.Second)
	require.NoError(t, os.Chtimes(filepath.Join(dir, "src", "app", "main.txt"), later, later))
	modified, _, _ = claim.narrow(ctx, []string{"src/app/main.txt"}, nil, nil)
	require.Equal(t, []string{"src/app/main.txt"}, modified)
}
