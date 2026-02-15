package opencode

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Compile-time interface assertions
var (
	_ agent.HookSupport = (*OpenCodeAgent)(nil)
	_ agent.HookHandler = (*OpenCodeAgent)(nil)
)

func TestInstallHooks_FreshInstall(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &OpenCodeAgent{}
	count, err := ag.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	if count != 1 {
		t.Errorf("InstallHooks() count = %d, want 1", count)
	}

	// Verify plugin file was created
	pluginPath := filepath.Join(tempDir, ".opencode", "plugins", "entire.ts")
	if _, err := os.Stat(pluginPath); os.IsNotExist(err) {
		t.Error("plugin file was not created")
	}

	// Verify content is not empty
	data, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("failed to read plugin file: %v", err)
	}
	if len(data) == 0 {
		t.Error("plugin file is empty")
	}
}

func TestInstallHooks_Idempotent(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &OpenCodeAgent{}

	// First install
	count1, err := ag.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("first InstallHooks() error = %v", err)
	}
	if count1 != 1 {
		t.Errorf("first InstallHooks() count = %d, want 1", count1)
	}

	// Second install should skip (file exists)
	count2, err := ag.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("second InstallHooks() error = %v", err)
	}
	if count2 != 0 {
		t.Errorf("second InstallHooks() count = %d, want 0 (idempotent)", count2)
	}
}

func TestInstallHooks_Force(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &OpenCodeAgent{}

	// First install
	_, err := ag.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("first InstallHooks() error = %v", err)
	}

	// Force reinstall should replace
	count, err := ag.InstallHooks(false, true)
	if err != nil {
		t.Fatalf("force InstallHooks() error = %v", err)
	}
	if count != 1 {
		t.Errorf("force InstallHooks() count = %d, want 1", count)
	}
}

func TestUninstallHooks(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &OpenCodeAgent{}

	// First install
	_, err := ag.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	// Verify hooks are installed
	if !ag.AreHooksInstalled() {
		t.Error("hooks should be installed before uninstall")
	}

	// Uninstall
	err = ag.UninstallHooks()
	if err != nil {
		t.Fatalf("UninstallHooks() error = %v", err)
	}

	// Verify hooks are removed
	if ag.AreHooksInstalled() {
		t.Error("hooks should not be installed after uninstall")
	}
}

func TestUninstallHooks_NoPluginFile(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &OpenCodeAgent{}

	// Should not error when no plugin file exists
	err := ag.UninstallHooks()
	if err != nil {
		t.Fatalf("UninstallHooks() should not error when no plugin file: %v", err)
	}
}

func TestAreHooksInstalled(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &OpenCodeAgent{}

	// Should be false when no plugin file
	if ag.AreHooksInstalled() {
		t.Error("AreHooksInstalled() should be false when no plugin file")
	}

	// Install hooks
	_, err := ag.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	// Should be true after installation
	if !ag.AreHooksInstalled() {
		t.Error("AreHooksInstalled() should be true after installation")
	}
}

func TestGetHookNames(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	names := ag.GetHookNames()

	expected := []string{
		HookNameSessionStart,
		HookNameStop,
		HookNameTaskStart,
		HookNameTaskComplete,
	}

	if len(names) != len(expected) {
		t.Fatalf("GetHookNames() returned %d names, want %d", len(names), len(expected))
	}

	for i, name := range expected {
		if names[i] != name {
			t.Errorf("GetHookNames()[%d] = %q, want %q", i, names[i], name)
		}
	}
}

func TestGetSupportedHooks(t *testing.T) {
	t.Parallel()
	ag := &OpenCodeAgent{}
	hooks := ag.GetSupportedHooks()

	if len(hooks) != 4 {
		t.Errorf("GetSupportedHooks() returned %d hooks, want 4", len(hooks))
	}

	expected := []agent.HookType{
		agent.HookSessionStart,
		agent.HookStop,
		agent.HookPreToolUse,
		agent.HookPostToolUse,
	}

	for i, hook := range expected {
		if hooks[i] != hook {
			t.Errorf("GetSupportedHooks()[%d] = %q, want %q", i, hooks[i], hook)
		}
	}
}

func TestInstallHooks_PluginContent(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &OpenCodeAgent{}
	_, err := ag.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	// Read the installed plugin
	pluginPath := filepath.Join(tempDir, ".opencode", "plugins", "entire.ts")
	data, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("failed to read plugin file: %v", err)
	}

	content := string(data)

	// Verify key content from the embedded plugin
	mustContain := []string{
		"hooks", "opencode",
		"session.created",
		"session.idle",
		"tool.execute.before",
		"tool.execute.after",
		"ENTIRE_BIN",
		"execFileSync",
	}

	for _, check := range mustContain {
		if !contains(content, check) {
			t.Errorf("plugin file missing expected content: %q", check)
		}
	}
}

func TestInstallHooks_PluginAPIContract(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	ag := &OpenCodeAgent{}
	_, err := ag.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	pluginPath := filepath.Join(tempDir, ".opencode", "plugins", "entire.ts")
	data, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("failed to read plugin file: %v", err)
	}

	content := string(data)

	// Structural assertions — plugin must follow OpenCode's plugin API
	structural := []struct {
		check string
		desc  string
	}{
		{"export const EntirePlugin", "must use named export (not default export)"},
		{"async (", "must be async function"},
		{"return {", "must return hooks object"},
		{"event.type ===", "must dispatch on event.type (not $.on)"},
		{"event.properties.", "must access event.properties (OpenCode event shape)"},
		{"client.session.messages", "must use SDK to export transcript"},
		{"client.app", "must use SDK structured logging (client.app.log)"},
		{"resolveBinary", "must resolve binary path at load time"},
		{"execFileSync", "must use execFileSync (not execSync with string)"},
	}
	for _, s := range structural {
		if !contains(content, s.check) {
			t.Errorf("plugin API contract: %s (missing %q)", s.desc, s.check)
		}
	}

	// Negative assertions — must NOT use patterns from broken implementations
	forbidden := []struct {
		check string
		desc  string
	}{
		{"$.on(", "must not use $.on() — $ is Bun shell, not event emitter"},
		{"export default function", "must not use default function export"},
		{"export default async function", "must not use default async function export"},
	}
	for _, f := range forbidden {
		if contains(content, f.check) {
			t.Errorf("plugin API contract violation: %s (found %q)", f.desc, f.check)
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
