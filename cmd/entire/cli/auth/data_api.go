package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/internal/entireclient/clusterdiscovery"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// dataAPIDiscoveryTimeout bounds the one /.well-known/entire-api.json GET we
// add per data-API command. Kept short so a slow or absent endpoint fails the
// command promptly rather than stalling it.
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
// configured data host. Test-only.
func SetResolveContextForAPIForTest(t interface{ Helper() }, fn resolveContextFunc) func() {
	t.Helper()
	prev := resolveContextForAPI
	resolveContextForAPI = fn
	return func() { resolveContextForAPI = prev }
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
// Discovery is the only path: an API host that doesn't advertise
// /.well-known/entire-api.json (unreachable / 404 / 503 / malformed) is an
// error — without it we can't know which login servers the host trusts, and
// guessing risks exchanging a token at a core the host doesn't accept.
//
// Callers that honour --insecure-http-auth must call EnableInsecureHTTP before
// invoking this (as they already do); the per-context exchange reads that
// global opt-in.
func ResolveDataAPIToken(ctx context.Context, dataBaseURL string) (string, error) {
	dataOrigin := api.OriginOnly(dataBaseURL)
	host, ok := hostOf(dataOrigin)
	if !ok {
		return "", fmt.Errorf("data API URL %q has no host to discover against", dataBaseURL)
	}

	dctx, cancel := context.WithTimeout(ctx, dataAPIDiscoveryTimeout)
	defer cancel()
	httpClient := &http.Client{Timeout: dataAPIDiscoveryTimeout}

	selected, err := resolveContextForAPI(dctx, userdirs.Config(), userdirs.Cache(), host, httpClient, nil)
	if errors.Is(err, clusterdiscovery.ErrDiscoveryUnavailable) {
		return "", fmt.Errorf("%s does not advertise its trusted login servers (/.well-known/entire-api.json missing or unreachable); cannot authenticate: %w", host, err)
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
