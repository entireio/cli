package geminicli

import (
	"context"
	"fmt"
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
func (g *GeminiCLIAgent) InstallUserHooks(ctx context.Context) (int, error) {
	settingsPath, err := UserSettingsPath()
	if err != nil {
		return 0, err
	}
	return installHooksToFile(ctx, settingsPath, false, false)
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

// AreUserHooksInstalled reports whether Entire's hooks are present in
// ~/.gemini/settings.json.
func (g *GeminiCLIAgent) AreUserHooksInstalled(_ context.Context) bool {
	settingsPath, err := UserSettingsPath()
	if err != nil {
		return false
	}
	return areHooksInstalledInFile(settingsPath)
}
