package plugins

import (
	"context"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/settings"
)

func TestRunCommand_ExecutesAndReturnsCode(t *testing.T) {
	t.Setenv("ENTIRE_PLUGIN_DIR", t.TempDir())
	dir := writePluginDir(t,
		`{"name":"greeter","commands":[{"name":"greet","short":"say hi"}]}`,
		`
entire.command{
  name = "greet",
  short = "say hi",
  run = function(args)
    _G.arg1 = args[1]
    _G.argc = #args
    return 7
  end
}
`)
	p, err := LoadPlugin(context.Background(), dir, SourceUser, "", settings.PluginSettings{Enabled: true})
	if err != nil {
		t.Fatalf("LoadPlugin() error = %v", err)
	}
	reg := &Registry{}
	reg.Add(p)
	defer reg.Close()

	code, found := reg.RunCommand(context.Background(), "greet", []string{"world", "again"})
	if !found {
		t.Fatal("expected command to be found")
	}
	if code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
	if got := p.L.GetGlobal("arg1").String(); got != "world" {
		t.Errorf("arg1 = %q, want world", got)
	}
}

func TestRunCommand_NotFound(t *testing.T) {
	reg := &Registry{}
	if _, found := reg.RunCommand(context.Background(), "nope", nil); found {
		t.Fatal("expected not found for unknown command")
	}
}

func TestRunCommand_DefaultsToZeroExit(t *testing.T) {
	t.Setenv("ENTIRE_PLUGIN_DIR", t.TempDir())
	dir := writePluginDir(t, `{"name":"p"}`,
		`entire.command{name="noop", run=function() end}`)
	p, err := LoadPlugin(context.Background(), dir, SourceUser, "", settings.PluginSettings{Enabled: true})
	if err != nil {
		t.Fatalf("LoadPlugin() error = %v", err)
	}
	reg := &Registry{}
	reg.Add(p)
	defer reg.Close()

	code, found := reg.RunCommand(context.Background(), "noop", nil)
	if !found || code != 0 {
		t.Fatalf("RunCommand noop = (%d, %v), want (0, true)", code, found)
	}
}

func TestRunCommand_ErrorReturnsExitOne(t *testing.T) {
	t.Setenv("ENTIRE_PLUGIN_DIR", t.TempDir())
	dir := writePluginDir(t, `{"name":"p"}`,
		`entire.command{name="boom", run=function() error("kaboom") end}`)
	p, err := LoadPlugin(context.Background(), dir, SourceUser, "", settings.PluginSettings{Enabled: true})
	if err != nil {
		t.Fatalf("LoadPlugin() error = %v", err)
	}
	reg := &Registry{}
	reg.Add(p)
	defer reg.Close()

	code, found := reg.RunCommand(context.Background(), "boom", nil)
	if !found || code != 1 {
		t.Fatalf("RunCommand boom = (%d, %v), want (1, true)", code, found)
	}
}

func TestCommands_ListSortedFirstWins(t *testing.T) {
	t.Setenv("ENTIRE_PLUGIN_DIR", t.TempDir())
	dirA := writePluginDir(t, `{"name":"a"}`, `entire.command{name="zeta", run=function() end}
entire.command{name="alpha", run=function() end}`)
	pa, err := LoadPlugin(context.Background(), dirA, SourceUser, "", settings.PluginSettings{Enabled: true})
	if err != nil {
		t.Fatalf("LoadPlugin(a) error = %v", err)
	}
	reg := &Registry{}
	reg.Add(pa)
	defer reg.Close()

	cmds := reg.Commands()
	if len(cmds) != 2 || cmds[0].Name != "alpha" || cmds[1].Name != "zeta" {
		t.Fatalf("Commands() = %+v, want [alpha, zeta] sorted", cmds)
	}
	if info, ok := reg.FindCommand("alpha"); !ok || info.Plugin != "a" {
		t.Errorf("FindCommand(alpha) = %+v, %v", info, ok)
	}
}
