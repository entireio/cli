// Package netcause names the cause of a transport-level failure, so a request
// that never left the machine is not reported as a credential or server
// problem.
package netcause

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
)

// Denied reports whether the operating system refused the connection itself
// (EPERM/EACCES from connect), as a sandbox or firewall does. Nothing reached
// the server, so no credential change can fix it.
func Denied(err error) bool {
	if canceled(err) {
		return false
	}
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)
}

// Hint explains a transport failure whose cause can be named, or returns ""
// when it cannot, or when Transport already added the explanation to err. A
// failed DNS lookup is named but not attributed: a blocked resolver and a host
// that does not exist look the same from here.
func Hint(err error) string {
	var hinted *hintedError
	if errors.As(err, &hinted) {
		return ""
	}
	if Denied(err) {
		return "this process is not allowed to open network connections (a sandbox or firewall is blocking them); re-run the command with network access"
	}
	var dnsErr *net.DNSError
	if !canceled(err) && errors.As(err, &dnsErr) {
		return fmt.Sprintf("could not resolve %s; check the network connection and that this process is allowed network access", dnsErr.Name)
	}
	return ""
}

func canceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// Transport returns next wrapped so a transport failure whose cause Hint can
// name carries that explanation in its message; the original error stays in
// the chain for errors.Is. Wrap at the base of a client's chain, so requests
// a higher transport synthesizes itself (token exchange, discovery) are
// covered too.
func Transport(next http.RoundTripper) http.RoundTripper {
	return hintTransport{next: next}
}

type hintTransport struct{ next http.RoundTripper }

func (t hintTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		if hint := Hint(err); hint != "" {
			return nil, &hintedError{err: err, hint: hint}
		}
		//nolint:wrapcheck // passthrough; the wrapped transport owns the error semantics.
		return nil, err
	}
	return resp, nil
}

// hintedError is a transport failure carrying its Hint, so a caller that adds
// hints of its own (an auth error naming the login server) does not add the
// same explanation twice.
type hintedError struct {
	err  error
	hint string
}

func (e *hintedError) Error() string { return e.err.Error() + "; " + e.hint }
func (e *hintedError) Unwrap() error { return e.err }
