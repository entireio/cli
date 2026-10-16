package httpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"time"
)

// ErrConnectTimeout reports that a request could not obtain a usable
// connection within its connect budget.
var ErrConnectTimeout = errors.New("connection setup timed out")

// connectTimeoutTransport bounds connection setup for each request: DNS, TCP,
// the proxy CONNECT when a proxy is configured, and TLS. The net.Dialer
// timeout alone covers only the TCP connect to the first hop; behind a proxy
// that hop is the proxy itself, and net/http then waits up to a minute for the
// CONNECT reply, so a blackholed replica would cost a minute per failover
// attempt instead of the configured budget.
//
// The budget runs per connection attempt, from httptrace GetConn until GotConn,
// so it never cuts off a slow response or a long pack stream.
type connectTimeoutTransport struct {
	next    *http.Transport
	timeout time.Duration
}

// RoundTrip implements http.RoundTripper.
func (t *connectTimeoutTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancelCause(req.Context())
	timer := time.AfterFunc(t.timeout, func() { cancel(ErrConnectTimeout) })
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		// GetConn fires for every connection attempt, including the redial
		// net/http makes inside this RoundTrip when a reused keep-alive
		// connection turns out dead, so each attempt gets the full budget.
		GetConn: func(string) { timer.Reset(t.timeout) },
		GotConn: func(httptrace.GotConnInfo) { timer.Stop() },
	})

	resp, err := t.next.RoundTrip(req.WithContext(ctx))
	if err != nil {
		timer.Stop()
		timedOut := errors.Is(context.Cause(ctx), ErrConnectTimeout)
		cancel(nil)
		if timedOut {
			return nil, fmt.Errorf("%w after %s (set %s to allow longer)", ErrConnectTimeout, t.timeout, EnvConnectTimeout)
		}
		//nolint:wrapcheck // passthrough: callers classify transport errors themselves
		return nil, err
	}
	// The request context must outlive RoundTrip: the body streams under it.
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: func() { cancel(nil) }}
	return resp, nil
}

// CloseIdleConnections lets http.Client.CloseIdleConnections reach the
// wrapped transport's pool.
func (t *connectTimeoutTransport) CloseIdleConnections() {
	t.next.CloseIdleConnections()
}

// cancelOnClose releases the per-request context once the caller is done with
// the response body.
type cancelOnClose struct {
	io.ReadCloser

	cancel func()
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	//nolint:wrapcheck // passthrough
	return err
}
