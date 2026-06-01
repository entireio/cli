package auth

import (
	"errors"
	"fmt"
	"strings"

	"github.com/entireio/auth-go/tokens"
	"github.com/entireio/auth-go/tokenstore"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/zalando/go-keyring"
)

// keyringService is the OS-keyring service name for this CLI. Renaming
// would orphan every existing user's stored credentials — they'd appear
// logged out until they ran `entire login` again. Don't change this
// without a migration path.
const keyringService = "entire-cli"

// Store manages CLI authentication tokens via a pluggable backend. The
// production binary always resolves to the OS keyring. A file-backed
// backend is available only in builds tagged `authfilestore` (used by
// integration tests to avoid the OS keychain).
//
// Implements tokenstore.Store so it can be passed to tokenmanager.New
// as the persistence layer. The interface methods (SaveTokens /
// LoadTokens / DeleteTokens) delegate to the same backend as the
// legacy SaveToken / GetToken / DeleteToken pair, so production and
// test paths share a single source of truth.
type Store struct {
	service string
	backend tokenBackend
}

// tokenBackend abstracts token persistence. Implementations must treat
// "missing key" as a non-error: get returns ("", nil) and delete is a
// no-op so callers don't have to plumb backend-specific sentinels.
type tokenBackend interface {
	save(service, key, value string) error
	get(service, key string) (string, error)
	delete(service, key string) error
}

// chooseBackend returns the backend used by NewStore and
// NewStoreWithService. The default returns the keyring backend; the
// `authfilestore` build adds an init() that may swap in a file-backed
// backend when the test env var is set.
var chooseBackend = func() tokenBackend { return keyringBackend{} }

// NewStore returns a Store backed by the system keyring (or, in
// `authfilestore` builds, optionally a file-backed test store).
func NewStore() *Store {
	return &Store{service: keyringService, backend: chooseBackend()}
}

// NewStoreWithService returns a Store with a custom keyring service name (for testing).
// Honors the same backend selection as NewStore so tests that opt into the
// file-backed test store via env var see consistent behavior across both
// constructors.
func NewStoreWithService(service string) *Store {
	return &Store{service: service, backend: chooseBackend()}
}

// SaveToken persists an access token for the given base URL. Prefer
// SaveTokens (the tokenstore.Store interface method) for new callers;
// SaveToken is kept for the legacy direct-bearer call sites (login,
// logout, auth status/list/revoke) that don't go through the tokenmanager.
func (s *Store) SaveToken(baseURL, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("refusing to save empty token")
	}
	return s.backend.save(s.service, baseURL, token)
}

// GetToken retrieves a stored token for the given base URL. Returns
// an empty string (and no error) if no token is stored, or if the
// stored value is JSON-shaped (defensive: pre-shim entries are
// opaque token strings, never JSON; a JSON blob in the keyring is
// corruption and must not be put on the wire as a bearer).
//
// Prefer LoadTokens (the tokenstore.Store interface method) for new
// callers — it returns the full TokenSet so refresh tokens and expiry
// survive the round trip. GetToken is retained for the direct-bearer
// call sites that only need the access token string.
func (s *Store) GetToken(baseURL string) (string, error) {
	raw, err := s.backend.get(s.service, baseURL)
	if err != nil {
		return "", err
	}
	if looksJSONShaped(raw) {
		return "", nil
	}
	return raw, nil
}

// looksJSONShaped reports whether the keyring value's first
// non-whitespace byte is '{' or '['. Used to reject corrupt /
// previous-encoding entries before they end up in an
// Authorization: Bearer header.
func looksJSONShaped(s string) bool {
	trimmed := strings.TrimLeft(s, " \t\r\n")
	if trimmed == "" {
		return false
	}
	return trimmed[0] == '{' || trimmed[0] == '['
}

// DeleteToken removes a stored token for the given base URL. Returns
// no error if the token does not exist. Prefer DeleteTokens (the
// tokenstore.Store interface method); DeleteToken is retained for
// direct-bearer call sites.
func (s *Store) DeleteToken(baseURL string) error {
	return s.backend.delete(s.service, baseURL)
}

// SaveTokens implements tokenstore.Store. Refresh token, scope, expiry,
// and token type are intentionally dropped — the entire device-flow
// surface doesn't issue refresh tokens, and the legacy keyring/file
// layout stores bare access-token strings. If refresh-token support
// lands, this method (and the tokenBackend interface) become the
// migration point.
func (s *Store) SaveTokens(profile string, t tokens.TokenSet) error {
	access := strings.TrimSpace(t.AccessToken)
	if access == "" {
		return errors.New("refusing to save empty access token")
	}
	return s.backend.save(s.service, profile, access)
}

// LoadTokens implements tokenstore.Store. Reads the bare-string entry
// and wraps it back into a TokenSet. Returns tokenstore.ErrNotFound
// when nothing is stored under the profile (or the stored value is
// JSON-shaped — see GetToken's note about defensive rejection of
// non-token blobs) so callers can errors.Is against the lib sentinel.
func (s *Store) LoadTokens(profile string) (tokens.TokenSet, error) {
	access, err := s.backend.get(s.service, profile)
	if err != nil {
		return tokens.TokenSet{}, err
	}
	if access == "" || looksJSONShaped(access) {
		return tokens.TokenSet{}, tokenstore.ErrNotFound
	}
	return tokens.TokenSet{AccessToken: access}, nil
}

// DeleteTokens implements tokenstore.Store.
func (s *Store) DeleteTokens(profile string) error {
	return s.backend.delete(s.service, profile)
}

// LookupCurrentToken retrieves the active login token. It prefers the
// current contexts.json context (so a login from this or entiredb's CLIs
// authenticates control-plane commands), falling back to the legacy entry
// keyed by the auth issuer (api.AuthBaseURL()) for pre-contexts logins.
func LookupCurrentToken() (string, error) {
	return NewContextStore().GetToken(api.AuthBaseURL())
}

type keyringBackend struct{}

func (keyringBackend) save(service, key, value string) error {
	if err := keyring.Set(service, key, value); err != nil {
		return fmt.Errorf("save token to keyring: %w", err)
	}
	return nil
}

func (keyringBackend) get(service, key string) (string, error) {
	token, err := keyring.Get(service, key)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get token from keyring: %w", err)
	}
	return token, nil
}

func (keyringBackend) delete(service, key string) error {
	err := keyring.Delete(service, key)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete token from keyring: %w", err)
	}
	return nil
}
