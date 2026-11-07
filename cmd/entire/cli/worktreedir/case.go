package worktreedir

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// CaseProbe tells, for directories in one worktree, whether they find their
// entries under a name in another case, as the macOS and Windows defaults do.
// It asks the filesystem rather than git's core.ignorecase, one setting for
// the whole repository that a linked worktree on another filesystem, a moved
// repository or a Windows directory with its own case sensitivity can
// disagree with. Each directory is asked at most once.
type CaseProbe struct {
	root *os.Root
	dirs map[string]bool
}

// NewCaseProbe returns a CaseProbe for the worktree at worktreeRoot. If the
// worktree cannot be opened, the probe answers false.
func NewCaseProbe(worktreeRoot string) *CaseProbe {
	root, err := OpenAt(worktreeRoot)
	if err != nil {
		root = nil
	}
	return &CaseProbe{root: root, dirs: make(map[string]bool)}
}

// IgnoresCase reports whether the directory dir (slash-separated, "." for
// the worktree root) finds its entries under a name in another case.
//
// An entry with an ASCII letter in its name tells: the name with its letters'
// case swapped opens the same file only where lookups ignore case. A
// directory that is gone, or that has no such entry, is answered by the
// nearest ancestor that can tell, and false when none can.
func (p *CaseProbe) IgnoresCase(dir string) bool {
	if p.root == nil {
		return false
	}
	if ignores, asked := p.dirs[dir]; asked {
		return ignores
	}
	ignores := lookupIgnoresCase(p.root, dir)
	p.dirs[dir] = ignores
	return ignores
}

// SameEntry reports whether file and spelling, slash-separated paths that
// differ only in case, name the same entry: each directory holding a
// component where they differ finds its entries ignoring case. It does not
// look at the entries themselves, so it answers for paths that are gone too.
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

func lookupIgnoresCase(root *os.Root, dir string) bool {
	for {
		if ignores, told := entriesIgnoreCase(root, dir); told {
			return ignores
		}
		if dir == "." || dir == "" {
			return false
		}
		dir = path.Dir(dir)
	}
}

// entriesIgnoreCase is lookupIgnoresCase for dir alone; told is false when
// dir cannot be read or has no entry that tells.
func entriesIgnoreCase(root *os.Root, dir string) (ignores, told bool) {
	d, closeDir, err := osroot.OpenDirNoSymlinks(root, dir)
	if err != nil {
		return false, false
	}
	defer closeDir()
	entries, err := osroot.ReadDir(d, ".")
	if err != nil {
		return false, false
	}
	for _, entry := range entries {
		name := entry.Name()
		swapped := SwapASCIICase(name)
		if swapped == name {
			continue
		}
		info, err := d.Lstat(name)
		if err != nil {
			continue
		}
		other, err := d.Lstat(swapped)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return false, true
		case err == nil:
			return os.SameFile(info, other), true
		}
	}
	return false, false
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
