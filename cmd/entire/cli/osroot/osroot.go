// Package osroot provides traversal-resistant file I/O helpers built on os.Root
// (Go 1.24+). These helpers ensure that file operations cannot escape a scoped
// directory, preventing symlink attacks and TOCTOU races at the kernel level.
//
// These wrappers predate Go 1.25, which added native ReadFile/WriteFile/MkdirAll
// (etc.) on *os.Root; they remain as the codebase's stable, consistent helper
// surface and delegate to the native methods where those now exist.
//
// Errors from these functions are returned unwrapped so that callers can use
// os.IsNotExist() and errors.Is() directly without losing the original sentinel.
package osroot

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// ReadFile reads the named file relative to root using os.Root for
// traversal-resistant access. The kernel enforces that the read cannot
// escape the root directory, preventing symlink and TOCTOU attacks.
func ReadFile(root *os.Root, name string) ([]byte, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // preserve original error for os.IsNotExist checks
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err //nolint:wrapcheck // preserve original error
	}
	return data, nil
}

// ReadFileNoFollow reads name while refusing a symbolic link at the leaf.
// os.Root already prevents a link from escaping root, but follows links whose
// targets remain inside it. Entire-owned trees require the stronger property:
// a name must identify the file stored at that name, not another file in the
// same tree.
func ReadFileNoFollow(root *os.Root, name string) ([]byte, error) {
	f, err := OpenNoFollow(root, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err //nolint:wrapcheck // preserve original error
	}
	return data, nil
}

// OpenNoFollow opens an existing file and verifies that the directory entry is
// a non-symlink referring to the opened object. The second check closes the
// Lstat/Open race; once verified, later replacements cannot redirect the open
// descriptor.
func OpenNoFollow(root *os.Root, name string) (*os.File, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // preserve original error classification
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s: %w", name, ErrSymlinkedPath)
	}

	f, err := root.Open(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // preserve original error classification
	}
	if err := validateOpenedFile(root, name, f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// OpenFileNoFollow opens or creates a file without following a leaf symlink.
// It is intended for append-style writers: O_TRUNC is rejected because
// truncating Entire-owned writes should use an atomic temp-file-and-rename
// operation instead. Unix builds add O_NOFOLLOW to close the write-before-
// validation race; other platforms still reject pre-existing links and verify
// the opened object before returning it.
func OpenFileNoFollow(root *os.Root, name string, flag int, perm os.FileMode) (*os.File, error) {
	if flag&os.O_TRUNC != 0 {
		return nil, errors.New("osroot: OpenFileNoFollow does not permit O_TRUNC")
	}
	if before, err := root.Lstat(name); err == nil {
		if before.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s: %w", name, ErrSymlinkedPath)
		}
	} else if !os.IsNotExist(err) {
		return nil, err //nolint:wrapcheck // preserve original error classification
	}

	f, err := root.OpenFile(name, flag|noFollowOpenFlag, perm)
	if err != nil {
		// Preserve the package sentinel when a link appeared after the first
		// Lstat and O_NOFOLLOW rejected it.
		if info, lstatErr := root.Lstat(name); lstatErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s: %w", name, ErrSymlinkedPath)
		}
		return nil, err //nolint:wrapcheck // preserve original error classification
	}
	if err := validateOpenedFile(root, name, f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func validateOpenedFile(root *os.Root, name string, f *os.File) error {
	pathInfo, err := root.Lstat(name)
	if err != nil {
		return err //nolint:wrapcheck // preserve original error classification
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s: %w", name, ErrSymlinkedPath)
	}
	openedInfo, err := f.Stat()
	if err != nil {
		return err //nolint:wrapcheck // preserve original error classification
	}
	if !os.SameFile(pathInfo, openedInfo) {
		return fmt.Errorf("%s changed while it was being opened", name)
	}
	return nil
}

// WriteFile writes data to the named file relative to root using os.Root
// for traversal-resistant access. Creates the file if it doesn't exist,
// truncates it if it does. The kernel enforces that the write cannot escape
// the root directory.
func WriteFile(root *os.Root, name string, data []byte, perm os.FileMode) (retErr error) {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err //nolint:wrapcheck // preserve original error for os.IsNotExist checks
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && retErr == nil {
			retErr = closeErr
		}
	}()

	if _, err := f.Write(data); err != nil {
		return err //nolint:wrapcheck // preserve original error
	}
	return nil
}

// MkdirAll creates the directory named by name, along with any necessary
// parents, relative to root. The kernel enforces containment: a name that
// escapes root (absolute, or climbing above it via "..") is rejected. Already-
// existing directories are tolerated, like os.MkdirAll. This thin wrapper keeps
// the package's os.Root helper surface (alongside ReadFile/WriteFile/Remove)
// consistent at call sites.
func MkdirAll(root *os.Root, name string, perm os.FileMode) error {
	return root.MkdirAll(name, perm) //nolint:wrapcheck // preserve original error for errors.Is/os.IsNotExist
}

// Remove removes the named file relative to root using os.Root for
// traversal-resistant access. Returns nil if the file doesn't exist.
func Remove(root *os.Root, name string) error {
	err := root.Remove(name)
	if err != nil && !os.IsNotExist(err) {
		return err //nolint:wrapcheck // preserve original error
	}
	return nil
}

// ReadDir reads the named directory relative to root, returning its entries
// sorted by filename, like os.ReadDir. os.Root has no ReadDir method of its
// own, so this is the sanctioned way to list a directory without falling back
// to an unconfined os.ReadDir on a joined path.
func ReadDir(root *os.Root, name string) ([]os.DirEntry, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // preserve original error for os.IsNotExist checks
	}
	defer f.Close()

	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err //nolint:wrapcheck // preserve original error
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int {
		return strings.Compare(a.Name(), b.Name())
	})
	return entries, nil
}

// ErrSymlinkedPath reports a symlink found where a real directory was required.
// Callers match it with errors.Is.
var ErrSymlinkedPath = errors.New("path component is a symlink")

// MkdirAllNoSymlink is MkdirAll with one added refusal: if any component of
// name already exists as a symlink, it returns an error wrapping
// ErrSymlinkedPath instead of creating anything.
//
// os.Root alone is not enough here. It refuses to follow a symlink that escapes
// the root, but a symlink pointing elsewhere *inside* the root is followed
// silently, and an escaping one fails later with whatever errno the first open
// happens to hit — which surfaces to the user as an unexplained I/O failure far
// from the cause. Checking at the point the directory is established turns both
// into one named error while the caller still has the context to report it.
//
// This is a check against a symlink that is already there — planted by hand, or
// arriving with a checkout — not a defence against one appearing between the
// check and the Mkdir. That race is not worth closing: os.Root still bounds the
// result to inside the root either way, so the worst case is the silent-follow
// behaviour that existed before this function.
func MkdirAllNoSymlink(root *os.Root, name string, perm os.FileMode) error {
	for _, prefix := range dirPrefixes(name) {
		info, err := root.Lstat(prefix)
		if err != nil {
			if os.IsNotExist(err) {
				continue // MkdirAll below creates it as a real directory
			}
			return err //nolint:wrapcheck // preserve the original for errors.Is/os.IsNotExist
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s: %w", prefix, ErrSymlinkedPath)
		}
	}
	return root.MkdirAll(name, perm) //nolint:wrapcheck // preserve original error for errors.Is/os.IsNotExist
}

// ErrWalkRootNotDirectory reports a walk root that exists but is not a
// directory. It is deliberately NOT ErrSymlinkedPath: "replace this link" and
// "this is a regular file" are different remedies, and conflating them is how a
// user gets told to fix the wrong thing.
var ErrWalkRootNotDirectory = errors.New("path is not a directory")

// lstatWalkRoot is the check fs.WalkDir does not do for itself.
//
// fs.WalkDir obtains the DirEntry for its ROOT from fs.Stat, which follows a
// symlink; only the entries below it come from ReadDir, which does not. So a
// callback that inspects d.Type() for ModeSymlink — every one in this codebase
// does — is dead code for the walk root, and a symlinked root is silently
// descended into and reported as a real directory. filepath.Walk did lstat its
// root, so this was lost in the move to os.Root, not absent from the start.
//
// Verified against go1.26: with meta/sess -> ../real, fs.WalkDir yields
// meta/sess (isdir=true) then meta/sess/secret.txt, while filepath.Walk yielded
// only the link.
func lstatWalkRoot(root *os.Root, dir string) error {
	info, err := root.Lstat(dir)
	if err != nil {
		return err //nolint:wrapcheck // preserve os.IsNotExist for the caller
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s: %w", dir, ErrSymlinkedPath)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s: %w", dir, ErrWalkRootNotDirectory)
	}
	return nil
}

// WalkDirNoSymlinks walks dir within root, refusing a symlink anywhere it goes:
// at the walk root, and at every entry beneath it.
//
// Refusing rather than skipping is the point. These walks copy an
// Entire-owned tree into a checkpoint, or read the rules that decide what gets
// redacted out of one, and Entire never puts a symlink in either. So a link
// there was planted, arrived with a checkout, or is a genuine mistake — and the
// two quiet answers are both wrong. Following it stores some other tree's
// contents under Entire's names; skipping it drops the session's content out of
// the checkpoint with nobody told. The caller stops and says which path.
//
// A missing dir is reported unwrapped, so callers can keep classifying it with
// os.IsNotExist.
func WalkDirNoSymlinks(root *os.Root, dir string, fn fs.WalkDirFunc) error {
	if err := lstatWalkRoot(root, dir); err != nil {
		return err
	}
	return fs.WalkDir(root.FS(), dir, func(name string, d fs.DirEntry, err error) error { //nolint:wrapcheck // the callback's errors are the caller's own
		if err == nil && d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s: %w", name, ErrSymlinkedPath)
		}
		return fn(name, d, err)
	})
}

// SymlinkPaths walks dir within root and returns the names of every symlink it
// finds, in the root's coordinates. It never follows one, so a symlinked
// directory is reported and not descended into.
//
// This is doctor's reporter, so unlike WalkDirNoSymlinks it does not stop at
// the first one — the whole point is to list them. It does share the walk-root
// blindness fix: a symlinked dir is reported as itself rather than followed and
// its target's contents enumerated as if they were Entire's.
//
// A missing dir yields no names and no error: nothing there is nothing wrong.
func SymlinkPaths(root *os.Root, dir string) ([]string, error) {
	switch err := lstatWalkRoot(root, dir); {
	case err == nil:
	case os.IsNotExist(err):
		return nil, nil
	case errors.Is(err, ErrSymlinkedPath):
		return []string{dir}, nil
	case errors.Is(err, ErrWalkRootNotDirectory):
		return nil, nil // not a directory, so nothing below it to report
	default:
		return nil, err
	}

	var found []string
	err := fs.WalkDir(root.FS(), dir, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			if name == dir && os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			found = append(found, name)
		}
		return nil
	})
	if err != nil {
		return nil, err //nolint:wrapcheck // caller names the directory it asked about
	}
	return found, nil
}

// dirPrefixes returns each ancestor of name in order, including name itself,
// and nothing for "." or "". Names are slash-separated, as os.Root requires.
func dirPrefixes(name string) []string {
	cleaned := path.Clean(name)
	if cleaned == "." || cleaned == "" || cleaned == "/" {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(cleaned, "/"), "/")
	prefixes := make([]string, 0, len(parts))
	for i := range parts {
		prefixes = append(prefixes, strings.Join(parts[:i+1], "/"))
	}
	return prefixes
}

// sharedRoots is the process-wide registry behind Shared. Entire anchors roots
// at three directories — .entire, the git common dir, and the worktree root —
// and each had its own memoized map before this existed. One registry means one
// place that decides a directory is opened at most once, and one Reset.
var (
	sharedMu    sync.Mutex
	sharedRoots = map[string]*os.Root{}
)

// Shared returns a process-wide *os.Root for dir, opening it at most once. The
// returned root is owned by the registry and shared with every other caller: do
// not close it.
//
// dir must be absolute. A relative key would name different directories as the
// process moved, which is the whole failure mode these roots exist to remove.
//
// A missing directory is returned unwrapped so callers can classify it with
// os.IsNotExist as well as errors.Is.
func Shared(dir string) (*os.Root, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("osroot: %q is not absolute", dir)
	}
	cleaned := filepath.Clean(dir)

	sharedMu.Lock()
	defer sharedMu.Unlock()

	if root, ok := sharedRoots[cleaned]; ok {
		return root, nil
	}
	root, err := os.OpenRoot(cleaned)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err //nolint:wrapcheck // see doc comment
		}
		return nil, fmt.Errorf("open %s: %w", cleaned, err)
	}
	sharedRoots[cleaned] = root
	return root, nil
}

// OpenChild opens a root for the directory name inside parent, WITHOUT
// memoizing it: the caller owns the returned root and must Close it.
//
// It is SharedChild's short-lived counterpart, and it exists so that opening a
// subdirectory of an anchor is never a bare parent.OpenRoot(name). os.Root
// refuses a symlink that escapes parent but follows one pointing elsewhere
// INSIDE it, so a bare OpenRoot silently accepts a redirected
// .git/entire-sessions. The Lstat before and the SameFile after are the same
// pair SharedChild uses, and they close the Lstat/OpenRoot race: once the
// identity is confirmed, a later replacement cannot redirect the handle.
//
// A missing directory is returned unwrapped so callers can classify it with
// os.IsNotExist.
func OpenChild(parent *os.Root, name string) (*os.Root, error) {
	if parent == nil {
		return nil, errors.New("osroot: parent root is required")
	}
	before, err := parent.Lstat(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // preserve original error classification
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, fmt.Errorf("%s: %w", name, ErrSymlinkedPath)
	}

	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // preserve original error classification
	}
	if err := validateOpenedRoot(parent, name, root); err != nil {
		_ = root.Close()
		return nil, err
	}
	return root, nil
}

// validateOpenedRoot confirms that name still names a real directory and that
// it is the object the opened root holds.
func validateOpenedRoot(parent *os.Root, name string, root *os.Root) error {
	pathInfo, err := parent.Lstat(name)
	if err != nil {
		return err //nolint:wrapcheck // preserve original error classification
	}
	openedInfo, err := root.Stat(".")
	if err != nil {
		return err //nolint:wrapcheck // preserve original error classification
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.IsDir() || !os.SameFile(pathInfo, openedInfo) {
		return fmt.Errorf("%s changed while it was being opened: %w", name, ErrSymlinkedPath)
	}
	return nil
}

// SharedChild returns a shared root for name beneath parent. Unlike opening the
// assembled absolute path, this keeps name inside an already-trusted root and
// refuses a symlink at the child boundary. The post-open identity check closes
// the Lstat/OpenRoot race.
func SharedChild(parent *os.Root, dir, name string) (*os.Root, error) {
	if parent == nil {
		return nil, errors.New("osroot: parent root is required")
	}
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("osroot: %q is not absolute", dir)
	}
	cleaned := filepath.Clean(dir)

	sharedMu.Lock()
	defer sharedMu.Unlock()
	if root, ok := sharedRoots[cleaned]; ok {
		return root, nil
	}

	before, err := parent.Lstat(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // preserve original error classification
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, fmt.Errorf("%s: %w", name, ErrSymlinkedPath)
	}

	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // preserve original error classification
	}
	if err := validateOpenedRoot(parent, name, root); err != nil {
		_ = root.Close()
		return nil, err
	}

	sharedRoots[cleaned] = root
	return root, nil
}

// Forget closes and drops the cached root for one directory, leaving the rest of
// the registry alone.
//
// Call it immediately before deleting or replacing a single rooted directory.
// ResetShared is the wrong tool there: it clears every anchor, and a caller
// rebuilding one cache directory has no business invalidating the handles other
// packages hold on .entire or .git. The plugin index cache is the case this
// exists for — its directory is removed and recreated by `git clone` while the
// process runs, so a root cached across that would be a handle to an unlinked
// inode.
//
// A directory that was never opened is not an error: forgetting is idempotent.
func Forget(dir string) {
	if !filepath.IsAbs(dir) {
		return
	}
	cleaned := filepath.Clean(dir)

	sharedMu.Lock()
	defer sharedMu.Unlock()
	if root, ok := sharedRoots[cleaned]; ok {
		_ = root.Close()
		delete(sharedRoots, cleaned)
	}
}

// ResetShared closes and forgets every root Shared handed out. Call it after
// deleting or replacing a directory a root was held on: a root that outlives its
// directory is a handle to an unlinked inode, so writes through it succeed and
// land nowhere.
func ResetShared() {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	for key, root := range sharedRoots {
		_ = root.Close()
		delete(sharedRoots, key)
	}
}
