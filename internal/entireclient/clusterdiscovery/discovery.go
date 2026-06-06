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
	url := "https://" + clusterHost + Path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build discovery request: %w", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		debugf("cluster discovery: %v", err)
		return nil, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusServiceUnavailable {
		return nil, ErrNoIssuers
	}
	if resp.StatusCode != http.StatusOK {
		debugf("cluster discovery: HTTP %d from %s", resp.StatusCode, url)
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	var body Response
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		debugf("cluster discovery: decode: %v", err)
		return nil, fmt.Errorf("decode %s: %w", url, err)
	}
	if len(body.CoreURLs) == 0 {
		debugf("cluster discovery: no core_urls in response from %s", url)
		return nil, ErrNoCoreURLs
	}
	return &body, nil
}

// RenderLoginHint formats a fatal-ready message describing which
// entire-core URLs an operator can log into to gain credentials for
// clusterHost. The output is stable (one indented URL per line) so
// callers can pattern-match in tests.
func RenderLoginHint(clusterHost string, coreURLs []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "no auth context for cluster %s. This cluster accepts logins from:\n", clusterHost)
	for _, u := range coreURLs {
		fmt.Fprintf(&b, "  %s\n", u)
	}
	fmt.Fprint(&b, "\nAuthenticate against one of those login servers and re-run your command:\n"+
		"  ENTIRE_AUTH_BASE_URL=<url> entire login\n"+
		"or, if you already have a login there, switch to it with `entire auth use <context>`.")
	return b.String()
}
