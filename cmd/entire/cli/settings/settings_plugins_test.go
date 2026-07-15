package settings

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePluginTestRepo creates a temp git repo with optional team
// (.entire/settings.json) and local (.entire/settings.local.json) contents and
// chdir's into it. Empty content skips that file.
func writePluginTestRepo(t *testing.T, team, local string) {
	t.Helper()
	dir := t.TempDir()
	entireDir := filepath.Join(dir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("mkdir .entire: %v", err)
	}
	if team != "" {
		if err := os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(team), 0o600); err != nil {
			t.Fatalf("write settings.json: %v", err)
		}
	}
	if local != "" {
		if err := os.WriteFile(filepath.Join(entireDir, "settings.local.json"), []byte(local), 0o600); err != nil {
			t.Fatalf("write settings.local.json: %v", err)
		}
	}
	// paths.AbsPath resolves relative to the git worktree root.
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	t.Chdir(dir)
}

func TestLocalPluginGrants_OnlyReadsLocalFile(t *testing.T) {
	// A repo-local plugin enabled solely in committed team settings must NOT
	// appear in the local grants; only settings.local.json entries do.
	team := `{"plugins": {"teamplug": {"enabled": true}}}`
	local := `{"plugins": {"localplug": {"enabled": true, "capabilities": ["exec"]}}}`
	writePluginTestRepo(t, team, local)

	grants, err := LocalPluginGrants(context.Background())
	if err != nil {
		t.Fatalf("LocalPluginGrants() error = %v", err)
	}
	if _, ok := grants["teamplug"]; ok {
		t.Error("team-settings plugin must not be reported as a local grant")
	}
	g, ok := grants["localplug"]
	if !ok || !g.Enabled {
		t.Fatalf("expected localplug enabled in local grants, got %+v", grants)
	}
	if !g.HasCapability(PluginCapabilityExec) {
		t.Errorf("localplug missing exec capability: %+v", g.Capabilities)
	}
}

func TestLocalPluginGrants_AbsentFileIsEmpty(t *testing.T) {
	writePluginTestRepo(t, `{"plugins": {"teamplug": {"enabled": true}}}`, "")

	grants, err := LocalPluginGrants(context.Background())
	if err != nil {
		t.Fatalf("LocalPluginGrants() error = %v", err)
	}
	if len(grants) != 0 {
		t.Errorf("expected no local grants when settings.local.json absent, got %+v", grants)
	}
}

func TestLocalPluginGrants_RejectsUnknownCapability(t *testing.T) {
	writePluginTestRepo(t, "", `{"plugins": {"bad": {"enabled": true, "capabilities": ["telepathy"]}}}`)

	_, err := LocalPluginGrants(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unknown capability") {
		t.Fatalf("expected unknown capability error, got %v", err)
	}
}

func TestLoadFromBytes_PluginsAllowlist(t *testing.T) {
	t.Parallel()
	data := []byte(`{
		"plugins": {
			"notify": {"enabled": true, "capabilities": ["http", "fs"]},
			"linter": {"enabled": false}
		}
	}`)
	s, err := LoadFromBytes(data)
	if err != nil {
		t.Fatalf("LoadFromBytes() error = %v", err)
	}
	if !s.IsPluginEnabled("notify") {
		t.Error("notify should be enabled")
	}
	if s.IsPluginEnabled("linter") {
		t.Error("linter should be disabled")
	}
	if s.IsPluginEnabled("absent") {
		t.Error("absent plugin should not be enabled")
	}

	grant, ok := s.PluginGrant("notify")
	if !ok {
		t.Fatal("expected notify grant present")
	}
	if !grant.HasCapability(PluginCapabilityHTTP) || !grant.HasCapability(PluginCapabilityFS) {
		t.Errorf("notify missing capabilities: %+v", grant.Capabilities)
	}
	if grant.HasCapability(PluginCapabilityExec) {
		t.Error("notify should not have exec capability")
	}
}

func TestLoadFromBytes_RejectsUnknownCapability(t *testing.T) {
	t.Parallel()
	data := []byte(`{"plugins": {"bad": {"enabled": true, "capabilities": ["telepathy"]}}}`)
	_, err := LoadFromBytes(data)
	if err == nil || !strings.Contains(err.Error(), "unknown capability") {
		t.Fatalf("expected unknown capability error, got %v", err)
	}
}

func TestLoadFromBytes_RejectsUnknownPluginField(t *testing.T) {
	t.Parallel()
	// DisallowUnknownFields must reject a typo'd PluginSettings field.
	data := []byte(`{"plugins": {"p": {"enable": true}}}`)
	_, err := LoadFromBytes(data)
	if err == nil {
		t.Fatal("expected error for unknown plugin field 'enable'")
	}
}

func TestPluginGrant_NilSafe(t *testing.T) {
	t.Parallel()
	var s *EntireSettings
	if _, ok := s.PluginGrant("x"); ok {
		t.Error("nil settings should report no grant")
	}
	if s.IsPluginEnabled("x") {
		t.Error("nil settings should report plugin disabled")
	}
}
