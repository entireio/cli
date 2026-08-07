//go:build !windows

package cli

import (
	"os"
	"syscall"
)

// pathOwnedByCurrentUser reports whether path exists and is owned by the uid
// running this process.
//
// The shell hook's auto mode writes into a repository unattended, so it must
// refuse anything the current user does not own — a shared checkout, a
// colleague's tree on a multi-user box, a path handed over by a mount.
func pathOwnedByCurrentUser(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return int(stat.Uid) == os.Getuid()
}
