package httpclient

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// These tests each run t.Parallel() and each stand up their own httptest
// server, so their UserAgentTransport must wrap that server's OWN transport
// (srv.Client().Transport) rather than the process-global
// http.DefaultTransport. Sharing the global means sharing its idle-connection
// pool: one test's t.Cleanup(srv.Close) then tears down a pooled connection
// another test is mid-request on, which surfaces as
//
//	Do: ... transport connection broken: http: CloseIdleConnections called
//
// on a test that did nothing wrong. That flake was observed twice on CI in
// two days, in both cases on a PR touching nothing in this package.

func TestUserAgentTransport_SetsHeader(t *testing.T) {
	t.Parallel()

	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{
		Transport: &UserAgentTransport{
			Next: srv.Client().Transport,
			UA:   "test-binary/1.2.3",
		},
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()

	if want := "test-binary/1.2.3"; got != want {
		t.Errorf("User-Agent = %q, want %q", got, want)
	}
}

func TestUserAgentTransport_OverwritesCallerHeader(t *testing.T) {
	t.Parallel()

	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{
		Transport: &UserAgentTransport{
			Next: srv.Client().Transport,
			UA:   "wrapper-set",
		},
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("User-Agent", "caller-set")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()

	if want := "wrapper-set"; got != want {
		t.Errorf("User-Agent = %q, want %q", got, want)
	}
}

func TestUserAgentTransport_DoesNotMutateCallerRequest(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{
		Transport: &UserAgentTransport{
			Next: srv.Client().Transport,
			UA:   "wrapper-set",
		},
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("User-Agent", "caller-set")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()

	if got := req.Header.Get("User-Agent"); got != "caller-set" {
		t.Errorf("caller request mutated: User-Agent = %q, want %q", got, "caller-set")
	}
}
