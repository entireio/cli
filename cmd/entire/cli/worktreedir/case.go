package worktreedir

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// maxCaseProbeEntries bounds how many entries CaseProbe reads from one
// directory. A larger directory is not asked, and answers false.
const maxCaseProbeEntries = 4096

// CaseProbe tells, for directories in one worktree, whether they find their
// entries under a name in another case, as the macOS and Windows defaults do.
// It asks the filesystem rather than git's core.ignorecase, one setting for
// the whole repository that a linked worktree on another filesystem, a moved
// repository or a Windows directory with its own case sensitivity can
// disagree with. Each directory is asked at most once.
type CaseProbe struct {
	root *os.Root
	dirs map[string]bool

	// maxEntries is maxCaseProbeEntries, and lookUp is the directory's
	// Lstat; tests replace them.
	maxEntries int
	lookUp     func(d *os.Root, dir, name string) (fs.FileInfo, error)
}

// NewCaseProbe returns a CaseProbe for the worktree at worktreeRoot. If the
// worktree cannot be opened, the probe answers false.
func NewCaseProbe(worktreeRoot string) *CaseProbe {
	root, err := OpenAt(worktreeRoot)
	if err != nil {
		root = nil
	}
	return &CaseProbe{
		root:       root,
		dirs:       make(map[string]bool),
		maxEntries: maxCaseProbeEntries,
		lookUp: func(d *os.Root, _, name string) (fs.FileInfo, error) {
			return d.Lstat(name)
		},
	}
}

// IgnoresCase reports whether the directory dir (slash-separated, "." for
// the worktree root) finds its entries under a name in another case.
//
// An entry with an ASCII letter in its name tells: the name with its letters'
// case swapped opens the same file only where lookups ignore case, unless
// the directory also lists the swapped name as an entry of its own (a hard
// link), which only a directory that respects case can hold. A directory that
// is gone, cannot be read, has no such entry, or has more than
// maxCaseProbeEntries entries cannot tell, and answers false: not knowing is
// no evidence that two spellings name one entry. Neither is the parent's
// answer, since Windows lets a directory respect case inside one that
// ignores it.
func (p *CaseProbe) IgnoresCase(dir string) bool {
	if p.root == nil {
		return false
	}
	if ignores, asked := p.dirs[dir]; asked {
		return ignores
	}
	ignores := p.entriesIgnoreCase(dir)
	p.dirs[dir] = ignores
	return ignores
}

// SameEntry reports whether file and spelling, slash-separated paths that
// differ only in case, name the same entry: each directory holding a
// component where they differ finds its entries ignoring case. It does not
// look at the entries themselves, so it answers for files that are gone too.
func (p *CaseProbe) SameEntry(file, spelling string) bool {
	parts, others := strings.Split(file, "/"), strings.Split(spelling, "/")
	if len(parts) != len(others) {
		return false
	}
	dir := "."
	for i, part := range parts {
		if part != others[i] && (!strings.EqualFold(part, others[i]) || !p.IgnoresCase(dir)) {
			return false
		}
		dir = path.Join(dir, part)
	}
	return true
}

// entriesIgnoreCase is IgnoresCase without the cache.
func (p *CaseProbe) entriesIgnoreCase(dir string) bool {
	d, closeDir, err := osroot.OpenDirNoSymlinks(p.root, dir)
	if err != nil {
		return false
	}
	defer closeDir()
	entries, ok := readEntries(d, p.maxEntries)
	if !ok {
		return false
	}
	listed := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		listed[entry.Name()] = struct{}{}
	}
	for _, entry := range entries {
		name := entry.Name()
		swapped := SwapASCIICase(name)
		if swapped == name {
			continue
		}
		if _, ownEntry := listed[swapped]; ownEntry {
			return false
		}
		info, err := p.lookUp(d, dir, name)
		if err != nil {
			continue
		}
		other, err := p.lookUp(d, dir, swapped)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return false
		case err == nil:
			return os.SameFile(info, other)
		}
	}
	return false
}

// readEntries reads d's entries, in directory order. ok is false when d
// cannot be read or has more than limit entries.
func readEntries(d *os.Root, limit int) (entries []os.DirEntry, ok bool) {
	f, err := d.Open(".")
	if err != nil {
		return nil, false
	}
	defer f.Close()
	entries, err = f.ReadDir(limit + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false
	}
	return entries, len(entries) <= limit
}

// SwapASCIICase swaps the case of name's ASCII letters.
func SwapASCIICase(name string) string {
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
