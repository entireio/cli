package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/internal/entireclient/clusterdiscovery"
	"github.com/entireio/cli/internal/entireclient/httputil"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

const (
	cellDataAPITimeout = 30 * time.Second

	jurisdictionIdentityScope = "openid"

	// clustersAPIPath is entire-core's cluster catalog endpoint.
	clustersAPIPath = "/api/v1/clusters"
)

// jurisdictionLabelPattern bounds a home_jurisdiction claim to a single DNS
// label before it is substituted into a URL template. The claim rides on the
// login JWT (which we decode without verifying the signature) and, for the
// home-jurisdiction fallback path, is attacker-influenceable if a token is ever
// mis-minted; constraining it to [a-z0-9-] means it can only ever name a
// sibling jurisdiction, never inject host/scheme syntax (e.g.
// "us.auth.evil.tld") into jurisdictionAudience / jurisdictionCoreURL.
var jurisdictionLabelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// CellTarget pins the entire-api cell a repo-scoped call must reach and the
// jurisdiction its identity token must be minted for. The cli layer resolves it
// from the repo's own cluster (via coreapi mirrors/clusters), so a repo-scoped
// route reaches the cell that HOSTS the repo — not the caller's home cell. A nil
// target falls back to home-jurisdiction routing (derived from the login JWT),
// which is correct for the common same-region case and for local dev.
type CellTarget struct {
	// BaseURL is the cell's apiUrl to dial (e.g. https://aws-eu-west-1.api.entire.io).
	BaseURL string
	// Jurisdiction is the repo's cluster jurisdiction; it drives the identity
	// token's audience and the core the exchange is performed at.
	Jurisdiction string
}

// resolveContextForCellAPI is the discovery seam for cell routing, swapped in
// tests. Mirrors resolveContextForAPI.
var resolveContextForCellAPI resolveContextFunc = clusterdiscovery.ResolveContextForAPI

// SetResolveContextForCellAPIForTest overrides the cell-API discovery seam.
func SetResolveContextForCellAPIForTest(t interface{ Helper() }, fn resolveContextFunc) func() {
	t.Helper()
	prev := resolveContextForCellAPI
	resolveContextForCellAPI = fn
	return func() { resolveContextForCellAPI = prev }
}

// cellExchangeTransportForTest, when non-nil, is the HTTP transport used for
// jurisdiction token exchange and cluster listing. Production leaves it nil.
var cellExchangeTransportForTest http.RoundTripper

// SetCellExchangeTransportForTest overrides the transport used for jurisdiction
// token exchange and cluster listing, returning a restore closure — the same
// set/restore convention the rest of the package uses for test seams.
func SetCellExchangeTransportForTest(t interface{ Helper() }, rt http.RoundTripper) func() {
	t.Helper()
	prev := cellExchangeTransportForTest
	cellExchangeTransportForTest = rt
	return func() { cellExchangeTransportForTest = prev }
}

// NewEntireAPICellClient returns an authenticated client aimed at an entire-api
// cell, carrying a jurisdictional identity token (scope=openid, aud=jurisdiction
// host). Repo-scoped entire-api routes do not accept the narrowed api-access
// bearer minted for the BFF origin — they require a cell identity token
// (COR-666).
//
// Cell selection, in precedence order:
//   - target != nil: dial target.BaseURL and mint for target.Jurisdiction. This
//     is the repo-scoped path — the caller (cli) resolved the repo's own cell.
//   - the configured data host already targets a cell (host contains ".api."):
//     keep that origin.
//   - a loopback data host (local dev): keep that origin.
//   - otherwise the data host is a BFF/apex: resolve the caller's home-cell
//     apiUrl from the cluster catalog (home-jurisdiction fallback).
func NewEntireAPICellClient(ctx context.Context, insecureHTTP bool, target *CellTarget) (*api.Client, error) {
	dataURL := api.BaseURL()
	if insecureHTTP {
		EnableInsecureHTTP()
	} else if err := api.RequireSecureURL(dataURL); err != nil {
		return nil, fmt.Errorf("base URL check: %w", err)
	}

	dataOrigin := api.OriginOnly(dataURL)
	host, ok := hostOf(dataOrigin)
	if !ok {
		return nil, fmt.Errorf("data API URL %q has no host to discover against", dataURL)
	}

	dctx, cancel := context.WithTimeout(ctx, dataAPIDiscoveryTimeout)
	defer cancel()
	var httpClient *http.Client
	switch {
	case cellExchangeTransportForTest != nil:
		httpClient = &http.Client{Timeout: cellDataAPITimeout, Transport: cellExchangeTransportForTest}
	case shouldUsePlainHTTPDiscovery(dataOrigin):
		httpClient = dataAPIDiscoveryClient(dataOrigin)
		httpClient.Timeout = cellDataAPITimeout
	default:
		httpClient = &http.Client{Timeout: cellDataAPITimeout}
	}

	selected, err := resolveContextForCellAPI(dctx, userdirs.Config(), userdirs.Cache(), host, httpClient, nil)
	if errors.Is(err, clusterdiscovery.ErrDiscoveryUnavailable) {
		return nil, fmt.Errorf("%s does not advertise its trusted login servers (/.well-known/entire-api.json missing or unreachable); cannot authenticate: %w", host, err)
	}
	if err != nil {
		return nil, err
	}

	// Gate the login provider's HTTPS relaxation on the core it actually dials
	// (selected.CoreURL) plus the explicit --insecure-http-auth opt-in, matching
	// the sibling ResolveDataAPIToken. A loopback data API must not relax HTTPS
	// for a non-loopback core.
	allowInsecure := insecureHTTPEnabled() || isLoopbackHTTP(selected.CoreURL)
	loginProvider, err := NewRefreshingLoginProvider(selected, cellExchangeTransportForTest, allowInsecure)
	if err != nil {
		return nil, err
	}

	loginJWT, err := loginProvider(ctx)
	if err != nil {
		if errors.Is(err, ErrNotLoggedIn) {
			return nil, fmt.Errorf("not logged in (run 'entire login' first): %w", err)
		}
		// The provider already prefixes "refresh login token:"; return as-is to
		// avoid a doubled prefix.
		return nil, err
	}

	jurisdiction, err := targetJurisdiction(target, loginJWT)
	if err != nil {
		return nil, err
	}

	coreURL := jurisdictionCoreURL(jurisdiction, dataOrigin, selected.CoreURL)
	if err := requireSafeExchangeURL("entire-core", coreURL); err != nil {
		return nil, err
	}

	cellBaseURL, err := resolveTargetCellBaseURL(ctx, target, dataOrigin, jurisdiction, coreURL, loginJWT, httpClient)
	if err != nil {
		return nil, err
	}
	if err := requireSafeExchangeURL("entire-api cell", cellBaseURL); err != nil {
		return nil, err
	}

	audience := jurisdictionAudience(jurisdiction, dataOrigin, selected.CoreURL)
	token, err := exchangeJurisdictionToken(ctx, coreURL, loginJWT, audience, httpClient)
	if err != nil {
		return nil, fmt.Errorf("exchange jurisdictional identity token: %w", err)
	}

	return api.NewClientWithBaseURL(token, cellBaseURL), nil
}

// targetJurisdiction picks the jurisdiction to mint for: the explicit repo
// target when present, otherwise the caller's home jurisdiction from the login
// JWT. The result is validated as a DNS label before it is templated into URLs.
func targetJurisdiction(target *CellTarget, loginJWT string) (string, error) {
	jurisdiction := ""
	if target != nil {
		jurisdiction = strings.TrimSpace(target.Jurisdiction)
	}
	if jurisdiction == "" {
		var err error
		jurisdiction, err = homeJurisdictionFromLoginJWT(loginJWT)
		if err != nil {
			return "", err
		}
	}
	if jurisdiction == "" {
		return "", errors.New("login token has no home_jurisdiction claim; cannot route to entire-api cell")
	}
	if !jurisdictionLabelPattern.MatchString(jurisdiction) {
		return "", fmt.Errorf("jurisdiction %q is not a valid label; refusing to route", jurisdiction)
	}
	return jurisdiction, nil
}

// resolveTargetCellBaseURL decides which cell origin to dial. See
// NewEntireAPICellClient's precedence doc.
func resolveTargetCellBaseURL(ctx context.Context, target *CellTarget, dataOrigin, jurisdiction, coreURL, loginJWT string, httpClient *http.Client) (string, error) {
	if target != nil && strings.TrimSpace(target.BaseURL) != "" {
		return strings.TrimRight(target.BaseURL, "/"), nil
	}
	if !isBFFOrigin(dataOrigin) {
		// Already a cell URL, or a loopback local-dev host: keep it verbatim.
		return strings.TrimRight(dataOrigin, "/"), nil
	}
	return resolveCellAPIBaseURL(ctx, coreURL, loginJWT, jurisdiction, httpClient)
}

// isBFFOrigin reports whether origin is a BFF / apex host that fronts multiple
// cells (so the actual cell must be resolved from the cluster catalog), as
// opposed to a direct entire-api cell (host contains ".api.") or a loopback
// local-dev host (kept verbatim). This is environment-agnostic: it recognises
// prod (entire.io), staging (partial.to) and any future apex without a
// hardcoded domain list.
func isBFFOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || isLoopbackHost(host) {
		return false
	}
	// A direct cell advertises itself under an ".api." label; anything else that
	// isn't loopback is treated as a BFF/apex needing cell resolution.
	return !strings.Contains(host, ".api.")
}

func isLoopbackHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// entireDomainFamily returns the registrable apex ("entire.io" / "partial.to")
// derived from the discovered login core's host, or "" for loopback/custom
// cores. It lets the audience/core templates follow the environment the user is
// actually logged into (prod vs staging) instead of a hardcoded prod default.
func entireDomainFamily(coreURL string) string {
	u, err := url.Parse(coreURL)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "partial.to" || strings.HasSuffix(host, ".partial.to"):
		return "partial.to"
	case host == "entire.io" || strings.HasSuffix(host, ".entire.io"):
		return "entire.io"
	default:
		return ""
	}
}

// environmentFamily picks the registrable apex to template jurisdiction URLs
// against. The configured data host (what the user pointed the CLI at) is the
// most reliable signal for prod-vs-staging, so it wins; the discovered login
// core is the fallback.
func environmentFamily(dataOrigin, discoveredCore string) string {
	if fam := entireDomainFamily(dataOrigin); fam != "" {
		return fam
	}
	return entireDomainFamily(discoveredCore)
}

// jurisdictionAudience returns the aud the entire-api cell for `jurisdiction`
// pins its identity tokens to (its jurisdiction host). Precedence:
//   - ENTIRE_API_AUDIENCE_TEMPLATE (with {jurisdiction}) if set;
//   - else https://{jurisdiction}.<family> for the environment family;
//   - else (loopback/custom) the data origin, best-effort and overridable.
//
// This mirrors the BFF's buildAudience(template, jurisdiction) (repos-stream.ts).
func jurisdictionAudience(jurisdiction, dataOrigin, discoveredCore string) string {
	if tmpl := strings.TrimSpace(os.Getenv("ENTIRE_API_AUDIENCE_TEMPLATE")); tmpl != "" {
		return applyJurisdictionTemplate(tmpl, jurisdiction)
	}
	if fam := environmentFamily(dataOrigin, discoveredCore); fam != "" {
		return "https://" + jurisdiction + "." + fam
	}
	return strings.TrimRight(dataOrigin, "/")
}

// jurisdictionCoreURL returns the entire-core origin the identity-token exchange
// is performed at for `jurisdiction`. Precedence:
//   - a loopback discovered core (local dev): honour it verbatim — the local
//     core signs the local login JWT, and the prod template would send the
//     exchange to production, which rejects the local token;
//   - ENTIRE_CORE_BASE_URL_TEMPLATE (with {jurisdiction}) if set;
//   - else https://{jurisdiction}.auth.<family> for the environment family;
//   - else the discovered core verbatim.
//
// This mirrors the BFF's buildCoreBaseUrl(template, jurisdiction, fallback),
// which honours a fallback core when the template can't produce one.
func jurisdictionCoreURL(jurisdiction, dataOrigin, discoveredCore string) string {
	if isLoopbackHTTP(discoveredCore) {
		return strings.TrimRight(discoveredCore, "/")
	}
	if tmpl := strings.TrimSpace(os.Getenv("ENTIRE_CORE_BASE_URL_TEMPLATE")); tmpl != "" {
		// Apply unconditionally: applyJurisdictionTemplate is a no-op when the
		// template has no {jurisdiction}, yielding the fixed core verbatim — the
		// single-core case, matching the BFF's buildCoreBaseUrl and this file's
		// own audience handling.
		return applyJurisdictionTemplate(tmpl, jurisdiction)
	}
	if fam := environmentFamily(dataOrigin, discoveredCore); fam != "" {
		return "https://" + jurisdiction + ".auth." + fam
	}
	return strings.TrimRight(discoveredCore, "/")
}

func applyJurisdictionTemplate(tmpl, jurisdiction string) string {
	return strings.ReplaceAll(strings.TrimRight(tmpl, "/"), "{jurisdiction}", jurisdiction)
}

// requireSafeExchangeURL rejects a target the login JWT / identity token would
// be sent to unless it is https (or an explicitly-allowed loopback/insecure
// http). It affirmatively requires the https scheme — not merely "not http" —
// so ftp/ws/scheme-relative/empty targets from a buggy core catalog can't
// smuggle the login JWT off https. Mirrors the tokenmanager guard the sibling
// data_api.go relies on.
func requireSafeExchangeURL(label, raw string) error {
	if insecureHTTPEnabled() || isLoopbackHTTP(raw) {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s URL check: parse %q: %w", label, raw, err)
	}
	if u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("%s URL %q must be https", label, raw)
	}
	return nil
}

func homeJurisdictionFromLoginJWT(loginJWT string) (string, error) {
	parts := strings.Split(loginJWT, ".")
	if len(parts) < 2 {
		return "", errors.New("login token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode login token payload: %w", err)
	}
	var claims struct {
		HomeJurisdiction string `json:"home_jurisdiction"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("parse login token payload: %w", err)
	}
	return claims.HomeJurisdiction, nil
}

type clusterListingRow struct {
	Jurisdiction string `json:"jurisdiction"`
	IsDefault    bool   `json:"isDefault"`
	APIURL       string `json:"apiUrl"`
}

// resolveCellAPIBaseURL is the home-jurisdiction fallback cell resolver: it
// lists the caller's clusters and picks the apiUrl for `jurisdiction` (default
// cluster first). It hand-parses GET /api/v1/clusters rather than reusing the
// generated coreapi.ListClusters() because coreapi imports this (auth) package,
// so auth cannot import coreapi without a cycle — the repo-scoped path avoids
// this by resolving the cell in the cli layer (see resolveExpertsCellTarget).
func resolveCellAPIBaseURL(ctx context.Context, coreURL, loginJWT, jurisdiction string, httpClient *http.Client) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(coreURL, "/")+clustersAPIPath, nil)
	if err != nil {
		return "", fmt.Errorf("build clusters request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+loginJWT)
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("list clusters: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096)) //nolint:errcheck // best-effort error-detail snippet
		return "", fmt.Errorf("list clusters: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var listing struct {
		Clusters []clusterListingRow `json:"clusters"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		return "", fmt.Errorf("decode clusters response: %w", err)
	}

	var matches []clusterListingRow
	sawJurisdiction := false
	for _, row := range listing.Clusters {
		if row.Jurisdiction != jurisdiction {
			continue
		}
		sawJurisdiction = true
		if strings.TrimSpace(row.APIURL) != "" {
			matches = append(matches, row)
		}
	}
	if len(matches) == 0 {
		if sawJurisdiction {
			// A cluster row exists for the jurisdiction but carries no apiUrl —
			// a schema/deploy problem, distinct from "no cell for jurisdiction".
			return "", fmt.Errorf("cluster for jurisdiction %q advertises no apiUrl (entire-api cell not configured?)", jurisdiction)
		}
		return "", fmt.Errorf("no entire-api cell configured for jurisdiction %q", jurisdiction)
	}
	chosen := matches[0]
	for _, row := range matches {
		if row.IsDefault {
			chosen = row
			break
		}
	}
	return strings.TrimRight(chosen.APIURL, "/"), nil
}

func exchangeJurisdictionToken(ctx context.Context, coreURL, loginJWT, audience string, httpClient *http.Client) (string, error) {
	if coreURL == "" {
		return "", errors.New("no entire-core URL configured for jurisdiction token exchange")
	}
	form := url.Values{}
	form.Set("grant_type", httputil.GrantTypeTokenExchange)
	form.Set("subject_token_type", "urn:ietf:params:oauth:token-type:access_token")
	form.Set("subject_token", loginJWT)
	form.Set("requested_token_type", "urn:ietf:params:oauth:token-type:access_token")
	form.Set("audience", audience)
	form.Set("scope", jurisdictionIdentityScope)
	form.Set("client_id", oauthClientID)

	token, _, err := httputil.PostOAuthToken(ctx, httpClient, coreURL, form)
	if err != nil {
		return "", fmt.Errorf("post token exchange: %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return "", errors.New("token exchange returned an empty access token")
	}
	return token, nil
}
