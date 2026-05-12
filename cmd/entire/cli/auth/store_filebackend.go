//go:build authfilestore

// File-backed auth store backend. Compiled only under the `authfilestore`
// build tag, which is enabled by:
//   - the integration test subprocess build (cmd/entire/cli/integration_test/setup_test.go)
//   - test:integration / test:ci tasks (mise.toml) for running this package's
//     tagged tests
//
// Production builds (no tag) do not include this file, so the env var below
// has no effect outside test environments.

package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// testAuthStoreFileEnv, when set, redirects token storage to a JSON file at
// the given path instead of the OS keyring. Only honored in `authfilestore`
// builds.
const testAuthStoreFileEnv = "ENTIRE_TEST_AUTH_STORE_FILE"

func init() {
	chooseBackend = func() tokenBackend {
		if path := os.Getenv(testAuthStoreFileEnv); path != "" {
			return &fileBackend{path: path}
		}
		return keyringBackend{}
	}
}

type fileBackend struct {
	path string
}

func (b *fileBackend) save(service, key, value string) error {
	tokens, err := b.read()
	if err != nil {
		return err
	}
	svc := tokens[service]
	if svc == nil {
		svc = make(map[string]string)
		tokens[service] = svc
	}
	svc[key] = value
	return b.write(tokens)
}

func (b *fileBackend) get(service, key string) (string, error) {
	tokens, err := b.read()
	if err != nil {
		return "", err
	}
	return tokens[service][key], nil
}

func (b *fileBackend) delete(service, key string) error {
	tokens, err := b.read()
	if err != nil {
		return err
	}
	if svc := tokens[service]; svc != nil {
		delete(svc, key)
		if len(svc) == 0 {
			delete(tokens, service)
		}
	}
	return b.write(tokens)
}

func (b *fileBackend) read() (map[string]map[string]string, error) {
	data, err := os.ReadFile(b.path)
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]map[string]string), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read test auth store: %w", err)
	}
	if len(data) == 0 {
		return make(map[string]map[string]string), nil
	}

	var tokens map[string]map[string]string
	if err := json.Unmarshal(data, &tokens); err != nil {
		return nil, fmt.Errorf("parse test auth store: %w", err)
	}
	if tokens == nil {
		tokens = make(map[string]map[string]string)
	}
	return tokens, nil
}

func (b *fileBackend) write(tokens map[string]map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(b.path), 0o700); err != nil {
		return fmt.Errorf("create test auth store directory: %w", err)
	}
	data, err := json.Marshal(tokens)
	if err != nil {
		return fmt.Errorf("marshal test auth store: %w", err)
	}
	if err := os.WriteFile(b.path, data, 0o600); err != nil {
		return fmt.Errorf("write test auth store: %w", err)
	}
	// os.WriteFile preserves an existing file's permission bits, so an
	// already-broad file (e.g. 0o644 from an earlier test setup) would
	// keep those bits. Force 0o600 to make the post-condition unconditional.
	if err := os.Chmod(b.path, 0o600); err != nil {
		return fmt.Errorf("restrict test auth store permissions: %w", err)
	}
	return nil
}
