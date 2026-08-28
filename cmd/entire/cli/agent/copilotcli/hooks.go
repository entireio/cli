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
)

// HooksFileName is the hooks file managed by Entire for Copilot CLI.
const HooksFileName = "entire.json"

// hooksDir is the directory within the repo where Copilot CLI looks for hook configs.
const hooksDir = ".github/hooks"

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
func (c *CopilotCLIAgent) InstallHooks(ctx context.Context, force bool) (int, error) {
	hooksPath := copilotHooksPath(ctx)

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
	const cmdPrefix = "entire hooks copilot-cli "

	count := 0

	// Sync each hook to its desired command. Entire-owned entries carrying any
	// other command are dropped first, even without --force: a hook written by
	// an older version would otherwise survive alongside the one added below and
	// keep firing, which for the removed local-dev mode means a script inside
	// the working tree still runs on every agent turn.
	staleDropped := false
	for _, hookName := range c.HookNames() {
		cmd := agent.WrapProductionSilentHookCommand(cmdPrefix + hookName)
		entries := hookEntries[hookName]

		// Keep the matching entry rather than remove-and-re-add: entry-level
		// fields (cwd, timeoutSec, env) live on the existing entry and a freshly
		// constructed one would discard them.
		kept, dropped := agent.DropStaleManagedHooks(entries, hookEntryBash, []string{cmd})
		if dropped {
			staleDropped = true
		}
		entries = kept

		if !hookBashExists(entries, cmd) {
			entries = append(entries, CopilotHookEntry{
				Type:    "command",
				Bash:    cmd,
				Comment: "Entire CLI",
			})
			count++
		}
		hookEntries[hookName] = entries
	}

	// staleDropped forces a write even when nothing was added: a file holding
	// both a stale and a current hook adds nothing, and returning early here
	// would leave the stale hook on disk.
	if count == 0 && !staleDropped {
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

// copilotHooksPath resolves .github/hooks/entire.json for the current repo, so
// install, detection and removal all look at the same file.
func copilotHooksPath(ctx context.Context) string {
	return agent.HookConfigPath(ctx, hooksDir, HooksFileName)
}

// UninstallHooks removes Entire hooks from Copilot CLI's entire.json.
// Unknown top-level fields and hook types are preserved on round-trip.
func (c *CopilotCLIAgent) UninstallHooks(ctx context.Context) error {
	if err := agent.RemoveHookArtifacts(copilotHooksPath(ctx), c.removeEntireArtifacts); err != nil {
		return fmt.Errorf("remove Copilot CLI hooks: %w", err)
	}
	return nil
}

// AreHooksInstalled reports whether Entire owns anything in Copilot CLI's hooks
// file, by asking whether UninstallHooks would strip anything. Both answers come
// from removeEntireArtifacts and so from one walk of hookConfigKey — detection
// used to re-enumerate those keys by hand as struct fields, which is how a
// detection set drifts away from the removal set it is supposed to mirror.
func (c *CopilotCLIAgent) AreHooksInstalled(ctx context.Context) bool {
	return agent.HookArtifactsInstalled(ctx, string(c.Name()), copilotHooksPath(ctx), c.removeEntireArtifacts)
}

// removeEntireArtifacts implements agent.HookArtifactRemoval for Copilot CLI's
// hooks file: Entire's entries under every key in hookConfigKey. Unknown hook
// types and unknown top-level fields round-trip untouched.
func (c *CopilotCLIAgent) removeEntireArtifacts(data []byte) ([]byte, bool, error) {
	var rawFile map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawFile); err != nil {
		return nil, false, fmt.Errorf("failed to parse %s: %w", HooksFileName, err)
	}

	var rawHooks map[string]json.RawMessage
	if hooksRaw, ok := rawFile["hooks"]; ok {
		if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
			return nil, false, fmt.Errorf("failed to parse hooks in %s: %w", HooksFileName, err)
		}
	}

	changed := false
	for _, hookName := range c.HookNames() {
		key := hookConfigKey[hookName]
		var entries []CopilotHookEntry
		if err := parseCopilotHookType(rawHooks, key, &entries); err != nil {
			return nil, false, fmt.Errorf("failed to parse %s hooks: %w", key, err)
		}
		if !hasEntireHook(entries) {
			continue
		}
		changed = true
		if err := marshalCopilotHookType(rawHooks, key, removeEntireHooks(entries)); err != nil {
			return nil, false, fmt.Errorf("failed to marshal %s hooks: %w", key, err)
		}
	}

	// Nothing of Entire's was here. Report that rather than rewriting a file we
	// have no artifacts in: removal sweeps every agent, including ones the user
	// never enabled.
	if !changed {
		return nil, false, nil
	}

	if len(rawHooks) > 0 {
		hooksJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawHooks)
		if err != nil {
			return nil, false, fmt.Errorf("failed to marshal hooks: %w", err)
		}
		rawFile["hooks"] = hooksJSON
	} else {
		delete(rawFile, "hooks")
	}

	// This file is Entire's own: InstallHooks creates entire.json and writes the
	// format-version key into it. Once the hooks are gone and only that key is
	// left, the file is a leftover of ours, so remove it rather than leave a stub
	// behind. A bare version key is never an artifact on its own — it is not
	// attributable to Entire, so it cannot make the file read as an installation.
	if _, versioned := rawFile["version"]; versioned && len(rawFile) == 1 {
		return nil, true, nil
	}

	output, err := jsonutil.MarshalIndentWithNewline(rawFile, "", "  ")
	if err != nil {
		return nil, false, fmt.Errorf("failed to marshal %s: %w", HooksFileName, err)
	}
	return output, true, nil
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
// hookEntryBash reads the command off a hook entry for the shared helpers.
// Copilot CLI stores it under `bash`, not `command`.
func hookEntryBash(e CopilotHookEntry) string { return e.Bash }

func isEntireHook(bash string) bool {
	return agent.IsManagedHookCommand(bash)
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
