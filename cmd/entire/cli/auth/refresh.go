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

// newContextTokenManager builds the per-context auth-go tokenmanager that both
// NewRefreshingLoginProvider and NewRefreshingResourceProvider sit on. Keying
// Issuer on c.CoreURL is the whole point: store reads, the refresh grant, and
// the STS exchange all target that context's core, so a multi-core user's
// credentials never travel to (or get keyed under) a host the context
// doesn't belong to.
//
// transport carries the caller's TLS configuration; allowInsecureHTTP permits
// an http:// core/resource for loopback/dev.
func newContextTokenManager(c *contexts.Context, transport http.RoundTripper, allowInsecureHTTP bool) (*tokenmanager.Manager, error) {
	if c == nil {
		return nil, errors.New("nil context")
	}
	if c.KeychainService == "" || c.Handle == "" {
		return nil, fmt.Errorf("context %q has no keychain slot", c.Name)
	}
	mgr, err := tokenmanager.New(tokenmanager.Config{
		Issuer:            strings.TrimRight(c.CoreURL, "/"),
		ClientID:          oauthClientID,
		STSPath:           oauthSTSPath,
		RefreshPath:       oauthTokenPath,
		Store:             contextTokenStore{service: c.KeychainService, handle: c.Handle},
		Transport:         transport,
		AllowInsecureHTTP: allowInsecureHTTP,
		UserAgent:         oauthClientID,
	})
	if err != nil {
		return nil, fmt.Errorf("init token manager for context %q: %w", c.Name, err)
	}
	return mgr, nil
}

// reauthError carries a friendly, context-named re-login message while still
// unwrapping to the underlying tokenmanager sentinel, so callers that branch
// on errors.Is(err, ErrNotLoggedIn) (NewAuthenticatedAPIClient, search,
// dispatch) keep matching. Error() returns only msg so the sentinel's terse
// text ("not logged in") doesn't leak into the rendered message.
type reauthError struct {
	msg      string
	sentinel error
}

func (e *reauthError) Error() string { return e.msg }
func (e *reauthError) Unwrap() error { return e.sentinel }

// contextReauthError maps the two re-auth sentinels a per-context manager can
// return into a friendly message that names the context and its core (so a
// multi-core user logs back into the right one — matching
// clusterdiscovery.RenderLoginHint's idiom), preserving the sentinel for
// errors.Is. Returns nil when err is neither sentinel, leaving the caller to
// wrap the residual error in its own terms (refresh vs exchange).
func contextReauthError(c *contexts.Context, err error) error {
	coreURL := strings.TrimRight(c.CoreURL, "/")
	switch {
	case errors.Is(err, tokenmanager.ErrReauthRequired):
		return &reauthError{
			msg:      fmt.Sprintf("login session for %q (%s) expired; run `entire login` to re-authenticate", c.Name, coreURL),
			sentinel: tokenmanager.ErrReauthRequired,
		}
	case errors.Is(err, tokenmanager.ErrNotLoggedIn):
		return &reauthError{
			msg:      fmt.Sprintf("no usable login for %q (%s); run `entire login`", c.Name, coreURL),
			sentinel: tokenmanager.ErrNotLoggedIn,
		}
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
// A still-valid token is returned with no network call. A context with no
// stored refresh token degrades gracefully: valid token used, expired token
// surfaces a re-login error.
//
// transport carries the caller's TLS configuration; allowInsecureHTTP
// permits an http:// core for loopback/dev.
func NewRefreshingLoginProvider(c *contexts.Context, transport http.RoundTripper, allowInsecureHTTP bool) (func(context.Context) (string, error), error) {
	mgr, err := newContextTokenManager(c, transport, allowInsecureHTTP)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) (string, error) {
		tok, err := mgr.Refresh(ctx)
		if mapped := contextReauthError(c, err); mapped != nil {
			return "", mapped
		}
		if err != nil {
			return "", fmt.Errorf("refresh login token: %w", err)
		}
		return tok, nil
	}, nil
}

// RefreshedLoginToken returns context c's login JWT, transparently re-minting
// an expired one from the stored refresh token. It is the convenience form of
// NewRefreshingLoginProvider for callers that want a single token now (e.g.
// `auth status` / `logout`, which must report a refreshable session as alive
// rather than telling the user to re-login). The insecure-HTTP decision mirrors
// the control-plane resolver: loopback cores and the --insecure-http-auth
// opt-in are permitted, everything else requires https.
//
// Errors preserve the tokenmanager sentinels (ErrReauthRequired when the
// session is genuinely dead, ErrNotLoggedIn when no credential is usable) so
// callers can branch on errors.Is.
func RefreshedLoginToken(ctx context.Context, c *contexts.Context) (string, error) {
	if c == nil {
		return "", errors.New("nil context")
	}
	provider, err := NewRefreshingLoginProvider(c, nil, insecureHTTPEnabled() || isLoopbackHTTP(c.CoreURL))
	if err != nil {
		return "", err
	}
	return provider(ctx)
}

// NewRefreshingResourceProvider returns a provider that mints a bearer valid
// for resourceOrigin, by exchanging context c's login JWT at c's own core (RFC
// 8693). It is NewRefreshingLoginProvider's sibling for resource servers: where
// that returns the bare login JWT (the control plane / cluster cases, where the
// host is the core), this performs the token exchange the data API requires.
//
// Both the silent login-JWT re-mint and the exchange run through the shared
// per-context tokenmanager (newContextTokenManager). resourceOrigin must
// already be origin-only (no path). No audience is passed: the token manager
// defaults the RFC 8693 audience to the resource origin, which is exactly what
// the data API requires (aud == its base URI), so the audience is derived from
// the host being dialed rather than read from discovery. Exchanged tokens are
// cached in-process by the tokenmanager for the life of this process.
//
// transport carries the caller's TLS configuration; allowInsecureHTTP permits
// an http:// core/resource for loopback/dev.
func NewRefreshingResourceProvider(c *contexts.Context, resourceOrigin string, transport http.RoundTripper, allowInsecureHTTP bool) (func(context.Context) (string, error), error) {
	mgr, err := newContextTokenManager(c, transport, allowInsecureHTTP)
	if err != nil {
		return nil, err
	}
	req := tokenmanager.TokenRequest{Resource: resourceOrigin}
	return func(ctx context.Context) (string, error) {
		tok, err := mgr.Token(ctx, req)
		if mapped := contextReauthError(c, err); mapped != nil {
			return "", mapped
		}
		if err != nil {
			return "", fmt.Errorf("exchange token for %s: %w", resourceOrigin, err)
		}
		return tok, nil
	}, nil
}
