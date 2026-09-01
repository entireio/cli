package versioninfo

import (
	"context"
	"net/http"
	"sync"
	"testing"
)

// uaRecorder is the terminal RoundTripper in a wrapped chain: it records the
// User-Agent each request carried by the time it would have hit the network.
// Mutex-guarded because a transport may be driven from more than one goroutine.
type uaRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *uaRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.seen = append(r.seen, req.Header.Get("User-Agent"))
	r.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
}

func (r *uaRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

func TestWrapTransport_StampsUserAgent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		callerU string // User-Agent the caller set, if any
	}{
		{name: "no user-agent set"},
		{name: "overwrites go default", callerU: "Go-http-client/2.0"},
		{name: "overwrites caller value", callerU: "something-else/1.2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := &uaRecorder{}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://entire.io/api/v1/x", nil)
			if err != nil {
				t.Fatal(err)
			}
			if tt.callerU != "" {
				req.Header.Set("User-Agent", tt.callerU)
			}

			resp, err := WrapTransport(rec).RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			want := UserAgent()
			if got := rec.snapshot(); len(got) != 1 || got[0] != want {
				t.Fatalf("User-Agent sent = %v, want [%s]", got, want)
			}
			// The caller's own request must be left as it was: callers reuse
			// requests across retries.
			if got := req.Header.Get("User-Agent"); got != tt.callerU {
				t.Fatalf("caller request mutated: User-Agent = %q, want %q", got, tt.callerU)
			}
		})
	}
}

// A nil next means "Go's default transport", so callers can wrap a client that
// has no Transport of its own without reaching for http.DefaultTransport.
func TestWrapTransport_NilNextUsesDefaultTransport(t *testing.T) {
	t.Parallel()

	wrapped, ok := WrapTransport(nil).(userAgentTransport)
	if !ok {
		t.Fatalf("WrapTransport(nil) = %T, want userAgentTransport", WrapTransport(nil))
	}
	if wrapped.next != http.DefaultTransport {
		t.Fatalf("next = %v, want http.DefaultTransport", wrapped.next)
	}
}

// The version is not known at package-initialization time: main() calls Load()
// to recover it from build info. A transport built before that — a package-level
// client like pluginHTTPClient — must still send the resolved version, so the
// User-Agent has to be read per request rather than captured at construction.
//
// Not parallel: it swaps the package-level Version. Go runs sequential top-level
// tests to completion before parallel ones resume, so the parallel tests above
// never observe the swap.
func TestWrapTransport_ResolvesUserAgentPerRequest(t *testing.T) {
	rec := &uaRecorder{}
	// Built first, while Version is still the pre-Load default.
	rt := WrapTransport(rec)

	original := Version
	t.Cleanup(func() { Version = original })
	Version = "9.9.9"

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://entire.io/api/v1/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	const want = "entire-cli/9.9.9"
	if got := rec.snapshot(); len(got) != 1 || got[0] != want {
		t.Fatalf("User-Agent sent = %v, want [%s] — the transport captured the version at construction", got, want)
	}
}
