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

// InstallUserHooks installs the production-form hook inventory in user
// settings, preserving unrelated keys and permissions. Matching repo/user
// commands let Claude deduplicate entries across scopes.
func (c *ClaudeCodeAgent) InstallUserHooks(ctx context.Context) (agent.UserHookInstallResult, error) {
	settingsPath, err := UserSettingsPath()
	if err != nil {
		return agent.UserHookInstallResult{}, err
	}
	release, err := agent.AcquireUserHookConfigLock(ctx, settingsPath)
	if err != nil {
		return agent.UserHookInstallResult{}, fmt.Errorf("lock Claude Code user hook settings: %w", err)
	}
	defer release()
	count, repaired, err := installHooksToFile(settingsPath, false, false)
	return agent.UserHookInstallResult{Installed: count, Repaired: repaired}, err
}

// UninstallUserHooks removes Entire's hooks (and only Entire's) from
// ~/.claude/settings.json. A missing file is not an error.
func (c *ClaudeCodeAgent) UninstallUserHooks(ctx context.Context) error {
	settingsPath, err := UserSettingsPath()
	if err != nil {
		return err
	}
	release, err := agent.AcquireUserHookConfigLock(ctx, settingsPath)
	if err != nil {
		return fmt.Errorf("lock Claude Code user hook settings: %w", err)
	}
	defer release()
	return uninstallHooksFromFile(settingsPath, false)
}

// AreUserHooksInstalled requires the complete current inventory. Missing is
// false; unreadable or invalid settings return an error.
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
	sections := settings.Hooks.hookSections()
	for _, spec := range claudeHookSpecs {
		var present bool
		if spec.matcher == "" {
			present = hookCommandExists(*sections[spec.section], spec.productionCommand())
		} else {
			present = hookCommandExistsWithMatcher(*sections[spec.section], spec.matcher, spec.productionCommand())
		}
		if !present {
			return false, nil
		}
	}
	return true, nil
}
