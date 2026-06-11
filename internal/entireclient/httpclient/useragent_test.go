package httpclient

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

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
			Next: http.DefaultTransport,
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
			Next: http.DefaultTransport,
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
			Next: http.DefaultTransport,
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
