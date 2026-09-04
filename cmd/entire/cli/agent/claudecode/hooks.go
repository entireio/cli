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
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Ensure ClaudeCodeAgent implements HookSupport
var (
	_ agent.HookSupport           = (*ClaudeCodeAgent)(nil)
	_ agent.HookFreshness         = (*ClaudeCodeAgent)(nil)
	_ agent.PermissionConfigOwner = (*ClaudeCodeAgent)(nil)
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

// hookSettingsIO abstracts where a hook install reads and writes its settings
// file. Repo scope uses agent.HookConfigFile: the path lives in the working
// tree, which arrives by clone, so a checked-in symlink at .claude must be
// refused rather than followed. User scope (~/.claude) is the opposite case —
// dotfile managers legitimately symlink it — so it follows symlinks
// (jsonutil.WriteFileAtomicFollowingSymlinks) under the user-hook lock.
type hookSettingsIO interface {
	Read() ([]byte, error)
	Write(data []byte, perm os.FileMode) error
	Path() string
}

// userSettingsIO is the user-scope implementation of hookSettingsIO.
type userSettingsIO struct{ path string }

func (u userSettingsIO) Path() string { return u.path }

func (u userSettingsIO) Read() ([]byte, error) {
	return os.ReadFile(u.path) //nolint:wrapcheck // fixed user-level settings location; callers name the file
}

func (u userSettingsIO) Write(data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(u.path), 0o750); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(u.path), err)
	}
	return jsonutil.WriteFileAtomicFollowingSymlinks(u.path, data, perm) //nolint:wrapcheck // callers name the file
}

// claudeHookConfig returns .claude/settings.json for the current worktree,
// opened through the worktree's root. Every repo-scope read, write and removal
// of that file goes through it: the path lives in the working tree, which
// arrives by clone, so a checked-in symlink at `.claude` must not be a
// directory Entire creates and writes through.
func claudeHookConfig(ctx context.Context) (*agent.HookConfigFile, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		// Fallback to CWD if not in a git repo (e.g., during tests)
		repoRoot, err = os.Getwd() //nolint:forbidigo // Intentional fallback when WorktreeRoot() fails (tests run outside git repos)
		if err != nil {
			return nil, fmt.Errorf("failed to get current directory: %w", err)
		}
	}
	return agent.OpenHookConfig(repoRoot, ".claude/"+ClaudeSettingsFileName) //nolint:wrapcheck // agent.HookConfigFile already names the file in its error
}

// entireClaudeHookCount is the number of hook entries a full install writes
// (SessionStart, SessionEnd, Stop, SubagentStop, UserPromptSubmit, PreToolUse[Agent],
// PostToolUse[Agent], PostToolUse[TaskCreate|TaskUpdate]).
const entireClaudeHookCount = 8

// claudeHookSpec is the single inventory entry used by repo/user install and
// user-hook completeness checks. Claude's native matcher model stays typed;
// only the shared orchestration is declarative.
type claudeHookSpec struct {
	section  string
	matcher  string
	hookName string
	warnWrap bool
}

var claudeHookSpecs = []claudeHookSpec{
	{section: "SessionStart", hookName: HookNameSessionStart, warnWrap: true},
	{section: "SessionEnd", hookName: HookNameSessionEnd},
	{section: "Stop", hookName: HookNameStop},
	{section: "SubagentStop", hookName: HookNameSubagentStop},
	{section: "UserPromptSubmit", hookName: HookNameUserPromptSubmit},
	{section: "PreToolUse", matcher: subagentToolMatcher, hookName: HookNamePreTask},
	{section: "PostToolUse", matcher: subagentToolMatcher, hookName: HookNamePostTask},
	{section: "PostToolUse", matcher: taskToolMatcher, hookName: HookNamePostTodo},
}

func (s claudeHookSpec) productionCommand() string {
	cmd := "entire hooks claude-code " + s.hookName
	if s.warnWrap {
		return agent.WrapProductionJSONWarningHookCommand(cmd, agent.WarningFormatMultiLine)
	}
	return agent.WrapProductionSilentHookCommand(cmd)
}

func (h *ClaudeHooks) hookSections() map[string]*[]ClaudeHookMatcher {
	return map[string]*[]ClaudeHookMatcher{
		"SessionStart":     &h.SessionStart,
		"SessionEnd":       &h.SessionEnd,
		"Stop":             &h.Stop,
		"SubagentStop":     &h.SubagentStop,
		"UserPromptSubmit": &h.UserPromptSubmit,
		"PreToolUse":       &h.PreToolUse,
		"PostToolUse":      &h.PostToolUse,
	}
}

func newClaudeHookSections() map[string]*[]ClaudeHookMatcher {
	return (&ClaudeHooks{}).hookSections()
}

// InstallHooks installs Claude Code hooks in .claude/settings.json.
// If force is true, removes existing Entire hooks before installing.
// Returns the number of hooks installed.
func (c *ClaudeCodeAgent) InstallHooks(ctx context.Context, force bool) (int, error) {
	// Use repo root instead of CWD to find .claude directory
	// This ensures hooks are installed correctly when run from a subdirectory
	cfg, err := claudeHookConfig(ctx)
	if err != nil {
		return 0, err
	}
	count, _, err := installHooksToFile(cfg, force, true)
	return count, err
}

// readClaudeRawSettings reads and shallow-parses the settings file for a
// hook install. projectScope controls whether the permissions section is
// parsed: a user-scope install must neither parse nor fail on it — whatever
// value is there (even a non-object) round-trips verbatim. Only a genuinely
// missing file means "start fresh"; any other read failure (permissions, I/O)
// aborts, because proceeding would replace the user's whole settings file
// with an Entire-only one.
func readClaudeRawSettings(file hookSettingsIO, projectScope bool) (rawSettings, rawHooks, rawPermissions map[string]json.RawMessage, err error) {
	settingsPath := file.Path()
	existingData, readErr := file.Read()
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

// installHooksToFile installs Entire's Claude Code hooks into the settings
// file at settingsPath. projectScope additionally removes Entire's retired
// repo-scoped permissions.deny rule; the user-level install (InstallUserHooks)
// passes false so it only ever touches the hooks section of ~/.claude/settings.json.
// repaired reports a user-scope rewrite that normalized pre-existing Entire
// entries (rather than a pure add or a no-op), so the caller can report the
// repair instead of "already installed".
func installHooksToFile(file hookSettingsIO, force, projectScope bool) (count int, repaired bool, err error) {
	settingsPath := file.Path()
	rawSettings, rawHooks, rawPermissions, err := readClaudeRawSettings(file, projectScope)
	if err != nil {
		return 0, false, err
	}

	sections := newClaudeHookSections()
	if err := parseHookSections(rawHooks, settingsPath, sections); err != nil {
		return 0, false, err
	}

	// Presence checks run against these pre-removal snapshots so a user-scope
	// repair pass (below) does not defeat the idempotency accounting.
	checks := make(map[string][]ClaudeHookMatcher, len(sections))
	for section, matchers := range sections {
		checks[section] = *matchers
	}

	// force removes all existing Entire hooks before reinstalling (mode
	// switch). The user-scope install ALWAYS runs this remove-ours-then-re-add
	// pass: hookCommandExists is an exact-string compare, so a pre-existing
	// Entire hook in a different command form (bare, legacy go-run) would
	// otherwise gain a second entry and double-fire machine-wide. removedOurs
	// counts the removed entries so an already-correct file is left untouched.
	userScope := !projectScope
	removedOurs := 0
	if force || userScope {
		for _, matchers := range sections {
			out, n := removeEntireHooksCounting(*matchers)
			removedOurs += n
			*matchers = out
		}
	}

	// Drop Entire hooks left by older versions before adding the current ones,
	// so a stale command (e.g. the removed local-dev launcher, which ran a
	// script inside the working tree) does not survive alongside them.
	// Unconditional: a plain `entire enable` must migrate too, not just --force.
	staleDropped := false
	if !force && !userScope {
		wanted := make(map[string][]string, len(sections))
		for _, spec := range claudeHookSpecs {
			wanted[spec.section] = append(wanted[spec.section], spec.productionCommand())
		}
		for section, matchers := range sections {
			out, dropped := dropStaleEntireHooks(*matchers, wanted[section]...)
			if dropped {
				staleDropped = true
			}
			*matchers = out
		}
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

	for _, spec := range claudeHookSpecs {
		matchers := sections[spec.section]
		*matchers = ensureHook(*matchers, checks[spec.section], spec.matcher, spec.productionCommand())
	}

	// A normal repo-scoped enable also removes Entire's retired metadata deny
	// rule. User-level installs must not modify user permissions.
	permissionsChanged := false
	if projectScope {
		changed, err := agent.RemoveMetadataDenyRule(rawPermissions)
		if err != nil {
			return 0, false, fmt.Errorf("failed to update permissions in %s: %w", settingsPath, err)
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

	for section, matchers := range sections {
		marshalHookType(rawHooks, section, *matchers)
	}

	// Marshal hooks and update raw settings
	hooksJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawHooks)
	if err != nil {
		return 0, false, fmt.Errorf("failed to marshal hooks: %w", err)
	}
	rawSettings["hooks"] = hooksJSON

	// Removing the retired rule can empty permissions. Delete that empty block
	// rather than leaving noise Entire introduced in the tracked settings file.
	if projectScope {
		if len(rawPermissions) == 0 {
			delete(rawSettings, "permissions")
		} else {
			permJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawPermissions)
			if err != nil {
				return 0, false, fmt.Errorf("failed to marshal permissions: %w", err)
			}
			rawSettings["permissions"] = permJSON
		}
	}

	output, err := jsonutil.MarshalIndentWithNewline(rawSettings, "", "  ")
	if err != nil {
		return 0, false, fmt.Errorf("failed to marshal settings: %w", err)
	}

	// Repo scope creates .claude with MkdirAllNoSymlink inside the write, so a
	// checked-in symlink there is refused by name rather than followed; user
	// scope follows dotfile-manager symlinks instead (see hookSettingsIO).
	if err := file.Write(output, 0o600); err != nil {
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
	cfg, err := claudeHookConfig(ctx)
	if err != nil {
		return err
	}
	return uninstallHooksFromFile(cfg, true)
}

// uninstallHooksFromFile removes Entire hooks (and only Entire hooks) from
// the settings file at settingsPath. projectScope additionally removes the
// repo-scoped permissions.deny rule; the user-level uninstall passes false.
func uninstallHooksFromFile(file hookSettingsIO, projectScope bool) error {
	settingsPath := file.Path()
	data, err := file.Read()
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

	sections := newClaudeHookSections()
	if err := parseHookSections(rawHooks, settingsPath, sections); err != nil {
		return err
	}
	for section, matchers := range sections {
		*matchers = removeEntireHooks(*matchers)
		marshalHookType(rawHooks, section, *matchers)
	}

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
		// Same removal InstallHooks now performs; a marshal failure here leaves
		// the rule in place, which uninstall reports through the write below
		// rather than aborting the rest of the hook removal.
		_, _ = agent.RemoveMetadataDenyRule(rawPermissions) //nolint:errcheck // best-effort during uninstall; hook removal must still complete

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
	if err := file.Write(output, 0o600); err != nil {
		return fmt.Errorf("failed to write %s: %w", settingsPath, err)
	}
	return nil
}

// loadClaudeSettings reads and parses .claude/settings.json from the repo root.
// Returns ok=false when the file is missing or unparseable.
// loadClaudeSettings reads the repo-scope settings through the worktree root.
// A missing file yields empty settings; an unreadable or unparseable one is an
// error (and a Warn — "not installed" and "cannot tell" must not collapse).
func loadClaudeSettings(ctx context.Context) (ClaudeSettings, error) {
	cfg, err := claudeHookConfig(ctx)
	if err != nil {
		return ClaudeSettings{}, err
	}
	settings, err := loadClaudeSettingsFile(cfg)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return ClaudeSettings{}, nil
	case err != nil:
		logging.Warn(ctx, "claude-code: failed to load settings file", "path", cfg.Path(), "err", err)
		return ClaudeSettings{}, err
	}
	return settings, nil
}

// loadClaudeSettingsFile reads and parses a Claude Code settings file. A
// missing file is an fs.ErrNotExist error; callers that need to distinguish
// "not installed" from "cannot tell" (a real read or parse failure) branch on
// errors.Is.
func loadClaudeSettingsFile(file hookSettingsIO) (ClaudeSettings, error) {
	data, err := file.Read()
	if err != nil {
		return ClaudeSettings{}, fmt.Errorf("read %s: %w", file.Path(), err)
	}

	var settings ClaudeSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return ClaudeSettings{}, fmt.Errorf("parse %s: %w", file.Path(), err)
	}
	return settings, nil
}

// AreHooksInstalled reports whether Entire hooks are installed; an unreadable
// or malformed settings file is an error, not "not installed".
func (c *ClaudeCodeAgent) AreHooksInstalled(ctx context.Context) (bool, error) {
	settings, err := loadClaudeSettings(ctx)
	if err != nil {
		return false, err
	}
	// Check for at least one of our hooks (new, wrapped, or legacy format)
	return hasEntireHook(settings.Hooks.Stop), nil
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
	settings, err := loadClaudeSettings(ctx)
	if err != nil || !hasEntireHook(settings.Hooks.Stop) {
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

// PermissionConfig implements agent.PermissionConfigOwner so the shared
// retired-deny-rule diagnostics and repair can reach .claude/settings.json
// without knowing Claude Code's layout.
func (c *ClaudeCodeAgent) PermissionConfig(ctx context.Context) (*agent.HookConfigFile, error) {
	return claudeHookConfig(ctx)
}
