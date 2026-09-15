// Package tokenstore provides a pluggable credential store shared by the
// entiredb and entire-core CLIs.
//
// By default it delegates to the OS keyring (macOS Keychain, Linux Secret
// Service, etc.). Set ENTIRE_TOKEN_STORE=file to use a JSON file instead. On
// Linux and the BSDs the CLI switches to the file on its own when the keyring
// is unavailable, and remembers the choice in token_store.json next to
// contexts.json — see preference.go and fallback.go.
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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
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

// backendMu guards `resolved`, `backend`, and `adoptedFile` across
// currentBackend's lazy resolve, setBackend's adoption (the fallback moving
// the process onto the file store), and the test-only overrides in
// testing.go, so the package-level state stays well-defined even when tests
// reset it.
var (
	backendMu sync.Mutex
	resolved  bool
	backend   store
	// adoptedFile records that the fallback adopted the file store in THIS
	// process. Provenance reads it where no marker can speak: with
	// ENTIRE_TOKEN_STORE_PATH set the marker is never written, and without
	// this flag `auth status` named the keyring for a token it had just read
	// from the file. Set by setBackend; cleared only by the test-only
	// overrides' restore functions.
	adoptedFile bool
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

// BackendEnvVar selects the credential backend explicitly: "file" uses the
// JSON file store, any other non-empty value (canonically "keyring") uses the
// OS keyring. An explicit selection is never overridden by the remembered
// preference and never falls back (under `go test` a non-`file` value is
// ignored so the test store still wins — see `resolveBackendLocked`); a
// successful write through it becomes the remembered preference (see
// preference.go). Unset means: remembered preference, else the platform
// default. PathEnvVar overrides where the file store lives (default:
// tokens.json in the per-user config directory).
// Exported so user-facing guidance (e.g. login's headless hint) names the
// same variables this package actually reads.
const (
	BackendEnvVar = "ENTIRE_TOKEN_STORE"
	PathEnvVar    = "ENTIRE_TOKEN_STORE_PATH"
)

// selectedBackend reports which backend the environment, this process's own
// adoption, and the remembered preference pick: BackendEnvVar when set, else
// the file store if the fallback adopted it in this process (adoptedFile — the
// only record of the switch when ENTIRE_TOKEN_STORE_PATH stops the marker
// being written), else the marker. "" means none of them said anything and
// the platform default applies. resolveBackend reads the environment and the
// marker separately (an explicit selection is recorded after a write, a
// remembered one is not); this is the combined view for callers that only
// ask "which store?".
func selectedBackend() string {
	if v := os.Getenv(BackendEnvVar); v != "" {
		if v == backendFile {
			return backendFile
		}
		return backendKeyring
	}
	if fileAdopted() {
		return backendFile
	}
	return persistedBackend()
}

// fileAdopted reads adoptedFile under the backend lock.
func fileAdopted() bool {
	backendMu.Lock()
	defer backendMu.Unlock()
	return adoptedFile
}

// FileBackendSelected reports whether the file backend is selected, by the
// environment or by the remembered preference — the single predicate shared
// by provenance wording and login's headless hint, so they can never disagree
// with each other or with resolution.
func FileBackendSelected() bool {
	return selectedBackend() == backendFile
}

// BackendDescription names the credential backend the current environment,
// this process's adoption, and the remembered preference resolve to, for
// user-facing provenance lines (e.g. `entire auth status`). It mirrors
// selectedBackend — the production resolution — rather than introspecting the
// live backend, so test-only overrides don't leak into user-facing wording;
// the one piece of live state it reads is the fallback's adoption flag,
// because that switch is real and, with ENTIRE_TOKEN_STORE_PATH set, is
// recorded nowhere else.
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

// defaultFileStore is the file store at FileBackendPath. Entire owns the
// directory only when it also picked it: an explicit PathEnvVar names a
// location the user chose, and its mode is theirs to set.
//
// pathErr is carried rather than resolved here because the callers have no
// error return, and swallowing it is what once put tokens in the working
// directory. Every operation reports it before touching the filesystem.
func defaultFileStore() *fileStore {
	path, pathErr := fileBackendPathChecked()
	return &fileStore{path: path, pathErr: pathErr, ownsDir: os.Getenv(PathEnvVar) == ""}
}

// backendInputs are the facts resolveBackend decides on. resolveBackendLocked
// gathers them once so the decision is a pure function tests can drive
// through every branch — including the keyring branches that `go test` never
// reaches on its own, because the testdirs store sits in front of them.
type backendInputs struct {
	envValue   string // BackendEnvVar, "" when unset
	remembered string // persistedBackend(), consulted only when envValue is ""
	testDir    string // testdirs.Dir("tokenstore"); "" outside `go test`
	goos       string // runtime.GOOS
}

// resolveBackend applies the precedence documented on BackendEnvVar:
// explicit env, then remembered preference, then the test store, then the
// platform default. Under `go test` the test store must beat the keyring so a
// test that forgets UseFileBackendForTesting cannot write real keychain
// entries; the remembered preference beats the test store because a test
// that wrote the marker is testing exactly that. The decision is pure over
// its inputs; constructing a file store still reads the path environment
// (PathEnvVar and the config dir), which is why the tests that reach those
// branches isolate it.
func resolveBackend(in backendInputs) store {
	if in.envValue != "" {
		if in.envValue == backendFile {
			return recordingStore{inner: defaultFileStore(), name: backendFile}
		}
		return recordingStore{inner: keyringStore{}, name: backendKeyring}
	}
	if in.remembered == backendFile {
		return defaultFileStore()
	}
	if in.testDir != "" {
		return &fileStore{path: filepath.Join(in.testDir, "tokens.json"), ownsDir: true}
	}
	if isSecretServicePlatform(in.goos) {
		return newFallbackStore(keyringStore{})
	}
	return keyringStore{}
}

func resolveBackendLocked() store {
	in := backendInputs{envValue: os.Getenv(BackendEnvVar), goos: runtime.GOOS}
	if dir, ok := testdirs.Dir("tokenstore"); ok {
		in.testDir = dir
		// Under `go test`, an explicit keyring selection inherited from the
		// developer's shell is not a test asking for the real OS keyring:
		// drop it so the test store still wins. An explicit "file" stays
		// honoured, since the file store is what the test harnesses select
		// on purpose. The pure resolver below keeps honouring the explicit
		// value; only this gathering step knows it is running under test.
		if in.envValue != "" && in.envValue != backendFile {
			in.envValue = ""
		}
	}
	if in.envValue == "" {
		in.remembered = persistedBackend()
	}
	return resolveBackend(in)
}

// recordingStore wraps the backend BackendEnvVar selected explicitly and,
// after each successful write, remembers that selection — so a one-off
// `ENTIRE_TOKEN_STORE=file entire login` sticks for every later process, and
// an explicit keyring login clears a stale file preference and removes the
// superseded copy of that credential from the default-path file store. Reads
// pass straight through and learn nothing; an explicit-keyring Delete also
// removes the file copy of the slot it deletes, because a login that carries
// no refresh token clears the refresh slot by Delete rather than Set.
type recordingStore struct {
	inner store
	name  string
}

func (r recordingStore) Get(service, user string) (string, error) {
	//nolint:wrapcheck // thin wrapper, callers handle errors
	return r.inner.Get(service, user)
}

func (r recordingStore) Set(service, user, password string) error {
	if err := r.inner.Set(service, user, password); err != nil {
		//nolint:wrapcheck // thin wrapper, callers handle errors
		return err
	}
	if err := rememberBackend(r.name); err != nil {
		// The credential is stored; failing to remember where is a
		// degradation (the next process may need ENTIRE_TOKEN_STORE), not a
		// failed login. Say so rather than failing or staying silent, and
		// say what it costs: the store that was just written is the one a
		// later process has to be pointed at by hand.
		fmt.Fprintf(fallbackNoticeW, "Warning: could not remember the token store choice: %v\nLater commands may not find this login without %s=%s; fix the Entire config directory and run entire login again to remember the choice.\n", err, BackendEnvVar, r.name)
	}
	if r.name == backendKeyring {
		removeSupersededFileCopy(service, user)
	}
	return nil
}

// removeSupersededFileCopy deletes the (service, user) entry from the
// default-path file store after an explicit keyring write or delete. Clearing
// the marker alone would leave a live bearer in tokens.json: a plaintext copy
// the user just chose to stop using, and one the Linux fallback would re-adopt
// on the next transient keyring failure (see switchTo in fallback.go). Only
// the default path is touched — a store named through PathEnvVar is the user's
// to manage — and a missing entry is the normal case.
//
// Whether the file exists is checked first, without creating anything: the
// file store's Delete creates the directory and a tokens.json.lock on its way
// to finding nothing, and a machine that has never used the file store must
// not gain either from a keyring login. A missing directory or file is the
// normal case; any other failure to look is reported like a failed removal,
// since from the user's side the outcome is the same.
func removeSupersededFileCopy(service, user string) {
	if !markerApplies() {
		return
	}
	root, err := userdirs.ConfigRootForRead()
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			warnSupersededCopyNotRemoved(err)
		}
		return
	}
	if _, err := root.Lstat(tokenStoreFileName); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			warnSupersededCopyNotRemoved(err)
		}
		return
	}
	if err := defaultFileStore().Delete(service, user); err != nil && !errors.Is(err, ErrNotFound) {
		warnSupersededCopyNotRemoved(err)
	}
}

// warnSupersededCopyNotRemoved says what a failed removal leaves behind and
// what to do about it. The credential itself was stored, so it is a warning.
func warnSupersededCopyNotRemoved(err error) {
	fmt.Fprintf(fallbackNoticeW, "Warning: could not remove the superseded copy of this credential from %s: %v\nA plaintext copy of the old token remains there; remove the entry or the file by hand.\n", FileBackendPath(), err)
}

func (r recordingStore) Delete(service, user string) error {
	err := r.inner.Delete(service, user)
	// A slot the keyring never held is still a slot the file may hold, so
	// ErrNotFound from the keyring does not stop the file cleanup.
	if r.name == backendKeyring && (err == nil || errors.Is(err, ErrNotFound)) {
		removeSupersededFileCopy(service, user)
	}
	//nolint:wrapcheck // thin wrapper, callers handle errors
	return err
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
