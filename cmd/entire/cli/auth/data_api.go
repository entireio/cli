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
// exercise a data-API command under ENTIRE_API_BASE_URL MUST install this —
// otherwise ResolveDataAPIToken makes a real network call to the configured
// data host. Test-only.
func SetResolveContextForAPIForTest(t interface{ Helper() }, fn resolveContextFunc) func() {
	t.Helper()
	prev := resolveContextForAPI
	resolveContextForAPI = fn
	return func() { resolveContextForAPI = prev }
}

// DataAPI is the web/data API origin to dial and the bearer for it.
type DataAPI struct {
	BaseURL string
	Token   string
}

// ResolveDataAPI picks the data API and bearer for this command.
//
// The selected login (--context / $ENTIRE_CONTEXT / current_context) decides
// both: its refreshed login JWT is the bearer and its login server's site is
// the host (DataBaseURL). The identity is never inferred from a target host,
// so a staging login talks to staging and a prod login to prod, exactly as
// the control plane does.
//
// ENTIRE_API_BASE_URL is the one exception: it names the host, and that host's
// /.well-known/entire-api.json says which saved login it accepts
// (ResolveDataAPIToken).
//
// Callers that honour --insecure-http-auth must call EnableInsecureHTTP before
// invoking this (as they already do); the per-context refresh reads that
// global opt-in.
func ResolveDataAPI(ctx context.Context) (DataAPI, error) {
	if dataURL, ok := api.BaseURLOverride(); ok {
		token, err := ResolveDataAPIToken(ctx, dataURL)
		if err != nil {
			return DataAPI{}, err
		}
		return DataAPI{BaseURL: dataURL, Token: token}, nil
	}
	c, ok, err := ActiveContext()
	if err != nil {
		return DataAPI{}, err
	}
	if !ok {
		return DataAPI{}, errNoLogin()
	}
	baseURL, err := dataBaseURLForCore(c.CoreURL)
	if err != nil {
		return DataAPI{}, err
	}
	token, err := RefreshedLoginToken(ctx, c)
	if err != nil {
		return DataAPI{}, err
	}
	return DataAPI{BaseURL: baseURL, Token: token}, nil
}

// DataBaseURL is the web/data API origin for the selected login, without a
// bearer: ENTIRE_API_BASE_URL when set, else the login server's site.
func DataBaseURL() (string, error) {
	if dataURL, ok := api.BaseURLOverride(); ok {
		return dataURL, nil
	}
	c, ok, err := activeContext()
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errNoLogin()
	}
	return dataBaseURLForCore(c.CoreURL)
}

// dataBaseURLForCore maps a login server to the site it serves: the site's
// apex for entire.io and partial.to, the server itself for local dev, where
// one process serves both.
func dataBaseURLForCore(coreURL string) (string, error) {
	origin := api.OriginOnly(coreURL)
	if isLoopbackOrigin(origin) {
		return origin, nil
	}
	if site := EntireSite(origin); site != "" {
		return "https://" + site, nil
	}
	return "", fmt.Errorf("login server %s has no known web host; set %s", origin, api.BaseURLEnvVar)
}

// ResolveDataAPIToken returns the bearer for the data plane at dataBaseURL:
// the login JWT of the saved context that host trusts.
//
// The host's /.well-known/entire-api.json names the login servers it trusts,
// and the selected context must be issued by one of them; the host never
// picks another saved login. A host that doesn't advertise discovery
// (unreachable / 404 / 503 / malformed) is an error — without it we can't know
// which login servers the host trusts, and guessing risks presenting a token
// to a host that doesn't accept that core.
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
