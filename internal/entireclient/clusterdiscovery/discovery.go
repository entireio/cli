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

// RenderLoginHint formats a fatal-ready "no auth context for cluster X"
// message telling the operator to run `entire login`. coreURLs (the cluster's
// advertised login servers) is accepted but intentionally not yet surfaced —
// see renderLoginHint.
func RenderLoginHint(clusterHost string, coreURLs []string) string {
	return renderLoginHint("cluster "+clusterHost, coreURLs)
}

// renderLoginHint is the subject-agnostic form behind RenderLoginHint:
// subject is a noun phrase like "cluster nyc.entire.io" or "API host
// partial.to" so the same hint serves both the git-cluster and data-API
// resolvers.
//
// We'll display coreURLs (2nd param) to the user as a hint at a later stage.
func renderLoginHint(subject string, _ []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "no auth context for %s.\n", subject)
	fmt.Fprint(&b, "Log in with `entire login`, then re-run your command.")
	return b.String()
}
