package worktreedir

import (
	"errors"
	"io/fs"
	"os"
	"path"
)

// IgnoresNormalization reports whether the filesystem holding name, a
// slash-separated path in root, takes names that differ only in Unicode
// normalization as one, so that café.txt composed and decomposed name one
// file. APFS and HFS+, the filesystems macOS formats, do. Anything else
// (SMB, NFS, exFAT, FAT, and every filesystem outside macOS) can keep the two
// forms apart, so it reports false, as it does when the filesystem cannot be
// asked. A worktree can hold a mount of another filesystem, so the one asked
// is that of name's nearest directory that exists (nearestDir).
func IgnoresNormalization(root *os.Root, name string) bool {
	dir, ok := nearestDir(root, name)
	if !ok {
		return false
	}
	f, err := root.Open(dir)
	if err != nil {
		return false
	}
	defer f.Close()
	return filesystemIgnoresNormalization(f)
}

// nearestDir returns name's directory, or the nearest of its parents that
// exists when it is gone, as a deleted file's directory can be. It returns
// false when the first one that exists is not a directory, or root cannot
// tell, as for a link that leads out of root.
func nearestDir(root *os.Root, name string) (string, bool) {
	for dir := path.Dir(name); ; dir = path.Dir(dir) {
		info, err := root.Stat(dir)
		switch {
		case err == nil && info.IsDir():
			return dir, true
		case err == nil || !errors.Is(err, fs.ErrNotExist) || dir == ".":
			return "", false
		}
	}
}
