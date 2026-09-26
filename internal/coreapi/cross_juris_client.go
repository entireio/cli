package coreapi

import (
	"fmt"
	"net/http"
	"net/url"
	"os"

	"github.com/entireio/auth-go/crossjuris"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/internal/entireclient/httpclient"
)

// newCrossJurisHTTPClient builds the *http.Client the control-plane
// (coreapi) client dials with. Its transport follows cross-jurisdiction
// 421 redirects and runs the RFC 8693 token exchange a foreign core
// requires, so a home-region login JWT can operate on a resource whose
// home jurisdiction is another region. Inert for same-jurisdiction calls.
//
// coreURL is the origin the client is built against. An http:// loopback
// core (local dev, httptest) unlocks http:// loopback redirect and
// exchange targets; anything else demands https, so a production core
// can never re-target the login JWT over plaintext.
func newCrossJurisHTTPClient(coreURL string) (*http.Client, error) {
	// The User-Agent wrapper goes *under* the cross-juris round tripper,
	// not over it. That transport sends requests it builds itself — the
	// RFC 8693 exchange and the federation manifest fetch, both straight
	// to its base — and those bypass anything wrapped outside it.
	base := versioninfo.WrapTransport(httpclient.NewTransport(false))
	rt, err := newCrossJurisRoundTripper(base, isLoopbackHTTP(coreURL))
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: rt}, nil
}

// newCrossJurisRoundTripper stacks the coreapi transport chain over base:
// the shared crossjuris follower, then the CLI-specific Location
// canonicalizer on the outside so it sees the replayed request.
func newCrossJurisRoundTripper(base http.RoundTripper, allowInsecureHTTP bool) (http.RoundTripper, error) {
	inner, err := crossjuris.New(crossjuris.Config{
		Base:              base,
		ClientID:          auth.OAuthClientID,
		AllowInsecureHTTP: allowInsecureHTTP,
		Logf:              debugf,
	})
	if err != nil {
		return nil, fmt.Errorf("cross-juris transport: %w", err)
	}
	return mirrorLocationCanonicalizer{next: inner}, nil
}

// debugf writes ENTIRE_DEBUG-gated trace lines for the transport. The
// recovery hops (421 follow, token exchange) are otherwise invisible, so
// a misconfigured federation / off-origin reject is hard to diagnose
// without this.
func debugf(format string, args ...any) {
	if os.Getenv("ENTIRE_DEBUG") == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "[entire] cross-juris transport: "+format+"\n", args...)
}

// mirrorLocationCanonicalizer rewrites the Location a 202 from
// POST /api/v1/mirror-requests carries onto the origin that actually
// answered. The home core emits a relative or self-rooted Location; after
// a 421 follow that is a different host than the caller dialled, so the
// scheme and host come from resp.Request — the replayed request — not
// from the caller's.
type mirrorLocationCanonicalizer struct {
	next http.RoundTripper
}

func (c mirrorLocationCanonicalizer) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.next.RoundTrip(req)
	if err != nil {
		return nil, err //nolint:wrapcheck // http.Client already names method and URL
	}
	canonicalizeMirrorRequestLocation(req, resp)
	return resp, nil
}

func canonicalizeMirrorRequestLocation(req *http.Request, resp *http.Response) {
	answered := resp.Request
	if answered == nil {
		answered = req
	}
	if resp.StatusCode != http.StatusAccepted || answered.URL.Path != apiBasePath+"/mirror-requests" {
		return
	}
	location, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || location.Path == "" {
		return
	}
	location.Scheme = answered.URL.Scheme
	location.Host = answered.URL.Host
	resp.Header.Set("Location", location.String())
}

// isLoopbackHTTP reports whether rawURL is http:// at a loopback host.
func isLoopbackHTTP(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "http" {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}
