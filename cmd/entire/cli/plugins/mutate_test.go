package plugins

import (
	"context"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// loadMut loads a plugin from inline main.lua with the given grant (no fire).
func loadMut(t *testing.T, name, manifest, mainLua string, grant settings.PluginSettings) *LoadedPlugin {
	t.Helper()
	t.Setenv("ENTIRE_PLUGIN_DIR", t.TempDir())
	dir := writePluginDir(t, manifest, mainLua)
	p, err := LoadPlugin(context.Background(), dir, SourceUser, "", grant)
	if err != nil {
		t.Fatalf("LoadPlugin(%s) error = %v", name, err)
	}
	return p
}

func TestFireCommitMsg_TrailerFromCapablePlugin(t *testing.T) {
	p := loadMut(t, "ct",
		`{"name":"ct","hooks":["prepare_commit_msg"],"capabilities":["commit_msg"]}`,
		`entire.on("prepare_commit_msg", function(ev) return "Plugin-Trailer: " .. ev.source end)`,
		settings.PluginSettings{Enabled: true, Capabilities: []string{settings.PluginCapabilityCommitMsg}})
	reg := &Registry{}
	reg.Add(p)
	defer reg.Close()

	got := reg.FireCommitMsg(context.Background(), map[string]any{"source": "message"})
	if len(got) != 1 || got[0] != "Plugin-Trailer: message" {
		t.Fatalf("FireCommitMsg = %v, want [Plugin-Trailer: message]", got)
	}
}

func TestFireCommitMsg_SkipsUncapablePlugin(t *testing.T) {
	p := loadMut(t, "ct",
		`{"name":"ct","hooks":["prepare_commit_msg"]}`,
		`entire.on("prepare_commit_msg", function() return "X: y" end)`,
		settings.PluginSettings{Enabled: true}) // no commit_msg cap
	reg := &Registry{}
	reg.Add(p)
	defer reg.Close()

	if got := reg.FireCommitMsg(context.Background(), nil); len(got) != 0 {
		t.Fatalf("expected no trailers without capability, got %v", got)
	}
}

func TestFireCommitMsg_MultiPluginLoadOrder(t *testing.T) {
	p1 := loadMut(t, "a",
		`{"name":"a","hooks":["prepare_commit_msg"],"capabilities":["commit_msg"]}`,
		`entire.on("prepare_commit_msg", function() return "A: 1" end)`,
		settings.PluginSettings{Enabled: true, Capabilities: []string{settings.PluginCapabilityCommitMsg}})
	p2 := loadMut(t, "b",
		`{"name":"b","hooks":["prepare_commit_msg"],"capabilities":["commit_msg"]}`,
		`entire.on("prepare_commit_msg", function() return "B: 2" end)`,
		settings.PluginSettings{Enabled: true, Capabilities: []string{settings.PluginCapabilityCommitMsg}})
	reg := &Registry{}
	reg.Add(p1)
	reg.Add(p2)
	defer reg.Close()

	got := reg.FireCommitMsg(context.Background(), nil)
	if len(got) != 2 || got[0] != "A: 1" || got[1] != "B: 2" {
		t.Fatalf("FireCommitMsg order = %v, want [A: 1, B: 2]", got)
	}
}

func TestFirePrePush_VetoWithCapability(t *testing.T) {
	p := loadMut(t, "veto",
		`{"name":"veto","hooks":["pre_push"],"capabilities":["pre_push"]}`,
		`entire.on("pre_push", function(ev) return false, "remote is protected" end)`,
		settings.PluginSettings{Enabled: true, Capabilities: []string{settings.PluginCapabilityPrePush}})
	reg := &Registry{}
	reg.Add(p)
	defer reg.Close()

	err := reg.FirePrePush(context.Background(), map[string]any{"remote": "origin"})
	if err == nil || !strings.Contains(err.Error(), "remote is protected") {
		t.Fatalf("expected veto error, got %v", err)
	}
}

func TestFirePrePush_NoVetoWithoutCapability(t *testing.T) {
	// Callback returns false but the plugin lacks the pre_push capability, so
	// the return is ignored (observer only) and the push proceeds.
	p := loadMut(t, "obs",
		`{"name":"obs","hooks":["pre_push"]}`,
		`
_G.ran = false
entire.on("pre_push", function() _G.ran = true; return false end)
`,
		settings.PluginSettings{Enabled: true})
	reg := &Registry{}
	reg.Add(p)
	defer reg.Close()

	if err := reg.FirePrePush(context.Background(), nil); err != nil {
		t.Fatalf("expected no veto without capability, got %v", err)
	}
	if p.L.GetGlobal("ran").String() != "true" {
		t.Error("expected observer callback to still run without the capability")
	}
}

func TestFirePrePush_AllowWhenTrue(t *testing.T) {
	p := loadMut(t, "ok",
		`{"name":"ok","hooks":["pre_push"],"capabilities":["pre_push"]}`,
		`entire.on("pre_push", function() return true end)`,
		settings.PluginSettings{Enabled: true, Capabilities: []string{settings.PluginCapabilityPrePush}})
	reg := &Registry{}
	reg.Add(p)
	defer reg.Close()

	if err := reg.FirePrePush(context.Background(), nil); err != nil {
		t.Fatalf("expected push allowed when callback returns true, got %v", err)
	}
}
