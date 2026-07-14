package httpclient

import "net/http"

// UserAgentTransport wraps another http.RoundTripper and stamps the
// User-Agent header on every outgoing request so the server can
// attribute traffic to the calling binary. Callers that already set
// User-Agent are overwritten: the wrapper exists to give the binary a
// single identity in upstream access logs, not to be overridden per
// request.
//
// The wrapper clones the request before mutating headers so the
// caller's original *http.Request is left untouched — important for
// retries and for callers that hold a reference after Do returns.
//
// Concurrent use is safe iff Next is safe for concurrent use.
type UserAgentTransport struct {
	Next http.RoundTripper
	UA   string
}

// RoundTrip implements http.RoundTripper.
func (t *UserAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("User-Agent", t.UA)
	//nolint:wrapcheck // thin passthrough: wrapping would change error semantics for callers that errors.As on transport errors.
	return t.Next.RoundTrip(r)
}
