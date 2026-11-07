//go:build !windows

package strategy

import (
	"os"

	"golang.org/x/sys/unix"
)

// canExecuteHook reports whether this process may execute name, a file
// directly in root. It asks the kernel with faccessat(X_OK) relative to the
// root's own directory descriptor, with the real ids, which is git's
// access(X_OK) before it runs a hook and what sh's [ -x ] asks, so ownership,
// the permission class that applies, POSIX ACLs and root count as they do for
// them, and no path is resolved outside the root.
func canExecuteHook(root *os.Root, name string) bool {
	dir, err := root.Open(".")
	if err != nil {
		return false
	}
	defer dir.Close()
	return unix.Faccessat(int(dir.Fd()), name, unix.X_OK, 0) == nil
}
