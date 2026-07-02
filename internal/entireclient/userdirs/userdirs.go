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
	"os"
	"path/filepath"

	"github.com/entireio/cli/internal/testdirs"
)

// Config returns the per-user config directory.
func Config() string {
	if dir := os.Getenv("ENTIRE_CONFIG_DIR"); dir != "" {
		return dir
	}
	if dir, ok := testdirs.Dir("config"); ok {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "entire")
}

// Cache returns the per-user cache directory.
func Cache() string {
	if xdg := os.Getenv("XDG_CACHE_HOME"); xdg != "" {
		return filepath.Join(xdg, "entire")
	}
	if dir, ok := testdirs.Dir("cache"); ok {
		return filepath.Join(dir, "entire")
	}
	home, _ := os.UserHomeDir() //nolint:errcheck // best-effort default
	return filepath.Join(home, ".cache", "entire")
}
