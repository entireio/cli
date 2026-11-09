package cli

import (
	"io/fs"
	"syscall"
)

// fileChangeStamp returns the inode number and status-change time (ctime) of
// info, the fields git's index also compares. ctime moves on every write and
// metadata change and cannot be set from userspace, so a same-size rewrite
// that restores the modification time (cp -p, touch -r) still changes it.
func fileChangeStamp(info fs.FileInfo) (inode uint64, changeTime int64) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	return st.Ino, st.Ctim.Nano()
}
