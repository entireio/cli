package cliauth

import (
	"bytes"
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

// crossJurisRoundTripper handles two recoverable failure modes uniformly,
// for every CLI HTTP call that wraps its transport:
//
//   - 421 Misdirected Request: a control-plane core responded that the
//     target resource lives in another jurisdiction and returned the
//     home core's base URL. The transport rewrites the request URL and
//     retries once.
//   - 401 with body `{"error":"cross_juris_token_required",
//     "token_exchange_url":"...", "audience":"..."}`: the JWT verified
//     but its audience is a sibling region's. The transport POSTs an
//     RFC 8693 exchange at the indicated /oauth/token (subject_token =
//     the original Authorization Bearer), caches the resulting access
//     token keyed by origin, swaps the Authorization header, and retries
//     once.
//
// Each original request gets at most one of each retry — a 421 followed
// by an exchange-retry is allowed; further recursion is a server bug we
// want to surface, not paper over.
//
// Per-origin token cache lives on the transport instance, scoped to one
// CLI invocation. TTL matches the foreign-session token's lifetime (5
// minutes — see ADR 20260520).
//
// # Trust anchors for server-supplied URLs
//
// Both recovery paths take a URL from the server response and re-target
// the user's long-lived login JWT at it. A misconfigured or malicious
// core could otherwise exfiltrate the JWT by pointing those URLs at an
// attacker-controlled host. The transport applies:
//
//   - All server-supplied URLs must be HTTPS, except loopback hosts on
//     http (so httptest fixtures keep working).
//   - 401 `token_exchange_url`: must be SAME-ORIGIN with the response
//     that emitted the 401. A core that emits cross_juris_token_required
//     does so from its own /oauth/token; there is no legitimate reason
//     to point off-origin.
//   - 421 `home_core_url`: by definition off-origin, so same-origin is
//     not applicable. Instead, the transport lazy-fetches
//     `/.well-known/entire-federation` from the response origin (the
//     host that emitted the 421 — which is already a host the CLI was
//     configured to talk to) and validates the redirect target appears
//     in the returned peer list. Cached per-origin for the lifetime of
//     the transport so federation discovery costs one extra round trip
//     per cluster per CLI invocation.
type crossJurisRoundTripper struct {
	base       http.RoundTripper
	tokens     sync.Map // origin (scheme://host) → cachedExchangedToken
	federation sync.Map // origin (scheme://host) → cachedFederation
}

type cachedExchangedToken struct {
	token string
	exp   time.Time
}

// cachedTokenTTL bounds how long an exchanged token stays in the
// process-local cache. Slightly shorter than the server-side foreign-
// session TTL (5m) so we never present a token within seconds of its
// expiry — the server would 401, we'd re-exchange, and the user would
// see the same latency as a cold cache.
const cachedTokenTTL = 4 * time.Minute

// crossJurisErrorBody is the wire shape the server emits for the
// machine-readable 401. Mirrored from core/api/middleware.go.
type crossJurisErrorBody struct {
	Error            string `json:"error"`
	TokenExchangeURL string `json:"token_exchange_url"`
	Audience         string `json:"audience"`
}

// mirror421Body mirrors core/api/mirrors.go's 421 envelope.
type mirror421Body struct {
	Error       string `json:"error"`
	HomeCoreURL string `json:"home_core_url"`
}

// federationBody mirrors the GET /.well-known/entire-federation response
// shape. Empty / missing peer_auth_hosts means no peers are trusted for
// 421 follow from this origin.
type federationBody struct {
	PeerAuthHosts []string `json:"peer_auth_hosts"`
}

// cachedFederation records the result of one lazy federation-list fetch
// so the next 421 from the same origin doesn't pay the round trip
// again. The negative case (fetch failed / endpoint absent) is cached
// the same way: an attacker can't bypass the check by 421-spamming and
// hoping for a stale negative, because we still reject when hosts is nil.
type cachedFederation struct {
	hosts map[string]struct{}
}

// federationLookupTimeout caps the lazy federation-list fetch on the
// critical path of a 421 follow. Kept tight: the endpoint is local to
// the responding core and the response is small. A slow fetch shouldn't
// stall a user-facing redirect.
const federationLookupTimeout = 3 * time.Second

// federationWellKnownPath is the URL path for the federation-trust
// manifest. Served public, unauthenticated (the list itself is not a
// secret — it names siblings any user can already enumerate via login).
const federationWellKnownPath = "/.well-known/entire-federation"

func newCrossJurisRoundTripper(base http.RoundTripper) *crossJurisRoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &crossJurisRoundTripper{base: base}
}

// RoundTrip implements http.RoundTripper. On the first attempt it swaps
// the Authorization header to a cached exchanged token when one exists
// for the request origin; otherwise the caller-supplied header is used.
// 421 and cross-juris 401 trigger one retry each via the helpers, and a
// 421-then-401 chain (misdirected then home core needs exchange) gets
// one exchange attempt per origin so the second exchange isn't starved.
//
// The original caller-supplied bearer is snapshotted at entry and used
// as the subject_token on every exchange — re-using a previously-
// exchanged token would already carry foreign_iss and the validator
// rejects chained cross-juris exchanges.
func (t *crossJurisRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := bufferBody(req)
	if err != nil {
		return nil, fmt.Errorf("cross-juris transport: buffer body: %w", err)
	}
	originalAuth := req.Header.Get("Authorization")
	return t.send(req, body, originalAuth, retryBudget{redirects: 1, triedExchange: map[string]bool{}})
}

// retryBudget caps the recursion. Redirects share one budget across the
// whole call; exchanges are bounded per-origin via triedExchange so a
// 421-then-401 chain doesn't starve the home core of its first attempt.
type retryBudget struct {
	redirects     int
	triedExchange map[string]bool
}

func (t *crossJurisRoundTripper) send(req *http.Request, body []byte, originalAuth string, budget retryBudget) (*http.Response, error) {
	// Pick the Authorization for this hop: cached exchanged token for
	// this origin if present (cache hit), otherwise the original caller-
	// supplied bearer. Crucially we DON'T inherit whatever Authorization
	// happens to be on req.Header — a previous hop may have stamped an
	// exchanged token scoped to a different origin, and presenting that
	// here would be wrong on its face AND would corrupt subject_token
	// for any exchange this hop ends up doing.
	origin := requestOrigin(req)
	switch cached, ok := t.lookupToken(origin); {
	case ok:
		req.Header.Set("Authorization", "Bearer "+cached)
	case originalAuth != "":
		req.Header.Set("Authorization", originalAuth)
	}
	resetBody(req, body)

	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("cross-juris transport: base round trip: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusMisdirectedRequest:
		if budget.redirects <= 0 {
			return resp, nil
		}
		newReq, err := t.followMisdirected(req, resp, origin)
		if err != nil {
			// Couldn't follow — surface the original response so the
			// caller sees the server's body.
			Debugf("cross-juris transport: 421 follow failed: %v", err)
			return resp, nil
		}
		_ = resp.Body.Close()
		budget.redirects--
		return t.send(newReq, body, originalAuth, budget)
	case http.StatusUnauthorized:
		if budget.triedExchange[origin] {
			return resp, nil
		}
		hint, ok, err := readCrossJurisHint(resp)
		if err != nil {
			Debugf("cross-juris transport: read 401 body failed: %v", err)
			return resp, nil
		}
		if !ok {
			return resp, nil
		}
		// Same-origin + safe-scheme check before re-targeting the user's
		// login JWT: a 401 emitted by a core has no legitimate reason to
		// point token_exchange_url at any other host. Off-origin or
		// http:// (non-loopback) hints get the original 401 passed
		// through unchanged so the caller sees the server's bytes and
		// the JWT never leaves the trusted origin.
		if err := validateExchangeURL(hint.TokenExchangeURL, req.URL); err != nil {
			Debugf("cross-juris transport: rejecting token_exchange_url: %v", err)
			return resp, nil
		}
		// Exchange the ORIGINAL login JWT (not whatever exchanged token
		// a prior hop stashed on req). An exchanged token already
		// carries foreign_iss; validateForeignSessionExchange rejects
		// chained cross-juris hops.
		subjectToken, found := strings.CutPrefix(originalAuth, "Bearer ")
		if !found || subjectToken == "" {
			return resp, nil
		}
		exchanged, err := t.exchangeSubjectToken(req.Context(), hint, subjectToken)
		if err != nil {
			Debugf("cross-juris transport: exchange failed: %v", err)
			return resp, nil
		}
		t.storeToken(origin, exchanged)
		budget.triedExchange[origin] = true
		_ = resp.Body.Close()
		req.Header.Set("Authorization", "Bearer "+exchanged)
		return t.send(req, body, originalAuth, budget)
	}
	return resp, nil
}

// followMisdirected reads the 421 envelope and constructs a fresh
// request aimed at the home core. Method, headers, and (buffered) body
// are preserved on the new request. On parse failure the original
// response body is restored (the caller is about to return resp
// unchanged) so downstream consumers still see the server's bytes.
//
// Trust gate: home_core_url must be HTTPS (loopback http allowed for
// tests), and its host must appear in the federation manifest fetched
// lazily from responseOrigin (the host that emitted the 421 — already
// a trusted origin for the CLI). Off-federation or off-scheme targets
// surface the original 421 unchanged.
func (t *crossJurisRoundTripper) followMisdirected(orig *http.Request, resp *http.Response, responseOrigin string) (*http.Request, error) {
	body, err := drainAndRestoreBody(resp, 64*1024)
	if err != nil {
		return nil, fmt.Errorf("read 421 body: %w", err)
	}
	var env mirror421Body
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("parse 421 body: %w", err)
	}
	if env.HomeCoreURL == "" {
		return nil, errors.New("421 body missing home_core_url")
	}
	homeCore, err := url.Parse(strings.TrimRight(env.HomeCoreURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse home_core_url: %w", err)
	}
	if !isSafeOrigin(homeCore) {
		return nil, fmt.Errorf("home_core_url %q is not https (or http loopback)", env.HomeCoreURL)
	}
	peers := t.federationHostsFor(orig.Context(), responseOrigin)
	if _, ok := peers[homeCore.Host]; !ok {
		return nil, fmt.Errorf("home_core_url host %q is not in the responding core's federation manifest", homeCore.Host)
	}
	target := *homeCore
	target.Path = orig.URL.Path
	target.RawQuery = orig.URL.RawQuery
	newReq, err := http.NewRequestWithContext(orig.Context(), orig.Method, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build redirected request: %w", err)
	}
	newReq.Header = orig.Header.Clone()
	return newReq, nil
}

// validateExchangeURL gates the URL the transport will POST the user's
// login JWT to for an RFC 8693 exchange. Two checks:
//
//   - Scheme must be https, except http://localhost for tests.
//   - Host must equal the request URL host that emitted the 401. A
//     core's /oauth/token always lives on the same origin as its
//     middleware; off-origin hints have no legitimate use and would
//     let a misconfigured / hostile core exfiltrate the user's JWT.
func validateExchangeURL(raw string, requestURL *url.URL) error {
	if raw == "" {
		return errors.New("token_exchange_url missing")
	}
	exchange, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse token_exchange_url: %w", err)
	}
	if !isSafeOrigin(exchange) {
		return fmt.Errorf("token_exchange_url %q is not https (or http loopback)", raw)
	}
	if requestURL == nil || requestURL.Host == "" {
		return errors.New("cannot determine response origin for same-origin check")
	}
	if exchange.Host != requestURL.Host {
		return fmt.Errorf("token_exchange_url host %q must match response host %q", exchange.Host, requestURL.Host)
	}
	return nil
}

// isSafeOrigin reports whether u is acceptable as a destination for the
// user's login JWT. https is always allowed; http is allowed only for
// loopback so httptest fixtures keep working.
func isSafeOrigin(u *url.URL) bool {
	switch u.Scheme {
	case "https":
		return u.Host != ""
	case "http":
		return isLoopbackHost(u.Hostname())
	default:
		return false
	}
}

// isLoopbackHost reports whether host is a loopback name or IP. Used
// only to widen isSafeOrigin for local/test fixtures — production
// federation traffic is always https.
func isLoopbackHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// federationHostsFor returns the set of hosts the core at origin
// publishes as its federation peers (via GET /.well-known/entire-
// federation). Cached for the life of the transport. nil result means
// the origin advertises no peers OR the fetch failed; either way, no
// 421 redirect from this origin will be followed.
func (t *crossJurisRoundTripper) federationHostsFor(ctx context.Context, origin string) map[string]struct{} {
	if origin == "" {
		return nil
	}
	if v, ok := t.federation.Load(origin); ok {
		cached, _ := v.(cachedFederation) //nolint:errcheck // type assertion, not error
		return cached.hosts
	}
	hosts := t.fetchFederationHosts(ctx, origin)
	t.federation.Store(origin, cachedFederation{hosts: hosts})
	return hosts
}

// fetchFederationHosts performs the one-shot GET against origin's
// well-known endpoint. Returns nil on any failure (transport error,
// non-200, parse error, empty list) — federationHostsFor caches the
// nil so we don't pound the endpoint per 421.
func (t *crossJurisRoundTripper) fetchFederationHosts(ctx context.Context, origin string) map[string]struct{} {
	ctx, cancel := context.WithTimeout(ctx, federationLookupTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+federationWellKnownPath, nil)
	if err != nil {
		Debugf("cross-juris transport: build federation request: %v", err)
		return nil
	}
	req.Header.Set("Accept", "application/json")
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		Debugf("cross-juris transport: federation fetch: %v", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		Debugf("cross-juris transport: federation fetch returned HTTP %d", resp.StatusCode)
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	if err != nil {
		Debugf("cross-juris transport: read federation body: %v", err)
		return nil
	}
	var env federationBody
	if err := json.Unmarshal(body, &env); err != nil {
		Debugf("cross-juris transport: parse federation body: %v", err)
		return nil
	}
	if len(env.PeerAuthHosts) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(env.PeerAuthHosts))
	for _, raw := range env.PeerAuthHosts {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			continue
		}
		out[u.Host] = struct{}{}
	}
	return out
}

// readCrossJurisHint reads the structured 401 envelope. Returns ok=false
// when the body isn't the expected shape (a plain 401 from somewhere
// else, an HTML proxy error, etc.) so the caller passes the response
// through unchanged. The body is restored regardless so downstream
// consumers can re-read.
func readCrossJurisHint(resp *http.Response) (crossJurisErrorBody, bool, error) {
	body, err := drainAndRestoreBody(resp, 8*1024)
	if err != nil {
		return crossJurisErrorBody{}, false, fmt.Errorf("read 401 body: %w", err)
	}
	var env crossJurisErrorBody
	// Unmarshal failure means the body isn't our envelope shape (HTML
	// proxy error, a different JSON shape, garbage). That's the
	// not-a-hint case, not an error condition — the caller passes the
	// response through unchanged.
	if err := json.Unmarshal(body, &env); err != nil {
		return crossJurisErrorBody{}, false, nil //nolint:nilerr // unmarshal-fail = "not our envelope shape"
	}
	if env.Error != "cross_juris_token_required" || env.TokenExchangeURL == "" {
		return crossJurisErrorBody{}, false, nil
	}
	return env, true, nil
}

// drainAndRestoreBody reads up to maxBytes from resp.Body and replaces
// resp.Body with a fresh in-memory reader over those bytes, so the
// caller can hand resp back to its own caller with the body still
// readable. Cap is conservative — control-plane error envelopes are
// always small.
func drainAndRestoreBody(resp *http.Response, maxBytes int64) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(nil, resp.Body, maxBytes))
	if err != nil {
		return nil, fmt.Errorf("drain response body: %w", err)
	}
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

// exchangeSubjectToken performs the RFC 8693 cross-juris session
// exchange and returns the issued access_token. Delegates the wire-
// level details (form encode, lifting client_id into Basic auth,
// response decode) to httputil.PostOAuthToken so we stay in step with
// the other exchange sites (admincreds, scoped_auth, ops_token).
func (t *crossJurisRoundTripper) exchangeSubjectToken(ctx context.Context, hint crossJurisErrorBody, subjectToken string) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:token-exchange")
	form.Set("subject_token", subjectToken)
	form.Set("subject_token_type", "urn:ietf:params:oauth:token-type:jwt")
	form.Set("requested_token_type", "urn:ietf:params:oauth:token-type:access_token")
	if hint.Audience != "" {
		form.Set("audience", hint.Audience)
	}
	// "entire-cli" is the public client every CLI binary identifies as on
	// /oauth/token; PostOAuthToken lifts it into Basic auth because
	// pkg/op's token-exchange handler reads client credentials only from
	// the Authorization header. The subject_token is the real authorizer.
	form.Set("client_id", "entire-cli")
	// PostOAuthToken expects a base core URL and appends /oauth/token
	// itself; hint.TokenExchangeURL is already the full path (and
	// validateExchangeURL has gated the origin), so strip the suffix.
	coreURL := strings.TrimSuffix(hint.TokenExchangeURL, "/oauth/token")
	// Exchange goes through the base transport so we don't re-enter our
	// own retry logic — a non-200 here is terminal.
	client := &http.Client{Transport: t.base}
	token, _, err := httputil.PostOAuthToken(ctx, client, coreURL, form)
	if err != nil {
		return "", fmt.Errorf("exchange: %w", err)
	}
	return token, nil
}

func (t *crossJurisRoundTripper) lookupToken(origin string) (string, bool) {
	v, ok := t.tokens.Load(origin)
	if !ok {
		return "", false
	}
	cached, _ := v.(cachedExchangedToken) //nolint:errcheck // type assertion, not error
	if time.Now().After(cached.exp) {
		t.tokens.Delete(origin)
		return "", false
	}
	return cached.token, true
}

func (t *crossJurisRoundTripper) storeToken(origin, token string) {
	if origin == "" || token == "" {
		return
	}
	t.tokens.Store(origin, cachedExchangedToken{
		token: token,
		exp:   time.Now().Add(cachedTokenTTL),
	})
}

// bufferBody reads req.Body once into memory and sets GetBody so each
// retry produces a fresh reader. Returns nil when there's no body.
func bufferBody(req *http.Request) ([]byte, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	buf, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}
	_ = req.Body.Close()
	return buf, nil
}

// resetBody attaches a fresh body reader (and ContentLength) to req,
// using the buffer captured by bufferBody. Safe to call when buf is nil.
func resetBody(req *http.Request, buf []byte) {
	if buf == nil {
		req.Body = http.NoBody
		req.ContentLength = 0
		req.GetBody = func() (io.ReadCloser, error) { return http.NoBody, nil }
		return
	}
	req.Body = io.NopCloser(bytes.NewReader(buf))
	req.ContentLength = int64(len(buf))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(buf)), nil
	}
}

// requestOrigin returns scheme://host of req, the cache key for
// exchanged tokens. Empty when req.URL is malformed.
func requestOrigin(req *http.Request) string {
	if req.URL == nil || req.URL.Scheme == "" || req.URL.Host == "" {
		return ""
	}
	return req.URL.Scheme + "://" + req.URL.Host
}
