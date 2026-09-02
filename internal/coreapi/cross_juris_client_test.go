package coreapi

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/entireio/auth-go/crossjuris"
)

// These tests cover the coreapi wiring over auth-go's crossjuris
// transport: which chain shapes complete, which server-supplied URLs are
// refused, and the CLI-specific Location canonicalization. The follower's
// internals (URL validation, TTL math, cache eviction) are tested in
// auth-go.

// Bearer tokens the fixtures assert on, hoisted to consts so goconst is happy.
const (
	bearerExchanged     = "Bearer exchanged-jwt"
	bearerHomeExchanged = "Bearer home-exchanged-jwt"
)

// authRecorder collects values captured inside an httptest handler
// goroutine for assertion in the test goroutine. HTTP completion isn't a
// happens-before edge the race detector recognises (see
// client_test.go:160-189), so the mutex makes the cross-goroutine reads
// race-safe under `-race`.
type authRecorder struct {
	mu   sync.Mutex
	vals []string
}

func (a *authRecorder) add(v string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.vals = append(a.vals, v)
}

func (a *authRecorder) snapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.vals...)
}

// crossJurisTestServer scripts responses keyed by the caller-controlled
// handler and records the Authorization header per request so tests can
// assert which token the transport presented on each hop. It auto-serves
// GET /.well-known/entire-federation with the hosts in `peers`.
type crossJurisTestServer struct {
	hits  atomic.Int32
	auths authRecorder
	peers []string
	srv   *httptest.Server
}

func (s *crossJurisTestServer) record(r *http.Request) {
	s.hits.Add(1)
	s.auths.add(r.Header.Get("Authorization"))
}

func newCrossJurisTestServer(t *testing.T, handler func(s *crossJurisTestServer, w http.ResponseWriter, r *http.Request)) *crossJurisTestServer {
	t.Helper()
	s := &crossJurisTestServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == crossjuris.WellKnownPath {
			writeTestFederation(w, s.peers)
			return
		}
		handler(s, w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func writeTestFederation(w http.ResponseWriter, peers []string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if len(peers) == 0 {
		w.Write([]byte(`{"peer_auth_hosts":[]}`)) //nolint:errcheck // test
		return
	}
	var b strings.Builder
	b.WriteString(`{"peer_auth_hosts":[`)
	for i, p := range peers {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"`)
		b.WriteString(p)
		b.WriteString(`"`)
	}
	b.WriteString(`]}`)
	w.Write([]byte(b.String())) //nolint:errcheck // test
}

// transportFor builds the coreapi round-tripper chain over a connection
// pool private to this test, with http loopback permitted because every
// fixture here is an httptest server.
//
// Do not hand it http.DefaultTransport. httptest.Server.Close calls
// http.DefaultTransport.CloseIdleConnections as a courtesy to its users
// (net/http/httptest/server.go), so with a shared pool any parallel test
// in this package tearing its server down closes a pooled connection
// another test is about to reuse. net/http does not retry that one:
// shouldRetryRequest lists errServerClosedIdle but not errCloseIdleConns,
// so it reaches the caller as "transport connection broken: http:
// CloseIdleConnections called". Clone keeps DefaultTransport's timeouts
// and proxy settings; only the pool is ours.
func transportFor(t *testing.T) http.RoundTripper {
	t.Helper()
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("http.DefaultTransport is %T, want *http.Transport", http.DefaultTransport)
	}
	tr := base.Clone()
	t.Cleanup(tr.CloseIdleConnections)
	rt, err := newCrossJurisRoundTripper(tr, true)
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

// TestRoundTripper_PassThrough: a 2xx is returned unchanged.
func TestRoundTripper_PassThrough(t *testing.T) {
	t.Parallel()
	srv := newCrossJurisTestServer(t, func(s *crossJurisTestServer, w http.ResponseWriter, r *http.Request) {
		s.record(r)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`)) //nolint:errcheck // test
	})

	client := &http.Client{Transport: transportFor(t)}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.srv.URL, nil) //nolint:errcheck // test
	req.Header.Set("Authorization", "Bearer user-jwt")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d", resp.StatusCode)
	}
	if srv.hits.Load() != 1 {
		t.Fatalf("hits=%d", srv.hits.Load())
	}
}

// TestRoundTripper_421FollowsToHomeCore: 421 with home_core_url is
// followed; the home core gets the original bearer on the first hop.
func TestRoundTripper_421FollowsToHomeCore(t *testing.T) {
	t.Parallel()
	homeCore := newCrossJurisTestServer(t, func(s *crossJurisTestServer, w http.ResponseWriter, r *http.Request) {
		s.record(r)
		w.WriteHeader(http.StatusOK)
	})
	wrongCore := newCrossJurisTestServer(t, func(s *crossJurisTestServer, w http.ResponseWriter, r *http.Request) {
		s.record(r)
		w.WriteHeader(http.StatusMisdirectedRequest)
		w.Write([]byte(`{"home_core_url":"` + homeCore.srv.URL + `"}`)) //nolint:errcheck // test
	})
	wrongCore.peers = []string{homeCore.srv.URL}

	client := &http.Client{Transport: transportFor(t)}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, wrongCore.srv.URL+"/api/v1/mirrors", strings.NewReader(`{"a":1}`)) //nolint:errcheck // test
	req.Header.Set("Authorization", "Bearer user-jwt")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d", resp.StatusCode)
	}
	if got := homeCore.auths.snapshot(); len(got) == 0 || got[0] != "Bearer user-jwt" {
		t.Fatalf("home core first hop auth = %v", got)
	}
}

// TestRoundTripper_421ThenBareUnauthorizedProactiveExchange is the
// production path: after following a 421, the home core can't verify the
// foreign-region login JWT's signature and returns a BARE 401 (no hint).
// The transport must still recover by exchanging the original login JWT
// at the home core's same-origin /oauth/token and retrying.
func TestRoundTripper_421ThenBareUnauthorizedProactiveExchange(t *testing.T) {
	t.Parallel()
	homeExchangeHits := atomic.Int32{}
	var homeExchangeSubjects, homeAPIAuths authRecorder
	homeCore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == crossjuris.TokenPath {
			_ = r.ParseForm() //nolint:errcheck // test
			homeExchangeSubjects.add(r.PostForm.Get("subject_token"))
			homeExchangeHits.Add(1)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"access_token":"home-exchanged-jwt","token_type":"Bearer","expires_in":300}`)) //nolint:errcheck // test
			return
		}
		homeAPIAuths.add(r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") == bearerHomeExchanged {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"ok":true}`)) //nolint:errcheck // test
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid token"}`)) //nolint:errcheck // test
	}))
	t.Cleanup(homeCore.Close)

	wrongCore := newCrossJurisTestServer(t, func(s *crossJurisTestServer, w http.ResponseWriter, r *http.Request) {
		s.record(r)
		w.WriteHeader(http.StatusMisdirectedRequest)
		w.Write([]byte(`{"home_core_url":"` + homeCore.URL + `"}`)) //nolint:errcheck // test
	})
	wrongCore.peers = []string{homeCore.URL}

	client := &http.Client{Transport: transportFor(t)}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, wrongCore.srv.URL+"/api/v1/mirrors/collaborators", strings.NewReader(`{}`)) //nolint:errcheck // test
	req.Header.Set("Authorization", "Bearer original-eu-login-jwt")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}
	if homeExchangeHits.Load() != 1 {
		t.Fatalf("home exchange hits=%d", homeExchangeHits.Load())
	}
	if subs := homeExchangeSubjects.snapshot(); len(subs) != 1 || subs[0] != "original-eu-login-jwt" {
		t.Fatalf("exchange subject_token = %v, want [original-eu-login-jwt]", subs)
	}
	if auths := homeAPIAuths.snapshot(); len(auths) != 2 || auths[0] != "Bearer original-eu-login-jwt" || auths[1] != bearerHomeExchanged {
		t.Fatalf("home API auths = %v", auths)
	}
}

// TestRoundTripper_BareUnauthorizedNoRedirectPassesThrough: a bare 401
// on a non-redirected request is a genuine failure — no exchange.
func TestRoundTripper_BareUnauthorizedNoRedirectPassesThrough(t *testing.T) {
	t.Parallel()
	exchangeHits := atomic.Int32{}
	srv := newCrossJurisTestServer(t, func(s *crossJurisTestServer, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == crossjuris.TokenPath {
			exchangeHits.Add(1)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"access_token":"x","expires_in":300}`)) //nolint:errcheck // test
			return
		}
		s.record(r)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid token"}`)) //nolint:errcheck // test
	})

	client := &http.Client{Transport: transportFor(t)}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.srv.URL+"/api/v1/me", nil) //nolint:errcheck // test
	req.Header.Set("Authorization", "Bearer user-jwt")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got %d", resp.StatusCode)
	}
	if srv.hits.Load() != 1 || exchangeHits.Load() != 0 {
		t.Fatalf("hits=%d exchanges=%d", srv.hits.Load(), exchangeHits.Load())
	}
}

// TestRoundTripper_401ExchangeAndRetry covers the structured-hint path.
func TestRoundTripper_401ExchangeAndRetry(t *testing.T) {
	t.Parallel()
	exchangeHits := atomic.Int32{}
	apiHits := atomic.Int32{}
	var apiAuths authRecorder
	var apiServer *httptest.Server
	apiServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == crossjuris.TokenPath {
			exchangeHits.Add(1)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"access_token":"exchanged-jwt","token_type":"Bearer","expires_in":300}`)) //nolint:errcheck // test
			return
		}
		n := apiHits.Add(1)
		apiAuths.add(r.Header.Get("Authorization"))
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"cross_juris_token_required","token_exchange_url":"` + apiServer.URL + `/oauth/token","audience":"` + apiServer.URL + `"}`)) //nolint:errcheck // test
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(apiServer.Close)

	client := &http.Client{Transport: transportFor(t)}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, apiServer.URL+"/api/v1/mirrors", strings.NewReader(`{"a":1}`)) //nolint:errcheck // test
	req.Header.Set("Authorization", "Bearer user-jwt")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d", resp.StatusCode)
	}
	if exchangeHits.Load() != 1 {
		t.Fatalf("exchanges=%d", exchangeHits.Load())
	}
	if auths := apiAuths.snapshot(); len(auths) != 2 || auths[0] != "Bearer user-jwt" || auths[1] != bearerExchanged {
		t.Fatalf("api auths = %v", auths)
	}
}

// TestRoundTripper_Rejects421OffFederation: a 421 to a host not in the
// responding core's federation manifest is not followed.
func TestRoundTripper_Rejects421OffFederation(t *testing.T) {
	t.Parallel()
	homeHits := atomic.Int32{}
	homeCore := newCrossJurisTestServer(t, func(s *crossJurisTestServer, w http.ResponseWriter, r *http.Request) {
		homeHits.Add(1)
		s.record(r)
		w.WriteHeader(http.StatusOK)
	})
	wrongCore := newCrossJurisTestServer(t, func(s *crossJurisTestServer, w http.ResponseWriter, r *http.Request) {
		s.record(r)
		w.WriteHeader(http.StatusMisdirectedRequest)
		w.Write([]byte(`{"home_core_url":"` + homeCore.srv.URL + `"}`)) //nolint:errcheck // test
	})
	// wrongCore.peers stays empty.

	client := &http.Client{Transport: transportFor(t)}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, wrongCore.srv.URL+"/api/v1/mirrors", nil) //nolint:errcheck // test
	req.Header.Set("Authorization", "Bearer user-jwt")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("got %d, want 421 passthrough", resp.StatusCode)
	}
	if homeHits.Load() != 0 {
		t.Fatalf("REGRESSION: off-federation home core received the JWT (hits=%d)", homeHits.Load())
	}
}

// TestRoundTripper_RejectsOffOrigin401ExchangeURL: a hint pointing
// token_exchange_url off-origin must not leak the JWT.
func TestRoundTripper_RejectsOffOrigin401ExchangeURL(t *testing.T) {
	t.Parallel()
	attackerHits := atomic.Int32{}
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attackerHits.Add(1)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"access_token":"attacker-issued","expires_in":300}`)) //nolint:errcheck // test
	}))
	t.Cleanup(attacker.Close)

	api := newCrossJurisTestServer(t, func(s *crossJurisTestServer, w http.ResponseWriter, r *http.Request) {
		s.record(r)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"cross_juris_token_required","token_exchange_url":"` + attacker.URL + `","audience":"https://api.test"}`)) //nolint:errcheck // test
	})

	client := &http.Client{Transport: transportFor(t)}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, api.srv.URL+"/api/v1/me", nil) //nolint:errcheck // test
	req.Header.Set("Authorization", "Bearer user-jwt")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got %d", resp.StatusCode)
	}
	if attackerHits.Load() != 0 {
		t.Fatalf("REGRESSION: attacker host received the JWT (hits=%d)", attackerHits.Load())
	}
}

// TestRoundTripper_BodyReplayedOnRetry: a retried request replays its body.
func TestRoundTripper_BodyReplayedOnRetry(t *testing.T) {
	t.Parallel()
	const payload = `{"provider":"github","owner":"acme","repo":"widget"}`
	homeCore := newCrossJurisTestServer(t, func(s *crossJurisTestServer, w http.ResponseWriter, r *http.Request) {
		s.record(r)
		body, _ := io.ReadAll(r.Body) //nolint:errcheck // test
		if string(body) != payload {
			t.Errorf("home body = %q", string(body))
		}
		w.WriteHeader(http.StatusOK)
	})
	wrongCore := newCrossJurisTestServer(t, func(s *crossJurisTestServer, w http.ResponseWriter, r *http.Request) {
		s.record(r)
		w.WriteHeader(http.StatusMisdirectedRequest)
		w.Write([]byte(`{"home_core_url":"` + homeCore.srv.URL + `"}`)) //nolint:errcheck // test
	})
	wrongCore.peers = []string{homeCore.srv.URL}

	client := &http.Client{Transport: transportFor(t)}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, wrongCore.srv.URL+"/api/v1/mirrors", bytes.NewReader([]byte(payload))) //nolint:errcheck // test
	req.Header.Set("Authorization", "Bearer user-jwt")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

// TestRoundTripper_NoInfiniteLoopOn421Chain confirms the redirect budget
// cap: a home core that itself 421s gets the second response passed
// through, not followed again.
func TestRoundTripper_NoInfiniteLoopOn421Chain(t *testing.T) {
	t.Parallel()
	var second *crossJurisTestServer
	first := newCrossJurisTestServer(t, func(s *crossJurisTestServer, w http.ResponseWriter, r *http.Request) {
		s.record(r)
		w.WriteHeader(http.StatusMisdirectedRequest)
		w.Write([]byte(`{"home_core_url":"` + second.srv.URL + `"}`)) //nolint:errcheck // test
	})
	second = newCrossJurisTestServer(t, func(s *crossJurisTestServer, w http.ResponseWriter, r *http.Request) {
		s.record(r)
		w.WriteHeader(http.StatusMisdirectedRequest)
		// Loopback host so the test exercises the budget cap, not the
		// federation/scheme reject path.
		w.Write([]byte(`{"home_core_url":"http://127.0.0.1:1"}`)) //nolint:errcheck // test
	})
	first.peers = []string{second.srv.URL}

	client := &http.Client{Transport: transportFor(t)}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, first.srv.URL+"/api/v1/mirrors", nil) //nolint:errcheck // test
	req.Header.Set("Authorization", "Bearer user-jwt")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("got %d, want the second 421 passed through after budget exhaustion", resp.StatusCode)
	}
	if first.hits.Load() != 1 || second.hits.Load() != 1 {
		t.Fatalf("first=%d second=%d, want 1/1 (no third hop)", first.hits.Load(), second.hits.Load())
	}
}

// TestRoundTripper_ExchangeFailurePropagates401: a non-200 exchange leaves
// the original 401 (and its hint body) visible to the caller.
func TestRoundTripper_ExchangeFailurePropagates401(t *testing.T) {
	t.Parallel()
	apiHits := atomic.Int32{}
	var api *httptest.Server
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == crossjuris.TokenPath {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"invalid_grant"}`)) //nolint:errcheck // test
			return
		}
		apiHits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"cross_juris_token_required","token_exchange_url":"` + api.URL + `/oauth/token","audience":"` + api.URL + `"}`)) //nolint:errcheck // test
	}))
	t.Cleanup(api.Close)

	client := &http.Client{Transport: transportFor(t)}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, api.URL+"/api/v1/me", nil) //nolint:errcheck // test
	req.Header.Set("Authorization", "Bearer user-jwt")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got %d, want original 401 surfaced", resp.StatusCode)
	}
	if apiHits.Load() != 1 {
		t.Fatalf("no retry when exchange fails, got %d hits", apiHits.Load())
	}
	body, _ := io.ReadAll(resp.Body) //nolint:errcheck // test
	if !strings.Contains(string(body), "cross_juris_token_required") {
		t.Errorf("original hint body must survive the failed exchange: %q", body)
	}
}

// TestRoundTripper_ExchangeAfter401Then421UsesOriginalSubjectToken is the
// regression for the chained-exchange case: 401-hint at the misdirected
// core → exchange → retry → 421 to home → 401-hint at home. The home
// exchange must reuse the ORIGINAL login JWT as subject_token, not the
// misdirected core's foreign-session token (which already carries
// foreign_iss; the server rejects chained cross-juris hops).
func TestRoundTripper_ExchangeAfter401Then421UsesOriginalSubjectToken(t *testing.T) {
	t.Parallel()
	const originalLoginJWT = "original-eu-login-jwt"

	homeExchangeHits := atomic.Int32{}
	var homeExchangeSubjects, homeAPIAuths authRecorder
	var homeAPI *httptest.Server
	homeAPI = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == crossjuris.TokenPath {
			_ = r.ParseForm() //nolint:errcheck // test
			homeExchangeSubjects.add(r.PostForm.Get("subject_token"))
			homeExchangeHits.Add(1)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"access_token":"home-exchanged-jwt","expires_in":300}`)) //nolint:errcheck // test
			return
		}
		homeAPIAuths.add(r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") == bearerHomeExchanged {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"cross_juris_token_required","token_exchange_url":"` + homeAPI.URL + `/oauth/token","audience":"` + homeAPI.URL + `"}`)) //nolint:errcheck // test
	}))
	t.Cleanup(homeAPI.Close)

	misdirectedExchangeHits := atomic.Int32{}
	misdirectedHits := atomic.Int32{}
	var misdirected *httptest.Server
	misdirected = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case crossjuris.WellKnownPath:
			writeTestFederation(w, []string{homeAPI.URL})
			return
		case crossjuris.TokenPath:
			misdirectedExchangeHits.Add(1)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"access_token":"misdirected-exchanged-jwt","expires_in":300}`)) //nolint:errcheck // test
			return
		}
		if misdirectedHits.Add(1) == 1 {
			// First hop: middleware sees the EU-aud JWT, emits the hint.
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"cross_juris_token_required","token_exchange_url":"` + misdirected.URL + `/oauth/token","audience":"` + misdirected.URL + `"}`)) //nolint:errcheck // test
			return
		}
		// Retry accepted; handler finds the cluster is in another
		// jurisdiction and 421s to home.
		w.WriteHeader(http.StatusMisdirectedRequest)
		w.Write([]byte(`{"home_core_url":"` + homeAPI.URL + `"}`)) //nolint:errcheck // test
	}))
	t.Cleanup(misdirected.Close)

	client := &http.Client{Transport: transportFor(t)}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, misdirected.URL+"/api/v1/mirrors", strings.NewReader(`{}`)) //nolint:errcheck // test
	req.Header.Set("Authorization", "Bearer "+originalLoginJWT)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want home core to accept the home-audience exchanged token", resp.StatusCode)
	}
	if misdirectedExchangeHits.Load() != 1 || homeExchangeHits.Load() != 1 {
		t.Fatalf("misdirected=%d home=%d exchanges, want 1/1", misdirectedExchangeHits.Load(), homeExchangeHits.Load())
	}
	if subs := homeExchangeSubjects.snapshot(); len(subs) != 1 || subs[0] != originalLoginJWT {
		t.Fatalf("REGRESSION: home exchange must reuse the ORIGINAL login JWT as subject_token, got %v", subs)
	}
	if auths := homeAPIAuths.snapshot(); len(auths) != 2 || auths[0] != "Bearer "+originalLoginJWT || auths[1] != bearerHomeExchanged {
		t.Fatalf("home API auths = %v", auths)
	}
}

// TestNewCrossJurisHTTPClient_InsecureHTTPOnlyForLoopbackCore pins the
// production posture: an https core never unlocks http:// redirect
// targets, so a 421 to a plaintext loopback home is refused outright.
func TestNewCrossJurisHTTPClient_InsecureHTTPOnlyForLoopbackCore(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// coreURL is "" to dial the loopback wrong core itself.
		coreURL      string
		wantStatus   int
		wantHomeHits int32
	}{
		{name: "loopback http core follows", coreURL: "", wantStatus: http.StatusOK, wantHomeHits: 1},
		{name: "https core refuses http target", coreURL: "https://core.example", wantStatus: http.StatusMisdirectedRequest, wantHomeHits: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			homeCore := newCrossJurisTestServer(t, func(s *crossJurisTestServer, w http.ResponseWriter, r *http.Request) {
				s.record(r)
				w.WriteHeader(http.StatusOK)
			})
			wrongCore := newCrossJurisTestServer(t, func(s *crossJurisTestServer, w http.ResponseWriter, r *http.Request) {
				s.record(r)
				w.WriteHeader(http.StatusMisdirectedRequest)
				w.Write([]byte(`{"home_core_url":"` + homeCore.srv.URL + `"}`)) //nolint:errcheck // test
			})
			wrongCore.peers = []string{homeCore.srv.URL}

			coreURL := tc.coreURL
			if coreURL == "" {
				coreURL = wrongCore.srv.URL
			}
			client, err := newCrossJurisHTTPClient(coreURL)
			if err != nil {
				t.Fatal(err)
			}
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, wrongCore.srv.URL+"/api/v1/mirrors", nil) //nolint:errcheck // test
			req.Header.Set("Authorization", "Bearer user-jwt")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("got %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if got := homeCore.hits.Load(); got != tc.wantHomeHits {
				t.Fatalf("home hits=%d, want %d", got, tc.wantHomeHits)
			}
		})
	}
}
