package tokenstore

import "github.com/entireio/cli/internal/entireclient/userconfig"

// configFileName holds per-user CLI settings.
const configFileName = userconfig.FileName

// ErrBadConfig marks an unusable config.json.
var ErrBadConfig = userconfig.ErrBadConfig

// CheckConfig reports an unusable config.json.
func CheckConfig() error {
	_, err := userconfig.Load()
	return err //nolint:wrapcheck // already names the file
}

// ConfigFilePath is config.json's full path.
func ConfigFilePath() string {
	return userconfig.FilePath()
}

// configTokenStore reads token_store; missing means unset.
func configTokenStore() (string, error) {
	cfg, err := userconfig.Load()
	return cfg.TokenStore, err //nolint:wrapcheck // already names the file
}
