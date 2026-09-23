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

// RemoveContext deletes the named context's keyring tokens, then its
// contexts.json entry. A missing context is a no-op. File.Delete clears
// current_context when name was the active one.
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
// concurrent `auth switch` or login. A nil pick result is a no-op.
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

// Contexts returns all stored login contexts and the name of the one currently
// acting, for listing/switching and for the status/logout targets. Order matches
// on-disk order.
//
// The second return is the EFFECTIVE active name, so a `--context`/
// $ENTIRE_CONTEXT selection is what `auth status` reports, what `auth contexts`
// marks, and what `logout` revokes — the alternative is status describing one
// identity while every other command uses another.
func Contexts() ([]*contexts.Context, string, error) {
	f, err := contexts.Load(userdirs.Config())
	if err != nil {
		return nil, "", fmt.Errorf("load contexts: %w", err)
	}
	sel, err := f.Active()
	if err != nil {
		return nil, "", err //nolint:wrapcheck // UnknownContextError is already a complete operator message
	}
	if sel.Context == nil {
		return f.Contexts, "", nil
	}
	return f.Contexts, sel.Context.Name, nil
}

// ActiveContext returns the selected login, or ok=false when there is none. It
// is a plain accessor: it reads, it does not announce. Callers that go on to
// *act* as the login want ActingContext instead.
//
// A context with no CoreURL is reported as ok=false rather than returned: it is
// an unusable pointer, and treating it as active means dialing an empty host
// instead of telling the user to log in.
//
// A `--context`/$ENTIRE_CONTEXT selection is honoured, so the identity resolved
// here is the one every other command acts as. An explicit selection naming no
// saved context is a hard error, not ok=false: "you asked for a context that
// doesn't exist" must not degrade into the `entire login` hint.
func ActiveContext() (c *contexts.Context, ok bool, err error) {
	_, c, ok, err = activeContextIn()
	return c, ok, err
}

// ActingContext is ActiveContext for a caller that is about to act as the
// login: same resolution, plus the notice naming it when several are saved.
//
// The two are separate so the side effect is visible at the call site. A caller
// that only *describes* the login — setup_identity's git-identity probe,
// DataBaseURL building a printed link — reads it with ActiveContext and stays
// silent, and a reader of either call can tell which it is without opening this
// file.
func ActingContext() (c *contexts.Context, ok bool, err error) {
	f, c, ok, err := activeContextIn()
	if ok {
		announceContext(len(f.Contexts), c)
	}
	return c, ok, err
}

func activeContextIn() (f *contexts.File, c *contexts.Context, ok bool, err error) {
	f, err = contexts.Load(userdirs.Config())
	if err != nil {
		return nil, nil, false, fmt.Errorf("load contexts: %w", err)
	}
	sel, err := f.Active()
	if err != nil {
		return nil, nil, false, err //nolint:wrapcheck // UnknownContextError is already a complete operator message
	}
	if sel.Context == nil || strings.TrimSpace(sel.Context.CoreURL) == "" {
		return f, nil, false, nil
	}
	return f, sel.Context, true, nil
}

// StoredContexts returns all stored login contexts and the STORED
// current_context, ignoring any `--context`/$ENTIRE_CONTEXT override.
//
// Use this for questions about what is *persisted* — which logins exist, does
// a default exist — as opposed to which identity is *acting*, which is
// Contexts. `logout` sweeps every stored login through this, so an override
// naming a context that does not exist neither narrows nor fails it.
func StoredContexts() ([]*contexts.Context, string, error) {
	f, err := contexts.Load(userdirs.Config())
	if err != nil {
		return nil, "", fmt.Errorf("load contexts: %w", err)
	}
	return f.Contexts, f.CurrentContext, nil
}
