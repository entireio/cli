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
	"sync"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"

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
// status lists under the directory's own path, real/data.txt. Such a name is
// matched as its directory resolves. A name that is itself a symlink to a
// file git status lists (AGENTS.md -> CLAUDE.md) matches that file, also when
// the link spells it in another case or form that opens the same file, unless
// git status lists the link too, under the name's own spelling, as its
// directory resolves, or in another case that opens the same link: a new or
// retargeted link is the session's change, and the file it leads to may be
// another session's.
type namedFiles struct {
	repoRoot string
	exact    map[string]struct{}
	// folded maps a name's caseFoldKey to the transcript's spellings of it
	// that git status does not list.
	folded map[string][]string
	// cases asks the worktree's directories about case, only for a
	// deletion named in another case.
	cases *worktreedir.CaseProbe
	// ignoresNormalization reports whether the filesystem holding a name
	// takes names in different Unicode normalization forms as one, as
	// macOS's APFS and HFS+ do (worktreedir.IgnoresNormalization). A
	// worktree can hold mounts of other filesystems, so it is asked for the
	// directory of each component that composing changes. Only a deletion,
	// which leaves no file to compare, asks.
	ignoresNormalization func(name string) bool
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
		repoRoot:             repoRoot,
		exact:                make(map[string]struct{}, len(named)),
		folded:               make(map[string][]string),
		cases:                worktreedir.NewCaseProbe(repoRoot),
		ignoresNormalization: normalizationProbe(repoRoot),
	}
	links := listedLinks{repoRoot: repoRoot, inStatus: inStatus}
	resolved := make(map[string]string)
	for _, f := range named {
		f = withoutDotComponents(f)
		if _, ok := inStatus[f]; !ok {
			f = resolvedDir(repoRoot, f, resolved)
			// A write through a link git status does not list changed
			// the file it leads to, which the link may spell in another
			// case or form than git, AGENTS.md -> claude.md for CLAUDE.md.
			// A link git status lists changed itself, and the name is
			// that change: whether the session also wrote through it, or
			// another session wrote the file it now leads to, the name
			// cannot tell.
			if target, ok := linkTarget(repoRoot, f); ok && !links.lists(f) {
				n.add(target, inStatus)
			}
		}
		n.add(f, inStatus)
	}
	return n
}

// normalizationProbe returns worktreedir.IgnoresNormalization for names in
// repoRoot's worktree, which it opens on the first ask, keeping each
// directory's answer.
func normalizationProbe(repoRoot string) func(name string) bool {
	openRoot := sync.OnceValues(func() (*os.Root, error) { return worktreedir.OpenAt(repoRoot) })
	answers := make(map[string]bool)
	return func(name string) bool {
		dir := path.Dir(name)
		if answer, ok := answers[dir]; ok {
			return answer
		}
		root, err := openRoot()
		answer := err == nil && worktreedir.IgnoresNormalization(root, name)
		answers[dir] = answer
		return answer
	}
}

// listedLinks tells whether git status lists a link a transcript names.
type listedLinks struct {
	repoRoot string
	inStatus map[string]struct{}
	// folded maps the caseFoldKey of each file git status lists to its
	// names, built on the first ask.
	folded map[string][]string
}

// lists reports whether git status lists link, in its own spelling or in
// another case or form that opens the same link, as macOS's and Windows'
// filesystems take agents.md for a listed AGENTS.md.
func (l *listedLinks) lists(link string) bool {
	if _, ok := l.inStatus[link]; ok {
		return true
	}
	if l.folded == nil {
		l.folded = make(map[string][]string, len(l.inStatus))
		for f := range l.inStatus {
			key := caseFoldKey(f)
			l.folded[key] = append(l.folded[key], f)
		}
	}
	named, err := lstatWorktreeFile(l.repoRoot, link)
	if err != nil {
		return false
	}
	for _, f := range l.folded[caseFoldKey(link)] {
		if listed, err := lstatWorktreeFile(l.repoRoot, f); err == nil && os.SameFile(named, listed) {
			return true
		}
	}
	return false
}

// add records name as exact when git status lists it, where a lookup
// suffices, and else under its fold key, so that namedInOtherCase can ask the
// filesystem whether it opens a listed file of another case.
func (n namedFiles) add(name string, inStatus map[string]struct{}) {
	if _, ok := inStatus[name]; ok {
		n.exact[name] = struct{}{}
		return
	}
	key := caseFoldKey(name)
	n.folded[key] = append(n.folded[key], name)
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
	name, err := resolvedName(root, root+string(filepath.Separator)+filepath.FromSlash(dir))
	return name, err == nil
}

// linkTarget returns the worktree file that file leads to, and false when
// file is no symlink or leads outside the worktree.
func linkTarget(repoRoot, file string) (string, bool) {
	if info, err := lstatWorktreeFile(repoRoot, file); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		return "", false
	}
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return "", false
	}
	name, err := resolvedName(root, filepath.Join(root, filepath.FromSlash(file)))
	return name, err == nil
}

// errOutsideWorktree is resolvedName's answer for a path that resolves
// outside the worktree.
var errOutsideWorktree = errors.New("directory resolves outside the worktree")

// resolvedName returns the worktree-relative name the file or directory at
// abs resolves to, symlinks and ".." taken as the filesystem takes them, and
// "." for the worktree root itself (a link such as alias -> .). It fails with
// errOutsideWorktree when abs resolves outside the worktree, and with
// another error when it does not resolve.
func resolvedName(repoRoot, abs string) (string, error) {
	root, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", repoRoot, err)
	}
	target, err := evalSymlinksOfExisting(abs, func(entry string) (string, error) {
		return readLink(repoRoot, root, entry)
	})
	if err != nil {
		return "", err
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

// evalSymlinksOfExisting is filepath.EvalSymlinks for a path whose entries
// may no longer all exist, as a deleted file's directory. It resolves the
// path one entry at a time: an entry that does not exist is no symlink, so a
// ".." after one only steps back out of it, and resolution resumes from
// there. A link whose target is gone is still followed by name. So with
// alias -> real/sub, alias/gone/../../file.txt and gone/../alias/../file.txt
// are both real/file.txt, and so is alias/../file.txt once real/sub is
// removed, where a lexical clean gives the root's file.txt. readlink reads
// such a link.
func evalSymlinksOfExisting(abs string, readlink func(entry string) (string, error)) (string, error) {
	target, err := filepath.EvalSymlinks(abs)
	if !errors.Is(err, fs.ErrNotExist) {
		return target, wrapResolveErr(abs, err)
	}
	sep := string(filepath.Separator)
	vol := filepath.VolumeName(abs)
	resolved := vol + sep // a path without links, which exists
	var missing []string  // names under resolved that do not exist
	parts := strings.Split(abs[len(vol):], sep)
	for links := 0; len(parts) > 0; {
		part := parts[0]
		parts = parts[1:]
		switch {
		case part == "" || part == ".":
		case len(missing) > 0 && part == "..":
			missing = missing[:len(missing)-1]
		case len(missing) > 0:
			missing = append(missing, part)
		case part == "..":
			resolved = filepath.Dir(resolved)
		default:
			entry := filepath.Join(resolved, part)
			next, err := filepath.EvalSymlinks(entry)
			if err == nil {
				resolved = next
				continue
			}
			if !errors.Is(err, fs.ErrNotExist) {
				return "", wrapResolveErr(abs, err)
			}
			link, err := readlink(entry)
			if err != nil {
				missing = append(missing, part)
				continue
			}
			if links++; links > maxLinksFollowed {
				return "", fmt.Errorf("resolve %s: too many links", abs)
			}
			if linkVol := filepath.VolumeName(link); filepath.IsAbs(link) {
				resolved, link = linkVol+sep, link[len(linkVol):]
			}
			parts = append(strings.Split(link, sep), parts...)
		}
	}
	return filepath.Join(append([]string{resolved}, missing...)...), nil
}

// maxLinksFollowed bounds evalSymlinksOfExisting on a link cycle, as
// filepath.EvalSymlinks bounds its own.
const maxLinksFollowed = 255

// readLink reads the link at entry, a path without links, through the
// worktree's root when entry is in the worktree (resolvedRoot is the root's
// own path without links). A resolution can pass through a link outside the
// worktree, which no worktree anchor covers; that one is read where it is.
// filepath.EvalSymlinks reads links the same way for resolvedName, since a
// root refuses a link with an absolute target even inside the worktree
// (worktreedir.NameFollowingLinks).
func readLink(repoRoot, resolvedRoot, entry string) (string, error) {
	name, err := worktreedir.Name(resolvedRoot, entry)
	if err != nil {
		link, err := os.Readlink(entry)
		return link, wrapResolveErr(entry, err)
	}
	root, err := worktreedir.OpenAt(repoRoot)
	if err != nil {
		return "", fmt.Errorf("open worktree %s: %w", repoRoot, err)
	}
	link, err := root.Readlink(filepath.FromSlash(name))
	return link, wrapResolveErr(entry, err)
}

// wrapResolveErr names abs in err, a failure to resolve it, or returns nil.
func wrapResolveErr(abs string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("resolve %s: %w", abs, err)
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
// ς. The name is composed (NFC) first, so a decomposed spelling (cafe\u0301)
// shares the key too: a filesystem that ignores normalization, as macOS's
// does, opens both as one file, while git on macOS lists the composed one.
// Sharing a key only makes two names candidates; the filesystem still has to
// say they name one file.
func caseFoldKey(name string) string {
	return strings.Map(func(r rune) rune {
		smallest := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			smallest = min(smallest, f)
		}
		return smallest
	}, norm.NFC.String(name))
}

// deletedInOtherCase is namedInOtherCase for a file the worktree lacks.
func (n namedFiles) deletedInOtherCase(file string, spellings []string) bool {
	for _, spelling := range spellings {
		if !n.sameDeletedEntry(file, spelling) {
			continue
		}
		if _, err := lstatWorktreeFile(n.repoRoot, spelling); errors.Is(err, fs.ErrNotExist) {
			return true
		}
	}
	return false
}

// sameDeletedEntry is CaseProbe.SameEntry, with names that differ in Unicode
// normalization taken as one where the filesystem holding each differing
// component ignores normalization.
func (n namedFiles) sameDeletedEntry(file, spelling string) bool {
	return n.cases.SameEntry(n.composedWhereIgnored(file), n.composedWhereIgnored(spelling))
}

// composedWhereIgnored returns name with each component composed (NFC) that
// the filesystem holding it takes in either form. A worktree on APFS can hold
// a mount that keeps the forms apart, and the other way round, so each
// component that composing changes asks its own directory: in
// cafe\u0301/mnt/x.txt it is the worktree's root, whatever mnt is. The
// filesystem is asked only for a name that is not composed already.
func (n namedFiles) composedWhereIgnored(name string) string {
	if norm.NFC.IsNormalString(name) {
		return name
	}
	parts := strings.Split(name, "/")
	composed := make([]string, len(parts))
	for i, part := range parts {
		composed[i] = part
		if c := norm.NFC.String(part); c != part && n.ignoresNormalization(strings.Join(parts[:i+1], "/")) {
			composed[i] = c
		}
	}
	return strings.Join(composed, "/")
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
