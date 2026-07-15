package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/plugins"
)

// Lua plugin distribution. Lua plugins live one directory per plugin under the
// managed lua/ tree (see plugins.UserLuaPluginsDir), a sibling of the binary
// store's bin/ and data/ dirs so the two never collide. Installing only places
// files; a plugin stays inert until it is allow-listed and enabled in settings,
// so `install` is safe to run on an untrusted URL — the code cannot run until
// the user explicitly opts in.

// luaInstallMetaFile records how a Lua plugin was installed so `update` can
// reproduce the pin.
const luaInstallMetaFile = ".entire-install.json"

// luaInstallMeta is persisted in each installed plugin dir.
type luaInstallMeta struct {
	Source string `json:"source"`        // git URL or local path
	Ref    string `json:"ref,omitempty"` // pinned tag/branch/sha (git installs)
	Type   string `json:"type"`          // "git" or "local"
}

// InstalledLuaPlugin describes a Lua plugin in the managed lua/ dir.
type InstalledLuaPlugin struct {
	Name    string
	Dir     string
	Version string
	Source  string
	Ref     string
	Type    string
}

// looksLikeGitURL reports whether src should be treated as a git remote rather
// than a local path.
func looksLikeGitURL(src string) bool {
	if strings.HasSuffix(src, ".git") || strings.HasPrefix(src, "git@") {
		return true
	}
	for _, scheme := range []string{"http://", "https://", "git://", "ssh://"} {
		if strings.HasPrefix(src, scheme) {
			return true
		}
	}
	return false
}

// ListInstalledLuaPlugins enumerates plugin dirs under the managed lua/ tree.
// A missing dir returns no error and an empty slice.
func ListInstalledLuaPlugins() ([]*InstalledLuaPlugin, error) {
	luaDir, err := plugins.UserLuaPluginsDir()
	if err != nil {
		return nil, fmt.Errorf("resolve lua plugin dir: %w", err)
	}
	entries, err := os.ReadDir(luaDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read lua plugin dir: %w", err)
	}
	var out []*InstalledLuaPlugin
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(luaDir, e.Name())
		manifest, err := plugins.LoadManifestFromDir(dir)
		if err != nil {
			continue // not a valid plugin dir
		}
		meta := readLuaInstallMeta(dir)
		out = append(out, &InstalledLuaPlugin{
			Name:    manifest.Name,
			Dir:     dir,
			Version: manifest.Version,
			Source:  meta.Source,
			Ref:     meta.Ref,
			Type:    meta.Type,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// FindInstalledLuaPlugin returns the installed Lua plugin with the given name,
// or nil when not installed.
func FindInstalledLuaPlugin(name string) (*InstalledLuaPlugin, error) {
	all, err := ListInstalledLuaPlugins()
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

// InstallLuaPluginFromGit clones url into the managed lua/ tree, keyed by the
// cloned plugin's manifest name. An optional ref pins a tag, branch, or commit.
func InstallLuaPluginFromGit(ctx context.Context, url, ref string, force bool) (*InstalledLuaPlugin, error) {
	luaDir, err := ensureLuaPluginsDir()
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(luaDir, ".clone-")
	if err != nil {
		return nil, fmt.Errorf("create clone temp dir: %w", err)
	}
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.RemoveAll(tmp)
		}
	}()

	if err := cloneGitPlugin(ctx, url, ref, tmp); err != nil {
		return nil, err
	}
	manifest, err := plugins.LoadManifestFromDir(tmp)
	if err != nil {
		return nil, fmt.Errorf("cloned source is not a valid plugin: %w", err)
	}

	dest, err := placeLuaPlugin(luaDir, manifest.Name, tmp, force)
	if err != nil {
		return nil, err
	}
	cleanupTmp = false // renamed into place

	writeLuaInstallMeta(dest, luaInstallMeta{Source: url, Ref: ref, Type: "git"})
	return FindInstalledLuaPlugin(manifest.Name)
}

// InstallLuaPluginFromPath copies a local plugin directory into the managed
// lua/ tree, keyed by the manifest name.
func InstallLuaPluginFromPath(ctx context.Context, srcDir string, force bool) (*InstalledLuaPlugin, error) {
	manifest, err := plugins.LoadManifestFromDir(srcDir)
	if err != nil {
		return nil, fmt.Errorf("source is not a valid plugin: %w", err)
	}
	luaDir, err := ensureLuaPluginsDir()
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(luaDir, ".copy-")
	if err != nil {
		return nil, fmt.Errorf("create copy temp dir: %w", err)
	}
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.RemoveAll(tmp)
		}
	}()

	if err := copyPluginDir(ctx, srcDir, tmp); err != nil {
		return nil, err
	}
	dest, err := placeLuaPlugin(luaDir, manifest.Name, tmp, force)
	if err != nil {
		return nil, err
	}
	cleanupTmp = false

	absSrc, _ := filepath.Abs(srcDir) //nolint:errcheck // best-effort for metadata display
	writeLuaInstallMeta(dest, luaInstallMeta{Source: absSrc, Type: "local"})
	return FindInstalledLuaPlugin(manifest.Name)
}

// UpdateLuaPlugin updates a git-installed Lua plugin: it fetches and, when a ref
// is pinned, checks it out (fast-forwarding a pinned branch); otherwise it
// fast-forward pulls the default branch. Local-path installs cannot be updated.
func UpdateLuaPlugin(ctx context.Context, name string) error {
	p, err := FindInstalledLuaPlugin(name)
	if err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("lua plugin %q is not installed", name)
	}
	if _, statErr := os.Stat(filepath.Join(p.Dir, ".git")); statErr != nil {
		return fmt.Errorf("lua plugin %q was not installed from git; reinstall to update", name)
	}
	if err := runPluginGit(ctx, p.Dir, "fetch", "--tags", "--prune", "origin"); err != nil {
		return err
	}
	if p.Ref != "" {
		if err := runPluginGit(ctx, p.Dir, "checkout", "--quiet", p.Ref); err != nil {
			return err
		}
		// Fast-forward a pinned branch to its upstream; a no-op (ignored error)
		// for a pinned tag or commit, which have no origin/<ref>.
		_ = runPluginGit(ctx, p.Dir, "merge", "--ff-only", "origin/"+p.Ref) //nolint:errcheck // pinned tag/sha has no upstream branch; best-effort ff
		return nil
	}
	if err := runPluginGit(ctx, p.Dir, "pull", "--ff-only"); err != nil {
		return err
	}
	return nil
}

// RemoveLuaPlugin removes an installed Lua plugin's directory.
func RemoveLuaPlugin(name string) error {
	if err := plugins.ValidatePluginName(name); err != nil {
		return fmt.Errorf("invalid plugin name: %w", err)
	}
	p, err := FindInstalledLuaPlugin(name)
	if err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("lua plugin %q is not installed", name)
	}
	if err := os.RemoveAll(p.Dir); err != nil {
		return fmt.Errorf("remove lua plugin dir: %w", err)
	}
	return nil
}

// ensureLuaPluginsDir resolves and creates the managed lua/ dir.
func ensureLuaPluginsDir() (string, error) {
	luaDir, err := plugins.UserLuaPluginsDir()
	if err != nil {
		return "", fmt.Errorf("resolve lua plugin dir: %w", err)
	}
	if err := os.MkdirAll(luaDir, 0o750); err != nil {
		return "", fmt.Errorf("create lua plugin dir: %w", err)
	}
	return luaDir, nil
}

// placeLuaPlugin atomically moves a staged plugin dir (tmp) to lua/<name>,
// honoring force for an existing install.
func placeLuaPlugin(luaDir, name, tmp string, force bool) (string, error) {
	if err := plugins.ValidatePluginName(name); err != nil {
		return "", fmt.Errorf("plugin name %q is not dispatchable: %w", name, err)
	}
	dest := filepath.Join(luaDir, name)
	if _, err := os.Stat(dest); err == nil {
		if !force {
			return "", fmt.Errorf("lua plugin %q already installed at %s; use --force to replace", name, dest)
		}
		if err := os.RemoveAll(dest); err != nil {
			return "", fmt.Errorf("replace existing plugin: %w", err)
		}
	}
	if err := os.Rename(tmp, dest); err != nil {
		return "", fmt.Errorf("install plugin: %w", err)
	}
	return dest, nil
}

// cloneGitPlugin clones url into dest. A ref triggers a full clone + checkout
// (so tags, branches, and commits all work); no ref does a shallow clone of the
// default branch.
func cloneGitPlugin(ctx context.Context, url, ref, dest string) error {
	if ref == "" {
		if err := runPluginGit(ctx, "", "clone", "--depth", "1", url, dest); err != nil {
			return err
		}
		return nil
	}
	if err := runPluginGit(ctx, "", "clone", url, dest); err != nil {
		return err
	}
	if err := runPluginGit(ctx, dest, "checkout", "--quiet", ref); err != nil {
		return err
	}
	return nil
}

// copyPluginDir copies a plugin source dir into dest, excluding any .git dir.
func copyPluginDir(ctx context.Context, src, dest string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("copy plugin: %w", err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("read plugin source: %w", err)
	}
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		from := filepath.Join(src, e.Name())
		to := filepath.Join(dest, e.Name())
		if e.IsDir() {
			if err := os.MkdirAll(to, 0o750); err != nil {
				return fmt.Errorf("create %s: %w", to, err)
			}
			if err := copyPluginDir(ctx, from, to); err != nil {
				return err
			}
			continue
		}
		data, err := os.ReadFile(from) //nolint:gosec // from is under the user-provided plugin source dir being installed
		if err != nil {
			return fmt.Errorf("read %s: %w", from, err)
		}
		//nolint:gosec // to is under the managed staging dir; the name comes from the user's own plugin source being installed
		if err := os.WriteFile(to, data, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", to, err)
		}
	}
	return nil
}

// runPluginGit runs a git command in dir (or the process cwd when dir is
// empty). Errors include the git output for diagnosis.
func runPluginGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func readLuaInstallMeta(dir string) luaInstallMeta {
	var meta luaInstallMeta
	data, err := os.ReadFile(filepath.Join(dir, luaInstallMetaFile)) //nolint:gosec // path inside the managed plugin dir
	if err != nil {
		return meta
	}
	_ = json.Unmarshal(data, &meta) //nolint:errcheck // best-effort; absent/corrupt metadata just yields empty fields
	return meta
}

func writeLuaInstallMeta(dir string, meta luaInstallMeta) {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, luaInstallMetaFile), data, 0o600) //nolint:errcheck // metadata is advisory; a write failure only degrades `update`
}
