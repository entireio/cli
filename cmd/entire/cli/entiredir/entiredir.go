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

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings/repopolicy"
)

// dirPerm is the mode .entire is created with. It matches the mode the
// directory already had when each consumer created its own subdirectory.
const dirPerm = 0o750

// ReadFile reads an Entire-owned file without following a symlink at the leaf.
func ReadFile(root *os.Root, name string) ([]byte, error) {
	return osroot.ReadFileNoFollow(root, name) //nolint:wrapcheck // preserve os error classification
}

// WriteFile atomically replaces an Entire-owned file. Renaming a newly-created
// temporary file over name replaces a planted leaf symlink rather than opening
// and truncating its target.
func WriteFile(root *os.Root, name string, data []byte, perm os.FileMode) error {
	return jsonutil.WriteFileAtomicIn(root, name, data, perm) //nolint:wrapcheck // caller supplies path context
}

// Open returns the shared *os.Root for the current worktree's .entire
// directory, creating the directory if it does not exist. Use it from write
// paths; read-only callers want OpenForRead.
//
// The returned root is owned by this package and shared with every other
// caller. Do not close it.
func Open(ctx context.Context) (*os.Root, error) {
	base, err := runtimeBase(ctx)
	if err != nil {
		return nil, err
	}
	return openDir(base, true)
}

// OpenForRead is Open without creating .entire. A missing directory is
// reported as fs.ErrNotExist, which callers classify with errors.Is.
func OpenForRead(ctx context.Context) (*os.Root, error) {
	base, err := runtimeBase(ctx)
	if err != nil {
		return nil, err
	}
	return openDir(base, false)
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
// The base is resolved eagerly, when the thunk is built: resolution can
// classify the repository policy, and doing that lazily inside the logger's
// first write would let a log line emitted DURING classification re-enter the
// opener. The thunk itself still touches the disk only when first invoked.
func Opener(ctx context.Context) func() (*os.Root, error) {
	base, err := runtimeBase(ctx)
	if err != nil {
		return func() (*os.Root, error) { return nil, err }
	}
	return func() (*os.Root, error) { return openDir(base, true) }
}

// OpenerAt returns a thunk that calls OpenAt when invoked.
func OpenerAt(worktreeRoot string) func() (*os.Root, error) {
	return func() (*os.Root, error) { return OpenAt(worktreeRoot) }
}

// Path returns the absolute path of the current worktree's .entire directory.
// It is for messages and for the few consumers that must hand a path to an
// external process; it is not an invitation to do I/O on the result.
func Path(ctx context.Context) (string, error) {
	return runtimeBase(ctx)
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
	name, err := Name(repoRelPath)
	if err != nil {
		return "", err
	}
	// Runtime-class paths follow the routed base (a globally tracked repo
	// keeps them under the git common dir); config-class paths — the settings
	// files this function mostly serves — are repository content and stay at
	// the worktree anchor whatever the activation tier.
	if paths.IsRuntimeDataPath(repoRelPath) {
		base, baseErr := runtimeBase(ctx)
		if baseErr != nil {
			return "", baseErr
		}
		return filepath.Join(base, filepath.FromSlash(name)), nil
	}
	worktreeRoot, err := anchor(ctx)
	if err != nil {
		return "", err
	}
	return filepath.Join(worktreeRoot, filepath.FromSlash(repoRelPath)), nil
}

// runtimeBase resolves the directory that plays the role of ".entire" for
// this process: <worktree>/.entire under repo-level activation, the git
// common dir's per-worktree runtime root when the global tier owns the repo
// (paths.RuntimeDirBase), and — outside any repository — anchor's cwd
// fallback, which `entire enable` needs before git init. A tier-owned repo
// whose git-side location cannot be resolved fails closed
// (paths.ErrUnroutableRuntimePath): routing uncertainty must never turn into
// worktree writes in a repo the tier keeps invisible.
func runtimeBase(ctx context.Context) (string, error) {
	base, err := paths.RuntimeDirBase(ctx)
	switch {
	case err == nil:
		return base, nil
	case errors.Is(err, paths.ErrNotARepository):
	default:
		return "", fmt.Errorf("resolve %s location: %w", paths.EntireDir, err)
	}
	cwd, cwdErr := os.Getwd() //nolint:forbidigo // no repository here; see anchor's doc comment
	if cwdErr != nil {
		return "", fmt.Errorf("resolve %s location: %w", paths.EntireDir, cwdErr)
	}
	return filepath.Join(cwd, paths.EntireDir), nil // entire-join-ok: this package IS the routing owner; the cwd fallback is the documented non-repo anchor
}

func open(worktreeRoot string, create bool) (*os.Root, error) {
	if worktreeRoot == "" {
		return nil, errors.New("entiredir: worktree root is required")
	}
	abs, err := filepath.Abs(worktreeRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", worktreeRoot, err)
	}
	return openDir(filepath.Join(abs, paths.EntireDir), create) // entire-join-ok: OpenAt is the explicit-root (non-routed) entry point; routed callers use Open(ctx)
}

// openDir is the one place a *os.Root over a .entire directory is created. Every
// entry point funnels here so the create/no-create split stays in a single
// branch; osroot.Shared owns the open-at-most-once part.
func openDir(dir string, create bool) (*os.Root, error) {
	if anchorDir, name, ok := splitRuntimeRoot(dir); ok {
		return openDescendant(anchorDir, name, dir, create)
	}

	parentDir := filepath.Dir(dir)
	name := filepath.Base(dir)
	parent, err := osroot.Shared(parentDir)
	if err != nil {
		return nil, err //nolint:wrapcheck // Shared names the directory, and returns a missing one unwrapped for errors.Is
	}
	if create {
		if err := osroot.MkdirAllNoSymlink(parent, name, dirPerm); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return osroot.SharedChild(parent, dir, name) //nolint:wrapcheck // preserves missing-path classification
}

// splitRuntimeRoot recognizes the one routed layout whose parent directories
// do not already exist. It returns a name relative to the git common dir so
// creation can remain anchored there instead of following an absolute-path
// symlink planted at an Entire-owned component.
func splitRuntimeRoot(dir string) (anchorDir, name string, ok bool) {
	cleaned := filepath.Clean(dir)
	key := filepath.Base(cleaned)
	if !isWorktreeKey(key) {
		return "", "", false
	}

	registry := strings.Split(repopolicy.WorktreeRegistryRelative, "/")
	cursor := filepath.Dir(cleaned)
	for i := len(registry) - 1; i >= 0; i-- {
		if filepath.Base(cursor) != registry[i] {
			return "", "", false
		}
		cursor = filepath.Dir(cursor)
	}
	rel := filepath.Join(filepath.FromSlash(repopolicy.WorktreeRegistryRelative), key)
	if filepath.Join(cursor, rel) != cleaned {
		return "", "", false
	}
	return cursor, filepath.ToSlash(rel), true
}

func openDescendant(anchorDir, name, dir string, create bool) (*os.Root, error) {
	root, err := osroot.Shared(anchorDir)
	if err != nil {
		return nil, fmt.Errorf("open runtime anchor %s: %w", anchorDir, err)
	}
	if create {
		if err := osroot.MkdirAllNoSymlink(root, name, dirPerm); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}

	current := root
	currentDir := anchorDir
	for _, component := range strings.Split(name, "/") {
		currentDir = filepath.Join(currentDir, component)
		current, err = osroot.SharedChild(current, currentDir, component)
		if err != nil {
			return nil, fmt.Errorf("open runtime directory %s: %w", currentDir, err)
		}
	}
	return current, nil
}

// splitRuntime is Split for the global tier's routed runtime layout:
// <git-common-dir>/entire/worktree/<worktree-key>/<name>. The key is the
// fixed-length hex hash worktreeid produces; matching the three-element shape
// keeps this from firing on user paths that merely contain "entire".
func splitRuntime(p string) (base, name string, ok bool) {
	cleaned := filepath.Clean(p)
	elems := strings.Split(filepath.ToSlash(cleaned), "/")
	registry := strings.Split(repopolicy.WorktreeRegistryRelative, "/")
	for i := 0; i+len(registry) < len(elems)-1; i++ {
		match := true
		for j, r := range registry {
			if elems[i+j] != r {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		key := elems[i+len(registry)]
		if !isWorktreeKey(key) {
			continue
		}
		baseElems := i + len(registry) + 1
		if baseElems >= len(elems) {
			return "", "", false // the runtime root itself, not a file within it
		}
		return filepath.FromSlash(strings.Join(elems[:baseElems], "/")),
			strings.Join(elems[baseElems:], "/"), true
	}
	return "", "", false
}

func isWorktreeKey(s string) bool {
	if len(s) != repopolicy.RuntimeKeyLength {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
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
		dir, name, ok = splitRuntime(p)
	}
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
