package plugins

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	lua "github.com/yuin/gopher-lua"
)

// Registry holds the loaded, allow-listed plugins for a process and dispatches
// hook events to them. All access to plugin Lua states goes through the
// registry, which serializes it (LStates are not goroutine-safe).
type Registry struct {
	// mu serializes hook dispatch: LStates are not goroutine-safe, so at most
	// one callback runs at a time even if two hooks fire concurrently.
	mu      sync.Mutex
	plugins []*LoadedPlugin
}

// discoverySource pairs a directory to scan with the origin to tag plugins from
// it. User plugins are scanned before repo plugins so a user-installed plugin
// wins a name collision with a repo-local one.
type discoverySource struct {
	dir    string
	source Source
}

// Discover loads every allow-listed, enabled plugin from the per-user managed
// dir and the repo-local .entire/plugins dir. worktreeRoot may be empty when
// not inside a repo (only user plugins are considered then). A plugin is loaded
// only when settings has an entry for it with enabled=true; repo-local plugins
// therefore never auto-run without an explicit opt-in.
//
// s is the merged team+local allow-list and governs user-global plugins.
// localGrants is the allow-list from the per-developer .entire/settings.local.json
// only (see settings.LocalPluginGrants) and is the *sole* authority for
// repo-local plugins: a committed team .entire/settings.json can neither enable
// a repo-local plugin nor grant it capabilities. This closes the trust hole
// where cloning a hostile repo that ships both a .entire/plugins/<name>
// directory and a team settings enable would auto-run the plugin's code.
//
// Discovery is resilient: a plugin that fails to parse or load is logged and
// skipped rather than failing the whole registry — one broken third-party
// plugin must not break the CLI.
func Discover(ctx context.Context, worktreeRoot string, s *settings.EntireSettings, localGrants map[string]settings.PluginSettings) *Registry {
	logCtx := logging.WithComponent(ctx, "plugins")
	r := &Registry{}
	// Kill switch: an operator can disable all plugins process-wide regardless
	// of the allow-list.
	if pluginsDisabledByEnv() {
		logging.Debug(logCtx, "lua plugins disabled via "+pluginsDisabledEnv)
		return r
	}
	// Fast path: with no enabled allow-list entry, nothing can run. Skip all
	// filesystem work so the common (no-plugins) case is essentially free.
	if !hasEnabledPlugin(s) {
		return r
	}

	var sources []discoverySource
	if userDir, err := UserLuaPluginsDir(); err == nil {
		sources = append(sources, discoverySource{dir: userDir, source: SourceUser})
	} else {
		logging.Debug(logCtx, "skip user plugin dir: resolve failed", slog.String("error", err.Error()))
	}
	if worktreeRoot != "" {
		sources = append(sources, discoverySource{dir: RepoLuaPluginsDir(worktreeRoot), source: SourceRepo})
	}

	loaded := make(map[string]struct{})
	for _, src := range sources {
		entries, err := os.ReadDir(src.dir)
		if err != nil {
			if !os.IsNotExist(err) {
				logging.Debug(logCtx, "read plugin dir failed", slog.String("dir", src.dir), slog.String("error", err.Error()))
			}
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			pluginDir := filepath.Join(src.dir, e.Name())
			r.tryLoadCandidate(ctx, logCtx, pluginDir, src.source, worktreeRoot, s, localGrants, loaded)
		}
	}
	return r
}

// hasEnabledPlugin reports whether settings enables at least one plugin.
func hasEnabledPlugin(s *settings.EntireSettings) bool {
	if s == nil {
		return false
	}
	for _, ps := range s.Plugins {
		if ps.Enabled {
			return true
		}
	}
	return false
}

// grantForSource returns the allow-list entry that governs a plugin and whether
// one exists, enforcing that repo-local plugins may only be enabled from the
// per-developer local settings file.
//
// User-global plugins (SourceUser) use the merged team+local allow-list, as
// before. Repo-local plugins (SourceRepo) use ONLY the grants sourced from
// .entire/settings.local.json: a committed team .entire/settings.json can
// neither enable a repo-local plugin nor grant it capabilities. This is the
// trust boundary — cloning a hostile repo that ships both a .entire/plugins/<name>
// directory and a team settings enable must not execute the plugin's code.
func grantForSource(source Source, name string, s *settings.EntireSettings, localGrants map[string]settings.PluginSettings) (settings.PluginSettings, bool) {
	if source == SourceRepo {
		grant, ok := localGrants[name]
		return grant, ok
	}
	return s.PluginGrant(name)
}

// tryLoadCandidate parses a candidate plugin dir, checks the allow-list, and
// loads it when enabled. Failures are logged and skipped.
func (r *Registry) tryLoadCandidate(ctx, logCtx context.Context, dir string, source Source, worktreeRoot string, s *settings.EntireSettings, localGrants map[string]settings.PluginSettings, loaded map[string]struct{}) {
	if _, statErr := os.Stat(filepath.Join(dir, ManifestFileName)); statErr != nil {
		return // not a plugin dir
	}
	manifest, err := LoadManifestFromDir(dir)
	if err != nil {
		logging.Warn(logCtx, "skip plugin: invalid manifest", slog.String("dir", dir), slog.String("error", err.Error()))
		return
	}
	name := manifest.Name
	if _, dup := loaded[name]; dup {
		logging.Warn(logCtx, "skip plugin: name already loaded from higher-precedence source",
			slog.String("plugin", name), slog.String("dir", dir), slog.String("source", string(source)))
		return
	}
	grant, ok := grantForSource(source, name, s, localGrants)
	if !ok || !grant.Enabled {
		// Not allow-listed (or disabled): never auto-run. Logged at debug so a
		// repo shipping a plugin the user hasn't opted into stays quiet.
		logging.Debug(logCtx, "skip plugin: not enabled in allow-list",
			slog.String("plugin", name), slog.String("source", string(source)))
		return
	}
	p, err := LoadPlugin(ctx, dir, source, worktreeRoot, grant)
	if err != nil {
		logging.Warn(logCtx, "skip plugin: load failed", slog.String("plugin", name), slog.String("error", err.Error()))
		return
	}
	loaded[name] = struct{}{}
	r.plugins = append(r.plugins, p)
	logging.Debug(logCtx, "loaded lua plugin",
		slog.String("plugin", name), slog.String("source", string(source)), slog.Int("hooks", len(p.callbacks)))
}

// Add appends an already-loaded plugin to the registry. Used by tests and by
// callers that load plugins directly.
func (r *Registry) Add(p *LoadedPlugin) {
	r.plugins = append(r.plugins, p)
}

// Plugins returns the loaded plugins in load order.
func (r *Registry) Plugins() []*LoadedPlugin {
	return r.plugins
}

// Len returns the number of loaded plugins.
func (r *Registry) Len() int {
	return len(r.plugins)
}

// PluginNames returns the sorted names of loaded plugins.
func (r *Registry) PluginNames() []string {
	names := make([]string, 0, len(r.plugins))
	for _, p := range r.plugins {
		names = append(names, p.Manifest.Name)
	}
	sort.Strings(names)
	return names
}

// FireObserver dispatches an observer hook to every subscribed plugin. Observer
// callbacks run for side effects only: a failing or slow callback is logged and
// ignored, never propagated to the host. Each callback runs under its own
// timeout so one plugin cannot stall the hook.
func (r *Registry) FireObserver(ctx context.Context, hook string, payload map[string]any) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.plugins) == 0 {
		return
	}
	logCtx := logging.WithComponent(ctx, "plugins")
	for _, p := range r.plugins {
		for _, cb := range p.callbacks[hook] {
			r.invokeObserver(ctx, logCtx, p, hook, cb, payload)
		}
	}
}

// invokeObserver runs a single observer callback with a bounded context and
// panic isolation.
func (r *Registry) invokeObserver(ctx, logCtx context.Context, p *LoadedPlugin, hook string, cb *lua.LFunction, payload map[string]any) {
	cctx, cancel := context.WithTimeout(ctx, hookTimeout())
	defer cancel()

	p.dispatchCtx = cctx
	p.L.SetContext(cctx)
	defer p.L.SetContext(context.Background())

	defer func() {
		if rec := recover(); rec != nil {
			logging.Warn(logCtx, "observer hook panicked",
				slog.String("plugin", p.Manifest.Name), slog.String("hook", hook))
		}
	}()

	arg := toLuaTable(p.L, payload)
	if err := p.L.CallByParam(lua.P{Fn: cb, NRet: 0, Protect: true}, arg); err != nil {
		logging.Warn(logCtx, "observer hook callback failed",
			slog.String("plugin", p.Manifest.Name), slog.String("hook", hook), slog.String("error", err.Error()))
	}
}

// Close releases every plugin's Lua state.
func (r *Registry) Close() {
	for _, p := range r.plugins {
		p.Close()
	}
	r.plugins = nil
}
