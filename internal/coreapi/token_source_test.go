package coreapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// renewingSource hands out tok-1, tok-2, ... and records 401 reports.
type renewingSource struct {
	mu           sync.Mutex
	n            int
	unauthorized []string
}

func (s *renewingSource) Token(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return fmt.Sprintf("tok-%d", s.n), nil
}

func (s *renewingSource) Unauthorized(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unauthorized = append(s.unauthorized, token)
}

// A control-plane client for a service account secret asks its source for
// every request and reports a 401 for the bearer that request carried.
func TestNewWithTokenSource_RenewsAndReports401(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		seen = append(seen, token)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/problem+json")
		if token == "tok-1" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"status":401,"title":"Unauthorized"}`)) //nolint:errcheck // test server
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"status":403,"title":"Forbidden"}`)) //nolint:errcheck // test server
	}))
	defer server.Close()

	source := &renewingSource{}
	client, err := newWithTokenSource(server.URL, source)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := client.GetMe(context.Background()); err == nil {
			t.Fatal("GetMe succeeded against an error-only server")
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(seen, ","); got != "tok-1,tok-2" {
		t.Errorf("bearers = %s, want one fresh token per request", got)
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if got := strings.Join(source.unauthorized, ","); got != "tok-1" {
		t.Errorf("401 reports = %q, want only the refused token", got)
	}
}
