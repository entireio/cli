package geminicli

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

// Ensure GeminiCLIAgent implements UserHookSupport.
var _ agent.UserHookSupport = (*GeminiCLIAgent)(nil)

// UserSettingsPath returns ~/.gemini/settings.json.
func UserSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home directory: %w", err)
	}
	return filepath.Join(home, ".gemini", GeminiSettingsFileName), nil
}

// InstallUserHooks installs Entire's hooks in ~/.gemini/settings.json so they
// fire in every repository (the user-level surface behind global tracking).
// Uses the explicitly selected installation and preserves unrelated settings.
func (g *GeminiCLIAgent) InstallUserHooks(ctx context.Context) (agent.UserHookInstallResult, error) {
	selected, err := globalhooks.Load()
	if err != nil {
		return agent.UserHookInstallResult{}, fmt.Errorf("select Gemini CLI user hook installation: %w", err)
	}
	settingsPath, err := UserSettingsPath()
	if err != nil {
		return agent.UserHookInstallResult{}, err
	}
	release, err := agent.AcquireUserHookConfigLock(ctx, settingsPath)
	if err != nil {
		return agent.UserHookInstallResult{}, fmt.Errorf("lock Gemini CLI user hook settings: %w", err)
	}
	defer release()
	count, repaired, err := installHooksToFile(ctx, userSettingsIO{path: settingsPath}, false, true, selected)
	return agent.UserHookInstallResult{Installed: count, Repaired: repaired}, err
}

// UninstallUserHooks removes Entire's hooks (and only Entire's) from
// ~/.gemini/settings.json. A missing file is not an error.
func (g *GeminiCLIAgent) UninstallUserHooks(ctx context.Context) error {
	settingsPath, err := UserSettingsPath()
	if err != nil {
		return err
	}
	release, err := agent.AcquireUserHookConfigLock(ctx, settingsPath)
	if err != nil {
		return fmt.Errorf("lock Gemini CLI user hook settings: %w", err)
	}
	defer release()
	return uninstallHooksFromFile(ctx, userSettingsIO{path: settingsPath})
}

// AreUserHooksInstalled requires the complete current inventory. Missing is
// false; unreadable or invalid settings return an error.
func (g *GeminiCLIAgent) AreUserHooksInstalled(_ context.Context) (bool, error) {
	selected, err := globalhooks.Load()
	if err != nil {
		return false, nil //nolint:nilerr // An unavailable selection cannot own user hooks.
	}
	settingsPath, err := UserSettingsPath()
	if err != nil {
		return false, err
	}
	current, err := areUserHooksCurrentInFile(userSettingsIO{path: settingsPath}, selected)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return current, nil
}
