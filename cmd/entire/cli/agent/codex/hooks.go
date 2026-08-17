package codex

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

// HooksFileName is the hooks config file used by Codex.
const HooksFileName = "hooks.json"

// InstallHooks installs Codex hooks in .codex/hooks.json.
func (c *CodexAgent) InstallHooks(ctx context.Context, force bool) (int, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot, err = os.Getwd() //nolint:forbidigo // Intentional fallback when WorktreeRoot() fails (tests)
		if err != nil {
			return 0, fmt.Errorf("failed to get current directory: %w", err)
		}
	}

	hooksPath := filepath.Join(repoRoot, ".codex", HooksFileName)

	// Read existing hooks.json if present
	var rawHooks map[string]json.RawMessage
	existingData, readErr := os.ReadFile(hooksPath) //nolint:gosec // path constructed from repo root
	if readErr == nil {
		var hooksFile map[string]json.RawMessage
		if err := json.Unmarshal(existingData, &hooksFile); err != nil {
			return 0, fmt.Errorf("failed to parse existing hooks.json: %w", err)
		}
		if hooksRaw, ok := hooksFile["hooks"]; ok {
			if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
				return 0, fmt.Errorf("failed to parse hooks in hooks.json: %w", err)
			}
		}
	}

	if rawHooks == nil {
		rawHooks = make(map[string]json.RawMessage)
	}

	// Parse event types we manage
	var sessionStart, userPromptSubmit, stop, postToolUse []MatcherGroup
	if err := parseHookType(rawHooks, "SessionStart", &sessionStart); err != nil {
		return 0, err
	}
	if err := parseHookType(rawHooks, "UserPromptSubmit", &userPromptSubmit); err != nil {
		return 0, err
	}
	if err := parseHookType(rawHooks, "Stop", &stop); err != nil {
		return 0, err
	}
	if err := parseHookType(rawHooks, "PostToolUse", &postToolUse); err != nil {
		return 0, err
	}

	if force {
		sessionStart = removeEntireHooks(sessionStart)
		userPromptSubmit = removeEntireHooks(userPromptSubmit)
		stop = removeEntireHooks(stop)
		postToolUse = removeEntireHooks(postToolUse)
	}

	// Build hook commands
	const cmdPrefix = "entire hooks codex "
	useWindowsProductionHooks := agent.UseWindowsProductionHooks(ctx)
	sessionStartCmd := agent.WrapProductionJSONWarningHookCommandForOS(cmdPrefix+"session-start", agent.WarningFormatSingleLine, useWindowsProductionHooks)
	userPromptSubmitCmd := agent.WrapProductionSilentHookCommandForOS(cmdPrefix+"user-prompt-submit", useWindowsProductionHooks)
	stopCmd := agent.WrapProductionSilentHookCommandForOS(cmdPrefix+"stop", useWindowsProductionHooks)
	postToolUseCmd := agent.WrapProductionSilentHookCommandForOS(cmdPrefix+"post-tool-use", useWindowsProductionHooks)

	count := 0

	if updated, changed := syncHookCommand(sessionStart, sessionStartCmd); changed {
		sessionStart = updated
		count++
	}
	if updated, changed := syncHookCommand(userPromptSubmit, userPromptSubmitCmd); changed {
		userPromptSubmit = updated
		count++
	}
	if updated, changed := syncHookCommand(stop, stopCmd); changed {
		stop = updated
		count++
	}
	if updated, changed := syncHookCommand(postToolUse, postToolUseCmd); changed {
		postToolUse = updated
		count++
	}

	if count == 0 {
		return 0, nil
	}

	// Marshal modified types back
	marshalHookType(rawHooks, "SessionStart", sessionStart)
	marshalHookType(rawHooks, "UserPromptSubmit", userPromptSubmit)
	marshalHookType(rawHooks, "Stop", stop)
	marshalHookType(rawHooks, "PostToolUse", postToolUse)

	// Preserve existing top-level keys (e.g., $schema) by reusing the parsed file
	topLevel := make(map[string]json.RawMessage)
	if readErr == nil {
		// Re-parse the original file to preserve all top-level keys
		_ = json.Unmarshal(existingData, &topLevel) //nolint:errcheck // best-effort preservation
	}
	hooksJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawHooks)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal hooks: %w", err)
	}
	topLevel["hooks"] = hooksJSON

	// Write to file
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o750); err != nil {
		return 0, fmt.Errorf("failed to create .codex directory: %w", err)
	}

	output, err := jsonutil.MarshalIndentWithNewline(topLevel, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("failed to marshal hooks.json: %w", err)
	}

	if err := os.WriteFile(hooksPath, output, 0o600); err != nil {
		return 0, fmt.Errorf("failed to write hooks.json: %w", err)
	}

	// No .codex/config.toml is written: hooks are enabled by default in
	// Codex (since 0.124.0), and a TOML file inside Codex's reserved
	// <CODEX_HOME>/agents tree would be rejected by its agent-role scanner
	// at every startup (entireio/cli#842). A leftover config.toml written
	// by an older entire version must be removed manually.
	return count, nil
}

// UninstallHooks removes Entire hooks from Codex hooks.json.
func (c *CodexAgent) UninstallHooks(ctx context.Context) error {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "."
	}

	hooksPath := filepath.Join(repoRoot, ".codex", HooksFileName)
	data, err := os.ReadFile(hooksPath) //nolint:gosec // path constructed from repo root
	if err != nil {
		return nil //nolint:nilerr // No hooks.json means nothing to uninstall
	}

	var topLevel map[string]json.RawMessage
	if err := json.Unmarshal(data, &topLevel); err != nil {
		return fmt.Errorf("failed to parse hooks.json: %w", err)
	}

	var rawHooks map[string]json.RawMessage
	if hooksRaw, ok := topLevel["hooks"]; ok {
		if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
			return fmt.Errorf("failed to parse hooks: %w", err)
		}
	}
	if rawHooks == nil {
		return nil
	}

	var sessionStart, userPromptSubmit, stop, postToolUse []MatcherGroup
	if err := parseHookType(rawHooks, "SessionStart", &sessionStart); err != nil {
		return err
	}
	if err := parseHookType(rawHooks, "UserPromptSubmit", &userPromptSubmit); err != nil {
		return err
	}
	if err := parseHookType(rawHooks, "Stop", &stop); err != nil {
		return err
	}
	if err := parseHookType(rawHooks, "PostToolUse", &postToolUse); err != nil {
		return err
	}

	sessionStart = removeEntireHooks(sessionStart)
	userPromptSubmit = removeEntireHooks(userPromptSubmit)
	stop = removeEntireHooks(stop)
	postToolUse = removeEntireHooks(postToolUse)

	marshalHookType(rawHooks, "SessionStart", sessionStart)
	marshalHookType(rawHooks, "UserPromptSubmit", userPromptSubmit)
	marshalHookType(rawHooks, "Stop", stop)
	marshalHookType(rawHooks, "PostToolUse", postToolUse)

	if len(rawHooks) > 0 {
		hooksJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawHooks)
		if err != nil {
			return fmt.Errorf("failed to marshal hooks: %w", err)
		}
		topLevel["hooks"] = hooksJSON
	} else {
		delete(topLevel, "hooks")
	}

	output, err := jsonutil.MarshalIndentWithNewline(topLevel, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal hooks.json: %w", err)
	}
	if err := os.WriteFile(hooksPath, output, 0o600); err != nil {
		return fmt.Errorf("failed to write hooks.json: %w", err)
	}
	return nil
}

// AreHooksInstalled checks if Entire hooks are installed in Codex hooks.json.
func (c *CodexAgent) AreHooksInstalled(ctx context.Context) bool {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "."
	}

	hooksPath := filepath.Join(repoRoot, ".codex", HooksFileName)
	data, err := os.ReadFile(hooksPath) //nolint:gosec // path constructed from repo root
	if err != nil {
		return false
	}

	var hooksFile HooksFile
	if err := json.Unmarshal(data, &hooksFile); err != nil {
		return false
	}

	return hasEntireHook(hooksFile.Hooks.SessionStart) &&
		hasEntireHook(hooksFile.Hooks.UserPromptSubmit) &&
		hasEntireHook(hooksFile.Hooks.Stop) &&
		hasEntireHook(hooksFile.Hooks.PostToolUse)
}

// --- Helpers ---

func parseHookType(rawHooks map[string]json.RawMessage, hookType string, target *[]MatcherGroup) error {
	if data, ok := rawHooks[hookType]; ok {
		if err := json.Unmarshal(data, target); err != nil {
			return fmt.Errorf("failed to parse %s hooks: %w", hookType, err)
		}
	}
	return nil
}

func marshalHookType(rawHooks map[string]json.RawMessage, hookType string, groups []MatcherGroup) {
	if len(groups) == 0 {
		delete(rawHooks, hookType)
		return
	}
	data, err := jsonutil.MarshalWithNoHTMLEscape(groups)
	if err != nil {
		return
	}
	rawHooks[hookType] = data
}

func hookCommandExists(groups []MatcherGroup, command string) bool {
	for _, group := range groups {
		for _, hook := range group.Hooks {
			if hook.Command == command {
				return true
			}
		}
	}
	return false
}

// syncHookCommand ensures groups contains exactly the given Entire hook command
// and no other Entire-owned entry, reporting whether the config changed.
//
// Stale entries are dropped even when command is already present. Checking
// presence first (as this did before) left a hook written by an older version
// sitting next to the current one, so both fired — for the removed local-dev mode
// that meant a script inside the working tree kept running on every agent turn.
func syncHookCommand(groups []MatcherGroup, command string) ([]MatcherGroup, bool) {
	groups, dropped := dropStaleEntireHooks(groups, command)
	if hookCommandExists(groups, command) {
		return groups, dropped
	}
	return addHook(groups, command), true
}

// dropStaleEntireHooks removes Entire-owned hooks whose command is not one of
// want, per matcher group, pruning groups left with no hooks. See
// agent.DropStaleManagedHooks for why this runs on every install.
func dropStaleEntireHooks(groups []MatcherGroup, want ...string) ([]MatcherGroup, bool) {
	result := make([]MatcherGroup, 0, len(groups))
	dropped := false
	for _, group := range groups {
		kept, d := agent.DropStaleManagedHooks(group.Hooks, hookEntryCommand, want)
		if d {
			dropped = true
		}
		if len(kept) > 0 {
			group.Hooks = kept
			result = append(result, group)
		}
	}
	if !dropped {
		return groups, false
	}
	return result, true
}

// hookEntryCommand reads the command off a hook entry for the shared helpers.
func hookEntryCommand(e HookEntry) string { return e.Command }

func addHook(groups []MatcherGroup, command string) []MatcherGroup {
	entry := HookEntry{
		Type:    "command",
		Command: command,
		Timeout: 30,
	}

	// Add to an existing group with null matcher, or create a new one
	for i, group := range groups {
		if group.Matcher == nil {
			groups[i].Hooks = append(groups[i].Hooks, entry)
			return groups
		}
	}
	return append(groups, MatcherGroup{
		Matcher: nil,
		Hooks:   []HookEntry{entry},
	})
}

func isEntireHook(command string) bool {
	return agent.IsManagedHookCommand(command)
}

func hasEntireHook(groups []MatcherGroup) bool {
	for _, group := range groups {
		for _, hook := range group.Hooks {
			if isEntireHook(hook.Command) {
				return true
			}
		}
	}
	return false
}

func removeEntireHooks(groups []MatcherGroup) []MatcherGroup {
	result := make([]MatcherGroup, 0, len(groups))
	for _, group := range groups {
		filtered := make([]HookEntry, 0, len(group.Hooks))
		for _, hook := range group.Hooks {
			if !isEntireHook(hook.Command) {
				filtered = append(filtered, hook)
			}
		}
		if len(filtered) > 0 {
			group.Hooks = filtered
			result = append(result, group)
		}
	}
	return result
}
