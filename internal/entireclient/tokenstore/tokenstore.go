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
//   - "entire:<cluster-host>"          — entiredb cluster login tokens
//   - "entire-core:<core-base-url>"    — entire-core control-plane tokens
//   - "entire-jurisdiction:<audience>" — jurisdiction (data-plane) access
//     tokens, keyed by jurisdiction audience
//   - "<service>:refresh"              — refresh-token entry paired with the
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
	ClusterKeyringPrefix      = "entire:"              // entiredb cluster-issued tokens
	CoreKeyringPrefix         = "entire-core:"         // entire-core control-plane tokens
	JurisdictionKeyringPrefix = "entire-jurisdiction:" // jurisdiction (data-plane) access tokens
)

// CoreKeyringService returns the service name for tokens issued by
// entire-core. coreURL is the base URL of the issuer; trailing slashes
// are normalized away so callers don't have to.
func CoreKeyringService(coreURL string) string {
	return CoreKeyringPrefix + strings.TrimRight(coreURL, "/")
}

// JurisdictionService returns the service name for a jurisdiction (data-plane)
// access token, keyed by the jurisdiction audience so tokens for different
// jurisdictions — and for prod vs staging — can't be confused. The account key
// is the login context's handle. Trailing slashes are normalized away so
// callers don't have to.
func JurisdictionService(audience string) string {
	return JurisdictionKeyringPrefix + strings.TrimRight(audience, "/")
}

// RefreshService returns the paired refresh-token service name for an
// access-token service, following the "<service>:refresh" convention
// documented in this package's service-name conventions. Callers store the
// raw refresh token under (RefreshService(service), user) alongside the
// access token at (service, user).
func RefreshService(service string) string {
	return service + ":refresh"
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

// BackendEnvVar selects the credential backend: set to "file" to use the
// JSON file store instead of the OS keyring. PathEnvVar overrides where the
// file store lives (default: tokens.json in the per-user config directory).
// Exported so user-facing guidance (e.g. login's headless hint) names the
// same variables this package actually reads.
const (
	BackendEnvVar = "ENTIRE_TOKEN_STORE"
	PathEnvVar    = "ENTIRE_TOKEN_STORE_PATH"
)

// FileBackendSelected reports whether the environment selects the file
// backend — the single predicate shared by backend resolution, provenance
// wording, and login's headless hint, so they can never disagree.
func FileBackendSelected() bool {
	return os.Getenv(BackendEnvVar) == "file"
}

// BackendDescription names the credential backend the current environment
// resolves to, for user-facing provenance lines (e.g. `entire auth status`).
// It mirrors resolveBackendLocked's env semantics — the production resolution
// — rather than introspecting the live backend, so test-only overrides don't
// leak into user-facing wording.
func BackendDescription() string {
	if FileBackendSelected() {
		return "file " + FileBackendPath()
	}
	return keyringProviderName()
}

// tokenStoreFileName is the file backend's name inside the per-user config dir.
const tokenStoreFileName = "tokens.json"

// FileBackendPath resolves where the file backend stores (or would store)
// tokens: PathEnvVar when set, else tokens.json in the per-user config
// directory. Exported so user-facing guidance can name the concrete path.
//
// The string form cannot report a rejected override, so it returns the path it
// would use; fileBackendPathChecked is what the store itself calls.
func FileBackendPath() string {
	path, _ := fileBackendPathChecked() //nolint:errcheck // see doc comment: the store reports it
	return path
}

// fileBackendPathChecked is FileBackendPath with the override check.
//
// The config-directory case is checked here rather than being left to the root
// that opens it, because there is no such root: fileStore.dir anchors on
// filepath.Dir of this path (one of the two places CLAUDE.md permits that, since
// PathEnvVar names a file the caller chose) and reaches it through
// filepath.Abs. That Abs is exactly the laundering the contexts and discovery
// roots stopped doing. Without a check here, ENTIRE_CONFIG_DIR=foo silently put
// bearer tokens at ./foo/tokens.json, which for a CLI run from a repository
// means inside the repository.
//
// PathEnvVar is deliberately NOT checked: it names a file the user chose
// directly, the same reasoning that exempts it from the root-base rule.
func fileBackendPathChecked() (string, error) {
	if path := os.Getenv(PathEnvVar); path != "" {
		return path, nil
	}
	dir, err := userdirs.ConfigDirChecked()
	if err != nil {
		return filepath.Join(dir, tokenStoreFileName), err
	}
	return filepath.Join(dir, tokenStoreFileName), nil
}

func resolveBackendLocked() store {
	if FileBackendSelected() {
		// Entire owns the directory only when it also picked it: an
		// explicit PathEnvVar names a location the user chose, and its
		// mode is theirs to set.
		//
		// pathErr is carried rather than resolved here: this function has no
		// error return, and swallowing it is what put tokens in the working
		// directory. Every operation reports it before touching the filesystem.
		path, pathErr := fileBackendPathChecked()
		return &fileStore{path: path, pathErr: pathErr, ownsDir: os.Getenv(PathEnvVar) == ""}
	}
	// Under `go test`, never fall through to the real OS keyring: a test
	// that forgets tokenstore.UseFileBackendForTesting would otherwise write
	// real keychain entries. The fallback file is per-process; tests that
	// need isolation from each other still swap in a per-test file via
	// UseFileBackendForTesting.
	if dir, ok := testdirs.Dir("tokenstore"); ok {
		return &fileStore{path: filepath.Join(dir, "tokens.json"), ownsDir: true}
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
