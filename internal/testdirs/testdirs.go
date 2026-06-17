// Package testdirs provides per-process fallback directories for the CLI's
// on-disk config, cache, and credential surfaces when running inside
// `go test`.
//
// Production code resolves these surfaces relative to the user's home
// directory (~/.config/entire, ~/.cache/entire, the OS keyring). A test that
// forgets to set the explicit override (ENTIRE_CONFIG_DIR, XDG_CACHE_HOME,
// ENTIRE_TOKEN_STORE, ...) would silently read and write the developer's
// real configuration — real auth contexts have been polluted with test
// fixtures this way. Dir gives the resolution functions a safe default under
// test: a throwaway directory under os.TempDir, never the real config/cache
// locations. (os.TempDir respects TMPDIR and may itself live under $HOME;
// the guarantee is "not ~/.config/entire or ~/.cache/entire", not "outside
// the home directory".)
//
// The fallback directory is per-process, not per-test: all tests in one test
// binary share it. Tests that need isolation from each other must still set
// the explicit override (e.g. t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())) —
// this package only guarantees that an unisolated test can never touch real
// user state.
//
// testing.Testing() is false in subprocesses, so spawned `entire` binaries
// are NOT covered by this fallback. Harnesses that spawn the real binary
// (integration tests, e2e) must export the override env vars instead — see
// their TestMain functions.
package testdirs

import (
	"os"
	"sync"
	"testing"
)

var (
	mu   sync.Mutex
	dirs = map[string]string{}
)

// Dir returns a stable per-process directory for the named surface (e.g.
// "config", "cache", "tokenstore") when running under `go test`, creating it
// on first use. ok is false in production binaries — testing.Testing() is
// false there — and when the directory cannot be created; callers then fall
// back to their normal home-relative resolution.
func Dir(surface string) (string, bool) {
	if !testing.Testing() {
		return "", false
	}
	mu.Lock()
	defer mu.Unlock()
	if d, exists := dirs[surface]; exists {
		return d, true
	}
	d, err := os.MkdirTemp("", "entire-test-"+surface+"-")
	if err != nil {
		// Never fall back to home-relative defaults under `go test`.
		return os.TempDir(), true
	}
	dirs[surface] = d
	return d, true
}
