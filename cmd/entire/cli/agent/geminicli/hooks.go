package geminicli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
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

// entireHookPrefixes are command prefixes that identify Entire hooks. Each
// prefix is scoped to the `hooks` verb: recognition by binary name alone
// (`entire `) would claim user-authored hooks that merely invoke the entire
// CLI (e.g. `entire status --json > /tmp/s.json`) as ours — and the install
// path's remove-then-add cycle would delete them on a plain `entire enable`.
// The "go run" prefix is retained so hooks installed by older versions are
// still recognized.
var entireHookPrefixes = []string{
	"entire hooks ",
	agent.LocalDevHookScript + " hooks ",
	`go run "$(git rev-parse --show-toplevel)"/cmd/entire/main.go hooks `,
}

// entireGeminiHookNames are the "name" fields InstallHooks writes; the name
// is Gemini's per-entry identifier and the cheapest on-disk discriminator
// between our entries and user-authored ones.
var entireGeminiHookNames = map[string]bool{
	"entire-session-start":         true,
	"entire-session-end-exit":      true,
	"entire-session-end-logout":    true,
	"entire-before-agent":          true,
	"entire-after-agent":           true,
	"entire-before-model":          true,
	"entire-after-model":           true,
	"entire-before-tool-selection": true,
	"entire-before-tool":           true,
	"entire-after-tool":            true,
	"entire-pre-compress":          true,
	"entire-notification":          true,
}

// InstallHooks installs Gemini CLI hooks in .gemini/settings.json.
// If force is true, removes existing Entire hooks before installing.
// Returns the number of hooks installed.
func (g *GeminiCLIAgent) InstallHooks(ctx context.Context, localDev bool, force bool) (int, error) {
	// Use repo root instead of CWD to find .gemini directory
	// This ensures hooks are installed correctly when run from a subdirectory
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		// Fallback to CWD if not in a git repo (e.g., during tests)
		repoRoot, err = os.Getwd() //nolint:forbidigo // Intentional fallback when WorktreeRoot() fails (tests run outside git repos)
		if err != nil {
			return 0, fmt.Errorf("failed to get current directory: %w", err)
		}
	}

	settingsPath := filepath.Join(repoRoot, ".gemini", GeminiSettingsFileName)
	return installHooksToFile(ctx, settingsPath, localDev, force)
}

// installHooksToFile installs Entire's Gemini CLI hooks into the settings
// file at settingsPath, preserving every unrelated key. Shared by the
// repo-level install above and the user-level install (InstallUserHooks).
func installHooksToFile(ctx context.Context, settingsPath string, localDev, force bool) (int, error) {
	// Read existing settings if they exist
	var rawSettings map[string]json.RawMessage

	// rawHooks preserves unknown hook types
	var rawHooks map[string]json.RawMessage

	// hooksConfig is held raw so every key except "enabled" (the one Entire
	// manages) round-trips: decoding into a typed struct dropped user keys
	// like "timeout" on write-back.
	var hooksConfig map[string]json.RawMessage

	existingData, readErr := os.ReadFile(settingsPath) //nolint:gosec // path is constructed from cwd + fixed path
	switch {
	case readErr == nil:
		if err := json.Unmarshal(existingData, &rawSettings); err != nil {
			return 0, fmt.Errorf("failed to parse existing %s: %w", settingsPath, err)
		}
		if hooksRaw, ok := rawSettings["hooks"]; ok {
			if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
				return 0, fmt.Errorf("failed to parse hooks in %s: %w", settingsPath, err)
			}
		}
		if hooksConfigRaw, ok := rawSettings["hooksConfig"]; ok {
			if err := json.Unmarshal(hooksConfigRaw, &hooksConfig); err != nil {
				return 0, fmt.Errorf("failed to parse hooksConfig in %s: %w", settingsPath, err)
			}
		}
	case errors.Is(readErr, fs.ErrNotExist):
		rawSettings = make(map[string]json.RawMessage)
	default:
		// Only a genuinely missing file means "start fresh". Any other read
		// failure (permissions, I/O) must abort: proceeding would replace the
		// user's whole settings file with an Entire-only one.
		return 0, fmt.Errorf("failed to read %s: %w", settingsPath, readErr)
	}

	if rawHooks == nil {
		rawHooks = make(map[string]json.RawMessage)
	}
	if hooksConfig == nil {
		hooksConfig = make(map[string]json.RawMessage)
	}

	cleanupDone := stripLegacyHooksEnabledField(ctx, rawHooks)

	// hooksConfig.enabled must be true for Gemini CLI to execute hooks.
	hooksConfig["enabled"] = json.RawMessage("true")

	// Define hook commands based on localDev mode
	var cmdPrefix string
	if localDev {
		cmdPrefix = agent.LocalDevHookScript + " hooks gemini "
	} else {
		cmdPrefix = "entire hooks gemini "
	}

	// Parse only the hook types we need to modify
	var sessionStart, sessionEnd, beforeAgent, afterAgent []GeminiHookMatcher
	var beforeModel, afterModel, beforeToolSelection []GeminiHookMatcher
	var beforeTool, afterTool, preCompress, notification []GeminiHookMatcher
	if err := parseGeminiHookSections(rawHooks, settingsPath, map[string]*[]GeminiHookMatcher{
		"SessionStart":        &sessionStart,
		"SessionEnd":          &sessionEnd,
		"BeforeAgent":         &beforeAgent,
		"AfterAgent":          &afterAgent,
		"BeforeModel":         &beforeModel,
		"AfterModel":          &afterModel,
		"BeforeToolSelection": &beforeToolSelection,
		"BeforeTool":          &beforeTool,
		"AfterTool":           &afterTool,
		"PreCompress":         &preCompress,
		"Notification":        &notification,
	}); err != nil {
		return 0, err
	}

	// Check for idempotency BEFORE removing hooks.
	// If the exact same hook command already exists, hooks are already installed.
	// When cleanupDone, we still need to write the file to persist the cleanup,
	// but we return 0 (not 12) so callers know no hooks were added.
	if !force {
		existingCmd := getFirstEntireHookCommand(sessionStart)
		expectedCmd := cmdPrefix + "session-start"
		if !localDev {
			expectedCmd = agent.WrapProductionJSONWarningHookCommand(expectedCmd, agent.WarningFormatSingleLine)
		}
		if existingCmd == expectedCmd {
			if !cleanupDone {
				return 0, nil // Already installed with same mode, nothing to write
			}
			// Cleanup needed but hooks already installed — write cleaned rawHooks
			// without running the full remove+add cycle.
			return 0, writeGeminiSettingsFile(rawSettings, rawHooks, hooksConfig, settingsPath)
		}
	}

	// Remove existing Entire hooks first (for clean installs and mode switching)
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
	sessionStartCmd := cmdPrefix + "session-start"
	if !localDev {
		sessionStartCmd = agent.WrapProductionJSONWarningHookCommand(sessionStartCmd, agent.WarningFormatSingleLine)
	}
	sessionStart = addGeminiHook(sessionStart, "", "entire-session-start", sessionStartCmd)
	// SessionEnd fires on both "exit" and "logout" - install hooks for both matchers
	sessionEndCmd := cmdPrefix + "session-end"
	if !localDev {
		sessionEndCmd = agent.WrapProductionSilentHookCommand(sessionEndCmd)
	}
	sessionEnd = addGeminiHook(sessionEnd, "exit", "entire-session-end-exit", sessionEndCmd)
	sessionEnd = addGeminiHook(sessionEnd, "logout", "entire-session-end-logout", sessionEndCmd)

	// Agent hooks (user prompt and response)
	beforeAgentCmd := cmdPrefix + "before-agent"
	afterAgentCmd := cmdPrefix + "after-agent"
	beforeModelCmd := cmdPrefix + "before-model"
	afterModelCmd := cmdPrefix + "after-model"
	beforeToolSelectionCmd := cmdPrefix + "before-tool-selection"
	beforeToolCmd := cmdPrefix + "before-tool"
	afterToolCmd := cmdPrefix + "after-tool"
	preCompressCmd := cmdPrefix + "pre-compress"
	notificationCmd := cmdPrefix + "notification"
	if !localDev {
		beforeAgentCmd = agent.WrapProductionSilentHookCommand(beforeAgentCmd)
		afterAgentCmd = agent.WrapProductionSilentHookCommand(afterAgentCmd)
		beforeModelCmd = agent.WrapProductionSilentHookCommand(beforeModelCmd)
		afterModelCmd = agent.WrapProductionSilentHookCommand(afterModelCmd)
		beforeToolSelectionCmd = agent.WrapProductionSilentHookCommand(beforeToolSelectionCmd)
		beforeToolCmd = agent.WrapProductionSilentHookCommand(beforeToolCmd)
		afterToolCmd = agent.WrapProductionSilentHookCommand(afterToolCmd)
		preCompressCmd = agent.WrapProductionSilentHookCommand(preCompressCmd)
		notificationCmd = agent.WrapProductionSilentHookCommand(notificationCmd)
	}
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

// stripLegacyHooksEnabledField removes the legacy "enabled" boolean that old
// Entire versions wrote directly into hooks, which Gemini CLI 0.33+ rejects
// because hooks.additionalProperties requires arrays. Only that one known key
// is touched: every other non-array member of hooks is user data and must
// round-trip. Returns true if the field was removed.
func stripLegacyHooksEnabledField(ctx context.Context, rawHooks map[string]json.RawMessage) bool {
	val, ok := rawHooks["enabled"]
	if !ok {
		return false
	}
	trimmed := bytes.TrimSpace(val)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		return false // an array under this key is a (strange) hook list, not the legacy field
	}
	delete(rawHooks, "enabled")
	logging.Debug(ctx, "removed legacy non-array field from hooks", slog.String("key", "enabled"))
	return true
}

// writeGeminiSettingsFile marshals rawHooks and hooksConfig back into rawSettings and writes to disk.
func writeGeminiSettingsFile(rawSettings map[string]json.RawMessage, rawHooks map[string]json.RawMessage, hooksConfig map[string]json.RawMessage, settingsPath string) error {
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

	if err := jsonutil.WriteFileAtomicFollowingSymlinks(settingsPath, output, 0o600); err != nil {
		return fmt.Errorf("failed to write %s: %w", settingsPath, err)
	}
	return nil
}

// parseGeminiHookSections parses the hook types Entire manages out of
// rawHooks. A section that exists but does not parse as []GeminiHookMatcher
// aborts with an error naming the section and file: these sections get
// rewritten on the way out, so an unparseable one cannot round-trip verbatim
// — silently treating it as empty would clobber it on install and delete it
// on uninstall.
func parseGeminiHookSections(rawHooks map[string]json.RawMessage, settingsPath string, sections map[string]*[]GeminiHookMatcher) error {
	for hookType, target := range sections {
		data, ok := rawHooks[hookType]
		if !ok {
			continue
		}
		if err := json.Unmarshal(data, target); err != nil {
			return fmt.Errorf("hooks.%s in %s has an unexpected shape (fix or remove that section): %w", hookType, settingsPath, err)
		}
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

// UninstallHooks removes Entire hooks from Gemini CLI settings.
func (g *GeminiCLIAgent) UninstallHooks(ctx context.Context) error {
	// Use repo root to find .gemini directory when run from a subdirectory
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "." // Fallback to CWD if not in a git repo
	}
	settingsPath := filepath.Join(repoRoot, ".gemini", GeminiSettingsFileName)
	return uninstallHooksFromFile(ctx, settingsPath)
}

// uninstallHooksFromFile removes Entire hooks (and only Entire hooks) from
// the settings file at settingsPath, preserving every unrelated key.
func uninstallHooksFromFile(ctx context.Context, settingsPath string) error {
	data, err := os.ReadFile(settingsPath) //nolint:gosec // path is constructed from a fixed settings location
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // No settings file means nothing to uninstall
		}
		return fmt.Errorf("failed to read %s: %w", settingsPath, err)
	}

	var rawSettings map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawSettings); err != nil {
		return fmt.Errorf("failed to parse %s: %w", settingsPath, err)
	}

	// rawHooks preserves unknown hook types
	var rawHooks map[string]json.RawMessage
	if hooksRaw, ok := rawSettings["hooks"]; ok {
		if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
			return fmt.Errorf("failed to parse hooks in %s: %w", settingsPath, err)
		}
	}
	if rawHooks == nil {
		rawHooks = make(map[string]json.RawMessage)
	}

	// Strip the legacy hooks.enabled field (same migration as InstallHooks)
	stripLegacyHooksEnabledField(ctx, rawHooks)

	// Parse only the hook types we need to modify
	var sessionStart, sessionEnd, beforeAgent, afterAgent []GeminiHookMatcher
	var beforeModel, afterModel, beforeToolSelection []GeminiHookMatcher
	var beforeTool, afterTool, preCompress, notification []GeminiHookMatcher
	if err := parseGeminiHookSections(rawHooks, settingsPath, map[string]*[]GeminiHookMatcher{
		"SessionStart":        &sessionStart,
		"SessionEnd":          &sessionEnd,
		"BeforeAgent":         &beforeAgent,
		"AfterAgent":          &afterAgent,
		"BeforeModel":         &beforeModel,
		"AfterModel":          &afterModel,
		"BeforeToolSelection": &beforeToolSelection,
		"BeforeTool":          &beforeTool,
		"AfterTool":           &afterTool,
		"PreCompress":         &preCompress,
		"Notification":        &notification,
	}); err != nil {
		return err
	}

	// Remove Entire hooks from all hook types
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

	// Marshal hooks back (preserving unknown hook types)
	if len(rawHooks) > 0 {
		hooksJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawHooks)
		if err != nil {
			return fmt.Errorf("failed to marshal hooks: %w", err)
		}
		rawSettings["hooks"] = hooksJSON
	} else {
		delete(rawSettings, "hooks")
	}

	// Write back
	output, err := jsonutil.MarshalIndentWithNewline(rawSettings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}

	if err := jsonutil.WriteFileAtomicFollowingSymlinks(settingsPath, output, 0o600); err != nil {
		return fmt.Errorf("failed to write %s: %w", settingsPath, err)
	}
	return nil
}

// AreHooksInstalled checks if Entire hooks are installed.
func (g *GeminiCLIAgent) AreHooksInstalled(ctx context.Context) bool {
	// Use repo root to find .gemini directory when run from a subdirectory
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "." // Fallback to CWD if not in a git repo
	}
	settingsPath := filepath.Join(repoRoot, ".gemini", GeminiSettingsFileName)
	return areHooksInstalledInFile(settingsPath)
}

// areHooksInstalledInFile reports whether any Entire hook is present in the
// settings file at settingsPath.
func areHooksInstalledInFile(settingsPath string) bool {
	data, err := os.ReadFile(settingsPath) //nolint:gosec // path is constructed from a fixed settings location
	if err != nil {
		return false
	}

	var settings GeminiSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return false
	}

	// Check for at least one of our hooks using isEntireHook (works for both localDev and production)
	return hasEntireHook(settings.Hooks.SessionStart) ||
		hasEntireHook(settings.Hooks.SessionEnd) ||
		hasEntireHook(settings.Hooks.BeforeAgent) ||
		hasEntireHook(settings.Hooks.AfterAgent) ||
		hasEntireHook(settings.Hooks.BeforeModel) ||
		hasEntireHook(settings.Hooks.AfterModel) ||
		hasEntireHook(settings.Hooks.BeforeToolSelection) ||
		hasEntireHook(settings.Hooks.BeforeTool) ||
		hasEntireHook(settings.Hooks.AfterTool) ||
		hasEntireHook(settings.Hooks.PreCompress) ||
		hasEntireHook(settings.Hooks.Notification)
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
	return agent.IsManagedHookCommand(command, entireHookPrefixes)
}

// isEntireHookEntry reports whether a hook entry is Entire's: by its "name"
// field (the identifier every install writes) or by its command's managed
// prefix (covers entries whose name was hand-edited but whose command is
// still ours).
func isEntireHookEntry(hook GeminiHookEntry) bool {
	return entireGeminiHookNames[hook.Name] || isEntireHook(hook.Command)
}

// hasEntireHook checks if any hook in the matchers is an Entire hook
func hasEntireHook(matchers []GeminiHookMatcher) bool {
	for _, matcher := range matchers {
		for _, hook := range matcher.Hooks {
			if isEntireHookEntry(hook) {
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
			if isEntireHookEntry(hook) {
				return hook.Command
			}
		}
	}
	return ""
}

// removeEntireHooks removes all Entire hooks from a list of matchers
func removeEntireHooks(matchers []GeminiHookMatcher) []GeminiHookMatcher {
	result := make([]GeminiHookMatcher, 0, len(matchers))
	for _, matcher := range matchers {
		filteredHooks := make([]GeminiHookEntry, 0, len(matcher.Hooks))
		for _, hook := range matcher.Hooks {
			if !isEntireHookEntry(hook) {
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
