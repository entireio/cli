package coreapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A draining/black-holed core accepts the connection but never sends
// response headers. Without an overall client timeout the request hangs on
// the OS TCP timeout; the client must instead fail fast (COR-572).
func TestCrossJurisHTTPClient_TimesOutOnSilentHost(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // never respond; unblock only when the client gives up
	}))
	defer srv.Close()

	client := newCrossJurisHTTPClientWithTimeout(150 * time.Millisecond)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)

	if err == nil {
		resp.Body.Close()
		t.Fatal("expected a timeout error, got a response")
	}
	var nerr net.Error
	if !errors.As(err, &nerr) || !nerr.Timeout() {
		t.Fatalf("expected a timeout net.Error, got %v (%T)", err, err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("client blocked %v; should have timed out near 150ms", elapsed)
	}
}

// The overall timeout must not break a normal, responsive request.
func TestCrossJurisHTTPClient_ResponsiveHostSucceeds(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := newCrossJurisHTTPClientWithTimeout(5 * time.Second)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("responsive host should succeed, got %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}
