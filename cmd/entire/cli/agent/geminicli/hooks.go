package geminicli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Ensure GeminiCLIAgent implements HookSupport and HookHandler
var (
	_ agent.HookSupport = (*GeminiCLIAgent)(nil)
	_ agent.HookHandler = (*GeminiCLIAgent)(nil)
)

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

// entireHookPrefixes are command prefixes that identify Entire hooks
var entireHookPrefixes = []string{
	"entire ",
	"go run ${GEMINI_PROJECT_DIR}/cmd/entire/main.go ",
}

// GetHookNames returns the hook verbs Gemini CLI supports.
// These become subcommands: entire hooks gemini <verb>
func (g *GeminiCLIAgent) GetHookNames() []string {
	return []string{
		HookNameSessionStart,
		HookNameSessionEnd,
		HookNameBeforeAgent,
		HookNameAfterAgent,
		HookNameBeforeModel,
		HookNameAfterModel,
		HookNameBeforeToolSelection,
		HookNameBeforeTool,
		HookNameAfterTool,
		HookNamePreCompress,
		HookNameNotification,
	}
}

// InstallHooks installs Gemini CLI hooks in .gemini/settings.json.
// If force is true, removes existing Entire hooks before installing.
// Returns the number of hooks installed.
func (g *GeminiCLIAgent) InstallHooks(localDev bool, force bool) (int, error) {
	// Use repo root instead of CWD to find .gemini directory
	// This ensures hooks are installed correctly when run from a subdirectory
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		// Fallback to CWD if not in a git repo (e.g., during tests)
		repoRoot, err = os.Getwd() //nolint:forbidigo // Intentional fallback when RepoRoot() fails (tests run outside git repos)
		if err != nil {
			return 0, fmt.Errorf("failed to get current directory: %w", err)
		}
	}

	settingsPath := filepath.Join(repoRoot, ".gemini", GeminiSettingsFileName)

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

	// Enable hooks via hooksConfig
	// hooksConfig.Enabled must be true for Gemini CLI to execute hooks
	hooksConfig.Enabled = true

	// Define hook commands based on localDev mode
	var cmdPrefix string
	if localDev {
		cmdPrefix = "go run ${GEMINI_PROJECT_DIR}/cmd/entire/main.go hooks gemini "
	} else {
		cmdPrefix = "entire hooks gemini "
	}

	// Parse only the hook types we need to modify
	var sessionStart, sessionEnd, beforeAgent, afterAgent []GeminiHookMatcher
	var beforeModel, afterModel, beforeToolSelection []GeminiHookMatcher
	var beforeTool, afterTool, preCompress, notification []GeminiHookMatcher
	parseGeminiHookType(rawHooks, "SessionStart", &sessionStart)
	parseGeminiHookType(rawHooks, "SessionEnd", &sessionEnd)
	parseGeminiHookType(rawHooks, "BeforeAgent", &beforeAgent)
	parseGeminiHookType(rawHooks, "AfterAgent", &afterAgent)
	parseGeminiHookType(rawHooks, "BeforeModel", &beforeModel)
	parseGeminiHookType(rawHooks, "AfterModel", &afterModel)
	parseGeminiHookType(rawHooks, "BeforeToolSelection", &beforeToolSelection)
	parseGeminiHookType(rawHooks, "BeforeTool", &beforeTool)
	parseGeminiHookType(rawHooks, "AfterTool", &afterTool)
	parseGeminiHookType(rawHooks, "PreCompress", &preCompress)
	parseGeminiHookType(rawHooks, "Notification", &notification)

	// Check for idempotency BEFORE removing hooks
	// If the exact same hook command already exists, return 0 (no changes needed)
	if !force {
		existingCmd := getFirstEntireHookCommand(sessionStart)
		expectedCmd := cmdPrefix + "session-start"
		if existingCmd == expectedCmd {
			return 0, nil // Already installed with same mode
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
	sessionStart = addGeminiHook(sessionStart, "", "entire-session-start", cmdPrefix+"session-start")
	// SessionEnd fires on both "exit" and "logout" - install hooks for both matchers
	sessionEnd = addGeminiHook(sessionEnd, "exit", "entire-session-end-exit", cmdPrefix+"session-end")
	sessionEnd = addGeminiHook(sessionEnd, "logout", "entire-session-end-logout", cmdPrefix+"session-end")

	// Agent hooks (user prompt and response)
	beforeAgent = addGeminiHook(beforeAgent, "", "entire-before-agent", cmdPrefix+"before-agent")
	afterAgent = addGeminiHook(afterAgent, "", "entire-after-agent", cmdPrefix+"after-agent")

	// Model hooks (LLM request/response - fires on every LLM call)
	beforeModel = addGeminiHook(beforeModel, "", "entire-before-model", cmdPrefix+"before-model")
	afterModel = addGeminiHook(afterModel, "", "entire-after-model", cmdPrefix+"after-model")

	// Tool selection hook (before planner selects tools)
	beforeToolSelection = addGeminiHook(beforeToolSelection, "", "entire-before-tool-selection", cmdPrefix+"before-tool-selection")

	// Tool hooks (before/after tool execution)
	beforeTool = addGeminiHook(beforeTool, "*", "entire-before-tool", cmdPrefix+"before-tool")
	afterTool = addGeminiHook(afterTool, "*", "entire-after-tool", cmdPrefix+"after-tool")

	// Compression hook (before chat history compression)
	preCompress = addGeminiHook(preCompress, "", "entire-pre-compress", cmdPrefix+"pre-compress")

	// Notification hook (errors, warnings, info)
	notification = addGeminiHook(notification, "", "entire-notification", cmdPrefix+"notification")

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

	// Marshal hooksConfig back to raw settings
	hooksConfigJSON, err := json.Marshal(hooksConfig)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal hooksConfig: %w", err)
	}
	rawSettings["hooksConfig"] = hooksConfigJSON

	// Marshal hooks back to raw settings (preserving unknown hook types)
	hooksJSON, err := json.Marshal(rawHooks)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal hooks: %w", err)
	}
	rawSettings["hooks"] = hooksJSON

	// Write back to file
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o750); err != nil {
		return 0, fmt.Errorf("failed to create .gemini directory: %w", err)
	}

	output, err := json.MarshalIndent(rawSettings, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("failed to marshal settings: %w", err)
	}

	if err := os.WriteFile(settingsPath, output, 0o600); err != nil {
		return 0, fmt.Errorf("failed to write settings.json: %w", err)
	}

	return count, nil
}

// parseGeminiHookType parses a specific hook type from rawHooks into the target slice.
// Silently ignores parse errors (leaves target unchanged).
func parseGeminiHookType(rawHooks map[string]json.RawMessage, hookType string, target *[]GeminiHookMatcher) {
	if data, ok := rawHooks[hookType]; ok {
		//nolint:errcheck,gosec // Intentionally ignoring parse errors - leave target as nil/empty
		json.Unmarshal(data, target)
	}
}

// marshalGeminiHookType marshals a hook type back to rawHooks.
// If the slice is empty, removes the key from rawHooks.
func marshalGeminiHookType(rawHooks map[string]json.RawMessage, hookType string, matchers []GeminiHookMatcher) {
	if len(matchers) == 0 {
		delete(rawHooks, hookType)
		return
	}
	data, err := json.Marshal(matchers)
	if err != nil {
		return // Silently ignore marshal errors (shouldn't happen)
	}
	rawHooks[hookType] = data
}

// UninstallHooks removes Entire hooks from Gemini CLI settings.
func (g *GeminiCLIAgent) UninstallHooks() error {
	// Use repo root to find .gemini directory when run from a subdirectory
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		repoRoot = "." // Fallback to CWD if not in a git repo
	}
	settingsPath := filepath.Join(repoRoot, ".gemini", GeminiSettingsFileName)
	data, err := os.ReadFile(settingsPath) //nolint:gosec // path is constructed from repo root + fixed path
	if err != nil {
		return nil //nolint:nilerr // No settings file means nothing to uninstall
	}

	var rawSettings map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawSettings); err != nil {
		return fmt.Errorf("failed to parse settings.json: %w", err)
	}

	// rawHooks preserves unknown hook types
	var rawHooks map[string]json.RawMessage
	if hooksRaw, ok := rawSettings["hooks"]; ok {
		if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
			return fmt.Errorf("failed to parse hooks: %w", err)
		}
	}
	if rawHooks == nil {
		rawHooks = make(map[string]json.RawMessage)
	}

	// Parse only the hook types we need to modify
	var sessionStart, sessionEnd, beforeAgent, afterAgent []GeminiHookMatcher
	var beforeModel, afterModel, beforeToolSelection []GeminiHookMatcher
	var beforeTool, afterTool, preCompress, notification []GeminiHookMatcher
	parseGeminiHookType(rawHooks, "SessionStart", &sessionStart)
	parseGeminiHookType(rawHooks, "SessionEnd", &sessionEnd)
	parseGeminiHookType(rawHooks, "BeforeAgent", &beforeAgent)
	parseGeminiHookType(rawHooks, "AfterAgent", &afterAgent)
	parseGeminiHookType(rawHooks, "BeforeModel", &beforeModel)
	parseGeminiHookType(rawHooks, "AfterModel", &afterModel)
	parseGeminiHookType(rawHooks, "BeforeToolSelection", &beforeToolSelection)
	parseGeminiHookType(rawHooks, "BeforeTool", &beforeTool)
	parseGeminiHookType(rawHooks, "AfterTool", &afterTool)
	parseGeminiHookType(rawHooks, "PreCompress", &preCompress)
	parseGeminiHookType(rawHooks, "Notification", &notification)

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
		hooksJSON, err := json.Marshal(rawHooks)
		if err != nil {
			return fmt.Errorf("failed to marshal hooks: %w", err)
		}
		rawSettings["hooks"] = hooksJSON
	} else {
		delete(rawSettings, "hooks")
	}

	// Write back
	output, err := json.MarshalIndent(rawSettings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}

	if err := os.WriteFile(settingsPath, output, 0o600); err != nil {
		return fmt.Errorf("failed to write settings.json: %w", err)
	}
	return nil
}

// AreHooksInstalled checks if Entire hooks are installed.
func (g *GeminiCLIAgent) AreHooksInstalled() bool {
	// Use repo root to find .gemini directory when run from a subdirectory
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		repoRoot = "." // Fallback to CWD if not in a git repo
	}
	settingsPath := filepath.Join(repoRoot, ".gemini", GeminiSettingsFileName)
	data, err := os.ReadFile(settingsPath) //nolint:gosec // path is constructed from repo root + fixed path
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

// GetSupportedHooks returns the hook types Gemini CLI supports.
func (g *GeminiCLIAgent) GetSupportedHooks() []agent.HookType {
	return []agent.HookType{
		agent.HookSessionStart,
		agent.HookSessionEnd,       // Maps to Gemini's SessionEnd (explicit exit/logout)
		agent.HookStop,             // Maps to Gemini's AfterAgent (end of response)
		agent.HookUserPromptSubmit, // Maps to Gemini's BeforeAgent (user prompt)
		agent.HookPreToolUse,       // Maps to Gemini's BeforeTool
		agent.HookPostToolUse,      // Maps to Gemini's AfterTool
	}
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
	for _, prefix := range entireHookPrefixes {
		if strings.HasPrefix(command, prefix) {
			return true
		}
	}
	return false
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

// removeEntireHooks removes all Entire hooks from a list of matchers
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
