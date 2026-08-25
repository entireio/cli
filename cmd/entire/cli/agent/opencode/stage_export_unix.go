//go:build !windows

package opencode

// isRenameContention always reports false: POSIX rename(2) replaces the
// destination regardless of open handles, so a failure here is not transient.
// See renameOverExisting.
func isRenameContention(error) bool { return false }
