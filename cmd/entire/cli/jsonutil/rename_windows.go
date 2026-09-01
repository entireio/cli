//go:build windows

package jsonutil

import (
	"errors"

	"golang.org/x/sys/windows"
)

// renameIsTransient reports the Windows errors that mean "someone else holds
// a handle on the destination right now" — retryable — as opposed to a
// missing directory that will not clear on its own.
//
// ERROR_ACCESS_DENIED is deliberately in the set even though it also covers
// permanent failures (a read-only location, real ACLs): MoveFileEx reports an
// open destination handle that way as often as ERROR_SHARING_VIOLATION, and
// the open-handle case is the whole point of the retry (#455). The cost of
// being wrong is bounded — one ~155ms backoff on a write that was going to
// fail anyway — while excluding it misses the most common spelling of the
// contention this exists to ride out. (syncdir_windows.go excludes it for the
// opposite reason: there a denied directory sync is treated as success, and
// success must not be inferred from a permissions error.)
func renameIsTransient(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED)
}
