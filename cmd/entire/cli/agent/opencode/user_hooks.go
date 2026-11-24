package opencode

import (
	"context"
	"os"
	"path/filepath"
	"regexp"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

var _ agent.UserHookInstaller = (*OpenCodeAgent)(nil)

// userHooksVersion is the Entire hook version stamped into the user-level
// plugin. Bump it when the user-level render changes.
const userHooksVersion = 1

var userVersionPattern = regexp.MustCompile(`Entire user-level plugin, hook version (\d+)`)

// xdgConfigHomeEnvVar relocates OpenCode's global config directory.
const xdgConfigHomeEnvVar = "XDG_CONFIG_HOME"

// resolveUserConfigDir returns OpenCode's global config directory:
// $XDG_CONFIG_HOME/opencode, else ~/.config/opencode, which every OpenCode
// loads plugins from.
func resolveUserConfigDir() (string, error) {
	base, ok, err := agent.LookupOverride(xdgConfigHomeEnvVar)
	if err != nil {
		return "", err //nolint:wrapcheck // names the override and its value
	}
	if ok {
		return filepath.Join(base, "opencode"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err //nolint:wrapcheck // the home directory is the only input
	}
	return filepath.Join(home, ".config", "opencode"), nil
}

// userPlugin is <config>/opencode/plugin/entire.ts: singular "plugin", which
// every OpenCode loads (0.15 and 1.0.0 refuse to start beside "plugins").
func userPlugin() (agent.GeneratedUserFile, error) {
	home, err := resolveUserConfigDir()
	if err != nil {
		return agent.GeneratedUserFile{}, err
	}
	return agent.GeneratedUserFile{
		Home:    home,
		Path:    filepath.Join(home, pluginDirName, pluginFileName),
		Marker:  entireMarker,
		Content: renderPluginFor(true),
		Version: userVersionPattern,
	}, nil
}

// EnsureUserHooks implements agent.UserHookInstaller.
func (a *OpenCodeAgent) EnsureUserHooks(ctx context.Context, wanted func(context.Context) bool) (int, string, error) {
	file, err := userPlugin()
	if err != nil {
		return 0, "", err
	}
	n, err := file.Ensure(ctx, wanted)
	return n, file.Path, err //nolint:wrapcheck // names the file
}

// RemoveUserHooks implements agent.UserHookInstaller.
func (a *OpenCodeAgent) RemoveUserHooks(ctx context.Context) (bool, string, error) {
	file, err := userPlugin()
	if err != nil {
		return false, "", err
	}
	removed, err := file.Remove(ctx)
	return removed, file.Path, err //nolint:wrapcheck // names the file
}

// InspectUserHooks implements agent.UserHookInstaller.
func (a *OpenCodeAgent) InspectUserHooks(context.Context) agent.UserHookReport {
	file, err := userPlugin()
	if err != nil {
		return agent.UserHookReport{State: agent.UserHooksUnavailable, Err: err}
	}
	return file.Inspect()
}

// ProjectHooksSupersede implements agent.UserHookInstaller. OpenCode's
// user-level plugin steps aside itself when it loads (see renderPluginFor),
// so no hook command needs to ask.
func (a *OpenCodeAgent) ProjectHooksSupersede(context.Context) bool { return false }
