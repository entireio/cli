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
// A spelling is rewritten only when the filesystem ignores case
// (ignoresCase) and it opens the same file as the index's spelling. A
// deleted file has nothing left to open: it is rewritten when the index has
// exactly one spelling of it and that spelling is gone too. On a
// case-sensitive filesystem nothing changes and git is not asked, since
// readme.md and README.md are two files there. If git cannot answer, the
// files are returned as they are.
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
	if err != nil || !ignoresCase(worktree, repoRoot, all) {
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
			file = trackedSpelling(worktree, repoRoot, file, tracked[strings.ToLower(file)])
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

// trackedSpelling returns the one of tracked (the index's spellings of file,
// ignoring case) that file names, or file itself.
func trackedSpelling(worktree *os.Root, repoRoot, file string, tracked []string) string {
	if len(tracked) == 0 || slices.Contains(tracked, file) {
		return file
	}
	named, err := lstatIn(worktree, repoRoot, file)
	if errors.Is(err, fs.ErrNotExist) {
		if len(tracked) == 1 {
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

// ignoresCase reports whether the filesystem holding files ignores case,
// judged by the first of them, or of their parent directories, that exists
// and has a letter in its name: whether it also opens with each letter's case
// swapped. When none can tell (every one is deleted at the top level, or has
// no letters), it reports true: git is asked then, and trackedSpelling's own
// checks still decide.
func ignoresCase(worktree *os.Root, repoRoot string, files []string) bool {
	for _, file := range files {
		for dir := file; dir != "." && dir != "/" && dir != ""; dir = path.Dir(dir) {
			swapped := swapCase(dir)
			if swapped == dir {
				break // no letters here or in any parent
			}
			info, err := lstatIn(worktree, repoRoot, dir)
			if err != nil {
				continue // deleted: ask its parent
			}
			other, err := lstatIn(worktree, repoRoot, swapped)
			if errors.Is(err, fs.ErrNotExist) {
				return false
			}
			if err != nil {
				continue
			}
			return os.SameFile(info, other)
		}
	}
	return true
}

// swapCase returns name with the case of each ASCII letter swapped.
func swapCase(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case 'a' <= r && r <= 'z':
			return r - 'a' + 'A'
		case 'A' <= r && r <= 'Z':
			return r - 'A' + 'a'
		}
		return r
	}, name)
}
