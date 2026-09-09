package coreapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/entireio/auth-go/crossjuris"

	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
)

// uaByHop records the User-Agent each kind of request arrived with. Same
// cross-goroutine rationale as authRecorder: HTTP completion is not a
// happens-before edge the race detector recognises, so the map is mutex-guarded.
type uaByHop struct {
	mu   sync.Mutex
	seen map[string][]string
}

func newUAByHop() *uaByHop { return &uaByHop{seen: make(map[string][]string)} }

func (u *uaByHop) add(hop string, r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.seen[hop] = append(u.seen[hop], r.Header.Get("User-Agent"))
}

func (u *uaByHop) snapshot() map[string][]string {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make(map[string][]string, len(u.seen))
	for hop, vals := range u.seen {
		out[hop] = append([]string(nil), vals...)
	}
	return out
}

// The control-plane client must send entire-cli/<version> on every request it
// puts on the wire — including the two the cross-juris transport builds itself:
// the federation manifest fetch and the RFC 8693 exchange. Neither passes
// through caller code, and both go straight to t.base, so a User-Agent wrapper
// placed *above* the transport would leave them as Go's default. This test is
// what pins the wrapper to the base of the chain.
//
// It drives newCrossJurisHTTPClient rather than transportFor(t) on purpose:
// the wiring is what's under test, not the round tripper in isolation.
func TestNewCrossJurisHTTPClient_StampsUserAgentOnEveryHop(t *testing.T) {
	t.Parallel()
	ua := newUAByHop()

	homeCore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == crossjuris.TokenPath {
			ua.add("exchange", r)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"access_token":"home-exchanged-jwt","token_type":"Bearer","expires_in":300}`)) //nolint:errcheck // test
			return
		}
		ua.add("home-api", r)
		if r.Header.Get("Authorization") == bearerHomeExchanged {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(homeCore.Close)

	// The wrong core 421s to the home core, which makes the transport fetch
	// the federation manifest before it will follow.
	wrongCore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == crossjuris.WellKnownPath {
			ua.add("federation", r)
			writeTestFederation(w, []string{homeCore.URL})
			return
		}
		ua.add("wrong-api", r)
		w.WriteHeader(http.StatusMisdirectedRequest)
		w.Write([]byte(`{"home_core_url":"` + homeCore.URL + `"}`)) //nolint:errcheck // test
	}))
	t.Cleanup(wrongCore.Close)

	client, err := newCrossJurisHTTPClient(wrongCore.URL)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, wrongCore.URL+"/api/v1/mirrors", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer original-login-jwt")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the 421 → federation → exchange chain must complete)", resp.StatusCode)
	}

	// Every hop must be present: an absent hop means the chain changed shape
	// and this test silently stopped covering the synthesized requests.
	hops := ua.snapshot()
	want := versioninfo.UserAgent()
	for _, hop := range []string{"wrong-api", "federation", "home-api", "exchange"} {
		got := hops[hop]
		if len(got) == 0 {
			t.Fatalf("no %q request recorded; hops = %v", hop, hops)
		}
		for i, sent := range got {
			if sent != want {
				t.Errorf("%s request %d: User-Agent = %q, want %q", hop, i, sent, want)
			}
		}
	}
}
