//go:build windows

package interactive

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// releasePendingReads aborts a console read blocked on f so that closing it
// does not wait for a keypress.
//
// Bubble Tea only gets a cancellable console reader for os.Stdin; a console
// handle opened separately (CONIN$) gets a fallback whose Cancel is a no-op,
// so the read it issued after the final keystroke stays pending, and Go's
// os.File.Close on Windows waits for every pending operation before it
// returns. CancelIoEx completes that read with zero bytes, which the reader
// sees as io.EOF rather than an aborted-operation error, so it must run only
// once the prompt has its answer.
func releasePendingReads(f *os.File) error {
	err := windows.CancelIoEx(windows.Handle(f.Fd()), nil)
	if err == nil || errors.Is(err, windows.ERROR_NOT_FOUND) {
		return nil // ERROR_NOT_FOUND: nothing was pending
	}
	return err //nolint:wrapcheck // Close wraps with the handle's role
}
