package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
	HookNameSubagentStop     = "subagent-stop"
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

// entireClaudeHookCount is the number of hook entries a full install writes
// (SessionStart, SessionEnd, Stop, SubagentStop, UserPromptSubmit, PreToolUse[Agent],
// PostToolUse[Agent], PostToolUse[TaskCreate|TaskUpdate]).
const entireClaudeHookCount = 8

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
	count, _, err := installHooksToFile(settingsPath, force, true)
	return count, err
}

// readClaudeRawSettings reads and shallow-parses the settings file for a
// hook install. projectScope controls whether the permissions section is
// parsed: a user-scope install must neither parse nor fail on it — whatever
// value is there (even a non-object) round-trips verbatim. Only a genuinely
// missing file means "start fresh"; any other read failure (permissions, I/O)
// aborts, because proceeding would replace the user's whole settings file
// with an Entire-only one.
func readClaudeRawSettings(settingsPath string, projectScope bool) (rawSettings, rawHooks, rawPermissions map[string]json.RawMessage, err error) {
	existingData, readErr := os.ReadFile(settingsPath) //nolint:gosec // path is constructed from repo root + settings file name
	switch {
	case readErr == nil:
		if err := json.Unmarshal(existingData, &rawSettings); err != nil {
			return nil, nil, nil, fmt.Errorf("failed to parse existing %s: %w", settingsPath, err)
		}
		if hooksRaw, ok := rawSettings["hooks"]; ok {
			if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
				return nil, nil, nil, fmt.Errorf("failed to parse hooks in %s: %w", settingsPath, err)
			}
		}
		if permRaw, ok := rawSettings["permissions"]; ok && projectScope {
			if err := json.Unmarshal(permRaw, &rawPermissions); err != nil {
				return nil, nil, nil, fmt.Errorf("failed to parse permissions in %s: %w", settingsPath, err)
			}
		}
	case errors.Is(readErr, fs.ErrNotExist):
		rawSettings = make(map[string]json.RawMessage)
	default:
		return nil, nil, nil, fmt.Errorf("failed to read %s: %w", settingsPath, readErr)
	}
	if rawHooks == nil {
		rawHooks = make(map[string]json.RawMessage)
	}
	if rawPermissions == nil {
		rawPermissions = make(map[string]json.RawMessage)
	}
	return rawSettings, rawHooks, rawPermissions, nil
}

// claudeHookCommands is the full set of hook commands one install writes.
type claudeHookCommands struct {
	sessionStart, sessionEnd, stop, subagentStop, userPromptSubmit, preTask, postTask, postTodo string
}

func buildClaudeHookCommands() claudeHookCommands {
	return claudeHookCommands{
		sessionStart:     agent.WrapProductionJSONWarningHookCommand("entire hooks claude-code session-start", agent.WarningFormatMultiLine),
		sessionEnd:       agent.WrapProductionSilentHookCommand("entire hooks claude-code session-end"),
		stop:             agent.WrapProductionSilentHookCommand("entire hooks claude-code stop"),
		subagentStop:     agent.WrapProductionSilentHookCommand("entire hooks claude-code subagent-stop"),
		userPromptSubmit: agent.WrapProductionSilentHookCommand("entire hooks claude-code user-prompt-submit"),
		preTask:          agent.WrapProductionSilentHookCommand("entire hooks claude-code pre-task"),
		postTask:         agent.WrapProductionSilentHookCommand("entire hooks claude-code post-task"),
		postTodo:         agent.WrapProductionSilentHookCommand("entire hooks claude-code post-todo"),
	}
}

// ensureMetadataDenyRule adds the repo-scoped metadata deny rule to
// rawPermissions when absent, reporting whether it changed anything.
func ensureMetadataDenyRule(rawPermissions map[string]json.RawMessage, settingsPath string) (bool, error) {
	var denyRules []string
	if denyRaw, ok := rawPermissions["deny"]; ok {
		if err := json.Unmarshal(denyRaw, &denyRules); err != nil {
			return false, fmt.Errorf("failed to parse permissions.deny in %s: %w", settingsPath, err)
		}
	}
	if slices.Contains(denyRules, metadataDenyRule) {
		return false, nil
	}
	denyRules = append(denyRules, metadataDenyRule)
	denyJSON, err := json.Marshal(denyRules)
	if err != nil {
		return false, fmt.Errorf("failed to marshal permissions.deny: %w", err)
	}
	rawPermissions["deny"] = denyJSON
	return true, nil
}

// installHooksToFile installs Entire's Claude Code hooks into the settings
// file at settingsPath. projectScope additionally maintains the repo-scoped
// permissions.deny rule; the user-level install (InstallUserHooks) passes
// false so it only ever touches the hooks section of ~/.claude/settings.json.
// repaired reports a user-scope rewrite that normalized pre-existing Entire
// entries (rather than a pure add or a no-op), so the caller can report the
// repair instead of "already installed".
func installHooksToFile(settingsPath string, force, projectScope bool) (count int, repaired bool, err error) {
	rawSettings, rawHooks, rawPermissions, err := readClaudeRawSettings(settingsPath, projectScope)
	if err != nil {
		return 0, false, err
	}

	// Parse only the hook types we need to modify
	var sessionStart, sessionEnd, stop, subagentStop, userPromptSubmit, preToolUse, postToolUse []ClaudeHookMatcher
	if err := parseHookSections(rawHooks, settingsPath, map[string]*[]ClaudeHookMatcher{
		"SessionStart":     &sessionStart,
		"SessionEnd":       &sessionEnd,
		"Stop":             &stop,
		"SubagentStop":     &subagentStop,
		"UserPromptSubmit": &userPromptSubmit,
		"PreToolUse":       &preToolUse,
		"PostToolUse":      &postToolUse,
	}); err != nil {
		return 0, false, err
	}

	// Presence checks run against these pre-removal snapshots so a user-scope
	// repair pass (below) does not defeat the idempotency accounting.
	checkSessionStart, checkSessionEnd, checkStop := sessionStart, sessionEnd, stop
	checkSubagentStop := subagentStop
	checkUserPromptSubmit, checkPreToolUse, checkPostToolUse := userPromptSubmit, preToolUse, postToolUse

	// force removes all existing Entire hooks before reinstalling (mode
	// switch). The user-scope install ALWAYS runs this remove-ours-then-re-add
	// pass: hookCommandExists is an exact-string compare, so a pre-existing
	// Entire hook in a different command form (bare, legacy go-run) would
	// otherwise gain a second entry and double-fire machine-wide. removedOurs
	// counts the removed entries so an already-correct file is left untouched.
	userScope := !projectScope
	removedOurs := 0
	if force || userScope {
		strip := func(matchers []ClaudeHookMatcher) []ClaudeHookMatcher {
			out, n := removeEntireHooksCounting(matchers)
			removedOurs += n
			return out
		}
		sessionStart = strip(sessionStart)
		sessionEnd = strip(sessionEnd)
		stop = strip(stop)
		subagentStop = strip(subagentStop)
		userPromptSubmit = strip(userPromptSubmit)
		preToolUse = strip(preToolUse)
		postToolUse = strip(postToolUse)
	}

	cmds := buildClaudeHookCommands()

	// Drop Entire hooks left by older versions before adding the current ones,
	// so a stale command (e.g. the removed local-dev launcher, which ran a
	// script inside the working tree) does not survive alongside them.
	// Unconditional: a plain `entire enable` must migrate too, not just --force.
	staleDropped := false
	if !force && !userScope {
		// Only meaningful when the strip pass above did not run: after it,
		// every Entire entry is already gone from these slices.
		drop := func(matchers []ClaudeHookMatcher, want ...string) []ClaudeHookMatcher {
			out, dropped := dropStaleEntireHooks(matchers, want...)
			if dropped {
				staleDropped = true
			}
			return out
		}
		sessionStart = drop(sessionStart, cmds.sessionStart)
		sessionEnd = drop(sessionEnd, cmds.sessionEnd)
		stop = drop(stop, cmds.stop)
		subagentStop = drop(subagentStop, cmds.subagentStop)
		userPromptSubmit = drop(userPromptSubmit, cmds.userPromptSubmit)
		preToolUse = drop(preToolUse, cmds.preTask)
		postToolUse = drop(postToolUse, cmds.postTask, cmds.postTodo)
	}

	// ensureHook counts a hook as newly installed when its exact command was
	// absent from the pre-removal snapshot (force counts everything: it always
	// reinstalls), and adds the entry unless a plain repo-scope install found
	// it already present. After a removal pass (force or user scope) every
	// Entire entry is gone from the working slices, so the add must not be
	// gated on the presence check. Sharing one snapshot across calls cannot
	// overcount: every call checks a distinct (matcher, command) pair — the
	// two PostToolUse entries included.
	addAll := force || userScope
	ensureHook := func(matchers, check []ClaudeHookMatcher, matcherName, cmd string) []ClaudeHookMatcher {
		var present bool
		if !force {
			if matcherName == "" {
				present = hookCommandExists(check, cmd)
			} else {
				present = hookCommandExistsWithMatcher(check, matcherName, cmd)
			}
		}
		if !present {
			count++
		}
		if present && !addAll {
			return matchers
		}
		return addHookToMatcher(matchers, matcherName, cmd)
	}

	sessionStart = ensureHook(sessionStart, checkSessionStart, "", cmds.sessionStart)
	sessionEnd = ensureHook(sessionEnd, checkSessionEnd, "", cmds.sessionEnd)
	stop = ensureHook(stop, checkStop, "", cmds.stop)
	subagentStop = ensureHook(subagentStop, checkSubagentStop, "", cmds.subagentStop)
	userPromptSubmit = ensureHook(userPromptSubmit, checkUserPromptSubmit, "", cmds.userPromptSubmit)
	preToolUse = ensureHook(preToolUse, checkPreToolUse, subagentToolMatcher, cmds.preTask)
	postToolUse = ensureHook(postToolUse, checkPostToolUse, subagentToolMatcher, cmds.postTask)
	postToolUse = ensureHook(postToolUse, checkPostToolUse, taskToolMatcher, cmds.postTodo)

	// Add permissions.deny rule if not present (repo scope only: the rule is
	// repo-relative and user-level installs must not modify user permissions).
	permissionsChanged := false
	if projectScope {
		changed, err := ensureMetadataDenyRule(rawPermissions, settingsPath)
		if err != nil {
			return 0, false, err
		}
		permissionsChanged = changed
	}

	// All hooks and permissions already installed. Two repair signals also
	// force a write: staleDropped (a stale command was pruned, so returning
	// early would leave it on disk) and, for the user scope, a removal pass
	// that took away more than the add pass restores (removedOurs !=
	// entireClaudeHookCount means duplicates or alternate-form entries were
	// repaired away).
	if count == 0 && !permissionsChanged && !staleDropped &&
		(!userScope || removedOurs == entireClaudeHookCount) {
		return 0, false, nil
	}

	// Marshal modified hook types back to rawHooks
	marshalHookType(rawHooks, "SessionStart", sessionStart)
	marshalHookType(rawHooks, "SessionEnd", sessionEnd)
	marshalHookType(rawHooks, "Stop", stop)
	marshalHookType(rawHooks, "SubagentStop", subagentStop)
	marshalHookType(rawHooks, "UserPromptSubmit", userPromptSubmit)
	marshalHookType(rawHooks, "PreToolUse", preToolUse)
	marshalHookType(rawHooks, "PostToolUse", postToolUse)

	// Marshal hooks and update raw settings
	hooksJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawHooks)
	if err != nil {
		return 0, false, fmt.Errorf("failed to marshal hooks: %w", err)
	}
	rawSettings["hooks"] = hooksJSON

	// Marshal permissions and update raw settings (repo scope only)
	if projectScope {
		permJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawPermissions)
		if err != nil {
			return 0, false, fmt.Errorf("failed to marshal permissions: %w", err)
		}
		rawSettings["permissions"] = permJSON
	}

	// Write back to file
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o750); err != nil {
		return 0, false, fmt.Errorf("failed to create .claude directory: %w", err)
	}

	output, err := jsonutil.MarshalIndentWithNewline(rawSettings, "", "  ")
	if err != nil {
		return 0, false, fmt.Errorf("failed to marshal settings: %w", err)
	}

	if err := jsonutil.WriteFileAtomicFollowingSymlinks(settingsPath, output, 0o600); err != nil {
		return 0, false, fmt.Errorf("failed to write %s: %w", settingsPath, err)
	}

	// A user-scope write that stripped pre-existing Entire entries is a repair
	// (partial, duplicate, or alternate-form install normalized), not a pure
	// add: the file the user had was changed beyond appending new hooks.
	return count, userScope && removedOurs > 0, nil
}

// parseHookSections parses the hook types Entire manages out of rawHooks. A
// section that exists but does not parse as []ClaudeHookMatcher aborts with
// an error naming the section and file: these sections get rewritten on the
// way out, so an unparseable one cannot round-trip verbatim — silently
// treating it as empty would clobber it on install and delete it on
// uninstall.
func parseHookSections(rawHooks map[string]json.RawMessage, settingsPath string, sections map[string]*[]ClaudeHookMatcher) error {
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
	return uninstallHooksFromFile(settingsPath, true)
}

// uninstallHooksFromFile removes Entire hooks (and only Entire hooks) from
// the settings file at settingsPath. projectScope additionally removes the
// repo-scoped permissions.deny rule; the user-level uninstall passes false.
func uninstallHooksFromFile(settingsPath string, projectScope bool) error {
	data, err := os.ReadFile(settingsPath) //nolint:gosec // path is constructed from repo root + fixed path
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

	// rawHooks preserves unknown hook types (e.g., "Notification").
	var rawHooks map[string]json.RawMessage
	if hooksRaw, ok := rawSettings["hooks"]; ok {
		if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
			return fmt.Errorf("failed to parse hooks in %s: %w", settingsPath, err)
		}
	}
	if rawHooks == nil {
		rawHooks = make(map[string]json.RawMessage)
	}

	// Parse only the hook types we need to modify
	var sessionStart, sessionEnd, stop, subagentStop, userPromptSubmit, preToolUse, postToolUse []ClaudeHookMatcher
	if err := parseHookSections(rawHooks, settingsPath, map[string]*[]ClaudeHookMatcher{
		"SessionStart":     &sessionStart,
		"SessionEnd":       &sessionEnd,
		"Stop":             &stop,
		"SubagentStop":     &subagentStop,
		"UserPromptSubmit": &userPromptSubmit,
		"PreToolUse":       &preToolUse,
		"PostToolUse":      &postToolUse,
	}); err != nil {
		return err
	}

	// Remove Entire hooks from all hook types
	sessionStart = removeEntireHooks(sessionStart)
	sessionEnd = removeEntireHooks(sessionEnd)
	stop = removeEntireHooks(stop)
	subagentStop = removeEntireHooks(subagentStop)
	userPromptSubmit = removeEntireHooks(userPromptSubmit)
	preToolUse = removeEntireHooksFromMatchers(preToolUse)
	postToolUse = removeEntireHooksFromMatchers(postToolUse)

	// Marshal modified hook types back to rawHooks
	marshalHookType(rawHooks, "SessionStart", sessionStart)
	marshalHookType(rawHooks, "SessionEnd", sessionEnd)
	marshalHookType(rawHooks, "Stop", stop)
	marshalHookType(rawHooks, "SubagentStop", subagentStop)
	marshalHookType(rawHooks, "UserPromptSubmit", userPromptSubmit)
	marshalHookType(rawHooks, "PreToolUse", preToolUse)
	marshalHookType(rawHooks, "PostToolUse", postToolUse)

	// Also remove the metadata deny rule from permissions (repo scope only:
	// user-level installs never wrote it, so leave user permissions alone).
	var rawPermissions map[string]json.RawMessage
	if permRaw, ok := rawSettings["permissions"]; ok && projectScope {
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
	if err := jsonutil.WriteFileAtomicFollowingSymlinks(settingsPath, output, 0o600); err != nil {
		return fmt.Errorf("failed to write %s: %w", settingsPath, err)
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
	settings, err := loadClaudeSettingsFile(settingsPath)
	return settings, err == nil
}

// loadClaudeSettingsFile reads and parses a Claude Code settings file. A
// missing file is an fs.ErrNotExist error; callers that need to distinguish
// "not installed" from "cannot tell" (a real read or parse failure) branch on
// errors.Is.
func loadClaudeSettingsFile(settingsPath string) (ClaudeSettings, error) {
	data, err := os.ReadFile(settingsPath) //nolint:gosec // path is constructed from a fixed settings location
	if err != nil {
		return ClaudeSettings{}, fmt.Errorf("read %s: %w", settingsPath, err)
	}

	var settings ClaudeSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return ClaudeSettings{}, fmt.Errorf("parse %s: %w", settingsPath, err)
	}
	return settings, nil
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
	if !hasEntireHook(settings.Hooks.SubagentStop) ||
		!hasEntireHookCoveringTools(settings.Hooks.PreToolUse, subagentTools) ||
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

// removeEntireHooksCounting is removeEntireHooks plus the number of entries
// removed, so the user-scope repair pass can tell an already-correct file
// (exactly the expected entries removed and re-added) from one that needed
// duplicate or alternate-form entries stripped. Duplicate IDENTICAL entries are
// why this cannot be expressed as dropStaleEntireHooks: that helper keeps every
// wanted command, however many copies there are.
func removeEntireHooksCounting(matchers []ClaudeHookMatcher) ([]ClaudeHookMatcher, int) {
	result := make([]ClaudeHookMatcher, 0, len(matchers))
	removed := 0
	for _, matcher := range matchers {
		kept := make([]ClaudeHookEntry, 0, len(matcher.Hooks))
		for _, hook := range matcher.Hooks {
			if isEntireHook(hook.Command) {
				removed++
				continue
			}
			kept = append(kept, hook)
		}
		if len(kept) > 0 {
			matcher.Hooks = kept
			result = append(result, matcher)
		}
	}
	return result, removed
}

// removeEntireHooksFromMatchers removes Entire hooks from tool-use matchers (PreToolUse, PostToolUse)
// This handles the nested structure where hooks are grouped by tool matcher (e.g., "Agent", "TaskCreate|TaskUpdate")
func removeEntireHooksFromMatchers(matchers []ClaudeHookMatcher) []ClaudeHookMatcher {
	// Same logic as removeEntireHooks - both work on the same structure
	return removeEntireHooks(matchers)
}
