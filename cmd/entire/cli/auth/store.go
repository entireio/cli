package auth

import (
	"errors"
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/zalando/go-keyring"
)

const keyringService = "entire-cli"

// Store manages CLI authentication tokens in the OS keyring.
type Store struct {
	service string
}

// NewStore returns a Store backed by the system keyring.
func NewStore() *Store {
	return &Store{service: keyringService}
}

// NewStoreWithService returns a Store with a custom keyring service name (for testing).
func NewStoreWithService(service string) *Store {
	return &Store{service: service}
}

// SaveToken persists an access token for the given base URL.
func (s *Store) SaveToken(baseURL, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("refusing to save empty token")
	}

	if err := keyring.Set(s.service, baseURL, token); err != nil {
		return fmt.Errorf("save token to keyring: %w", err)
	}

	return nil
}

// GetToken retrieves a stored token for the given base URL.
// Returns an empty string (and no error) if no token is stored.
func (s *Store) GetToken(baseURL string) (string, error) {
	token, err := keyring.Get(s.service, baseURL)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get token from keyring: %w", err)
	}

	return token, nil
}

// DeleteToken removes a stored token for the given base URL.
func (s *Store) DeleteToken(baseURL string) error {
	err := keyring.Delete(s.service, baseURL)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete token from keyring: %w", err)
	}

	return nil
}

// LookupCurrentToken retrieves the token for the current base URL.
func LookupCurrentToken() (string, error) {
	store := NewStore()
	return store.GetToken(api.BaseURL())
}
