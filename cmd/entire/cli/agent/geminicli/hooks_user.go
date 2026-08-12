package geminicli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Ensure GeminiCLIAgent implements UserHookSupport.
var _ agent.UserHookSupport = (*GeminiCLIAgent)(nil)

// UserSettingsPath returns the path of Gemini CLI's user-level settings file
// (~/.gemini/settings.json). It accepts the same hooks schema as the repo's
// .gemini/settings.json; workspace settings take precedence per key, so a
// repo-level install wins over these entries and hooks do not double-fire.
func UserSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home directory: %w", err)
	}
	return filepath.Join(home, ".gemini", GeminiSettingsFileName), nil
}

// InstallUserHooks installs Entire's hooks in ~/.gemini/settings.json so they
// fire in every repository (the user-level surface behind global tracking).
// Always the plain production `entire` command form; preserves every
// unrelated key in the user settings file.
func (g *GeminiCLIAgent) InstallUserHooks(ctx context.Context) (agent.UserHookInstallResult, error) {
	settingsPath, err := UserSettingsPath()
	if err != nil {
		return agent.UserHookInstallResult{}, err
	}
	count, repaired, err := installHooksToFile(ctx, settingsPath, false, false, true)
	return agent.UserHookInstallResult{Installed: count, Repaired: repaired}, err
}

// UninstallUserHooks removes Entire's hooks (and only Entire's) from
// ~/.gemini/settings.json. A missing file is not an error.
func (g *GeminiCLIAgent) UninstallUserHooks(ctx context.Context) error {
	settingsPath, err := UserSettingsPath()
	if err != nil {
		return err
	}
	return uninstallHooksFromFile(ctx, settingsPath)
}

// AreUserHooksInstalled reports whether Entire's hooks are COMPLETELY
// installed in ~/.gemini/settings.json — the full expected entry set in
// current production form, so a partial install reads as not-installed,
// doctor prompts repair, and the idempotent installer repairs it. A missing
// file is (false, nil); an unreadable or unparseable one returns the error
// rather than posing as "not installed".
func (g *GeminiCLIAgent) AreUserHooksInstalled(_ context.Context) (bool, error) {
	settingsPath, err := UserSettingsPath()
	if err != nil {
		return false, err
	}
	current, err := areUserHooksCurrentInFile(settingsPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return current, nil
}
