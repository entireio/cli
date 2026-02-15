package codexcli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Ensure CodexCLIAgent implements HookSupport and HookHandler
var (
	_ agent.HookSupport = (*CodexCLIAgent)(nil)
	_ agent.HookHandler = (*CodexCLIAgent)(nil)
)

// entireNotifyCmd is the full Entire notify command for Codex turn-complete
const entireNotifyCmd = "entire hooks codex turn-complete"

// entireNotifyPrefix identifies Entire's notify command
const entireNotifyPrefix = "entire hooks codex"

// localDevNotifyPrefix identifies Entire's local dev notify command
const localDevNotifyPrefix = "go run"

// GetHookNames returns the hook verbs Codex supports.
// These become subcommands: entire hooks codex <verb>
func (c *CodexCLIAgent) GetHookNames() []string {
	return []string{
		HookNameTurnComplete,
	}
}

// InstallHooks installs the Codex notify hook in ~/.codex/config.toml.
// Codex's notify config triggers an external command on agent-turn-complete events.
// If force is true, removes existing Entire hooks before installing.
// Returns the number of hooks installed.
func (c *CodexCLIAgent) InstallHooks(localDev bool, force bool) (int, error) {
	configPath := c.GetHookConfigPath()
	if configPath == "" {
		return 0, errors.New("could not determine Codex config path")
	}

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(configPath), 0o750); err != nil {
		return 0, fmt.Errorf("failed to create .codex directory: %w", err)
	}

	// Define the notify command
	var notifyCmd string
	if localDev {
		// Get repo root for local dev builds
		repoRoot, err := paths.RepoRoot()
		if err != nil {
			return 0, fmt.Errorf("failed to get repo root for local dev: %w", err)
		}
		notifyCmd = fmt.Sprintf("go run %s/cmd/entire/main.go hooks codex turn-complete", repoRoot)
	} else {
		notifyCmd = entireNotifyCmd
	}

	// Read existing config
	existingData, err := os.ReadFile(configPath) //nolint:gosec // path is from user's home dir
	if err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("failed to read config.toml: %w", err)
	}

	content := string(existingData)

	// If force, remove existing Entire notify entries
	if force {
		content = removeEntireNotify(content)
	}

	// Check if notify command already exists
	if strings.Contains(content, notifyCmd) {
		return 0, nil // Already installed
	}

	// Check if there's already a notify line
	if hasNotifyConfig(content) {
		// There's an existing notify — we need to check if it's an array or a simple value.
		// Codex config supports notify as an array of strings.
		// For simplicity, we append our command alongside the existing one.
		content = appendToNotify(content, notifyCmd)
	} else {
		// No existing notify — insert at top level (before first [section] header)
		// to avoid placing it inside a TOML section.
		notifyLine := fmt.Sprintf("notify = [\"%s\"]\n", escapeTomlString(notifyCmd))
		content = insertAtTopLevel(content, notifyLine)
	}

	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		return 0, fmt.Errorf("failed to write config.toml: %w", err)
	}

	return 1, nil
}

// UninstallHooks removes Entire hooks from Codex config.
func (c *CodexCLIAgent) UninstallHooks() error {
	configPath := c.GetHookConfigPath()
	if configPath == "" {
		return nil
	}

	data, err := os.ReadFile(configPath) //nolint:gosec // path is from user's home dir
	if err != nil {
		return nil //nolint:nilerr // No config file means nothing to uninstall
	}

	content := removeEntireNotify(string(data))

	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		return fmt.Errorf("failed to write config.toml: %w", err)
	}

	return nil
}

// AreHooksInstalled checks if Entire hooks are installed in Codex config.
func (c *CodexCLIAgent) AreHooksInstalled() bool {
	configPath := c.GetHookConfigPath()
	if configPath == "" {
		return false
	}

	data, err := os.ReadFile(configPath) //nolint:gosec // path is from user's home dir
	if err != nil {
		return false
	}

	content := string(data)
	return strings.Contains(content, entireNotifyPrefix)
}

// GetSupportedHooks returns the hook types Codex supports.
func (c *CodexCLIAgent) GetSupportedHooks() []agent.HookType {
	return []agent.HookType{
		agent.HookStop, // agent-turn-complete maps to stop
	}
}

// hasNotifyConfig checks if there's already a notify line in the config.
func hasNotifyConfig(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "notify") && strings.Contains(trimmed, "=") {
			return true
		}
	}
	return false
}

// appendToNotify adds a command to an existing notify config.
// Handles both array format (notify = ["cmd1", "cmd2"]) and string format (notify = "cmd").
func appendToNotify(content, cmd string) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "notify") || !strings.Contains(trimmed, "=") {
			continue
		}

		// Check if it's array format
		if strings.Contains(trimmed, "[") {
			// Find the closing bracket and insert before it
			closeBracket := strings.LastIndex(trimmed, "]")
			if closeBracket >= 0 {
				before := trimmed[:closeBracket]
				// Check if there are existing entries
				if strings.Contains(before, "\"") {
					lines[i] = before + fmt.Sprintf(", \"%s\"]", escapeTomlString(cmd))
				} else {
					lines[i] = before + fmt.Sprintf("\"%s\"]", escapeTomlString(cmd))
				}
			}
		} else {
			// Simple string format — convert to array
			eqIdx := strings.Index(trimmed, "=")
			if eqIdx >= 0 {
				existingVal := strings.TrimSpace(trimmed[eqIdx+1:])
				existingVal = strings.Trim(existingVal, "\"")
				lines[i] = fmt.Sprintf("notify = [\"%s\", \"%s\"]",
					escapeTomlString(existingVal), escapeTomlString(cmd))
			}
		}
		break
	}
	return strings.Join(lines, "\n")
}

// removeEntireNotify removes Entire-related notify entries from config content.
func removeEntireNotify(content string) string {
	lines := strings.Split(content, "\n")
	var result []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// If it's a notify line, filter out Entire entries
		if strings.HasPrefix(trimmed, "notify") && strings.Contains(trimmed, "=") {
			if strings.Contains(trimmed, entireNotifyPrefix) || strings.Contains(trimmed, localDevNotifyPrefix) {
				// Check if there are other non-Entire entries
				// For simplicity, if the entire notify line is just Entire, remove it
				if !hasNonEntireNotifyEntries(trimmed) {
					continue // Skip the entire line
				}
				// Otherwise, filter out just the Entire entries
				line = filterEntireFromNotifyLine(trimmed)
			}
		}

		result = append(result, line)
	}

	return strings.Join(result, "\n")
}

// hasNonEntireNotifyEntries checks if a notify line has entries other than Entire's.
func hasNonEntireNotifyEntries(line string) bool {
	eqIdx := strings.Index(line, "=")
	if eqIdx < 0 {
		return false
	}
	val := strings.TrimSpace(line[eqIdx+1:])
	val = strings.Trim(val, "[]")

	for _, entry := range strings.Split(val, ",") {
		entry = strings.TrimSpace(entry)
		entry = strings.Trim(entry, "\"")
		if entry != "" && !strings.Contains(entry, entireNotifyPrefix) && !strings.Contains(entry, localDevNotifyPrefix) {
			return true
		}
	}
	return false
}

// filterEntireFromNotifyLine removes Entire entries from a notify array line.
func filterEntireFromNotifyLine(line string) string {
	eqIdx := strings.Index(line, "=")
	if eqIdx < 0 {
		return line
	}

	val := strings.TrimSpace(line[eqIdx+1:])
	isArray := strings.HasPrefix(val, "[")

	if !isArray {
		// Simple string value — if it's Entire, remove the whole line
		unquoted := strings.Trim(val, "\"")
		if strings.Contains(unquoted, entireNotifyPrefix) || strings.Contains(unquoted, localDevNotifyPrefix) {
			return ""
		}
		return line
	}

	// Array format — filter entries
	val = strings.Trim(val, "[]")
	entries := strings.Split(val, ",")
	var kept []string
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		unquoted := strings.Trim(entry, "\"")
		if unquoted != "" && !strings.Contains(unquoted, entireNotifyPrefix) && !strings.Contains(unquoted, localDevNotifyPrefix) {
			kept = append(kept, entry)
		}
	}

	if len(kept) == 0 {
		return ""
	}

	return fmt.Sprintf("notify = [%s]", strings.Join(kept, ", "))
}

// insertAtTopLevel inserts a line into TOML content at the top level,
// before the first [section] header. This ensures the key stays at
// the root scope and doesn't accidentally end up inside a section.
func insertAtTopLevel(content, line string) string {
	lines := strings.Split(content, "\n")
	for i, l := range lines {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "[") && !strings.HasPrefix(trimmed, "[[") {
			// Insert before this section header
			before := strings.Join(lines[:i], "\n")
			if before != "" && !strings.HasSuffix(before, "\n") {
				before += "\n"
			}
			after := strings.Join(lines[i:], "\n")
			return before + line + after
		}
	}
	// No sections found — append at end
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return content + line
}

// escapeTomlString escapes a string for use in TOML.
func escapeTomlString(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	return s
}
