//go:build windows

package opencode

import (
	"errors"
	"syscall"
)

// Windows error codes MoveFileEx returns when it cannot delete the destination
// because another handle is open on it. ERROR_ACCESS_DENIED is included because
// MoveFileEx reports it for a destination opened without FILE_SHARE_DELETE.
const (
	errorAccessDenied     syscall.Errno = 5
	errorSharingViolation syscall.Errno = 32
)

// isRenameContention reports whether err is a transient sharing failure worth
// retrying. See renameOverExisting for why this only ever fires on Windows.
func isRenameContention(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == errorSharingViolation || errno == errorAccessDenied
}
