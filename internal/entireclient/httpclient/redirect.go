package httpclient

import (
	"errors"
	"net/http"
)

// CheckSecureRedirect is the shared redirect floor, independent of each
// caller's host policy. An initial local-development HTTP request may upgrade,
// but once HTTPS has been reached no later hop may leave it.
func CheckSecureRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if req.URL.Scheme != "https" {
		for _, previous := range via {
			if previous.URL.Scheme == "https" {
				return errors.New("refusing redirect from HTTPS to an insecure scheme")
			}
		}
	}
	return nil
}

// WithSecureRedirects copies a client and enforces the mandatory TLS floor
// before and after its existing redirect policy. Transport, timeout and jar are preserved; the
// caller's client is not mutated. A nil policy retains Go's default host/header
// handling and our usual ten-hop cap.
func WithSecureRedirects(client *http.Client) *http.Client {
	guarded := *client
	policy := client.CheckRedirect
	guarded.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := CheckSecureRedirect(req, via); err != nil {
			return err
		}
		if policy != nil {
			if err := policy(req, via); err != nil {
				// net/http compares ErrUseLastResponse by identity; do not wrap.
				return err
			}
			// A callback can rewrite the destination. Validate the URL that
			// will actually be sent, not just the server's original Location.
			return CheckSecureRedirect(req, via)
		}
		return nil
	}
	return &guarded
}
