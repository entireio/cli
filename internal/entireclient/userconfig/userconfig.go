// Package userconfig reads the per-user config.json.
package userconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// FileName holds per-user CLI settings.
const FileName = "config.json"

// ErrBadConfig marks an unusable config.json.
var ErrBadConfig = errors.New("invalid Entire config")

// Keys config.json accepts, matched exactly.
const (
	tokenStoreKey   = "token_store"
	noAutoUpdateKey = "no_auto_update"
)

var knownKeys = []string{noAutoUpdateKey, tokenStoreKey}

// token_store values config.json accepts.
const (
	TokenStoreFile    = "file"
	TokenStoreKeyring = "keyring"
)

// Config is config.json's parsed contents.
type Config struct {
	// TokenStore is "" or a TokenStore value.
	TokenStore string
	// NoAutoUpdate turns off update checks.
	NoAutoUpdate bool
}

// FilePath is config.json's full path.
func FilePath() string {
	dir, _ := userdirs.ConfigDirChecked() //nolint:errcheck // display only
	return filepath.Join(dir, FileName)
}

// Load reads config.json; missing means zero.
func Load() (Config, error) {
	// Bad override: its consumers report it.
	if _, err := userdirs.ConfigDirChecked(); err != nil {
		return Config{}, nil //nolint:nilerr // see above
	}
	root, err := userdirs.ConfigRootForRead()
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, badConfig(err)
	}
	data, err := osroot.ReadFileNoFollow(root, FileName)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, badConfig(err)
	}
	return parse(data)
}

func parse(data []byte) (Config, error) {
	// A map, so keys match exactly.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return Config{}, badConfig(err)
	}
	for _, key := range slices.Sorted(maps.Keys(keys)) {
		if !slices.Contains(knownKeys, key) {
			return Config{}, badConfig(fmt.Errorf("unknown key %q", key))
		}
	}
	var cfg Config
	if raw, ok := keys[tokenStoreKey]; ok {
		if err := json.Unmarshal(raw, &cfg.TokenStore); err != nil {
			return Config{}, badConfig(fmt.Errorf("%s: %w", tokenStoreKey, err))
		}
	}
	switch cfg.TokenStore {
	case "", TokenStoreFile, TokenStoreKeyring:
	default:
		return Config{}, badConfig(fmt.Errorf("%s is %q, want %q or %q", tokenStoreKey, cfg.TokenStore, TokenStoreFile, TokenStoreKeyring))
	}
	if raw, ok := keys[noAutoUpdateKey]; ok {
		if err := json.Unmarshal(raw, &cfg.NoAutoUpdate); err != nil {
			return Config{}, badConfig(fmt.Errorf("%s: %w", noAutoUpdateKey, err))
		}
	}
	return cfg, nil
}

func badConfig(err error) error {
	return fmt.Errorf("%w %s: %w; fix or remove it", ErrBadConfig, FilePath(), err)
}
