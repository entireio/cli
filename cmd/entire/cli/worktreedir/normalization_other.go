//go:build !darwin

package worktreedir

import "os"

// filesystemIgnoresNormalization reports false: outside macOS no filesystem
// is known to take names that differ only in Unicode normalization as one.
// NTFS and Linux's filesystems keep café.txt composed and decomposed as two
// names.
func filesystemIgnoresNormalization(*os.File) bool {
	return false
}
