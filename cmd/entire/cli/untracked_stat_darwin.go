package cli

import (
	"io/fs"
	"syscall"
)

// fileChangeStamp returns the inode number and status-change time (ctime) of
// info. See the linux implementation.
func fileChangeStamp(info fs.FileInfo) (inode uint64, changeTime int64) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	return st.Ino, st.Ctimespec.Nano()
}
