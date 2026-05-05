package plugin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Prefix is the required filename prefix for plugin executables and their
// containing directories. `entire <name>` dispatches to `entire-<name>`.
const Prefix = "entire-"

// osWindows mirrors runtime.GOOS for windows; constant to satisfy goconst.
const osWindows = "windows"

// Kind classifies how a plugin was installed.
type Kind string

const (
	// KindBinary is a plugin installed from a GitHub release asset. Has a
	// manifest.yml describing owner/name/tag.
	KindBinary Kind = "binary"
	// KindScript is a git-cloned repository whose root contains an executable
	// of the same name as the directory.
	KindScript Kind = "script"
	// KindLocal is a symlink, used by `entire plugin install .` for development.
	KindLocal Kind = "local"
)

// Plugin describes one installed plugin.
type Plugin struct {
	// Name is the bare plugin name (without the "entire-" prefix).
	Name string
	// Kind is how this plugin was installed.
	Kind Kind
	// Dir is the absolute path to the plugin directory inside the plugins
	// root. For local plugins, this is the symlink path itself.
	Dir string
	// ExecPath is the absolute path to the executable to invoke.
	ExecPath string
	// Manifest is populated for binary plugins. Nil otherwise.
	Manifest *BinaryManifest
	// PinnedSHA is the sha recorded in a .pin-<sha> marker, empty if unpinned.
	PinnedSHA string
}

// FullName returns the executable name including the "entire-" prefix.
func (p *Plugin) FullName() string {
	return Prefix + p.Name
}

// Manager handles plugin storage discovery and lifecycle.
type Manager struct {
	// Root is the plugins directory (e.g. ~/.local/share/entire/plugins).
	Root string
}

// NewManager returns a manager rooted at the configured plugins directory.
// Honors ENTIRE_PLUGIN_DIR; falls back to XDG_DATA_HOME/entire/plugins, then
// to a platform default.
func NewManager() (*Manager, error) {
	root, err := DefaultRoot()
	if err != nil {
		return nil, err
	}
	return &Manager{Root: root}, nil
}

// DefaultRoot resolves the plugins directory.
func DefaultRoot() (string, error) {
	if v := os.Getenv("ENTIRE_PLUGIN_DIR"); v != "" {
		return v, nil
	}
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "entire", "plugins"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	switch runtime.GOOS {
	case "windows":
		if appData := os.Getenv("LOCALAPPDATA"); appData != "" {
			return filepath.Join(appData, "entire", "plugins"), nil
		}
		return filepath.Join(home, "AppData", "Local", "entire", "plugins"), nil
	case "darwin":
		// Match XDG convention even on macOS for consistency with gh.
		return filepath.Join(home, ".local", "share", "entire", "plugins"), nil
	default:
		return filepath.Join(home, ".local", "share", "entire", "plugins"), nil
	}
}

// EnsureRoot creates the plugins directory if it does not exist.
func (m *Manager) EnsureRoot() error {
	if err := os.MkdirAll(m.Root, 0o750); err != nil {
		return fmt.Errorf("create plugin dir: %w", err)
	}
	return nil
}

// List returns all plugins discovered under Root, sorted by name.
// Entries that don't follow the entire-<name> naming or aren't classifiable
// are skipped silently.
func (m *Manager) List() ([]*Plugin, error) {
	entries, err := os.ReadDir(m.Root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read plugin dir: %w", err)
	}

	var plugins []*Plugin
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, Prefix) {
			continue
		}
		p, err := m.classify(name)
		if err != nil || p == nil {
			continue
		}
		plugins = append(plugins, p)
	}

	sort.Slice(plugins, func(i, j int) bool { return plugins[i].Name < plugins[j].Name })
	return plugins, nil
}

// Find returns the plugin with the given bare name (without the "entire-"
// prefix), or (nil, nil) if none is installed. Callers check both: a non-nil
// plugin means installed, nil + nil error means not installed.
func (m *Manager) Find(name string) (*Plugin, error) {
	if name == "" {
		return nil, nil //nolint:nilnil // not-installed signal
	}
	full := Prefix + name
	p, err := m.classify(full)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// classify inspects a single entry name (e.g. "entire-foo") under Root and
// returns the corresponding Plugin or nil if it isn't a valid plugin.
func (m *Manager) classify(fullName string) (*Plugin, error) {
	if !strings.HasPrefix(fullName, Prefix) {
		return nil, nil //nolint:nilnil // not-a-plugin signal
	}
	bare := strings.TrimPrefix(fullName, Prefix)
	if bare == "" {
		return nil, nil //nolint:nilnil // not-a-plugin signal
	}

	path := filepath.Join(m.Root, fullName)
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil //nolint:nilnil // not-installed signal
		}
		return nil, fmt.Errorf("stat plugin %q: %w", fullName, err)
	}

	// Symlink → local plugin.
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, fmt.Errorf("resolve local plugin symlink %q: %w", fullName, err)
		}
		exec := localExecPath(target, fullName)
		return &Plugin{
			Name:     bare,
			Kind:     KindLocal,
			Dir:      path,
			ExecPath: exec,
		}, nil
	}

	// Regular file at root level isn't a valid plugin layout.
	if !info.IsDir() {
		return nil, nil //nolint:nilnil // not-a-plugin signal
	}

	// Manifest present → binary plugin.
	manifestPath := filepath.Join(path, ManifestFileName)
	if _, err := os.Stat(manifestPath); err == nil {
		mf, err := LoadBinaryManifest(manifestPath)
		if err != nil {
			return nil, err
		}
		exec := mf.Path
		if exec == "" {
			exec = filepath.Join(path, executableName(fullName))
		}
		return &Plugin{
			Name:      bare,
			Kind:      KindBinary,
			Dir:       path,
			ExecPath:  exec,
			Manifest:  mf,
			PinnedSHA: readPinSHA(path),
		}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("stat manifest %q: %w", manifestPath, err)
	}

	// Otherwise → script plugin (must contain an executable matching the dir name).
	exec := filepath.Join(path, executableName(fullName))
	if _, err := os.Stat(exec); err != nil {
		// Missing executable: not a valid plugin layout, but not an error
		// either (the dir might be partial/in-progress). Return not-found.
		return nil, nil //nolint:nilerr,nilnil // not-a-plugin signal
	}
	return &Plugin{
		Name:      bare,
		Kind:      KindScript,
		Dir:       path,
		ExecPath:  exec,
		PinnedSHA: readPinSHA(path),
	}, nil
}

// localExecPath returns the executable path for a local plugin given the
// resolved symlink target. The target is expected to be either the executable
// itself or a directory containing it.
func localExecPath(target, fullName string) string {
	info, err := os.Stat(target)
	if err == nil && info.IsDir() {
		return filepath.Join(target, executableName(fullName))
	}
	return target
}

// executableName returns the platform-specific executable filename inside a
// plugin directory.
func executableName(fullName string) string {
	if runtime.GOOS == osWindows {
		return fullName + ".exe"
	}
	return fullName
}

// ValidName reports whether s is a valid bare plugin name. Plugin names must
// be non-empty, lowercase ASCII letters, digits, and dashes. They must not
// start with a dash.
func ValidName(s string) bool {
	if s == "" || s[0] == '-' {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return false
		}
	}
	return true
}
