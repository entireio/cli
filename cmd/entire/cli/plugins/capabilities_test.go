package plugins

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/settings"
	lua "github.com/yuin/gopher-lua"
)

// luaFalse is the string form of a Lua false boolean, as returned by LValue.String().
const luaFalse = "false"

// loadAndFire loads a plugin from an inline main.lua with the given grant, fires
// turn_end once, and returns the plugin for global inspection.
func loadAndFire(t *testing.T, mainLua string, grant settings.PluginSettings, worktreeRoot string) *LoadedPlugin {
	t.Helper()
	t.Setenv("ENTIRE_PLUGIN_DIR", t.TempDir())
	dir := writePluginDir(t, `{"name":"cap","hooks":["turn_end"]}`, mainLua)
	p, err := LoadPlugin(context.Background(), dir, SourceUser, worktreeRoot, grant)
	if err != nil {
		t.Fatalf("LoadPlugin() error = %v", err)
	}
	reg := &Registry{}
	reg.Add(p)
	t.Cleanup(reg.Close)
	reg.FireObserver(context.Background(), HookTurnEnd, nil)
	return p
}

func TestCapability_DeniedWithoutGrant(t *testing.T) {
	const probe = `
entire.on("turn_end", function()
  local ok, err = pcall(function() entire.exec.run("echo", "hi") end)
  _G.ok = ok
  _G.err = tostring(err)
end)
`
	p := loadAndFire(t, probe, settings.PluginSettings{Enabled: true}, "")
	if p.L.GetGlobal("ok").String() != luaFalse {
		t.Error("expected exec.run to be denied without the exec grant")
	}
	if errMsg := p.L.GetGlobal("err").String(); !strings.Contains(errMsg, "capability") {
		t.Errorf("expected capability-denied error, got %q", errMsg)
	}
}

func TestCapability_ExecRunsWhenGranted(t *testing.T) {
	if runtime.GOOS == windowsGOOS {
		t.Skip("echo semantics differ on Windows")
	}
	const probe = `
entire.on("turn_end", function()
  local r = entire.exec.run("echo", "hi")
  _G.out = r.stdout
  _G.code = r.code
end)
`
	grant := settings.PluginSettings{Enabled: true, Capabilities: []string{settings.PluginCapabilityExec}}
	p := loadAndFire(t, probe, grant, "")
	if got := strings.TrimSpace(p.L.GetGlobal("out").String()); got != "hi" {
		t.Errorf("exec stdout = %q, want hi", got)
	}
	if got := lua.LVAsNumber(p.L.GetGlobal("code")); got != 0 {
		t.Errorf("exec code = %v, want 0", got)
	}
}

func TestCapability_FSReadWriteConfined(t *testing.T) {
	root := t.TempDir()
	const probe = `
entire.on("turn_end", function()
  entire.fs.write("sub/x.txt", "hello")
  _G.content = entire.fs.read("sub/x.txt")
  local ok = pcall(function() entire.fs.read("../escape.txt") end)
  _G.escape_ok = ok
end)
`
	grant := settings.PluginSettings{Enabled: true, Capabilities: []string{settings.PluginCapabilityFS}}
	p := loadAndFire(t, probe, grant, root)

	if got := p.L.GetGlobal("content").String(); got != "hello" {
		t.Errorf("fs read = %q, want hello", got)
	}
	if got := filepath.Join(root, "sub", "x.txt"); !fileHasContent(t, got, "hello") {
		t.Errorf("expected file written under repo root at %s", got)
	}
	if p.L.GetGlobal("escape_ok").String() != luaFalse {
		t.Error("expected traversal outside repo root to be rejected")
	}
}

func TestCapability_HTTPGetWhenGranted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		fmt.Fprint(w, "brewing")
	}))
	defer srv.Close()

	probe := fmt.Sprintf(`
entire.on("turn_end", function()
  local r = entire.http.get(%q)
  _G.status = r.status
  _G.body = r.body
  local ok = pcall(function() entire.http.get("file:///etc/passwd") end)
  _G.scheme_ok = ok
end)
`, srv.URL)

	grant := settings.PluginSettings{Enabled: true, Capabilities: []string{settings.PluginCapabilityHTTP}}
	p := loadAndFire(t, probe, grant, "")

	if got := lua.LVAsNumber(p.L.GetGlobal("status")); got != http.StatusTeapot {
		t.Errorf("http status = %v, want 418", got)
	}
	if got := p.L.GetGlobal("body").String(); got != "brewing" {
		t.Errorf("http body = %q", got)
	}
	if p.L.GetGlobal("scheme_ok").String() != luaFalse {
		t.Error("expected non-http(s) scheme to be rejected")
	}
}

func TestCapability_NetConnectGatedNotImplemented(t *testing.T) {
	const probe = `
entire.on("turn_end", function()
  local ok, err = pcall(function() entire.net.connect("host", 80) end)
  _G.ok = ok
  _G.err = tostring(err)
end)
`
	grant := settings.PluginSettings{Enabled: true, Capabilities: []string{settings.PluginCapabilityNet}}
	p := loadAndFire(t, probe, grant, "")
	if p.L.GetGlobal("ok").String() != luaFalse {
		t.Error("expected net.connect to error")
	}
	if errMsg := p.L.GetGlobal("err").String(); !strings.Contains(errMsg, "not implemented") {
		t.Errorf("expected not-implemented error, got %q", errMsg)
	}
}

func TestReadOnlyAccessors(t *testing.T) {
	t.Setenv("ENTIRE_PLUGIN_DIR", t.TempDir())
	main := `
_G.name = entire.plugin_name
_G.src = entire.source
_G.root = entire.repo_root
`
	dir := writePluginDir(t, `{"name":"accessors","version":"2.3.4"}`, main)
	p, err := LoadPlugin(context.Background(), dir, SourceRepo, "/tmp/repo", settings.PluginSettings{Enabled: true})
	if err != nil {
		t.Fatalf("LoadPlugin() error = %v", err)
	}
	defer p.Close()

	if got := p.L.GetGlobal("name").String(); got != "accessors" {
		t.Errorf("plugin_name = %q", got)
	}
	if got := p.L.GetGlobal("src").String(); got != "repo" {
		t.Errorf("source = %q", got)
	}
	if got := p.L.GetGlobal("root").String(); got != "/tmp/repo" {
		t.Errorf("repo_root = %q", got)
	}
}

func TestDiscover_KillSwitchDisablesAll(t *testing.T) {
	parent := t.TempDir()
	t.Setenv("ENTIRE_PLUGIN_DIR", parent)
	t.Setenv(pluginsDisabledEnv, "1")
	writeUserPlugin(t, parent, "notify", `{"name":"notify"}`, turnEndCounter)

	s := &settings.EntireSettings{Plugins: map[string]settings.PluginSettings{"notify": {Enabled: true}}}
	reg := Discover(context.Background(), "", s, nil)
	defer reg.Close()
	if reg.Len() != 0 {
		t.Fatalf("kill switch should disable all plugins, got %d", reg.Len())
	}
}

func fileHasContent(t *testing.T, path, want string) bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return string(data) == want
}
