package checkpoint

import (
	"errors"
	"fmt"
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
// SCOPE: global and system config only — never the repository's own
// .git/config. go-git dispatches on config.Scope (see repository.go's
// ConfigScoped): LocalScope is served by r.Storer.Config() and never reaches a
// ConfigLoader plugin, and xconfig.NewAuto's Load rejects any scope other than
// Global and System outright. The files this can therefore touch are
// ~/.gitconfig, $XDG_CONFIG_HOME/git/config, /etc/gitconfig, and whatever
// GIT_CONFIG_GLOBAL / GIT_CONFIG_SYSTEM name — all outside the repository.
//
// WHY THE OVERRIDE EXISTS: go-git's default loader is backed by osfs.Default,
// which is a boundOS over os.Root. os.Root documents that "symbolic links must
// not be absolute" — unconditionally, no matter where the root is anchored, and
// osfs.Default is anchored at "/". So an absolute symlink anywhere in the path
// is rejected with "path escapes from parent" even though it resolves back
// inside the root. Users whose global config lives behind a symlinked directory
// (a ~/.config managed by chezmoi, GNU Stow, or yadm) therefore had their global
// config silently dropped: checkpoint-commit author identity fell back to
// "Unknown", commit signing was skipped, and the lifecycle hook printed a
// "failed to load global git config: path escapes from parent" warning for every
// commit it created while pushing (once per cherry-picked commit during a
// sync/rebase).
//
// This is not a confinement boundary being given up. There is no root these
// paths belong under — they are absolute locations in $HOME and /etc that git
// itself resolves the same way. What os.Root withholds here is traversal, not
// protection.
//
// The loader only ever calls Open and Stat. Every mutating method below fails
// closed rather than reaching the os package: nothing needs them today, and a
// future go-git that started writing config must not silently gain an
// unconfined writer over a path taken from the environment.
type osSymlinkFS struct{}

// errReadOnlyConfigFS is returned by osSymlinkFS's mutating methods. It is a
// hard failure, not a fallback: see the type's doc comment.
var errReadOnlyConfigFS = errors.New("git config loader filesystem is read-only")

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

func (osSymlinkFS) OpenFile(name string, _ int, _ fs.FileMode) (billy.File, error) {
	return nil, fmt.Errorf("%w: OpenFile %s", errReadOnlyConfigFS, name)
}

func (osSymlinkFS) Create(name string) (billy.File, error) {
	return nil, fmt.Errorf("%w: Create %s", errReadOnlyConfigFS, name)
}

func (osSymlinkFS) Rename(oldpath, newpath string) error {
	return fmt.Errorf("%w: Rename %s -> %s", errReadOnlyConfigFS, oldpath, newpath)
}

func (osSymlinkFS) Remove(name string) error {
	return fmt.Errorf("%w: Remove %s", errReadOnlyConfigFS, name)
}

func (osSymlinkFS) Join(elem ...string) string {
	return filepath.Join(elem...)
}
