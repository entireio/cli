package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
)

// uaSink records the User-Agent of every request an httptest server received.
// Mutex-guarded: HTTP completion is not a happens-before edge the race
// detector recognises, so the handler goroutine and the test goroutine need
// real synchronisation.
type uaSink struct {
	mu   sync.Mutex
	seen []string
}

func (s *uaSink) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.seen = append(s.seen, r.Header.Get("User-Agent"))
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`)) //nolint:errcheck // test fixture response
	}
}

func (s *uaSink) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

// This package's HTTP clients are handed to helpers that build their own
// requests — clusterdiscovery's well-known fetch and resolveCellAPIBaseURL's
// cluster listing — so nothing in this package sets a User-Agent per call.
// It has to ride on the transport, which means each constructor is on the hook
// for it.
func TestAuthHTTPClients_SendVersionedUserAgent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// build receives the test server's origin so the loopback-HTTP branch
		// can be selected by a real loopback URL rather than a global flag.
		build func(origin string) *http.Client
	}{
		{
			name:  "data API discovery, https origin",
			build: func(string) *http.Client { return dataAPIDiscoveryClient("https://entire.io") },
		},
		{
			// The plain-HTTP branch layers dataAPIHTTPDiscoveryTransport over
			// the stamp; it has to survive that.
			name:  "data API discovery, loopback http origin",
			build: dataAPIDiscoveryClient,
		},
		{
			name:  "cell exchange client",
			build: func(string) *http.Client { return cellExchangeHTTPClient("https://entire.io") },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sink := &uaSink{}
			srv := httptest.NewServer(sink.handler())
			t.Cleanup(srv.Close)

			client := tt.build(srv.URL)
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/api/v1/clusters", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			want := versioninfo.UserAgent()
			if got := sink.snapshot(); len(got) != 1 || got[0] != want {
				t.Fatalf("User-Agent seen = %v, want [%s]", got, want)
			}
		})
	}
}
