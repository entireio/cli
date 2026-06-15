package auth

import (
	"fmt"

	"github.com/entireio/auth-go/tokens"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// CurrentContextToken returns the login JWT for the active context in
// contexts.json, or ("", false) when there is no current context or it
// has no stored token. This is the contexts.json half of the CLI's
// credential resolution; callers fall back to the legacy keyring entry so
// pre-contexts logins keep working until migrated.
func CurrentContextToken() (string, bool) {
	f, err := contexts.Load(userdirs.Config())
	if err != nil {
		return "", false
	}
	c := f.Find(f.CurrentContext)
	if c == nil {
		return "", false
	}
	tok, err := LoginTokenForContext(c)
	if err != nil || tok == "" {
		return "", false
	}
	return tok, true
}

// RemoveCurrentContext deletes the active context from contexts.json and
// its keyring token, clearing current_context. It is a no-op (returns nil)
// when there is no current context. Used by logout.
func RemoveCurrentContext() error {
	// Read-modify-write in a single locked Modify so the context we delete
	// is exactly the one we capture the keychain slot from (separate Load +
	// Modify would race a concurrent `auth use`).
	var svc, handle string
	if err := contexts.Modify(userdirs.Config(), func(f *contexts.File) (bool, error) {
		current := f.Find(f.CurrentContext)
		if current == nil {
			return false, nil
		}
		svc, handle = current.KeychainService, current.Handle
		// Delete clears current_context because we're deleting the active
		// one — logged out means logged out, no switch to another identity.
		f.Delete(current.Name)
		return true, nil
	}); err != nil {
		return fmt.Errorf("remove current context: %w", err)
	}
	deleteContextKeychain(svc, handle)
	return nil
}

// RemoveContext deletes the named context from contexts.json and its keyring
// tokens. A missing context is a no-op. Used by `logout --all-contexts` to
// drain every saved login. File.Delete clears current_context when name was
// the active one, so removing the current context this way also logs it out.
func RemoveContext(name string) error {
	var svc, handle string
	if err := contexts.Modify(userdirs.Config(), func(f *contexts.File) (bool, error) {
		c := f.Find(name)
		if c == nil {
			return false, nil
		}
		svc, handle = c.KeychainService, c.Handle
		f.Delete(name)
		return true, nil
	}); err != nil {
		return fmt.Errorf("remove context %q: %w", name, err)
	}
	deleteContextKeychain(svc, handle)
	return nil
}

// deleteContextKeychain best-effort removes a context's keyring slots,
// sequenced off the context just removed from contexts.json. A missing entry
// is fine — the contexts.json removal is what makes us "logged out". Both the
// access slot and its paired refresh slot must go: leaving the long-lived
// refresh token behind would let any later keyring-capable process mint fresh
// access tokens after logout.
func deleteContextKeychain(svc, handle string) {
	if svc == "" || handle == "" {
		return
	}
	_ = tokenstore.Delete(svc, handle)                            //nolint:errcheck // best-effort; contexts.json removal is the source of truth for logout
	_ = tokenstore.Delete(tokenstore.RefreshService(svc), handle) //nolint:errcheck // best-effort; absent refresh slot is fine
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

// ContextStore wraps the legacy keyring Store so token *reads* prefer the
// active contexts.json context, falling back to the legacy
// entire-cli/<authBaseURL> entry. Writes are inherited from Store
// unchanged — login dual-writes the context via RecordLoginContext, so the
// write side needs no override here.
//
// This is the single seam that lets the control-plane readers (the
// tokenmanager, LookupCurrentToken, and `auth status`/`list`) honor a
// contexts.json login — including one created by entiredb's CLIs that
// share this file. *ContextStore satisfies both the cli package's
// tokenStore interface and auth-go's tokenstore.Store.
type ContextStore struct {
	*Store
}

// NewContextStore returns a context-preferring view over the legacy store.
func NewContextStore() *ContextStore {
	return &ContextStore{Store: NewStore()}
}

// GetToken prefers the active context's token, falling back to the legacy
// entry keyed by baseURL.
func (s *ContextStore) GetToken(baseURL string) (string, error) {
	if tok, ok := CurrentContextToken(); ok {
		return tok, nil
	}
	return s.Store.GetToken(baseURL)
}

// LoadTokens (the tokenstore.Store method the tokenmanager calls) prefers
// the active context's token, falling back to the legacy profile entry.
func (s *ContextStore) LoadTokens(profile string) (tokens.TokenSet, error) {
	if tok, ok := CurrentContextToken(); ok {
		return tokens.TokenSet{AccessToken: tok}, nil
	}
	return s.Store.LoadTokens(profile)
}
