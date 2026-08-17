package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Ensure ClaudeCodeAgent implements HookSupport
var (
	_ agent.HookSupport   = (*ClaudeCodeAgent)(nil)
	_ agent.HookFreshness = (*ClaudeCodeAgent)(nil)
)

// Claude Code hook names - these become subcommands under `entire hooks claude-code`
const (
	HookNameSessionStart     = "session-start"
	HookNameSessionEnd       = "session-end"
	HookNameStop             = "stop"
	HookNameUserPromptSubmit = "user-prompt-submit"
	HookNamePreTask          = "pre-task"
	HookNamePostTask         = "post-task"
	HookNamePostTodo         = "post-todo"
)

// Claude Code tool-name matchers for Entire's PreToolUse/PostToolUse hooks.
//
// The subagent dispatch tool is "Agent" (Claude Code never exposed a tool named
// "Task"), and the "TodoWrite" tool was disabled by default in v2.1.142 in favor
// of the Task* tools. "TaskCreate|TaskUpdate" is a matcher list of exact tool
// names (Claude Code treats a matcher containing only letters/digits/_/-/spaces/
// ,/| as exact strings, not a regex). See:
//   - https://code.claude.com/docs/en/tools-reference.md (Agent, TodoWrite entries)
//   - https://code.claude.com/docs/en/hooks.md (matcher evaluation rules)
//
// Configs written by older CLI versions used the outdated matchers "Task" and
// "TodoWrite", where the hooks silently never fired. Those are not rewritten in
// place on a normal `entire enable`; run with --force to strip and reinstall.
const (
	subagentToolMatcher = "Agent"
	taskToolMatcher     = "TaskCreate|TaskUpdate"
)

// ClaudeSettingsFileName is the settings file used by Claude Code.
// This is Claude-specific and not shared with other agents.
const ClaudeSettingsFileName = "settings.json"

// metadataDenyRule blocks Claude from reading Entire session metadata
const metadataDenyRule = "Read(./.entire/metadata/**)"

// InstallHooks installs Claude Code hooks in .claude/settings.json.
// If force is true, removes existing Entire hooks before installing.
// Returns the number of hooks installed.
func (c *ClaudeCodeAgent) InstallHooks(ctx context.Context, force bool) (int, error) {
	// Use repo root instead of CWD to find .claude directory
	// This ensures hooks are installed correctly when run from a subdirectory
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		// Fallback to CWD if not in a git repo (e.g., during tests)
		repoRoot, err = os.Getwd() //nolint:forbidigo // Intentional fallback when WorktreeRoot() fails (tests run outside git repos)
		if err != nil {
			return 0, fmt.Errorf("failed to get current directory: %w", err)
		}
	}

	settingsPath := filepath.Join(repoRoot, ".claude", ClaudeSettingsFileName)

	// Read existing settings if they exist
	var rawSettings map[string]json.RawMessage

	// rawHooks preserves unknown hook types (e.g., "Notification", "SubagentStop")
	var rawHooks map[string]json.RawMessage

	// rawPermissions preserves unknown permission fields (e.g., "ask")
	var rawPermissions map[string]json.RawMessage

	existingData, readErr := os.ReadFile(settingsPath) //nolint:gosec // path is constructed from repo root + settings file name
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
	var sessionStart, sessionEnd, stop, userPromptSubmit, preToolUse, postToolUse []ClaudeHookMatcher
	parseHookType(rawHooks, "SessionStart", &sessionStart)
	parseHookType(rawHooks, "SessionEnd", &sessionEnd)
	parseHookType(rawHooks, "Stop", &stop)
	parseHookType(rawHooks, "UserPromptSubmit", &userPromptSubmit)
	parseHookType(rawHooks, "PreToolUse", &preToolUse)
	parseHookType(rawHooks, "PostToolUse", &postToolUse)

	// If force is true, remove all existing Entire hooks first
	if force {
		sessionStart = removeEntireHooks(sessionStart)
		sessionEnd = removeEntireHooks(sessionEnd)
		stop = removeEntireHooks(stop)
		userPromptSubmit = removeEntireHooks(userPromptSubmit)
		preToolUse = removeEntireHooksFromMatchers(preToolUse)
		postToolUse = removeEntireHooksFromMatchers(postToolUse)
	}

	// Define hook commands
	sessionStartCmd := agent.WrapProductionJSONWarningHookCommand("entire hooks claude-code session-start", agent.WarningFormatMultiLine)
	sessionEndCmd := agent.WrapProductionSilentHookCommand("entire hooks claude-code session-end")
	stopCmd := agent.WrapProductionSilentHookCommand("entire hooks claude-code stop")
	userPromptSubmitCmd := agent.WrapProductionSilentHookCommand("entire hooks claude-code user-prompt-submit")
	preTaskCmd := agent.WrapProductionSilentHookCommand("entire hooks claude-code pre-task")
	postTaskCmd := agent.WrapProductionSilentHookCommand("entire hooks claude-code post-task")
	postTodoCmd := agent.WrapProductionSilentHookCommand("entire hooks claude-code post-todo")

	count := 0

	// Drop Entire hooks left by older versions before adding the current ones,
	// so a stale command (e.g. the removed local-dev launcher, which ran a
	// script inside the working tree) does not survive alongside them.
	// Unconditional: a plain `entire enable` must migrate too, not just --force.
	staleDropped := false
	drop := func(matchers []ClaudeHookMatcher, want ...string) []ClaudeHookMatcher {
		out, dropped := dropStaleEntireHooks(matchers, want...)
		if dropped {
			staleDropped = true
		}
		return out
	}
	sessionStart = drop(sessionStart, sessionStartCmd)
	sessionEnd = drop(sessionEnd, sessionEndCmd)
	stop = drop(stop, stopCmd)
	userPromptSubmit = drop(userPromptSubmit, userPromptSubmitCmd)
	preToolUse = drop(preToolUse, preTaskCmd)
	postToolUse = drop(postToolUse, postTaskCmd, postTodoCmd)

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
	if !hookCommandExistsWithMatcher(preToolUse, subagentToolMatcher, preTaskCmd) {
		preToolUse = addHookToMatcher(preToolUse, subagentToolMatcher, preTaskCmd)
		count++
	}
	if !hookCommandExistsWithMatcher(postToolUse, subagentToolMatcher, postTaskCmd) {
		postToolUse = addHookToMatcher(postToolUse, subagentToolMatcher, postTaskCmd)
		count++
	}
	if !hookCommandExistsWithMatcher(postToolUse, taskToolMatcher, postTodoCmd) {
		postToolUse = addHookToMatcher(postToolUse, taskToolMatcher, postTodoCmd)
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
		return 0, fmt.Errorf("failed to create .claude directory: %w", err)
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
func parseHookType(rawHooks map[string]json.RawMessage, hookType string, target *[]ClaudeHookMatcher) {
	if data, ok := rawHooks[hookType]; ok {
		//nolint:errcheck,gosec // Intentionally ignoring parse errors - leave target as nil/empty
		json.Unmarshal(data, target)
	}
}

// marshalHookType marshals a hook type back to rawHooks.
// If the slice is empty, removes the key from rawHooks.
func marshalHookType(rawHooks map[string]json.RawMessage, hookType string, matchers []ClaudeHookMatcher) {
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

// UninstallHooks removes Entire hooks from Claude Code settings.
func (c *ClaudeCodeAgent) UninstallHooks(ctx context.Context) error {
	// Use repo root to find .claude directory when run from a subdirectory
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "." // Fallback to CWD if not in a git repo
	}
	settingsPath := filepath.Join(repoRoot, ".claude", ClaudeSettingsFileName)
	data, err := os.ReadFile(settingsPath) //nolint:gosec // path is constructed from repo root + fixed path
	if err != nil {
		return nil //nolint:nilerr // No settings file means nothing to uninstall
	}

	var rawSettings map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawSettings); err != nil {
		return fmt.Errorf("failed to parse settings.json: %w", err)
	}

	// rawHooks preserves unknown hook types (e.g., "Notification", "SubagentStop")
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
	var sessionStart, sessionEnd, stop, userPromptSubmit, preToolUse, postToolUse []ClaudeHookMatcher
	parseHookType(rawHooks, "SessionStart", &sessionStart)
	parseHookType(rawHooks, "SessionEnd", &sessionEnd)
	parseHookType(rawHooks, "Stop", &stop)
	parseHookType(rawHooks, "UserPromptSubmit", &userPromptSubmit)
	parseHookType(rawHooks, "PreToolUse", &preToolUse)
	parseHookType(rawHooks, "PostToolUse", &postToolUse)

	// Remove Entire hooks from all hook types
	sessionStart = removeEntireHooks(sessionStart)
	sessionEnd = removeEntireHooks(sessionEnd)
	stop = removeEntireHooks(stop)
	userPromptSubmit = removeEntireHooks(userPromptSubmit)
	preToolUse = removeEntireHooksFromMatchers(preToolUse)
	postToolUse = removeEntireHooksFromMatchers(postToolUse)

	// Marshal modified hook types back to rawHooks
	marshalHookType(rawHooks, "SessionStart", sessionStart)
	marshalHookType(rawHooks, "SessionEnd", sessionEnd)
	marshalHookType(rawHooks, "Stop", stop)
	marshalHookType(rawHooks, "UserPromptSubmit", userPromptSubmit)
	marshalHookType(rawHooks, "PreToolUse", preToolUse)
	marshalHookType(rawHooks, "PostToolUse", postToolUse)

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

// loadClaudeSettings reads and parses .claude/settings.json from the repo root.
// Returns ok=false when the file is missing or unparseable.
func loadClaudeSettings(ctx context.Context) (ClaudeSettings, bool) {
	// Use repo root to find .claude directory when run from a subdirectory
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "." // Fallback to CWD if not in a git repo
	}
	settingsPath := filepath.Join(repoRoot, ".claude", ClaudeSettingsFileName)
	data, err := os.ReadFile(settingsPath) //nolint:gosec // path is constructed from repo root + fixed path
	if err != nil {
		return ClaudeSettings{}, false
	}

	var settings ClaudeSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return ClaudeSettings{}, false
	}
	return settings, true
}

// AreHooksInstalled checks if Entire hooks are installed.
func (c *ClaudeCodeAgent) AreHooksInstalled(ctx context.Context) bool {
	settings, ok := loadClaudeSettings(ctx)
	if !ok {
		return false
	}
	// Check for at least one of our hooks (new, wrapped, or legacy format)
	return hasEntireHook(settings.Hooks.Stop)
}

// HookConfigState describes how Entire's Claude Code hooks compare to what
// InstallHooks would write today. Aliased to the shared agent-package type so
// `entire status` and `entire doctor` can treat every agent's drift check
// uniformly; the names stay exported here for existing call sites.
//
// For Claude Code, HooksOutdated means Entire hooks are installed but the
// current tool-use matchers no longer carry them (e.g. an older CLI wrote them
// under the now non-firing "Task"/"TodoWrite" matchers).
type HookConfigState = agent.HookConfigState

const (
	// HooksAbsent means Entire hooks are not installed in this repo.
	HooksAbsent = agent.HooksAbsent
	// HooksCurrent means the installed hooks match the current config.
	HooksCurrent = agent.HooksCurrent
	// HooksOutdated means the installed hooks are stale.
	// Fix: `entire enable --force`.
	HooksOutdated = agent.HooksOutdated
)

// CheckHookConfig satisfies agent.HookFreshness by delegating to the
// package-level check, which predates the interface and is still called
// directly by tests.
func (c *ClaudeCodeAgent) CheckHookConfig(ctx context.Context) agent.HookConfigState {
	return CheckHookConfig(ctx)
}

// CheckHookConfig reports whether Entire's Claude Code hooks are absent,
// current, or outdated. It is a read-only diagnostic used by `entire status`
// and `entire doctor`; it never modifies settings. Outdated is detected on the
// positive spec: Entire is installed (Stop hook present) yet one of the current
// tool-use matchers does not carry its Entire hook.
func CheckHookConfig(ctx context.Context) HookConfigState {
	settings, ok := loadClaudeSettings(ctx)
	if !ok || !hasEntireHook(settings.Hooks.Stop) {
		return HooksAbsent
	}
	subagentTools := splitMatcherTools(subagentToolMatcher)
	taskTools := splitMatcherTools(taskToolMatcher)
	if !hasEntireHookCoveringTools(settings.Hooks.PreToolUse, subagentTools) ||
		!hasEntireHookCoveringTools(settings.Hooks.PostToolUse, subagentTools) ||
		!hasEntireHookCoveringTools(settings.Hooks.PostToolUse, taskTools) {
		return HooksOutdated
	}
	return HooksCurrent
}

// Helper functions for hook management

func hookCommandExists(matchers []ClaudeHookMatcher, command string) bool {
	for _, matcher := range matchers {
		for _, hook := range matcher.Hooks {
			if hook.Command == command {
				return true
			}
		}
	}
	return false
}

func hasEntireHook(matchers []ClaudeHookMatcher) bool {
	for _, matcher := range matchers {
		for _, hook := range matcher.Hooks {
			if isEntireHook(hook.Command) {
				return true
			}
		}
	}
	return false
}

// splitMatcherTools splits a Claude Code tool matcher into its exact tool
// names. Matchers that InstallHooks writes are `|`-separated lists (Claude Code
// also accepts `,`); whitespace around separators is ignored. Returns the tools
// in order, dropping empties.
func splitMatcherTools(matcher string) []string {
	parts := strings.FieldsFunc(matcher, func(r rune) bool { return r == '|' || r == ',' })
	tools := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			tools = append(tools, t)
		}
	}
	return tools
}

// hasEntireHookCoveringTools reports whether an Entire hook is installed under a
// matcher that covers every tool in want. A widened matcher still counts: a
// matcher of "TaskCreate|TaskUpdate|TaskGet" covers {TaskCreate, TaskUpdate},
// so users who broaden a matcher aren't falsely flagged as outdated.
func hasEntireHookCoveringTools(matchers []ClaudeHookMatcher, want []string) bool {
	for _, matcher := range matchers {
		have := splitMatcherTools(matcher.Matcher)
		coversAll := true
		for _, w := range want {
			if !slices.Contains(have, w) {
				coversAll = false
				break
			}
		}
		if !coversAll {
			continue
		}
		for _, hook := range matcher.Hooks {
			if isEntireHook(hook.Command) {
				return true
			}
		}
	}
	return false
}

func hookCommandExistsWithMatcher(matchers []ClaudeHookMatcher, matcherName, command string) bool {
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

// addHookToMatcher appends command to the matcher named matcherName, creating it
// if absent. No timeout is set: Timeout exists on ClaudeHookEntry to round-trip
// settings files that carry one, but Entire's own hooks all keep Claude Code's
// default. (The removed local-dev mode was the only thing that needed a longer
// SessionEnd budget, because it compiled the CLI from source.)
func addHookToMatcher(matchers []ClaudeHookMatcher, matcherName, command string) []ClaudeHookMatcher {
	entry := ClaudeHookEntry{
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
		return append(matchers, ClaudeHookMatcher{
			Matcher: "",
			Hooks:   []ClaudeHookEntry{entry},
		})
	}

	// Find or create matcher with the given name
	for i, matcher := range matchers {
		if matcher.Matcher == matcherName {
			matchers[i].Hooks = append(matchers[i].Hooks, entry)
			return matchers
		}
	}

	return append(matchers, ClaudeHookMatcher{
		Matcher: matcherName,
		Hooks:   []ClaudeHookEntry{entry},
	})
}

// isEntireHook checks if a command is an Entire hook (old or new format)
func isEntireHook(command string) bool {
	return agent.IsManagedHookCommand(command)
}

// dropStaleEntireHooks removes Entire-owned hooks whose command is not one of
// want, per matcher, pruning matchers left with no hooks. want is a set because
// one hook list can hold several Entire commands (PostToolUse carries both
// post-task and post-todo). See agent.DropStaleManagedHooks for why this runs on
// every install and why the dropped flag matters.
func dropStaleEntireHooks(matchers []ClaudeHookMatcher, want ...string) ([]ClaudeHookMatcher, bool) {
	result := make([]ClaudeHookMatcher, 0, len(matchers))
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
func hookEntryCommand(e ClaudeHookEntry) string { return e.Command }

// removeEntireHooks removes all Entire hooks from a list of matchers (for simple
// hooks like Stop). It is dropStaleEntireHooks with an empty want set: nothing is
// wanted, so every managed hook is stale.
func removeEntireHooks(matchers []ClaudeHookMatcher) []ClaudeHookMatcher {
	out, _ := dropStaleEntireHooks(matchers)
	return out
}

// removeEntireHooksFromMatchers removes Entire hooks from tool-use matchers (PreToolUse, PostToolUse)
// This handles the nested structure where hooks are grouped by tool matcher (e.g., "Agent", "TaskCreate|TaskUpdate")
func removeEntireHooksFromMatchers(matchers []ClaudeHookMatcher) []ClaudeHookMatcher {
	// Same logic as removeEntireHooks - both work on the same structure
	return removeEntireHooks(matchers)
}
