package devin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// HooksFileName is the standalone hooks file used by Devin CLI. Unlike
// .claude/settings.json, the hooks object is the entire file — event names
// are top-level keys with no "hooks" wrapper.
const HooksFileName = "hooks.v1.json"

// entireHookPrefixes are command prefixes that identify Entire hooks.
var entireHookPrefixes = []string{
	"entire ",
	agent.LocalDevHookScript + " ",
}

// localDevHookCommand builds a local-dev hook command for the given hook
// name, using the shared git-based launcher script (the pattern for agents
// that locate the repo root with `git rev-parse` instead of a
// ${CLAUDE_PROJECT_DIR}-style variable).
func localDevHookCommand(hookName string) string {
	return fmt.Sprintf("%s hooks devin %s", agent.LocalDevHookScript, hookName)
}

// hooksFilePath returns the absolute path of .devin/hooks.v1.json for the repo.
func hooksFilePath(ctx context.Context) (string, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		// Fallback to CWD if not in a git repo (e.g., during tests)
		repoRoot, err = os.Getwd() //nolint:forbidigo // Intentional fallback when WorktreeRoot() fails
		if err != nil {
			return "", fmt.Errorf("failed to get current directory: %w", err)
		}
	}
	return filepath.Join(repoRoot, ".devin", HooksFileName), nil
}

// InstallHooks installs Devin hooks in .devin/hooks.v1.json.
// If force is true, removes existing Entire hooks before installing.
// Returns the number of hooks installed.
func (d *DevinAgent) InstallHooks(ctx context.Context, localDev bool, force bool) (int, error) {
	hooksPath, err := hooksFilePath(ctx)
	if err != nil {
		return 0, err
	}

	// The whole file is the hooks object; use a raw map to preserve unknown
	// event types on round-trip.
	rawHooks := make(map[string]json.RawMessage)
	if existingData, readErr := os.ReadFile(hooksPath); readErr == nil { //nolint:gosec // path is constructed from repo root + fixed path
		if err := json.Unmarshal(existingData, &rawHooks); err != nil {
			return 0, fmt.Errorf("failed to parse existing %s: %w", HooksFileName, err)
		}
	}

	// Parse only the hook types we manage
	var sessionStart, sessionEnd, stop, userPromptSubmit, postToolUse []HookMatcher
	parseHookType(rawHooks, "SessionStart", &sessionStart)
	parseHookType(rawHooks, "SessionEnd", &sessionEnd)
	parseHookType(rawHooks, "Stop", &stop)
	parseHookType(rawHooks, "UserPromptSubmit", &userPromptSubmit)
	parseHookType(rawHooks, "PostToolUse", &postToolUse)

	// If force is true, remove all existing Entire hooks first
	if force {
		sessionStart = removeEntireHooks(sessionStart)
		sessionEnd = removeEntireHooks(sessionEnd)
		stop = removeEntireHooks(stop)
		userPromptSubmit = removeEntireHooks(userPromptSubmit)
		postToolUse = removeEntireHooksFromMatchers(postToolUse)
	}

	// Define hook commands. Devin ships a native Windows CLI; pick the
	// cmd.exe-based wrapper when a working POSIX sh is not available
	// (shared probe, codex pattern).
	useWindows := agent.UseWindowsProductionHooks(ctx, localDev)
	var sessionStartCmd, sessionEndCmd, stopCmd, userPromptSubmitCmd, postToolUseCmd string
	if localDev {
		sessionStartCmd = localDevHookCommand(HookNameSessionStart)
		sessionEndCmd = localDevHookCommand(HookNameSessionEnd)
		stopCmd = localDevHookCommand(HookNameStop)
		userPromptSubmitCmd = localDevHookCommand(HookNameUserPromptSubmit)
		postToolUseCmd = localDevHookCommand(HookNamePostToolUse)
	} else {
		sessionStartCmd = agent.WrapProductionSilentHookCommandForOS("entire hooks devin "+HookNameSessionStart, useWindows)
		sessionEndCmd = agent.WrapProductionSilentHookCommandForOS("entire hooks devin "+HookNameSessionEnd, useWindows)
		stopCmd = agent.WrapProductionSilentHookCommandForOS("entire hooks devin "+HookNameStop, useWindows)
		userPromptSubmitCmd = agent.WrapProductionSilentHookCommandForOS("entire hooks devin "+HookNameUserPromptSubmit, useWindows)
		postToolUseCmd = agent.WrapProductionSilentHookCommandForOS("entire hooks devin "+HookNamePostToolUse, useWindows)
	}

	count := 0

	// Add hooks if they don't exist
	if !hookCommandExists(sessionStart, sessionStartCmd) {
		sessionStart = addHookToMatcher(sessionStart, "", sessionStartCmd)
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
	if !hookCommandExistsWithMatcher(postToolUse, fileModificationToolsMatcher, postToolUseCmd) {
		postToolUse = addHookToMatcher(postToolUse, fileModificationToolsMatcher, postToolUseCmd)
		count++
	}

	if count == 0 {
		return 0, nil // All hooks already installed
	}

	// Marshal modified hook types back to rawHooks
	marshalHookType(rawHooks, "SessionStart", sessionStart)
	marshalHookType(rawHooks, "SessionEnd", sessionEnd)
	marshalHookType(rawHooks, "Stop", stop)
	marshalHookType(rawHooks, "UserPromptSubmit", userPromptSubmit)
	marshalHookType(rawHooks, "PostToolUse", postToolUse)

	// Write back to file
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o750); err != nil {
		return 0, fmt.Errorf("failed to create .devin directory: %w", err)
	}

	output, err := jsonutil.MarshalIndentWithNewline(rawHooks, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("failed to marshal hooks: %w", err)
	}

	if err := os.WriteFile(hooksPath, output, 0o600); err != nil {
		return 0, fmt.Errorf("failed to write %s: %w", HooksFileName, err)
	}

	return count, nil
}

// UninstallHooks removes Entire hooks from .devin/hooks.v1.json.
func (d *DevinAgent) UninstallHooks(ctx context.Context) error {
	hooksPath, err := hooksFilePath(ctx)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(hooksPath) //nolint:gosec // path is constructed from repo root + fixed path
	if err != nil {
		return nil //nolint:nilerr // No hooks file means nothing to uninstall
	}

	rawHooks := make(map[string]json.RawMessage)
	if err := json.Unmarshal(data, &rawHooks); err != nil {
		return fmt.Errorf("failed to parse %s: %w", HooksFileName, err)
	}

	// Parse only the hook types we need to modify
	var sessionStart, sessionEnd, stop, userPromptSubmit, postToolUse []HookMatcher
	parseHookType(rawHooks, "SessionStart", &sessionStart)
	parseHookType(rawHooks, "SessionEnd", &sessionEnd)
	parseHookType(rawHooks, "Stop", &stop)
	parseHookType(rawHooks, "UserPromptSubmit", &userPromptSubmit)
	parseHookType(rawHooks, "PostToolUse", &postToolUse)

	// Remove Entire hooks from all hook types
	sessionStart = removeEntireHooks(sessionStart)
	sessionEnd = removeEntireHooks(sessionEnd)
	stop = removeEntireHooks(stop)
	userPromptSubmit = removeEntireHooks(userPromptSubmit)
	postToolUse = removeEntireHooksFromMatchers(postToolUse)

	// Marshal modified hook types back to rawHooks
	marshalHookType(rawHooks, "SessionStart", sessionStart)
	marshalHookType(rawHooks, "SessionEnd", sessionEnd)
	marshalHookType(rawHooks, "Stop", stop)
	marshalHookType(rawHooks, "UserPromptSubmit", userPromptSubmit)
	marshalHookType(rawHooks, "PostToolUse", postToolUse)

	output, err := jsonutil.MarshalIndentWithNewline(rawHooks, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal hooks: %w", err)
	}
	if err := os.WriteFile(hooksPath, output, 0o600); err != nil {
		return fmt.Errorf("failed to write %s: %w", HooksFileName, err)
	}
	return nil
}

// AreHooksInstalled checks if Entire hooks are installed.
func (d *DevinAgent) AreHooksInstalled(ctx context.Context) bool {
	hooksPath, err := hooksFilePath(ctx)
	if err != nil {
		return false
	}
	data, err := os.ReadFile(hooksPath) //nolint:gosec // path is constructed from repo root + fixed path
	if err != nil {
		return false
	}

	rawHooks := make(map[string]json.RawMessage)
	if err := json.Unmarshal(data, &rawHooks); err != nil {
		return false
	}

	// Check for at least one of our hooks
	var stop []HookMatcher
	parseHookType(rawHooks, "Stop", &stop)
	return hasEntireHook(stop)
}

// Helper functions for hook management

// parseHookType parses a specific hook type from rawHooks into the target slice.
// Silently ignores parse errors (leaves target unchanged).
func parseHookType(rawHooks map[string]json.RawMessage, hookType string, target *[]HookMatcher) {
	if data, ok := rawHooks[hookType]; ok {
		//nolint:errcheck,gosec // Intentionally ignoring parse errors - leave target as nil/empty
		json.Unmarshal(data, target)
	}
}

// marshalHookType marshals a hook type back to rawHooks.
// If the slice is empty, removes the key from rawHooks.
func marshalHookType(rawHooks map[string]json.RawMessage, hookType string, matchers []HookMatcher) {
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

func hookCommandExists(matchers []HookMatcher, command string) bool {
	for _, matcher := range matchers {
		for _, hook := range matcher.Hooks {
			if hook.Command == command {
				return true
			}
		}
	}
	return false
}

func hasEntireHook(matchers []HookMatcher) bool {
	for _, matcher := range matchers {
		for _, hook := range matcher.Hooks {
			if isEntireHook(hook.Command) {
				return true
			}
		}
	}
	return false
}

func hookCommandExistsWithMatcher(matchers []HookMatcher, matcherName, command string) bool {
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

func addHookToMatcher(matchers []HookMatcher, matcherName, command string) []HookMatcher {
	entry := HookEntry{
		Type:    "command",
		Command: command,
	}

	// If no matcher name, add to a matcher with empty string
	if matcherName == "" {
		for i, matcher := range matchers {
			if matcher.Matcher == "" {
				matchers[i].Hooks = append(matchers[i].Hooks, entry)
				return matchers
			}
		}
		return append(matchers, HookMatcher{
			Matcher: "",
			Hooks:   []HookEntry{entry},
		})
	}

	// Find or create matcher with the given name
	for i, matcher := range matchers {
		if matcher.Matcher == matcherName {
			matchers[i].Hooks = append(matchers[i].Hooks, entry)
			return matchers
		}
	}

	return append(matchers, HookMatcher{
		Matcher: matcherName,
		Hooks:   []HookEntry{entry},
	})
}

// isEntireHook checks if a command is an Entire hook (direct or wrapped form)
func isEntireHook(command string) bool {
	return agent.IsManagedHookCommand(command, entireHookPrefixes)
}

// removeEntireHooks removes all Entire hooks from a list of matchers (for simple hooks like Stop)
func removeEntireHooks(matchers []HookMatcher) []HookMatcher {
	result := make([]HookMatcher, 0, len(matchers))
	for _, matcher := range matchers {
		filteredHooks := make([]HookEntry, 0, len(matcher.Hooks))
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

// removeEntireHooksFromMatchers removes Entire hooks from tool-use matchers (PostToolUse)
// This handles the nested structure where hooks are grouped by tool matcher.
func removeEntireHooksFromMatchers(matchers []HookMatcher) []HookMatcher {
	// Same logic as removeEntireHooks - both work on the same structure
	return removeEntireHooks(matchers)
}
