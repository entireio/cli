package clusterdiscovery

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/discovery"
)

// APIPath is the well-known path a data/web API (entire.io) serves to advertise
// its trust roots, mirroring entire.io's api/src/app.ts route. The document also
// carries an `audience`, but the CLI doesn't consume it: the data API requires
// `aud` == its own base URI (https://entire.io / https://partial.to), which the
// token manager already derives from the resource origin it's dialing, so the
// only field the CLI needs is the trusted-issuer list — exactly like a git
// cluster's core_urls.
const APIPath = "/.well-known/entire-api.json"

// APIResponse is the parsed shape of /.well-known/entire-api.json. The CLI reads
// only trusted_issuers (to pick the login context); issuer/audience/jwks_uris
// are server-side concerns and ignored on decode.
type APIResponse struct {
	// TrustedIssuers is every core whose JWTs the API accepts. Used the same way
	// cluster discovery uses core_urls: to pick the local context whose CoreURL
	// the API will honour.
	TrustedIssuers []string `json:"trusted_issuers"`
}

// ErrDiscoveryUnavailable wraps every "the API didn't give us a usable
// trust-root document" outcome: it doesn't serve /.well-known/entire-api.json
// (404 — old deployment), is unreachable, answers 503 (unconfigured), or
// returns a malformed/empty body. Callers match on it to surface a clear
// "host does not advertise its trusted login servers" error — discovery is
// the only path, so there is no static fallback. Selection failures (no
// eligible / ambiguous context) are NOT wrapped — those are real "log in /
// pick one" errors the user must see.
var ErrDiscoveryUnavailable = errors.New("api discovery unavailable")

// DiscoverAPI fetches and parses an API host's /.well-known/entire-api.json,
// returning its trusted issuers. Every failure mode (transport, non-200,
// decode, empty trusted_issuers) is folded under ErrDiscoveryUnavailable so the
// caller has a single sentinel to match on.
//
// debugf is optional; nil suppresses debug output.
func DiscoverAPI(ctx context.Context, apiHost string, c *http.Client, debugf DebugFunc) (*APIResponse, error) {
	if debugf == nil {
		debugf = func(string, ...any) {}
	}
	var body APIResponse
	if err := fetchWellKnownJSON(ctx, apiHost, APIPath, c, &body, debugf); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDiscoveryUnavailable, err)
	}
	if len(body.TrustedIssuers) == 0 {
		debugf("api discovery: no trusted_issuers in response from https://%s%s", apiHost, APIPath)
		return nil, fmt.Errorf("%w: incomplete /.well-known/entire-api.json from %s", ErrDiscoveryUnavailable, apiHost)
	}
	return &body, nil
}

// resolveAPICores returns apiHost's trusted issuer URLs, from
// api_discovery.json when fresh, otherwise via a live
// /.well-known/entire-api.json fetch (cached, with stale fallback on failure).
// Shares the cache-then-discover logic with resolveClusterCores via
// resolveCachedCores — the data-API trusted issuers ARE core URLs, so they
// reuse the cores cache (different file). Cold failures stay folded under
// ErrDiscoveryUnavailable (from DiscoverAPI) for the caller to surface.
func resolveAPICores(ctx context.Context, cacheDir, apiHost string, httpClient *http.Client, debugf DebugFunc) ([]string, error) {
	return resolveCachedCores(cacheDir, apiHost, "api host",
		discovery.LoadAPICores, discovery.ModifyAPICores,
		func() ([]string, error) {
			body, err := DiscoverAPI(ctx, apiHost, httpClient, debugf)
			if err != nil {
				return nil, err
			}
			return body.TrustedIssuers, nil
		}, debugf)
}

// ResolveContextForAPI picks the local login context to authenticate data-API
// calls against apiHost.
//
// It mirrors ResolveContextForCluster: active context wins when its CoreURL is
// among the API's trusted issuers, else the sole eligible context, else an
// explicit-choice / login error — sourcing the trusted issuers from
// /.well-known/entire-api.json (cached in api_discovery.json, long TTL,
// re-fetched on expiry with stale fallback) instead of entire-cluster.json.
// Account selection is recomputed every call from the live contexts, never
// persisted. The caller exchanges the chosen context's token for the data host
// origin (which is the audience the API requires); no audience is read here.
//
// When the API doesn't advertise discovery (404 / unreachable / 503 /
// malformed) and no cache entry exists, the returned error wraps
// ErrDiscoveryUnavailable; data-API callers surface it as a fatal "host does
// not advertise its trusted login servers" error rather than guessing which
// login servers to trust. A successful fetch whose context selection fails
// returns that selection error unwrapped — the user must act on it.
//
// debugf is optional; nil suppresses debug output.
func ResolveContextForAPI(ctx context.Context, configDir, cacheDir, apiHost string, httpClient *http.Client, debugf DebugFunc) (*contexts.Context, error) {
	if debugf == nil {
		debugf = func(string, ...any) {}
	}
	trustedIssuers, err := resolveAPICores(ctx, cacheDir, apiHost, httpClient, debugf)
	if err != nil {
		return nil, err
	}
	f, err := contexts.Load(configDir)
	if err != nil {
		return nil, fmt.Errorf("load contexts: %w", err)
	}
	return selectContext(f, "API host "+apiHost, trustedIssuers, debugf)
}
