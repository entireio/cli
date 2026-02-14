package codex

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Ensure CodexAgent implements HookSupport and HookHandler
var (
	_ agent.HookSupport = (*CodexAgent)(nil)
	_ agent.HookHandler = (*CodexAgent)(nil)
)

// Codex CLI hook names - these become subcommands under `entire hooks codex`
const (
	HookNameAgentTurnComplete = "agent-turn-complete"
)

// CodexConfigFileName is the config file used by Codex CLI.
const CodexConfigFileName = "config.toml"

// entireNotifyCommand is the command Entire installs as the Codex notify handler.
const entireNotifyCommand = `["entire", "hooks", "codex", "agent-turn-complete"]`

// entireNotifyLocalDevPrefix is the argv prefix for the local-dev notify handler.
// The project directory is resolved at write-time since Codex executes notify
// commands as a direct argv array without shell expansion.
var entireNotifyLocalDevPrefix = []string{"go", "run"} //nolint:gochecknoglobals // template for local-dev command construction

// entireNotifyLocalDevSuffix is the binary-relative path and subcommand for local dev.
const entireNotifyLocalDevSuffix = "/cmd/entire/main.go"

// GetHookNames returns the hook verbs Codex CLI supports.
// These become subcommands: entire hooks codex <verb>
func (c *CodexAgent) GetHookNames() []string {
	return []string{
		HookNameAgentTurnComplete,
	}
}

// InstallHooks installs the Codex CLI notify hook in .codex/config.toml.
// Codex uses a TOML config with a `notify` field that specifies the command
// to run on agent-turn-complete events.
// If force is true, removes existing Entire hooks before installing.
// Returns the number of hooks installed.
func (c *CodexAgent) InstallHooks(localDev bool, force bool) (int, error) {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		repoRoot, err = os.Getwd() //nolint:forbidigo // Intentional fallback when RepoRoot() fails (tests run outside git repos)
		if err != nil {
			return 0, fmt.Errorf("failed to get current directory: %w", err)
		}
	}

	configPath := filepath.Join(repoRoot, ".codex", CodexConfigFileName)

	// Read existing config if it exists
	var lines []string
	existingData, readErr := os.ReadFile(configPath) //nolint:gosec // path is constructed from repo root + fixed path
	if readErr == nil {
		lines = strings.Split(string(existingData), "\n")
	}

	// Determine which command to install
	notifyValue := entireNotifyCommand
	if localDev {
		notifyValue = buildLocalDevNotifyCommand()
	}
	notifyLine := "notify = " + notifyValue

	// Check if already installed (idempotency)
	if !force {
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == notifyLine {
				return 0, nil // Already installed
			}
		}
	}

	// Remove existing notify lines:
	// - Always remove Entire notify lines (to update)
	// - When force=true, also remove non-Entire notify lines (to avoid duplicate TOML keys)
	var filteredLines []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "notify") && strings.Contains(trimmed, "=") {
			if isEntireNotifyLine(trimmed) {
				continue // Skip existing Entire notify line
			}
			if force {
				continue // force=true: remove non-Entire notify lines too
			}
		}
		filteredLines = append(filteredLines, line)
	}

	// If there's a non-Entire notify line, don't overwrite it (regardless of whether
	// an Entire line was found/removed — we respect user-configured notify commands).
	if !force {
		for _, line := range filteredLines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "notify") && strings.Contains(trimmed, "=") {
				// There's a user-configured notify line; don't overwrite
				return 0, nil
			}
		}
	}

	// Add the notify line
	filteredLines = appendNotifyLine(filteredLines, notifyLine)

	// Write back to file
	if err := os.MkdirAll(filepath.Dir(configPath), 0o750); err != nil {
		return 0, fmt.Errorf("failed to create .codex directory: %w", err)
	}

	output := strings.Join(filteredLines, "\n")
	// Ensure file ends with a newline
	if !strings.HasSuffix(output, "\n") {
		output += "\n"
	}

	if err := os.WriteFile(configPath, []byte(output), 0o600); err != nil {
		return 0, fmt.Errorf("failed to write config.toml: %w", err)
	}

	return 1, nil
}

// UninstallHooks removes the Entire notify hook from Codex CLI config.
func (c *CodexAgent) UninstallHooks() error {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		repoRoot = "." // Fallback to CWD if not in a git repo
	}
	configPath := filepath.Join(repoRoot, ".codex", CodexConfigFileName)
	data, err := os.ReadFile(configPath) //nolint:gosec // path is constructed from repo root + fixed path
	if err != nil {
		return nil //nolint:nilerr // No config file means nothing to uninstall
	}

	lines := strings.Split(string(data), "\n")
	var filteredLines []string
	changed := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "notify") && strings.Contains(trimmed, "=") && isEntireNotifyLine(trimmed) {
			changed = true
			continue // Remove Entire notify line
		}
		filteredLines = append(filteredLines, line)
	}

	if !changed {
		return nil
	}

	output := strings.Join(filteredLines, "\n")
	if err := os.WriteFile(configPath, []byte(output), 0o600); err != nil {
		return fmt.Errorf("failed to write config.toml: %w", err)
	}
	return nil
}

// AreHooksInstalled checks if the Entire notify hook is installed.
func (c *CodexAgent) AreHooksInstalled() bool {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		repoRoot = "." // Fallback to CWD if not in a git repo
	}
	configPath := filepath.Join(repoRoot, ".codex", CodexConfigFileName)
	data, err := os.ReadFile(configPath) //nolint:gosec // path is constructed from repo root + fixed path
	if err != nil {
		return false
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "notify") && strings.Contains(trimmed, "=") && isEntireNotifyLine(trimmed) {
			return true
		}
	}

	return false
}

// GetSupportedHooks returns the hook types Codex CLI supports.
func (c *CodexAgent) GetSupportedHooks() []agent.HookType {
	return []agent.HookType{
		agent.HookStop, // agent-turn-complete maps to Stop
	}
}

// isEntireNotifyLine checks if a TOML notify line contains an Entire command.
// Uses two detection patterns:
//   - Production: contains "entire" as a standalone TOML array element (quoted)
//   - Local dev: contains "go", "run" prefix with "entire" anywhere (path substring)
func isEntireNotifyLine(line string) bool {
	// Production hook: notify = ["entire", "hooks", "codex", ...]
	// Check for "entire" as a standalone quoted element to avoid false positives
	// like notify = ["my-entire-tool"]
	if strings.Contains(line, `"entire"`) {
		return true
	}
	// Local dev hook: notify = ["go", "run", ".../entire/main.go", ...]
	// The "go", "run" prefix combined with "entire" substring is specific enough
	if strings.Contains(line, `"go", "run"`) && strings.Contains(line, "entire") {
		return true
	}
	return false
}

// buildLocalDevNotifyCommand constructs the local-dev notify command with the
// project directory resolved at write-time. Codex executes notify commands as
// a direct argv array without shell expansion, so env vars like ${CODEX_PROJECT_DIR}
// would not be expanded at runtime.
func buildLocalDevNotifyCommand() string {
	projectDir := os.Getenv("CODEX_PROJECT_DIR")
	if projectDir == "" {
		// Fallback: use repo root (not CWD) to avoid baking in a subdir path
		var err error
		projectDir, err = paths.RepoRoot()
		if err != nil {
			// Last resort: use CWD if not in a git repo (e.g., tests)
			projectDir, err = os.Getwd() //nolint:forbidigo // Intentional: need CWD for local dev path resolution
			if err != nil {
				projectDir = "."
			}
		}
	}
	mainGo := projectDir + entireNotifyLocalDevSuffix
	// Build TOML array: ["go", "run", "/abs/path/cmd/entire/main.go", "hooks", "codex", "agent-turn-complete"]
	parts := make([]string, 0, len(entireNotifyLocalDevPrefix)+4)
	for _, p := range entireNotifyLocalDevPrefix {
		parts = append(parts, `"`+p+`"`)
	}
	parts = append(parts, `"`+mainGo+`"`, `"hooks"`, `"codex"`, `"agent-turn-complete"`)
	return "[" + strings.Join(parts, ", ") + "]"
}

// appendNotifyLine adds a notify line to the config, placing it logically.
// If there's an existing commented-out notify line, places the new one near it.
// Otherwise, appends to the end.
func appendNotifyLine(lines []string, notifyLine string) []string {
	// Look for a commented notify line to place near
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") && strings.Contains(trimmed, "notify") {
			// Insert after the comment
			result := make([]string, 0, len(lines)+1)
			result = append(result, lines[:i+1]...)
			result = append(result, notifyLine)
			result = append(result, lines[i+1:]...)
			return result
		}
	}

	// Append to the end, with a blank line separator if the file isn't empty
	if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
		lines = append(lines, "")
	}
	return append(lines, notifyLine)
}
