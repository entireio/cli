// Package tokenstore provides a pluggable credential store shared by the
// entiredb and entire-core CLIs.
//
// By default it delegates to the OS keyring (macOS Keychain, Linux Secret
// Service, etc.). Set ENTIRE_TOKEN_STORE=file to use a JSON file instead,
// which is useful in CI environments that lack a keyring daemon.
//
// When using the file backend the tokens are stored in
// $ENTIRE_TOKEN_STORE_PATH (default: ~/.config/entire/tokens.json).
//
// Service-name conventions:
//   - "entire:<cluster-host>"        — entiredb cluster login tokens
//   - "entire-core:<core-base-url>"  — entire-core control-plane tokens
//   - "<service>:refresh"            — refresh-token entry paired with the
//     corresponding access-token service
package tokenstore

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/zalando/go-keyring"
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

func currentBackend() store { //nolint:ireturn // pluggable keyring backend; the interface return is the seam for the file-backed test store
	backendMu.Lock()
	defer backendMu.Unlock()
	if !resolved {
		backend = resolveBackendLocked()
		resolved = true
	}
	return backend
}

func resolveBackendLocked() store { //nolint:ireturn // see currentBackend: the interface return is the test-store seam
	if os.Getenv("ENTIRE_TOKEN_STORE") == "file" {
		path := os.Getenv("ENTIRE_TOKEN_STORE_PATH")
		if path == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				panic(fmt.Sprintf("tokenstore: cannot determine home directory: %v", err))
			}
			path = home + "/.config/entire/tokens.json"
		}
		return &fileStore{path: path}
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

// keyringStore delegates to the OS keyring.
type keyringStore struct{}

func (keyringStore) Get(service, user string) (string, error) {
	//nolint:wrapcheck // thin wrapper, callers handle errors
	return keyring.Get(service, user)
}

func (keyringStore) Set(service, user, password string) error {
	//nolint:wrapcheck // thin wrapper, callers handle errors
	return keyring.Set(service, user, password)
}

func (keyringStore) Delete(service, user string) error {
	//nolint:wrapcheck // thin wrapper, callers handle errors
	return keyring.Delete(service, user)
}
