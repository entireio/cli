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
	"runtime"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
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

// EnsurePrivateDir creates dir as a private, user-only directory (0700) and,
// when it already exists with group or other access, clears those bits.
//
// The tightening step is the point. Config() holds bearer tokens — the login
// JWTs in contexts.json and the file token store's tokens.json — and those
// files are written 0600, but a mode-0755 parent leaks their existence and
// hands anyone on the box a directory they can traverse and enumerate. Because
// os.MkdirAll is a no-op on an existing path, whichever caller created the
// directory first fixes its mode permanently: a version check that ran before
// the first login used to leave it 0755 for good, and the credential stores'
// own MkdirAll(0700) could never repair it.
//
// Only the group and other bits are ever cleared: the owner bits are carried
// across untouched, so a directory the user deliberately made stricter than
// 0700 stays that way whether or not it also needed tightening (0500 survives,
// 0555 becomes 0500). Masking can leave the owner no access at all, which is
// the same thing an already-private 0000 directory gets: this function makes a
// directory private, and does not claim to make it usable.
//
// Windows has no unix permission bits (Go reports synthetic modes and Chmod
// only toggles the read-only flag), so the tightening step is skipped there.
func EnsurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat %s: %w", dir, err)
	}
	if info.Mode().Perm()&0o077 == 0 {
		return nil
	}
	if err := os.Chmod(dir, info.Mode().Perm()&^0o077); err != nil {
		return fmt.Errorf("tighten %s: %w", dir, err)
	}
	return nil
}

// ConfigRoot returns the shared *os.Root over the per-user config directory,
// creating the directory if it does not exist. ConfigRootForRead is the same
// without creation.
//
// Every read and write of contexts.json, version_check.json, and the
// file-backed token store goes through this rather than through a path joined
// onto Config(). The names inside are fixed today, but the point of the root is
// that they do not have to stay that way: a future context name, cluster slug,
// or token key that reaches a filename cannot escape the directory, and a
// symlink swapped in between resolution and open surfaces as an error rather
// than a redirected write to somewhere in the user's home.
//
// The create/no-create split matters here for the same reason it does for
// .entire: a command that only looks for a saved login must not leave an
// ~/.config/entire behind on a machine that has never used one.
func ConfigRoot() (*os.Root, error) {
	return openUserRoot(Config(), true)
}

// ConfigRootForRead is ConfigRoot without creating the directory. A missing
// directory is reported unwrapped, so callers classify it with os.IsNotExist.
func ConfigRootForRead() (*os.Root, error) {
	return openUserRoot(Config(), false)
}

// CacheRoot returns the shared *os.Root over the per-user cache directory,
// creating it. CacheRootForRead is the same without creation.
func CacheRoot() (*os.Root, error) {
	return openUserRoot(Cache(), true)
}

// CacheRootForRead is CacheRoot without creating the directory.
func CacheRootForRead() (*os.Root, error) {
	return openUserRoot(Cache(), false)
}

// openUserRoot absolutizes dir before handing it to the shared registry.
//
// Config and Cache can both return a RELATIVE path: when os.UserHomeDir fails
// they fall back to "." and "" respectively, which join to ".config/entire" and
// ".cache/entire". A relative root would then mean a different directory
// depending on where the process happened to be — the same failure the repo
// anchors exist to remove — so it is resolved once, here.
func openUserRoot(dir string, create bool) (*os.Root, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", dir, err)
	}
	if create {
		// EnsurePrivateDir, not os.MkdirAll(abs, 0o700): these directories hold
		// login tokens, and MkdirAll is a no-op on a path that already exists,
		// so whichever caller created the directory first would fix its mode
		// permanently. Routing creation through here means every consumer of a
		// root inherits the tightening instead of each one having to remember
		// to ask for it.
		if err := EnsurePrivateDir(abs); err != nil {
			return nil, err
		}
	}
	return osroot.Shared(abs) //nolint:wrapcheck // Shared names the directory and returns a missing one unwrapped
}
