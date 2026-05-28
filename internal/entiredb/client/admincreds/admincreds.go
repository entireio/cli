// Package admincreds exchanges a logged-in user's login JWT for a
// short-lived admin-tier JWT. Used by both:
//
//   - the entiredb CLI's admin commands, to mint an ops-access JWT for
//     any `entiredb admin ...` RPC, and
//   - the entire-backup binary running on an operator's laptop, to mint
//     a backup-access JWT instead of presenting workload-mTLS material
//     (which only the in-cluster CronJob carries).
//
// The two kinds use different endpoints. Ops-access is an RFC 8693
// token exchange against pkg/op's canonical /oauth/token with
// audience=https://<cluster>/admin — the audience names the resource
// and dispatch happens server-side via ClassifyAudience. Backup-access
// still rides the bespoke /api/authz/sts/token endpoint because the
// new endpoint doesn't yet have a backup-access audience surface.
// Both share the cached-exchange infrastructure here so callers get
// just-in-time refresh, clock-skew safety margin, and de-duplication
// of concurrent fetches for free.
package admincreds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/entireio/cli/internal/entiredb/httputil"
)

// TokenType selects which admin-tier token the caller wants. The two
// kinds use different endpoints and form shapes; the dispatch lives in
// Get.
//
// The backup-access URN string MUST match the server-side URN in
// core/authn/stsurns.go (where the authoritative copy lives), since
// it's still sent verbatim as requested_token_type on the legacy
// /api/authz/sts/token path. The ops-access kind no longer travels as
// a URN — the new /oauth/token endpoint dispatches on audience — so
// the constant is a CLI-internal selector with no wire equivalent.
// admincreds intentionally redeclares the backup URN rather than
// importing core/authn to avoid pulling go-jose and other JWT-signing
// deps into the lightweight CLI binaries. The pairing is pinned by
// TestTokenTypeURNsMatchServerAuthoritative in the external _test
// package — drift between the two copies fails that test at build
// time.
type TokenType string

const (
	// TokenTypeOpsAccess selects the /oauth/token path with
	// audience=https://<cluster>/admin. SpiceDB gate is platform#admin;
	// the issued JWT authorizes the entiredb admin RPC surface.
	TokenTypeOpsAccess TokenType = "ops-access"

	// TokenTypeDeployAccess selects the /oauth/token path with
	// audience=https://<cluster>/deploy. SpiceDB gate is
	// platform#deploy_ops; the issued JWT authorizes the deploy
	// orchestration RPCs (PreFlight/Stepdown/Status) called by
	// entire-deploy. Subject_token must already carry
	// entire:ops-access — platform-admin login JWTs and sa-session
	// JWTs both do, narrowed api-access tokens do not.
	TokenTypeDeployAccess TokenType = "deploy-access"

	// TokenTypeBackupAccess selects the legacy /api/authz/sts/token
	// path. SpiceDB gate is platform#backup_admin (strictly separate
	// from platform#admin); the issued JWT authorizes the three
	// backup admin RPCs plus cluster-wide repo read.
	TokenTypeBackupAccess TokenType = "urn:entire:token-type:backup-access"
)

// opsAccessClientID is the public OAuth client_id the CLI uses on
// /oauth/token. Lifted into Basic auth by httputil.PostOAuthToken
// because pkg/op reads token-exchange client credentials from Basic
// auth only.
const opsAccessClientID = "entire-cli"

// Audience path suffixes appended to clusterURL for the /oauth/token
// exchange. Duplicated from core/authn.AudiencePath{Admin,Deploy} to
// keep admincreds free of the heavy authn deps; pinned to the
// server-side values by the same _test pairing as the URN constants.
const (
	adminAudiencePath  = "/admin"
	deployAudiencePath = "/deploy"
)

// safetyMargin is subtracted from the server's expires_in so the
// client rotates before actual expiry — covers clock skew and gives
// slow downstream RPCs a fresh token. One minute matches the margin
// the legacy opsAuth used and is the same as entire-backup's
// tokenHolder (cmd/entire-backup/token_holder.go); a future change
// should keep them aligned so backup-access and ops-access don't
// diverge on freshness semantics.
const safetyMargin = time.Minute

// Auth caches a login-JWT → admin-JWT exchange so repeated Get calls
// in a single process don't hit core every tick. The cache is
// keyed implicitly by the (coreURL, loginJWT, tokenType, extraForm)
// tuple captured at construction; if any of those need to change,
// build a new Auth via New (or derive one via WithExtraForm, which
// is the only mutator and returns a fresh-cache copy).
type Auth struct {
	coreURL    string // trimmed, no trailing slash
	loginJWT   string
	tokenType  TokenType
	httpClient *http.Client
	// clusterURL is the full URL (scheme://host[:port]) of the
	// entiredb cluster the issued token will target. Ops-access uses
	// it to build audience=clusterURL+/admin on the /oauth/token
	// exchange. Backup-access uses url.Parse(clusterURL).Host as the
	// `cluster_host` form field on the legacy STS endpoint. Required
	// for ops-access; optional for backup-access (the server-side
	// branch currently ignores it).
	clusterURL string
	// extraForm carries token-type-specific form fields the exchange
	// requires. backup-access adds restore_mode/reason here; ops-access
	// leaves this nil. Treated as immutable post-construction.
	extraForm url.Values

	mu     sync.Mutex
	cached entry
}

type entry struct {
	token     string
	expiresAt time.Time
}

// New returns an Auth for the given token type. coreURL must be the
// issuer URL of the login JWT (resolved from the caller's active
// context); passing a different region's core returns 401 because
// signers don't match. clusterURL is the target entiredb cluster's
// full URL (scheme://host[:port], trailing slash trimmed); required
// for ops-access, optional for backup-access (server-side branch
// currently ignores it).
func New(coreURL, loginJWT, clusterURL string, tokenType TokenType, httpClient *http.Client) *Auth {
	return &Auth{
		coreURL:    strings.TrimRight(coreURL, "/"),
		loginJWT:   loginJWT,
		tokenType:  tokenType,
		httpClient: httpClient,
		clusterURL: strings.TrimRight(clusterURL, "/"),
	}
}

// WithExtraForm returns a derived Auth that includes the given form
// fields in every exchange. Use this for backup-access's restore_mode
// and reason fields; ops-access doesn't need it.
//
// The returned Auth shares no state with the receiver (fresh mutex,
// fresh cache) so callers can build several Auths against the same
// login JWT with different extras (e.g. backup vs restore) without
// aliasing the cache or holding a copied mutex.
func (a *Auth) WithExtraForm(extra url.Values) *Auth {
	return &Auth{
		coreURL:    a.coreURL,
		loginJWT:   a.loginJWT,
		tokenType:  a.tokenType,
		httpClient: a.httpClient,
		clusterURL: a.clusterURL,
		extraForm:  extra,
	}
}

// Get returns a valid admin JWT, exchanging against core if the
// cached entry is missing or inside the safety margin. Concurrent
// callers serialize on the mutex; a single fetch satisfies all of
// them.
func (a *Auth) Get(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.cached.token != "" && time.Now().Before(a.cached.expiresAt) {
		return a.cached.token, nil
	}

	if a.coreURL == "" {
		return "", errors.New("admincreds: no entire-core URL configured for STS exchange")
	}

	token, ttl, err := a.exchange(ctx)
	if err != nil {
		return "", err
	}
	if ttl <= safetyMargin {
		// Effectively already in the stale window — hand it out for
		// this request but don't cache it, or the next call would
		// see expiresAt in the past and re-exchange synchronously.
		return token, nil
	}
	a.cached = entry{token: token, expiresAt: time.Now().Add(ttl - safetyMargin)}
	return token, nil
}

// exchange dispatches to the per-token-type endpoint. Ops-access uses
// the canonical /oauth/token RFC 8693 path with audience-based
// dispatch; backup-access uses the legacy /api/authz/sts/token path
// with a requested_token_type URN. Returns the access token and the
// server-reported TTL.
func (a *Auth) exchange(ctx context.Context) (string, time.Duration, error) {
	switch a.tokenType {
	case TokenTypeOpsAccess:
		return a.exchangeAudienceAccess(ctx, adminAudiencePath)
	case TokenTypeDeployAccess:
		return a.exchangeAudienceAccess(ctx, deployAudiencePath)
	case TokenTypeBackupAccess:
		return a.exchangeLegacy(ctx)
	}
	return "", 0, fmt.Errorf("admincreds: unknown token type %q", a.tokenType)
}

// exchangeAudienceAccess posts an RFC 8693 token exchange to
// /oauth/token with audience=https://<cluster><audiencePath>. The
// server classifies the audience, runs the gate matching the audience
// (platform#admin for /admin, platform#deploy_ops for /deploy), and
// stamps the right scope on the issued token. client_id is lifted
// into Basic auth by httputil.PostOAuthToken because pkg/op's
// token-exchange handler reads client credentials only from there.
func (a *Auth) exchangeAudienceAccess(ctx context.Context, audiencePath string) (string, time.Duration, error) {
	if a.clusterURL == "" {
		return "", 0, fmt.Errorf("admincreds: %s exchange requires a target cluster URL", a.tokenType)
	}
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:token-exchange")
	form.Set("subject_token_type", "urn:ietf:params:oauth:token-type:access_token")
	form.Set("subject_token", a.loginJWT)
	form.Set("requested_token_type", "urn:ietf:params:oauth:token-type:access_token")
	form.Set("audience", a.clusterURL+audiencePath)
	form.Set("client_id", opsAccessClientID)
	for k, vs := range a.extraForm {
		for _, v := range vs {
			form.Add(k, v)
		}
	}

	token, expiresIn, err := httputil.PostOAuthToken(ctx, a.httpClient, a.coreURL, form)
	if err != nil {
		var oauthErr *httputil.OAuthError
		if errors.As(err, &oauthErr) && oauthErr.Status == http.StatusForbidden {
			return "", 0, fmt.Errorf("%s: %s", deniedReason(a.tokenType), oauthErr.Body)
		}
		return "", 0, fmt.Errorf("%s exchange: %w", a.tokenType, err)
	}
	return token, time.Duration(expiresIn) * time.Second, nil
}

// exchangeLegacy posts to the bespoke /api/authz/sts/token endpoint.
// Used by backup-access until the new /oauth/token endpoint grows a
// backup audience surface.
func (a *Auth) exchangeLegacy(ctx context.Context) (string, time.Duration, error) {
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:token-exchange")
	form.Set("subject_token_type", "urn:ietf:params:oauth:token-type:jwt")
	form.Set("subject_token", a.loginJWT)
	form.Set("requested_token_type", string(a.tokenType))
	if host := clusterHostFromURL(a.clusterURL); host != "" {
		form.Set("cluster_host", host)
	}
	for k, vs := range a.extraForm {
		for _, v := range vs {
			form.Add(k, v)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.coreURL+"/api/authz/sts/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("build STS request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("STS exchange (%s): %w", a.tokenType, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024)) //nolint:errcheck // best-effort body read for error message
		if resp.StatusCode == http.StatusForbidden {
			return "", 0, fmt.Errorf("%s denied: %s", deniedReason(a.tokenType), strings.TrimSpace(string(msg)))
		}
		return "", 0, fmt.Errorf("STS exchange (%s) failed: HTTP %d: %s",
			a.tokenType, resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", 0, fmt.Errorf("decode STS response: %w", err)
	}
	if out.AccessToken == "" {
		return "", 0, errors.New("STS response missing access_token")
	}
	return out.AccessToken, time.Duration(out.ExpiresIn) * time.Second, nil
}

// clusterHostFromURL returns the host[:port] of a parsed URL, or the
// raw string if parsing fails or the URL is empty. Tolerant by design:
// the legacy endpoint accepts a bare host, so a caller passing
// "cluster.example" instead of "https://cluster.example" continues to
// work.
func clusterHostFromURL(s string) string {
	if s == "" {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return s
	}
	return u.Host
}

// deniedReason picks the right human-readable hint for a 403 from
// core. Mirrors the legacy opsAuth error message and adds the
// backup-access variant.
func deniedReason(t TokenType) string {
	switch t {
	case TokenTypeOpsAccess:
		return "ops-token denied: you are not a platform admin"
	case TokenTypeDeployAccess:
		return "deploy-token denied: you do not have platform#deploy_ops"
	case TokenTypeBackupAccess:
		return "backup-token denied: you do not have platform#backup_admin"
	default:
		return string(t) + " denied"
	}
}
