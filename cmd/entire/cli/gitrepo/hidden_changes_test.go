package gitrepo_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
)

// HiddenWorktreeChanges reports what git status hides behind assume-unchanged
// and skip-worktree, compared by git's own rules, and leaves the repository's
// index as it was. The tests drive git against temporary repositories with
// HiddenWorktreeChanges, which keeps the caller's repository selectors, so
// they clear any a test run inherited. Not parallel: that changes the
// environment.
func TestHiddenWorktreeChanges(t *testing.T) {
	gitenv.IsolateRepository(t)
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	// The CRLF blob must survive the commit, so no conversion on add.
	testutil.RunGit(t, dir, "config", "core.autocrlf", "false")
	for _, name := range []string{"edited.txt", "deleted.txt", "retyped.txt", "touched.txt", "sparse.txt", "sparse-edited.txt"} {
		testutil.WriteFile(t, dir, name, "x\n")
	}
	testutil.WriteFile(t, dir, "crlf.txt", "a\r\nb\r\n")
	testutil.WriteFile(t, dir, "plain.txt", "x\n")
	testutil.RunGit(t, dir, "add", "-A")
	testutil.RunGit(t, dir, "commit", "-m", "init")
	testutil.WriteFile(t, dir, ".gitattributes", "* text=auto\n")
	testutil.RunGit(t, dir, "add", ".gitattributes")
	testutil.RunGit(t, dir, "commit", "-m", "normalize line endings")

	testutil.RunGit(t, dir, "update-index", "--assume-unchanged", "edited.txt", "deleted.txt", "retyped.txt", "touched.txt", "crlf.txt")
	testutil.RunGit(t, dir, "update-index", "--skip-worktree", "sparse.txt", "sparse-edited.txt")
	testutil.WriteFile(t, dir, "edited.txt", "edited\n")
	testutil.WriteFile(t, dir, "sparse-edited.txt", "edited\n")
	testutil.WriteFile(t, dir, "touched.txt", "x\n") // same contents, new stat
	testutil.WriteFile(t, dir, "plain.txt", "not hidden, so not reported here\n")
	for _, name := range []string{"deleted.txt", "retyped.txt", "sparse.txt"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"deleted.txt", "edited.txt", "retyped.txt", "sparse-edited.txt"}
	if err := os.Symlink("plain.txt", filepath.Join(dir, "retyped.txt")); err != nil {
		t.Logf("symlinks unavailable, checking a deletion instead: %v", err)
	}

	indexPath := filepath.Join(dir, ".git", "index")
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := gitrepo.HiddenWorktreeChanges(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("HiddenWorktreeChanges = %q, want %q", got, want)
	}
	after, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("the repository's index was rewritten")
	}
	if status := testutil.RunGit(t, dir, "--no-optional-locks", "status", "--porcelain"); strings.Contains(status, "edited.txt") {
		t.Errorf("the hidden flags were lost; git status now reports:\n%s", status)
	}
}

// core.ignoreStat marks entries assume-unchanged as they enter an index, which
// must not happen to the copies compared in the temporary one.
func TestHiddenWorktreeChanges_IgnoreStat(t *testing.T) {
	gitenv.IsolateRepository(t)
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "x")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	testutil.RunGit(t, dir, "config", "core.ignoreStat", "true")
	testutil.RunGit(t, dir, "update-index", "--assume-unchanged", "f.txt")
	testutil.WriteFile(t, dir, "f.txt", "edited")

	got, err := gitrepo.HiddenWorktreeChanges(context.Background(), dir)
	if err != nil || !slices.Equal(got, []string{"f.txt"}) {
		t.Fatalf("HiddenWorktreeChanges = %q, %v; want the edited f.txt", got, err)
	}
}

// An index the caller selected with GIT_INDEX_FILE is the one read, as git
// status reads it. Not parallel: sets the environment.
func TestHiddenWorktreeChanges_CallerSelectedIndex(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "x")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	// f.txt is assume-unchanged only in the alternate index, so reading the
	// repository's default index would find nothing hidden.
	alternate := filepath.Join(t.TempDir(), "index")
	for _, args := range [][]string{{"read-tree", "HEAD"}, {"update-index", "--assume-unchanged", "f.txt"}} {
		cmd := execx.NonInteractive(context.Background(), "git", args...)
		cmd.Dir = dir
		cmd.Env = append(gitenv.Isolated(), "GIT_INDEX_FILE="+alternate)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	testutil.WriteFile(t, dir, "f.txt", "edited")
	gitenv.IsolateRepository(t)
	t.Setenv("GIT_INDEX_FILE", alternate)

	got, err := gitrepo.HiddenWorktreeChanges(context.Background(), dir)
	if err != nil || !slices.Equal(got, []string{"f.txt"}) {
		t.Fatalf("HiddenWorktreeChanges = %q, %v; want f.txt, hidden in the selected index", got, err)
	}
}

// A repository with nothing marked hidden needs no temporary index.
func TestHiddenWorktreeChanges_NoneHidden(t *testing.T) {
	gitenv.IsolateRepository(t)
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "x")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	testutil.WriteFile(t, dir, "f.txt", "edited")

	got, err := gitrepo.HiddenWorktreeChanges(context.Background(), dir)
	if err != nil || len(got) != 0 {
		t.Fatalf("HiddenWorktreeChanges = %q, %v; want none", got, err)
	}
}

// A submodule's changes count when the superproject's gitlink is flagged, and
// so do the edits a checked-out submodule's own index hides and the changes
// its own settings keep from git status, in nested submodules too. A
// submodule that is not checked out, or that a sparse checkout left out, has
// nothing to hide.
//
//nolint:tparallel // Parent isolates the environment with t.Setenv, which prevents t.Parallel(); each subtest builds its own repositories
func TestHiddenWorktreeChanges_Submodules(t *testing.T) {
	gitenv.IsolateRepository(t)
	inner := t.TempDir()
	testutil.InitRepo(t, inner)
	testutil.WriteFile(t, inner, "f.txt", "x")
	testutil.RunGit(t, inner, "add", "f.txt")
	testutil.RunGit(t, inner, "commit", "-m", "inner")
	source := t.TempDir()
	testutil.InitRepo(t, source)
	testutil.WriteFile(t, source, "lib.txt", "x")
	testutil.WriteFile(t, source, "other.txt", "x")
	testutil.RunGit(t, source, "add", "lib.txt", "other.txt")
	testutil.RunGit(t, source, "-c", "protocol.file.allow=always", "submodule", "add", inner, "inner")
	testutil.RunGit(t, source, "commit", "-m", "lib")
	for _, tt := range []struct {
		name string
		hide func(t *testing.T, dir string)
		want []string
	}{
		{"edit under an assume-unchanged gitlink", func(t *testing.T, dir string) {
			t.Helper()
			testutil.RunGit(t, dir, "update-index", "--assume-unchanged", "sub")
			testutil.WriteFile(t, filepath.Join(dir, "sub"), "lib.txt", "edited")
		}, []string{"sub"}},
		{"untracked file under a skip-worktree gitlink", func(t *testing.T, dir string) {
			t.Helper()
			testutil.RunGit(t, dir, "update-index", "--skip-worktree", "sub")
			testutil.WriteFile(t, filepath.Join(dir, "sub"), "new.txt", "new")
		}, []string{"sub"}},
		{"flagged gitlink, submodule unchanged", func(t *testing.T, dir string) {
			t.Helper()
			testutil.RunGit(t, dir, "update-index", "--assume-unchanged", "sub")
		}, nil},
		{"edit the submodule's own index hides", func(t *testing.T, dir string) {
			t.Helper()
			sub := filepath.Join(dir, "sub")
			testutil.RunGit(t, sub, "update-index", "--assume-unchanged", "other.txt")
			testutil.WriteFile(t, sub, "other.txt", "edited")
		}, []string{"sub/other.txt"}},
		{"untracked file the submodule's status.showUntrackedFiles hides", func(t *testing.T, dir string) {
			t.Helper()
			sub := filepath.Join(dir, "sub")
			testutil.RunGit(t, sub, "config", "status.showUntrackedFiles", "no")
			testutil.WriteFile(t, sub, "new.txt", "new")
		}, []string{"sub/new.txt"}},
		{"edit in a nested submodule the submodule ignores", func(t *testing.T, dir string) {
			t.Helper()
			sub := filepath.Join(dir, "sub")
			testutil.RunGit(t, sub, "config", "submodule.inner.ignore", "all")
			testutil.WriteFile(t, filepath.Join(sub, "inner"), "f.txt", "edited")
		}, []string{"sub/inner"}},
		{"edit the nested submodule's own index hides", func(t *testing.T, dir string) {
			t.Helper()
			nested := filepath.Join(dir, "sub", "inner")
			testutil.RunGit(t, nested, "update-index", "--assume-unchanged", "f.txt")
			testutil.WriteFile(t, nested, "f.txt", "edited")
		}, []string{"sub/inner/f.txt"}},
		{"submodule not checked out", func(t *testing.T, dir string) {
			t.Helper()
			testutil.RunGit(t, dir, "submodule", "deinit", "-q", "-f", "sub")
			testutil.RunGit(t, dir, "update-index", "--assume-unchanged", "sub")
		}, nil},
		{"submodule left out of a sparse checkout", func(t *testing.T, dir string) {
			t.Helper()
			testutil.RunGit(t, dir, "submodule", "deinit", "-q", "-f", "sub")
			testutil.RunGit(t, dir, "update-index", "--skip-worktree", "sub")
			if err := os.Remove(filepath.Join(dir, "sub")); err != nil {
				t.Fatal(err)
			}
		}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			testutil.InitRepo(t, dir)
			testutil.RunGit(t, dir, "commit", "--allow-empty", "-m", "init")
			testutil.RunGit(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", source, "sub")
			testutil.RunGit(t, dir, "-c", "protocol.file.allow=always", "submodule", "update", "-q", "--init", "--recursive")
			testutil.RunGit(t, dir, "commit", "-m", "add a submodule")
			tt.hide(t, dir)
			if status := testutil.RunGit(t, dir, "--no-optional-locks", "status", "--porcelain", "--ignore-submodules=none"); status != "" {
				t.Fatalf("git status reports the change, so this checks nothing:\n%s", status)
			}

			got, err := gitrepo.HiddenWorktreeChanges(context.Background(), dir)
			if err != nil || !slices.Equal(got, tt.want) {
				t.Fatalf("HiddenWorktreeChanges = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

// A symlink at a submodule's path that leads back to the superproject is not
// searched again, so the search ends. Not parallel: isolates the environment.
func TestHiddenWorktreeChanges_SubmoduleSymlinkCycle(t *testing.T) {
	gitenv.IsolateRepository(t)
	source := t.TempDir()
	testutil.InitRepo(t, source)
	testutil.WriteFile(t, source, "lib.txt", "x")
	testutil.RunGit(t, source, "add", "lib.txt")
	testutil.RunGit(t, source, "commit", "-m", "lib")
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.RunGit(t, dir, "commit", "--allow-empty", "-m", "init")
	testutil.RunGit(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", source, "sub")
	testutil.RunGit(t, dir, "commit", "-m", "add a submodule")
	testutil.RunGit(t, dir, "submodule", "deinit", "-q", "-f", "sub")
	if err := os.Remove(filepath.Join(dir, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".", filepath.Join(dir, "sub")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	got, err := gitrepo.HiddenWorktreeChanges(ctx, dir)
	if err != nil || len(got) != 0 {
		t.Fatalf("HiddenWorktreeChanges = %q, %v; want none, and an end to the search", got, err)
	}
}
