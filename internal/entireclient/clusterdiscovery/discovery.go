// Package clusterdiscovery resolves the trusted entire-core URLs that a
// given entire-server cluster will accept JWTs from, by hitting the
// cluster's /.well-known/entire-cluster.json endpoint. Used on the
// cold-boot path where contexts.json doesn't yet bind a cluster to a
// context, so we can tell the user *which* core(s) to log into instead
// of leaving them to guess --base-url.
package clusterdiscovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

// Path mirrors server/cluster_discovery.go: the cluster advertises which
// entire-core URLs it accepts as JWT issuers.
const Path = "/.well-known/entire-cluster.json"

// DebugFunc is the shape of the optional debug-log callback callers pass
// in. It mirrors fmt.Printf-style formatting so each caller can plug in
// its own env-var-gated logger (e.g. the [git-remote-entire] /
// [entiredb] prefixed debugfs gated by ENTIRE_DEBUG). Pass nil to
// suppress debug output.
type DebugFunc func(format string, args ...any)

// Response is the parsed shape of /.well-known/entire-cluster.json. New
// fields may be added by the server; unknown ones are ignored.
type Response struct {
	CoreURLs []string `json:"core_urls"`
	// JurisdictionAudience is the cluster's jurisdiction-token audience (the
	// exact aud its data plane accepts on jurisdiction access tokens,
	// e.g. https://au.entire.io). Empty when the cluster does not accept
	// jurisdiction tokens or predates the field.
	JurisdictionAudience string `json:"jurisdiction_audience"`
	// JurisdictionCoreURL is the core (OAuth AS) that mints tokens for
	// JurisdictionAudience — the endpoint a cross-jurisdiction
	// token exchange dials. The audience itself is not dialable.
	JurisdictionCoreURL string `json:"jurisdiction_core_url"`
	// LoginURL is the cluster's advertised login server — the apex auth
	// router (e.g. https://auth.entire.io), which fans an authorization
	// request out to the caller's own regional core. Empty when the cluster
	// advertises none or predates the field.
	//
	// Never a member of CoreURLs, and never eligible to be: the apex issues
	// no tokens, so no login can carry it as an issuer. It answers "what do
	// I type", not "whose tokens are accepted". For the same reason it is
	// outside requireSameSiteIssuers' gate: no token is ever sent because
	// of it, only a URL shown in a hint.
	LoginURL string `json:"login_url"`
}

// Sentinel errors returned by Discover so callers can branch on the
// failure mode (and surface different operator messages) without
// stringly-typing the diagnosis.
var (
	// ErrUnreachable wraps any transport-level failure dialing the
	// cluster (DNS, connection refused, TLS handshake, timeout). The
	// host might be a typo or a real-but-down cluster — the client
	// can't tell, and operators get the same nudge for both.
	ErrUnreachable = errors.New("cluster unreachable")
	// ErrNoIssuers means the cluster responded HTTP 503 — up but with
	// an empty trusted-issuers configuration. Operator misconfig,
	// not a client problem.
	ErrNoIssuers = errors.New("cluster does not advertise any trusted login servers")
	// ErrNoCoreURLs means the cluster responded HTTP 200 but the JSON
	// carried an empty (or absent) core_urls list. Distinct from
	// ErrNoIssuers because the operator fix is different — the
	// response shape is wrong, rather than the 503 surface being
	// served.
	ErrNoCoreURLs = errors.New("cluster advertises no trusted core URLs")
)

// statusError carries the HTTP status from a well-known fetch that
// returned a non-200, so each caller (cluster vs api) can map specific
// codes to its own sentinel (503 → not-configured, 404 → not-advertised)
// without the shared fetcher knowing either contract.
type statusError struct {
	Code int
	URL  string
}

func (e *statusError) Error() string { return fmt.Sprintf("HTTP %d from %s", e.Code, e.URL) }

// fetchWellKnownJSON GETs https://host+path and decodes a 200 body into
// out. Transport failures are wrapped under ErrUnreachable; a non-200
// returns a *statusError so the caller can branch on Code; a malformed
// 200 body returns a wrapped decode error. The scheme is hard-coded to
// https: the response is a trust root (which login servers to honour),
// so it must be TLS-authenticated — a plaintext fetch would let a
// network attacker advertise an attacker-controlled issuer.
func fetchWellKnownJSON(ctx context.Context, host, path string, c *http.Client, out any, debugf DebugFunc) error {
	url := "https://" + host + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build discovery request: %w", err)
	}
	// Refuse redirects. This is a trust-root fetch — the response decides which
	// login servers we honour — so a 3xx to another origin (or a plaintext
	// downgrade) from a hostile/misconfigured host must not be followed.
	// Shallow-copy the caller's client so we don't mutate its redirect policy
	// (it's reused for other operations); the copy shares Transport/TLS config.
	if c == nil {
		c = http.DefaultClient
	}
	noRedirect := *c
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("discovery does not follow redirects (trust root)")
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		debugf("discovery: %v", err)
		return fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		debugf("discovery: HTTP %d from %s", resp.StatusCode, url)
		return &statusError{Code: resp.StatusCode, URL: url}
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		debugf("discovery: decode: %v", err)
		return fmt.Errorf("decode %s: %w", url, err)
	}
	return nil
}

// Discover fetches and parses the cluster's
// /.well-known/entire-cluster.json. On success returns the parsed body
// with a non-empty CoreURLs list. On failure returns one of the
// sentinel errors above (wrapped with context) or a wrapped decode
// error for malformed JSON. Network failures are wrapped under
// ErrUnreachable so the caller can render the "looks unreachable"
// nudge without string-matching.
//
// debugf is optional; nil suppresses debug output.
func Discover(ctx context.Context, clusterHost string, c *http.Client, debugf DebugFunc) (*Response, error) {
	if debugf == nil {
		debugf = func(string, ...any) {}
	}
	var body Response
	err := fetchWellKnownJSON(ctx, clusterHost, Path, c, &body, debugf)
	var se *statusError
	switch {
	case errors.As(err, &se) && se.Code == http.StatusServiceUnavailable:
		return nil, ErrNoIssuers
	case err != nil:
		return nil, err
	}
	if len(body.CoreURLs) == 0 {
		debugf("cluster discovery: no core_urls in response from https://%s%s", clusterHost, Path)
		return nil, ErrNoCoreURLs
	}
	return &body, nil
}

// loginTargets is what a resource says about authenticating to it. The two
// fields answer different questions and are not interchangeable: coreURLs is
// the trust set, which decides whether a saved login is *accepted*; loginURL is
// the one server to send the operator to, which is the *remedy*.
type loginTargets struct {
	coreURLs []string
	loginURL string
}

// renderLoginHint formats a fatal-ready "no auth context for <subject>"
// message telling the operator how to get one. subject is a noun phrase like
// "cluster nyc.entire.io" or "API host partial.to" so the same hint serves both
// the git-cluster and data-API resolvers.
//
// The instruction half is shared with the "active login rejected and nothing
// saved fits" error, which supplies its own first line — see
// renderLoginInstruction.
func renderLoginHint(subject string, t loginTargets) string {
	return fmt.Sprintf("no auth context for %s.\n%s", subject, renderLoginInstruction(t))
}

// renderLoginInstruction is the actionable tail of a login hint: the command
// that obtains a login the resource will accept. Split out so callers that have
// already explained the situation in their own words can append the remedy
// without repeating "no auth context for <subject>".
//
// An advertised login server ends it — that host is the apex router, and it
// dispatches to whichever regional core owns the operator's account, so one URL
// serves every account on the federation. When that host is the one `entire
// login` already defaults to, the flag is dropped: naming it would teach a flag
// whose own help text says it is rarely needed, and a user who learns it here is
// liable to carry it to a host it doesn't belong on. Compared against the
// constant rather than a literal, so the message follows if the default moves.
//
// Without one, the trusted issuers are named instead. They are a set of
// candidates rather than an instruction, but for anything outside the default
// federation they are the only actionable part: bare `entire login`
// re-authenticates against api.DefaultAuthBaseURL, which for a resource that
// doesn't trust that server reproduces the very failure being reported. They are
// normalised and de-duplicated so a resource advertising the same core twice (or
// with an inconsistent trailing slash) doesn't repeat itself, and the order is
// the resource's own — its preferred core first.
func renderLoginInstruction(t loginTargets) string {
	if loginURL := normalizeCoreURL(t.loginURL); loginURL != "" {
		if loginURL == normalizeCoreURL(api.DefaultAuthBaseURL) {
			return "Log in with `entire login`, then re-run your command."
		}
		return fmt.Sprintf("Log in with `entire login --server %s`, then re-run your command.", loginURL)
	}
	servers := trustedLoginServers(t.coreURLs)
	if len(servers) == 0 {
		return "Log in with `entire login`, then re-run your command."
	}
	return fmt.Sprintf("It trusts these login servers: %s\nLog in with `entire login --server <url>`, then re-run your command.",
		strings.Join(servers, ", "))
}

// trustedLoginServers normalises a resource's advertised cores for display
// through normalizeCoreURL — the same folding the eligibility check uses, so a
// server echoed back as trusted is one that would actually be accepted — then
// drops blanks and duplicates while preserving the advertised order.
func trustedLoginServers(coreURLs []string) []string {
	seen := make(map[string]bool, len(coreURLs))
	out := make([]string, 0, len(coreURLs))
	for _, coreURL := range coreURLs {
		normalized := normalizeCoreURL(coreURL)
		if normalized == "" || seen[normalized] {
			continue
		}
		seen[normalized] = true
		out = append(out, normalized)
	}
	return out
}
