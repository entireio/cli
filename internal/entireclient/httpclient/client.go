package httpclient

import (
	"net/http"
	"time"
)

// Options configures an HTTP client without allowing callers to omit the shared
// redirect policy. A nil Transport uses http.DefaultTransport and a zero
// Timeout means no timeout, as on http.Client. CheckRedirect runs before the
// shared policy on every hop and can only add restrictions; see
// WithSecureRedirects for the exact composition.
type Options struct {
	Transport     http.RoundTripper
	Timeout       time.Duration
	CheckRedirect func(*http.Request, []*http.Request) error
}

// New returns an HTTP client that always rejects HTTPS downgrades and caps
// redirect chains. It preserves Go's sensitive-header handling and composes
// any supplied callback.
func New(options Options) *http.Client {
	return WithSecureRedirects(&http.Client{
		Transport:     options.Transport,
		Timeout:       options.Timeout,
		CheckRedirect: options.CheckRedirect,
	})
}
