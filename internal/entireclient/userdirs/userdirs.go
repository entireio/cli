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

// The environment variables that override these directories.
const (
	// EnvConfigDir overrides the per-user config directory.
	EnvConfigDir = "ENTIRE_CONFIG_DIR"
	// EnvCacheHome overrides the per-user cache directory's parent.
	EnvCacheHome = "XDG_CACHE_HOME"
)

// RequireAbsoluteOverride rejects a non-absolute directory override.
//
// A relative value resolves against the process's working directory, so the
// same environment names a different directory in every process — and, for a
// tool run from inside a repository, typically names a directory inside it.
// That is wrong for all three trees this rule covers. The config directory
// holds bearer tokens (contexts.json and the file token store), the cache
// directory holds cluster discovery state, and the managed plugin directory
// holds binaries whose bin subdirectory main.go prepends to $PATH.
//
// One helper rather than one rule per tree. Every override gets it, not just
// the Entire-specific ones: XDG_DATA_HOME, XDG_CACHE_HOME and LOCALAPPDATA were
// joined unchecked and left for osroot (which refuses a relative root open) and
// for main.go (which restores $PATH) to notice. Those backstops hold, but each
// answers a question of its own, two layers from where this one is decided.
//
// Rejecting is louder than falling through to the platform default, and that is
// deliberate: a misconfigured override is a user error worth surfacing, and for
// the config directory the platform default is the developer's REAL
// ~/.config/entire — quietly substituting it for a test harness's mistyped
// override is the one outcome worse than an error.
// name is whatever the message should call the directory. Pass the environment
// variable where the value plainly came from one, so the reader knows what to
// change; callers a level down from the variable (contexts, discovery) pass the
// role instead, because by then the value may equally have come from the
// platform default.
func RequireAbsoluteOverride(name, value string) error {
	if !filepath.IsAbs(value) {
		return fmt.Errorf("%s must be an absolute path, got %q", name, value)
	}
	return nil
}

// Config returns the per-user config directory.
//
// The string form cannot report a rejected override, so it returns whatever was
// set, and every consumer that turns one into I/O is responsible for learning
// about a bad value BEFORE it creates a directory, takes a lock, or writes.
//
// Deliberately NOT a list of those consumers. Two successive revisions of this
// comment enumerated them and both were wrong within a commit or two -- the
// token store slipped past the first (bearer tokens at ./<value>/tokens.json)
// and plugin_index past the second (an index clone and its lock file in the
// working directory). A comment cannot fail when someone adds a caller.
// TestUserDirConsumersAreAudited can, and does: every call site of Config() or
// Cache() must appear in its ledger with the reason it is safe.
//
// Callers that only display the path are unaffected.
func Config() string {
	dir, _ := configDir() //nolint:errcheck // see doc comment: the consumers report it
	return dir
}

// ConfigDirChecked is Config for a caller that can report a rejected override.
// It returns the directory in both cases, so a caller building a path for a
// message still has one.
func ConfigDirChecked() (string, error) {
	return configDir()
}

// CacheDirChecked is Cache for a caller that can report a rejected override.
//
// Needed for the same reason as its config twin: a consumer that creates a
// directory or takes a lock before handing the path to something that opens a
// root has to learn about a bad override BEFORE it creates anything, and the
// string form cannot tell it. plugin_index was the consumer that needed it.
func CacheDirChecked() (string, error) {
	return cacheDir()
}

// configDir is Config with the override check, for the callers that can report.
func configDir() (string, error) {
	if dir := os.Getenv(EnvConfigDir); dir != "" {
		return dir, RequireAbsoluteOverride(EnvConfigDir, dir)
	}
	if dir, ok := testdirs.Dir("config"); ok {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// nil error deliberately: a machine with no resolvable home is not a
		// user error to report, and ownFallbackDir returns something usable.
		return ownFallbackDir(".config", "entire"), nil //nolint:nilerr // see ownFallbackDir
	}
	return filepath.Join(home, ".config", "entire"), nil
}

// Cache returns the per-user cache directory. See Config on the unreported
// override.
func Cache() string {
	dir, _ := cacheDir() //nolint:errcheck // see Config's doc comment
	return dir
}

// cacheDir is Cache with the override check, for the callers that can report.
func cacheDir() (string, error) {
	if xdg := os.Getenv(EnvCacheHome); xdg != "" {
		return filepath.Join(xdg, "entire"), RequireAbsoluteOverride(EnvCacheHome, xdg)
	}
	if dir, ok := testdirs.Dir("cache"); ok {
		return filepath.Join(dir, "entire"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// See configDir: the fallback is usable, so there is nothing to report.
		return ownFallbackDir(".cache", "entire"), nil //nolint:nilerr // see ownFallbackDir
	}
	return filepath.Join(home, ".cache", "entire"), nil
}

// ownFallbackDir builds the home-relative default for a machine where
// os.UserHomeDir fails, resolved to an absolute path.
//
// The absolutization is the point, and it belongs HERE rather than at each
// consumer, because this is the only place that still knows the difference
// between a path the USER set and a path Entire made up. Both resolvers return
// a plain string, so by the time contexts, discovery or the token store sees
// one, a relative value looks identical either way -- and those consumers must
// refuse a relative override, since it names a different directory in every
// process. Refusing this one too was a regression: on a machine where
// UserHomeDir fails (no HOME, an odd container, a service account) every
// command that touched a saved login or a discovery cache started failing with
// advice about an environment variable the user had never set.
//
// Absolutized rather than refused because it is not a user error to report:
// there is nothing for them to fix, and a cwd-relative directory that at least
// works beats a hard failure. It resolves against the working directory at
// call time, which is the same tradeoff openUserRoot documented when it was
// the only place doing this.
func ownFallbackDir(parts ...string) string {
	rel := filepath.Join(parts...)
	abs, err := filepath.Abs(rel)
	if err != nil {
		return rel
	}
	return abs
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
	return resolveUserRoot(configDir, true)
}

// ConfigRootForRead is ConfigRoot without creating the directory. A missing
// directory is reported unwrapped, so callers classify it with os.IsNotExist.
func ConfigRootForRead() (*os.Root, error) {
	return resolveUserRoot(configDir, false)
}

// CacheRoot returns the shared *os.Root over the per-user cache directory,
// creating it. CacheRootForRead is the same without creation.
func CacheRoot() (*os.Root, error) {
	return resolveUserRoot(cacheDir, true)
}

// CacheRootForRead is CacheRoot without creating the directory.
func CacheRootForRead() (*os.Root, error) {
	return resolveUserRoot(cacheDir, false)
}

// resolveUserRoot fails on a rejected override before any directory is created:
// creating one under a path that resolves against the cwd is the mistake, so it
// must not happen on the way to reporting it.
func resolveUserRoot(resolve func() (string, error), create bool) (*os.Root, error) {
	dir, err := resolve()
	if err != nil {
		return nil, err
	}
	return openUserRoot(dir, create)
}

// openUserRoot absolutizes dir before handing it to the shared registry.
//
// Config and Cache can both return a RELATIVE path even with no override in
// play: when os.UserHomeDir fails they fall back to "." and "" respectively,
// which join to ".config/entire" and ".cache/entire". A relative root would then
// mean a different directory depending on where the process happened to be —
// the same failure the repo anchors exist to remove — so it is resolved once,
// here. That fallback is Entire's own, not something the user set, which is why
// it is absolutized rather than refused the way RequireAbsoluteOverride refuses
// an override.
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
