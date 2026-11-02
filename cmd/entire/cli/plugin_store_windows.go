//go:build windows

package cli

import "os"

// symlinkManagedEntry never links on Windows. os.Root.Symlink (Go 1.27) stores
// an absolute target without the `\??\` prefix CreateSymbolicLinkW adds, so
// the link is created (Go enables the symlink privilege itself, so an elevated
// shell gets no error) but every follow fails with ERROR_INVALID_NAME — that is
// how `entire graph` installed a 0-byte bin\entire-graph.exe it could not run.
// Without the privilege the call fails anyway, so hardlink → copy is the path.
func symlinkManagedEntry(*os.Root, string, string) bool { return false }
