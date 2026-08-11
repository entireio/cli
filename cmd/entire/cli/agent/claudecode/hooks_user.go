package claudecode

import (
	"context"
	"fmt"
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
// once. Never touches user-level permissions and preserves unrelated keys.
func (c *ClaudeCodeAgent) InstallUserHooks(_ context.Context) (int, error) {
	settingsPath, err := UserSettingsPath()
	if err != nil {
		return 0, err
	}
	return installHooksToFile(settingsPath, false, false, false)
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

// AreUserHooksInstalled reports whether Entire's hooks are present in
// ~/.claude/settings.json.
func (c *ClaudeCodeAgent) AreUserHooksInstalled(_ context.Context) bool {
	settingsPath, err := UserSettingsPath()
	if err != nil {
		return false
	}
	settings, ok := loadClaudeSettingsFile(settingsPath)
	if !ok {
		return false
	}
	return hasEntireHook(settings.Hooks.Stop)
}
