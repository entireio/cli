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
	"slices"
	"strings"
	"time"

	"github.com/entireio/auth-go/sts"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/internal/entireclient/clusterdiscovery"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

const (
	cellDataAPITimeout = 30 * time.Second

	// JurisdictionIdentityScope is the scope jurisdiction identity tokens
	// are minted with (also used by git-remote-entire's jurisdiction git
	// auth). The receiving surface authorizes live per request, so the
	// scope carries identity semantics only, not a permission grant.
	JurisdictionIdentityScope = "openid"

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

// CellTarget pins the entire-api cell a repo-scoped call must reach and its
// jurisdiction. The cli layer resolves it from the repo's own cluster (via
// coreapi mirrors/clusters), so a repo-scoped route reaches the cell that HOSTS
// the repo — not the caller's home cell. A nil target falls back to
// home-jurisdiction routing (derived from the login JWT), which is correct for
// the common same-region case and for local dev.
type CellTarget struct {
	// BaseURL is the cell's apiUrl to dial (e.g. https://aws-eu-west-1.api.entire.io).
	BaseURL string
	// Jurisdiction is the repo's cluster jurisdiction; it drives cell routing.
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
// cell, carrying the caller's login JWT directly.
//
// The login is ENTIRE_TOKEN when set, else the selected context, unless
// ENTIRE_API_BASE_URL names a data host (see resolveCellClientSubject) — the
// same identity `--to core` acts as.
//
// Cell selection, in precedence order:
//   - target != nil: dial target.BaseURL. This is the repo-scoped path — the
//     caller (cli) resolved the repo's own cell and jurisdiction.
//   - an explicit data host that already targets a cell (host contains
//     ".api.") or is loopback (local dev): keep it.
//   - otherwise: resolve the apiUrl for the jurisdiction (target.Jurisdiction,
//     else the caller's home_jurisdiction claim) from the login core's cluster
//     catalog — so a staging login lists staging's catalog and lands on a
//     staging cell, and a local-dev login lands on the cell its local core
//     advertises rather than on the core itself.
func NewEntireAPICellClient(ctx context.Context, insecureHTTP bool, target *CellTarget) (*api.Client, error) {
	factory, err := NewEntireAPICellClientFactory(ctx, insecureHTTP)
	if err != nil {
		return nil, err
	}
	return factory.ClientFor(ctx, target)
}

// CellClientFactory builds entire-api cell clients from a single resolved
// login subject. A caller dialing several cells in one operation (multi-cell
// fan-out over the caller's repos) should build one factory and reuse it for
// every cell, instead of paying discovery + login refresh once per cell via
// NewEntireAPICellClient.
//
// A factory is safe for concurrent use, and holds credentials resolved at
// construction time — build it per operation, don't store it long-term.
type CellClientFactory struct {
	subject cellSubject
}

// NewEntireAPICellClientFactory resolves the login credential once
// (resolveCellClientSubject), for building clients aimed at several cells. See
// NewEntireAPICellClient for the single-cell convenience wrapper.
func NewEntireAPICellClientFactory(ctx context.Context, insecureHTTP bool) (*CellClientFactory, error) {
	subject, err := resolveCellClientSubject(ctx, insecureHTTP)
	if err != nil {
		return nil, err
	}
	return &CellClientFactory{subject: subject}, nil
}

// ClientFor returns an authenticated client for the given cell target (nil
// falls back to home-jurisdiction routing), using the resolved login JWT as its
// bearer.
func (f *CellClientFactory) ClientFor(ctx context.Context, target *CellTarget) (*api.Client, error) {
	cellBaseURL, err := f.cellBaseURLFor(ctx, target)
	if err != nil {
		return nil, err
	}
	return api.NewClientWithBaseURL(f.subject.loginJWT, cellBaseURL), nil
}

// cellBaseURLFor resolves the cell origin ClientFor dials for target, checked
// safe to send the login JWT to.
func (f *CellClientFactory) cellBaseURLFor(ctx context.Context, target *CellTarget) (string, error) {
	jurisdiction, err := targetJurisdiction(target, f.subject.loginJWT)
	if err != nil {
		return "", err
	}

	// The catalog fallback lists clusters with loginJWT, which is signed by the
	// subject's login core — so list there, not at the templated jurisdiction
	// core, which in a multi-core setup could differ and reject the token.
	cellBaseURL, err := resolveTargetCellBaseURL(ctx, target, f.subject.dataHost, jurisdiction, f.subject.discoveredCore, f.subject.loginJWT, f.subject.httpClient)
	if err != nil {
		return "", err
	}
	if err := requireSafeExchangeURL("entire-api cell", cellBaseURL); err != nil {
		return "", err
	}
	return cellBaseURL, nil
}

// JurisdictionToken mints and returns a jurisdictional identity token
// (scope=openid, aud=jurisdiction host) for `jurisdiction`, for authenticating
// against that jurisdiction's entire-api cells (e.g.
// https://aws-us-east-2.api.entire.io/api/v1). Unlike NewEntireAPICellClient it
// returns the raw token string (it skips the cell-base-URL resolution, which is
// only needed to build a client) and never discovers against a data host.
//
// Subject credential precedence:
//   - ENTIRE_TOKEN set: the env token is the exchange subject_token, and its own
//     aud core drives the environment family (so this works with only
//     ENTIRE_TOKEN set, no ENTIRE_API_BASE_URL, in prod/staging/loopback).
//     Presence is exclusive and fail-closed — a malformed/blank value errors
//     rather than falling back to a stored login. The env token must be a login
//     JWT (subject-capable); a rejected exchange surfaces the server error.
//   - otherwise: the active stored context's refreshed login JWT.
//
// An empty `jurisdiction` falls back to the subject token's home_jurisdiction
// claim.
func JurisdictionToken(ctx context.Context, insecureHTTP bool, jurisdiction string) (string, error) {
	subject, err := resolveCellSubject(ctx, insecureHTTP)
	if err != nil {
		return "", err
	}

	j, err := resolveJurisdiction(jurisdiction, subject.loginJWT)
	if err != nil {
		return "", err
	}

	coreURL := jurisdictionCoreURL(j, subject.dataHost, subject.discoveredCore)
	if err := requireSafeExchangeURL("entire-core", coreURL); err != nil {
		return "", err
	}

	audience := jurisdictionAudience(j, subject.dataHost, subject.discoveredCore)
	token, err := exchangeJurisdictionToken(ctx, coreURL, subject.loginJWT, audience, subject.httpClient.Transport)
	if err != nil {
		return "", fmt.Errorf("exchange jurisdictional identity token: %w", err)
	}
	return token, nil
}

// cellSubject carries the credential and routing signals cell routing and the
// jurisdiction token exchange need.
type cellSubject struct {
	loginJWT string
	// discoveredCore is the core that issued loginJWT: the core to list
	// clusters at and exchange against, and the environment signal (prod /
	// staging / loopback) when no data host says otherwise.
	discoveredCore string
	// dataHost is the origin ENTIRE_API_BASE_URL names, or "" when the CLI is
	// not pointed at an explicit data host. Only an explicit host is ever
	// dialed verbatim as a cell or preferred as the environment signal; "" means
	// the cell is always resolved from discoveredCore's catalog, so a login core
	// is never mistaken for the cell it fronts.
	dataHost   string
	httpClient *http.Client
}

// resolveCellSubject picks the jurisdiction-exchange subject for
// JurisdictionToken (the `entire auth token --jurisdiction` scripting helper):
// ENTIRE_TOKEN when set (exclusive, fail-closed), otherwise the ACTIVE stored
// login context.
//
// It never runs data-host discovery, even under an ENTIRE_API_BASE_URL
// override: `--jurisdiction` mints a token for the caller's SELECTED
// environment, and the target jurisdiction comes from the flag, not from
// whatever data host is configured. resolveCellClientSubject differs only in
// honouring an explicit data host, because it dials the data plane.
func resolveCellSubject(ctx context.Context, insecureHTTP bool) (cellSubject, error) {
	if raw, ok := os.LookupEnv(EnvTokenVar); ok {
		return resolveEnvTokenCellSubject(raw, insecureHTTP)
	}
	return resolveActiveContextCellSubject(ctx, insecureHTTP)
}

// resolveActiveContextCellSubject builds the subject from the selected stored
// login context (--context / $ENTIRE_CONTEXT / current_context): it refreshes
// that context's login JWT and uses the context's own core as the environment
// signal and the core to exchange at / list clusters from. Shared by
// resolveCellSubject and resolveCellClientSubject.
func resolveActiveContextCellSubject(ctx context.Context, insecureHTTP bool) (cellSubject, error) {
	if insecureHTTP {
		EnableInsecureHTTP()
	}
	c, ok, err := ActiveContext()
	if err != nil {
		return cellSubject{}, err
	}
	if !ok {
		return cellSubject{}, fmt.Errorf("not logged in (run 'entire login' first): %w", ErrNotLoggedIn)
	}

	loginJWT, err := refreshCellLoginJWT(ctx, c)
	if err != nil {
		return cellSubject{}, err
	}

	origin := api.OriginOnly(c.CoreURL)
	return cellSubject{
		loginJWT:       loginJWT,
		discoveredCore: origin,
		httpClient:     cellExchangeHTTPClient(origin),
	}, nil
}

// resolveCellClientSubject resolves the cell-client subject: ENTIRE_TOKEN when
// set (exclusive, fail-closed — the precedence `--to core` and `auth status`
// apply), else the SELECTED context (--context / $ENTIRE_CONTEXT /
// current_context), unless ENTIRE_API_BASE_URL names a data host, in which
// case the login is discovered against that host.
//
// Following the login makes the environment track it the way it does for
// `--to core`. Discovering against api.BaseURL() here instead would, with no
// override, mean the production apex: clusterdiscovery.selectLoginContext
// checks the selected context against entire.io's trusted issuers and refuses
// a staging (partial.to) login with "API host entire.io does not accept the
// login selected by --context", leaving every staging cell unreachable through
// the CLI (COR-1634) — and only failing closed because the environments share
// no credentials. An explicit override is different: the user named a host the
// request must reach, so its trusted-issuer document decides which saved login
// may authenticate it rather than the selected context being trusted blindly.
// An env token is used verbatim even under an override, as coreapi.New does —
// but the override still names the host to dial: a direct cell or loopback
// dev server stays verbatim, exactly as it does for a stored login, so a
// pasted token plus a local ENTIRE_API_BASE_URL keeps the request on the
// machine instead of sending it to the token's home cell.
func resolveCellClientSubject(ctx context.Context, insecureHTTP bool) (cellSubject, error) {
	dataURL, overridden := api.BaseURLOverride()
	if raw, ok := os.LookupEnv(EnvTokenVar); ok {
		subject, err := resolveEnvTokenCellSubject(raw, insecureHTTP)
		if err != nil {
			return cellSubject{}, err
		}
		if overridden {
			if !insecureHTTP {
				if err := api.RequireSecureURL(dataURL); err != nil {
					return cellSubject{}, fmt.Errorf("base URL check: %w", err)
				}
			}
			subject.dataHost = api.OriginOnly(dataURL)
		}
		return subject, nil
	}
	if !overridden {
		return resolveActiveContextCellSubject(ctx, insecureHTTP)
	}
	return resolveDiscoveredCellSubject(ctx, insecureHTTP, dataURL)
}

// resolveDiscoveredCellSubject builds the subject for an explicitly configured
// data host: it discovers the host's trusted login servers, picks the saved
// login they accept, and refreshes that login's JWT.
func resolveDiscoveredCellSubject(ctx context.Context, insecureHTTP bool, dataURL string) (cellSubject, error) {
	if insecureHTTP {
		EnableInsecureHTTP()
	} else if err := api.RequireSecureURL(dataURL); err != nil {
		return cellSubject{}, fmt.Errorf("base URL check: %w", err)
	}

	dataOrigin := api.OriginOnly(dataURL)
	host, ok := hostOf(dataOrigin)
	if !ok {
		return cellSubject{}, fmt.Errorf("data API URL %q has no host to discover against", dataURL)
	}

	dctx, cancel := context.WithTimeout(ctx, dataAPIDiscoveryTimeout)
	defer cancel()
	httpClient := cellExchangeHTTPClient(dataOrigin)

	selected, err := resolveContextForCellAPI(dctx, userdirs.Config(), userdirs.Cache(), host, httpClient, nil)
	if errors.Is(err, clusterdiscovery.ErrDiscoveryUnavailable) {
		return cellSubject{}, fmt.Errorf("%s does not advertise its trusted login servers (/.well-known/entire-api.json missing or unreachable); cannot authenticate: %w", host, err)
	}
	if err != nil {
		return cellSubject{}, err
	}
	announceLogin(selected)

	loginJWT, err := refreshCellLoginJWT(ctx, selected)
	if err != nil {
		return cellSubject{}, err
	}

	return cellSubject{
		loginJWT:       loginJWT,
		discoveredCore: selected.CoreURL,
		dataHost:       dataOrigin,
		httpClient:     httpClient,
	}, nil
}

// refreshCellLoginJWT returns c's login JWT, transparently re-minting it from the
// stored refresh token. Shared by the active-context and discovered-context cell
// subject resolvers, which differ only in how they pick c.
func refreshCellLoginJWT(ctx context.Context, c *contexts.Context) (string, error) {
	// Gate the login provider's HTTPS relaxation on the core it actually dials
	// plus the explicit --insecure-http-auth opt-in: a loopback core must not
	// relax HTTPS for a non-loopback one.
	allowInsecure := insecureHTTPEnabled() || isLoopbackHTTP(c.CoreURL)
	loginProvider, err := NewRefreshingLoginProvider(c, cellExchangeTransportForTest, allowInsecure)
	if err != nil {
		return "", err
	}
	loginJWT, err := loginProvider(ctx)
	if err != nil {
		if errors.Is(err, ErrNotLoggedIn) {
			return "", fmt.Errorf("not logged in (run 'entire login' first): %w", err)
		}
		// The provider already prefixes "refresh login token:"; return as-is to
		// avoid a doubled prefix.
		return "", err
	}
	return loginJWT, nil
}

// resolveEnvTokenCellSubject builds the subject from ENTIRE_TOKEN: the env token
// is the login JWT and its aud core is the environment signal, so the
// audience/core templates and the cell catalog follow prod/staging/loopback
// without ENTIRE_API_BASE_URL. Discovery is skipped — the token is used
// verbatim. Presence is fail-closed via ParseEnvToken.
func resolveEnvTokenCellSubject(raw string, insecureHTTP bool) (cellSubject, error) {
	if insecureHTTP {
		EnableInsecureHTTP()
	}
	core, token, err := ParseEnvToken(raw)
	if err != nil {
		return cellSubject{}, err
	}
	return cellSubject{
		loginJWT:       token,
		discoveredCore: core,
		httpClient:     cellExchangeHTTPClient(core),
	}, nil
}

// cellExchangeHTTPClient builds the HTTP client used for jurisdiction token
// exchange (and the home-jurisdiction cluster listing). It honours the test
// transport seam, then the plain-HTTP-discovery relaxation for a loopback
// origin, else a plain timeout client.
func cellExchangeHTTPClient(origin string) *http.Client {
	switch {
	case cellExchangeTransportForTest != nil:
		return &http.Client{Timeout: cellDataAPITimeout, Transport: versioninfo.WrapTransport(cellExchangeTransportForTest)}
	case shouldUsePlainHTTPDiscovery(origin):
		c := dataAPIDiscoveryClient(origin)
		c.Timeout = cellDataAPITimeout
		return c
	default:
		return &http.Client{Timeout: cellDataAPITimeout, Transport: versioninfo.WrapTransport(nil)}
	}
}

// targetJurisdiction picks the jurisdiction to mint for from a repo CellTarget:
// the target's explicit jurisdiction when present, otherwise the caller's home
// jurisdiction from the login JWT.
func targetJurisdiction(target *CellTarget, loginJWT string) (string, error) {
	override := ""
	if target != nil {
		override = target.Jurisdiction
	}
	return resolveJurisdiction(override, loginJWT)
}

// resolveJurisdiction picks the jurisdiction to mint for: the explicit override
// when non-empty, otherwise the subject token's home_jurisdiction claim. Either
// source is normalised to a lowercase DNS label and validated before it is
// templated into URLs — `--jurisdiction US`, `" us "` and `us` all resolve to
// `us`, and an uppercase home_jurisdiction claim routes instead of hard-failing
// the strict [a-z0-9-] label check.
func resolveJurisdiction(override, loginJWT string) (string, error) {
	jurisdiction := strings.TrimSpace(override)
	if jurisdiction == "" {
		var err error
		jurisdiction, err = HomeJurisdictionFromLoginJWT(loginJWT)
		if err != nil {
			return "", err
		}
	}
	jurisdiction, err := NormalizeJurisdiction(jurisdiction)
	if err != nil {
		return "", fmt.Errorf("%w; refusing to route", err)
	}
	if jurisdiction == "" {
		return "", errors.New("login token has no home_jurisdiction claim; cannot route to entire-api cell")
	}
	return jurisdiction, nil
}

// NormalizeJurisdiction is the one rule for a user- or claim-supplied
// jurisdiction: trimmed, lowercased, and constrained to a single DNS label
// (`--jurisdiction US`, `" us "` and `us` all yield `us`). Empty is returned as
// "" without error so callers can apply their own default (home).
func NormalizeJurisdiction(value string) (string, error) {
	jurisdiction := strings.ToLower(strings.TrimSpace(value))
	if jurisdiction == "" {
		return "", nil
	}
	if !jurisdictionLabelPattern.MatchString(jurisdiction) {
		return "", fmt.Errorf("jurisdiction %q is not a valid label", jurisdiction)
	}
	return jurisdiction, nil
}

// resolveTargetCellBaseURL decides which cell origin to dial. See
// NewEntireAPICellClient's precedence doc. dataHost is the explicit
// ENTIRE_API_BASE_URL origin or "" (cellSubject.dataHost); listCoreURL is the
// login core whose cluster catalog is consulted, so it must accept loginJWT.
func resolveTargetCellBaseURL(ctx context.Context, target *CellTarget, dataHost, jurisdiction, listCoreURL, loginJWT string, httpClient *http.Client) (string, error) {
	if target != nil && strings.TrimSpace(target.BaseURL) != "" {
		return strings.TrimRight(target.BaseURL, "/"), nil
	}
	// Only an EXPLICIT data host is ever dialed verbatim: when it isn't a
	// BFF/apex fronting multiple cells — i.e. it's already a direct cell or a
	// loopback dev host — EXCEPT when a jurisdiction is explicitly pinned
	// (target.Jurisdiction, e.g. `entire api --jurisdiction eu`) against a
	// non-loopback origin. A pinned jurisdiction may name a DIFFERENT cell than
	// the configured direct-cell origin, so dialing that origin verbatim would
	// send an identity token minted for the pinned jurisdiction to the wrong
	// cell; resolve the pinned jurisdiction's own cell from the catalog instead.
	// A loopback dev host serves a single cell with no jurisdiction catalog, so
	// it always stays verbatim. With no data host the only origin known is the
	// login core, which is not a cell (a loopback core would otherwise be dialed
	// as one), so the catalog decides.
	if dataHost != "" {
		explicitJurisdiction := target != nil && strings.TrimSpace(target.Jurisdiction) != ""
		if !isBFFOrigin(dataHost) && (!explicitJurisdiction || isLoopbackOrigin(dataHost)) {
			return strings.TrimRight(dataHost, "/"), nil
		}
	}
	return resolveCellAPIBaseURL(ctx, listCoreURL, loginJWT, jurisdiction, httpClient)
}

// isLoopbackOrigin reports whether origin's host is a loopback address, at any
// scheme (isLoopbackHTTP only accepts http). Used to keep a local-dev cell
// verbatim even when a jurisdiction is explicitly pinned.
func isLoopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return isLoopbackHost(strings.ToLower(u.Hostname()))
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

// EntireSite returns the Entire site ("entire.io" / "partial.to") a login
// server or data host belongs to, or "" for loopback and custom hosts.
func EntireSite(coreURL string) string {
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
// against. An explicit data host (what the user pointed the CLI at) is the
// most reliable signal for prod-vs-staging, so it wins when set; the login core
// is the fallback, and the only signal when dataHost is "".
func environmentFamily(dataHost, discoveredCore string) string {
	if fam := EntireSite(dataHost); fam != "" {
		return fam
	}
	return EntireSite(discoveredCore)
}

// jurisdictionAudience returns the aud the entire-api cell for `jurisdiction`
// pins its identity tokens to (its jurisdiction host). Precedence:
//   - ENTIRE_API_AUDIENCE_TEMPLATE (with {jurisdiction}) if set;
//   - else https://{jurisdiction}.<family> for the environment family;
//   - else (loopback/custom) the explicit data host, or the login core when
//     none is configured — best-effort and overridable.
//
// This mirrors the BFF's buildAudience(template, jurisdiction) (repos-stream.ts).
func jurisdictionAudience(jurisdiction, dataHost, discoveredCore string) string {
	if tmpl := strings.TrimSpace(os.Getenv("ENTIRE_API_AUDIENCE_TEMPLATE")); tmpl != "" {
		return applyJurisdictionTemplate(tmpl, jurisdiction)
	}
	if fam := environmentFamily(dataHost, discoveredCore); fam != "" {
		return "https://" + jurisdiction + "." + fam
	}
	if dataHost == "" {
		return strings.TrimRight(discoveredCore, "/")
	}
	return strings.TrimRight(dataHost, "/")
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
func jurisdictionCoreURL(jurisdiction, dataHost, discoveredCore string) string {
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
	if fam := environmentFamily(dataHost, discoveredCore); fam != "" {
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

// HomeJurisdictionFromLoginJWT reads the home_jurisdiction claim without
// verifying the signature — callers only route with it; the server
// re-verifies. The claim is normalized (NormalizeJurisdiction), so a
// malformed label is an error here and every reader sees one spelling.
// Returns "" (no error) when the claim is absent so each caller can phrase
// its own missing-claim error. Shared with git-remote-entire's jurisdiction
// git auth.
func HomeJurisdictionFromLoginJWT(loginJWT string) (string, error) {
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
	return NormalizeJurisdiction(claims.HomeJurisdiction)
}

type clusterListingRow struct {
	Jurisdiction string `json:"jurisdiction"`
	IsDefault    bool   `json:"isDefault"`
	APIURL       string `json:"apiUrl"`
}

// ErrNoCellForJurisdiction signals that the requested jurisdiction — the
// caller's home, or an explicit --jurisdiction — has no entire-api cell in the
// login core's cluster catalog (or its row carries no apiUrl). It is not fatal:
// callers that also have a data-API path (e.g. activity/recap) treat it as
// "entire-api isn't serving this region yet" and fall back rather than failing
// the command. errors.Is unwraps it from the contextual message, which names
// the core consulted and the jurisdictions it does serve.
var ErrNoCellForJurisdiction = errors.New("no entire-api cell configured for jurisdiction")

// resolveCellAPIBaseURL is the catalog cell resolver: it lists the clusters of
// the login core at coreURL and picks the apiUrl for `jurisdiction` (default
// cluster first). It hand-parses GET /api/v1/clusters rather than reusing the
// generated coreapi.ListClusters() because coreapi imports this (auth) package,
// so auth cannot import coreapi without a cycle — the repo-scoped path avoids
// this by resolving the cell in the cli layer (see resolveRepoCellTarget).
func resolveCellAPIBaseURL(ctx context.Context, coreURL, loginJWT, jurisdiction string, httpClient *http.Client) (string, error) {
	coreURL = strings.TrimRight(coreURL, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, coreURL+clustersAPIPath, nil)
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
		// jurisdiction is already a folded lowercase label (resolveJurisdiction);
		// fold the catalog row too so a differently-cased row still matches
		// instead of misreporting "no cell for jurisdiction".
		if !strings.EqualFold(strings.TrimSpace(row.Jurisdiction), jurisdiction) {
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
			return "", fmt.Errorf("%w %q at %s: cluster advertises no apiUrl (entire-api cell not configured?)", ErrNoCellForJurisdiction, jurisdiction, coreURL)
		}
		// Name the environment consulted and what it does serve: `-j eu` against
		// a staging login that only has a us cell is fixed by picking another
		// slug or another context, and the message should say which.
		return "", fmt.Errorf("%w %q at %s (jurisdictions with a cell: %s)", ErrNoCellForJurisdiction, jurisdiction, coreURL, servedJurisdictions(listing.Clusters))
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

// servedJurisdictions renders the jurisdictions a cluster catalog has a cell
// for — rows with an apiUrl whose label `-j` could accept — sorted and
// deduplicated, or "none".
func servedJurisdictions(rows []clusterListingRow) string {
	var names []string
	for _, row := range rows {
		j, err := NormalizeJurisdiction(row.Jurisdiction)
		if err != nil || j == "" || strings.TrimSpace(row.APIURL) == "" {
			continue
		}
		names = append(names, j)
	}
	if len(names) == 0 {
		return "none"
	}
	slices.Sort(names)
	return strings.Join(slices.Compact(names), ", ")
}

// exchangeJurisdictionToken mints the jurisdictional identity token for a
// cell, trading the login JWT for one pinned to audience.
//
// Through auth-go's sts client rather than a hand-rolled POST, so the CLI has
// one RFC 8693 implementation: the duplicate this replaced had drifted, losing
// auth-go's terminal-escape sanitisation of server error text and keeping its
// own redirect guard in the CLI rather than the library.
//
// Takes the transport, not the caller's *http.Client: only the transport (and
// so the connection pool) carries over. The Timeout deliberately does not —
// sts applies the same budget via context.WithTimeout, which unlike
// Client.Timeout does not cancel the post-response body read. Note sts also
// narrows plain HTTP to loopback on top of AllowInsecureHTTP, so that is the
// effective policy here regardless of --insecure-http-auth.
//
// subject_token_type stays access_token, not JWT — what the replaced form sent
// and what entire-core matches on.
func exchangeJurisdictionToken(ctx context.Context, coreURL, loginJWT, audience string, transport http.RoundTripper) (string, error) {
	if coreURL == "" {
		return "", errors.New("no entire-core URL configured for jurisdiction token exchange")
	}
	client := &sts.Client{
		Transport:         transport,
		BaseURL:           coreURL,
		Path:              oauthTokenPath,
		AllowInsecureHTTP: shouldUsePlainHTTPDiscovery(coreURL),
		RequestTimeout:    cellDataAPITimeout,
	}
	ts, err := client.Exchange(ctx, sts.ExchangeRequest{
		SubjectToken:       loginJWT,
		SubjectTokenType:   sts.SubjectTokenTypeAccessToken,
		RequestedTokenType: sts.SubjectTokenTypeAccessToken,
		Audience:           audience,
		Scope:              JurisdictionIdentityScope,
		ClientID:           oauthClientID,
	})
	if err != nil {
		return "", fmt.Errorf("post token exchange: %w", err)
	}
	if strings.TrimSpace(ts.AccessToken) == "" {
		return "", errors.New("token exchange returned an empty access token")
	}
	return ts.AccessToken, nil
}
