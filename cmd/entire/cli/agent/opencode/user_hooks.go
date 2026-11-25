package opencode

import (
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

// userPluginSource is <config>/opencode/plugin/entire.ts: singular "plugin",
// which every OpenCode loads (0.15 and 1.0.0 refuse to start beside
// "plugins"). The plugin steps aside itself (see renderPluginFor).
type userPluginSource struct{}

// UserFile implements agent.UserFileSource.
func (userPluginSource) UserFile() (agent.GeneratedUserFile, error) {
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
