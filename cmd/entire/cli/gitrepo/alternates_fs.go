//nolint:wrapcheck // passthrough methods must preserve osfs errors exactly; wrapping would break go-git's os.IsNotExist fall-through.
package gitrepo

import (
	gofs "io/fs"
	"os"
	"path/filepath"

	"github.com/go-git/go-billy/v6"
	"github.com/go-git/go-billy/v6/osfs"
)

type alternatesFilesystem struct {
	root billy.Filesystem
	base string
}

func newAlternatesFilesystem() billy.Filesystem {
	return &alternatesFilesystem{
		root: osfs.New(string(os.PathSeparator), osfs.WithBoundOS()),
		base: string(os.PathSeparator),
	}
}

func (fs *alternatesFilesystem) Create(filename string) (billy.File, error) {
	return fs.root.Create(fs.resolve(filename))
}

func (fs *alternatesFilesystem) Open(filename string) (billy.File, error) {
	return fs.root.Open(fs.resolve(filename))
}

func (fs *alternatesFilesystem) OpenFile(filename string, flag int, perm gofs.FileMode) (billy.File, error) {
	return fs.root.OpenFile(fs.resolve(filename), flag, perm)
}

func (fs *alternatesFilesystem) Stat(filename string) (gofs.FileInfo, error) {
	return fs.root.Stat(fs.resolve(filename))
}

func (fs *alternatesFilesystem) Rename(oldpath, newpath string) error {
	return fs.root.Rename(fs.resolve(oldpath), fs.resolve(newpath))
}

func (fs *alternatesFilesystem) Remove(filename string) error {
	return fs.root.Remove(fs.resolve(filename))
}

func (fs *alternatesFilesystem) Join(elem ...string) string {
	return filepath.Join(elem...)
}

func (fs *alternatesFilesystem) TempFile(dir, prefix string) (billy.File, error) {
	return fs.root.TempFile(fs.resolve(dir), prefix)
}

func (fs *alternatesFilesystem) ReadDir(path string) ([]gofs.DirEntry, error) {
	return fs.root.ReadDir(fs.resolve(path))
}

func (fs *alternatesFilesystem) MkdirAll(filename string, perm gofs.FileMode) error {
	return fs.root.MkdirAll(fs.resolve(filename), perm)
}

func (fs *alternatesFilesystem) Lstat(filename string) (gofs.FileInfo, error) {
	return fs.root.Lstat(fs.resolve(filename))
}

func (fs *alternatesFilesystem) Symlink(target, link string) error {
	return fs.root.Symlink(target, fs.resolve(link))
}

func (fs *alternatesFilesystem) Readlink(link string) (string, error) {
	return fs.root.Readlink(fs.resolve(link))
}

func (fs *alternatesFilesystem) Chroot(path string) (billy.Filesystem, error) {
	return &alternatesFilesystem{
		root: fs.root,
		base: fs.resolve(path),
	}, nil
}

func (fs *alternatesFilesystem) Root() string {
	return fs.base
}

func (fs *alternatesFilesystem) Capabilities() billy.Capability {
	if capable, ok := fs.root.(billy.Capable); ok {
		return capable.Capabilities()
	}
	return billy.DefaultCapabilities
}

func (fs *alternatesFilesystem) resolve(name string) string {
	if name == "" {
		return fs.base
	}
	if filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return filepath.Clean(name)
	}
	return filepath.Clean(filepath.Join(fs.base, name))
}
