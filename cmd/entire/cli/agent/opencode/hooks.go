package opencode

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Ensure OpenCodeAgent implements HookSupport and HookHandler
var (
	_ agent.HookSupport = (*OpenCodeAgent)(nil)
	_ agent.HookHandler = (*OpenCodeAgent)(nil)
)

// pluginFileName is the name of the Entire plugin file installed into OpenCode.
const pluginFileName = "entire.ts"

// pluginDir is the directory within .opencode where plugins are stored.
const pluginDir = "plugins"

//go:embed entire.ts
var pluginFS embed.FS

// GetHookNames returns the hook verbs OpenCode supports.
// These become subcommands: entire hooks opencode <verb>
func (o *OpenCodeAgent) GetHookNames() []string {
	return []string{
		HookNameSessionStart,
		HookNameStop,
		HookNameTaskStart,
		HookNameTaskComplete,
	}
}

// InstallHooks installs the Entire plugin into .opencode/plugins/entire.ts.
// OpenCode auto-loads TypeScript plugins from .opencode/plugins/ at startup.
// If force is true, overwrites the existing plugin file.
// Returns the number of hooks installed (1 plugin file = 1).
func (o *OpenCodeAgent) InstallHooks(_ bool, force bool) (int, error) {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		repoRoot, err = os.Getwd() //nolint:forbidigo // Intentional fallback when RepoRoot() fails (tests run outside git repos)
		if err != nil {
			return 0, fmt.Errorf("failed to get current directory: %w", err)
		}
	}

	pluginPath := filepath.Join(repoRoot, ".opencode", pluginDir, pluginFileName)

	// Idempotency: if plugin already exists and force is false, skip
	if !force {
		if _, err := os.Stat(pluginPath); err == nil {
			return 0, nil
		}
	}

	// Read the embedded plugin source
	pluginContent, err := pluginFS.ReadFile(pluginFileName)
	if err != nil {
		return 0, fmt.Errorf("failed to read embedded plugin: %w", err)
	}

	// Create the plugins directory
	if err := os.MkdirAll(filepath.Dir(pluginPath), 0o750); err != nil {
		return 0, fmt.Errorf("failed to create plugins directory: %w", err)
	}

	// Write the plugin file
	if err := os.WriteFile(pluginPath, pluginContent, 0o600); err != nil {
		return 0, fmt.Errorf("failed to write plugin file: %w", err)
	}

	return 1, nil
}

// UninstallHooks removes the Entire plugin from .opencode/plugins/.
func (o *OpenCodeAgent) UninstallHooks() error {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		repoRoot = "."
	}

	pluginPath := filepath.Join(repoRoot, ".opencode", pluginDir, pluginFileName)
	if err := os.Remove(pluginPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove plugin file: %w", err)
	}
	return nil
}

// AreHooksInstalled checks if the Entire plugin file exists.
func (o *OpenCodeAgent) AreHooksInstalled() bool {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		repoRoot = "."
	}

	pluginPath := filepath.Join(repoRoot, ".opencode", pluginDir, pluginFileName)
	_, err = os.Stat(pluginPath)
	return err == nil
}

// GetSupportedHooks returns the hook types OpenCode supports.
func (o *OpenCodeAgent) GetSupportedHooks() []agent.HookType {
	return []agent.HookType{
		agent.HookSessionStart,
		agent.HookStop,
		agent.HookPreToolUse,
		agent.HookPostToolUse,
	}
}
