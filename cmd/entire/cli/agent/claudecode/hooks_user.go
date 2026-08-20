package claudecode

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Ensure ClaudeCodeAgent implements UserHookSupport.
var _ agent.UserHookSupport = (*ClaudeCodeAgent)(nil)

// UserSettingsPath returns the path of Claude Code's user-level settings file
// (~/.claude/settings.json). It accepts the same hooks schema as the repo's
// .claude/settings.json.
func UserSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home directory: %w", err)
	}
	return filepath.Join(home, ".claude", ClaudeSettingsFileName), nil
}

// InstallUserHooks installs Entire's hooks in ~/.claude/settings.json so they
// fire in every repository (the user-level surface behind global tracking).
// Always the plain production `entire` command form — byte-identical to the
// repo-level production entries, so Claude Code's cross-scope dedup of
// identical hook commands keeps a repo with both installs firing each hook
// once. That dedup covers byte-identical commands ONLY: a repo whose hooks
// were installed with --local-dev (scripts/entire-dev form) or by a legacy
// go-run install has different command strings at repo scope, so its hooks
// double-fire alongside these user-level entries. It self-heals within the
// user file: existing Entire entries in any other command form are removed
// and re-added in the current form, so a pre-existing alternate-form entry
// can never double-fire alongside the one this install writes. Never touches
// user-level permissions and preserves unrelated keys.
func (c *ClaudeCodeAgent) InstallUserHooks(_ context.Context) (agent.UserHookInstallResult, error) {
	settingsPath, err := UserSettingsPath()
	if err != nil {
		return agent.UserHookInstallResult{}, err
	}
	count, repaired, err := installHooksToFile(settingsPath, false, false)
	return agent.UserHookInstallResult{Installed: count, Repaired: repaired}, err
}

// UninstallUserHooks removes Entire's hooks (and only Entire's) from
// ~/.claude/settings.json. A missing file is not an error.
func (c *ClaudeCodeAgent) UninstallUserHooks(_ context.Context) error {
	settingsPath, err := UserSettingsPath()
	if err != nil {
		return err
	}
	return uninstallHooksFromFile(settingsPath, false)
}

// AreUserHooksInstalled reports whether Entire's hooks are COMPLETELY
// installed in ~/.claude/settings.json — the same completeness spec as the
// repo-level CheckHookConfig (Stop present plus the current tool-use
// matchers), so a partial install reads as not-installed, doctor prompts
// repair, and the idempotent installer repairs it. A missing file is
// (false, nil); an unreadable or unparseable one returns the error rather
// than posing as "not installed".
func (c *ClaudeCodeAgent) AreUserHooksInstalled(_ context.Context) (bool, error) {
	settingsPath, err := UserSettingsPath()
	if err != nil {
		return false, err
	}
	settings, err := loadClaudeSettingsFile(settingsPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if !hasEntireHook(settings.Hooks.Stop) {
		return false, nil
	}
	subagentTools := splitMatcherTools(subagentToolMatcher)
	taskTools := splitMatcherTools(taskToolMatcher)
	return hasEntireHookCoveringTools(settings.Hooks.PreToolUse, subagentTools) &&
		hasEntireHookCoveringTools(settings.Hooks.PostToolUse, subagentTools) &&
		hasEntireHookCoveringTools(settings.Hooks.PostToolUse, taskTools), nil
}
