// Package entiredir owns access to the repository's .entire directory.
//
// Every non-test read and write under .entire goes through the single *os.Root
// this package hands out, rather than through an absolute path assembled by the
// caller. Two properties follow from that, and neither survives if a call site
// reverts to os.ReadFile/os.WriteFile on a joined path:
//
//   - Containment. .entire holds paths named from agent-supplied session IDs,
//     tool-use IDs, and tree paths. A root confines every open to the directory
//     at the kernel level, so a crafted name cannot escape it, and a symlink
//     swapped in between resolution and open (TOCTOU) surfaces as an error
//     rather than a redirected write.
//   - One directory handle per worktree. The root is opened once and memoized,
//     so the hot hook paths stop paying an OpenRoot per state file, and every
//     consumer observes the same directory even if .entire is renamed or
//     replaced underneath a long-running process.
//
// Creation is deliberately lazy and split across two entry points. Open creates
// .entire because its callers are about to write; OpenForRead never creates,
// because a command that only looks must leave an untouched repo untouched.
// That distinction is load-bearing for logging in particular: the log file is
// created by the first line actually written, so a command that logs nothing
// (completion, version, help) must not leave an .entire behind.
//
// Reset exists for the paths that delete the directory (entire disable,
// entire clean) and for tests. A cached root that outlives its directory is a
// handle to an unlinked inode: writes through it succeed and land nowhere.
package entiredir

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// dirPerm is the mode .entire is created with. It matches the mode the
// directory already had when each consumer created its own subdirectory.
const dirPerm = 0o750

// Open returns the shared *os.Root for the current worktree's .entire
// directory, creating the directory if it does not exist. Use it from write
// paths; read-only callers want OpenForRead.
//
// The returned root is owned by this package and shared with every other
// caller. Do not close it.
func Open(ctx context.Context) (*os.Root, error) {
	worktreeRoot, err := anchor(ctx)
	if err != nil {
		return nil, err
	}
	return OpenAt(worktreeRoot)
}

// OpenForRead is Open without creating .entire. A missing directory is
// reported as fs.ErrNotExist, which callers classify with errors.Is.
func OpenForRead(ctx context.Context) (*os.Root, error) {
	worktreeRoot, err := anchor(ctx)
	if err != nil {
		return nil, err
	}
	return OpenAtForRead(worktreeRoot)
}

// OpenAt is Open for an explicit worktree root, for the callers that already
// resolved one (or that act on a worktree other than the current directory).
func OpenAt(worktreeRoot string) (*os.Root, error) {
	return open(worktreeRoot, true)
}

// OpenAtForRead is OpenForRead for an explicit worktree root.
func OpenAtForRead(worktreeRoot string) (*os.Root, error) {
	return open(worktreeRoot, false)
}

// Opener returns a thunk that calls Open when invoked, for consumers that are
// handed their storage up front but must not touch the disk until they first
// write (see logging.Config.Root).
func Opener(ctx context.Context) func() (*os.Root, error) {
	return func() (*os.Root, error) { return Open(ctx) }
}

// OpenerAt returns a thunk that calls OpenAt when invoked.
func OpenerAt(worktreeRoot string) func() (*os.Root, error) {
	return func() (*os.Root, error) { return OpenAt(worktreeRoot) }
}

// Path returns the absolute path of the current worktree's .entire directory.
// It is for messages and for the few consumers that must hand a path to an
// external process; it is not an invitation to do I/O on the result.
func Path(ctx context.Context) (string, error) {
	worktreeRoot, err := anchor(ctx)
	if err != nil {
		return "", err
	}
	return filepath.Join(worktreeRoot, paths.EntireDir), nil
}

// Reset closes and forgets every cached root. Call it after deleting .entire:
// a root cached across the deletion refers to an unlinked directory, so writes
// through it succeed and land nowhere.
//
// The registry is shared with the other anchors, so this clears them too. That
// is the behaviour teardown wants — a repo whose .entire just went away is not a
// repo whose .git root is worth keeping — and tests get one reset to call.
func Reset() {
	osroot.ResetShared()
}

// Name converts a repo-relative path under .entire (for example
// paths.EntireTmpDir) to a name relative to the .entire root. The repo-relative
// constants stay as they are because git paths, settings display, and gitignore
// entries all need them; this is the one-line bridge to root-relative I/O.
//
// A path outside .entire is an error rather than a silent passthrough: the
// alternative is a name that resolves inside .entire but means something else.
func Name(repoRelPath string) (string, error) {
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(repoRelPath)))
	if cleaned == paths.EntireDir {
		return ".", nil
	}
	rest, ok := strings.CutPrefix(cleaned, paths.EntireDir+"/")
	if !ok || rest == "" {
		return "", fmt.Errorf("%q is not under %s", repoRelPath, paths.EntireDir)
	}
	return rest, nil
}

// MustName is Name for package-level constants that are known to be under
// .entire. It panics on a path that is not, which can only be a programming
// error in a literal.
func MustName(repoRelPath string) string {
	name, err := Name(repoRelPath)
	if err != nil {
		panic(err)
	}
	return name
}

// anchor resolves the directory .entire sits in: the git worktree root, falling
// back to the current directory only on git's positive "not a repository"
// verdict.
//
// The fallback is load-bearing — `entire enable` runs in a directory that is not
// a repository yet — and gating it on paths.ErrNotARepository is what keeps it
// narrow. That sentinel means git ran and said there is no repository here.
// Every other failure (git off $PATH, a cancelled context, dubious ownership, a
// permission error, malformed output) means "we could not find out", and
// answering that with the current directory is how the paths.AbsPath fallback
// this replaces used to give an agent working in a subdirectory of a real repo
// its own .entire beside itself. A root is only as trustworthy as the directory
// it is opened on, so a base this function is not sure about is not a base it
// hands back.
//
// Callers that name a worktree themselves use OpenAt and never reach this.
func anchor(ctx context.Context) (string, error) {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	switch {
	case err == nil:
		return worktreeRoot, nil
	case !errors.Is(err, paths.ErrNotARepository):
		return "", fmt.Errorf("resolve %s location: %w", paths.EntireDir, err)
	}

	cwd, cwdErr := os.Getwd() //nolint:forbidigo // no repository here; see the doc comment
	if cwdErr != nil {
		return "", fmt.Errorf("resolve %s location: %w", paths.EntireDir, cwdErr)
	}
	return cwd, nil
}

// PathTo returns the absolute path of repoRelPath, which must name something
// under .entire. It exists for the consumers that carry an absolute path in
// their own API — settings resolves one per layer and hands it back to callers
// that print it — so those resolve through this package's anchor rather than
// paths.AbsPath, whose relative fallback is the thing anchor exists to avoid.
func PathTo(ctx context.Context, repoRelPath string) (string, error) {
	if _, err := Name(repoRelPath); err != nil {
		return "", err
	}
	worktreeRoot, err := anchor(ctx)
	if err != nil {
		return "", err
	}
	return filepath.Join(worktreeRoot, filepath.FromSlash(repoRelPath)), nil
}

func open(worktreeRoot string, create bool) (*os.Root, error) {
	if worktreeRoot == "" {
		return nil, errors.New("entiredir: worktree root is required")
	}
	abs, err := filepath.Abs(worktreeRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", worktreeRoot, err)
	}
	return openDir(filepath.Join(abs, paths.EntireDir), create)
}

// openDir is the one place a *os.Root over a .entire directory is created. Every
// entry point funnels here so the create/no-create split stays in a single
// branch; osroot.Shared owns the open-at-most-once part.
func openDir(dir string, create bool) (*os.Root, error) {
	if create {
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return osroot.Shared(dir) //nolint:wrapcheck // Shared names the directory, and returns a missing one unwrapped for errors.Is
}

// OpenPath returns the shared root for the .entire directory containing p,
// together with p's name within it, creating the directory if needed.
// OpenPathForRead is the same without creation.
//
// These exist for the consumers that already carry an absolute path — settings
// resolves one per layer, and hands it back to callers that print it — so those
// APIs keep their shape while the open itself still goes through the one root.
// A path not under a .entire directory yields ok=false from Split and an error
// here; callers that legitimately handle both (settings also reads clone
// preferences out of .git) branch on that rather than guessing.
func OpenPath(p string) (root *os.Root, name string, err error) {
	return openPath(p, true)
}

// OpenPathForRead is OpenPath without creating .entire.
func OpenPathForRead(p string) (root *os.Root, name string, err error) {
	return openPath(p, false)
}

// Split splits a path that lies under a .entire directory into that directory
// and the name of the path within it. It is lexical: the path is cleaned and the
// innermost ".entire" component wins, which is the directory that actually
// contains the file if a repo is ever checked out inside another repo's .entire.
//
// A relative path is accepted, because paths.AbsPath still falls back to one
// when it cannot resolve a worktree root and callers hand that value straight
// through. The walk is component-by-component with filepath rather than a split
// on separators so a Windows volume name survives being rejoined.
func Split(p string) (entireDir, name string, ok bool) {
	cleaned := filepath.Clean(p)
	rest := cleaned
	for {
		dir, base := filepath.Split(rest)
		if base == "" {
			return "", "", false
		}
		if base == paths.EntireDir {
			rel, relErr := filepath.Rel(rest, cleaned)
			// rel == "." means the caller named the directory itself, which is
			// not a file within it.
			if relErr != nil || rel == "." || paths.IsRelativeTraversal(rel) {
				return "", "", false
			}
			return rest, filepath.ToSlash(rel), true
		}
		if dir == "" {
			return "", "", false // reached the start of a relative path
		}
		parent := filepath.Clean(dir)
		if parent == rest {
			return "", "", false // reached the filesystem root
		}
		rest = parent
	}
}

func openPath(p string, create bool) (*os.Root, string, error) {
	dir, name, ok := Split(p)
	if !ok {
		return nil, "", fmt.Errorf("%q is not under a %s directory", p, paths.EntireDir)
	}
	// A relative path is rejected rather than resolved against the current
	// directory: that is the cwd anchor anchor() refuses, reached by another
	// route. Callers resolve their path from the worktree root before getting
	// here.
	if !filepath.IsAbs(dir) {
		return nil, "", fmt.Errorf("%q is not absolute; resolve it against the worktree root first", p)
	}
	root, err := openDir(dir, create)
	if err != nil {
		return nil, "", err
	}
	return root, name, nil
}
