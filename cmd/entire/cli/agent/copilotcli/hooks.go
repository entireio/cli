package copilotcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// HooksFileName is the hooks file managed by Entire for Copilot CLI.
const HooksFileName = "entire.json"

// hooksDir is the directory within the repo where Copilot CLI looks for hook configs.
const hooksDir = ".github/hooks"

// entireHookPrefixes are command prefixes that identify Entire hooks in the bash field.
var entireHookPrefixes = []string{
	"entire ",
	`go run "$(git rev-parse --show-toplevel)"/cmd/entire/main.go `,
}

// hookConfigKey maps our kebab-case hook names to camelCase JSON keys.
var hookConfigKey = map[string]string{
	HookNameUserPromptSubmitted: "userPromptSubmitted",
	HookNameSessionStart:        "sessionStart",
	HookNameAgentStop:           "agentStop",
	HookNameSessionEnd:          "sessionEnd",
	HookNameSubagentStop:        "subagentStop",
	HookNamePreToolUse:          "preToolUse",
	HookNamePostToolUse:         "postToolUse",
	HookNameErrorOccurred:       "errorOccurred",
}

// InstallHooks installs Copilot CLI hooks in .github/hooks/entire.json.
// If force is true, removes existing Entire hooks before installing.
// Returns the number of hooks installed.
// Unknown top-level fields and hook types are preserved on round-trip.
func (c *CopilotCLIAgent) InstallHooks(ctx context.Context, localDev bool, force bool) (int, error) {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		worktreeRoot = "."
	}

	hooksPath := filepath.Join(worktreeRoot, hooksDir, HooksFileName)

	// Use raw maps to preserve unknown fields on round-trip
	var rawFile map[string]json.RawMessage
	var rawHooks map[string]json.RawMessage

	existingData, readErr := os.ReadFile(hooksPath) //nolint:gosec // path is constructed from repo root + fixed path
	switch {
	case readErr == nil:
		if err := json.Unmarshal(existingData, &rawFile); err != nil {
			return 0, fmt.Errorf("failed to parse existing %s: %w", HooksFileName, err)
		}
		if hooksRaw, ok := rawFile["hooks"]; ok {
			if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
				return 0, fmt.Errorf("failed to parse hooks in %s: %w", HooksFileName, err)
			}
		}
		if _, ok := rawFile["version"]; !ok {
			rawFile["version"] = json.RawMessage(`1`)
		}
	case errors.Is(readErr, os.ErrNotExist):
		rawFile = map[string]json.RawMessage{
			"version": json.RawMessage(`1`),
		}
	default:
		return 0, fmt.Errorf("failed to read %s: %w", HooksFileName, readErr)
	}

	if rawHooks == nil {
		rawHooks = make(map[string]json.RawMessage)
	}

	// Parse existing entries for each hook type we manage
	hookEntries := make(map[string][]CopilotHookEntry)
	for _, hookName := range c.HookNames() {
		key := hookConfigKey[hookName]
		var entries []CopilotHookEntry
		if err := parseCopilotHookType(rawHooks, key, &entries); err != nil {
			return 0, fmt.Errorf("failed to parse %s hooks: %w", key, err)
		}
		hookEntries[hookName] = entries
	}

	// If force, remove existing Entire hooks first
	if force {
		for hookName, entries := range hookEntries {
			hookEntries[hookName] = removeEntireHooks(entries)
		}
	}

	// Define command prefix
	var cmdPrefix string
	if localDev {
		cmdPrefix = `go run "$(git rev-parse --show-toplevel)"/cmd/entire/main.go hooks copilot-cli `
	} else {
		cmdPrefix = "entire hooks copilot-cli "
	}

	count := 0

	// Add hooks that don't already exist
	for _, hookName := range c.HookNames() {
		cmd := cmdPrefix + hookName
		if !localDev {
			cmd = agent.WrapProductionSilentHookCommand(cmd)
		}
		entries := hookEntries[hookName]
		if !hookBashExists(entries, cmd) {
			entries = append(entries, CopilotHookEntry{
				Type:    "command",
				Bash:    cmd,
				Comment: "Entire CLI",
			})
			hookEntries[hookName] = entries
			count++
		}
	}

	if count == 0 {
		return 0, nil
	}

	// Marshal modified hook types back into rawHooks
	for _, hookName := range c.HookNames() {
		key := hookConfigKey[hookName]
		if err := marshalCopilotHookType(rawHooks, key, hookEntries[hookName]); err != nil {
			return 0, fmt.Errorf("failed to marshal %s hooks: %w", key, err)
		}
	}

	// Marshal hooks and update raw file
	hooksJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawHooks)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal hooks: %w", err)
	}
	rawFile["hooks"] = hooksJSON

	// Write to file
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o750); err != nil {
		return 0, fmt.Errorf("failed to create %s directory: %w", hooksDir, err)
	}

	output, err := jsonutil.MarshalIndentWithNewline(rawFile, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("failed to marshal %s: %w", HooksFileName, err)
	}

	if err := os.WriteFile(hooksPath, output, 0o600); err != nil {
		return 0, fmt.Errorf("failed to write %s: %w", HooksFileName, err)
	}

	return count, nil
}

// UninstallHooks removes Entire hooks from Copilot CLI's entire.json.
// Unknown top-level fields and hook types are preserved on round-trip.
func (c *CopilotCLIAgent) UninstallHooks(ctx context.Context) error {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		worktreeRoot = "."
	}
	hooksPath := filepath.Join(worktreeRoot, hooksDir, HooksFileName)
	data, err := os.ReadFile(hooksPath) //nolint:gosec // path is constructed from repo root + fixed path
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // No hooks file means nothing to uninstall
		}
		return fmt.Errorf("failed to read %s: %w", HooksFileName, err)
	}

	var rawFile map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawFile); err != nil {
		return fmt.Errorf("failed to parse %s: %w", HooksFileName, err)
	}

	var rawHooks map[string]json.RawMessage
	if hooksRaw, ok := rawFile["hooks"]; ok {
		if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
			return fmt.Errorf("failed to parse hooks in %s: %w", HooksFileName, err)
		}
	}
	if rawHooks == nil {
		rawHooks = make(map[string]json.RawMessage)
	}

	// Parse and remove Entire hooks from each hook type we manage
	for _, hookName := range c.HookNames() {
		key := hookConfigKey[hookName]
		var entries []CopilotHookEntry
		if err := parseCopilotHookType(rawHooks, key, &entries); err != nil {
			return fmt.Errorf("failed to parse %s hooks: %w", key, err)
		}
		entries = removeEntireHooks(entries)
		if err := marshalCopilotHookType(rawHooks, key, entries); err != nil {
			return fmt.Errorf("failed to marshal %s hooks: %w", key, err)
		}
	}

	// Marshal hooks back (preserving unknown hook types)
	if len(rawHooks) > 0 {
		hooksJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawHooks)
		if err != nil {
			return fmt.Errorf("failed to marshal hooks: %w", err)
		}
		rawFile["hooks"] = hooksJSON
	} else {
		delete(rawFile, "hooks")
	}

	// Write back
	output, err := jsonutil.MarshalIndentWithNewline(rawFile, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal %s: %w", HooksFileName, err)
	}

	if err := os.WriteFile(hooksPath, output, 0o600); err != nil {
		return fmt.Errorf("failed to write %s: %w", HooksFileName, err)
	}
	return nil
}

// AreHooksInstalled checks if Entire hooks are installed in the Copilot CLI config.
func (c *CopilotCLIAgent) AreHooksInstalled(ctx context.Context) bool {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		worktreeRoot = "."
	}
	hooksPath := filepath.Join(worktreeRoot, hooksDir, HooksFileName)
	data, err := os.ReadFile(hooksPath) //nolint:gosec // path is constructed from repo root + fixed path
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			logging.Warn(ctx, "copilot-cli: failed to read hooks file", "path", hooksPath, "err", err)
		}
		return false
	}

	var hooksFile CopilotHooksFile
	if err := json.Unmarshal(data, &hooksFile); err != nil {
		logging.Warn(ctx, "copilot-cli: failed to parse hooks file", "path", hooksPath, "err", err)
		return false
	}

	return hasEntireHook(hooksFile.Hooks.UserPromptSubmitted) ||
		hasEntireHook(hooksFile.Hooks.SessionStart) ||
		hasEntireHook(hooksFile.Hooks.AgentStop) ||
		hasEntireHook(hooksFile.Hooks.SessionEnd) ||
		hasEntireHook(hooksFile.Hooks.SubagentStop) ||
		hasEntireHook(hooksFile.Hooks.PreToolUse) ||
		hasEntireHook(hooksFile.Hooks.PostToolUse) ||
		hasEntireHook(hooksFile.Hooks.ErrorOccurred)
}

// GetSupportedHooks returns the normalized lifecycle events this agent supports.
// Note: HookNames() returns 8 hooks but GetSupportedHooks() returns only 6.
// The two not listed here are:
//   - subagentStop: handled by ParseHookEvent (returns SubagentEnd), but there is no
//     HookType constant for subagent events (they use EventType instead).
//   - errorOccurred: pass-through hook with no lifecycle action (ParseHookEvent returns nil).
func (c *CopilotCLIAgent) GetSupportedHooks() []agent.HookType {
	return []agent.HookType{
		agent.HookSessionStart,
		agent.HookSessionEnd,
		agent.HookUserPromptSubmit,
		agent.HookStop,
		agent.HookPreToolUse,
		agent.HookPostToolUse,
	}
}

// parseCopilotHookType parses a specific hook type from rawHooks into the target slice.
func parseCopilotHookType(rawHooks map[string]json.RawMessage, hookType string, target *[]CopilotHookEntry) error {
	if data, ok := rawHooks[hookType]; ok {
		if err := json.Unmarshal(data, target); err != nil {
			return fmt.Errorf("invalid JSON for hook type %s: %w", hookType, err)
		}
	}
	return nil
}

// marshalCopilotHookType marshals a hook type back into rawHooks.
// If the slice is empty, removes the key from rawHooks.
func marshalCopilotHookType(rawHooks map[string]json.RawMessage, hookType string, entries []CopilotHookEntry) error {
	if len(entries) == 0 {
		delete(rawHooks, hookType)
		return nil
	}
	data, err := jsonutil.MarshalWithNoHTMLEscape(entries)
	if err != nil {
		return fmt.Errorf("failed to marshal hook type %s: %w", hookType, err)
	}
	rawHooks[hookType] = data
	return nil
}

// hookBashExists checks if a hook with the given bash command already exists.
func hookBashExists(entries []CopilotHookEntry, bash string) bool {
	for _, entry := range entries {
		if entry.Bash == bash {
			return true
		}
	}
	return false
}

// isEntireHook checks if a hook entry's bash command belongs to Entire.
func isEntireHook(bash string) bool {
	return agent.IsManagedHookCommand(bash, entireHookPrefixes)
}

// hasEntireHook checks if any entry in the slice is an Entire hook.
func hasEntireHook(entries []CopilotHookEntry) bool {
	for _, entry := range entries {
		if isEntireHook(entry.Bash) {
			return true
		}
	}
	return false
}

// removeEntireHooks removes all Entire hooks from the slice.
func removeEntireHooks(entries []CopilotHookEntry) []CopilotHookEntry {
	result := make([]CopilotHookEntry, 0, len(entries))
	for _, entry := range entries {
		if !isEntireHook(entry.Bash) {
			result = append(result, entry)
		}
	}
	return result
}
