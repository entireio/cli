//go:build !darwin

package worktreedir

import "os"

// IgnoresNormalization reports whether the filesystem holding root's
// directory takes names that differ only in Unicode normalization as one.
// Outside macOS none is known to: NTFS and Linux's filesystems keep café.txt
// composed and decomposed as two names.
func IgnoresNormalization(*os.Root) bool {
	return false
}
