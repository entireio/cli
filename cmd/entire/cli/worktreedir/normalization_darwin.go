//go:build darwin

package worktreedir

import (
	"os"

	"golang.org/x/sys/unix"
)

// filesystemIgnoresNormalization reports whether dir's filesystem is APFS or
// HFS+, which take names that differ only in Unicode normalization as one.
func filesystemIgnoresNormalization(dir *os.File) bool {
	conn, err := dir.SyscallConn()
	if err != nil {
		return false
	}
	var st unix.Statfs_t
	var statErr error
	if err := conn.Control(func(fd uintptr) { statErr = unix.Fstatfs(int(fd), &st) }); err != nil || statErr != nil {
		return false
	}
	switch unix.ByteSliceToString(st.Fstypename[:]) {
	case "apfs", "hfs":
		return true
	}
	return false
}
