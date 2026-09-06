package coreapi

import (
	"fmt"
	"net/http"
	"net/url"
	"os"

	"github.com/entireio/auth-go/crossjuris"

	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/internal/entireclient/httpclient"
	"github.com/entireio/cli/internal/entireclient/httputil"
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

// newCrossJurisRoundTripper builds the coreapi transport over base: the
// shared crossjuris follower, and nothing else.
//
// It used to stack a CLI-specific canonicalizer on the outside, rewriting the
// Location a 202 from POST /api/v1/mirror-requests carries onto the origin
// that answered. Nothing reads that header any more — the async mirror-create
// poll is driven by the 202 body's requestId and stays on the client's own
// base URL (see awaitMirrorPlacement) — so the rewrite was maintaining a
// header no caller consumed, over a value the server supplies. Reviving it
// means giving a server-named host a say in where the control-plane bearer is
// sent, which is what the follower's federation check exists to gate.
func newCrossJurisRoundTripper(base http.RoundTripper, allowInsecureHTTP bool) (http.RoundTripper, error) {
	inner, err := crossjuris.New(crossjuris.Config{
		Base:              base,
		ClientID:          httputil.OAuthClientID,
		AllowInsecureHTTP: allowInsecureHTTP,
		Logf:              debugf,
	})
	if err != nil {
		return nil, fmt.Errorf("cross-juris transport: %w", err)
	}
	return inner, nil
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
