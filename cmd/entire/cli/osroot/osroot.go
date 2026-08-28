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

// SymlinkPaths walks dir within root and returns the names of every symlink it
// finds, in the root's coordinates. It never follows one, so a symlinked
// directory is reported and not descended into.
//
// A missing dir yields no names and no error: nothing there is nothing wrong.
func SymlinkPaths(root *os.Root, dir string) ([]string, error) {
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
