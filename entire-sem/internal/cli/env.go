package cli

import "os"

const (
	envCLIVersion    = "ENTIRE_CLI_VERSION"
	envRepoRoot      = "ENTIRE_REPO_ROOT"
	envPluginDataDir = "ENTIRE_PLUGIN_DATA_DIR"
)

// EntireEnv captures environment variables supplied by Entire when it dispatches
// an external plugin command.
type EntireEnv struct {
	CLIVersion    string
	RepoRoot      string
	PluginDataDir string
}

func EnvFromOS() EntireEnv {
	return EntireEnv{
		CLIVersion:    os.Getenv(envCLIVersion),
		RepoRoot:      os.Getenv(envRepoRoot),
		PluginDataDir: os.Getenv(envPluginDataDir),
	}
}

func valueOrUnset(value string) string {
	if value == "" {
		return "<unset>"
	}
	return value
}
