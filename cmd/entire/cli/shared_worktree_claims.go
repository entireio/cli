package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/worktreedir"
)

// git status shows every dirty file in the worktree, not which session
// changed it. A turn end, and a foreground task's completion, use it to find
// edits no transcript names (shell commands), and statusClaim narrows what
// they take from it to what the turn or task could have changed (#3011).

// statusClaim is what a turn end or a foreground task completion needs to
// narrow git status's view of its changes.
type statusClaim struct {
	repoRoot  string
	sessionID string

	// related are sessions whose activity is not another session's turn:
	// the parent of a subagent that runs as a session of its own.
	related []string

	// forTask marks a foreground task's completion, for which the session's
	// own subagent sessions are other tasks' work.
	forTask bool

	// capturedAt is when the prompt-start or task-launch baseline was
	// captured; zero when unknown.
	capturedAt time.Time

	// dirtyTracked is the baseline's DirtyTrackedFileStats.
	dirtyTracked map[string]UntrackedFileStat

	// named are the files the transcript names, repo-relative.
	named []string
}

// narrow returns the files of modified, created and deleted (git status's
// view, repo-relative) that the turn or task can claim:
//
//   - A tracked file that was already dirty when the baseline was captured,
//     and whose stat still matches, is left out: it was not changed since.
//   - While another session in the worktree was active at some point since
//     the capture (strategy.OtherSessionActiveSince), only files the
//     transcript names are kept. Either session could have changed any other
//     file git status lists, and taking it would link this session to the
//     other's commit and show its prompt as that commit's intent. A shell
//     edit made during such an overlap is therefore claimed by neither
//     session, the same trade the background-task path makes
//     (unrecordedNewFilesSinceLaunch). If the other sessions cannot be read,
//     the overlap is assumed.
func (c statusClaim) narrow(ctx context.Context, modified, created, deleted []string) (claimedModified, claimedCreated, claimedDeleted []string) {
	modified = c.withoutUnchangedDirty(modified)
	deleted = c.withoutUnchangedDirty(deleted)
	named := newNamedFiles(c.repoRoot, c.named, modified, created, deleted)
	claimedModified = named.only(modified)
	claimedCreated = named.only(created)
	claimedDeleted = named.only(deleted)
	unnamed := len(modified) + len(created) + len(deleted) - len(claimedModified) - len(claimedCreated) - len(claimedDeleted)
	if unnamed == 0 || c.capturedAt.IsZero() {
		return modified, created, deleted
	}
	logCtx := logging.WithComponent(ctx, "lifecycle")
	overlapped, err := strategy.OtherSessionActiveSince(ctx, c.repoRoot, c.sessionID, c.related, c.capturedAt, c.forTask)
	if err != nil {
		logging.Warn(logCtx, "cannot read other sessions in the worktree; claiming only files the transcript names",
			slog.String("error", err.Error()))
		overlapped = true
	}
	if !overlapped {
		return modified, created, deleted
	}
	logging.Info(logCtx, "another session was active in the worktree; not claiming files only git status shows",
		slog.Int("files", unnamed))
	return claimedModified, claimedCreated, claimedDeleted
}

// withoutUnchangedDirty drops the files the baseline recorded as already
// dirty whose stat still matches.
func (c statusClaim) withoutUnchangedDirty(files []string) []string {
	if len(c.dirtyTracked) == 0 {
		return files
	}
	var kept []string
	for _, file := range files {
		if before, dirty := c.dirtyTracked[file]; dirty {
			if now, ok := statTrackedFile(c.repoRoot, file); ok && now == before {
				continue
			}
		}
		kept = append(kept, file)
	}
	return kept
}

// namedFiles tells which of the files git status lists the transcript names.
// A transcript can spell a path in another case than git does: on a
// case-insensitive filesystem (the macOS and Windows default) an agent that
// writes readme.md changes README.md. So a name also matches a listed file of
// another case, when git status lists no file with the name's exact spelling
// and both spellings open the same file in the worktree. On a case-sensitive
// filesystem readme.md and README.md are two files, and a session that named
// one must not take the other.
//
// A transcript can also name a file through a symlink to a directory in the
// worktree, or to its root (alias/data.txt for alias -> real), which git
// status lists under the directory's own path, real/data.txt. Such a name is matched as its
// directory resolves.
type namedFiles struct {
	repoRoot string
	exact    map[string]struct{}
	// folded maps a name's caseFoldKey to the transcript's spellings of it
	// that git status does not list.
	folded map[string][]string
	// cases asks the worktree's directories about case, only for a
	// deletion named in another case.
	cases *worktreedir.CaseProbe
}

// newNamedFiles matches named against the files git status lists.
func newNamedFiles(repoRoot string, named []string, listed ...[]string) namedFiles {
	inStatus := make(map[string]struct{})
	for _, files := range listed {
		for _, f := range files {
			inStatus[f] = struct{}{}
		}
	}
	n := namedFiles{
		repoRoot: repoRoot,
		exact:    make(map[string]struct{}, len(named)),
		folded:   make(map[string][]string),
		cases:    worktreedir.NewCaseProbe(repoRoot),
	}
	resolved := make(map[string]string)
	for _, f := range named {
		f = withoutDotComponents(f)
		if _, ok := inStatus[f]; !ok {
			f = resolvedDir(repoRoot, f, resolved)
		}
		if _, ok := inStatus[f]; ok {
			n.exact[f] = struct{}{}
		} else {
			key := caseFoldKey(f)
			n.folded[key] = append(n.folded[key], f)
		}
	}
	return n
}

// withoutDotComponents drops a name's "." components and repeated slashes,
// which git status never spells and a transcript can (./notes.txt). Outside
// Windows a name with a ".." component is left for resolvedDir: after a
// symlink to a directory, ".." leaves the link's target, which a lexical
// clean cannot see. Windows takes ".." lexically, before any link.
func withoutDotComponents(name string) string {
	if runtime.GOOS != windowsGOOS && slices.Contains(strings.Split(name, "/"), "..") {
		return name
	}
	return path.Clean(name)
}

// resolvedDir returns file with its directory resolved as the filesystem
// resolves it, symlinks and ".." included, when that leads to a directory in
// the worktree, or else file itself. resolved caches each directory's answer.
func resolvedDir(repoRoot, file string, resolved map[string]string) string {
	// Not path.Dir, which would clean ".." away before the filesystem sees it.
	i := strings.LastIndex(file, "/")
	if i < 0 {
		return file
	}
	dir, base := file[:i], file[i+1:]
	target, ok := resolved[dir]
	if !ok {
		target = dir
		if name, ok := dirName(repoRoot, dir); ok {
			target = name
		}
		resolved[dir] = target
	}
	return path.Join(target, base)
}

// dirName returns the worktree-relative name dir, a repo-relative directory,
// resolves to, and false when it leaves the worktree or does not resolve.
func dirName(repoRoot, dir string) (string, bool) {
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return "", false
	}
	// Not filepath.Join, which would clean ".." away: EvalSymlinks takes it
	// after the links before it.
	name, err := resolvedDirName(root, root+string(filepath.Separator)+filepath.FromSlash(dir))
	return name, err == nil
}

// errOutsideWorktree is resolvedDirName's answer for a directory that
// resolves outside the worktree.
var errOutsideWorktree = errors.New("directory resolves outside the worktree")

// resolvedDirName returns the worktree-relative name the directory at absDir
// resolves to, symlinks and ".." taken as the filesystem takes them, and "."
// for the worktree root itself (a link such as alias -> .). It fails with
// errOutsideWorktree when absDir resolves outside the worktree, and with
// another error when it does not resolve.
func resolvedDirName(repoRoot, absDir string) (string, error) {
	target, err := filepath.EvalSymlinks(absDir)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", absDir, err)
	}
	root, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", repoRoot, err)
	}
	if target == root {
		return ".", nil
	}
	name, err := worktreedir.Name(root, target)
	if err != nil {
		return "", errOutsideWorktree
	}
	return name, nil
}

// only returns the files of all the transcript names, in all's order and
// spelled as git spells them.
func (n namedFiles) only(all []string) []string {
	var kept []string
	for _, f := range all {
		if _, exact := n.exact[f]; exact || n.namedInOtherCase(f) {
			kept = append(kept, f)
		}
	}
	return kept
}

// namedInOtherCase reports whether the transcript names file in another
// case, as a spelling that opens the same file.
//
// A file absent from the worktree (a deletion) has nothing left to compare.
// It matches a spelling that is absent too when each directory whose entry
// the two spellings name in different cases finds its entries ignoring case
// (worktreedir.CaseProbe): there the spellings went together, while on a
// case-sensitive filesystem a spelling that still opens a file is another
// file.
func (n namedFiles) namedInOtherCase(file string) bool {
	spellings := n.folded[caseFoldKey(file)]
	if len(spellings) == 0 {
		return false
	}
	listed, err := lstatWorktreeFile(n.repoRoot, file)
	if errors.Is(err, fs.ErrNotExist) {
		return n.deletedInOtherCase(file, spellings)
	}
	if err != nil {
		return false
	}
	for _, spelling := range spellings {
		if named, err := lstatWorktreeFile(n.repoRoot, spelling); err == nil && os.SameFile(listed, named) {
			return true
		}
	}
	return false
}

// caseFoldKey returns a key two names share when they are equal under
// Unicode simple case folding, as strings.EqualFold and worktreedir.CaseProbe
// compare them: each rune becomes the smallest rune it folds with. Lower case
// is not such a key: Σ lowercases to σ, while ς, which folds with both, stays
// ς. Sharing a key only makes two names candidates; the filesystem still has
// to say they name one file.
func caseFoldKey(name string) string {
	return strings.Map(func(r rune) rune {
		smallest := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			smallest = min(smallest, f)
		}
		return smallest
	}, name)
}

// deletedInOtherCase is namedInOtherCase for a file the worktree lacks.
func (n namedFiles) deletedInOtherCase(file string, spellings []string) bool {
	for _, spelling := range spellings {
		if !n.cases.SameEntry(file, spelling) {
			continue
		}
		if _, err := lstatWorktreeFile(n.repoRoot, spelling); errors.Is(err, fs.ErrNotExist) {
			return true
		}
	}
	return false
}

// statusClaim returns the statusClaim for the turn this state began. A nil
// state has no baseline, and narrows nothing.
func (s *PrePromptState) statusClaim(repoRoot, sessionID string, named []string, related ...string) statusClaim {
	if s == nil {
		return statusClaim{}
	}
	return newStatusClaim(repoRoot, sessionID, s.Timestamp, s.DirtyTrackedFileStats, named, related)
}

// statusClaim returns the statusClaim for the task this state began. A nil
// state has no baseline, and narrows nothing.
func (s *PreTaskState) statusClaim(repoRoot, sessionID string, named []string) statusClaim {
	if s == nil {
		return statusClaim{}
	}
	c := newStatusClaim(repoRoot, sessionID, s.Timestamp, s.DirtyTrackedFileStats, named, nil)
	c.forTask = true
	return c
}

func newStatusClaim(repoRoot, sessionID, timestamp string, dirtyTracked map[string]UntrackedFileStat, named, related []string) statusClaim {
	c := statusClaim{
		repoRoot:     repoRoot,
		sessionID:    sessionID,
		capturedAt:   parseCaptureTime(timestamp),
		dirtyTracked: dirtyTracked,
		named:        named,
	}
	for _, id := range related {
		if id != "" {
			c.related = append(c.related, id)
		}
	}
	return c
}

// parseCaptureTime parses a pre-prompt or pre-task Timestamp; zero when it is
// empty or malformed.
func parseCaptureTime(timestamp string) time.Time {
	t, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return time.Time{}
	}
	return t
}
