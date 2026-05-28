package checkpoint

import (
	"io/fs"
	"os"
	"path/filepath"

	"github.com/go-git/go-billy/v6"
	"github.com/go-git/go-git/v6/x/plugin"
	xconfig "github.com/go-git/go-git/v6/x/plugin/config"
)

// osSymlinkFS is a minimal billy.Basic backed directly by the os package so
// that global/system git-config reads follow symlinks the same way git does.
//
// go-git's default config loader (xconfig.NewAuto backed by osfs.Default)
// reads config files through Go's os.Root, which rejects an absolute symlink
// in any path component — even when it resolves back inside the root — with
// "path escapes from parent". Users whose global config lives behind a
// symlinked directory (e.g. ~/.config managed by a dotfile tool such as
// chezmoi, GNU Stow, or yadm) therefore had their global config silently
// dropped: checkpoint-commit author identity fell back to "Unknown", commit
// signing was skipped, and the lifecycle hook printed a "failed to load
// global git config: path escapes from parent" warning for every commit it
// created while pushing (once per cherry-picked commit during a sync/rebase).
//
// The auto loader only ever calls Open and Stat; the remaining billy.Basic
// methods exist to satisfy the interface and are not expected to be exercised
// for config resolution.
type osSymlinkFS struct{}

//nolint:gochecknoinits // Override go-git's default config loader so global git config behind symlinks is read (see osSymlinkFS).
func init() {
	// plugin.Register replaces the factory go-git registered during its own
	// package init. This runs afterwards because this package imports x/plugin,
	// and before any plugin.Get call (which only happens at command runtime).
	//nolint:errcheck,gosec // Best-effort: Register only fails after a plugin.Get, which cannot precede init; go-git's default loader remains as fallback.
	registerSymlinkConfigLoader()
}

// registerSymlinkConfigLoader registers the symlink-following config loader as
// the ConfigLoader plugin. Exposed for tests that reset the registry.
func registerSymlinkConfigLoader() error {
	return plugin.Register(plugin.ConfigLoader(), func() plugin.ConfigSource {
		return xconfig.NewAuto(xconfig.WithFilesystem(osSymlinkFS{}))
	})
}

func (osSymlinkFS) Open(name string) (billy.File, error) {
	f, err := os.Open(name) //nolint:gosec // G304: name comes from git's own config-path resolution, not user input.
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (osSymlinkFS) Stat(name string) (fs.FileInfo, error) {
	return os.Stat(name)
}

func (osSymlinkFS) OpenFile(name string, flag int, perm fs.FileMode) (billy.File, error) {
	f, err := os.OpenFile(name, flag, perm) //nolint:gosec // G304: name comes from git's own config-path resolution, not user input.
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (o osSymlinkFS) Create(name string) (billy.File, error) {
	return o.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o666)
}

func (osSymlinkFS) Rename(oldpath, newpath string) error {
	return os.Rename(oldpath, newpath)
}

func (osSymlinkFS) Remove(name string) error {
	return os.Remove(name)
}

func (osSymlinkFS) Join(elem ...string) string {
	return filepath.Join(elem...)
}
