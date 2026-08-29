//go:build !windows

package jsonutil

// renameIsTransient: rename(2) has no handle-contention failure mode on POSIX,
// so nothing is worth retrying.
func renameIsTransient(error) bool { return false }
