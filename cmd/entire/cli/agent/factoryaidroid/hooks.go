package factoryaidroid

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Ensure FactoryAIDroidAgent implements HookSupport
var _ agent.HookSupport = (*FactoryAIDroidAgent)(nil)

// Factory AI Droid's own names for the hooks it fires, as they appear in
// hookSpecificOutput.hookEventName on the way back. A response naming a hook that
// did not fire is discarded, so which one Entire echoes is load-bearing — see
// RenderContextInjection.
const (
	NativeHookSessionStart     = "SessionStart"
	NativeHookUserPromptSubmit = "UserPromptSubmit"
)

// Factory AI Droid hook names - these become subcommands under `entire hooks factoryai-droid`
const (
	HookNameSessionStart     = "session-start"
	HookNameSessionEnd       = "session-end"
	HookNameStop             = "stop"
	HookNameUserPromptSubmit = "user-prompt-submit"
	HookNamePreToolUse       = "pre-tool-use"
	HookNamePostToolUse      = "post-tool-use"
	HookNameSubagentStop     = "subagent-stop"
	HookNamePreCompact       = "pre-compact"
	HookNameNotification     = "notification"
)

// FactorySettingsFileName is the settings file used by Factory AI Droid.
// This is Factory-specific and not shared with other agents.
const FactorySettingsFileName = "settings.json"

// metadataDenyRule blocks Factory Droid from reading Entire session metadata
const metadataDenyRule = "Read(./.entire/metadata/**)"

// InstallHooks installs Factory AI Droid hooks in .factory/settings.json.
// If force is true, removes existing Entire hooks before installing.
// Returns the number of hooks installed.
//
//nolint:maintidx // Hook installation is intentionally centralized here; splitting it further would add churn for a config-assembly path.
func (f *FactoryAIDroidAgent) InstallHooks(ctx context.Context, force bool) (int, error) {
	// Use repo root instead of CWD to find .factory directory
	// This ensures hooks are installed correctly when run from a subdirectory
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		// Fallback to CWD if not in a git repo (e.g., during tests)
		repoRoot, err = os.Getwd() //nolint:forbidigo // Intentional fallback when WorktreeRoot() fails (tests run outside git repos)
		if err != nil {
			return 0, fmt.Errorf("failed to get current directory: %w", err)
		}
	}

	settingsPath := filepath.Join(repoRoot, ".factory", FactorySettingsFileName)

	// Read existing settings if they exist
	var rawSettings map[string]json.RawMessage

	// rawHooks preserves unknown hook types
	var rawHooks map[string]json.RawMessage

	// rawPermissions preserves unknown permission fields (e.g., "ask")
	var rawPermissions map[string]json.RawMessage

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
		if permRaw, ok := rawSettings["permissions"]; ok {
			if err := json.Unmarshal(permRaw, &rawPermissions); err != nil {
				return 0, fmt.Errorf("failed to parse permissions in settings.json: %w", err)
			}
		}
	} else {
		rawSettings = make(map[string]json.RawMessage)
	}

	if rawHooks == nil {
		rawHooks = make(map[string]json.RawMessage)
	}
	if rawPermissions == nil {
		rawPermissions = make(map[string]json.RawMessage)
	}

	// Parse only the hook types we need to modify
	var sessionStart, sessionEnd, stop, userPromptSubmit, preToolUse, postToolUse, preCompact []FactoryHookMatcher
	parseHookType(rawHooks, "SessionStart", &sessionStart)
	parseHookType(rawHooks, "SessionEnd", &sessionEnd)
	parseHookType(rawHooks, "Stop", &stop)
	parseHookType(rawHooks, "UserPromptSubmit", &userPromptSubmit)
	parseHookType(rawHooks, "PreToolUse", &preToolUse)
	parseHookType(rawHooks, "PostToolUse", &postToolUse)
	parseHookType(rawHooks, "PreCompact", &preCompact)

	// If force is true, remove all existing Entire hooks first
	if force {
		sessionStart = removeEntireHooks(sessionStart)
		sessionEnd = removeEntireHooks(sessionEnd)
		stop = removeEntireHooks(stop)
		userPromptSubmit = removeEntireHooks(userPromptSubmit)
		preToolUse = removeEntireHooks(preToolUse)
		postToolUse = removeEntireHooks(postToolUse)
		preCompact = removeEntireHooks(preCompact)
	}

	// Define hook commands
	sessionStartCmd := agent.WrapProductionSilentHookCommand("entire hooks factoryai-droid session-start")
	sessionEndCmd := agent.WrapProductionSilentHookCommand("entire hooks factoryai-droid session-end")
	stopCmd := agent.WrapProductionPlainTextWarningHookCommand("entire hooks factoryai-droid stop", agent.WarningFormatSingleLine)
	userPromptSubmitCmd := agent.WrapProductionSilentHookCommand("entire hooks factoryai-droid user-prompt-submit")
	preTaskCmd := agent.WrapProductionSilentHookCommand("entire hooks factoryai-droid pre-tool-use")
	postTaskCmd := agent.WrapProductionSilentHookCommand("entire hooks factoryai-droid post-tool-use")
	preCompactCmd := agent.WrapProductionSilentHookCommand("entire hooks factoryai-droid pre-compact")

	// Drop Entire hooks left by older versions before adding the current ones,
	// so a stale command (e.g. the removed local-dev launcher) does not survive
	// alongside them. Unconditional: a plain `entire enable` must migrate too.
	staleDropped := false
	drop := func(matchers []FactoryHookMatcher, want ...string) []FactoryHookMatcher {
		out, dropped := dropStaleEntireHooks(matchers, want...)
		if dropped {
			staleDropped = true
		}
		return out
	}
	sessionStart = drop(sessionStart, sessionStartCmd, userPromptSubmitCmd)
	sessionEnd = drop(sessionEnd, sessionEndCmd)
	stop = drop(stop, stopCmd)
	userPromptSubmit = drop(userPromptSubmit, userPromptSubmitCmd)
	preToolUse = drop(preToolUse, preTaskCmd)
	postToolUse = drop(postToolUse, postTaskCmd)
	preCompact = drop(preCompact, preCompactCmd)

	count := 0

	// Add hooks if they don't exist
	if !hookCommandExists(sessionStart, sessionStartCmd) {
		sessionStart = addHookToMatcher(sessionStart, "", sessionStartCmd)
		count++
	}
	// Also install user-prompt-submit on SessionStart to ensure TurnStart fires
	// even when UserPromptSubmit doesn't (e.g., droid exec mode).
	// The user-prompt-submit handler gracefully handles SessionStart's stdin format
	// (userPromptSubmitRaw is a superset of sessionInfoRaw; Prompt defaults to "").
	if !hookCommandExists(sessionStart, userPromptSubmitCmd) {
		sessionStart = addHookToMatcher(sessionStart, "", userPromptSubmitCmd)
		count++
	}
	if !hookCommandExists(sessionEnd, sessionEndCmd) {
		sessionEnd = addHookToMatcher(sessionEnd, "", sessionEndCmd)
		count++
	}
	if !hookCommandExists(stop, stopCmd) {
		stop = addHookToMatcher(stop, "", stopCmd)
		count++
	}
	if !hookCommandExists(userPromptSubmit, userPromptSubmitCmd) {
		userPromptSubmit = addHookToMatcher(userPromptSubmit, "", userPromptSubmitCmd)
		count++
	}
	if !hookCommandExistsWithMatcher(preToolUse, "Task", preTaskCmd) {
		preToolUse = addHookToMatcher(preToolUse, "Task", preTaskCmd)
		count++
	}
	if !hookCommandExistsWithMatcher(postToolUse, "Task", postTaskCmd) {
		postToolUse = addHookToMatcher(postToolUse, "Task", postTaskCmd)
		count++
	}
	if !hookCommandExists(preCompact, preCompactCmd) {
		preCompact = addHookToMatcher(preCompact, "", preCompactCmd)
		count++
	}

	// Add permissions.deny rule if not present
	permissionsChanged := false
	var denyRules []string
	if denyRaw, ok := rawPermissions["deny"]; ok {
		if err := json.Unmarshal(denyRaw, &denyRules); err != nil {
			return 0, fmt.Errorf("failed to parse permissions.deny in settings.json: %w", err)
		}
	}
	if !slices.Contains(denyRules, metadataDenyRule) {
		denyRules = append(denyRules, metadataDenyRule)
		denyJSON, err := json.Marshal(denyRules)
		if err != nil {
			return 0, fmt.Errorf("failed to marshal permissions.deny: %w", err)
		}
		rawPermissions["deny"] = denyJSON
		permissionsChanged = true
	}

	// staleDropped forces a write even when nothing was added: a file holding
	// both a stale and a current hook adds nothing, and returning early here
	// would leave the stale hook on disk.
	if count == 0 && !permissionsChanged && !staleDropped {
		return 0, nil // All hooks and permissions already installed
	}

	// Marshal modified hook types back to rawHooks
	marshalHookType(rawHooks, "SessionStart", sessionStart)
	marshalHookType(rawHooks, "SessionEnd", sessionEnd)
	marshalHookType(rawHooks, "Stop", stop)
	marshalHookType(rawHooks, "UserPromptSubmit", userPromptSubmit)
	marshalHookType(rawHooks, "PreToolUse", preToolUse)
	marshalHookType(rawHooks, "PostToolUse", postToolUse)
	marshalHookType(rawHooks, "PreCompact", preCompact)

	// Marshal hooks and update raw settings
	hooksJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawHooks)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal hooks: %w", err)
	}
	rawSettings["hooks"] = hooksJSON

	// Marshal permissions and update raw settings
	permJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawPermissions)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal permissions: %w", err)
	}
	rawSettings["permissions"] = permJSON

	// Write back to file
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o750); err != nil {
		return 0, fmt.Errorf("failed to create .factory directory: %w", err)
	}

	output, err := jsonutil.MarshalIndentWithNewline(rawSettings, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("failed to marshal settings: %w", err)
	}

	if err := os.WriteFile(settingsPath, output, 0o600); err != nil {
		return 0, fmt.Errorf("failed to write settings.json: %w", err)
	}

	return count, nil
}

// parseHookType parses a specific hook type from rawHooks into the target slice.
// Silently ignores parse errors (leaves target unchanged).
func parseHookType(rawHooks map[string]json.RawMessage, hookType string, target *[]FactoryHookMatcher) {
	if data, ok := rawHooks[hookType]; ok {
		//nolint:errcheck,gosec // Intentionally ignoring parse errors - leave target as nil/empty
		json.Unmarshal(data, target)
	}
}

// marshalHookType marshals a hook type back to rawHooks.
// If the slice is empty, removes the key from rawHooks.
func marshalHookType(rawHooks map[string]json.RawMessage, hookType string, matchers []FactoryHookMatcher) {
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

// UninstallHooks removes Entire hooks from Factory AI Droid settings.
func (f *FactoryAIDroidAgent) UninstallHooks(ctx context.Context) error {
	// Use repo root to find .factory directory when run from a subdirectory
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "." // Fallback to CWD if not in a git repo
	}
	settingsPath := filepath.Join(repoRoot, ".factory", FactorySettingsFileName)
	data, err := os.ReadFile(settingsPath) //nolint:gosec // path is constructed from repo root + fixed path
	if err != nil {
		// An absent file means nothing to uninstall; an unreadable one does not.
		// Collapsing both leaves hooks on disk while reporting success.
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read %s: %w", settingsPath, err)
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
	var sessionStart, sessionEnd, stop, userPromptSubmit, preToolUse, postToolUse, preCompact []FactoryHookMatcher
	parseHookType(rawHooks, "SessionStart", &sessionStart)
	parseHookType(rawHooks, "SessionEnd", &sessionEnd)
	parseHookType(rawHooks, "Stop", &stop)
	parseHookType(rawHooks, "UserPromptSubmit", &userPromptSubmit)
	parseHookType(rawHooks, "PreToolUse", &preToolUse)
	parseHookType(rawHooks, "PostToolUse", &postToolUse)
	parseHookType(rawHooks, "PreCompact", &preCompact)

	// Remove Entire hooks from all hook types
	sessionStart = removeEntireHooks(sessionStart)
	sessionEnd = removeEntireHooks(sessionEnd)
	stop = removeEntireHooks(stop)
	userPromptSubmit = removeEntireHooks(userPromptSubmit)
	preToolUse = removeEntireHooks(preToolUse)
	postToolUse = removeEntireHooks(postToolUse)
	preCompact = removeEntireHooks(preCompact)

	// Marshal modified hook types back to rawHooks
	marshalHookType(rawHooks, "SessionStart", sessionStart)
	marshalHookType(rawHooks, "SessionEnd", sessionEnd)
	marshalHookType(rawHooks, "Stop", stop)
	marshalHookType(rawHooks, "UserPromptSubmit", userPromptSubmit)
	marshalHookType(rawHooks, "PreToolUse", preToolUse)
	marshalHookType(rawHooks, "PostToolUse", postToolUse)
	marshalHookType(rawHooks, "PreCompact", preCompact)

	// Also remove the metadata deny rule from permissions
	var rawPermissions map[string]json.RawMessage
	if permRaw, ok := rawSettings["permissions"]; ok {
		if err := json.Unmarshal(permRaw, &rawPermissions); err != nil {
			// If parsing fails, just skip permissions cleanup
			rawPermissions = nil
		}
	}

	if rawPermissions != nil {
		if denyRaw, ok := rawPermissions["deny"]; ok {
			var denyRules []string
			if err := json.Unmarshal(denyRaw, &denyRules); err == nil {
				// Filter out the metadata deny rule
				filteredRules := make([]string, 0, len(denyRules))
				for _, rule := range denyRules {
					if rule != metadataDenyRule {
						filteredRules = append(filteredRules, rule)
					}
				}
				if len(filteredRules) > 0 {
					denyJSON, err := json.Marshal(filteredRules)
					if err == nil {
						rawPermissions["deny"] = denyJSON
					}
				} else {
					// Remove empty deny array
					delete(rawPermissions, "deny")
				}
			}
		}

		// If permissions is empty, remove it entirely
		if len(rawPermissions) > 0 {
			permJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawPermissions)
			if err == nil {
				rawSettings["permissions"] = permJSON
			}
		} else {
			delete(rawSettings, "permissions")
		}
	}

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
	if err := os.WriteFile(settingsPath, output, 0o600); err != nil {
		return fmt.Errorf("failed to write settings.json: %w", err)
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
func (f *FactoryAIDroidAgent) AreHooksInstalled(ctx context.Context) (bool, error) {
	// Use repo root to find .factory directory when run from a subdirectory
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "." // Fallback to CWD if not in a git repo
	}
	settingsPath := filepath.Join(repoRoot, ".factory", FactorySettingsFileName)
	data, err := os.ReadFile(settingsPath) //nolint:gosec // path is constructed from repo root + fixed path
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		logging.Warn(ctx, "factoryai-droid: failed to read settings file", "path", settingsPath, "err", err)
		return false, fmt.Errorf("read %s: %w", settingsPath, err)
	}

	var settings FactorySettings
	if err := json.Unmarshal(data, &settings); err != nil {
		logging.Warn(ctx, "factoryai-droid: failed to parse settings file", "path", settingsPath, "err", err)
		return false, fmt.Errorf("parse hook config: %w", err)
	}

	// Check for at least one of our hooks (production, wrapped, or local-dev format)
	return hasEntireHook(settings.Hooks.Stop), nil
}

// Helper functions for hook management

func hookCommandExists(matchers []FactoryHookMatcher, command string) bool {
	for _, matcher := range matchers {
		for _, hook := range matcher.Hooks {
			if hook.Command == command {
				return true
			}
		}
	}
	return false
}

func hasEntireHook(matchers []FactoryHookMatcher) bool {
	for _, matcher := range matchers {
		for _, hook := range matcher.Hooks {
			if isEntireHook(hook.Command) {
				return true
			}
		}
	}
	return false
}

func hookCommandExistsWithMatcher(matchers []FactoryHookMatcher, matcherName, command string) bool {
	for _, matcher := range matchers {
		if matcher.Matcher == matcherName {
			for _, hook := range matcher.Hooks {
				if hook.Command == command {
					return true
				}
			}
		}
	}
	return false
}

func addHookToMatcher(matchers []FactoryHookMatcher, matcherName, command string) []FactoryHookMatcher {
	entry := FactoryHookEntry{Type: "command", Command: command}
	for i := range matchers {
		if matchers[i].Matcher == matcherName {
			matchers[i].Hooks = append(matchers[i].Hooks, entry)
			return matchers
		}
	}
	return append(matchers, FactoryHookMatcher{Matcher: matcherName, Hooks: []FactoryHookEntry{entry}})
}

// isEntireHook checks if a command is an Entire hook
func isEntireHook(command string) bool {
	return agent.IsManagedHookCommand(command)
}

// dropStaleEntireHooks removes Entire-owned hooks whose command is not one of
// want, per matcher, pruning matchers left with no hooks. want is a set because
// one hook list can hold several Entire commands (SessionStart carries both
// session-start and user-prompt-submit). See agent.DropStaleManagedHooks for why
// this runs on every install and why the dropped flag matters.
func dropStaleEntireHooks(matchers []FactoryHookMatcher, want ...string) ([]FactoryHookMatcher, bool) {
	result := make([]FactoryHookMatcher, 0, len(matchers))
	dropped := false
	for _, matcher := range matchers {
		kept, d := agent.DropStaleManagedHooks(matcher.Hooks, hookEntryCommand, want)
		if d {
			dropped = true
		}
		if len(kept) > 0 {
			matcher.Hooks = kept
			result = append(result, matcher)
		}
	}
	if !dropped {
		return matchers, false
	}
	return result, true
}

// hookEntryCommand reads the command off a hook entry for the shared helpers.
func hookEntryCommand(e FactoryHookEntry) string { return e.Command }

// removeEntireHooks removes all Entire hooks from a list of matchers (for simple
// hooks like Stop). It is dropStaleEntireHooks with an empty want set: nothing is
// wanted, so every managed hook is stale.
func removeEntireHooks(matchers []FactoryHookMatcher) []FactoryHookMatcher {
	out, _ := dropStaleEntireHooks(matchers)
	return out
}
