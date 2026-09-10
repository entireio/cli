package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// End-to-end coverage of the dispatching-login-server shape against real
// HTTP servers: an apex that redirects the OAuth entry points to a regional
// login server and serves no token endpoint at all, exactly as
// https://auth.entire.io behaves in front of https://us.auth.entire.io.
//
// The apex handler 404s anything it is not supposed to serve, so a CLI that
// failed to follow the handoff fails these tests rather than silently
// passing.

const (
	devicePath   = "/device_authorization"
	tokenPath    = "/oauth/token"
	deviceCodeJS = `{"device_code":"dev-1","user_code":"WDJB-MJHT",` +
		`"verification_uri":"https://regional.example/device","expires_in":600,"interval":5}`
)

// newTestHTTPClient gives each test its own connection pool. Sharing
// http.DefaultTransport (the deviceflow/authcode fallback when NewClient
// gets a nil *http.Client) is unsafe here: httptest.Server.Close calls
// http.DefaultTransport.CloseIdleConnections as a courtesy to its users
// (net/http/httptest/server.go), so one parallel test's teardown can close
// a pooled connection another parallel test is about to reuse — and
// net/http does not retry that error, so it surfaces as "transport
// connection broken: http: CloseIdleConnections called".
func newTestHTTPClient(t *testing.T) *http.Client {
	t.Helper()
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("http.DefaultTransport is %T, want *http.Transport", http.DefaultTransport)
	}
	tr := base.Clone()
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

// newRegionalServer serves the endpoints a real regional login server does,
// recording which paths it was asked for.
func newRegionalServer(t *testing.T, seen *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case devicePath:
			if _, err := w.Write([]byte(deviceCodeJS)); err != nil {
				t.Errorf("write device response: %v", err)
			}
		case tokenPath:
			if _, err := w.Write([]byte(`{"access_token":"at-regional","refresh_token":"rt-regional","token_type":"Bearer","expires_in":3600}`)); err != nil {
				t.Errorf("write token response: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestDeviceFlow_FollowsApexRedirectToRegionalTokenEndpoint drives the real
// deviceflow client through a 307 on /device_authorization, then polls the
// region the response came from.
func TestDeviceFlow_FollowsApexRedirectToRegionalTokenEndpoint(t *testing.T) {
	t.Parallel()

	var regionalSeen []string
	regional := newRegionalServer(t, &regionalSeen)

	apex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != devicePath {
			// The apex mints nothing; reaching its token endpoint is the bug.
			http.Error(w, "apex serves no token endpoint", http.StatusNotFound)
			return
		}
		// 307 preserves the method and body, which is what lets the regional
		// core see the same form post.
		http.Redirect(w, r, regional.URL+devicePath, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(apex.Close)

	c := NewClient(apex.URL, newTestHTTPClient(t), false)

	start, err := c.StartDeviceAuth(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceAuth() error = %v", err)
	}
	if start.ResponseOrigin != regional.URL {
		t.Fatalf("ResponseOrigin = %q, want the regional origin %q", start.ResponseOrigin, regional.URL)
	}
	if start.DeviceCode != "dev-1" {
		t.Fatalf("DeviceCode = %q, want dev-1", start.DeviceCode)
	}

	if err := c.UseTokenIssuer(start.ResponseOrigin); err != nil {
		t.Fatalf("UseTokenIssuer(%q) error = %v", start.ResponseOrigin, err)
	}

	poll, err := c.PollDeviceAuth(context.Background(), start.DeviceCode)
	if err != nil {
		t.Fatalf("PollDeviceAuth() error = %v", err)
	}
	if poll.AccessToken != "at-regional" || poll.RefreshToken != "rt-regional" {
		t.Fatalf("poll = %+v, want the regional token pair", poll)
	}
	if len(regionalSeen) != 2 || regionalSeen[1] != tokenPath {
		t.Fatalf("regional saw %v, want the device request followed by %s", regionalSeen, tokenPath)
	}
}

// Without the retarget, the poll would go to the apex — which 404s. This
// pins that the override, not some incidental redirect following, is what
// makes the flow work.
func TestDeviceFlow_PollWithoutRetargetHitsTheApex(t *testing.T) {
	t.Parallel()

	apex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "apex serves no token endpoint", http.StatusNotFound)
	}))
	t.Cleanup(apex.Close)

	c := NewClient(apex.URL, newTestHTTPClient(t), false)
	if _, err := c.PollDeviceAuth(context.Background(), "dev-1"); err == nil {
		t.Fatal("PollDeviceAuth() = nil error, want the apex 404")
	}
}

func TestUseTokenIssuer_RejectsPlainHTTPWhenInsecureIsNotAllowed(t *testing.T) {
	t.Parallel()

	// A loopback base URL is always permitted, but that must not silently
	// extend to an arbitrary plaintext host handed over at runtime.
	c := NewClient("http://127.0.0.1:8787", nil, false)
	if err := c.UseTokenIssuer("http://regional.example"); err == nil {
		t.Fatal("UseTokenIssuer(plain http) = nil, want an insecure-URL error")
	}
	// Explicit --insecure-http-auth does not widen it either.
	insecure := NewClient("http://devbox.internal", nil, true)
	if err := insecure.UseTokenIssuer("http://regional.example"); err == nil {
		t.Fatal("UseTokenIssuer(plain http) = nil under --insecure-http-auth, want an insecure-URL error")
	}
	// Loopback http stays usable for local development.
	if err := c.UseTokenIssuer("http://127.0.0.1:9999"); err != nil {
		t.Fatalf("UseTokenIssuer(loopback http) = %v, want nil", err)
	}
}

// The browser flow's retarget is validated inside auth-go, which likewise
// permits plaintext only on loopback.
func TestBrowserFlowUseTokenIssuer_RejectsPlainHTTP(t *testing.T) {
	t.Parallel()

	c := NewClient("http://127.0.0.1:8787", nil, false)
	flow, err := c.StartBrowserAuth(context.Background())
	if err != nil {
		t.Fatalf("StartBrowserAuth() error = %v", err)
	}
	t.Cleanup(func() { _ = flow.Close() })

	if err := flow.UseTokenIssuer("http://regional.example"); err == nil {
		t.Fatal("UseTokenIssuer(plain http) = nil, want an insecure-URL error")
	}
}

func TestUseTokenIssuer_EmptyClearsTheOverride(t *testing.T) {
	t.Parallel()

	var seen []string
	regional := newRegionalServer(t, &seen)

	c := NewClient(regional.URL, newTestHTTPClient(t), false)
	if err := c.UseTokenIssuer("https://elsewhere.example"); err != nil {
		t.Fatalf("UseTokenIssuer() error = %v", err)
	}
	if err := c.UseTokenIssuer(""); err != nil {
		t.Fatalf(`UseTokenIssuer("") error = %v`, err)
	}
	if _, err := c.PollDeviceAuth(context.Background(), "dev-1"); err != nil {
		t.Fatalf("PollDeviceAuth() error = %v, want the poll back on BaseURL", err)
	}
}

// TestBrowserFlow_ExchangesAtTheCallbackIssuer drives the real authcode
// client: the apex builds the authorization URL, the callback names the
// region via RFC 9207 `iss`, and the code is redeemed there.
func TestBrowserFlow_ExchangesAtTheCallbackIssuer(t *testing.T) {
	t.Parallel()

	var regionalSeen []string
	regional := newRegionalServer(t, &regionalSeen)

	apex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "apex serves no token endpoint", http.StatusNotFound)
	}))
	t.Cleanup(apex.Close)

	c := NewClient(apex.URL, newTestHTTPClient(t), false)
	flow, err := c.StartBrowserAuth(context.Background())
	if err != nil {
		t.Fatalf("StartBrowserAuth() error = %v", err)
	}
	t.Cleanup(func() { _ = flow.Close() })

	authURL, err := url.Parse(flow.AuthorizationURL())
	if err != nil {
		t.Fatalf("parse AuthorizationURL: %v", err)
	}
	q := authURL.Query()
	if !strings.HasPrefix(authURL.String(), apex.URL) {
		t.Fatalf("AuthorizationURL = %q, want it on the dialled apex", authURL)
	}

	// Stand in for the browser landing on the loopback listener after the
	// apex handed the user off to the region.
	callback := q.Get("redirect_uri") + "?" + url.Values{
		"code":  {"code-1"},
		"state": {q.Get("state")},
		"iss":   {regional.URL},
	}.Encode()
	resp, err := http.Get(callback) //nolint:noctx // test helper hitting our own loopback listener
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	_ = resp.Body.Close()

	code, err := flow.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if code != "code-1" {
		t.Fatalf("code = %q, want code-1", code)
	}
	if flow.Issuer() != regional.URL {
		t.Fatalf("Issuer() = %q, want %q", flow.Issuer(), regional.URL)
	}

	if err := flow.UseTokenIssuer(flow.Issuer()); err != nil {
		t.Fatalf("UseTokenIssuer(%q) error = %v", flow.Issuer(), err)
	}

	access, refresh, err := flow.Exchange(context.Background(), code)
	if err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	if access != "at-regional" || refresh != "rt-regional" {
		t.Fatalf("Exchange() = (%q, %q), want the regional token pair", access, refresh)
	}
	if len(regionalSeen) != 1 || regionalSeen[0] != tokenPath {
		t.Fatalf("regional saw %v, want a single %s request", regionalSeen, tokenPath)
	}
}
