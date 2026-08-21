// Package userdirs resolves the per-user directories where the Entire CLIs
// keep global state. It is the single implementation of that resolution —
// don't derive ~/.config/entire or ~/.cache/entire paths anywhere else.
//
//   - Config: contexts.json, version_check.json, the file-backed token
//     store. $ENTIRE_CONFIG_DIR if set, else ~/.config/entire.
//   - Cache: discovery caches (nodes.json, cluster_cores.json,
//     api_discovery.json). $XDG_CACHE_HOME/entire if set, else
//     ~/.cache/entire.
//
// Under `go test`, both fall back to a throwaway per-process directory when
// their env override is unset (see internal/testdirs), so a test that
// forgets to isolate can never read or pollute the developer's real state.
package userdirs

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/internal/testdirs"
)

// Config returns the per-user config directory.
func Config() string {
	dir, err := ResolveConfig()
	if err != nil {
		// Compatibility path for callers whose API cannot return an error.
		// Keep it absolute and process-scoped so a repository can never
		// supply user-global state through a CWD-relative fallback.
		return filepath.Join(os.TempDir(), fmt.Sprintf("entire-no-home-%d", os.Getpid()), "config")
	}
	return dir
}

// ResolveConfig returns the per-user config directory, or an error when no
// explicit override exists and the user's home directory cannot be resolved.
// Security-sensitive readers should use this form so an unavailable home
// fails closed instead of turning the config path into a repository-relative
// path.
func ResolveConfig() (string, error) {
	testDir, testDirOK := testdirs.Dir("config")
	return resolveConfig(os.Getenv("ENTIRE_CONFIG_DIR"), testDir, testDirOK, os.UserHomeDir)
}

func resolveConfig(override, testDir string, testDirOK bool, homeDir func() (string, error)) (string, error) {
	if override != "" {
		return override, nil
	}
	if testDirOK {
		return testDir, nil
	}
	home, err := homeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".config", "entire"), nil
}

// Cache returns the per-user cache directory.
func Cache() string {
	dir, err := ResolveCache()
	if err != nil {
		return filepath.Join(os.TempDir(), fmt.Sprintf("entire-no-home-%d", os.Getpid()), "cache")
	}
	return dir
}

// ResolveCache returns the per-user cache directory, or an error when neither
// XDG_CACHE_HOME nor a resolvable user home is available.
func ResolveCache() (string, error) {
	testDir, testDirOK := testdirs.Dir("cache")
	return resolveCache(os.Getenv("XDG_CACHE_HOME"), testDir, testDirOK, os.UserHomeDir)
}

func resolveCache(xdg, testDir string, testDirOK bool, homeDir func() (string, error)) (string, error) {
	if xdg != "" {
		return filepath.Join(xdg, "entire"), nil
	}
	if testDirOK {
		return filepath.Join(testDir, "entire"), nil
	}
	home, err := homeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".cache", "entire"), nil
}
