package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// Managed plugin storage. The kubectl-style dispatcher in plugin.go resolves
// `entire-<name>` binaries from $PATH, period. To let `entire plugin install`
// be additive rather than a parallel mechanism, this file provides:
//
//  1. PluginBinDir() — a per-user managed dir that main.go prepends to PATH
//     before the dispatcher runs. Anything dropped here (or symlinked here)
//     becomes invocable as `entire <name>` without the user fiddling with PATH.
//
//  2. PluginDataDir(name) — a per-plugin durable storage dir, passed to plugins
//     as ENTIRE_PLUGIN_DATA_DIR. Independent of where the binary itself lives
//     so plugins installed via PATH and via the managed dir get the same
//     contract.
//
// Honors ENTIRE_PLUGIN_DIR as a parent-dir override; falls back to
// XDG_DATA_HOME, then a platform default.

const (
	pluginEnvPluginDir      = "ENTIRE_PLUGIN_DIR"
	pluginManagedBinSubdir  = "bin"
	pluginManagedDataSubdir = "data"
	pluginEnvPluginData     = "ENTIRE_PLUGIN_DATA_DIR"
	// Path segments for the managed plugin tree. Kept as separate
	// segments (rather than "entire/plugins") so filepath.Join produces
	// platform-native separators on Windows.
	pluginManagedTopDir = "entire"
	pluginManagedSubDir = "plugins"
)

// pluginParentDir returns the per-user directory that holds the managed
// plugin storage. Resolution, in order:
//
//  1. ENTIRE_PLUGIN_DIR (cross-platform override).
//  2. On Windows: LOCALAPPDATA if set, else ~\AppData\Local\entire\plugins.
//  3. On Unix: XDG_DATA_HOME if set, else ~/.local/share/entire/plugins.
//
// XDG_DATA_HOME is deliberately ignored on Windows even when set (e.g. in
// MSYS/Cygwin) — Windows users expect Windows conventions, and routing
// through XDG would produce a surprising location.
//
// os.UserHomeDir is called only when no env-var branch resolves, so a
// degenerate environment with $LOCALAPPDATA or $XDG_DATA_HOME but no home
// still returns a usable path.
func pluginParentDir() (string, error) {
	// ENTIRE_PLUGIN_DIR must be absolute. A relative value would resolve
	// against the user's CWD at startup — typically inside their repo —
	// which is the wrong place for managed plugin storage. Reject loudly
	// rather than silently falling through to the platform default, since
	// a misconfigured override is almost certainly a user error worth
	// surfacing.
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

// PluginBinDir returns the managed install directory. Binaries (or symlinks)
// placed here are auto-discovered by the kubectl-style dispatcher because
// main.go prepends this dir to PATH before MaybeRunPlugin runs.
func PluginBinDir() (string, error) {
	parent, err := pluginParentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, pluginManagedBinSubdir), nil
}

// PluginDataDir returns the per-plugin data directory for the given bare name
// (e.g. "pgr" for `entire-pgr`). The returned path is not created — that's
// the plugin's responsibility on first use.
//
// Returns an error for names the dispatcher would never invoke (empty,
// flag-shaped, agent-protocol-reserved, "."/".." path-traversal, slashes).
// This guarantees ENTIRE_PLUGIN_DATA_DIR always points inside the managed
// data subtree.
func PluginDataDir(name string) (string, error) {
	if err := validatePluginName(name); err != nil {
		return "", err
	}
	parent, err := pluginParentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, pluginManagedDataSubdir, name), nil
}

// validatePluginName mirrors the dispatcher's isPluginCandidate rules and
// returns a descriptive error for invalid names. Used by every entry point
// that takes a plugin name from outside the dispatcher (data dir resolution,
// managed-dir install).
func validatePluginName(name string) error {
	if name == "" {
		return errors.New("plugin name is empty")
	}
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("plugin name %q must not start with '-'", name)
	}
	if strings.HasPrefix(name, "agent-") {
		return fmt.Errorf("plugin name %q is reserved for the external agent protocol", name)
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("plugin name %q must not contain path separators", name)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("plugin name %q is not a valid identifier", name)
	}
	return nil
}

// EnsurePluginBinDir creates the managed install dir if it doesn't exist.
func EnsurePluginBinDir() (string, error) {
	dir, err := PluginBinDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create plugin bin dir: %w", err)
	}
	return dir, nil
}

// PrependPluginBinDirToPATH prepends the managed bin dir to the process's
// PATH so the kubectl dispatcher discovers managed-installed plugins.
// Idempotent against an already-prepended dir.
//
// Returns a restore closure the caller invokes to revert PATH to its
// previous value. Restoring matters when no plugin runs: built-in commands
// and the subprocesses they spawn (git, hooks, less, …) should see the
// user's original PATH, not one with the managed plugin dir prepended.
// When a plugin *is* dispatched, callers can simply skip the restore — the
// process exits anyway, and the plugin child intentionally inherits the
// prepended PATH so it can spawn sibling managed plugins.
//
// Errors and no-op cases (already-prepended, lookup failure) return a
// no-op restore so callers always have a safe func to call. Failures are
// emitted at debug level — the surface symptom ("my managed plugin
// doesn't run") is otherwise silent and hard to diagnose; a debug log
// surfaces the cause for users who flip log_level=DEBUG.
func PrependPluginBinDirToPATH(ctx context.Context) func() {
	dir, err := PluginBinDir()
	if err != nil || dir == "" {
		if err != nil {
			logging.Debug(ctx, "skip prepend managed plugin bin dir to PATH: resolve failed", slog.String("error", err.Error()))
		}
		return func() {}
	}
	prev := os.Getenv("PATH")
	sep := string(os.PathListSeparator)
	if prev == "" {
		if err := os.Setenv("PATH", dir); err != nil {
			logging.Debug(ctx, "skip prepend managed plugin bin dir to PATH: setenv failed", slog.String("error", err.Error()))
			return func() {}
		}
		return func() { _ = os.Setenv("PATH", "") }
	}
	// Idempotent: if the first entry already matches, leave PATH alone.
	// Windows PATH lookups are case-insensitive (`C:\Foo` and `c:\foo`
	// resolve identically), so a case-different first entry is the same
	// dir and we should not double-prepend.
	first := prev
	if i := strings.Index(prev, sep); i >= 0 {
		first = prev[:i]
	}
	if pathEntriesEqual(first, dir) {
		return func() {}
	}
	if err := os.Setenv("PATH", dir+sep+prev); err != nil {
		logging.Debug(ctx, "skip prepend managed plugin bin dir to PATH: setenv failed", slog.String("error", err.Error()))
		return func() {}
	}
	return func() { _ = os.Setenv("PATH", prev) }
}

// pathEntriesEqual reports whether two PATH entries refer to the same dir.
// filepath.Clean normalizes trailing separators and (on Windows) slash
// orientation; case folding handles Windows' case-insensitive lookup.
func pathEntriesEqual(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if runtime.GOOS == windowsGOOS {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// InstalledPlugin describes a single entry in the managed bin dir.
type InstalledPlugin struct {
	// Name is the bare plugin name (without the `entire-` prefix and any
	// platform-specific extension).
	Name string
	// Path is the absolute path inside the managed bin dir.
	Path string
	// Symlink is true when Path is a symlink to a source location elsewhere
	// (the typical local-dev install). LinkTarget is populated in that case.
	Symlink    bool
	LinkTarget string
}

// ListInstalledPlugins enumerates entries in the managed bin dir whose name
// starts with `entire-`. Sorted by bare name. A missing dir returns no error
// and an empty slice.
func ListInstalledPlugins() ([]*InstalledPlugin, error) {
	dir, err := PluginBinDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read plugin bin dir: %w", err)
	}

	var out []*InstalledPlugin
	for _, e := range entries {
		full := e.Name()
		if !strings.HasPrefix(full, pluginBinaryPrefix) {
			continue
		}
		bare := bareNameFromBinaryName(full)
		if bare == "" {
			continue
		}
		path := filepath.Join(dir, full)
		info, err := os.Lstat(path)
		if err != nil {
			continue
		}
		ip := &InstalledPlugin{Name: bare, Path: path}
		if info.Mode()&os.ModeSymlink != 0 {
			ip.Symlink = true
			if target, err := os.Readlink(path); err == nil {
				ip.LinkTarget = target
			}
		}
		out = append(out, ip)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// FindInstalledPlugin returns the entry for the given bare name, or nil if
// it isn't installed in the managed dir.
func FindInstalledPlugin(name string) (*InstalledPlugin, error) {
	all, err := ListInstalledPlugins()
	if err != nil {
		return nil, err
	}
	for _, p := range all {
		if p.Name == name {
			return p, nil
		}
	}
	return nil, nil //nolint:nilnil // not-installed signal
}

// InstallPluginOptions configures InstallPluginFromPath.
type InstallPluginOptions struct {
	// SourcePath is the absolute (or working-dir-relative) path to the plugin
	// executable. Its basename — minus any platform extension — must match
	// `entire-<name>` so the dispatcher can resolve it.
	SourcePath string
	// Force replaces an already-installed plugin with the same name.
	Force bool
}

// InstallPluginFromPath symlinks SourcePath into the managed bin dir. The
// caller is responsible for built-in conflict checks (resolvePlugin already
// gates dispatch on rootCmd.Find — installing a name that shadows a built-in
// is allowed but the built-in still wins at runtime).
//
// Refuses names the dispatcher will never invoke (agent-protocol prefix,
// flag-shaped, "."/"..", slashes), and refuses self-install when the source
// is the same file as the would-be managed entry. The replace step is
// atomic: a new symlink is created at <dest>.tmp and renamed onto <dest>,
// so a failed --force never leaves the previous install missing.
func InstallPluginFromPath(opts InstallPluginOptions) (*InstalledPlugin, error) {
	src, err := filepath.Abs(opts.SourcePath)
	if err != nil {
		return nil, fmt.Errorf("resolve source path: %w", err)
	}
	srcInfo, err := os.Stat(src)
	if err != nil {
		return nil, fmt.Errorf("stat source: %w", err)
	}
	if srcInfo.IsDir() {
		return nil, fmt.Errorf("source must be a file, got directory: %s", src)
	}
	base := filepath.Base(src)
	bare := bareNameFromBinaryName(base)
	if bare == "" {
		return nil, fmt.Errorf("source basename %q must start with %q and have a runnable name", base, pluginBinaryPrefix)
	}
	if err := validatePluginName(bare); err != nil {
		return nil, fmt.Errorf("derived plugin name from %q is not dispatchable: %w", base, err)
	}
	if runtime.GOOS != windowsGOOS && srcInfo.Mode()&0o111 == 0 {
		return nil, fmt.Errorf("source %s is not executable (chmod +x)", src)
	}

	binDir, err := EnsurePluginBinDir()
	if err != nil {
		return nil, err
	}
	dest := filepath.Join(binDir, base)

	// Reject self-install: the source path already equals the managed
	// destination path. Without this guard, `--force` would atomically
	// rename a tmp symlink onto its own target file and the underlying
	// binary would be unlinked.
	//
	// We deliberately do NOT use os.SameFile here. The legitimate
	// repeat-install case has dest as a symlink we created on a prior
	// install; SameFile would resolve dest through the symlink to src
	// and falsely trip. Path equality is the precise risky case.
	if filepath.Clean(src) == filepath.Clean(dest) {
		return nil, fmt.Errorf("source %s is already the managed entry; nothing to install", src)
	}

	// Conflict check on the bare name (not the exact filename). On Windows,
	// entire-foo.exe / .bat / .cmd all map to bare name "foo"; checking only
	// the destination filename would let a second install of a different
	// extension silently coexist with the first, with PATHEXT ordering then
	// deciding which one runs. List all variants and require --force when
	// any exist.
	conflicts, err := installedVariantsByBareName(bare)
	if err != nil {
		return nil, err
	}
	if len(conflicts) > 0 && !opts.Force {
		return nil, fmt.Errorf("plugin %q already installed at %s; use --force to replace", bare, conflicts[0].Path)
	}
	// With --force, remove every variant other than the one we're about to
	// atomically overwrite. The same-extension case is handled by the
	// rename below, which atomically replaces dest.
	for _, c := range conflicts {
		if filepath.Clean(c.Path) == filepath.Clean(dest) {
			continue
		}
		if err := os.Remove(c.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("remove existing variant %s: %w", c.Path, err)
		}
	}

	// Atomic replace via tmp + rename. Rename is atomic on POSIX and
	// replaces an existing target on Windows (Go's os.Rename uses
	// MoveFileEx with MOVEFILE_REPLACE_EXISTING). If linking or rename
	// fails, the previously installed plugin (if any) is unaffected.
	//
	// The tmp path uses a random suffix and a `.install-` prefix that does
	// NOT match `entire-`. This protects against two distinct hazards:
	//   1. A user can have a legitimate plugin named "foo.tmp" (file
	//      "entire-foo.tmp"), which a naive `dest + ".tmp"` would clobber.
	//   2. ListInstalledPlugins filters by `entire-` prefix, so a tmp that
	//      starts with `.install-` will not appear in `entire plugin list`
	//      while the install is in progress.
	tmpDest, err := makeInstallTmpPath(binDir)
	if err != nil {
		return nil, err
	}
	if err := materializeManagedEntry(src, tmpDest, srcInfo); err != nil {
		return nil, fmt.Errorf("install plugin: %w", err)
	}
	if err := os.Rename(tmpDest, dest); err != nil {
		_ = os.Remove(tmpDest) // best-effort cleanup; previous install is intact
		return nil, fmt.Errorf("install plugin: %w", err)
	}
	return FindInstalledPlugin(bare)
}

// installedVariantsByBareName returns every managed entry whose bare name
// matches name. On Unix this is at most one entry; on Windows multiple
// extensions can map to the same bare name.
func installedVariantsByBareName(name string) ([]*InstalledPlugin, error) {
	all, err := ListInstalledPlugins()
	if err != nil {
		return nil, err
	}
	var out []*InstalledPlugin
	for _, p := range all {
		if p.Name == name {
			out = append(out, p)
		}
	}
	return out, nil
}

// makeInstallTmpPath returns a unique scratch path inside binDir for the
// in-progress install. The `.install-` prefix is deliberately distinct from
// pluginBinaryPrefix so ListInstalledPlugins ignores it; a 16-char hex
// suffix from crypto/rand makes collisions vanishingly unlikely and keeps
// concurrent installs safe from each other.
func makeInstallTmpPath(binDir string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate install tmp suffix: %w", err)
	}
	return filepath.Join(binDir, ".install-"+hex.EncodeToString(b[:])), nil
}

// materializeManagedEntry creates dest as a reference to src, falling back
// through symlink → hardlink → copy in that order.
//
// Symlink-first preserves the dev-loop property that rebuilding the source
// is immediately reflected in the managed entry. The fallbacks exist for
// Windows: os.Symlink there requires Developer Mode or admin, and silently
// breaks `entire plugin install` for typical users without either. Mirrors
// the pattern in setup_test.go's copyExecutable.
//
// On a successful copy the file mode of the source is preserved so the
// executable bit survives.
func materializeManagedEntry(src, dest string, srcInfo os.FileInfo) error {
	if err := os.Symlink(src, dest); err == nil {
		return nil
	}
	if err := os.Link(src, dest); err == nil {
		return nil
	}
	return copyFileStreaming(src, dest, srcInfo)
}

// copyFileStreaming copies src to dest in fixed-size buffers, preserving the
// source's executable mode. Plugin binaries can be tens of megabytes; using
// io.Copy avoids the heap spike of reading the whole file into memory.
func copyFileStreaming(src, dest string, srcInfo os.FileInfo) error {
	mode := srcInfo.Mode().Perm()
	if mode == 0 {
		mode = 0o755
	}
	in, err := os.Open(src) //nolint:gosec // src is the user-provided plugin executable; reading it is the point
	if err != nil {
		return fmt.Errorf("open source for copy fallback: %w", err)
	}
	defer in.Close()

	// G304: dest is always inside the managed bin dir. The basename comes
	// from a validated plugin name (validatePluginName ran upstream), and
	// the parent dir comes from EnsurePluginBinDir.
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode) //nolint:gosec // dest is constrained to the managed bin dir
	if err != nil {
		return fmt.Errorf("open destination for copy fallback: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dest)
		return fmt.Errorf("copy fallback: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dest)
		return fmt.Errorf("close destination after copy fallback: %w", err)
	}
	return nil
}

// RemoveInstalledPlugin removes every managed-dir entry whose bare name
// matches name. Symlinks are unlinked without touching the source file.
//
// Iterating all variants matters on Windows, where entire-foo.exe,
// entire-foo.bat, and entire-foo.cmd all map to bare name "foo" and could
// otherwise leave a runnable variant behind after `entire plugin remove foo`.
// On Unix the loop typically runs once.
func RemoveInstalledPlugin(name string) error {
	variants, err := installedVariantsByBareName(name)
	if err != nil {
		return err
	}
	if len(variants) == 0 {
		return fmt.Errorf("plugin %q is not installed in the managed directory", name)
	}
	for _, p := range variants {
		if err := os.Remove(p.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove plugin entry %s: %w", p.Path, err)
		}
	}
	return nil
}

// bareNameFromBinaryName turns a plugin executable's basename into the bare
// name the dispatcher uses (e.g. "entire-pgr" → "pgr"). Returns "" if the
// input doesn't match the expected shape.
//
// Extension stripping is platform-conditional:
//
//   - On Windows, .exe/.bat/.cmd are the natural extensions for executables
//     and exec.LookPath resolves them via PATHEXT, so we strip them so the
//     bare name a user types ("pgr") matches both the managed-list display
//     and the dispatcher's lookup.
//
//   - On Unix, exec.LookPath matches the exact filename. If we stripped here,
//     "entire-pgr.exe" would be listed as "pgr" and the user would type
//     "entire pgr", but the dispatcher's exec.LookPath("entire-pgr") would
//     not find "entire-pgr.exe". Leaving the dot in place keeps the listed
//     name aligned with the only invocation that actually resolves
//     ("entire pgr.exe"), avoiding silent shadowing surprises.
func bareNameFromBinaryName(base string) string {
	if !strings.HasPrefix(base, pluginBinaryPrefix) {
		return ""
	}
	cleaned := base
	if runtime.GOOS == windowsGOOS {
		// Reuse the canonical Windows-executable-extension list from
		// agent/external rather than maintaining a parallel copy. plugin.go
		// already depends on this package for isAgentProtocolBinary, so
		// there's no new layering cost.
		cleaned = external.StripExeExt(base)
	}
	bare := strings.TrimPrefix(cleaned, pluginBinaryPrefix)
	if bare == "" {
		return ""
	}
	return bare
}
