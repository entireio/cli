package plugins

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/settings"
	lua "github.com/yuin/gopher-lua"
)

// Source identifies where a plugin was discovered.
type Source string

const (
	// SourceUser is the per-user managed Lua plugins dir (installed plugins).
	SourceUser Source = "user"
	// SourceRepo is the repo-local .entire/plugins dir. Repo-local plugins
	// never auto-run without an explicit allow-list entry.
	SourceRepo Source = "repo"
)

// LoadedPlugin is a plugin whose entry script has run and whose hook
// subscriptions have been registered. Its Lua state is long-lived: callbacks
// captured at load are invoked on each matching hook. LStates are NOT
// goroutine-safe; the owning Registry serializes all access.
type LoadedPlugin struct {
	Manifest Manifest
	Dir      string
	Source   Source
	Grant    settings.PluginSettings

	L         *lua.LState
	callbacks map[string][]*lua.LFunction

	// dispatchCtx is the context of the in-flight load or hook dispatch, used
	// only for logging correlation. It is set under the Registry mutex before
	// each callback runs, so no additional synchronization is needed.
	dispatchCtx context.Context //nolint:containedctx // set per-dispatch under the registry mutex; carries logging correlation only
}

// LoadManifestFromDir reads and validates the plugin.json in dir without
// executing any Lua. Used by discovery and `entire plugin list` to inspect a
// plugin cheaply.
func LoadManifestFromDir(dir string) (*Manifest, error) {
	manifestPath := filepath.Join(dir, ManifestFileName)
	data, err := os.ReadFile(manifestPath) //nolint:gosec // dir is a resolved plugin dir (managed store or repo .entire/plugins); reading its manifest is the point
	if err != nil {
		return nil, fmt.Errorf("read plugin manifest %s: %w", manifestPath, err)
	}
	return ParseManifest(data)
}

// LoadPlugin builds a sandboxed Lua state for the plugin in dir, installs the
// entire API, and runs the entry script once so it can register hooks and
// commands. The caller owns the returned plugin's Lua state and must Close it
// (via the Registry) when done.
//
// grant is the plugin's allow-list entry; callers must have already confirmed
// the plugin is enabled. ctx bounds the entry-script execution.
func LoadPlugin(ctx context.Context, dir string, source Source, grant settings.PluginSettings) (*LoadedPlugin, error) {
	manifest, err := LoadManifestFromDir(dir)
	if err != nil {
		return nil, err
	}

	L := NewSandboxedState(context.Background())
	p := &LoadedPlugin{
		Manifest:    *manifest,
		Dir:         dir,
		Source:      source,
		Grant:       grant,
		L:           L,
		callbacks:   make(map[string][]*lua.LFunction),
		dispatchCtx: ctx,
	}
	p.installAPI(L)

	entryPath := filepath.Join(dir, manifest.EntryFile())
	if _, statErr := os.Stat(entryPath); statErr != nil {
		L.Close()
		return nil, fmt.Errorf("plugin %q: entry script: %w", manifest.Name, statErr)
	}

	loadCtx, cancel := context.WithTimeout(ctx, loadTimeout)
	defer cancel()
	L.SetContext(loadCtx)
	if err := L.DoFile(entryPath); err != nil {
		L.Close()
		return nil, fmt.Errorf("plugin %q: run entry %s: %w", manifest.Name, manifest.EntryFile(), err)
	}
	// Clear the load deadline; each hook dispatch installs its own.
	L.SetContext(context.Background())

	return p, nil
}

// Close releases the plugin's Lua state. Safe to call once.
func (p *LoadedPlugin) Close() {
	if p.L != nil {
		p.L.Close()
		p.L = nil
	}
}

// HasHook reports whether the plugin registered any callback for the hook.
func (p *LoadedPlugin) HasHook(hook string) bool {
	return len(p.callbacks[hook]) > 0
}
