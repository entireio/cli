package clusterdiscovery

import (
	"errors"
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
		Transport: rewritingTransport{base: http.DefaultTransport, scheme: srvURL.Scheme, host: srvURL.Host},
	}
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
		// Closed server: connection refused. From the caller's POV this
		// is indistinguishable from a typo'd host like "foo.invalid";
		// both deserve the same actionable nudge.
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		client := hostPinningClient(t, srv)
		srv.Close()

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

func TestRenderLoginHint(t *testing.T) {
	hint := RenderLoginHint("rc.partial.to", []string{"https://a.example", "https://b.example"})

	// Each URL is on its own indented line — the "bog-simple stdout
	// list" requirement.
	assert.Contains(t, hint, "no auth context for cluster rc.partial.to")
	assert.Contains(t, hint, "\n  https://a.example\n", "missing indented URL line: %q", hint)
	assert.Contains(t, hint, "\n  https://b.example\n", "missing indented URL line: %q", hint)
	assert.Contains(t, hint, "entire login")
	assert.Contains(t, hint, "ENTIRE_AUTH_BASE_URL")
}
