package claudecode

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/globalhooks"
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

// InstallUserHooks installs the selected global ingress, preserving unrelated settings.
func (c *ClaudeCodeAgent) InstallUserHooks(ctx context.Context) (agent.UserHookInstallResult, error) {
	selected, err := globalhooks.Load()
	if err != nil {
		return agent.UserHookInstallResult{}, fmt.Errorf("select Claude Code user hook installation: %w", err)
	}
	settingsPath, err := UserSettingsPath()
	if err != nil {
		return agent.UserHookInstallResult{}, err
	}
	release, err := agent.AcquireUserHookConfigLock(ctx, settingsPath)
	if err != nil {
		return agent.UserHookInstallResult{}, fmt.Errorf("lock Claude Code user hook settings: %w", err)
	}
	defer release()
	count, repaired, err := installHooksToFile(userSettingsIO{path: settingsPath}, false, false, selected)
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
	return uninstallHooksFromFile(userSettingsIO{path: settingsPath}, false)
}

// AreUserHooksInstalled requires the complete current inventory. Missing is
// false; unreadable or invalid settings return an error.
func (c *ClaudeCodeAgent) AreUserHooksInstalled(_ context.Context) (bool, error) {
	selected, err := globalhooks.Load()
	if err != nil {
		return false, nil //nolint:nilerr // An unavailable selection cannot own user hooks.
	}
	settingsPath, err := UserSettingsPath()
	if err != nil {
		return false, err
	}
	settings, err := loadClaudeSettingsFile(userSettingsIO{path: settingsPath})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	sections := settings.Hooks.hookSections()
	managed := 0
	for _, matchers := range sections {
		for _, matcher := range *matchers {
			for _, hook := range matcher.Hooks {
				if agent.IsManagedHookCommand(hook.Command) {
					managed++
				}
			}
		}
	}
	if managed != len(claudeHookSpecs) {
		return false, nil
	}
	for _, spec := range claudeHookSpecs {
		var present bool
		for _, matcher := range *sections[spec.section] {
			if matcher.Matcher != spec.matcher {
				continue
			}
			for _, hook := range matcher.Hooks {
				if hook.Type == "command" && hook.Command == spec.productionCommand(selected) {
					present = true
				}
			}
		}
		if !present {
			return false, nil
		}
	}
	return true, nil
}
