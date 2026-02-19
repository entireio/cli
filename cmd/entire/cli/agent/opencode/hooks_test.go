package opencode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Compile-time check
var _ agent.HookSupport = (*OpenCodeAgent)(nil)

func TestInstallHooks_FreshInstall(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ag := &OpenCodeAgent{}

	count, err := ag.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 hook installed, got %d", count)
	}

	// Verify plugin file was created
	pluginPath := filepath.Join(dir, ".opencode", "plugins", "entire.ts")
	data, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("plugin file not created: %v", err)
	}

	content := string(data)
	// The plugin uses JS template literal ${ENTIRE_CMD} — check the constant was set correctly
	if !strings.Contains(content, `const ENTIRE_CMD = "entire"`) {
		t.Error("plugin file does not contain production command constant")
	}
	if !strings.Contains(content, "hooks opencode") {
		t.Error("plugin file does not contain 'hooks opencode'")
	}
	if !strings.Contains(content, "EntirePlugin") {
		t.Error("plugin file does not contain 'EntirePlugin' export")
	}
	// Should use production command
	if strings.Contains(content, "go run") {
		t.Error("plugin file contains 'go run' in production mode")
	}
}

func TestInstallHooks_Idempotent(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ag := &OpenCodeAgent{}

	// First install
	count1, err := ag.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("first install failed: %v", err)
	}
	if count1 != 1 {
		t.Errorf("first install: expected 1, got %d", count1)
	}

	// Second install — should be idempotent
	count2, err := ag.InstallHooks(false, false)
	if err != nil {
		t.Fatalf("second install failed: %v", err)
	}
	if count2 != 0 {
		t.Errorf("second install: expected 0 (idempotent), got %d", count2)
	}
}

func TestInstallHooks_LocalDev(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ag := &OpenCodeAgent{}

	count, err := ag.InstallHooks(true, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 hook installed, got %d", count)
	}

	pluginPath := filepath.Join(dir, ".opencode", "plugins", "entire.ts")
	data, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("plugin file not created: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "go run") {
		t.Error("local dev mode: plugin file should contain 'go run'")
	}
}

func TestInstallHooks_ForceReinstall(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ag := &OpenCodeAgent{}

	// First install
	if _, err := ag.InstallHooks(false, false); err != nil {
		t.Fatalf("first install failed: %v", err)
	}

	// Force reinstall
	count, err := ag.InstallHooks(false, true)
	if err != nil {
		t.Fatalf("force install failed: %v", err)
	}
	if count != 1 {
		t.Errorf("force install: expected 1, got %d", count)
	}
}

func TestUninstallHooks(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ag := &OpenCodeAgent{}

	if _, err := ag.InstallHooks(false, false); err != nil {
		t.Fatalf("install failed: %v", err)
	}

	if err := ag.UninstallHooks(); err != nil {
		t.Fatalf("uninstall failed: %v", err)
	}

	pluginPath := filepath.Join(dir, ".opencode", "plugins", "entire.ts")
	if _, err := os.Stat(pluginPath); !os.IsNotExist(err) {
		t.Error("plugin file still exists after uninstall")
	}
}

func TestUninstallHooks_NoFile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ag := &OpenCodeAgent{}

	// Should not error when no plugin file exists
	if err := ag.UninstallHooks(); err != nil {
		t.Fatalf("uninstall with no file should not error: %v", err)
	}
}

func TestAreHooksInstalled(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ag := &OpenCodeAgent{}

	if ag.AreHooksInstalled() {
		t.Error("hooks should not be installed initially")
	}

	if _, err := ag.InstallHooks(false, false); err != nil {
		t.Fatalf("install failed: %v", err)
	}

	if !ag.AreHooksInstalled() {
		t.Error("hooks should be installed after InstallHooks")
	}

	if err := ag.UninstallHooks(); err != nil {
		t.Fatalf("uninstall failed: %v", err)
	}

	if ag.AreHooksInstalled() {
		t.Error("hooks should not be installed after UninstallHooks")
	}
}
