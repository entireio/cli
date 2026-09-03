package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
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

// ResolveDataAPIToken returns the bearer for the data plane at dataBaseURL:
// the active context's refreshed login JWT — the account access token (scope
// entire:session) that the entire.io gateway and the entire-api cells accept
// directly, and that the gateway uses to mint per-jurisdiction cell tokens
// itself (COR-1095).
//
// It used to return an RFC 8693 exchange of that JWT for the data host's
// audience (a narrower entire:api-access token). Cell-backed gateway routes
// can no longer serve that shape: the gateway had to re-exchange it at
// entire-core to reach a cell, and core refuses a non-session subject — which
// is how every released CLI's `entire dispatch` 502'd from 2026-08-20.
//
// Discovery is unchanged and remains the only path: the host's
// /.well-known/entire-api.json names the login servers it trusts, and the
// ACTIVE auth context must be issued by one of them. Pointing the CLI at
// another environment therefore still takes two steps, because the acting
// identity is never inferred from the target host:
//
//	entire auth use staging
//	ENTIRE_API_BASE_URL=https://partial.to entire activity
//
// A host that doesn't advertise discovery (unreachable / 404 / 503 /
// malformed) is an error — without it we can't know which login servers the
// host trusts, and guessing risks presenting a token to a host that doesn't
// accept that core (see clusterdiscovery.requireActiveContext).
//
// Callers that honour --insecure-http-auth must call EnableInsecureHTTP before
// invoking this (as they already do); the per-context refresh reads that
// global opt-in.
func ResolveDataAPIToken(ctx context.Context, dataBaseURL string) (string, error) {
	dataOrigin := api.OriginOnly(dataBaseURL)
	host, ok := hostOf(dataOrigin)
	if !ok {
		return "", fmt.Errorf("data API URL %q has no host to discover against", dataBaseURL)
	}

	dctx, cancel := context.WithTimeout(ctx, dataAPIDiscoveryTimeout)
	defer cancel()
	httpClient := dataAPIDiscoveryClient(dataOrigin)

	selected, err := resolveContextForAPI(dctx, userdirs.Config(), userdirs.Cache(), host, httpClient, nil)
	if errors.Is(err, clusterdiscovery.ErrDiscoveryUnavailable) {
		return "", fmt.Errorf("%s does not advertise its trusted login servers (/.well-known/entire-api.json missing or unreachable); cannot authenticate: %w", host, err)
	}
	if err != nil {
		return "", err
	}

	return RefreshedLoginToken(ctx, selected)
}

type dataAPIHTTPDiscoveryTransport struct {
	base http.RoundTripper
}

func (t dataAPIHTTPDiscoveryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = schemeHTTP
	resp, err := t.base.RoundTrip(clone)
	if err != nil {
		return nil, fmt.Errorf("plain HTTP data API discovery: %w", err)
	}
	return resp, nil
}

func dataAPIDiscoveryClient(dataOrigin string) *http.Client {
	client := &http.Client{Timeout: dataAPIDiscoveryTimeout, Transport: versioninfo.WrapTransport(nil)}
	if !shouldUsePlainHTTPDiscovery(dataOrigin) {
		return client
	}

	client.Transport = dataAPIHTTPDiscoveryTransport{base: client.Transport}
	return client
}

func shouldUsePlainHTTPDiscovery(dataOrigin string) bool {
	u, err := url.Parse(dataOrigin)
	if err != nil || u.Scheme != schemeHTTP {
		return false
	}
	return insecureHTTPEnabled() || isLoopbackHTTP(dataOrigin)
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
