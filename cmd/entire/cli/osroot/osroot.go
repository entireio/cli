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
	"io"
	"os"
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
