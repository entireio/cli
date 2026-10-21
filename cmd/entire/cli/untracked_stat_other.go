//go:build !linux && !darwin

package cli

import "io/fs"

// fileChangeStamp has no portable source outside linux and darwin: Windows'
// file attribute data carries no change time. Size and modification time are
// all UntrackedFileStat compares there.
func fileChangeStamp(fs.FileInfo) (inode uint64, changeTime int64) {
	return 0, 0
}
