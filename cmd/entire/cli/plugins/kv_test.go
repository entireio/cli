package plugins

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// writePluginDir creates an arbitrary plugin dir (not under the managed tree)
// with the given manifest and main.lua, returning its path.
func writePluginDir(t *testing.T, manifest, mainLua string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "plug")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFileName), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, DefaultEntry), []byte(mainLua), 0o600); err != nil {
		t.Fatalf("write main.lua: %v", err)
	}
	return dir
}

func TestKV_RoundTripAndPersist(t *testing.T) {
	parent := t.TempDir()
	t.Setenv("ENTIRE_PLUGIN_DIR", parent)

	dir := writePluginDir(t,
		`{"name":"counter","hooks":["turn_end"]}`,
		`
entire.on("turn_end", function()
  local prev = entire.kv.get("count")
  local n = tonumber(prev or "0") + 1
  entire.kv.set("count", tostring(n))
  _G.last = entire.kv.get("count")
end)
`)
	p, err := LoadPlugin(context.Background(), dir, SourceUser, "", settings.PluginSettings{Enabled: true})
	if err != nil {
		t.Fatalf("LoadPlugin() error = %v", err)
	}
	reg := &Registry{}
	reg.Add(p)
	defer reg.Close()

	reg.FireObserver(context.Background(), HookTurnEnd, nil)
	reg.FireObserver(context.Background(), HookTurnEnd, nil)

	if got := p.L.GetGlobal("last").String(); got != "2" {
		t.Errorf("kv count = %q, want 2", got)
	}

	// Persisted to the per-plugin data dir under the managed parent.
	kvPath := filepath.Join(parent, "data", "counter", "kv.json")
	data, err := os.ReadFile(kvPath)
	if err != nil {
		t.Fatalf("read kv file: %v", err)
	}
	if !strings.Contains(string(data), `"count": "2"`) {
		t.Errorf("kv.json missing count=2: %s", data)
	}
}

func TestKV_SurvivesReload(t *testing.T) {
	parent := t.TempDir()
	t.Setenv("ENTIRE_PLUGIN_DIR", parent)

	main := `
entire.on("turn_end", function()
  local prev = entire.kv.get("v")
  if prev == nil then entire.kv.set("v", "first") else _G.seen = prev end
end)
`
	dir := writePluginDir(t, `{"name":"persist","hooks":["turn_end"]}`, main)

	p1, err := LoadPlugin(context.Background(), dir, SourceUser, "", settings.PluginSettings{Enabled: true})
	if err != nil {
		t.Fatalf("LoadPlugin() error = %v", err)
	}
	reg1 := &Registry{}
	reg1.Add(p1)
	reg1.FireObserver(context.Background(), HookTurnEnd, nil) // sets v=first
	reg1.Close()

	// Fresh load must read the persisted value.
	p2, err := LoadPlugin(context.Background(), dir, SourceUser, "", settings.PluginSettings{Enabled: true})
	if err != nil {
		t.Fatalf("LoadPlugin() reload error = %v", err)
	}
	reg2 := &Registry{}
	reg2.Add(p2)
	defer reg2.Close()
	reg2.FireObserver(context.Background(), HookTurnEnd, nil) // reads v

	if got := p2.L.GetGlobal("seen").String(); got != "first" {
		t.Errorf("reloaded kv value = %q, want first", got)
	}
}
