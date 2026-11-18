// Package githelper implements the git-remote-helper protocol: the
// "capabilities / list / option / connect / stateless-connect / push"
// command loop that canonical Git speaks to any helper binary on
// stdin/stdout, plus the bodies of those commands (ref listing,
// fetch negotiation, push relay).
//
// The protocol layer is decoupled from the wire layer via the
// Transport interface so callers can plug a different backend in
// without dragging Entire-specific concerns into this package.
package githelper

import (
	"context"
	"io"
	"net/http"
)

// Transport is the wire layer the helper protocol speaks against. In
// production it's *entire/transport.Proxy; tests can supply a fake.
type Transport interface {
	// InfoRefs fetches the v0/v1 ref advertisement.
	InfoRefs(ctx context.Context, service string) (io.ReadCloser, error)
	// InfoRefsV2 fetches the v2 capability advertisement.
	InfoRefsV2(ctx context.Context) (io.ReadCloser, error)
	// ServiceRPC sends a request to /<service> and returns the
	// response. extraHeaders runs on each request before send.
	ServiceRPC(ctx context.Context, service string, body io.ReadSeeker, extraHeaders ...func(*http.Request)) (io.ReadCloser, error)
	// Do sends a request that is not a git request, such as a BB xorb
	// upload, to suffix under the repo path. It returns the response for
	// the caller to read and close. It is safe for concurrent use.
	Do(ctx context.Context, method, suffix string, body io.ReadSeeker, setHeaders func(*http.Request)) (*http.Response, error)
	// ErrorBaseURL is embedded into helper-status error messages and
	// passed to send-pack as the remote URL.
	ErrorBaseURL() string
}
