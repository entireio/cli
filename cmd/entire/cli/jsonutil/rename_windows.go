//go:build windows

package jsonutil

import (
	"errors"

	"golang.org/x/sys/windows"
)

// renameIsTransient reports the Windows errors that mean "someone else holds
// a handle on the destination right now" — retryable — as opposed to a
// missing directory or a permissions problem that will not clear on its own.
// ERROR_ACCESS_DENIED is in the set because MoveFileEx reports an open
// destination handle that way as often as ERROR_SHARING_VIOLATION.
func renameIsTransient(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED)
}
