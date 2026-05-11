package auth

import (
	"errors"
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/zalando/go-keyring"
)

const keyringService = "entire-cli"

// Store manages CLI authentication tokens via a pluggable backend. The
// production binary always resolves to the OS keyring. A file-backed
// backend is available only in builds tagged `authfilestore` (used by
// integration tests to avoid the OS keychain).
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

// SaveToken persists an access token for the given base URL.
func (s *Store) SaveToken(baseURL, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("refusing to save empty token")
	}
	return s.backend.save(s.service, baseURL, token)
}

// GetToken retrieves a stored token for the given base URL.
// Returns an empty string (and no error) if no token is stored.
func (s *Store) GetToken(baseURL string) (string, error) {
	return s.backend.get(s.service, baseURL)
}

// DeleteToken removes a stored token for the given base URL.
// Returns no error if the token does not exist.
func (s *Store) DeleteToken(baseURL string) error {
	return s.backend.delete(s.service, baseURL)
}

// LookupCurrentToken retrieves the token for the current base URL.
func LookupCurrentToken() (string, error) {
	store := NewStore()
	return store.GetToken(api.BaseURL())
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
