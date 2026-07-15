package plugins

import (
	"context"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/settings"
)

const capProbe = `
entire.on("turn_end", function()
  local ok, err = pcall(function() entire.exec.run("echo", "hi") end)
  _G.ok = ok
  _G.err = tostring(err)
end)
`

func TestCapabilityStub_DeniedWithoutGrant(t *testing.T) {
	t.Setenv("ENTIRE_PLUGIN_DIR", t.TempDir())
	dir := writePluginDir(t, `{"name":"cap","hooks":["turn_end"],"capabilities":["exec"]}`, capProbe)

	// Manifest requests exec, but the allow-list grant does NOT include it.
	p, err := LoadPlugin(context.Background(), dir, SourceUser, "", settings.PluginSettings{Enabled: true})
	if err != nil {
		t.Fatalf("LoadPlugin() error = %v", err)
	}
	reg := &Registry{}
	reg.Add(p)
	defer reg.Close()
	reg.FireObserver(context.Background(), HookTurnEnd, nil)

	if ok := p.L.GetGlobal("ok"); ok.String() != "false" {
		t.Errorf("expected exec.run to error without grant, ok=%v", ok)
	}
	if errMsg := p.L.GetGlobal("err").String(); !strings.Contains(errMsg, "capability") {
		t.Errorf("expected capability-denied error, got %q", errMsg)
	}
}

func TestCapabilityStub_GrantedReportsNotImplemented(t *testing.T) {
	t.Setenv("ENTIRE_PLUGIN_DIR", t.TempDir())
	dir := writePluginDir(t, `{"name":"cap","hooks":["turn_end"],"capabilities":["exec"]}`, capProbe)

	grant := settings.PluginSettings{Enabled: true, Capabilities: []string{settings.PluginCapabilityExec}}
	p, err := LoadPlugin(context.Background(), dir, SourceUser, "", grant)
	if err != nil {
		t.Fatalf("LoadPlugin() error = %v", err)
	}
	reg := &Registry{}
	reg.Add(p)
	defer reg.Close()
	reg.FireObserver(context.Background(), HookTurnEnd, nil)

	if errMsg := p.L.GetGlobal("err").String(); !strings.Contains(errMsg, "not implemented") {
		t.Errorf("expected not-implemented error with grant, got %q", errMsg)
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
