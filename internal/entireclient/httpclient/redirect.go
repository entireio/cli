package httpclient

import (
	"errors"
	"net/http"
	"strconv"
)

const (
	// SchemeHTTPS is the secure scheme required for production credential transport.
	SchemeHTTPS = "https"
	// SchemeHTTP is permitted only by callers' explicit insecure-origin policies.
	SchemeHTTP   = "http"
	maxRedirects = 10
)

var (
	// ErrRequestPolicy identifies a request refused for security policy reasons.
	ErrRequestPolicy = errors.New("request rejected by policy")
	// ErrRedirectPolicy identifies a redirect rejected by the shared security policy.
	ErrRedirectPolicy error = &policyError{msg: "redirect refused", parent: ErrRequestPolicy}
	// ErrInsecureRedirect identifies a redirect that would leave HTTPS.
	ErrInsecureRedirect error = &policyError{msg: "refusing redirect from HTTPS to an insecure scheme", parent: ErrRedirectPolicy}
	// ErrTooManyRedirects identifies a redirect chain exceeding the hop limit.
	// A loop is an availability failure, not a security rejection, so it does
	// not match ErrRequestPolicy: callers treat it like any other node failure.
	ErrTooManyRedirects = errors.New("stopped after " + strconv.Itoa(maxRedirects) + " redirects")
)

// policyError is a sentinel that matches its parent via errors.Is without
// repeating the parent's text, so a chained rejection prints one message.
type policyError struct {
	msg    string
	parent error
}

func (e *policyError) Error() string { return e.msg }
func (e *policyError) Unwrap() error { return e.parent }

// CheckSecureRedirect is a CheckRedirect policy that limits redirect chains and
// refuses to leave HTTPS once reached. Host restrictions belong to the caller.
//
// It returns ErrTooManyRedirects once via holds the hop limit, and
// ErrInsecureRedirect when req is not HTTPS but any request in via was. Only
// via counts as having reached HTTPS: req is the proposed next hop, so an
// HTTP → HTTPS upgrade is allowed and anything after it must stay HTTPS.
func CheckSecureRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return ErrTooManyRedirects
	}
	if req.URL.Scheme != SchemeHTTPS {
		for _, previous := range via {
			if previous.URL.Scheme == SchemeHTTPS {
				return ErrInsecureRedirect
			}
		}
	}
	return nil
}

// WithSecureRedirects returns a copy of client whose CheckRedirect enforces
// CheckSecureRedirect around the client's own callback. The callback runs
// first, on every hop, and can only add restrictions:
//   - http.ErrUseLastResponse is returned unchanged, so a callback can still
//     expose a 3xx response without following it;
//   - otherwise a shared-policy rejection of the proposed URL wins over the
//     callback's own error, so a downgrade always reports ErrInsecureRedirect;
//   - if the callback permits the hop, the possibly rewritten req.URL is
//     checked again, and the callback cannot undo an HTTPS upgrade.
//
// The callback also runs on hops the shared policy then rejects, so it must
// not have external side effects; changes to a rejected req are discarded
// because that request is never sent. Callbacks must not call
// CheckSecureRedirect themselves. A nil client uses
// http.DefaultClient. Transport, timeout and jar are preserved; neither the
// supplied client nor the default client is mutated.
func WithSecureRedirects(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	guarded := *client
	policy := client.CheckRedirect
	guarded.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		// Remember the proposed URL before the callback can rewrite it, but
		// do not reject a redirect the callback declines to follow.
		proposedErr := CheckSecureRedirect(req, via)
		if policy != nil {
			proposedScheme := req.URL.Scheme
			err := policy(req, via)
			// net/http compares ErrUseLastResponse by identity; do not wrap.
			if err == http.ErrUseLastResponse { //nolint:errorlint // net/http requires sentinel identity
				return err
			}
			if proposedErr != nil {
				return proposedErr
			}
			if err != nil {
				return err
			}
			// The first HTTPS upgrade is not yet in via. Do not let a
			// callback undo it, even when every previous hop was HTTP.
			if proposedScheme == SchemeHTTPS && req.URL.Scheme != SchemeHTTPS {
				return ErrInsecureRedirect
			}
			return CheckSecureRedirect(req, via)
		}
		return proposedErr
	}
	return &guarded
}
