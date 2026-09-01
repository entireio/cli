//go:build windows

package jsonutil

import (
	"errors"

	"golang.org/x/sys/windows"
)

// renameIsTransient reports the Windows errors that unambiguously mean
// "someone else holds a lock or handle on the destination right now" —
// retryable — as opposed to anything that will not clear on its own.
//
// ERROR_ACCESS_DENIED is deliberately NOT in the set. MoveFileEx does report
// an open destination handle that way sometimes, but the same errno covers
// permanent failures — a read-only location, real ACLs — and a retry loop
// must not treat a permissions verdict as contention (the same rule
// syncdir_windows.go applies: never infer a good outcome from a denied one).
// If the open-handle-as-ACCESS_DENIED case proves common in practice, revisit
// with evidence.
func renameIsTransient(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
