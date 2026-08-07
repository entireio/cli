//go:build windows

package cli

// pathOwnedByCurrentUser always reports false on Windows.
//
// The shell hook is a POSIX-shell feature that `install`/`init` refuse to set
// up here, and Windows ACLs have no cheap uid equivalent. Reporting false
// keeps auto mode from ever running `entire enable` unattended on a platform
// where the ownership guarantee cannot be checked.
func pathOwnedByCurrentUser(string) bool { return false }
