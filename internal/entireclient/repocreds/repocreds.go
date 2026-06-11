// Package repocreds exchanges a logged-in user's login JWT for short-lived
// repo-scoped JWTs (TokenScopedJWT) and caches them per (audience, action).
//
// Used by client/client.go to authenticate per-repo /api/v1/repos/... calls
// against entire-server, and by cmd/git-remote-entire to authenticate git
// smart-HTTP push/pull. Both surfaces share the same RFC 8693 token-exchange
// endpoint (POST /oauth/token on entire-core) but pass different audience
// shapes — by-ULID (/git/repo/<ULID>) for client.go, by-slug
// (/et/<owner>/<repo>) for git-remote-entire — so the Cache is neutral on
// the audience format: callers compute audienceSuffix themselves and the
// cache keys on it.
package repocreds

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/entireio/cli/internal/entireclient/httputil"
)

// SafetyMargin is subtracted from the server's expires_in so callers rotate
// before actual expiry. One minute covers clock skew and gives slow downstream
// RPCs a fresh token. Matches admincreds.safetyMargin so the two cred caches
// don't diverge on freshness semantics.
const SafetyMargin = time.Minute

// oauthClientID is the public OAuth client_id the CLI identifies as on
// /oauth/token. Lifted into Basic auth by httputil.PostOAuthToken.
const oauthClientID = "entire-cli"

// LoginJWTProvider returns the current login JWT. Callers wire this to their
// own refresh logic (e.g. proactive refresh, 401 retry) so the Cache always
// exchanges with the live parent token, never a captured-then-stale copy.
type LoginJWTProvider func(ctx context.Context) (string, error)

// Cache holds repo-scoped JWTs minted by exchanging a login JWT at entire-core's
// /oauth/token endpoint. Keyed by (audienceSuffix, action) so a single Cache
// can hold tokens for many repos and both pull/push at once.
//
// The map mutex (mu) is held only long enough to find-or-create a per-key
// entry; the network exchange runs under the entry's own mutex. This lets
// exchanges for different keys proceed concurrently while still de-duplicating
// concurrent fetches for the same key.
type Cache struct {
	coreURL          string // trimmed, no trailing slash
	clusterURL       string // trimmed, no trailing slash
	loginJWTProvider LoginJWTProvider
	httpClient       *http.Client

	mu      sync.Mutex
	entries map[key]*entry
}

type key struct {
	audienceSuffix string
	action         string
}

type entry struct {
	mu        sync.Mutex // serializes exchange for this key
	token     string
	expiresAt time.Time
}

// New constructs a Cache. coreURL is the issuer URL of the login JWT;
// clusterURL is the entiredb cluster the resulting JWT will target (audience
// = clusterURL + audienceSuffix). Trailing slashes are trimmed.
func New(coreURL, clusterURL string, loginJWTProvider LoginJWTProvider, httpClient *http.Client) *Cache {
	return &Cache{
		coreURL:          strings.TrimRight(coreURL, "/"),
		clusterURL:       strings.TrimRight(clusterURL, "/"),
		loginJWTProvider: loginJWTProvider,
		httpClient:       httpClient,
		entries:          make(map[key]*entry),
	}
}

// Token returns a valid repo-scoped JWT for the (audienceSuffix, action)
// pair, exchanging against /oauth/token if no cached entry exists or the
// cached one has crossed the safety margin. Concurrent callers serialize on
// the mutex; a single fetch satisfies all waiters.
//
// audienceSuffix is the path portion of the audience claim — e.g.
// "/git/repo/<ULID>" or "/et/<owner>/<repo>". action is "pull" or "push".
func (c *Cache) Token(ctx context.Context, audienceSuffix, action string) (string, error) {
	if audienceSuffix == "" {
		return "", errors.New("repocreds: empty audienceSuffix")
	}
	if action == "" {
		return "", errors.New("repocreds: empty action")
	}
	if c.coreURL == "" {
		return "", errors.New("repocreds: no entire-core URL configured for STS exchange")
	}

	e := c.entryFor(audienceSuffix, action)

	// Per-key lock: serializes concurrent fetches for the same key while
	// leaving other keys free to exchange in parallel. A slow exchange for
	// repo A no longer blocks a cache hit for repo B.
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.token != "" && time.Now().Before(e.expiresAt) {
		return e.token, nil
	}

	token, ttl, err := c.exchange(ctx, audienceSuffix, action)
	if err != nil {
		return "", err
	}
	if ttl <= 0 {
		// Already expired on arrival — hand it out once, cache nothing.
		e.token = ""
		e.expiresAt = time.Time{}
		return token, nil
	}
	// Rotate before expiry by SafetyMargin, but never reserve more than half
	// the lifetime: a short-lived token (TTL ≤ SafetyMargin) still gets cached
	// for the remaining half instead of being re-exchanged on every call.
	margin := min(SafetyMargin, ttl/2)
	e.token = token
	e.expiresAt = time.Now().Add(ttl - margin)
	return token, nil
}

// entryFor returns the per-key entry, creating it under the map mutex if
// absent. The map mutex is held only for this lookup, never across the
// network exchange.
func (c *Cache) entryFor(audienceSuffix, action string) *entry {
	k := key{audienceSuffix: audienceSuffix, action: action}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[k]
	if !ok {
		e = &entry{}
		c.entries[k] = e
	}
	return e
}

// Invalidate drops any cached token for (audienceSuffix, action). Callers
// invoke this on 401 from the data plane so the next Token call re-mints
// instead of replaying the rejected one.
func (c *Cache) Invalidate(audienceSuffix, action string) {
	c.mu.Lock()
	e, ok := c.entries[key{audienceSuffix: audienceSuffix, action: action}]
	c.mu.Unlock()
	if !ok {
		return
	}
	e.mu.Lock()
	e.token = ""
	e.expiresAt = time.Time{}
	e.mu.Unlock()
}

// exchange runs a single RFC 8693 token-exchange against /oauth/token. The
// issued JWT's aud claim is clusterURL+audienceSuffix; the receiver compares
// against the resolved repo, so this string IS the resource check.
func (c *Cache) exchange(ctx context.Context, audienceSuffix, action string) (string, time.Duration, error) {
	loginJWT, err := c.loginJWTProvider(ctx)
	if err != nil {
		return "", 0, err
	}
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:token-exchange")
	form.Set("subject_token_type", "urn:ietf:params:oauth:token-type:access_token")
	form.Set("subject_token", loginJWT)
	form.Set("requested_token_type", "urn:ietf:params:oauth:token-type:access_token")
	form.Set("audience", c.clusterURL+audienceSuffix)
	form.Set("scope", "repo:"+action)
	form.Set("client_id", oauthClientID)

	token, expiresIn, err := httputil.PostOAuthToken(ctx, c.httpClient, c.coreURL, form)
	if err != nil {
		// %w preserves *httputil.OAuthError so callers can errors.As it.
		return "", 0, fmt.Errorf("oauth token exchange: %w", err)
	}
	return token, time.Duration(expiresIn) * time.Second, nil
}
