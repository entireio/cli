package clusterdiscovery

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hostPinningClient returns an http.Client that rewrites every request
// to hit srv. Lets the discovery helper assemble its
// "https://<clusterHost>/..." URL while we still serve responses from
// an httptest server on localhost.
func hostPinningClient(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	srvURL, err := url.Parse(srv.URL)
	require.NoError(t, err)
	return &http.Client{
		Transport: rewritingTransport{base: newTestTransport(t), scheme: srvURL.Scheme, host: srvURL.Host},
	}
}

// deadPinningClient returns a client pinned to a 127.0.0.1 address that
// accepts connections and immediately closes them, so every request
// through it fails. fetchWellKnownJSON wraps any transport error as
// ErrUnreachable, so callers asserting on that keep working.
//
// Do not build this by starting an httptest.Server and closing it to free
// the port. Tests here run with t.Parallel(), so another test's
// httptest.NewServer can be handed the released port and answer a request
// this client expects to fail — and that foreign request also trips the
// other test's in-handler path assertion. CI hit exactly that: one event
// failed both TestResolve_PreAudienceEntryNotUsedAsDiscoveryFallback
// (its "unreachable" cluster answered) and TestResolveContextForAPI
// (apiHandler saw /.well-known/entire-cluster.json). Holding the listener
// for the test's lifetime keeps the port ours.
func deadPinningClient(t *testing.T) *http.Client {
	t.Helper()
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		// If Accept ever fails for a reason other than our own Close, drop the
		// listener with it. A bound port with nobody accepting completes the
		// handshake from the kernel backlog, so requests would hang instead of
		// failing.
		defer func() { _ = l.Close() }()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return &http.Client{
		Transport: rewritingTransport{base: newTestTransport(t), scheme: "http", host: l.Addr().String()},
	}
}

// newTestTransport gives each test its own connection pool. Sharing
// http.DefaultTransport is unsafe here: httptest.Server.Close calls
// http.DefaultTransport.CloseIdleConnections as a courtesy to its users
// (net/http/httptest/server.go), so one parallel test's teardown can close
// a pooled connection another test is about to reuse — and net/http does
// not retry that error.
func newTestTransport(t *testing.T) *http.Transport {
	t.Helper()
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("http.DefaultTransport is %T, want *http.Transport", http.DefaultTransport)
	}
	tr := base.Clone()
	t.Cleanup(tr.CloseIdleConnections)
	return tr
}

type rewritingTransport struct {
	base   http.RoundTripper
	scheme string
	host   string
}

func (r rewritingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = r.scheme
	req.URL.Host = r.host

	return r.base.RoundTrip(req)
}

func TestDiscover(t *testing.T) {
	t.Run("returns parsed core_urls on 200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, Path, r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"core_urls":["https://eu.auth.partial.to","https://us.auth.partial.to"]}`)) //nolint:errcheck // test handler
		}))
		defer srv.Close()

		body, err := Discover(t.Context(), "royalcanin.partial.to", hostPinningClient(t, srv), t.Logf)
		require.NoError(t, err)
		assert.Equal(t, []string{"https://eu.auth.partial.to", "https://us.auth.partial.to"}, body.CoreURLs)
	})

	t.Run("HTTP 503 → ErrNoIssuers", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "cluster discovery not configured", http.StatusServiceUnavailable)
		}))
		defer srv.Close()

		_, err := Discover(t.Context(), "rc.partial.to", hostPinningClient(t, srv), t.Logf)
		assert.ErrorIs(t, err, ErrNoIssuers)
	})

	t.Run("empty core_urls → ErrNoCoreURLs", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"core_urls":[]}`)) //nolint:errcheck // test handler
		}))
		defer srv.Close()

		_, err := Discover(t.Context(), "rc.partial.to", hostPinningClient(t, srv), t.Logf)
		assert.ErrorIs(t, err, ErrNoCoreURLs)
	})

	t.Run("non-200 non-503 surfaces as generic error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "nope", http.StatusNotFound)
		}))
		defer srv.Close()

		_, err := Discover(t.Context(), "rc.partial.to", hostPinningClient(t, srv), t.Logf)
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrUnreachable)
		require.NotErrorIs(t, err, ErrNoIssuers)
		require.NotErrorIs(t, err, ErrNoCoreURLs)
	})

	t.Run("transport error → ErrUnreachable", func(t *testing.T) {
		// A host that never answers. From the caller's POV this is
		// indistinguishable from a typo'd host like "foo.invalid";
		// both deserve the same actionable nudge.
		client := deadPinningClient(t)

		_, err := Discover(t.Context(), "rc.partial.to", client, t.Logf)
		assert.ErrorIs(t, err, ErrUnreachable)
	})

	t.Run("malformed JSON surfaces as decode error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{not json`)) //nolint:errcheck // test handler
		}))
		defer srv.Close()

		_, err := Discover(t.Context(), "rc.partial.to", hostPinningClient(t, srv), t.Logf)
		require.Error(t, err)
		// Not a sentinel — caller falls through to the generic
		// "cluster discovery for <host>" wrapper.
		assert.False(t, errors.Is(err, ErrUnreachable) || errors.Is(err, ErrNoIssuers) || errors.Is(err, ErrNoCoreURLs),
			"decode error should not match any sentinel: %v", err)
	})

	t.Run("nil debugf is allowed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "nope", http.StatusNotFound)
		}))
		defer srv.Close()

		_, err := Discover(t.Context(), "rc.partial.to", hostPinningClient(t, srv), nil)
		assert.Error(t, err)
	})
}

// TestTrustedLoginServers normalises the advertised cores for display: whitespace
// and trailing slashes trimmed, blanks dropped, duplicates collapsed, advertised
// order kept (the resource lists its preferred core first).
func TestTrustedLoginServers(t *testing.T) {
	t.Parallel()
	got := trustedLoginServers([]string{"https://us.entire.io/", " ", " https://eu.entire.io ", "https://us.entire.io", ""})
	assert.Equal(t, []string{"https://us.entire.io", "https://eu.entire.io"}, got)
}
