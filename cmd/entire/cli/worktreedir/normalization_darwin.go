//go:build darwin

package worktreedir

import (
	"os"

	"golang.org/x/sys/unix"
)

// IgnoresNormalization reports whether the filesystem holding root's
// directory takes names that differ only in Unicode normalization as one, so
// that café.txt composed and decomposed name one file. APFS and HFS+, the
// filesystems macOS formats, do. Anything else it mounts (SMB, NFS, exFAT,
// FAT) can keep the two forms apart, so it reports false, as it does when the
// filesystem cannot be asked.
func IgnoresNormalization(root *os.Root) bool {
	dir, err := root.Open(".")
	if err != nil {
		return false
	}
	defer dir.Close()
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
