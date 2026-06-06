package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/entireio/auth-go/tokenmanager"
	"github.com/entireio/auth-go/tokens"
	authtokenstore "github.com/entireio/auth-go/tokenstore"

	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
)

// defaultSavedTokenTTL is the encoded keychain expiry used when a refreshed
// token carries no usable ExpiresAt. The server is the real authority; this
// only governs when local readers consider the cached token stale.
const defaultSavedTokenTTL = time.Hour

// contextTokenStore adapts one login context's keyring slots to auth-go's
// tokenstore.Store, so tokenmanager can load, refresh, and persist that
// context's credentials. It is bound to a specific (service, handle) at
// construction and ignores the profile argument: the cluster resolver has
// already chosen exactly one context, so there is no per-issuer account
// ambiguity to resolve here.
//
// Access token lives at `service`/`handle` (with the "|<expiry>" encoding
// the rest of the CLI reads); the refresh token lives raw at
// `service:refresh`/`handle`.
type contextTokenStore struct {
	service string
	handle  string
}

func (s contextTokenStore) LoadTokens(string) (tokens.TokenSet, error) {
	enc, err := tokenstore.Get(s.service, s.handle)
	// Map "no credential stored" to auth-go's sentinel so tokenmanager
	// reports "not logged in" rather than a hard store failure.
	if errors.Is(err, tokenstore.ErrNotFound) || (err == nil && enc == "") {
		return tokens.TokenSet{}, authtokenstore.ErrNotFound
	}
	if err != nil {
		return tokens.TokenSet{}, fmt.Errorf("read access token: %w", err)
	}
	access, expiresAt := tokenstore.DecodeTokenWithExpiration(enc)
	// A missing refresh slot is fine (login predating offline_access) — treat
	// it as no-refresh. Any other store error must surface, not be swallowed:
	// dropping it would silently discard a valid refresh token and force a
	// re-login on what was really a transient keyring/file-store failure.
	refresh, err := tokenstore.Get(tokenstore.RefreshService(s.service), s.handle)
	if err != nil && !errors.Is(err, tokenstore.ErrNotFound) {
		return tokens.TokenSet{}, fmt.Errorf("read refresh token: %w", err)
	}
	return tokens.TokenSet{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresAt:    expiresAt,
	}, nil
}

func (s contextTokenStore) SaveTokens(_ string, t tokens.TokenSet) error {
	if t.AccessToken == "" {
		return errors.New("save tokens: empty access token")
	}
	expiresIn := int64(defaultSavedTokenTTL.Seconds())
	if !t.ExpiresAt.IsZero() {
		if secs := int64(time.Until(t.ExpiresAt).Seconds()); secs > 0 {
			expiresIn = secs
		}
	}
	// Persist the rotated refresh token BEFORE the access token. The server
	// single-use-rotates refresh tokens, so a partial write must never leave
	// a fresh access token paired with a stale refresh token: that pairing
	// looks healthy until the access token expires, then the dead refresh
	// token trips invalid_grant/family revocation and forces a re-login.
	// Refresh-first inverts the failure modes: a failed refresh write aborts
	// before touching the access slot (old pair preserved), and a failed
	// access write after a good refresh write self-heals on the next load
	// (the new refresh token re-mints an access token).
	//
	// persistRefreshed carries a still-valid refresh token forward when the
	// server doesn't rotate, so an empty value here means "leave as-is",
	// never "clear".
	if t.RefreshToken != "" {
		if err := tokenstore.Set(tokenstore.RefreshService(s.service), s.handle, t.RefreshToken); err != nil {
			return fmt.Errorf("store refresh token: %w", err)
		}
	}
	if err := tokenstore.Set(s.service, s.handle, tokenstore.EncodeTokenWithExpiration(t.AccessToken, expiresIn)); err != nil {
		return fmt.Errorf("store access token: %w", err)
	}
	return nil
}

func (s contextTokenStore) DeleteTokens(string) error {
	_ = tokenstore.Delete(tokenstore.RefreshService(s.service), s.handle) //nolint:errcheck // best-effort; the access-token delete below is what matters
	if err := tokenstore.Delete(s.service, s.handle); err != nil {
		return fmt.Errorf("delete access token: %w", err)
	}
	return nil
}

// NewRefreshingLoginProvider returns a login-JWT provider (the shape
// repocreds wants) for context c that transparently re-mints an expired
// login JWT from the stored refresh token.
//
// It is backed by auth-go's tokenmanager, which is what makes this safe
// against the server's single-use refresh-token rotation: refreshes are
// serialised across processes (an advisory file lock) and goroutines, the
// store is re-read after locking so a late waiter reuses a peer's freshly
// minted token, and the rotated refresh token is persisted. Without that,
// two concurrent git-remote-entire processes (e.g. a recursive submodule
// fetch) could replay the same single-use token and trip the server's
// reuse detection, revoking the whole family.
//
// Behaviour is a strict superset of the old read-only provider: a still
// valid token is returned with no network call; a context with no refresh
// token (e.g. a login predating offline_access) behaves exactly as before
// — valid token used, expired token surfaces a re-login error.
//
// transport carries the caller's TLS configuration; allowInsecureHTTP
// permits an http:// core for loopback/dev.
func NewRefreshingLoginProvider(c *contexts.Context, transport http.RoundTripper, allowInsecureHTTP bool) (func(context.Context) (string, error), error) {
	if c == nil {
		return nil, errors.New("nil context")
	}
	if c.KeychainService == "" || c.Handle == "" {
		return nil, fmt.Errorf("context %q has no keychain slot", c.Name)
	}
	mgr, err := tokenmanager.New(tokenmanager.Config{
		Issuer:            strings.TrimRight(c.CoreURL, "/"),
		ClientID:          CurrentProvider().ClientID,
		RefreshPath:       CurrentProvider().TokenPath,
		Store:             contextTokenStore{service: c.KeychainService, handle: c.Handle},
		Transport:         transport,
		AllowInsecureHTTP: allowInsecureHTTP,
		UserAgent:         CurrentProvider().ClientID,
	})
	if err != nil {
		return nil, fmt.Errorf("init token manager for context %q: %w", c.Name, err)
	}
	name := c.Name
	coreURL := strings.TrimRight(c.CoreURL, "/")
	// Name the core in the re-login hint so a multi-core user logs back
	// into the right one; matches clusterdiscovery.RenderLoginHint's idiom.
	relogin := fmt.Sprintf("ENTIRE_AUTH_BASE_URL=%s entire login", coreURL)
	return func(ctx context.Context) (string, error) {
		tok, err := mgr.Refresh(ctx)
		switch {
		case errors.Is(err, tokenmanager.ErrReauthRequired):
			return "", fmt.Errorf("login session for %q (%s) expired; run `%s` to re-authenticate", name, coreURL, relogin)
		case errors.Is(err, tokenmanager.ErrNotLoggedIn):
			return "", fmt.Errorf("no usable login for %q (%s); run `%s`", name, coreURL, relogin)
		case err != nil:
			return "", fmt.Errorf("refresh login token: %w", err)
		}
		return tok, nil
	}, nil
}
