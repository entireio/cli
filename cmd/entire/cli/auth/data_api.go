package auth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/internal/entireclient/clusterdiscovery"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// dataAPIDiscoveryTimeout bounds the one /.well-known/entire-api.json GET we
// add per data-API command. Kept short: on any failure we fall back to static
// resolution, so a slow or absent endpoint must not stall the command.
const dataAPIDiscoveryTimeout = 8 * time.Second

// resolveContextFunc is the shape of a context-discovery seam: it mirrors
// clusterdiscovery.ResolveContextForAPI / ResolveContextForCluster
// (ctx, configDir, cacheDir, host, httpClient, debugf).
type resolveContextFunc func(context.Context, string, string, string, *http.Client, clusterdiscovery.DebugFunc) (*contexts.Context, error)

// resolveContextForAPI is the discovery seam, swapped in tests so they don't
// reach the network. See SetResolveContextForAPIForTest for cross-package tests.
var resolveContextForAPI resolveContextFunc = clusterdiscovery.ResolveContextForAPI

// SetResolveContextForAPIForTest overrides the /.well-known/entire-api.json
// discovery seam and returns a cleanup func. Tests in other packages that
// exercise a data-API command (activity/search/dispatch/recap) MUST install
// this — otherwise ResolveDataAPIToken makes a real network call to the
// configured data host and bypasses any SetManagerForTest fallback seam. Pass
// a func returning clusterdiscovery.ErrDiscoveryUnavailable to force the static
// fallback path. Test-only.
func SetResolveContextForAPIForTest(t interface{ Helper() }, fn resolveContextFunc) func() {
	t.Helper()
	prev := resolveContextForAPI
	resolveContextForAPI = fn
	return func() { resolveContextForAPI = prev }
}

// DiscoveryUnavailableForTest is a ready-made SetResolveContextForAPIForTest
// value that forces the discovery-unavailable fallback (no network), so a
// cross-package test exercises the static TokenForResource path deterministically.
func DiscoveryUnavailableForTest(context.Context, string, string, string, *http.Client, clusterdiscovery.DebugFunc) (*contexts.Context, error) {
	return nil, clusterdiscovery.ErrDiscoveryUnavailable
}

// ResolveDataAPIToken returns a bearer for the data API at dataBaseURL.
//
// It dials the API's /.well-known/entire-api.json to learn which login
// server(s) the API trusts and which audience to exchange for, picks the
// matching local auth context (active-wins-if-eligible → sole → explicit
// choice), and exchanges that context's login JWT for the advertised audience
// at that context's core. This is what makes
//
//	ENTIRE_API_BASE_URL=https://partial.to entire activity
//
// authenticate as the partial.to login even while the active context is a
// prod entire.io login — with no per-command override needed.
//
// When the API doesn't advertise discovery (404 / unreachable / 503 /
// malformed — e.g. a deployment predating the well-known), it falls back to
// the pre-discovery static path (TokenForResource through the singleton
// manager) so behaviour is never worse than before. A reachable API whose
// context selection fails (no eligible context, or several with none active)
// surfaces that error directly — the user must log in or pick one.
//
// Callers that honour --insecure-http-auth must call EnableInsecureHTTP before
// invoking this (as they already do); the per-context exchange and the static
// fallback both read that global opt-in.
func ResolveDataAPIToken(ctx context.Context, dataBaseURL string) (string, error) {
	dataOrigin := api.OriginOnly(dataBaseURL)
	host, ok := hostOf(dataOrigin)
	if !ok {
		// Can't derive a host to discover against — use static resolution.
		return TokenForResource(ctx, dataOrigin)
	}

	// Bridge any pre-contexts.json login so the resolver can match it, mirroring
	// the git remote helper's cold-boot path. Best-effort: a migration failure
	// must not block resolution.
	_, _ = MigrateLegacyLoginContext() //nolint:errcheck // best-effort bridge; resolution proceeds regardless

	dctx, cancel := context.WithTimeout(ctx, dataAPIDiscoveryTimeout)
	defer cancel()
	httpClient := &http.Client{Timeout: dataAPIDiscoveryTimeout}

	selected, err := resolveContextForAPI(dctx, userdirs.Config(), userdirs.Cache(), host, httpClient, nil)
	if errors.Is(err, clusterdiscovery.ErrDiscoveryUnavailable) {
		// Old deployment / not rolled out / transient — preserve today's behaviour.
		return TokenForResource(ctx, dataOrigin)
	}
	if err != nil {
		return "", err
	}

	// Exchange for the data host origin; the token manager derives the RFC 8693
	// audience from it, which is the aud the API requires (aud == base URI).
	allowInsecure := insecureHTTPEnabled() || isLoopbackHTTP(selected.CoreURL)
	provider, err := NewRefreshingResourceProvider(selected, dataOrigin, nil, allowInsecure)
	if err != nil {
		return "", err
	}
	return provider(ctx)
}

// hostOf returns the host[:port] of an origin URL, ok=false when it can't be
// parsed into a host.
func hostOf(origin string) (string, bool) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return "", false
	}
	return u.Host, true
}
