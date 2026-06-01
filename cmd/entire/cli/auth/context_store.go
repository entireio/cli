package auth

import (
	"fmt"

	"github.com/entireio/auth-go/tokens"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
)

// CurrentContextToken returns the login JWT for the active context in
// contexts.json, or ("", false) when there is no current context or it
// has no stored token. This is the contexts.json half of the CLI's
// credential resolution; callers fall back to the legacy keyring entry so
// pre-contexts logins keep working until migrated.
func CurrentContextToken() (string, bool) {
	f, err := contexts.Load(contexts.DefaultConfigDir())
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
// its keyring token, advancing current_context to whatever remains. It is
// a no-op (returns nil) when there is no current context. Used by logout.
func RemoveCurrentContext() error {
	// Read-modify-write in a single locked Modify so the context we delete
	// is exactly the one we capture the keychain slot from (separate Load +
	// Modify would race a concurrent `auth use`).
	var svc, handle string
	if err := contexts.Modify(contexts.DefaultConfigDir(), func(f *contexts.File) (bool, error) {
		current := f.Find(f.CurrentContext)
		if current == nil {
			return false, nil
		}
		svc, handle = current.KeychainService, current.Handle
		f.Delete(current.Name)
		// Delete advances current_context to a surviving context; for logout
		// that would silently re-authenticate as another account. Leave no
		// active context — logged out means logged out.
		f.CurrentContext = ""
		return true, nil
	}); err != nil {
		return fmt.Errorf("remove current context: %w", err)
	}
	// Best-effort keychain cleanup, sequenced off the context just removed.
	// A missing entry is fine — the contexts.json removal above is what makes
	// us "logged out".
	if svc != "" && handle != "" {
		_ = tokenstore.Delete(svc, handle) //nolint:errcheck // best-effort; contexts.json removal is the source of truth for logout
	}
	return nil
}

// RemoveAllContexts deletes every stored context, its keyring token, and all
// cluster bindings — a full local logout. Returns the number of contexts
// removed. Best-effort on the keyring deletes; the contexts.json clear is
// what makes the CLI fully logged out (no surviving binding can authenticate
// a clone afterward).
func RemoveAllContexts() (int, error) {
	var removed int
	if err := contexts.Modify(contexts.DefaultConfigDir(), func(f *contexts.File) (bool, error) {
		if len(f.Contexts) == 0 && f.CurrentContext == "" && len(f.ClusterContexts) == 0 {
			return false, nil
		}
		for _, c := range f.Contexts {
			if c.KeychainService != "" && c.Handle != "" {
				_ = tokenstore.Delete(c.KeychainService, c.Handle) //nolint:errcheck // best-effort; the contexts.json clear below is authoritative
			}
			removed++
		}
		f.Contexts = nil
		f.CurrentContext = ""
		f.ClusterContexts = nil
		return true, nil
	}); err != nil {
		return 0, fmt.Errorf("remove all contexts: %w", err)
	}
	return removed, nil
}

// SetCurrentContext makes name the active context. Returns an error when
// no context with that name exists (a stale current pointer is a foot-gun).
func SetCurrentContext(name string) error {
	if err := contexts.Modify(contexts.DefaultConfigDir(), func(f *contexts.File) (bool, error) {
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
	f, err := contexts.Load(contexts.DefaultConfigDir())
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
