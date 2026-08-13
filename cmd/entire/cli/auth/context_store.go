package auth

import (
	"errors"
	"fmt"
	"strings"

	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// RemoveCurrentLogin deletes the current login's credentials and metadata.
// It is a no-op when logged out. Used by logout.
func RemoveCurrentLogin() error {
	if err := removeContextLocked(func(f *contexts.File) *contexts.Context {
		return f.Find(f.CurrentContext)
	}); err != nil {
		return fmt.Errorf("remove current login: %w", err)
	}
	return nil
}

// removeContextLocked deletes the selected login's credential slots first,
// then its metadata, under the login-store lock. A nil result is a no-op.
//
// Credential deletion comes first and is part of the success contract:
// removing the entry and then failing the keyring delete would report
// "Logged out." while the long-lived refresh token survives on the machine,
// mintable by any keyring-capable process. A delete error aborts the Modify,
// leaving the entry intact for a retry. The inverse partial failure (slots
// deleted, entry write fails) is benign — the context reads as not logged in
// and a retried logout no-ops the deletes.
func removeContextLocked(pick func(*contexts.File) *contexts.Context) error {
	//nolint:wrapcheck // callers wrap with their own operation context
	return contexts.Modify(userdirs.Config(), func(f *contexts.File) (bool, error) {
		c := pick(f)
		if c == nil {
			return false, nil
		}
		if err := deleteContextKeychain(c); err != nil {
			return false, fmt.Errorf("remove credentials for %q: %w", c.Name, err)
		}
		f.Delete(c.Name)
		return true, nil
	})
}

// deleteContextKeychain removes every keyring slot a context owns: the paired
// refresh + access tokens, plus one jurisdiction (data-plane) access token per
// recorded audience — each of those authorizes git against every repo the
// account can reach. A missing entry is fine; any other failure surfaces so
// logout doesn't claim success over surviving credentials.
//
// Deletion runs longest-lived-first — refresh (indefinite), jurisdiction (8h),
// access (an hour at most) — so a mid-sequence failure leaves behind only the
// shorter-lived credential. Unrecorded jurisdiction slots are unreachable (no
// enumeration API) and left to expire.
func deleteContextKeychain(c *contexts.Context) error {
	if c == nil || c.Handle == "" {
		return nil
	}
	if c.KeychainService != "" {
		if err := tokenstore.Delete(tokenstore.RefreshService(c.KeychainService), c.Handle); err != nil && !errors.Is(err, tokenstore.ErrNotFound) {
			return fmt.Errorf("delete refresh token: %w", err)
		}
	}
	for _, audience := range c.JurisdictionAudiences {
		// A blank entry can only come from a hand-edited or corrupted
		// migrated metadata, and would resolve to the bare service prefix — no
		// token lives there, so skip rather than round-trip the keyring.
		if strings.TrimSpace(audience) == "" {
			continue
		}
		if err := tokenstore.Delete(tokenstore.JurisdictionService(audience), c.Handle); err != nil && !errors.Is(err, tokenstore.ErrNotFound) {
			return fmt.Errorf("delete jurisdiction token for %s: %w", audience, err)
		}
	}
	if c.KeychainService != "" {
		if err := tokenstore.Delete(c.KeychainService, c.Handle); err != nil && !errors.Is(err, tokenstore.ErrNotFound) {
			return fmt.Errorf("delete access token: %w", err)
		}
	}
	return nil
}

// CurrentLogin returns the one stored login, or nil when logged out.
func CurrentLogin() (*contexts.Context, error) {
	f, err := contexts.Load(userdirs.Config())
	if err != nil {
		return nil, fmt.Errorf("load current login: %w", err)
	}
	return f.Find(f.CurrentContext), nil
}
