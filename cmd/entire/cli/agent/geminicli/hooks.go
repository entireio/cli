package geminicli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// Ensure GeminiCLIAgent implements HookSupport
var _ agent.HookSupport = (*GeminiCLIAgent)(nil)

// Gemini CLI hook names - these become subcommands under `entire hooks gemini`
const (
	HookNameSessionStart        = "session-start"
	HookNameSessionEnd          = "session-end"
	HookNameBeforeAgent         = "before-agent"
	HookNameAfterAgent          = "after-agent"
	HookNameBeforeModel         = "before-model"
	HookNameAfterModel          = "after-model"
	HookNameBeforeToolSelection = "before-tool-selection"
	HookNameBeforeTool          = "before-tool"
	HookNameAfterTool           = "after-tool"
	HookNamePreCompress         = "pre-compress"
	HookNameNotification        = "notification"
)

// GeminiSettingsFileName is the settings file used by Gemini CLI.
const GeminiSettingsFileName = "settings.json"

// InstallHooks installs Gemini CLI hooks in .gemini/settings.json.
// If force is true, removes existing Entire hooks before installing.
// Returns the number of hooks installed.
func (g *GeminiCLIAgent) InstallHooks(ctx context.Context, force bool) (int, error) {
	settingsPath := geminiSettingsPath(ctx)

	// Read existing settings if they exist
	var rawSettings map[string]json.RawMessage

	// rawHooks preserves unknown hook types
	var rawHooks map[string]json.RawMessage

	var hooksConfig GeminiHooksConfig

	existingData, readErr := os.ReadFile(settingsPath) //nolint:gosec // path is constructed from cwd + fixed path
	if readErr == nil {
		if err := json.Unmarshal(existingData, &rawSettings); err != nil {
			return 0, fmt.Errorf("failed to parse existing settings.json: %w", err)
		}
		if hooksRaw, ok := rawSettings["hooks"]; ok {
			if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
				return 0, fmt.Errorf("failed to parse hooks in settings.json: %w", err)
			}
		}
		if hooksConfigRaw, ok := rawSettings["hooksConfig"]; ok {
			if err := json.Unmarshal(hooksConfigRaw, &hooksConfig); err != nil {
				return 0, fmt.Errorf("failed to parse hooksConfig in settings.json: %w", err)
			}
		}
	} else {
		rawSettings = make(map[string]json.RawMessage)
	}

	if rawHooks == nil {
		rawHooks = make(map[string]json.RawMessage)
	}

	// Strip non-array values from hooks (removes legacy fields like "enabled": true
	// that old Entire versions wrote directly into hooks, which Gemini CLI 0.33+
	// rejects because hooks.additionalProperties requires arrays).
	strippedHookFields := stripNonArrayHookFields(rawHooks)
	for _, key := range strippedHookFields {
		logging.Debug(ctx, "removed non-array field from hooks", slog.String("key", key))
	}
	cleanupDone := len(strippedHookFields) > 0

	// Enable hooks via hooksConfig
	// hooksConfig.Enabled must be true for Gemini CLI to execute hooks
	hooksConfig.Enabled = true

	// Define hook commands up front: the idempotency check below needs the full
	// expected set, not just session-start, to tell "already installed" from
	// "some hook is still on an older command".
	const cmdPrefix = "entire hooks gemini "
	sessionStartCmd := agent.WrapProductionJSONWarningHookCommand(cmdPrefix+"session-start", agent.WarningFormatSingleLine)
	sessionEndCmd := agent.WrapProductionSilentHookCommand(cmdPrefix + "session-end")
	beforeAgentCmd := agent.WrapProductionSilentHookCommand(cmdPrefix + "before-agent")
	afterAgentCmd := agent.WrapProductionSilentHookCommand(cmdPrefix + "after-agent")
	beforeModelCmd := agent.WrapProductionSilentHookCommand(cmdPrefix + "before-model")
	afterModelCmd := agent.WrapProductionSilentHookCommand(cmdPrefix + "after-model")
	beforeToolSelectionCmd := agent.WrapProductionSilentHookCommand(cmdPrefix + "before-tool-selection")
	beforeToolCmd := agent.WrapProductionSilentHookCommand(cmdPrefix + "before-tool")
	afterToolCmd := agent.WrapProductionSilentHookCommand(cmdPrefix + "after-tool")
	preCompressCmd := agent.WrapProductionSilentHookCommand(cmdPrefix + "pre-compress")
	notificationCmd := agent.WrapProductionSilentHookCommand(cmdPrefix + "notification")
	wantCommands := []string{
		sessionStartCmd, sessionEndCmd, beforeAgentCmd, afterAgentCmd,
		beforeModelCmd, afterModelCmd, beforeToolSelectionCmd, beforeToolCmd,
		afterToolCmd, preCompressCmd, notificationCmd,
	}

	// Parse only the hook types we need to modify
	var sessionStart, sessionEnd, beforeAgent, afterAgent []GeminiHookMatcher
	var beforeModel, afterModel, beforeToolSelection []GeminiHookMatcher
	var beforeTool, afterTool, preCompress, notification []GeminiHookMatcher
	for _, parsed := range []struct {
		hookType string
		target   *[]GeminiHookMatcher
	}{
		{"SessionStart", &sessionStart},
		{"SessionEnd", &sessionEnd},
		{"BeforeAgent", &beforeAgent},
		{"AfterAgent", &afterAgent},
		{"BeforeModel", &beforeModel},
		{"AfterModel", &afterModel},
		{"BeforeToolSelection", &beforeToolSelection},
		{"BeforeTool", &beforeTool},
		{"AfterTool", &afterTool},
		{"PreCompress", &preCompress},
		{"Notification", &notification},
	} {
		if err := parseGeminiHookType(rawHooks, parsed.hookType, parsed.target); err != nil {
			return 0, err
		}
	}

	// Check for idempotency BEFORE removing hooks.
	// If the exact same hook command already exists, hooks are already installed.
	// When cleanupDone, we still need to write the file to persist the cleanup,
	// but we return 0 (not 12) so callers know no hooks were added.
	//
	// Sampling session-start alone is not enough: a stale Entire hook on any
	// other type (notably one left by the removed local-dev mode, which ran a
	// script inside the working tree) would then survive every non-force install
	// because this returns before the remove+add cycle below.
	allHookLists := [][]GeminiHookMatcher{
		sessionStart, sessionEnd, beforeAgent, afterAgent, beforeModel, afterModel,
		beforeToolSelection, beforeTool, afterTool, preCompress, notification,
	}
	if !force {
		existingCmd := getFirstEntireHookCommand(sessionStart)
		if existingCmd == sessionStartCmd && !hasStaleEntireHook(allHookLists, wantCommands) {
			if !cleanupDone {
				return 0, nil // Already installed with this exact command, nothing to write
			}
			// Cleanup needed but hooks already installed — write cleaned rawHooks
			// without running the full remove+add cycle.
			return 0, writeGeminiSettingsFile(rawSettings, rawHooks, hooksConfig, settingsPath)
		}
	}

	// Remove existing Entire hooks first. Besides clean installs, this is what
	// replaces hooks left by older versions — including local-dev hooks that
	// pointed at a script inside the working tree (see
	// agent.LegacyLocalDevHookScript, still matched by entireHookPrefixes).
	sessionStart = removeEntireHooks(sessionStart)
	sessionEnd = removeEntireHooks(sessionEnd)
	beforeAgent = removeEntireHooks(beforeAgent)
	afterAgent = removeEntireHooks(afterAgent)
	beforeModel = removeEntireHooks(beforeModel)
	afterModel = removeEntireHooks(afterModel)
	beforeToolSelection = removeEntireHooks(beforeToolSelection)
	beforeTool = removeEntireHooks(beforeTool)
	afterTool = removeEntireHooks(afterTool)
	preCompress = removeEntireHooks(preCompress)
	notification = removeEntireHooks(notification)

	// Install all hooks
	// Session lifecycle hooks
	sessionStart = addGeminiHook(sessionStart, "", "entire-session-start", sessionStartCmd)
	// SessionEnd fires on both "exit" and "logout" - install hooks for both matchers
	sessionEnd = addGeminiHook(sessionEnd, "exit", "entire-session-end-exit", sessionEndCmd)
	sessionEnd = addGeminiHook(sessionEnd, "logout", "entire-session-end-logout", sessionEndCmd)

	// Agent hooks (user prompt and response)
	beforeAgent = addGeminiHook(beforeAgent, "", "entire-before-agent", beforeAgentCmd)
	afterAgent = addGeminiHook(afterAgent, "", "entire-after-agent", afterAgentCmd)

	// Model hooks (LLM request/response - fires on every LLM call)
	beforeModel = addGeminiHook(beforeModel, "", "entire-before-model", beforeModelCmd)
	afterModel = addGeminiHook(afterModel, "", "entire-after-model", afterModelCmd)

	// Tool selection hook (before planner selects tools)
	beforeToolSelection = addGeminiHook(beforeToolSelection, "", "entire-before-tool-selection", beforeToolSelectionCmd)

	// Tool hooks (before/after tool execution)
	beforeTool = addGeminiHook(beforeTool, "*", "entire-before-tool", beforeToolCmd)
	afterTool = addGeminiHook(afterTool, "*", "entire-after-tool", afterToolCmd)

	// Compression hook (before chat history compression)
	preCompress = addGeminiHook(preCompress, "", "entire-pre-compress", preCompressCmd)

	// Notification hook (errors, warnings, info)
	notification = addGeminiHook(notification, "", "entire-notification", notificationCmd)

	// 12 hooks total:
	// - session-start (1)
	// - session-end exit + logout (2)
	// - before-agent, after-agent (2)
	// - before-model, after-model (2)
	// - before-tool-selection (1)
	// - before-tool, after-tool (2)
	// - pre-compress (1)
	// - notification (1)
	count := 12

	// Marshal modified hook types back to rawHooks
	marshalGeminiHookType(rawHooks, "SessionStart", sessionStart)
	marshalGeminiHookType(rawHooks, "SessionEnd", sessionEnd)
	marshalGeminiHookType(rawHooks, "BeforeAgent", beforeAgent)
	marshalGeminiHookType(rawHooks, "AfterAgent", afterAgent)
	marshalGeminiHookType(rawHooks, "BeforeModel", beforeModel)
	marshalGeminiHookType(rawHooks, "AfterModel", afterModel)
	marshalGeminiHookType(rawHooks, "BeforeToolSelection", beforeToolSelection)
	marshalGeminiHookType(rawHooks, "BeforeTool", beforeTool)
	marshalGeminiHookType(rawHooks, "AfterTool", afterTool)
	marshalGeminiHookType(rawHooks, "PreCompress", preCompress)
	marshalGeminiHookType(rawHooks, "Notification", notification)

	if err := writeGeminiSettingsFile(rawSettings, rawHooks, hooksConfig, settingsPath); err != nil {
		return 0, err
	}
	return count, nil
}

// stripNonArrayHookFields removes non-array values from rawHooks (e.g., legacy
// "enabled": true that old Entire versions wrote directly into hooks, which
// Gemini CLI 0.33+ rejects because hooks.additionalProperties requires arrays).
// Returns true if any fields were removed.
func stripNonArrayHookFields(rawHooks map[string]json.RawMessage) []string {
	var removed []string
	for key, val := range rawHooks {
		trimmed := bytes.TrimSpace(val)
		if len(trimmed) == 0 || trimmed[0] != '[' {
			delete(rawHooks, key)
			removed = append(removed, key)
		}
	}
	slices.Sort(removed)
	return removed
}

// writeGeminiSettingsFile marshals rawHooks and hooksConfig back into rawSettings and writes to disk.
func writeGeminiSettingsFile(rawSettings map[string]json.RawMessage, rawHooks map[string]json.RawMessage, hooksConfig GeminiHooksConfig, settingsPath string) error {
	hooksConfigJSON, err := jsonutil.MarshalWithNoHTMLEscape(hooksConfig)
	if err != nil {
		return fmt.Errorf("failed to marshal hooksConfig: %w", err)
	}
	rawSettings["hooksConfig"] = hooksConfigJSON

	hooksJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawHooks)
	if err != nil {
		return fmt.Errorf("failed to marshal hooks: %w", err)
	}
	rawSettings["hooks"] = hooksJSON

	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o750); err != nil {
		return fmt.Errorf("failed to create .gemini directory: %w", err)
	}

	output, err := jsonutil.MarshalIndentWithNewline(rawSettings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}

	if err := os.WriteFile(settingsPath, output, 0o600); err != nil {
		return fmt.Errorf("failed to write settings.json: %w", err)
	}
	return nil
}

// parseGeminiHookType parses a specific hook type from rawHooks into the target slice.
// Silently ignores parse errors (leaves target unchanged).
// A hook type that will not parse is reported rather than treated as empty: to
// removal that is the difference between "Entire owns nothing here" and "we
// could not tell", and the two must not be collapsed.
func parseGeminiHookType(rawHooks map[string]json.RawMessage, hookType string, target *[]GeminiHookMatcher) error {
	data, ok := rawHooks[hookType]
	if !ok {
		return nil
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("failed to parse %s hooks: %w", hookType, err)
	}
	return nil
}

// marshalGeminiHookType marshals a hook type back to rawHooks.
// If the slice is empty, removes the key from rawHooks.
func marshalGeminiHookType(rawHooks map[string]json.RawMessage, hookType string, matchers []GeminiHookMatcher) {
	if len(matchers) == 0 {
		delete(rawHooks, hookType)
		return
	}
	data, err := jsonutil.MarshalWithNoHTMLEscape(matchers)
	if err != nil {
		return // Silently ignore marshal errors (shouldn't happen)
	}
	rawHooks[hookType] = data
}

// managedHookTypes is every .gemini/settings.json hook key Entire installs
// under. Removal walks this list and detection is derived from removal, so a
// hook type cannot be stripped by one and missed by the other.
//
// InstallHooks keeps its own table because it also carries each type's matcher,
// name and command; TestAreHooksInstalledMatchesUninstallHooks pins the two
// together.
var managedHookTypes = []string{
	"SessionStart",
	"SessionEnd",
	"BeforeAgent",
	"AfterAgent",
	"BeforeModel",
	"AfterModel",
	"BeforeToolSelection",
	"BeforeTool",
	"AfterTool",
	"PreCompress",
	"Notification",
}

// geminiSettingsPath resolves .gemini/settings.json for the current repo, so
// install, detection and removal all look at the same file.
func geminiSettingsPath(ctx context.Context) string {
	return agent.HookConfigPath(ctx, ".gemini", GeminiSettingsFileName)
}

// UninstallHooks removes Entire hooks from Gemini CLI settings.
func (g *GeminiCLIAgent) UninstallHooks(ctx context.Context) error {
	if err := agent.RemoveHookArtifacts(geminiSettingsPath(ctx), removeEntireArtifacts); err != nil {
		return fmt.Errorf("remove Gemini CLI hooks: %w", err)
	}
	return nil
}

// AreHooksInstalled reports whether Entire owns anything in Gemini CLI's
// settings, by asking whether UninstallHooks would strip anything. Both answers
// come from removeEntireArtifacts, so detection cannot drift narrower than
// removal.
func (g *GeminiCLIAgent) AreHooksInstalled(ctx context.Context) bool {
	return agent.HookArtifactsInstalled(ctx, string(g.Name()), geminiSettingsPath(ctx), removeEntireArtifacts)
}

// removeEntireArtifacts implements agent.HookArtifactRemoval for Gemini CLI
// settings: Entire's hooks under every managed hook type, the legacy
// "hooks.enabled" field older versions of Entire wrote, and the hooksConfig
// switch Entire flips to make Gemini run hooks at all. Unknown hook types and
// unrelated settings round-trip untouched.
func removeEntireArtifacts(data []byte) ([]byte, bool, error) {
	var rawSettings map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawSettings); err != nil {
		return nil, false, fmt.Errorf("failed to parse settings.json: %w", err)
	}

	// rawHooks preserves unknown hook types.
	var rawHooks map[string]json.RawMessage
	if hooksRaw, ok := rawSettings["hooks"]; ok {
		if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
			return nil, false, fmt.Errorf("failed to parse hooks: %w", err)
		}
	}

	// A non-array value under "hooks" is Entire's own leftover: older versions
	// wrote "hooks.enabled" there, and Gemini CLI 0.33+ rejects the whole hooks
	// object over it. That makes it an artifact worth removing on its own, unlike
	// the unowned scaffolding below — leaving it behind means a config Gemini
	// refuses to load surviving an uninstall that reported success.
	changed := len(stripNonArrayHookFields(rawHooks)) > 0

	for _, hookType := range managedHookTypes {
		var matchers []GeminiHookMatcher
		if err := parseGeminiHookType(rawHooks, hookType, &matchers); err != nil {
			return nil, false, err
		}
		if !hasEntireHook(matchers) {
			continue
		}
		changed = true
		marshalGeminiHookType(rawHooks, hookType, removeEntireHooks(matchers))
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
		rawSettings["hooks"] = hooksJSON
	} else {
		delete(rawSettings, "hooks")
		// With no hooks left to run, the switch InstallHooks flipped to make
		// Gemini run them is ours to put back. It is only ever cleared as a
		// consequence of removing real artifacts, never counted as one: on its own
		// it is an ordinary Gemini setting, and treating it as Entire's would keep
		// reporting an installation forever after a successful uninstall.
		if err := clearHooksConfigEnabled(rawSettings); err != nil {
			return nil, false, err
		}
	}

	output, err := jsonutil.MarshalIndentWithNewline(rawSettings, "", "  ")
	if err != nil {
		return nil, false, fmt.Errorf("failed to marshal settings: %w", err)
	}
	return output, true, nil
}

// clearHooksConfigEnabled drops the "enabled" flag from hooksConfig, and
// hooksConfig itself when that leaves it empty. Any other field a user put there
// is preserved.
func clearHooksConfigEnabled(rawSettings map[string]json.RawMessage) error {
	configRaw, ok := rawSettings["hooksConfig"]
	if !ok {
		return nil
	}
	var rawConfig map[string]json.RawMessage
	if err := json.Unmarshal(configRaw, &rawConfig); err != nil {
		return fmt.Errorf("failed to parse hooksConfig: %w", err)
	}
	if _, ok := rawConfig["enabled"]; !ok {
		return nil
	}
	delete(rawConfig, "enabled")
	if len(rawConfig) == 0 {
		delete(rawSettings, "hooksConfig")
		return nil
	}
	configJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawConfig)
	if err != nil {
		return fmt.Errorf("failed to marshal hooksConfig: %w", err)
	}
	rawSettings["hooksConfig"] = configJSON
	return nil
}

// Helper functions for hook management

// addGeminiHook adds a hook entry to matchers.
// Unlike Claude Code, Gemini hooks require a "name" field.
func addGeminiHook(matchers []GeminiHookMatcher, matcherName, hookName, command string) []GeminiHookMatcher {
	entry := GeminiHookEntry{
		Name:    hookName,
		Type:    "command",
		Command: command,
	}

	// Find or create matcher
	for i, matcher := range matchers {
		if matcher.Matcher == matcherName {
			matchers[i].Hooks = append(matchers[i].Hooks, entry)
			return matchers
		}
	}

	// Create new matcher
	newMatcher := GeminiHookMatcher{
		Hooks: []GeminiHookEntry{entry},
	}
	if matcherName != "" {
		newMatcher.Matcher = matcherName
	}
	return append(matchers, newMatcher)
}

// isEntireHook checks if a command is an Entire hook
func isEntireHook(command string) bool {
	return agent.IsManagedHookCommand(command)
}

// hasEntireHook checks if any hook in the matchers is an Entire hook
func hasEntireHook(matchers []GeminiHookMatcher) bool {
	for _, matcher := range matchers {
		for _, hook := range matcher.Hooks {
			if isEntireHook(hook.Command) {
				return true
			}
		}
	}
	return false
}

// getFirstEntireHookCommand returns the command of the first Entire hook found, or empty string
func getFirstEntireHookCommand(matchers []GeminiHookMatcher) string {
	for _, matcher := range matchers {
		for _, hook := range matcher.Hooks {
			if isEntireHook(hook.Command) {
				return hook.Command
			}
		}
	}
	return ""
}

// hasStaleEntireHook reports whether any list holds an Entire-owned hook whose
// command is not in want — i.e. a hook this version would not write. Foreign
// hooks are ignored; only commands recognized by entireHookPrefixes count, which
// includes the shapes older versions wrote (see agent.LegacyLocalDevHookScript).
func hasStaleEntireHook(lists [][]GeminiHookMatcher, want []string) bool {
	for _, list := range lists {
		for _, matcher := range list {
			for _, hook := range matcher.Hooks {
				if isEntireHook(hook.Command) && !slices.Contains(want, hook.Command) {
					return true
				}
			}
		}
	}
	return false
}

// removeEntireHooks removes all Entire hooks from a list of matchers.
func removeEntireHooks(matchers []GeminiHookMatcher) []GeminiHookMatcher {
	result := make([]GeminiHookMatcher, 0, len(matchers))
	for _, matcher := range matchers {
		filteredHooks := make([]GeminiHookEntry, 0, len(matcher.Hooks))
		for _, hook := range matcher.Hooks {
			if !isEntireHook(hook.Command) {
				filteredHooks = append(filteredHooks, hook)
			}
		}
		// Only keep the matcher if it has hooks remaining
		if len(filteredHooks) > 0 {
			matcher.Hooks = filteredHooks
			result = append(result, matcher)
		}
	}
	return result
}
