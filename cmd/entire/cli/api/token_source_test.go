package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// countingSource hands out tok-1, tok-2, ... and records 401 reports.
type countingSource struct {
	mu           sync.Mutex
	n            int
	err          error
	unauthorized []string
}

func (s *countingSource) Token(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return "", s.err
	}
	s.n++
	return fmt.Sprintf("tok-%d", s.n), nil
}

func (s *countingSource) Unauthorized(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unauthorized = append(s.unauthorized, token)
}

func TestTokenSourceClient_AsksPerRequestAndReports401(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		seen = append(seen, token)
		mu.Unlock()
		if token == "tok-2" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	source := &countingSource{}
	client := NewClientWithTokenSource(source, server.URL)
	for range 3 {
		resp, err := client.Get(context.Background(), "/x")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(seen, ","); got != "tok-1,tok-2,tok-3" {
		t.Errorf("bearers = %s, want one fresh token per request", got)
	}
	if got := strings.Join(source.unauthorized, ","); got != "tok-2" {
		t.Errorf("401 reports = %q, want exactly the refused token", got)
	}
}

func TestTokenSourceClient_SourceErrorSendsNothing(t *testing.T) {
	t.Parallel()

	hit := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	errExchange := errors.New("exchange refused")
	client := NewClientWithTokenSource(&countingSource{err: errExchange}, server.URL)
	resp, err := client.Get(context.Background(), "/x")
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, errExchange) {
		t.Fatalf("err = %v, want the source's error", err)
	}
	if hit {
		t.Error("a request was sent without a bearer")
	}
}
