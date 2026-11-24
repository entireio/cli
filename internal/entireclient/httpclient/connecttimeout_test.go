package httpclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// stalledListener accepts TCP connections and never answers, standing in for a
// proxy whose upstream is blackholed (CONNECT never replied to) or a node that
// accepts TCP and then stalls the TLS handshake.
func stalledListener(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		close(done)
		_ = l.Close()
	})
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				<-done
				_ = c.Close()
			}()
		}
	}()
	return l.Addr().String()
}

// getWithin issues a GET that gives up after 5s, so a missing connect budget
// fails the test instead of hanging it.
func getWithin(t *testing.T, rt http.RoundTripper, rawURL string) (*http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return (&http.Client{Transport: rt}).Do(req)
}

func TestConnectTimeoutBoundsStalledProxyConnect(t *testing.T) {
	t.Parallel()
	proxyURL := &url.URL{Scheme: "http", Host: stalledListener(t)}
	rt := &connectTimeoutTransport{
		next:    &http.Transport{Proxy: http.ProxyURL(proxyURL)},
		timeout: 200 * time.Millisecond,
	}

	// Without the budget net/http waits a minute for the CONNECT reply; the 5s
	// limit turns that into a deadline error instead of ErrConnectTimeout.
	resp, err := getWithin(t, rt, "https://region.auth.entire.io/")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("request through a stalled proxy succeeded")
	}
	if !errors.Is(err, ErrConnectTimeout) {
		t.Fatalf("err = %v, want ErrConnectTimeout", err)
	}
}

func TestNewTransportBoundsStalledTLSHandshake(t *testing.T) {
	t.Setenv(EnvConnectTimeout, "1")
	addr := stalledListener(t)

	start := time.Now()
	resp, err := getWithin(t, NewTransport(true), "https://"+addr+"/")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("request to a stalled TLS server succeeded")
	}
	if !errors.Is(err, ErrConnectTimeout) {
		t.Fatalf("err = %v, want ErrConnectTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("gave up after %s, want about the 1s budget", elapsed)
	}
}

func TestConnectTimeoutDoesNotLimitResponse(t *testing.T) {
	t.Parallel()
	const budget = 100 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(3 * budget) // slow headers
		w.WriteHeader(http.StatusOK)
		for i := range 3 {
			fmt.Fprintf(w, "chunk%d;", i)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(budget) // slow body
		}
	}))
	t.Cleanup(srv.Close)
	rt := &connectTimeoutTransport{next: &http.Transport{}, timeout: budget}

	resp, err := getWithin(t, rt, srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if got := string(body); got != "chunk0;chunk1;chunk2;" {
		t.Fatalf("body = %q", got)
	}
}

func TestConnectTimeoutKeepsCallerCancellation(t *testing.T) {
	t.Parallel()
	addr := stalledListener(t)
	rt := &connectTimeoutTransport{next: &http.Transport{}, timeout: time.Minute}

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+addr+"/", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := (&http.Client{Transport: rt}).Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("request succeeded")
	}
	if errors.Is(err, ErrConnectTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's context.DeadlineExceeded", err)
	}
}

func TestConnectTimeoutComposesWithCallerTrace(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(srv.Close)
	client := &http.Client{Transport: &connectTimeoutTransport{next: &http.Transport{}, timeout: time.Second}}

	var reused []bool
	ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) { reused = append(reused, info.Reused) },
	})
	for range 2 {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body) //nolint:errcheck // drain for reuse
		_ = resp.Body.Close()
	}
	if len(reused) != 2 || reused[0] || !reused[1] {
		t.Fatalf("caller GotConn reused = %v, want [false true]", reused)
	}
}

func TestNewTransportClosesIdleConnections(t *testing.T) {
	t.Parallel()
	rt, ok := newTransport(false, time.Second).(*connectTimeoutTransport)
	if !ok {
		t.Fatalf("newTransport returned %T", rt)
	}
	if got := rt.next.IdleConnTimeout; got != 90*time.Second {
		t.Fatalf("IdleConnTimeout = %s, want 90s", got)
	}
}

// holdAfterFirst passes the first accepted connection through and holds every
// later one open without serving it, so a redial stalls in the TLS handshake.
type holdAfterFirst struct {
	net.Listener

	accepted atomic.Int32
	held     chan net.Conn
}

func (l *holdAfterFirst) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.accepted.Add(1) == 1 {
			return c, nil
		}
		l.held <- c
	}
}

// TestConnectTimeoutCoversRetryAfterDeadReusedConn: when a reused keep-alive
// connection turns out dead, net/http redials inside the same RoundTrip. That
// redial must get its own connect budget rather than inherit the stopped one.
func TestConnectTimeoutCoversRetryAfterDeadReusedConn(t *testing.T) {
	t.Parallel()
	var served atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if served.Add(1) == 1 {
			fmt.Fprint(w, "ok")
			return
		}
		// Second request on the reused conn: hang up before any response
		// byte, which net/http treats as a dead idle conn and retries.
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("ResponseWriter not Hijacker")
			return
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	held := make(chan net.Conn, 8)
	srv.Listener = &holdAfterFirst{Listener: srv.Listener, held: held}
	srv.StartTLS()
	t.Cleanup(func() {
		srv.Close()
		close(held)
		for c := range held {
			_ = c.Close()
		}
	})
	rt := &connectTimeoutTransport{
		next:    &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		timeout: 200 * time.Millisecond,
	}

	resp, err := getWithin(t, rt, srv.URL)
	if err != nil {
		t.Fatalf("first request: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body) //nolint:errcheck // drain for reuse
	_ = resp.Body.Close()

	resp, err = getWithin(t, rt, srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("second request succeeded against a stalled redial")
	}
	if !errors.Is(err, ErrConnectTimeout) {
		t.Fatalf("err = %v, want ErrConnectTimeout from the redial", err)
	}
}

// TestNewTransportEndsAbandonedTLSHandshake: net/http keeps dialing after the
// connect budget cancels the request (the dial ignores request cancellation so
// a later request could reuse the conn), so the inner transport must bound the
// handshake itself or the abandoned dial holds the socket until the peer hangs up.
func TestNewTransportEndsAbandonedTLSHandshake(t *testing.T) {
	t.Parallel()
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	closedByClient := make(chan struct{})
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		// Never answer the ClientHello; Read returns once the client gives up.
		_, _ = io.Copy(io.Discard, c) //nolint:errcheck // only the return matters
		close(closedByClient)
	}()

	const budget = 200 * time.Millisecond
	resp, err := getWithin(t, newTransport(false, budget), "https://"+l.Addr().String()+"/")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("request to a stalled TLS server succeeded")
	}
	if !errors.Is(err, ErrConnectTimeout) {
		t.Fatalf("err = %v, want ErrConnectTimeout", err)
	}
	select {
	case <-closedByClient:
	case <-time.After(15 * budget):
		t.Fatal("abandoned TLS handshake still running after the request gave up")
	}
}
