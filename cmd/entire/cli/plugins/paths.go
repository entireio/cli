package plugins

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Managed storage layout constants. These MUST match the values used by the
// binary plugin store (cmd/entire/cli/plugin_store.go); the parent-dir
// resolution is duplicated here (rather than imported) because the cli package
// imports plugins, so plugins cannot import cli. The duplication is small and
// stable; a mismatch would split Lua and binary plugins across different
// per-user trees.
const (
	pluginEnvPluginDir  = "ENTIRE_PLUGIN_DIR"
	pluginManagedTopDir = "entire"
	pluginManagedSubDir = "plugins"

	// luaSubDir is the subdirectory under the managed plugin parent that holds
	// Lua plugins, one directory per plugin. Kept separate from the binary
	// store's bin/ and data/ subdirs so a Lua plugin dir is never confused with
	// an `entire-<name>` executable or a binary plugin's data dir.
	luaSubDir = "lua"

	// dataSubDir holds per-plugin durable key/value storage (entire.kv),
	// namespaced by plugin name and shared by Lua plugins from either source.
	dataSubDir = "data"

	windowsGOOS = "windows"
)

// pluginParentDir mirrors cli.pluginParentDir: the per-user directory that
// holds the managed plugin storage. See that function for the full rationale of
// the resolution order and platform conventions.
func pluginParentDir() (string, error) {
	if v := os.Getenv(pluginEnvPluginDir); v != "" {
		if !filepath.IsAbs(v) {
			return "", fmt.Errorf("%s must be an absolute path, got %q", pluginEnvPluginDir, v)
		}
		return v, nil
	}
	if runtime.GOOS == windowsGOOS {
		if appData := os.Getenv("LOCALAPPDATA"); appData != "" {
			return filepath.Join(appData, pluginManagedTopDir, pluginManagedSubDir), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		return filepath.Join(home, "AppData", "Local", pluginManagedTopDir, pluginManagedSubDir), nil
	}
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, pluginManagedTopDir, pluginManagedSubDir), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".local", "share", pluginManagedTopDir, pluginManagedSubDir), nil
}

// UserLuaPluginsDir returns the per-user directory that holds installed Lua
// plugins (one subdirectory per plugin). It is the sibling of the binary
// store's bin/ and data/ dirs.
func UserLuaPluginsDir() (string, error) {
	parent, err := pluginParentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, luaSubDir), nil
}

// PluginDataDir returns the durable per-plugin data directory used by
// entire.kv. The name must be dispatch-safe; the directory is not created here
// (callers create it lazily on first write) so a plugin that never persists
// state leaves no directory behind.
func PluginDataDir(name string) (string, error) {
	if err := ValidatePluginName(name); err != nil {
		return "", err
	}
	parent, err := pluginParentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, dataSubDir, name), nil
}

// RepoLuaPluginsDir returns the repo-local Lua plugin directory
// (.entire/plugins) under the given worktree root. Plugins discovered here
// NEVER auto-run without an explicit allow-list entry.
func RepoLuaPluginsDir(worktreeRoot string) string {
	return filepath.Join(worktreeRoot, ".entire", "plugins")
}
