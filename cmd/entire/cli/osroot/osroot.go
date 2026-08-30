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

// ReadFileNoFollow reads name while refusing a symbolic link in any component.
// os.Root already prevents a link from escaping root, but follows links whose
// targets remain inside it. Entire-owned trees require the stronger property:
// every component must identify the object stored at that name.
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

// OpenNoFollow opens an existing file without following any symlink component.
// Parent directories are opened and pinned one at a time; the leaf's second
// check closes its Lstat/Open race. Once verified, later replacements cannot
// redirect the open descriptor.
func OpenNoFollow(root *os.Root, name string) (*os.File, error) {
	parent, leaf, closeParent, err := OpenParentNoSymlinks(root, name)
	if err != nil {
		return nil, err
	}
	defer closeParent()

	before, err := parent.Lstat(leaf)
	if err != nil {
		return nil, err //nolint:wrapcheck // preserve original error classification
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s: %w", name, ErrSymlinkedPath)
	}

	f, err := parent.Open(leaf)
	if err != nil {
		return nil, err //nolint:wrapcheck // preserve original error classification
	}
	if err := validateOpenedFile(parent, leaf, f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// OpenFileNoFollow opens or creates a file without following any symlink.
// It is intended for append-style writers: O_TRUNC is rejected because
// truncating Entire-owned writes should use an atomic temp-file-and-rename
// operation instead. Unix builds add O_NOFOLLOW to close the write-before-
// validation race; other platforms still reject pre-existing links and verify
// the opened object before returning it.
func OpenFileNoFollow(root *os.Root, name string, flag int, perm os.FileMode) (*os.File, error) {
	if flag&os.O_TRUNC != 0 {
		return nil, errors.New("osroot: OpenFileNoFollow does not permit O_TRUNC")
	}
	parent, leaf, closeParent, err := OpenParentNoSymlinks(root, name)
	if err != nil {
		return nil, err
	}
	defer closeParent()

	if before, err := parent.Lstat(leaf); err == nil {
		if before.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s: %w", name, ErrSymlinkedPath)
		}
	} else if !os.IsNotExist(err) {
		return nil, err //nolint:wrapcheck // preserve original error classification
	}

	f, err := parent.OpenFile(leaf, flag|noFollowOpenFlag, perm)
	if err != nil {
		// Preserve the package sentinel when a link appeared after the first
		// Lstat and O_NOFOLLOW rejected it.
		if info, lstatErr := parent.Lstat(leaf); lstatErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s: %w", name, ErrSymlinkedPath)
		}
		return nil, err //nolint:wrapcheck // preserve original error classification
	}
	if err := validateOpenedFile(parent, leaf, f); err != nil {
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

// ReadDirNoSymlinks reads a directory after opening every component beneath
// root as a real directory. Unlike ReadDir, an in-root symlink is rejected
// rather than followed.
func ReadDirNoSymlinks(root *os.Root, name string) ([]os.DirEntry, error) {
	dir, closeDir, err := OpenDirNoSymlinks(root, name)
	if err != nil {
		return nil, err
	}
	defer closeDir()

	return ReadDir(dir, ".")
}

// ErrSymlinkedPath reports a symlink where a real path component was required.
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
// Each component is opened relative to its already-pinned parent, including
// components created by this call. That makes creation and validation one
// descriptor-relative sequence rather than a check followed by MkdirAll.
func MkdirAllNoSymlink(root *os.Root, name string, perm os.FileMode) error {
	if name == "." {
		return nil
	}
	if !fs.ValidPath(name) {
		return fmt.Errorf("%q is not a valid root-relative path", name)
	}

	current := root
	var owned *os.Root
	defer func() {
		if owned != nil {
			_ = owned.Close()
		}
	}()
	for _, component := range strings.Split(name, "/") {
		if _, err := current.Lstat(component); os.IsNotExist(err) {
			if err := current.Mkdir(component, perm); err != nil && !errors.Is(err, fs.ErrExist) {
				return err //nolint:wrapcheck // preserve original error classification
			}
		} else if err != nil {
			return err //nolint:wrapcheck // preserve original error classification
		}
		next, err := OpenChild(current, component)
		if err != nil {
			return err
		}
		if owned != nil {
			_ = owned.Close()
		}
		owned = next
		current = next
	}
	return nil
}

// ErrWalkRootNotDirectory reports a walk root that exists but is not a
// directory. It is deliberately NOT ErrSymlinkedPath: "replace this link" and
// "this is a regular file" are different remedies, and conflating them is how a
// user gets told to fix the wrong thing.
var ErrWalkRootNotDirectory = errors.New("path is not a directory")

// OpenParentNoSymlinks opens and pins the parent directory of name while
// rejecting a symlink in every parent component. The returned close function
// must be called; it is a no-op when name is directly beneath root.
//
// Operations that must not follow parent symlinks should act on the returned
// root using leaf, rather than validating and then resolving name again from
// the original root. Holding the parent descriptor closes that check/use gap.
func OpenParentNoSymlinks(root *os.Root, name string) (parent *os.Root, leaf string, closeParent func(), err error) {
	if root == nil {
		return nil, "", nil, errors.New("osroot: root is required")
	}
	if !fs.ValidPath(name) || name == "." {
		return nil, "", nil, fmt.Errorf("%q is not a valid file name beneath root", name)
	}

	dir, leaf := path.Split(name)
	dir = strings.TrimSuffix(dir, "/")
	if dir == "" {
		return root, leaf, func() {}, nil
	}
	parent, closeParent, err = OpenDirNoSymlinks(root, dir)
	if err != nil {
		return nil, "", nil, err
	}
	return parent, leaf, closeParent, nil
}

// OpenDirNoSymlinks opens dir one component at a time. Every child is lstat'd,
// opened relative to its already-pinned parent, and identity-checked, so a
// symlink or a replacement racing the open is rejected. The caller must invoke
// the returned close function.
func OpenDirNoSymlinks(root *os.Root, dir string) (*os.Root, func(), error) {
	if root == nil {
		return nil, nil, errors.New("osroot: root is required")
	}
	if dir == "." || dir == "" {
		return root, func() {}, nil
	}
	if !fs.ValidPath(dir) {
		return nil, nil, fmt.Errorf("%q is not a valid directory name beneath root", dir)
	}

	current := root
	var owned *os.Root
	for _, component := range strings.Split(dir, "/") {
		next, err := OpenChild(current, component)
		if err != nil {
			if owned != nil {
				_ = owned.Close()
			}
			return nil, nil, err
		}
		if owned != nil {
			_ = owned.Close()
		}
		owned = next
		current = next
	}
	return current, func() { _ = current.Close() }, nil
}

// LstatNoSymlinks lstats the leaf while rejecting and pinning every parent
// directory component. The leaf itself is returned as-is, including when it is
// a symlink, so callers can choose whether to reject or unlink it.
func LstatNoSymlinks(root *os.Root, name string) (os.FileInfo, error) {
	parent, leaf, closeParent, err := OpenParentNoSymlinks(root, name)
	if err != nil {
		return nil, err
	}
	defer closeParent()
	return parent.Lstat(leaf) //nolint:wrapcheck // preserve original error classification
}

// RemoveNoSymlinks removes leaf without following it and rejects a symlink in
// every parent directory component. A missing leaf is not an error.
func RemoveNoSymlinks(root *os.Root, name string) error {
	parent, leaf, closeParent, err := OpenParentNoSymlinks(root, name)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer closeParent()
	return Remove(parent, leaf)
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
	walkRoot, closeWalkRoot, err := OpenDirNoSymlinks(root, dir)
	if err != nil {
		return err
	}
	defer closeWalkRoot()
	return fs.WalkDir(walkRoot.FS(), ".", func(name string, d fs.DirEntry, err error) error { //nolint:wrapcheck // the callback's errors are the caller's own
		reported := dir
		if name != "." {
			reported = path.Join(dir, strings.TrimPrefix(name, "./"))
		}
		if err == nil && d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s: %w", reported, ErrSymlinkedPath)
		}
		return fn(reported, d, err)
	})
}

// NoSymlinkedParent reports ErrSymlinkedPath when any directory component of
// name is a symlink. The leaf is deliberately not examined. This is a
// diagnostic predicate; an operation that follows it must still use
// OpenParentNoSymlinks so the checked parent remains pinned through the use.
//
// A component that does not exist is not an error: the caller is about to fail
// on the missing file, with a better message than this could give.
func NoSymlinkedParent(root *os.Root, name string) error {
	_, _, closeParent, err := OpenParentNoSymlinks(root, name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	closeParent()
	return nil
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
	walkRoot, closeWalkRoot, openErr := OpenDirNoSymlinks(root, dir)
	switch {
	case openErr == nil:
		defer closeWalkRoot()
	case os.IsNotExist(openErr):
		return nil, nil
	case errors.Is(openErr, ErrSymlinkedPath):
		return []string{dir}, nil
	case errors.Is(openErr, ErrWalkRootNotDirectory):
		return nil, nil // not a directory, so nothing below it to report
	default:
		return nil, openErr
	}

	var found []string
	err := fs.WalkDir(walkRoot.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		reported := dir
		if name != "." {
			reported = path.Join(dir, strings.TrimPrefix(name, "./"))
		}
		if err != nil {
			if name == "." && os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			found = append(found, reported)
		}
		return nil
	})
	if err != nil {
		return nil, err //nolint:wrapcheck // caller names the directory it asked about
	}
	return found, nil
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
	if before.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s: %w", name, ErrSymlinkedPath)
	}
	if !before.IsDir() {
		return nil, fmt.Errorf("%s: %w", name, ErrWalkRootNotDirectory)
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
