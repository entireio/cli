// Package tokenstore provides a pluggable credential store shared by the
// entiredb and entire-core CLIs.
//
// By default it delegates to the OS keyring (macOS Keychain, Linux Secret
// Service, etc.). Set ENTIRE_TOKEN_STORE=file to use a JSON file instead,
// which is useful in CI environments that lack a keyring daemon.
//
// When using the file backend the tokens are stored in
// $ENTIRE_TOKEN_STORE_PATH (default: tokens.json in the per-user config
// directory — see internal/entireclient/userdirs).
//
// Service-name conventions:
//   - "entire:<cluster-host>"        — entiredb cluster login tokens
//   - "entire-core:<core-base-url>"  — entire-core control-plane tokens
//   - "<service>:refresh"            — refresh-token entry paired with the
//     corresponding access-token service
package tokenstore

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/zalando/go-keyring"

	"github.com/entireio/cli/internal/entireclient/userdirs"
	"github.com/entireio/cli/internal/testdirs"
)

// ErrNotFound is returned when a credential is not present in the store.
var ErrNotFound = keyring.ErrNotFound

// Keyring service-name prefixes. Tokens are filed under whichever issuer
// vouched for them, so a JWT obtained via an entire-core login flow lives
// at "entire-core:<base-url>" regardless of which CLI wrote it. Two CLIs
// sharing this prefix on the same machine read each other's writes.
const (
	ClusterKeyringPrefix = "entire:"      // entiredb cluster-issued tokens
	CoreKeyringPrefix    = "entire-core:" // entire-core control-plane tokens
)

// ClusterKeyringService returns the service name for tokens issued by an
// entiredb cluster. host is typically the cluster's entry domain.
func ClusterKeyringService(host string) string {
	return ClusterKeyringPrefix + host
}

// CoreKeyringService returns the service name for tokens issued by
// entire-core. coreURL is the base URL of the issuer; trailing slashes
// are normalized away so callers don't have to.
func CoreKeyringService(coreURL string) string {
	return CoreKeyringPrefix + strings.TrimRight(coreURL, "/")
}

// RefreshService returns the paired refresh-token service name for an
// access-token service, following the "<service>:refresh" convention
// documented in this package's service-name conventions. Callers store the
// raw refresh token under (RefreshService(service), user) alongside the
// access token at (service, user).
func RefreshService(service string) string {
	return service + ":refresh"
}

// KeyringServiceForIssuerKey infers the right service prefix from a
// raw issuer key (entire-core URL or entiredb cluster host). URL-shaped
// keys (anything beginning with a scheme) are treated as entire-core
// issuers; bare hostnames as cluster issuers. Used by callers that
// derive a service name without already having a *contexts.Context in
// hand (tests, entiredb's pre-resolution code paths).
func KeyringServiceForIssuerKey(key string) string {
	if strings.HasPrefix(key, "http://") || strings.HasPrefix(key, "https://") {
		return CoreKeyringService(key)
	}
	return ClusterKeyringService(key)
}

// backendMu guards `resolved` and `backend`. It serializes the production
// resolve() against the test-only override path (UseFileBackendForTesting),
// so the package-level state stays well-defined even when tests reset it.
var (
	backendMu sync.Mutex
	resolved  bool
	backend   store
)

type store interface {
	Get(service, user string) (string, error)
	Set(service, user, password string) error
	Delete(service, user string) error
}

func currentBackend() store {
	backendMu.Lock()
	defer backendMu.Unlock()
	if !resolved {
		backend = resolveBackendLocked()
		resolved = true
	}
	return backend
}

func resolveBackendLocked() store {
	if os.Getenv("ENTIRE_TOKEN_STORE") == "file" {
		path := os.Getenv("ENTIRE_TOKEN_STORE_PATH")
		if path == "" {
			path = filepath.Join(userdirs.Config(), "tokens.json")
		}
		return &fileStore{path: path}
	}
	// Under `go test`, never fall through to the real OS keyring: a test
	// that forgets tokenstore.UseFileBackendForTesting would otherwise write
	// real keychain entries. The fallback file is per-process; tests that
	// need isolation from each other still swap in a per-test file via
	// UseFileBackendForTesting.
	if dir, ok := testdirs.Dir("tokenstore"); ok {
		return &fileStore{path: filepath.Join(dir, "tokens.json")}
	}
	return keyringStore{}
}

// Get retrieves a credential.
func Get(service, user string) (string, error) {
	//nolint:wrapcheck // thin wrapper, callers handle errors
	return currentBackend().Get(service, user)
}

// Set stores a credential.
func Set(service, user, password string) error {
	//nolint:wrapcheck // thin wrapper, callers handle errors
	return currentBackend().Set(service, user, password)
}

// Delete removes a credential.
func Delete(service, user string) error {
	//nolint:wrapcheck // thin wrapper, callers handle errors
	return currentBackend().Delete(service, user)
}

// keyringStore delegates to the OS keyring. Every call is bounded by
// callKeyringWithTimeout: the underlying provider (Secret Service,
// Keychain, Credential Manager) can block indefinitely when no daemon
// is reachable, and an unbounded keyring call freezes the whole CLI.
type keyringStore struct{}

func (keyringStore) Get(service, user string) (string, error) {
	// keyring.ErrNotFound propagates unchanged; only a timeout wraps.
	return callKeyringWithTimeout("get", func() (string, error) {
		return keyring.Get(service, user)
	})
}

func (keyringStore) Set(service, user, password string) error {
	_, err := callKeyringWithTimeout("set", func() (string, error) {
		return "", keyring.Set(service, user, password)
	})
	return err
}

func (keyringStore) Delete(service, user string) error {
	_, err := callKeyringWithTimeout("delete", func() (string, error) {
		return "", keyring.Delete(service, user)
	})
	return err
}
