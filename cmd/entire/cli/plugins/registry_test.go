package plugins

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/settings"
	lua "github.com/yuin/gopher-lua"
)

// writeUserPlugin creates <pluginParent>/lua/<name>/{plugin.json,main.lua}.
func writeUserPlugin(t *testing.T, parent, name, manifest, mainLua string) {
	t.Helper()
	dir := filepath.Join(parent, "lua", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir plugin dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFileName), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, DefaultEntry), []byte(mainLua), 0o600); err != nil {
		t.Fatalf("write main.lua: %v", err)
	}
}

// writeRepoPlugin creates <root>/.entire/plugins/<name>/{plugin.json,main.lua}.
func writeRepoPlugin(t *testing.T, root, name, manifest, mainLua string) {
	t.Helper()
	dir := filepath.Join(RepoLuaPluginsDir(root), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir repo plugin dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFileName), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, DefaultEntry), []byte(mainLua), 0o600); err != nil {
		t.Fatalf("write main.lua: %v", err)
	}
}

const turnEndCounter = `
_G.fired = 0
entire.on("turn_end", function(ev)
  _G.fired = _G.fired + 1
  _G.last_session = ev.session_id
end)
`

func TestDiscover_LoadsEnabledUserPlugin_AndFires(t *testing.T) {
	parent := t.TempDir()
	t.Setenv("ENTIRE_PLUGIN_DIR", parent)
	writeUserPlugin(t, parent, "notify",
		`{"name":"notify","hooks":["turn_end"]}`, turnEndCounter)

	// A user-global plugin enabled via the merged team settings (localGrants nil)
	// runs, mirroring the external_agents posture — team settings.json may
	// enable a user-installed plugin.
	s := &settings.EntireSettings{
		Plugins: map[string]settings.PluginSettings{
			"notify": {Enabled: true},
		},
	}
	reg := Discover(context.Background(), "", s, nil)
	defer reg.Close()

	if reg.Len() != 1 {
		t.Fatalf("expected 1 loaded plugin, got %d (%v)", reg.Len(), reg.PluginNames())
	}
	p := reg.Plugins()[0]
	if !p.HasHook(HookTurnEnd) {
		t.Fatal("plugin did not register turn_end hook")
	}

	reg.FireObserver(context.Background(), HookTurnEnd, map[string]any{"session_id": "abc123"})

	if got := lua.LVAsNumber(p.L.GetGlobal("fired")); got != 1 {
		t.Errorf("fired = %v, want 1", got)
	}
	if got := p.L.GetGlobal("last_session").String(); got != "abc123" {
		t.Errorf("last_session = %q, want abc123", got)
	}
}

func TestDiscover_SkipsNotAllowlisted(t *testing.T) {
	parent := t.TempDir()
	t.Setenv("ENTIRE_PLUGIN_DIR", parent)
	writeUserPlugin(t, parent, "notify", `{"name":"notify"}`, turnEndCounter)

	// Empty allow-list: nothing runs.
	reg := Discover(context.Background(), "", &settings.EntireSettings{}, nil)
	defer reg.Close()
	if reg.Len() != 0 {
		t.Fatalf("expected 0 plugins for empty allow-list, got %d", reg.Len())
	}
}

func TestDiscover_SkipsDisabled(t *testing.T) {
	parent := t.TempDir()
	t.Setenv("ENTIRE_PLUGIN_DIR", parent)
	writeUserPlugin(t, parent, "notify", `{"name":"notify"}`, turnEndCounter)

	s := &settings.EntireSettings{
		Plugins: map[string]settings.PluginSettings{"notify": {Enabled: false}},
	}
	reg := Discover(context.Background(), "", s, nil)
	defer reg.Close()
	if reg.Len() != 0 {
		t.Fatalf("expected 0 plugins for disabled entry, got %d", reg.Len())
	}
}

func TestDiscover_RepoLocalNeverAutoRunsWithoutAllowlist(t *testing.T) {
	parent := t.TempDir()
	t.Setenv("ENTIRE_PLUGIN_DIR", parent)
	root := t.TempDir()
	writeRepoPlugin(t, root, "repoplug", `{"name":"repoplug"}`, turnEndCounter)

	// No allow-list entry anywhere: the repo-local plugin must not load.
	reg := Discover(context.Background(), root, &settings.EntireSettings{}, nil)
	defer reg.Close()
	if reg.Len() != 0 {
		t.Fatalf("repo-local plugin auto-ran without allow-list: got %d", reg.Len())
	}
}

func TestDiscover_RepoLocalIgnoresTeamSettingsEnable(t *testing.T) {
	parent := t.TempDir()
	t.Setenv("ENTIRE_PLUGIN_DIR", parent)
	root := t.TempDir()
	writeRepoPlugin(t, root, "repoplug", `{"name":"repoplug"}`, turnEndCounter)

	// Enabled via committed team settings (merged EntireSettings) but NOT in the
	// per-developer local settings: a repo-local plugin must stay inert. This is
	// the trust boundary — cloning a hostile repo that ships a plugin plus a team
	// settings enable cannot execute the plugin's code.
	teamEnabled := &settings.EntireSettings{
		Plugins: map[string]settings.PluginSettings{"repoplug": {Enabled: true}},
	}
	reg := Discover(context.Background(), root, teamEnabled, nil)
	defer reg.Close()
	if reg.Len() != 0 {
		t.Fatalf("repo-local plugin ran from team settings alone: got %d", reg.Len())
	}
}

func TestDiscover_RepoLocalRunsWhenEnabledInLocalSettings(t *testing.T) {
	parent := t.TempDir()
	t.Setenv("ENTIRE_PLUGIN_DIR", parent)
	root := t.TempDir()
	writeRepoPlugin(t, root, "repoplug", `{"name":"repoplug"}`, turnEndCounter)

	// The local developer opted in via .entire/settings.local.json. The merged
	// settings therefore also show it enabled, and localGrants carries the same
	// entry — the repo-local plugin may now run.
	merged := &settings.EntireSettings{
		Plugins: map[string]settings.PluginSettings{"repoplug": {Enabled: true}},
	}
	localGrants := map[string]settings.PluginSettings{"repoplug": {Enabled: true}}
	reg := Discover(context.Background(), root, merged, localGrants)
	defer reg.Close()
	if reg.Len() != 1 {
		t.Fatalf("expected repo-local plugin to load when enabled in local settings, got %d", reg.Len())
	}
	if got := reg.Plugins()[0].Source; got != SourceRepo {
		t.Errorf("expected repo source, got %q", got)
	}
}

func TestDiscover_UserWinsNameCollision(t *testing.T) {
	parent := t.TempDir()
	t.Setenv("ENTIRE_PLUGIN_DIR", parent)
	root := t.TempDir()

	writeUserPlugin(t, parent, "dup", `{"name":"dup"}`, `_G.origin = "user"`+"\n"+turnEndCounter)
	writeRepoPlugin(t, root, "dup", `{"name":"dup"}`, `_G.origin = "repo"`+"\n"+turnEndCounter)

	s := &settings.EntireSettings{
		Plugins: map[string]settings.PluginSettings{"dup": {Enabled: true}},
	}
	// Enable the repo copy locally too, so if precedence somehow flipped the
	// repo-local plugin would be eligible; the user copy must still win.
	localGrants := map[string]settings.PluginSettings{"dup": {Enabled: true}}
	reg := Discover(context.Background(), root, s, localGrants)
	defer reg.Close()

	if reg.Len() != 1 {
		t.Fatalf("expected 1 plugin after de-dup, got %d", reg.Len())
	}
	p := reg.Plugins()[0]
	if p.Source != SourceUser {
		t.Errorf("expected user plugin to win collision, got source %q", p.Source)
	}
	if got := p.L.GetGlobal("origin").String(); got != "user" {
		t.Errorf("origin = %q, want user", got)
	}
}

func TestFireObserver_ErrorIsNonFatal(t *testing.T) {
	parent := t.TempDir()
	t.Setenv("ENTIRE_PLUGIN_DIR", parent)
	writeUserPlugin(t, parent, "boom",
		`{"name":"boom","hooks":["turn_end"]}`,
		`entire.on("turn_end", function() error("boom") end)`)

	s := &settings.EntireSettings{
		Plugins: map[string]settings.PluginSettings{"boom": {Enabled: true}},
	}
	reg := Discover(context.Background(), "", s, nil)
	defer reg.Close()

	// Must not panic or propagate; the error is swallowed and logged.
	reg.FireObserver(context.Background(), HookTurnEnd, map[string]any{})
}

func TestFireObserver_PerHookTimeout(t *testing.T) {
	L := NewSandboxedState(context.Background())
	fn := L.NewFunction(func(l *lua.LState) int {
		_ = l.DoString(`while true do end`) //nolint:errcheck // intentional runaway; the per-hook context timeout aborts it
		return 0
	})
	p := &LoadedPlugin{
		Manifest:  Manifest{Name: "slow"},
		Source:    SourceUser,
		L:         L,
		callbacks: map[string][]*lua.LFunction{HookTurnEnd: {fn}},
	}
	reg := &Registry{}
	reg.Add(p)
	defer reg.Close()

	start := time.Now()
	reg.FireObserver(context.Background(), HookTurnEnd, map[string]any{})
	if elapsed := time.Since(start); elapsed > observerHookTimeout+3*time.Second {
		t.Fatalf("observer timeout not enforced: %v", elapsed)
	}
}

func TestFireObserver_LatencyGate(t *testing.T) {
	L := NewSandboxedState(context.Background())
	fn := L.NewFunction(func(*lua.LState) int { return 0 })
	p := &LoadedPlugin{
		Manifest:  Manifest{Name: "noop"},
		Source:    SourceUser,
		L:         L,
		callbacks: map[string][]*lua.LFunction{HookTurnEnd: {fn}},
	}
	reg := &Registry{}
	reg.Add(p)
	defer reg.Close()

	ctx := context.Background()
	payload := map[string]any{"session_id": "s1", "model": "test"}
	const n = 1000
	start := time.Now()
	for range n {
		reg.FireObserver(ctx, HookTurnEnd, payload)
	}
	per := time.Since(start) / n
	t.Logf("empty observer hook dispatch: %v/op", per)
	if per > time.Millisecond {
		t.Errorf("empty hook dispatch too slow: %v/op (gate: <1ms)", per)
	}
}
