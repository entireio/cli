package strategy

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/worktreedir"

	"github.com/go-git/go-git/v6/plumbing/object"
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
	return trackedSpellingsOf(ctx, repoRoot, gitrepo.IndexPathsIgnoringCase, spellingList{files: files})[0]
}

// commitTrackedSpellings is TrackedSpellings for paths a commit hook checks
// against the commit git is preparing. When repoRoot is the worktree the hook
// runs in, the spellings come from the commit's own index: `git commit -a`
// and path-limited commits prepare it in a temporary index, which can spell a
// path otherwise than the worktree's index (a staged rename of README.md to
// readme.md, with only README.md committed), and the commit's paths come from
// that index too (getStagedChanges). Another worktree's paths are spelled as
// its own index does.
func commitTrackedSpellings(ctx context.Context, repoRoot string, files []string) []string {
	index := gitrepo.IndexPathsIgnoringCase
	if root, err := paths.WorktreeRoot(ctx); err == nil && sameDir(root, repoRoot) {
		index = gitrepo.CommitIndexPathsIgnoringCase
	}
	return trackedSpellingsOf(ctx, repoRoot, index, spellingList{files: files})[0]
}

// spelledAsCommitted returns files with each one the commit holds in another
// case respelled as the commit spells it, by trackedSpelling's rule with the
// commit's paths in place of an index's.
//
// A session records a path as the worktree's index spelled it when the
// session saw it, and a commit can spell it otherwise: with a staged rename
// of README.md to readme.md, a tool hook records readme.md, and a
// path-limited commit of README.md, which git prepares in a temporary index,
// commits README.md. Matching the commit's own paths rather than an index
// gives PrepareCommitMsg and PostCommit, which see different indexes, the
// same answer.
//
// It asks the filesystem only when the commit holds a file in another case
// than one of files, so an ordinary commit pays a map lookup per file.
func spelledAsCommitted(repoRoot string, files []string, commitFiles map[string]struct{}) []string {
	if repoRoot == "" || len(files) == 0 || len(commitFiles) == 0 {
		return files
	}
	committed := make(map[string][]string, len(commitFiles))
	for file := range commitFiles {
		key := strings.ToLower(file)
		committed[key] = append(committed[key], file)
	}
	if !slices.ContainsFunc(files, func(file string) bool {
		_, exact := commitFiles[file]
		return !exact && len(committed[strings.ToLower(file)]) > 0
	}) {
		return files
	}
	worktree, err := worktreedir.OpenAt(repoRoot)
	if err != nil {
		return files
	}
	cases := worktreedir.NewCaseProbe(repoRoot)
	if !anyDirectoryIgnoresCase(cases, files) {
		return files
	}
	spelled := make([]string, 0, len(files))
	for _, file := range files {
		spellings := committed[strings.ToLower(file)]
		slices.Sort(spellings)
		spelled = append(spelled, trackedSpelling(worktree, cases, repoRoot, file, spellings, false))
	}
	return spelled
}

// carriedSpelledAsCommitted returns files, a session's files, and hashes, its
// recorded hashes, for carry-forward after the commit: a file the commit
// does not hold under that spelling but changed in another case, that the
// index no longer tracks under that spelling but tracks in another case that
// the name opens, or names in a directory that ignores case once both are
// gone (trackedSpelling), is carried as the index spells it, its hash too.
// After the commit of a case-only rename of README.md to readme.md, README.md
// still opens readme.md where the filesystem ignores case, so carry-forward
// would find the removed README.md still in the worktree and keep the
// committed file pending for good. With readme.md removed from the worktree
// before that commit, carry-forward would instead drop README.md as gone from
// both, and with it the session's claim on the deletion still pending. The
// same holds for a file the session created as NEW.md that the user staged
// and renamed to new.md before committing: the commit adds new.md and never
// had NEW.md. A spelling the index still tracks stays the session's to
// commit, as readme.md does after a commit of README.md that leaves a staged
// rename to readme.md.
func carriedSpelledAsCommitted(ctx context.Context, repoRoot string, commitTree *object.Tree, files []string, hashes map[string]string, committedFileSet map[string]struct{}) ([]string, map[string]string) {
	renamed := renamedInAnotherCase(repoRoot, commitTree, files, committedFileSet)
	if len(renamed) == 0 {
		return files, hashes
	}
	lists := make([]spellingList, len(renamed))
	for j, i := range renamed {
		lists[j] = spellingList{files: files[i : i+1]}
	}
	spelledLists := trackedSpellingsOf(ctx, repoRoot, gitrepo.IndexPathsIgnoringCase, lists...)
	spelled := slices.Clone(files)
	for j, i := range renamed {
		spelled[i] = spelledLists[j][0]
	}
	return respelled(files, spelled, hashes)
}

// renamedInAnotherCase returns the indexes of the files the commit does not
// hold under their spelling but changed in another case, and that still open
// a file in repoRoot, or open nothing there: the old spelling of a case-only
// rename where the filesystem ignores case, which trackedSpelling decides.
// The commit removed that spelling, or never had it, when the file renamed
// was a staged addition. An ordinary commit pays a map lookup per file.
func renamedInAnotherCase(repoRoot string, commitTree *object.Tree, files []string, committedFileSet map[string]struct{}) []int {
	if repoRoot == "" || commitTree == nil || len(files) == 0 || len(committedFileSet) == 0 {
		return nil
	}
	committed := make(map[string]int, len(committedFileSet))
	for file := range committedFileSet {
		committed[strings.ToLower(file)]++
	}
	var renamed []int
	var worktree *os.Root
	for i, file := range files {
		otherSpellings := committed[strings.ToLower(file)]
		if _, changed := committedFileSet[file]; changed {
			otherSpellings--
		}
		if otherSpellings == 0 {
			continue
		}
		if _, err := commitTree.File(file); err == nil {
			continue
		}
		if worktree == nil {
			root, err := worktreedir.OpenAt(repoRoot)
			if err != nil {
				return nil
			}
			worktree = root
		}
		if _, err := lstatIn(worktree, repoRoot, file); err == nil || errors.Is(err, fs.ErrNotExist) {
			renamed = append(renamed, i)
		}
	}
	return renamed
}

// respelled returns files with each one replaced by spelled's entry at the
// same place, once each, and hashes with each new spelling's hash: the one
// hash recorded for every spelling that became it. hashes itself is not
// changed.
//
// Spellings that become one need not agree. A session that recorded
// readme.md, then README.md after a case-only rename and another edit, holds
// an older hash under one and the newer under the other, and a per-tool hook
// records an edit with no hash at all (MergeUnhashedFilesTouched). Which
// record the file's remaining work matches is unknown, so then the new
// spelling keeps no hash, and the file is judged as one recorded without one:
// by its name for an overlap, by whether the worktree still differs from the
// commit for carry-forward (filesWithRemainingAgentChanges). Keeping a hash
// could find older content committed and drop a newer edit.
func respelled(files, spelled []string, hashes map[string]string) ([]string, map[string]string) {
	if slices.Equal(spelled, files) {
		return files, hashes
	}
	spelledFiles := make([]string, 0, len(spelled))
	sources := make(map[string][]string, len(spelled))
	for i, file := range spelled {
		if _, dup := sources[file]; !dup {
			spelledFiles = append(spelledFiles, file)
		}
		sources[file] = append(sources[file], files[i])
	}
	spelledHashes := maps.Clone(hashes)
	for file, from := range sources {
		if len(from) == 1 && from[0] == file {
			continue
		}
		if hash, ok := agreedHash(hashes, file, from); ok {
			spelledHashes[file] = hash
		} else {
			delete(spelledHashes, file)
		}
	}
	return spelledFiles, spelledHashes
}

// agreedHash returns the hash recorded for each of from, the spellings that
// became file, and for file itself if one is recorded under it, and false
// when one of from has none or two differ.
func agreedHash(hashes map[string]string, file string, from []string) (string, bool) {
	agreed, ok := hashes[file]
	for _, spelling := range from {
		hash, recorded := hashes[spelling]
		if !recorded {
			return "", false
		}
		if ok && hash != agreed {
			return "", false
		}
		agreed, ok = hash, true
	}
	return agreed, ok
}

// indexSpellings returns the spellings an index tracks for files, ignoring
// case, as gitrepo.IndexPathsIgnoringCase does.
type indexSpellings func(ctx context.Context, repoRoot string, files []string) (map[string][]string, error)

// spellingList is one list of paths trackedSpellingsOf respells. deleted
// marks paths reported as deleted (see trackedSpelling).
type spellingList struct {
	files   []string
	deleted bool
}

// trackedSpellingsOf is TrackedSpellings for several lists, with one call of
// index for all of them.
func trackedSpellingsOf(ctx context.Context, repoRoot string, index indexSpellings, lists ...spellingList) [][]string {
	unchanged := make([][]string, len(lists))
	var all []string
	for i, list := range lists {
		unchanged[i] = list.files
		all = append(all, list.files...)
	}
	if len(all) == 0 {
		return unchanged
	}
	worktree, err := worktreedir.OpenAt(repoRoot)
	if err != nil {
		return unchanged
	}
	cases := worktreedir.NewCaseProbe(repoRoot)
	if !anyDirectoryIgnoresCase(cases, all) {
		return unchanged
	}
	tracked, err := index(ctx, repoRoot, all)
	if err != nil {
		logging.Debug(logging.WithComponent(ctx, "checkpoint"), "could not read git's spelling of reported paths; keeping them as reported",
			slog.String("error", err.Error()))
		return unchanged
	}
	spelled := make([][]string, len(lists))
	for i, list := range lists {
		seen := make(map[string]struct{}, len(list.files))
		out := make([]string, 0, len(list.files))
		for _, file := range list.files {
			file = trackedSpelling(worktree, cases, repoRoot, file, tracked[strings.ToLower(file)], list.deleted)
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

// entryCases is what TrackedSpellings asks of a worktreedir.CaseProbe, so
// that a test can stand in for a directory that ignores case.
type entryCases interface {
	IgnoresCase(dir string) bool
	SameEntry(file, spelling string) bool
}

// trackedSpelling returns the one of tracked (the index's spellings of file,
// ignoring case) that file names, or file itself.
//
// A path reported as deleted that still opens a file keeps its spelling. git
// status reports a staged case-only rename of README.md to readme.md as
// README.md deleted and readme.md added, and where the filesystem ignores case
// README.md still opens readme.md. The deletion is of HEAD's README.md, the
// spelling under which a session may have recorded editing it; respelled, it
// would not mark that edit deleted, and the session would stay pending after
// the rename is committed.
func trackedSpelling(worktree *os.Root, cases entryCases, repoRoot, file string, tracked []string, deleted bool) string {
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
	if err != nil || deleted {
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
