package geminicli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Ensure GeminiCLIAgent implements HookSupport
var (
	_ agent.HookSupport       = (*GeminiCLIAgent)(nil)
	_ agent.HookConfigLocator = (*GeminiCLIAgent)(nil)
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

// geminiHookConfig returns .gemini/settings.json for the current worktree,
// opened through the worktree's root. That directory lives in the working tree,
// which arrives by clone, so a checked-in symlink at `.gemini` must not be
// something Entire creates directories under and writes through. See
// agent.HookConfigFile.
func geminiHookConfig(ctx context.Context) (*agent.HookConfigFile, error) {
	// Repo root rather than CWD, so hooks land correctly when run from a
	// subdirectory.
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		// Not a repository (tests, and `enable` before `git init`): the process
		// directory is the only candidate, and it is one the caller chose rather
		// than one derived from anything read off disk.
		repoRoot, err = os.Getwd() //nolint:forbidigo // Intentional fallback when WorktreeRoot() fails
		if err != nil {
			return nil, fmt.Errorf("failed to get current directory: %w", err)
		}
	}
	return agent.OpenHookConfig(repoRoot, (&GeminiCLIAgent{}).HookConfigRelPath()) //nolint:wrapcheck // agent.HookConfigFile already names the file in its error
}

// InstallHooks installs Gemini CLI hooks in .gemini/settings.json.
// If force is true, removes existing Entire hooks before installing.
// Returns the number of hooks installed.
func (g *GeminiCLIAgent) InstallHooks(ctx context.Context, force bool) (int, error) {
	cfg, err := geminiHookConfig(ctx)
	if err != nil {
		return 0, err
	}

	// Read existing settings if they exist
	var rawSettings map[string]json.RawMessage

	// rawHooks preserves unknown hook types
	var rawHooks map[string]json.RawMessage

	var hooksConfig GeminiHooksConfig

	existingData, readErr := cfg.Read()
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
	cleanupDone := stripNonArrayHookFields(ctx, rawHooks)

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
			return 0, writeGeminiSettingsFile(rawSettings, rawHooks, hooksConfig, cfg)
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

	if err := writeGeminiSettingsFile(rawSettings, rawHooks, hooksConfig, cfg); err != nil {
		return 0, err
	}
	return count, nil
}

// stripNonArrayHookFields removes non-array values from rawHooks (e.g., legacy
// "enabled": true that old Entire versions wrote directly into hooks, which
// Gemini CLI 0.33+ rejects because hooks.additionalProperties requires arrays).
// Returns true if any fields were removed.
func stripNonArrayHookFields(ctx context.Context, rawHooks map[string]json.RawMessage) bool {
	var cleaned bool
	for key, val := range rawHooks {
		trimmed := bytes.TrimSpace(val)
		if len(trimmed) == 0 || trimmed[0] != '[' {
			delete(rawHooks, key)
			logging.Debug(ctx, "removed non-array field from hooks", slog.String("key", key))
			cleaned = true
		}
	}
	return cleaned
}

// writeGeminiSettingsFile marshals rawHooks and hooksConfig back into rawSettings and writes to disk.
func writeGeminiSettingsFile(rawSettings map[string]json.RawMessage, rawHooks map[string]json.RawMessage, hooksConfig GeminiHooksConfig, cfg *agent.HookConfigFile) error {
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

	output, err := jsonutil.MarshalIndentWithNewline(rawSettings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}

	// Write creates .gemini with MkdirAllNoSymlink.
	return cfg.Write(output, 0o600) //nolint:wrapcheck // agent.HookConfigFile already names the file in its error
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
	data, err := jsonutil.MarshalWithNoHTMLEscape(matchers)
	if err != nil {
		return // Silently ignore marshal errors (shouldn't happen)
	}
	rawHooks[hookType] = data
}

// UninstallHooks removes Entire hooks from Gemini CLI settings.
func (g *GeminiCLIAgent) UninstallHooks(ctx context.Context) error {
	// Use repo root to find .gemini directory when run from a subdirectory
	cfg, err := geminiHookConfig(ctx)
	if err != nil {
		return err
	}
	data, err := cfg.Read()
	if err != nil {
		// An absent file means nothing to uninstall; an unreadable one does not.
		// Collapsing both leaves hooks on disk while reporting success.
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read %s: %w", cfg.Path(), err)
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

	// Strip non-array values from hooks (same migration as InstallHooks)
	stripNonArrayHookFields(ctx, rawHooks)

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

	if err := cfg.Write(output, 0o600); err != nil {
		return err //nolint:wrapcheck // agent.HookConfigFile already names the file in its error
	}
	return nil
}

// AreHooksInstalled checks if Entire hooks are installed.
//
// A missing config file is an answer, not a failure: that file is where the
// state lives, so its absence means no hooks. Anything that stops us reading the
// answer — an unreadable file, malformed config — is returned as an error, since
// "we could not tell" and "there are none" are different things to a caller
// deciding whether hooks can be left alone.
func (g *GeminiCLIAgent) AreHooksInstalled(ctx context.Context) (bool, error) {
	// Use repo root to find .gemini directory when run from a subdirectory
	cfg, err := geminiHookConfig(ctx)
	if err != nil {
		return false, err
	}
	data, err := cfg.Read()
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		logging.Warn(ctx, "gemini: failed to read settings file", "path", cfg.Path(), "err", err)
		return false, fmt.Errorf("read %s: %w", cfg.Path(), err)
	}

	var settings GeminiSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		logging.Warn(ctx, "gemini: failed to parse settings file", "path", cfg.Path(), "err", err)
		return false, fmt.Errorf("parse hook config: %w", err)
	}

	// Check for at least one of our hooks using isEntireHook (matches legacy hook shapes too)
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
		hasEntireHook(settings.Hooks.Notification), nil
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

// HookConfigRelPath implements agent.HookConfigLocator.
func (g *GeminiCLIAgent) HookConfigRelPath() string { return ".gemini/" + GeminiSettingsFileName }
