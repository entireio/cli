package opencode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Compile-time check
var _ agent.HookSupport = (*OpenCodeAgent)(nil)

// Note: Hook tests cannot use t.Parallel() because t.Chdir() modifies process state.

func TestInstallHooks_FreshInstall(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ag := &OpenCodeAgent{}

	count, err := ag.InstallHooks(context.Background(), false, false)
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
	if !strings.Contains(content, `const ENTIRE_CMD = 'entire'`) {
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
	count1, err := ag.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("first install failed: %v", err)
	}
	if count1 != 1 {
		t.Errorf("first install: expected 1, got %d", count1)
	}

	// Second install — should be idempotent
	count2, err := ag.InstallHooks(context.Background(), false, false)
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

	count, err := ag.InstallHooks(context.Background(), true, false)
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
	if !strings.Contains(content, `"$(git rev-parse --show-toplevel)"/scripts/entire-dev`) {
		t.Error("local dev mode: plugin file should delegate to the entire-dev launcher via git rev-parse")
	}
}

func TestInstallHooks_SessionStartIsGuardedBySessionSwitch(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ag := &OpenCodeAgent{}

	if _, err := ag.InstallHooks(context.Background(), false, false); err != nil {
		t.Fatalf("install failed: %v", err)
	}

	pluginPath := filepath.Join(dir, ".opencode", "plugins", "entire.ts")
	data, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("plugin file not created: %v", err)
	}

	content := string(data)
	guard := "if (resetSessionTracking(session.id)) {"
	hook := `const proc = Bun.spawn(hookCmd("session-start"), {`

	guardIdx := strings.Index(content, guard)
	hookIdx := strings.Index(content, hook)

	if guardIdx == -1 {
		t.Fatalf("plugin file missing guard %q", guard)
	}
	if hookIdx == -1 {
		t.Fatalf("plugin file missing session-start hook spawn %q", hook)
	}
	if guardIdx >= hookIdx {
		t.Fatalf("expected guarded session-start call after guard, got guard=%d hook=%d",
			guardIdx, hookIdx)
	}
	if !strings.Contains(content, `if ! command -v entire >/dev/null 2>&1; then exit 0; fi; exec entire hooks opencode ${hookName}`) {
		t.Fatal("plugin file missing silent production hook command")
	}
}

func TestInstallHooks_TurnStartUsesSyncHook(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ag := &OpenCodeAgent{}

	if _, err := ag.InstallHooks(context.Background(), false, false); err != nil {
		t.Fatalf("install failed: %v", err)
	}

	pluginPath := filepath.Join(dir, ".opencode", "plugins", "entire.ts")
	data, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("plugin file not created: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, `callHookSync("turn-start", {`) {
		t.Fatal("plugin file should dispatch turn-start via callHookSync")
	}
	if strings.Contains(content, `await callHook("turn-start", {`) {
		t.Fatal("plugin file should not dispatch turn-start via async callHook")
	}
}

func TestInstallHooks_MessageUpdatedFallsBackToSessionStart(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ag := &OpenCodeAgent{}

	if _, err := ag.InstallHooks(context.Background(), false, false); err != nil {
		t.Fatalf("install failed: %v", err)
	}

	pluginPath := filepath.Join(dir, ".opencode", "plugins", "entire.ts")
	data, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("plugin file not created: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, `if (msg.sessionID && resetSessionTracking(msg.sessionID)) {`) {
		t.Fatal("plugin file should bootstrap session tracking from message.updated")
	}
	if !strings.Contains(content, `callHookSync("session-start", {`) {
		t.Fatal("plugin file should dispatch fallback session-start via callHookSync")
	}
	if !strings.Contains(content, `session_id: msg.sessionID,`) {
		t.Fatal("plugin file should pass msg.sessionID in fallback session-start")
	}
}

func TestInstallHooks_MessageUpdatedFallsBackToTurnStart(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ag := &OpenCodeAgent{}

	if _, err := ag.InstallHooks(context.Background(), false, false); err != nil {
		t.Fatalf("install failed: %v", err)
	}

	pluginPath := filepath.Join(dir, ".opencode", "plugins", "entire.ts")
	data, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("plugin file not created: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, `if (msg.role === "user" && !seenUserMessages.has(msg.id)) {`) {
		t.Fatal("plugin file should use message.updated as a fallback turn-start source")
	}
	if !strings.Contains(content, `prompt: "",`) {
		t.Fatal("plugin file should send an empty prompt for fallback turn-start")
	}
}

func TestInstallHooks_ForceReinstall(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ag := &OpenCodeAgent{}

	// First install
	if _, err := ag.InstallHooks(context.Background(), false, false); err != nil {
		t.Fatalf("first install failed: %v", err)
	}

	// Force reinstall
	count, err := ag.InstallHooks(context.Background(), false, true)
	if err != nil {
		t.Fatalf("force install failed: %v", err)
	}
	if count != 1 {
		t.Errorf("force install: expected 1, got %d", count)
	}
}

func TestInstallHooks_RewritesWhenContentDiffers(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ag := &OpenCodeAgent{}

	// Install with localDev=true
	count, err := ag.InstallHooks(context.Background(), true, false)
	if err != nil {
		t.Fatalf("first install failed: %v", err)
	}
	if count != 1 {
		t.Errorf("first install: expected 1, got %d", count)
	}

	pluginPath := filepath.Join(dir, ".opencode", "plugins", "entire.ts")
	before, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("failed to read plugin file: %v", err)
	}
	if !strings.Contains(string(before), "scripts/entire-dev") {
		t.Fatal("expected localDev content to delegate to scripts/entire-dev")
	}

	// Reinstall with localDev=false (content differs) — should rewrite
	count, err = ag.InstallHooks(context.Background(), false, false)
	if err != nil {
		t.Fatalf("second install failed: %v", err)
	}
	if count != 1 {
		t.Errorf("second install with different content: expected 1, got %d", count)
	}

	after, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("failed to read plugin file after rewrite: %v", err)
	}
	if strings.Contains(string(after), "scripts/entire-dev") {
		t.Error("expected production content after rewrite, but still references scripts/entire-dev")
	}
	if !strings.Contains(string(after), `const ENTIRE_CMD = 'entire'`) {
		t.Error("expected production command constant after rewrite")
	}
}

func TestUninstallHooks(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ag := &OpenCodeAgent{}

	if _, err := ag.InstallHooks(context.Background(), false, false); err != nil {
		t.Fatalf("install failed: %v", err)
	}

	if err := ag.UninstallHooks(context.Background()); err != nil {
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
	if err := ag.UninstallHooks(context.Background()); err != nil {
		t.Fatalf("uninstall with no file should not error: %v", err)
	}
}

func TestAreHooksInstalled(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ag := &OpenCodeAgent{}

	if ag.AreHooksInstalled(context.Background()) {
		t.Error("hooks should not be installed initially")
	}

	if _, err := ag.InstallHooks(context.Background(), false, false); err != nil {
		t.Fatalf("install failed: %v", err)
	}

	if !ag.AreHooksInstalled(context.Background()) {
		t.Error("hooks should be installed after InstallHooks")
	}

	if err := ag.UninstallHooks(context.Background()); err != nil {
		t.Fatalf("uninstall failed: %v", err)
	}

	if ag.AreHooksInstalled(context.Background()) {
		t.Error("hooks should not be installed after UninstallHooks")
	}
}
