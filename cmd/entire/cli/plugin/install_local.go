package plugin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

// InstallLocalOptions configures InstallLocal.
type InstallLocalOptions struct {
	// SourceDir is the directory containing the plugin executable.
	SourceDir string
	// Force replaces any existing plugin entry with the same name.
	Force bool
	// RootCmd is used for built-in conflict detection. Optional; if nil, the
	// check is skipped.
	RootCmd *cobra.Command
}

// InstallLocal symlinks SourceDir into the plugins root as a local plugin.
// Used by `entire plugin install .`. The directory name must be `entire-<name>`
// and must contain an executable of the same name.
func (m *Manager) InstallLocal(opts InstallLocalOptions) (*Plugin, error) {
	src, err := filepath.Abs(opts.SourceDir)
	if err != nil {
		return nil, fmt.Errorf("resolve source dir: %w", err)
	}
	info, err := os.Stat(src)
	if err != nil {
		return nil, fmt.Errorf("stat source dir: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("source must be a directory: %s", src)
	}

	dirName := filepath.Base(src)
	if !strings.HasPrefix(dirName, Prefix) {
		return nil, fmt.Errorf("directory name must start with %q (got %q)", Prefix, dirName)
	}
	bare := strings.TrimPrefix(dirName, Prefix)
	if !ValidName(bare) {
		return nil, fmt.Errorf("invalid plugin name %q", bare)
	}

	exec := filepath.Join(src, executableName(dirName))
	execInfo, err := os.Stat(exec)
	if err != nil {
		return nil, fmt.Errorf("plugin executable %q not found in %s", executableName(dirName), src)
	}
	if runtime.GOOS != osWindows && execInfo.Mode()&0o111 == 0 {
		return nil, fmt.Errorf("plugin executable %s is not executable (chmod +x)", exec)
	}

	if opts.RootCmd != nil && isBuiltin(opts.RootCmd, bare) {
		return nil, fmt.Errorf("name %q conflicts with a built-in command; rename the plugin or use 'entire plugin exec %s' to invoke", bare, bare)
	}

	if err := m.EnsureRoot(); err != nil {
		return nil, err
	}

	dest := filepath.Join(m.Root, dirName)
	if _, err := os.Lstat(dest); err == nil {
		if !opts.Force {
			return nil, fmt.Errorf("plugin %q already installed; use --force to replace", bare)
		}
		if err := os.RemoveAll(dest); err != nil {
			return nil, fmt.Errorf("remove existing plugin: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("stat install destination: %w", err)
	}

	if err := os.Symlink(src, dest); err != nil {
		return nil, fmt.Errorf("symlink plugin: %w", err)
	}

	return m.Find(bare)
}
