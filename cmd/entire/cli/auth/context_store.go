package auth

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// RemoveCurrentContext deletes the active context's keyring tokens and its
// contexts.json entry, clearing current_context. It is a no-op (returns nil)
// when there is no current context. Used by logout.
func RemoveCurrentContext() error {
	if err := removeContextLocked(func(f *contexts.File) *contexts.Context {
		return f.Find(f.CurrentContext)
	}); err != nil {
		return fmt.Errorf("remove current context: %w", err)
	}
	return nil
}

// RemoveContext deletes the named context's keyring tokens, then its
// contexts.json entry. A missing context is a no-op. Used by logout and
// `logout --all-contexts`. File.Delete clears current_context when name was
// the active one, so removing the current context this way also logs it out.
func RemoveContext(name string) error {
	if err := removeContextLocked(func(f *contexts.File) *contexts.Context {
		return f.Find(name)
	}); err != nil {
		return fmt.Errorf("remove context %q: %w", name, err)
	}
	return nil
}

// RememberJurisdictionAudience adds audience to context `name`'s
// JurisdictionAudiences, so logout can find the matching keyring slot.
// Idempotent: an already-recorded audience rewrites nothing.
//
// Callers MUST record before writing the token to the credential store — a
// persisted-but-unrecorded token is a bearer logout can't find, whereas a
// failed record that aborts the write costs only one token exchange.
func RememberJurisdictionAudience(name, audience string) error {
	aud := strings.TrimRight(strings.TrimSpace(audience), "/")
	if name == "" || aud == "" {
		return errors.New("context name and jurisdiction audience are both required")
	}
	if err := contexts.Modify(userdirs.Config(), func(f *contexts.File) (bool, error) {
		c := f.Find(name)
		if c == nil {
			return false, fmt.Errorf("no login context named %q", name)
		}
		if slices.Contains(c.JurisdictionAudiences, aud) {
			return false, nil
		}
		c.JurisdictionAudiences = append(c.JurisdictionAudiences, aud)
		return true, nil
	}); err != nil {
		return fmt.Errorf("record jurisdiction audience %q for context %q: %w", aud, name, err)
	}
	return nil
}

// removeContextLocked deletes the context selected by pick — keyring slots
// first, then the contexts.json entry — inside a single locked Modify, so
// selection, credential deletion, and entry removal can't interleave with a
// concurrent `auth use` or login. A nil pick result is a no-op.
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
		// contexts.json, and would resolve to the bare service prefix — no
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

// SetCurrentContext makes name the active context. Returns an error when
// no context with that name exists (a stale current pointer is a foot-gun).
func SetCurrentContext(name string) error {
	if err := contexts.Modify(userdirs.Config(), func(f *contexts.File) (bool, error) {
		if f.Find(name) == nil {
			return false, fmt.Errorf("no login context named %q (run `entire auth contexts` to list)", name)
		}
		if f.CurrentContext == name {
			return false, nil
		}
		f.CurrentContext = name
		return true, nil
	}); err != nil {
		return fmt.Errorf("set current context: %w", err)
	}
	return nil
}

// Contexts returns all stored login contexts and the current context name,
// for listing/switching. Order matches on-disk order.
func Contexts() ([]*contexts.Context, string, error) {
	f, err := contexts.Load(userdirs.Config())
	if err != nil {
		return nil, "", fmt.Errorf("load contexts: %w", err)
	}
	return f.Contexts, f.CurrentContext, nil
}
