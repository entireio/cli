package strategy

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/worktreedir"
)

// TrackedSpellings rewrites each of files (repo-relative) that git's index
// tracks in another case to the index's spelling, and drops the duplicates
// that leaves, keeping the first occurrence's place.
//
// An agent's transcript or hook reports paths as the agent spelled them. On a
// case-insensitive filesystem (the macOS and Windows default) an agent that
// writes readme.md changes the tracked README.md, but a session that records
// readme.md never matches a commit of README.md: after that commit the
// session keeps readme.md pending for good, and the turn end that merges git
// status's README.md records both.
//
// A spelling is rewritten only when each directory where it differs from the
// index's spelling finds its entries ignoring case (worktreedir.CaseProbe),
// and it opens the same file as the index's spelling: in a directory that
// respects case, two hard-linked names are two entries git tracks apart. A
// deleted file has nothing left to open: it is rewritten when the index has
// exactly one spelling of it and that spelling is gone too. Where no
// directory on a reported path ignores case, nothing changes and git is not
// asked, since readme.md and README.md are two files there. If git cannot
// answer, the files are returned as they are.
func TrackedSpellings(ctx context.Context, repoRoot string, files []string) []string {
	return trackedSpellingsOf(ctx, repoRoot, files)[0]
}

// trackedSpellingsOf is TrackedSpellings for several lists, with one git call
// for all of them.
func trackedSpellingsOf(ctx context.Context, repoRoot string, lists ...[]string) [][]string {
	var all []string
	for _, list := range lists {
		all = append(all, list...)
	}
	if len(all) == 0 {
		return lists
	}
	worktree, err := worktreedir.OpenAt(repoRoot)
	if err != nil {
		return lists
	}
	cases := worktreedir.NewCaseProbe(repoRoot)
	if !anyDirectoryIgnoresCase(cases, all) {
		return lists
	}
	tracked, err := gitrepo.IndexPathsIgnoringCase(ctx, repoRoot, all)
	if err != nil {
		logging.Debug(logging.WithComponent(ctx, "checkpoint"), "could not read git's spelling of reported paths; keeping them as reported",
			slog.String("error", err.Error()))
		return lists
	}
	spelled := make([][]string, len(lists))
	for i, list := range lists {
		seen := make(map[string]struct{}, len(list))
		out := make([]string, 0, len(list))
		for _, file := range list {
			file = trackedSpelling(worktree, cases, repoRoot, file, tracked[strings.ToLower(file)])
			if _, dup := seen[file]; dup {
				continue
			}
			seen[file] = struct{}{}
			out = append(out, file)
		}
		spelled[i] = out
	}
	return spelled
}

// entryCases is what TrackedSpellings asks of a worktreedir.CaseProbe.
type entryCases interface {
	IgnoresCase(dir string) bool
	SameEntry(file, spelling string) bool
}

// trackedSpelling returns the one of tracked (the index's spellings of file,
// ignoring case) that file names, or file itself.
func trackedSpelling(worktree *os.Root, cases entryCases, repoRoot, file string, tracked []string) string {
	if len(tracked) == 0 || slices.Contains(tracked, file) {
		return file
	}
	named, err := lstatIn(worktree, repoRoot, file)
	if errors.Is(err, fs.ErrNotExist) {
		if len(tracked) == 1 && cases.SameEntry(tracked[0], file) {
			if _, err := lstatIn(worktree, repoRoot, tracked[0]); errors.Is(err, fs.ErrNotExist) {
				return tracked[0]
			}
		}
		return file
	}
	if err != nil {
		return file
	}
	for _, spelling := range tracked {
		if !cases.SameEntry(spelling, file) {
			continue
		}
		if info, err := lstatIn(worktree, repoRoot, spelling); err == nil && os.SameFile(named, info) {
			return spelling
		}
	}
	return file
}

func lstatIn(worktree *os.Root, repoRoot, file string) (fs.FileInfo, error) {
	name, err := worktreedir.Name(repoRoot, file)
	if err != nil {
		return nil, err //nolint:wrapcheck // callers only test for success and fs.ErrNotExist
	}
	return worktree.Lstat(name) //nolint:wrapcheck // callers only test for success and fs.ErrNotExist
}

// anyDirectoryIgnoresCase reports whether a directory on the path to one of
// files, from the one holding it up to the worktree root, finds its entries
// under a name in another case. Where none does, a spelling in another case
// is another file, and git need not be asked. The directory holding the file
// alone does not tell: on Windows, SRC/App.go in a root that ignores case
// opens src/App.go even when src/ respects case.
func anyDirectoryIgnoresCase(cases entryCases, files []string) bool {
	for _, file := range files {
		for dir := path.Dir(file); ; dir = path.Dir(dir) {
			if cases.IgnoresCase(dir) {
				return true
			}
			if dir == "." || dir == "/" {
				break
			}
		}
	}
	return false
}
