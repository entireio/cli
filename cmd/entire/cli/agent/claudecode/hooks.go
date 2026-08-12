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

// localDevHookCmdPrefix is the command prefix used for hooks in local-dev mode.
// It points at scripts/entire-dev, which compiles the CLI on demand and falls
// back to the entire binary on PATH when the tree does not build (e.g. mid
// merge-conflict-fix). ${CLAUDE_PROJECT_DIR} is set by Claude Code to the
// repository root when it runs hooks.
const localDevHookCmdPrefix = "${CLAUDE_PROJECT_DIR}/scripts/entire-dev "

// localDevSessionEndTimeoutSecs gives the local-dev SessionEnd hook an explicit
// timeout (seconds) so Claude Code waits for it on exit instead of cancelling it
// after its short default exit-grace, which the build-from-source dev launcher
// (scripts/entire-dev) can exceed. Only set in local-dev mode; production leaves
// Claude Code's default in place.
const localDevSessionEndTimeoutSecs = 60

// entireHookPrefixes are command prefixes that identify Entire hooks. Each
// prefix is scoped to the `hooks` verb: recognition by binary name alone
// (`entire `) would claim user-authored hooks that merely invoke the entire
// CLI (e.g. `entire status --json > /tmp/s.json`) as ours and delete them on
// uninstall or a remove-then-reinstall pass. The "go run" prefix is retained
// so hooks installed by older versions are still recognized for
// removal/upgrade.
var entireHookPrefixes = []string{
	"entire hooks ",
	localDevHookCmdPrefix + "hooks ",
	"go run ${CLAUDE_PROJECT_DIR}/cmd/entire/main.go hooks ",
}

// entireClaudeHookCount is the number of hook entries a full install writes
// (SessionStart, SessionEnd, Stop, UserPromptSubmit, PreToolUse[Agent],
// PostToolUse[Agent], PostToolUse[TaskCreate|TaskUpdate]).
const entireClaudeHookCount = 7

// localDevHookCommand builds a local-dev hook command for the given hook name,
// delegating to scripts/entire-dev for the build-probe-and-fallback logic.
func localDevHookCommand(hookName string) string {
	return fmt.Sprintf("%shooks claude-code %s", localDevHookCmdPrefix, hookName)
}

// InstallHooks installs Claude Code hooks in .claude/settings.json.
// If force is true, removes existing Entire hooks before installing.
// Returns the number of hooks installed.
func (c *ClaudeCodeAgent) InstallHooks(ctx context.Context, localDev bool, force bool) (int, error) {
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
	count, _, err := installHooksToFile(settingsPath, localDev, force, true)
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
	sessionStart, sessionEnd, stop, userPromptSubmit, preTask, postTask, postTodo string
}

func buildClaudeHookCommands(localDev bool) claudeHookCommands {
	if localDev {
		return claudeHookCommands{
			sessionStart:     localDevHookCommand(HookNameSessionStart),
			sessionEnd:       localDevHookCommand(HookNameSessionEnd),
			stop:             localDevHookCommand(HookNameStop),
			userPromptSubmit: localDevHookCommand(HookNameUserPromptSubmit),
			preTask:          localDevHookCommand(HookNamePreTask),
			postTask:         localDevHookCommand(HookNamePostTask),
			postTodo:         localDevHookCommand(HookNamePostTodo),
		}
	}
	return claudeHookCommands{
		sessionStart:     agent.WrapProductionJSONWarningHookCommand("entire hooks claude-code session-start", agent.WarningFormatMultiLine),
		sessionEnd:       agent.WrapProductionSilentHookCommand("entire hooks claude-code session-end"),
		stop:             agent.WrapProductionSilentHookCommand("entire hooks claude-code stop"),
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
func installHooksToFile(settingsPath string, localDev, force, projectScope bool) (count int, repaired bool, err error) {
	rawSettings, rawHooks, rawPermissions, err := readClaudeRawSettings(settingsPath, projectScope)
	if err != nil {
		return 0, false, err
	}

	// Parse only the hook types we need to modify
	var sessionStart, sessionEnd, stop, userPromptSubmit, preToolUse, postToolUse []ClaudeHookMatcher
	if err := parseHookSections(rawHooks, settingsPath, map[string]*[]ClaudeHookMatcher{
		"SessionStart":     &sessionStart,
		"SessionEnd":       &sessionEnd,
		"Stop":             &stop,
		"UserPromptSubmit": &userPromptSubmit,
		"PreToolUse":       &preToolUse,
		"PostToolUse":      &postToolUse,
	}); err != nil {
		return 0, false, err
	}

	// Presence checks run against these pre-removal snapshots so a user-scope
	// repair pass (below) does not defeat the idempotency accounting.
	checkSessionStart, checkSessionEnd, checkStop := sessionStart, sessionEnd, stop
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
		userPromptSubmit = strip(userPromptSubmit)
		preToolUse = strip(preToolUse)
		postToolUse = strip(postToolUse)
	}

	cmds := buildClaudeHookCommands(localDev)

	// The local-dev SessionEnd hook gets an explicit timeout so Claude Code
	// waits for it on exit; every other hook (and all production hooks) keeps
	// Claude Code's default.
	sessionEndTimeoutSecs := 0
	if localDev {
		sessionEndTimeoutSecs = localDevSessionEndTimeoutSecs
	}

	// ensureHook counts a hook as newly installed when its exact command was
	// absent from the pre-removal snapshot (force counts everything: it always
	// reinstalls), and adds the entry unless a plain repo-scope install found
	// it already present. After a removal pass (force or user scope) every
	// Entire entry is gone from the working slices, so the add must not be
	// gated on the presence check.
	addAll := force || userScope
	ensureHook := func(matchers, check []ClaudeHookMatcher, matcherName, cmd string, timeoutSecs int) []ClaudeHookMatcher {
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
		return addHookToMatcher(matchers, matcherName, cmd, timeoutSecs)
	}

	sessionStart = ensureHook(sessionStart, checkSessionStart, "", cmds.sessionStart, 0)
	sessionEnd = ensureHook(sessionEnd, checkSessionEnd, "", cmds.sessionEnd, sessionEndTimeoutSecs)
	stop = ensureHook(stop, checkStop, "", cmds.stop, 0)
	userPromptSubmit = ensureHook(userPromptSubmit, checkUserPromptSubmit, "", cmds.userPromptSubmit, 0)
	preToolUse = ensureHook(preToolUse, checkPreToolUse, subagentToolMatcher, cmds.preTask, 0)
	postToolUse = ensureHook(postToolUse, checkPostToolUse, subagentToolMatcher, cmds.postTask, 0)
	postToolUse = ensureHook(postToolUse, checkPostToolUse, taskToolMatcher, cmds.postTodo, 0)

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

	// All hooks and permissions already installed — for the user scope only
	// when the removal pass removed exactly the entries the add pass restores
	// (removedOurs > entireClaudeHookCount means duplicates or alternate-form
	// entries were repaired away and the file must be rewritten).
	if count == 0 && !permissionsChanged && (!userScope || removedOurs == entireClaudeHookCount) {
		return 0, false, nil
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

	// rawHooks preserves unknown hook types (e.g., "Notification", "SubagentStop")
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
	var sessionStart, sessionEnd, stop, userPromptSubmit, preToolUse, postToolUse []ClaudeHookMatcher
	if err := parseHookSections(rawHooks, settingsPath, map[string]*[]ClaudeHookMatcher{
		"SessionStart":     &sessionStart,
		"SessionEnd":       &sessionEnd,
		"Stop":             &stop,
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

func addHookToMatcher(matchers []ClaudeHookMatcher, matcherName, command string, timeoutSecs int) []ClaudeHookMatcher {
	entry := ClaudeHookEntry{
		Type:    "command",
		Command: command,
		Timeout: timeoutSecs,
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
	return agent.IsManagedHookCommand(command, entireHookPrefixes)
}

// removeEntireHooks removes all Entire hooks from a list of matchers (for simple hooks like Stop)
func removeEntireHooks(matchers []ClaudeHookMatcher) []ClaudeHookMatcher {
	result, _ := removeEntireHooksCounting(matchers)
	return result
}

// removeEntireHooksCounting is removeEntireHooks plus the number of entries
// removed, so the user-scope repair pass can tell an already-correct file
// (exactly the expected entries removed and re-added) from one that needed
// duplicate or alternate-form entries stripped.
func removeEntireHooksCounting(matchers []ClaudeHookMatcher) ([]ClaudeHookMatcher, int) {
	removed := 0
	result := make([]ClaudeHookMatcher, 0, len(matchers))
	for _, matcher := range matchers {
		filteredHooks := make([]ClaudeHookEntry, 0, len(matcher.Hooks))
		for _, hook := range matcher.Hooks {
			if !isEntireHook(hook.Command) {
				filteredHooks = append(filteredHooks, hook)
			} else {
				removed++
			}
		}
		// Only keep the matcher if it has hooks remaining
		if len(filteredHooks) > 0 {
			matcher.Hooks = filteredHooks
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
