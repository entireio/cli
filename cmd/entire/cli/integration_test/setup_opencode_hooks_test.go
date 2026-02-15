//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSetupOpencodeHooks_InstallsPluginFile verifies that
// `entire enable --agent opencode` writes the plugin file to .opencode/plugins/entire.ts.
func TestSetupOpencodeHooks_InstallsPluginFile(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire("manual-commit")

	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	// Run entire enable --agent opencode
	output, err := env.RunCLIWithError("enable", "--agent", "opencode")
	if err != nil {
		t.Fatalf("enable opencode failed: %v\nOutput: %s", err, output)
	}

	// Verify plugin file exists
	pluginPath := filepath.Join(env.RepoDir, ".opencode", "plugins", "entire.ts")
	info, err := os.Stat(pluginPath)
	if err != nil {
		t.Fatalf("plugin file should exist at %s: %v", pluginPath, err)
	}
	if info.Size() == 0 {
		t.Error("plugin file should not be empty")
	}

	// Verify content contains expected markers
	data, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("failed to read plugin file: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "hooks") || !strings.Contains(content, "opencode") {
		t.Error("plugin file should contain hook command references")
	}
	if !strings.Contains(content, "ENTIRE_BIN") {
		t.Error("plugin must support ENTIRE_BIN env var for binary resolution")
	}
	if !strings.Contains(content, "execFileSync") {
		t.Error("plugin must use execFileSync for safe binary execution")
	}
	if !strings.Contains(content, "session-start") {
		t.Error("plugin file should reference session-start hook")
	}
	if !strings.Contains(content, "session.created") || !strings.Contains(content, "session.idle") {
		t.Error("plugin file should subscribe to OpenCode session events")
	}

	// Verify plugin follows OpenCode's plugin API contract
	if !strings.Contains(content, "export const EntirePlugin") {
		t.Error("plugin must use named export (not default export)")
	}
	if strings.Contains(content, "$.on(") {
		t.Error("plugin must not use $.on() — $ is Bun shell, not an event emitter")
	}
	if !strings.Contains(content, "event.type ===") {
		t.Error("plugin must dispatch on event.type in the event handler")
	}
	if !strings.Contains(content, "client.session.messages") {
		t.Error("plugin must use SDK client to export transcripts")
	}
}

// TestSetupOpencodeHooks_Idempotent verifies that running enable twice doesn't error
// and doesn't duplicate the plugin file.
func TestSetupOpencodeHooks_Idempotent(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire("manual-commit")

	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	// First enable
	output, err := env.RunCLIWithError("enable", "--agent", "opencode")
	if err != nil {
		t.Fatalf("first enable failed: %v\nOutput: %s", err, output)
	}

	pluginPath := filepath.Join(env.RepoDir, ".opencode", "plugins", "entire.ts")
	firstContent, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("failed to read plugin after first enable: %v", err)
	}

	// Second enable (should be idempotent — file already exists)
	output, err = env.RunCLIWithError("enable", "--agent", "opencode")
	if err != nil {
		t.Fatalf("second enable failed: %v\nOutput: %s", err, output)
	}

	secondContent, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("failed to read plugin after second enable: %v", err)
	}

	if string(firstContent) != string(secondContent) {
		t.Error("plugin content should be identical after second enable")
	}
}

// TestSetupOpencodeHooks_ForceReinstall verifies that --force overwrites the plugin file.
func TestSetupOpencodeHooks_ForceReinstall(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire("manual-commit")

	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	// First enable
	output, err := env.RunCLIWithError("enable", "--agent", "opencode")
	if err != nil {
		t.Fatalf("first enable failed: %v\nOutput: %s", err, output)
	}

	// Tamper with the plugin file
	pluginPath := filepath.Join(env.RepoDir, ".opencode", "plugins", "entire.ts")
	if err := os.WriteFile(pluginPath, []byte("// tampered content"), 0o600); err != nil {
		t.Fatalf("failed to tamper plugin file: %v", err)
	}

	// Re-enable with --force
	output, err = env.RunCLIWithError("enable", "--agent", "opencode", "--force")
	if err != nil {
		t.Fatalf("force enable failed: %v\nOutput: %s", err, output)
	}

	// Verify content is restored (not the tampered content)
	data, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("failed to read plugin after force enable: %v", err)
	}

	content := string(data)
	if strings.Contains(content, "tampered") {
		t.Error("force enable should overwrite tampered content")
	}
	if !strings.Contains(content, "execFileSync") {
		t.Error("force enable should restore correct plugin content")
	}
}

// TestSetupOpencodeHooks_DisableRemovesPlugin verifies that `entire disable --uninstall --force`
// removes the plugin file.
func TestSetupOpencodeHooks_DisableRemovesPlugin(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire("manual-commit")

	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	// Enable opencode
	output, err := env.RunCLIWithError("enable", "--agent", "opencode")
	if err != nil {
		t.Fatalf("enable failed: %v\nOutput: %s", err, output)
	}

	pluginPath := filepath.Join(env.RepoDir, ".opencode", "plugins", "entire.ts")
	if _, err := os.Stat(pluginPath); err != nil {
		t.Fatalf("plugin should exist after enable: %v", err)
	}

	// Uninstall (removes all agent hooks including OpenCode)
	output, err = env.RunCLIWithError("disable", "--uninstall", "--force")
	if err != nil {
		t.Fatalf("uninstall failed: %v\nOutput: %s", err, output)
	}

	// Verify plugin is removed
	if _, err := os.Stat(pluginPath); !os.IsNotExist(err) {
		t.Errorf("plugin file should be removed after uninstall, got err: %v", err)
	}
}
